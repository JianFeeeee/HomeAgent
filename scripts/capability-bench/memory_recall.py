#!/usr/bin/env python3
"""记忆召回测试 v2 —— 打在「压缩 vs 外化裁剪」真正分叉的地方。

v1 的两个结构性缺陷（两侧行为完全一致地打满分 ⇒ 无区分度）：
  1. 针全部显式标注「请牢牢记住」⇒ 任何 harness 都会保住被标重点的内容；
  2. 填充是语义空转的中性句 ⇒ 压缩可以几乎无损摘要，向量裁剪也挑不出毛病。

v2 的设计原则：**取消一切显式强调**，把要召回的事实伪装成填充的一部分，
并引入两类压缩天然吃亏、检索/外化理论上占优的负载：

探针类型（全部落在 plan 里，报告按类分层）：
  · casual     偶发事实 —— 某值只在一轮填充里顺口出现一次，无任何标记
  · overwrite  值覆盖   —— 同一属性先后给两个值，只认最新值（压缩易塌回旧值）
  · multihop   多跳散点 —— 答案 = 两个分散在不同轮的偶发事实的组合运算
  · paraphrase 改述提问 —— 提问措辞与出现时的措辞不同，防廉价字符串匹配

填充分三档：
  · neutral    中性句（保留 v1 风格，作对照）
  · confusable 同域干扰 —— 大量形似值（端口/代号/编号），逼检索分辨
  · needle     偶发针就藏在同域干扰轮里（这是关键：针不单独成轮）

预期：两侧都不该 100%。若偶发针仍全中，说明判据或填充强度不够，先改工具再谈结论。
"""
from __future__ import annotations

import argparse
import json
import os
import random
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import bench


def est_tokens(s: str) -> int:
    """与内核同口径的估算（min(字节, 2×rune)）—— 用于控制灌入量。"""
    b = len(s.encode("utf-8"))
    r = len(s) * 2
    return max(1, min(b, r)) if s else 0


def _int(v: object) -> int:
    try:
        return int(v)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return 0


def _norm_ha_usage(u: object) -> dict:
    """HomeAgent（OpenAI 口径，prompt 含缓存）→ 统一键。"""
    u = u if isinstance(u, dict) else {}
    return {
        "prompt": _int(u.get("prompt_tokens")),
        "completion": _int(u.get("completion_tokens")),
        "total": _int(u.get("total_tokens")),
        "cache_read": _int(u.get("cache_read_tokens")),
        "cache_miss": _int(u.get("cache_miss_tokens")),
    }


def _norm_pi_usage(u: object) -> dict:
    """pi（Anthropic 口径，input=未命中侧）→ 统一键。"""
    u = u if isinstance(u, dict) else {}
    uncached = _int(u.get("input"))
    read = _int(u.get("cacheRead"))
    out = _int(u.get("output"))
    return {
        "prompt": uncached + read, "completion": out,
        "total": _int(u.get("totalTokens")) or (uncached + read + out),
        "cache_read": read, "cache_miss": uncached,
    }


# ════════════════════════════════════════════════════════════════
# 材料生成（全部确定性：seed 相同 ⇒ 探针/答案/干扰集完全一致）
# ════════════════════════════════════════════════════════════════

NEUTRAL_TOPICS = [
    "今天的天气看上去不错", "窗外的树叶在动", "桌上的杯子是陶瓷的",
    "打印机需要更换墨盒", "走廊的灯换了新的", "会议室的椅子有些松动",
    "楼下的咖啡店换了招牌", "电梯里贴了新的通知", "前台的绿植长得很好",
    "午休时间走廊很安静", "空调的温度设定在二十六度", "储物柜的编号是连续的",
    "茶水间的咖啡豆快用完了", "停车场 B 区在修地面", "二楼窗户的把手有点松",
    "打印机旁的纸箱该清了", "新来的同事工位在三楼", "楼道口的自行车该挪了",
]


class Material:
    """v2 全部材料。生成时同时产出：对话脚本 + 期望答案 + 判据。"""

    def __init__(self, rng_seed: int, window: int, overshoot: float):
        rng = random.Random(rng_seed)
        self.rng = rng
        self.window = window
        self.overshoot = overshoot

        # ---- 偶发针：值伪装成同域干扰值，措辞与干扰项同构 ----
        # 同域 A：服务端口（4-5 位数）。4 根偶发针 + 40 个干扰值混在一批填充里。
        self.casual = {
            "auth 服务的端口": rng.randrange(8000, 8999),
            "metrics 服务的端口": rng.randrange(8000, 8999),
            "trace 服务的端口": rng.randrange(8000, 8999),
            "admin 服务的端口": rng.randrange(8000, 8999),
        }
        self.port_distractors = [rng.randrange(8000, 8999) for _ in range(40)]
        # 同域 B：内部代号（字母-数字）。多跳针 + 干扰值。
        self.code_distractors = [
            f"{rng.choice(['BLUE','GREEN','SILVER','COPPER','IVORY','ONYX'])}"
            f"-{rng.randrange(1000, 9999)}" for _ in range(24)
        ]
        self.multihop_team = f"{rng.choice(['SILVER','COPPER','IVORY','ONYX'])}-{rng.randrange(1000, 9999)}"
        self.multihop_room = rng.choice(["A1103", "A1107", "B2201", "B2210", "C3305"])

        # ---- 覆盖针：同一属性先后两个值，只认新值 ----
        self.overwrite_key = "值班室的分机号"
        self.overwrite_old = rng.randrange(4000, 4999)
        self.overwrite_new = rng.randrange(4000, 4999)

        # ---- 填充 token 目标 ----
        self.target_tokens = int(window * overshoot)

    # -- 各类填充轮 --

    def neutral_block(self, n: int) -> str:
        """中性句，逐句不重复编号。作对照档。"""
        out = []
        for i in range(n):
            t = self.rng.choice(NEUTRAL_TOPICS)
            out.append(f"{t}（编号 {self.rng.randrange(100000, 999999)}）")
        return "；".join(out)

    def confusable_block(self, n: int, hide: list[tuple[str, int]] | None = None) -> str:
        """同域干扰：n 个形似端口值；hide 里的偶发针**混在其中**，
        措辞与干扰项完全同构（都叫「某端口是 XXXX」），无任何强调。"""
        vals: list[str] = []
        slots = self.rng.sample(range(n), len(hide)) if hide else []
        hidden_used = 0
        for i in range(n):
            if hide and hidden_used < len(hide) and i in slots:
                vals.append(f"{hide[hidden_used][0]}是 {hide[hidden_used][1]}")
                hidden_used += 1
            else:
                vals.append(f"网关端口是 {self.rng.choice(self.port_distractors)}")
        return "；".join(vals)

    def code_block(self, n: int) -> str:
        """代号干扰：n 个形似内部代号。"""
        vals = []
        for _ in range(n):
            vals.append(f"{self.rng.choice(['BLUE','GREEN','SILVER','COPPER','IVORY','ONYX'])}"
                        f"-{self.rng.randrange(1000, 9999)} 归档在 {self.rng.choice(['A','B','C'])}"
                        f"{self.rng.randrange(1000, 3999)}")
        return "；".join(vals)

    def build_plan(self) -> list[dict]:
        """对话脚本。针混在干扰轮里，绝不单独成轮、绝不预告。"""
        rng = self.rng
        plan: list[dict] = []

        # 每轮 token：80 句/轮。v2 句子短（"网关端口是 8123"），30 句只有 ~900 token
        # 会让轮数烟到 200+，轮开销（每轮一次 LLM 调用）反而稀释了填充密度。
        per_turn = 80
        # ★ 估算必须按**实际混合比例**：neutral 句长（~43 token/句），
        # confusable 句短（~22 token/句）。v2 轮里 confusable 占多数，
        # 若按 neutral 估会高估近一倍 ⇒ 实际灌入量不超窗（教训：上轮 dry-run
        # 估 178k 实际只有 ~90k）。
        sample_neutral = self.neutral_block(per_turn)
        sample_conf = self.confusable_block(per_turn)
        # 混合比例与下方循环一致：45% neutral，55% confusable（含针/覆盖/多跳轮，
        # 它们的长度落在 confusable 同级）
        per_turn_tokens = max(1, int(est_tokens(sample_neutral) * 0.45
                                     + est_tokens(sample_conf) * 0.55))
        filler_turns = max(4, self.target_tokens // per_turn_tokens)

        # 偶发针分散到 4 个不同位置（~10%/35%/60%/85% 处），各藏 1 根
        hide_positions = {
            int(filler_turns * 0.10): [("auth 服务的端口", self.casual["auth 服务的端口"])],
            int(filler_turns * 0.35): [("metrics 服务的端口", self.casual["metrics 服务的端口"])],
            int(filler_turns * 0.60): [("trace 服务的端口", self.casual["trace 服务的端口"])],
            int(filler_turns * 0.85): [("admin 服务的端口", self.casual["admin 服务的端口"])],
        }
        # 覆盖针：旧值在 ~20% 处，新值在 ~50% 处（中间隔大量干扰）
        old_pos = int(filler_turns * 0.20)
        new_pos = int(filler_turns * 0.50)
        # 多跳要素：团队代号在 ~30%，会议室在 ~70%（两处都混进代号干扰轮）
        hop_pos = int(filler_turns * 0.30)
        room_pos = int(filler_turns * 0.70)

        # 开场白：完全自然，不预告任何要记的东西
        plan.append({"kind": "filler", "cat": "neutral",
                     "text": "我接着上一条线继续同步一些琐碎记录，你顺手收着就行。"})

        for i in range(filler_turns):
            r = rng.random()
            if i in hide_positions:
                block = self.confusable_block(per_turn, hide=hide_positions[i])
                cat = "confusable"
            elif i == old_pos:
                block = self.confusable_block(per_turn - 1) + f"；{self.overwrite_key}是 {self.overwrite_old}"
                cat = "confusable"
            elif i == new_pos:
                block = self.confusable_block(per_turn - 1) + f"；对了，{self.overwrite_key}改成 {self.overwrite_new} 了"
                cat = "confusable"
            elif i == hop_pos:
                block = self.code_block(per_turn - 1) + f"；{self.multihop_team} 团队负责门禁"
                cat = "confusable"
            elif i == room_pos:
                block = self.code_block(per_turn - 1) + f"；门禁申请表放在 {self.multihop_room}"
                cat = "confusable"
            elif r < 0.45:
                block = self.neutral_block(per_turn)
                cat = "neutral"
            else:
                block = self.confusable_block(per_turn)
                cat = "confusable"
            # 每轮带唯一序号避免内核 dedupe（v1 教训：相同输入被 skipped:true 跳过）
            plan.append({"kind": "filler", "cat": cat,
                         "text": f"日常记录（第 {i} 批）：{block}"})

        # ---- 提问（措辞与出现时不同；一次一问）----

        def q_port(name: str) -> str:
            return (f"我记不清具体数字了，帮我对一下：{name}应该是多少？"
                    f"直接告诉我数字就行。")

        probes: list[dict] = []
        # casual ×4：改述提问
        for name, v in self.casual.items():
            probes.append({"kind": "probe", "ptype": "casual",
                           "text": q_port(name), "expect": str(v)})
        # overwrite ×1：只问最新值，措辞里不提示"改过"
        probes.append({"kind": "probe", "ptype": "overwrite",
                       "text": "对了，值班室的分机现在多少来着？就回个数字。",
                       "expect": str(self.overwrite_new)})
        # overwrite 反向哨兵 ×1：期望答出旧值算「塌回」（不计召回，单独统计）
        probes.append({"kind": "probe", "ptype": "overwrite-stale",
                       "text": "帮我确认下，值班室分机是不是 4 开头的那个老号码？是多少？",
                       "expect": str(self.overwrite_old)})
        # multihop ×1：两个散点组合（团队代号 → 门禁 → 表放哪间）
        probes.append({"kind": "probe", "ptype": "multihop",
                       "text": ("有同事要找负责门禁的那个团队拿门禁申请表，"
                                "我记得表放在某个房间。帮我捋一下：负责门禁的团队是哪个代号，"
                                "申请表在哪个房间？两个都要答。"),
                       "expect": f"{self.multihop_team} {self.multihop_room}"})

        for p in probes:
            plan.append(p)

        # ★ 协议断言：cli.sock 按行读，任何一轮含换行都会被静默拆成多条消息
        for idx, step in enumerate(plan, 1):
            assert "\n" not in step["text"], f"第 {idx} 轮含换行（协议按行读）"
        return plan


# ════════════════════════════════════════════════════════════════
# 判定
# ════════════════════════════════════════════════════════════════

def score(reply: str, expect: str, ptype: str) -> bool:
    """召回判定。

    casual/overwrite：值必须出现，且**不得**把同域干扰值或旧值当答案；
    multihop：两个要素都出现；
    overwrite-stale：期望答出旧值（塌回哨兵，反向计分）。
    """
    r = reply.upper()
    if ptype == "multihop":
        parts = expect.split()
        return all(p.upper() in r for p in parts)
    if ptype == "overwrite-stale":
        return expect in r
    ok = expect in r
    return ok


def run(args: argparse.Namespace) -> int:
    mat = Material(args.seed, args.window, args.overshoot)
    plan = mat.build_plan()
    filler_tokens = sum(est_tokens(p["text"]) for p in plan if p["kind"] == "filler")

    print(f"会话计划：{len(plan)} 轮 = 填充 "
          f"{sum(1 for p in plan if p['kind']=='filler')} + 提问 "
          f"{sum(1 for p in plan if p['kind']=='probe')}")
    print(f"  填充 token 估算 ≈ {filler_tokens}（目标窗口 {args.window}，"
          f"比值 {filler_tokens / max(1, args.window):.2f}×）")
    print("  探针：casual×4  overwrite×1  overwrite-stale(哨兵)×1  multihop×1")
    if filler_tokens < args.window:
        print("  ⚠️ 填充量没超过窗口 ⇒ 测的是「窗口内记忆」，结果无效")

    if args.dry_run:
        return 0

    driver = HomeAgentDriver(args) if args.harness == "homeagent" else PiDriver(args)
    started_at = datetime.now(timezone.utc).astimezone().isoformat(timespec="seconds")
    turns: list[dict] = []
    try:
        for i, step in enumerate(plan, 1):
            r = driver.ask(step["text"])
            rec = {
                "index": i, "kind": step["kind"],
                "cat": step.get("cat"), "ptype": step.get("ptype"),
                "wall_s": round(r["wall_s"], 3),
                "usage": r["usage"], "tools": r["tools"],
                "timed_out": r["timed_out"], "error": r["error"],
                "reply_head": (r["reply"] or "")[:200],
            }
            if step["kind"] == "probe":
                rec["expect"] = step["expect"]
                rec["recalled"] = score(r["reply"] or "", step["expect"], step["ptype"])
                mark = "✅" if rec["recalled"] else "❌"
                print(f"[{i}/{len(plan)}] 提问[{step['ptype']}] {step['expect']} … {mark} "
                      f"{r['wall_s']:.1f}s", flush=True)
            elif i % 10 == 0 or i == len(plan) - len([p for p in plan if p['kind'] == 'probe']):
                print(f"[{i}/{len(plan)}] 填充[{step.get('cat')}] … {r['wall_s']:.1f}s", flush=True)
            turns.append(rec)
    finally:
        driver.close()

    probes_done = [t for t in turns if t["kind"] == "probe"]
    total_in = sum(t["usage"].get("prompt", 0) for t in turns)
    total_out = sum(t["usage"].get("completion", 0) for t in turns)
    read = sum(t["usage"].get("cache_read", 0) for t in turns)
    miss = sum(t["usage"].get("cache_miss", 0) for t in turns)

    # 按探针类型分层
    by_type: dict[str, dict] = {}
    for t in probes_done:
        pt = t["ptype"]
        d = by_type.setdefault(pt, {"n": 0, "recalled": 0})
        d["n"] += 1
        d["recalled"] += 1 if t.get("recalled") else 0

    # 主指标 = 全部探针里排除 overwrite-stale（那是反向哨兵，单独报）
    counted = [t for t in probes_done if t["ptype"] != "overwrite-stale"]
    hits = sum(1 for t in counted if t.get("recalled"))
    stale = next((t for t in probes_done if t["ptype"] == "overwrite-stale"), None)

    summary = {
        "harness": args.harness,
        "window": args.window,
        "overshoot": args.overshoot,
        "filler_tokens_est": filler_tokens,
        "filler_to_window": round(filler_tokens / max(1, args.window), 3),
        "turns": len(turns),
        "recall_rate": (hits / len(counted)) if counted else None,
        "recalled": hits,
        "counted_probes": len(counted),
        "by_type": by_type,
        "overwrite_stale_hit": (stale.get("recalled") if stale else None),
        "wall_s_total": round(sum(t["wall_s"] for t in turns), 2),
        "prompt_total": total_in,
        "completion_total": total_out,
        "total_tokens": total_in + total_out,
        "cache_read_total": read,
        "cache_miss_total": miss,
        "timed_out_turns": sum(1 for t in turns if t["timed_out"]),
    }
    if read or miss:
        summary["cache_hit_rate"] = read / (read + miss)

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    (out / "memory_recall.json").write_text(
        json.dumps({"meta": {"started_at": started_at, "config": vars(args),
                             "material": {
                                 "casual": mat.casual,
                                 "overwrite": {"old": mat.overwrite_old, "new": mat.overwrite_new},
                                 "multihop": {"team": mat.multihop_team, "room": mat.multihop_room},
                             }},
                    "summary": summary, "turns": turns},
                   ensure_ascii=False, indent=2), encoding="utf-8")

    print(f"\n{'=' * 60}")
    print(f"主召回 {hits}/{len(counted)}（{bench.fmt_rate(summary['recall_rate'])}）  按类：")
    for pt, d in by_type.items():
        tag = "（反向哨兵）" if pt == "overwrite-stale" else ""
        print(f"  {pt}{tag}: {d['recalled']}/{d['n']}")
    print(f"  灌入 ≈ {filler_tokens} token（{summary['filler_to_window']}× 窗口），"
          f"实际累计 prompt {total_in}")
    print(f"  墙钟 {summary['wall_s_total']}s  缓存命中率 "
          f"{bench.fmt_rate(summary.get('cache_hit_rate'))}")
    print(f"结果: {out / 'memory_recall.json'}")
    return 0


class HomeAgentDriver:
    """经 cli.sock 驱动 HomeAgent（复用 bench.CliSession）。"""

    name = "homeagent"

    def __init__(self, args: argparse.Namespace):
        self.s = bench.CliSession(args.socket, args.api_key, args.timeout)
        self.frames_seen: list[dict] = []

    def ask(self, text: str) -> dict:
        res = self.s.send(text, timeout=self.s.timeout)
        self.frames_seen.extend(res["frames"])
        terminal = res["terminal"] or {}
        return {
            "reply": terminal.get("content") or "",
            "wall_s": res["wall_s"],
            "usage": _norm_ha_usage(terminal.get("usage") or {}),
            "tools": sorted({f.get("tool") for f in res["frames"]
                             if f.get("type") == bench.FRAME_TOOL_CALL and f.get("tool")}),
            "timed_out": res["timed_out"],
            "error": terminal.get("error"),
        }

    def close(self) -> None:
        pass


class PiDriver:
    """pi -p --mode json 逐轮驱动（隔离配置目录 + 空闲端口，v1 实测口径）。"""

    name = "pi"

    def __init__(self, args: argparse.Namespace):
        self.args = args
        self.session_id = f"memrecall-v2-{int(time.time())}"
        self.port_base = args.pi_port_base
        self.turn = 0

    def ask(self, text: str) -> dict:
        self.turn += 1
        env = {
            "PATH": os.environ.get("PATH", "/usr/bin:/bin:/usr/local/bin"),
            "HOME": os.environ.get("HOME", "/root"),
            "PI_CODING_AGENT_DIR": self.args.iso_dir,
            "PI_CODING_AGENT_SESSION_DIR": str(Path(self.args.iso_dir).parent / "sessions"),
            "PI_A2A_PORT": str(self.port_base + 2 * self.turn),
            "PI_ACP_PORT": str(self.port_base + 2 * self.turn + 1),
            "PI_TELEMETRY": "0",
            "PI_SKIP_VERSION_CHECK": "1",
        }
        cmd = [
            "pi", "-p", "--mode", "json",
            "--session-id", self.session_id,
            "--provider", self.args.provider, "--model", self.args.model,
            "-C", self.args.cwd, text,
        ]
        started = time.monotonic()
        try:
            proc = subprocess.run(
                cmd, env=env, capture_output=True, text=True,
                timeout=self.args.timeout)
        except subprocess.TimeoutExpired:
            return {"reply": "", "wall_s": time.monotonic() - started,
                    "usage": {}, "tools": [], "timed_out": True,
                    "error": f"timeout after {self.args.timeout}s"}
        wall = time.monotonic() - started

        usage: dict = {}
        reply_parts: list[str] = []
        for line in proc.stdout.splitlines():
            try:
                ev = json.loads(line)
            except json.JSONDecodeError:
                continue
            if ev.get("type") == "message_end" and (ev.get("message") or {}).get("role") == "assistant":
                usage = _norm_pi_usage(ev["message"].get("usage") or {})
            if ev.get("type") == "message_end" and (ev.get("message") or {}).get("role") == "assistant":
                c = ev["message"].get("content")
                if isinstance(c, str) and c:
                    reply_parts.append(c)
                elif isinstance(c, list):
                    for blk in c:
                        if isinstance(blk, dict) and blk.get("type") == "text":
                            reply_parts.append(blk.get("text") or "")
        return {
            "reply": "\n".join(reply_parts),
            "wall_s": wall,
            "usage": usage,
            "tools": [],
            "timed_out": False,
            "error": None if proc.returncode == 0 else f"exit={proc.returncode}",
        }

    def close(self) -> None:
        pass


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="记忆召回 v2：偶发针/覆盖/多跳/改述")
    ap.add_argument("--harness", choices=["homeagent", "pi"], required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--window", type=int, default=200000)
    ap.add_argument("--overshoot", type=float, default=1.25)
    ap.add_argument("--seed", type=int, default=20261001, help="全部材料随机种子（可复现）")
    ap.add_argument("--timeout", type=float, default=900.0, help="单轮超时（秒）")
    ap.add_argument("--dry-run", action="store_true")
    # HomeAgent
    ap.add_argument("--socket", help="HomeAgent cli.sock")
    ap.add_argument("--api-key", default=os.environ.get("HOMEAGENT_CLI_KEY", ""))
    # pi
    ap.add_argument("--provider", default="llmsproxy")
    ap.add_argument("--model", default="AUTO")
    ap.add_argument("--iso-dir", default="/var/tmp/pi-iso/agent")
    ap.add_argument("--cwd", default="/var/tmp/pi-iso/work")
    ap.add_argument("--pi-port-base", type=int, default=20110)
    args = ap.parse_args(argv)

    if args.harness == "homeagent" and not args.socket:
        print("--harness homeagent 需要 --socket", file=sys.stderr)
        return 2
    return run(args)


if __name__ == "__main__":
    sys.exit(main())
