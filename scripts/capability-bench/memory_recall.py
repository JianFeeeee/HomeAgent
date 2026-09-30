#!/usr/bin/env python3
"""记忆召回测试器 —— 多轮对话超出上下文窗口后，早期内容还记得吗？

要回答的问题
============

各 harness 在上下文被塞满时采取不同策略：

  HomeAgent  按**向量相关性**裁剪（踢掉低相关事件），保留的应当是与当前
             提问相关的那些 —— 所以「早期但相关」的内容**有理由**活下来。
  pi         压缩（compaction）：把老对话摘要成一段。

这两种策略对「早期事实的召回」影响完全不同，而它恰恰是长期助手的核心能力。
本工具把它变成可测量的数字。

方法
====

1. 在对话的**不同深度**埋若干「针」（distinctive 事实，如 `MAGENTA-7742`）；
2. 灌入足量**语义无关**的填充对话，使历史超过目标窗口；
3. 逐个提问「针」的值；
4. 计分：命中率（按深度分层）。

控制变量（都是踩过坑之后的纪律）：

  · **填充必须真的撑爆窗口**，否则测的是「窗口内记忆」而不是「超出后的召回」。
    本工具按 token 估算控制总量，并在报告里如实写上实际灌入量与窗口的比值。
  · **每一轮的文本都必须不同**：HomeAgent 会把完全相同的输入判为 duplicate
    直接跳过（`skipped:true`），那样根本没进历史。
  · **针与提问必须不同字**：否则可能是模式匹配而非记忆召回。
  · 填充用**固定种子**生成 ⇒ 可复现，两侧看到的材料逐字相同。

用法
====

    # 先在小窗口验证仪器（快）
    python3 memory_recall.py --harness homeagent --socket /path/cli.sock \
        --api-key KEY --window 20000 --out /var/tmp/mem/small

    # 正式：200k 窗口
    python3 memory_recall.py --harness pi --window 200000 --out /var/tmp/mem/pi-200k
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

# ── 针：与填充毫无语义关联，便于判定是否真的"记住" ──
NEEDLE_POOL = [
    ("项目代号", "MAGENTA-7742"),
    ("门禁口令", "CORAL-9918"),
    ("备用端口", "P-44821"),
    ("仓库别名", "TANGERINE-QX"),
    ("调度密钥", "VIOLET-3057"),
    ("备份标记", "AZURE-6603"),
]

# ── 填充材料：语义上无关的中性文本，逐句不重复 ──
FILLER_TOPICS = [
    "今天的天气看上去不错", "窗外的树叶在动", "桌上的杯子是陶瓷的",
    "打印机需要更换墨盒", "走廊的灯换了新的", "会议室的椅子有些松动",
    "楼下的咖啡店换了招牌", "电梯里贴了新的通知", "前台的绿植长得很好",
    "午休时间走廊很安静", "空调的温度设定在二十六度", "储物柜的编号是连续的",
]


def filler_block(rng: random.Random, n: int) -> str:
    """生成 n 句互不相同的中性填充。

    ★ 必须是**单行**：cli.sock 的协议是按行读的（scanner.Scan()），
    一条消息里有换行会被拆成几十条独立消息 —— 各轮历史完全乱掉，
    且表现为连接被提前关闭（BrokenPipe）。本函数第一版用了 "\n".join
    就踩了这个坑。
    """
    out = []
    for i in range(n):
        t = rng.choice(FILLER_TOPICS)
        out.append(f"{t}（第 {i} 条记录，编号 {rng.randrange(100000, 999999)}）")
    return "；".join(out)


def est_tokens(s: str) -> int:
    """与内核同口径的估算（min(字节, 2×rune)）—— 用于控制灌入量。

    这里刻意复刻内核公式（而不是换个"更准"的）：两侧的窗口都是按这个口径
    配置的，用别的口径估会得出不一致的"是否已超窗"。
    """
    b = len(s.encode("utf-8"))
    r = len(s) * 2
    return max(1, min(b, r)) if s else 0


class HomeAgentDriver:
    """经 cli.sock 驱动 HomeAgent（复用 bench.CliSession）。"""

    name = "homeagent"

    def __init__(self, args: argparse.Namespace):
        self.s = bench.CliSession(args.socket, args.api_key, args.timeout)
        self.frames_seen: list[dict] = []

    def ask(self, text: str) -> dict:
        r = self.s.send(text, timeout=600)
        self.frames_seen.extend(r["frames"])
        t = r["terminal"] or {}
        return {
            "reply": t.get("content") or "",
            "usage": _norm_ha_usage(t.get("usage")),
            "error": t.get("error"),
            "wall_s": r["wall_s"],
            "timed_out": r["timed_out"],
            "tools": sorted({f.get("tool") for f in r["frames"] if f.get("tool")}),
        }

    def close(self) -> None:
        self.s.close()


class PiDriver:
    """逐轮起 pi 进程，用 --session-id 续接同一会话。

    ★ 为什么是一轮一个进程：pi 的 `-p` 是「给一条提示就退出」的形态，
      多轮靠**会话文件**续接，而不是常驻进程。
    ★ 每轮必须换端口：前一轮的进程可能还没完全释放监听
      （实测：不加偏移时第二轮偶发 EADDRINUSE）。
    """

    name = "pi"

    def __init__(self, args: argparse.Namespace):
        self.args = args
        self.session_id = f"memrecall-{int(time.time())}"
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
            text,
        ]
        started = time.monotonic()
        try:
            proc = subprocess.run(
                cmd, cwd=self.args.cwd, env=env, capture_output=True, text=True,
                timeout=self.args.timeout,
            )
            stdout, finished = proc.stdout, True
        except subprocess.TimeoutExpired as exc:
            # 超时是长时对话的常态，不当崩溃：拿已经吐出的部分继续判定。
            stdout, finished = _timeout_stdout(exc), False
        wall = time.monotonic() - started

        reply, usage, tools = "", {}, []
        for line in stdout.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                ev = json.loads(line)
            except json.JSONDecodeError:
                continue
            etype = ev.get("type")
            if etype == "turn_end":
                msg = ev.get("message") or {}
                usage = _norm_pi_usage(msg.get("usage"))
                t = _pi_text(msg)
                if t:
                    reply = t
            elif etype == "message_end":
                msg = ev.get("message") or {}
                if msg.get("role") == "assistant":
                    t = _pi_text(msg)
                    if t:
                        reply = t
            elif etype == "tool_execution_start":
                tools.append(ev.get("toolName") or "")
        return {
            "reply": reply, "usage": usage, "error": None if finished else "timeout",
            "wall_s": wall, "timed_out": not finished, "tools": sorted({t for t in tools if t}),
        }

    def close(self) -> None:
        return


def _pi_text(msg: dict) -> str:
    c = msg.get("content")
    if isinstance(c, str):
        return c
    if isinstance(c, list):
        return "".join(b.get("text", "") for b in c
                       if isinstance(b, dict) and b.get("type") == "text")
    return ""


def _int(v: object) -> int:
    """尽力取整数。上游字段可能是字符串/None/浮点，取不到就记 0。"""
    try:
        return int(v)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return 0


def _timeout_stdout(exc: subprocess.TimeoutExpired) -> str:
    """从超时异常里尽可能取出已经输出的部分（文本或字节）。"""
    raw = exc.stdout
    if raw is None:
        return ""
    if isinstance(raw, bytes):
        return raw.decode("utf-8", "replace")
    return str(raw)


def _norm_ha_usage(u: object) -> dict:
    """把 HomeAgent 回包的 usage 归一成统一键名。

    ★ HomeAgent 用 OpenAI 命名（prompt_tokens/completion_tokens/
      cache_read_tokens/cache_miss_tokens），而汇总代码原先按
      pi 那套（prompt/completion/...）读 ⇒ 全是 0。

    这与本项目刚在缓存规则上吃过的亏是同一类：**同名不同键**不会报错，
    只会静默给 0。所以两侧都必须过归一函数，汇总侧只认归一后的键。
    """
    u = u if isinstance(u, dict) else {}
    return {
        "prompt": _int(u.get("prompt_tokens")),
        "completion": _int(u.get("completion_tokens")),
        "total": _int(u.get("total_tokens")),
        "cache_read": _int(u.get("cache_read_tokens")),
        "cache_miss": _int(u.get("cache_miss_tokens")),
    }


def _norm_pi_usage(u: object) -> dict:
    """把 pi 的 usage 归一成与 HomeAgent 同口径（详见 pi_bench.py 的说明）。

    pi 的 input 是**未命中**输入（Anthropic 口径），HomeAgent 的 prompt 含缓存。
    """
    u = u if isinstance(u, dict) else {}
    uncached = _int(u.get("input"))
    read = _int(u.get("cacheRead"))
    out = _int(u.get("output"))
    return {
        "prompt": uncached + read, "completion": out,
        "total": _int(u.get("totalTokens")) or (uncached + read + out),
        "cache_read": read, "cache_miss": uncached,
    }


def build_plan(args: argparse.Namespace) -> list[dict]:
    """构造对话脚本：针 + 填充 + 提问。

    结构（needles 个针均匀分布）：
        前言（含第 1 个针）
        ... 填充（分成若干块，每块之间插下一个针）...
        提问（逐个问针的值）
    """
    rng = random.Random(args.seed)
    needles = NEEDLE_POOL[: args.needles]

    # 目标：填充总量 ≈ window * overshoot（默认 1.25 倍窗口 ⇒ 确实超窗）
    target_tokens = int(args.window * args.overshoot)
    # 每轮填充句数：按「一轮 ~3000 token」配，轮数由总量决定
    per_turn_sentences = 40
    sample = filler_block(rng, per_turn_sentences)
    per_turn_tokens = max(1, est_tokens(sample))
    filler_turns = max(4, target_tokens // per_turn_tokens)

    plan: list[dict] = []
    # 前置：自我介绍 + 埋针
    # 同样必须单行（协议按行读）：用「；」分隔多条针。
    preamble = (
        "你好，先做一些记录。请牢牢记住下面的信息，后面我会追问："
        + "；".join(f"我的{n}是 {v}" for n, v in needles)
    )
    plan.append({"kind": "needle", "text": preamble})

    for i in range(filler_turns):
        block = filler_block(rng, per_turn_sentences)
        # 每个填充轮都带唯一编号，避免被 dedupe 跳过
        plan.append({"kind": "filler", "text": f"补充一些日常记录（第 {i} 批）：{block}"})

    # 提问：一次一问，措辞各不相同
    for n, v in needles:
        plan.append({"kind": "probe", "text": f"我又想问一下：我之前告诉你的{n}是什么？只回答那个值本身。", "expect": v})
    # ★ 断言而非注释：cli.sock 按行读，任何一轮含换行都会被**静默**拆成
    # 多条独立消息（历史全乱，且表现为 BrokenPipe）。这属于"看起来在跑、
    # 数字全是垃圾"，所以宁可直接炸掉。
    for step in plan:
        assert "\n" not in step["text"], (
            f"第 {plan.index(step)+1} 轮含换行：协议按行读，会被拆成多条消息")
    return plan


def score(reply: str, expect: str) -> bool:
    """判定召回是否成功：值必须出现（允许模型加解释）。"""
    if not expect:
        return False
    return expect.upper() in reply.upper()


def run(args: argparse.Namespace) -> int:
    plan = build_plan(args)
    filler_tokens = sum(est_tokens(p["text"]) for p in plan if p["kind"] == "filler")
    needle_turns = [p for p in plan if p["kind"] == "needle"]
    probes = [p for p in plan if p["kind"] == "probe"]

    print(f"会话计划：{len(plan)} 轮 = 埋针 {len(needle_turns)} + 填充 "
          f"{sum(1 for p in plan if p['kind']=='filler')} + 提问 {len(probes)}")
    print(f"  填充 token 估算 ≈ {filler_tokens}（目标窗口 {args.window}，"
          f"比值 {filler_tokens / max(1, args.window):.2f}×）")
    if filler_tokens < args.window:
        print("  ⚠️ 填充量没超过窗口 ⇒ 本跑测的是「窗口内记忆」，不是「超窗后召回」")

    if args.dry_run:
        return 0

    driver = HomeAgentDriver(args) if args.harness == "homeagent" else PiDriver(args)
    started_at = datetime.now(timezone.utc).astimezone().isoformat(timespec="seconds")
    turns: list[dict] = []
    try:
        for i, step in enumerate(plan, 1):
            r = driver.ask(step["text"])
            rec = {
                "index": i, "kind": step["kind"], "wall_s": round(r["wall_s"], 3),
                "usage": r["usage"], "tools": r["tools"],
                "timed_out": r["timed_out"], "error": r["error"],
                "reply_head": (r["reply"] or "")[:200],
            }
            if step["kind"] == "probe":
                rec["expect"] = step["expect"]
                rec["recalled"] = score(r["reply"], step["expect"])
                mark = "✅" if rec["recalled"] else "❌"
                print(f"[{i}/{len(plan)}] 提问 {step['expect']} … {mark} "
                      f"{r['wall_s']:.1f}s", flush=True)
            elif step["kind"] == "needle":
                print(f"[{i}/{len(plan)}] 埋针 … {r['wall_s']:.1f}s", flush=True)
            elif i % 10 == 0 or i == len(plan):
                print(f"[{i}/{len(plan)}] 填充 … {r['wall_s']:.1f}s", flush=True)
            turns.append(rec)
    finally:
        driver.close()

    probes_done = [t for t in turns if t["kind"] == "probe"]
    hits = sum(1 for t in probes_done if t.get("recalled"))
    total_in = sum(t["usage"].get("prompt", 0) for t in turns)
    total_out = sum(t["usage"].get("completion", 0) for t in turns)
    read = sum(t["usage"].get("cache_read", 0) for t in turns)
    miss = sum(t["usage"].get("cache_miss", 0) for t in turns)

    summary = {
        "harness": args.harness,
        "window": args.window,
        "overshoot": args.overshoot,
        "filler_tokens_est": filler_tokens,
        "filler_to_window": round(filler_tokens / max(1, args.window), 3),
        "turns": len(turns),
        "probes": len(probes_done),
        "recalled": hits,
        "recall_rate": (hits / len(probes_done)) if probes_done else None,
        "wall_s_total": round(sum(t["wall_s"] for t in turns), 2),
        "prompt_total": total_in,
        "completion_total": total_out,
        "total_tokens": total_in + total_out,
        "cache_read_total": read,
        "cache_miss_total": miss,
        "cache_hit_rate": (read / (read + miss)) if (read or miss) else None,
        "timed_out_turns": sum(1 for t in turns if t["timed_out"]),
    }

    if read or miss:
        summary["cache_hit_rate"] = read / (read + miss)
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    (out / "memory_recall.json").write_text(
        json.dumps({"meta": {"started_at": started_at, "config": vars(args)},
                    "summary": summary, "turns": turns},
                   ensure_ascii=False, indent=2), encoding="utf-8")

    print(f"\n{'=' * 60}")
    print(f"召回 {hits}/{len(probes_done)}  "
          f"（{bench.fmt_rate(summary['recall_rate'])}）")
    print(f"  灌入 ≈ {filler_tokens} token（{summary['filler_to_window']}× 窗口），"
          f"实际累计 prompt {summary['prompt_total']}")
    print(f"  墙钟 {summary['wall_s_total']}s  缓存命中率 "
          f"{bench.fmt_rate(summary['cache_hit_rate'])}")
    print(f"结果: {out / 'memory_recall.json'}")
    return 0 if hits == len(probes_done) else 1


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="超出上下文窗口后的记忆召回测试")
    ap.add_argument("--harness", choices=["homeagent", "pi"], required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--window", type=int, default=200000, help="两侧统一配置的窗口（token）")
    ap.add_argument("--overshoot", type=float, default=1.25, help="填充量 / 窗口")
    ap.add_argument("--needles", type=int, default=4, help="埋几个针（≤6）")
    ap.add_argument("--seed", type=int, default=20260930, help="填充随机种子（可复现）")
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
    args.needles = min(args.needles, len(NEEDLE_POOL))
    return run(args)


if __name__ == "__main__":
    sys.exit(main())
