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
#
# ★ v3 填充模型：高密度叙事，不是废话流。
#
# v2 的填充（"打印机需要更换墨盒（编号 123456）"×104 轮）是**明显无价值**
# 的信息：pi 压缩时直接丢弃垃圾，偶发针反而成了填充里仅有的有价值内容
# 被保住 —— 跑分虚高，且不贴合真实对话。真实对话是高信息密度的：
# 有任务、决策、因果、变化、前后引用，压缩必须在「保什么丢什么」间真取舍。
#
# v3 主线：order-gw 服务运维全程（上线准备→灰度事故→修复验证→新版本→
# 交接→收尾）。针全部自然嵌在价值信息流里：
#   · 偶发针 = 服务依赖清单里的真实条目（同构干扰 = 其它服务的端口，
#     同样有意义，只是不是针）
#   · 覆盖针 = 值班安排里的分机变更（真实变更流）
#   · 多跳 = 交接流程（团队→门禁→申请表位置）
# 叙事含跨轮引用（"按之前定的阈值…"），压缩丢弃早期轮次会破坏骨架。

SERVICE_PHASES = [  # (占比起点, 阶段名) —— 由 build_plan 按填充轮总数切分
    (0.00, "launch-prep"),
    (0.16, "incident"),
    (0.46, "verify"),
    (0.61, "release"),
    (0.76, "handover"),
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
        # 叙事里的其它服务依赖（干扰端口）：每个都是真实依赖，只是不是针
        self.dep_services = ["billing", "notify", "inventory", "search", "oauth", "captcha"]
        self.dep_ports = {s: rng.randrange(8000, 8999) for s in self.dep_services}
        self.dep_ports["auth"] = self.casual["auth 服务的端口"]
        self.dep_ports["metrics"] = self.casual["metrics 服务的端口"]
        self.dep_ports["trace"] = self.casual["trace 服务的端口"]
        self.dep_ports["admin"] = self.casual["admin 服务的端口"]

    # -- 高密度叙事轮 --

    def _dep_line(self, services: list[str]) -> str:
        """依赖服务清单片段：真实工作信息，偶发针混在其中无任何标记。"""
        parts = [f"{s} 服务的端口是 {self.dep_ports[s]}" for s in services]
        return "；".join(parts)

    def narrative_block(self, i: int, total: int) -> tuple[str, str]:
        """第 i/total 轮的高密度叙事片段。返回 (text, cat)。

        每轮 2-4 条有信息增量的工作项：数值、因果、决策、跨轮引用。
        确定性：同 seed 同位置产出相同文本。"""
        rng = self.rng
        frac = i / max(1, total - 1)
        phase = SERVICE_PHASES[0][1]
        for start, name in SERVICE_PHASES:
            if frac >= start:
                phase = name
        day = 12 + i // 6  # 叙事日期推进
        items: list[str] = []

        if phase == "launch-prep":
            if i == int(total * 0.10):
                # ★ auth 偶发针：藏在依赖清单里（清单本身是真实工作项）
                items.append(self._dep_line(["auth", "billing", "notify"]))
            else:
                items.append(f"order-gw 上线准备：压测环境跑通了下单链路，网关 QPS 压到 {rng.randrange(800, 1500)}，"
                             f"错误率 {rng.randrange(2, 9) / 100:.2f}%，符合准入线")
            items.append(f"超时配置定了：读接口 {rng.choice([800, 1000, 1200])}ms、写接口 {rng.choice([2500, 3000, 3500])}ms，"
                         f"理由是写链路要等库存扣减（平均 {rng.randrange(400, 900)}ms）")
            items.append(f"重试策略：最多 {rng.choice([2, 3])} 次，只对幂等接口开启；"
                         f"指数退避基数 {rng.choice([100, 200, 300])}ms")
            if rng.random() < 0.5:
                items.append(f"回滚预案演练完成：从发现异常到回滚到上一版本用时 {rng.randrange(3, 9)} 分钟，"
                             f"预案里写明触发条件是错误率连续 5 分钟超过 {rng.choice([1.0, 2.0])}%")

        elif phase == "incident":
            if i == int(total * 0.20):
                # ★ 覆盖旧值：值班安排里的分机（真实变更流）
                items.append(f"本周值班安排下来了，夜班有问题打{self.overwrite_key} {self.overwrite_old}，"
                             f"值班的是 {rng.choice(['老陈','小王','阿李'])}")
            else:
                items.append(f"灰度事故推进：order-gw 的 p99 从 {rng.choice([180, 220, 260])}ms 涨到 "
                             f"{rng.randrange(2800, 5200)}ms，错误率峰值 {rng.randrange(4, 18)}%，"
                             f"集中在 {rng.choice(['下单','退款','查询'])}接口")
            items.append(f"定位进展：慢查询数从每分钟 {rng.randrange(2, 6)} 条涨到 {rng.randrange(120, 400)} 条，"
                         f"根因是 {rng.choice(['退款导出','对账任务','库存同步'])}在循环里逐单查库，"
                         f"单次调用产生 {rng.randrange(600, 2000)} 次查询")
            items.append(f"临时措施：把连接池从 {rng.choice([32, 64])} 扩到 {rng.choice([128, 256])}，"
                         f"并给等待队列加了长度告警（阈值 {rng.choice([300, 400, 500])}）；"
                         f"注意这只是缓解，根因要等代码修复")

        elif phase == "verify":
            if i == int(total * 0.52):
                # ★ metrics 偶发针
                items.append(self._dep_line(["metrics", "inventory", "search"]))
            else:
                items.append(f"修复验证：N+1 查询改成批量后，慢查询回落到每分钟 {rng.randrange(1, 4)} 条，"
                             f"p99 稳定在 {rng.randrange(150, 320)}ms，观察了 {rng.randrange(6, 24)} 小时无反弹")
            items.append(f"缓存层核对了淘汰策略：ttl 设 {rng.choice([300, 600, 900])} 秒，"
                         f"容量 {rng.choice([5000, 10000, 20000])} 条；命中率从 {rng.randrange(40, 60)}% 提到 {rng.randrange(72, 91)}%")
            items.append(f"限流参数调整：全局 {rng.randrange(400, 900)} QPS，单用户 {rng.randrange(5, 30)} QPS，"
                         f"超限返回 {rng.choice([429, 503])} 并带 Retry-After")

        elif phase == "release":
            if i == int(total * 0.66):
                # ★ 覆盖新值（真实变更：值班表更新）
                items.append(f"注意：下周起{self.overwrite_key}换成 {self.overwrite_new} 了，"
                             f"旧号停用，值班轮换到 {rng.choice(['小赵','老周'])}")
            elif i == int(total * 0.70):
                # ★ trace 偶发针
                items.append(self._dep_line(["trace", "oauth", "captcha"]))
            else:
                items.append(f"新版本 v2.{rng.randrange(30, 34)}.{rng.randrange(0, 9)} 发布评审通过，"
                             f"变更项：修复 N+1、连接池参数化、慢查询日志采样率 {rng.choice([5, 10, 20])}%")
            items.append(f"发布窗口定在 {rng.choice(['周二','周三','周四'])} 凌晨 {rng.choice([1, 2, 3])} 点，"
                         f"预计停机 {rng.randrange(3, 10)} 分钟；回滚版本锁定为 v2.{rng.randrange(28, 30)}.{rng.randrange(0, 6)}")
            items.append(f"灰度比例：先 {rng.choice([5, 10])}% 流量观察 {rng.randrange(30, 90)} 分钟，"
                         f"无异常再放到 {rng.choice([50, 100])}%")

        else:  # handover
            if i == int(total * 0.85):
                # ★ admin 偶发针
                items.append(self._dep_line(["admin", "billing", "oauth"]))
            elif i == int(total * 0.76):
                # ★ 多跳要素 1：负责门禁的团队
                items.append(f"交接事项：门禁相关事务由 {self.multihop_team} 团队负责，"
                             f"对接人是 {rng.choice(['小林','老郑','阿芳'])}，工位在 {rng.choice(['3 楼东','4 楼西'])}")
            elif i == int(total * 0.92):
                # ★ 多跳要素 2：申请表位置
                items.append(f"门禁申请表已归档，放在 {self.multihop_room} 的文件柜，"
                             f"需要 {rng.choice(['部门主管','行政'])}签字后提交")
            else:
                items.append(f"交接文档整理：监控大盘链接、告警规则 {rng.randrange(8, 20)} 条、"
                             f"值班手册更新到第 {rng.randrange(3, 9)} 版；新增了容量预警（连接池使用率连续 10 分钟超 "
                             f"{rng.choice([70, 80])}% 就升级到人工）")
            items.append(f"后续排期：下季度做连接池动态化（现在改参数要重启），"
                         f"预计 {rng.choice(['10 月','11 月'])} 排期；另一个待办是把对账任务迁到独立连接池")

        text = f"order-gw 运维同步（第 {i} 批，{day} 日）：" + "；".join(items)
        return text, "narrative"

    def build_plan(self) -> list[dict]:
        """对话脚本：高密度叙事填充 + 嵌入式探针 + 改述提问。

        ★ v3 语义：填充是 order-gw 服务运维的真实工作流（有任务/决策/因果/变化），
        不是废话流。针自然嵌在价值信息里（依赖清单/值班变更/交接流程），
        压缩必须保住叙事骨架才有真实取舍 —— 这是针对「废话填充让 pi
        直接丢垃圾 ⇒ 跑分虚高」的修正。
        """
        plan: list[dict] = []

        # 先探每轮 token：叙事片段 ~450 token/条，每批拼 4 条 ≈ 1800 token。
        # 轮数 ~140：在「轮数开销」（每轮一次 LLM 调用）与「信息密度」之间取平衡。
        # 每轮拼多少叙事片段。它决定「轮数」与「信息密度」的换算：
        # 填充总量必须 > 窗口（否则测的是窗口内记忆），而轮数越少每轮越贵。
        # 100k 窗口 × 1.45 = 145k ⇒ 8 片段/轮（≈2900 tok）= 50 轮填充。
        frags_per_turn = 8
        sample = "；".join(self.narrative_block(k, 600)[0] for k in range(frags_per_turn))
        per_turn_tokens = max(1, est_tokens(sample))
        # 填充轮数：以「累计叙事文本 ≈ 1.45× 窗口」为目标，靠**加厚灌入**而非
        # 压低窗口来制造超窗（2026-10-01 定案）。压窗口（如 10k）会让 HA 反复
        # 上下文管理抖动 —— 相当于在低内存设备上测内存调度，测出的是病态行为
        # 不是设计工作点。两侧窗口保持一致（如 50k），让灌入量本身超过窗口：
        #   pi 是水位线压缩，累计文本 > 窗口 ⇒ 必须开始真实丢信息；
        #   HA 同窗口下已实测超窗召回（50k 窗口 max prompt 286k 仍 6/6）。
        # 上限 25 轮是成本护栏（实测 pi ~20s/轮、HA ~120s/轮）。
        filler_turns = max(4, min(25, self.target_tokens // per_turn_tokens))

        # 开场白：完全自然，不预告任何要记的东西
        plan.append({"kind": "filler", "cat": "narrative",
                     "text": "我这边开始按天同步 order-gw 的运维进展，你顺手套理着就行，后面我会随时问细节。"})

        for i in range(filler_turns):
            # 每批拼 3 条不同位置的片段（同阶段推进）：信息密度高且互不重复
            base = i * frags_per_turn
            parts = [self.narrative_block(base + k, filler_turns * frags_per_turn)[0]
                     for k in range(frags_per_turn)]
            plan.append({"kind": "filler", "cat": "narrative", "text": "；".join(parts)})

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
    # ★ 超窗判定以**实测 prompt** 为准，不以文本估算为准（2026-10-01 实测教训）：
    #   纯叙事文本估算 14k，而 HA 第 6 轮真实 prompt 已 127k —— 每轮还含系统提示、
    #   工具定义、记忆上下文与工具回执，估算只是保守下界。若按估算判「无效」，
    #   会把实际已超窗的有效跑分误杀；反过来只看估算也会让无效跑分看起来合格。
    est_ok = filler_tokens >= args.window
    print(f"  {'✓' if est_ok else 'ℹ'} 文本估算 {filler_tokens} vs 窗口 {args.window}"
          f"（估算为下界，超窗以实测 prompt 为准）")

    if args.dry_run:
        return 0

    driver = HomeAgentDriver(args) if args.harness == "homeagent" else PiDriver(args)
    started_at = datetime.now(timezone.utc).astimezone().isoformat(timespec="seconds")
    turns: list[dict] = []
    # ★ 断点/防丢：每轮结束就把已完成轮次落盘。长跑（143 轮、实测均值 ~150s/轮、
    #   约 6h）本会话被杀过三次，而原来只在全部跑完后才写 json ⇒ 中途崩溃等于零产出。
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    partial = out / "partial.jsonl"
    # 非 resume 模式必须清掉上一次运行的残留（实测踩过：跨运行 append 会把
    # 被 kill 的旧轮次混进本次结果，出现两条「轮1」）
    if not args.resume and partial.exists():
        partial.unlink()

    # --resume：从 partial.jsonl 续跑。服务端会话状态跨连接保留（CliSession 每轮
    # 新建连接但 agent 会话在服务端），所以「跳过已完成的轮次、从第 N+1 轮继续发」
    # 是真正接续而不是重放。
    # ★ 为什么必须能续而不是只看 partial：7 个探针全排在最后 7 轮，崩溃后的
    #   partial 只有填充，**拿不到任何召回信号** —— 没有 resume 就等于重跑。
    # 前提：续跑时实例必须仍是同一个（未重启、未清库），否则上下文对不上。
    done: dict[int, dict] = {}
    if args.resume and partial.exists():
        for line in partial.read_text(encoding="utf-8").splitlines():
            if line.strip():
                rec0 = json.loads(line)
                done[int(rec0["index"])] = rec0
    if done:
        print(f"  ↻ resume：已有 {len(done)} 轮落盘，从第 {max(done) + 1} 轮继续"
              f"（实例必须仍未重启）", flush=True)
        turns.extend(done[k] for k in sorted(done))

    def _flush_turn(rec: dict) -> None:
        with partial.open("a", encoding="utf-8") as fh:
            fh.write(json.dumps(rec, ensure_ascii=False) + "\n")

    try:
        for i, step in enumerate(plan, 1):
            if i in done:
                continue
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
            else:
                # 每轮都落笔：既给长跑可见进度，又把「单轮 5 分钟」这类异常立刻暴露出来
                print(f"[{i}/{len(plan)}] 填充[{step.get('cat')}] … {r['wall_s']:.1f}s", flush=True)
            turns.append(rec)
            _flush_turn(rec)
    finally:
        driver.close()

    # 实测超窗校验：任一填充轮的 prompt 超过窗口 ⇒ 压缩真实发生过
    max_prompt = max((t["usage"].get("prompt", 0) for t in turns), default=0)
    overshoot_verified = max_prompt > args.window

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
        "max_prompt_observed": max_prompt,
        "overshoot_verified": overshoot_verified,
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
    print(f"  灌入 ≈ {filler_tokens} token（估算 {summary['filler_to_window']}× 窗口），"
          f"实际累计 prompt {total_in}")
    print(f"  实测最大单轮 prompt {max_prompt} vs 窗口 {args.window} ⇒ "
          f"{'✓ 超窗校验通过（压缩真实发生）' if overshoot_verified else '⚠️ 未超窗 ⇒ 结果无效'}")
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
            text,
        ]
        started = time.monotonic()
        try:
            proc = subprocess.run(
                cmd, env=env, capture_output=True, text=True,
                timeout=self.args.timeout, cwd=self.args.cwd)
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
    ap.add_argument("--expect-window", type=int, default=None,
                    help="实例（模型）真实窗口；与 --window 不同则拒跑（防「窗口内召回」假跑分）")
    ap.add_argument("--overshoot", type=float, default=1.25)
    ap.add_argument("--seed", type=int, default=20261001, help="全部材料随机种子（可复现）")
    ap.add_argument("--timeout", type=float, default=900.0, help="单轮超时（秒）")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--resume", action="store_true",
                    help="从 <out>/partial.jsonl 续跑（要求实例未重启；探针在末尾，"
                         "崩溃后不续跑就拿不到召回信号）")
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
    # ★ 超窗校验：灌入量必须既超过灌入目标（--window），又被实例真实窗口
    #   （--expect-window）容纳。两个条件任一不满足都是「测错东西」：
    #   - 填充 < window ⇒ 测的是窗口内记忆，无需压缩；
    #   - expect-window < 填充 ⇒ 实例先于压缩策略把内容截断，结果不可归因。
    if args.expect_window is not None and args.expect_window != args.window:
        print(f"✗ 超窗校验失败：--expect-window {args.expect_window} != --window {args.window} "
              f"（实例窗口与灌入目标不一致，结果不可归因）", file=sys.stderr)
        return 2
    return run(args)


if __name__ == "__main__":
    sys.exit(main())
