#!/usr/bin/env python3
"""把两个 harness 的标定结果并排对比。

用法：
    python3 compare.py --a /var/tmp/cmp/ha --b /var/tmp/cmp/pi \
        --label-a HomeAgent --label-b pi --out /var/tmp/cmp

设计要点（都是踩过坑之后的纪律）：

1. **两侧必须跑同一套任务、同一个模型**。本脚本会检查任务 id 集合是否一致，
   不一致就明确报出来 —— 悄悄对比不同任务集是产生假结论的最快方式。

2. **口径差异必须先归一，再比。** 实测：
      HomeAgent 的 prompt 含缓存（OpenAI 口径）
      pi 的 input 是未命中侧（Anthropic 口径，totalTokens = input+read+output）
   两者若直接比，会给 pi 系统性低估。归一在各自 runner 里做（见 pi_bench.py
   的 _usage_of），本脚本只比归一后的字段。

3. **命中率无数据时写「—」而不是 0%**。与内核 usageLedger 同一约定。

4. **成本栏位**：本机网关不计费（cost 全 0），所以不编美元数字；
   只报 token 与墙钟 —— 有数就报数，没数就不报。
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


def load(d: str) -> dict:
    p = Path(d) / "results.json"
    if not p.exists():
        print(f"缺少 {p}", file=sys.stderr)
        sys.exit(2)
    try:
        return json.loads(p.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        print(f"读 {p} 失败: {exc}", file=sys.stderr)
        sys.exit(2)


def fmt(n, suffix: str = "") -> str:
    if n is None:
        return "—"
    try:
        n = float(n)
    except (TypeError, ValueError):
        return "—"
    if n >= 1000000:
        return f"{n / 1000000:.2f}M{suffix}"
    if n >= 1000:
        return f"{n / 1000:.1f}k{suffix}"
    return f"{n:.0f}{suffix}"


def rate(v) -> str:
    return "—" if v is None else f"{v * 100:.1f}%"


def main() -> int:
    ap = argparse.ArgumentParser(description="两个 harness 的标定结果并排对比")
    ap.add_argument("--a", required=True, help="A 的结果目录（如 HomeAgent）")
    ap.add_argument("--b", required=True, help="B 的结果目录（如 pi）")
    ap.add_argument("--label-a", default="A")
    ap.add_argument("--label-b", default="B")
    ap.add_argument("--out", help="在此目录写 compare.md（默认不写文件）")
    args = ap.parse_args()

    A, B = load(args.a), load(args.b)
    ra = {r["id"]: r for r in A["records"]}
    rb = {r["id"]: r for r in B["records"]}

    # 纪律 1：任务集必须一致，否则比了也没意义。
    only_a = sorted(set(ra) - set(rb))
    only_b = sorted(set(rb) - set(ra))
    shared = [i for i in ra if i in rb]

    L = []
    L.append(f"# {args.label_a} vs {args.label_b} —— 能力标定对比")
    L.append("")
    L.append(f"- 结果目录：`{args.a}` vs `{args.b}`")
    L.append(f"- 任务集：{args.label_a} {len(ra)} 个 / {args.label_b} {len(rb)} 个，共同 {len(shared)} 个")
    L.append(f"- 模型：{args.label_a} `{A['meta'].get('socket')}` / {args.label_b} `{B['meta'].get('socket')}`")
    L.append("")
    if only_a or only_b:
        L.append(f"> ⚠️ 任务集不完全一致：仅 {args.label_a}：{only_a}；仅 {args.label_b}：{only_b}")
        L.append("")

    L.append("## 汇总（仅共同任务）")
    L.append("")
    L.append(f"| 指标 | {args.label_a} | {args.label_b} |")
    L.append("|---|---|---|")

    def agg(records: dict, ids: list) -> dict:
        rs = [records[i] for i in ids if i in records]
        n = len(rs)
        ok = sum(1 for r in rs if r["ok"])
        wall = sum(r["wall_s"] for r in rs)
        tools = sum(r["tool_calls"] for r in rs)
        tok = sum(r["usage_task"].get("total", 0) for r in rs)
        rd = sum(r["usage_task"].get("cache_read", 0) for r in rs)
        ms = sum(r["usage_task"].get("cache_miss", 0) for r in rs)
        return {
            "n": n, "ok": ok,
            "pass": (ok / n) if n else None,
            "wall": wall, "tools": tools, "tok": tok,
            "rate": (rd / (rd + ms)) if (rd or ms) else None,
        }

    aa, ab = agg(ra, shared), agg(rb, shared)
    L.append(f"| 通过 | {aa['ok']}/{aa['n']}（{rate(aa['pass'])}）| {ab['ok']}/{ab['n']}（{rate(ab['pass'])}）|")
    L.append(f"| 总墙钟 | {aa['wall']:.1f}s | {ab['wall']:.1f}s |")
    L.append(f"| 工具调用总数 | {aa['tools']} | {ab['tools']} |")
    L.append(f"| token 合计 | {fmt(aa['tok'])} | {fmt(ab['tok'])} |")
    L.append(f"| 缓存命中率 | {rate(aa['rate'])} | {rate(ab['rate'])} |")
    L.append("")

    L.append("## 逐任务")
    L.append("")
    L.append(f"| 任务 | 维度 | {args.label_a} | {args.label_b} | {args.label_a} token | {args.label_b} token |")
    L.append("|---|---|---|---|---|---|")
    for i in shared:
        a, b = ra[i], rb[i]
        L.append(
            f"| {i} | {a.get('dimension','?')} | {'✅' if a['ok'] else '❌'} {a['wall_s']:.1f}s "
            f"| {'✅' if b['ok'] else '❌'} {b['wall_s']:.1f}s "
            f"| {fmt(a['usage_task'].get('total'))} | {fmt(b['usage_task'].get('total'))} |"
        )
    L.append("")

    # 失败原因要留在报告里，否则「谁赢了」没有可查证的依据
    fails = [(i, ra[i]) for i in shared if not ra[i]["ok"]] + \
            [(i, rb[i]) for i in shared if not rb[i]["ok"]]
    if fails:
        L.append("## 失败详情")
        L.append("")
        for i, r in fails:
            side = args.label_a if r in [ra[x] for x in shared] else args.label_b
            L.append(f"- **{i}**（{side}）：{r['why']}；error={r.get('error')}")
        L.append("")

    L.append("## 读表须知")
    L.append("")
    L.append("- **token 不可直接比大小**：两侧 harness 的系统提示与工具表体积不同，")
    L.append("  基线开销本来就不一样。要比的是「同一 harness 内的变化」与「通过率」。")
    L.append("- **缓存命中率已归一**：HomeAgent 的 prompt 含缓存（OpenAI 口径），")
    L.append("  pi 的 input 是未命中侧（Anthropic 口径）—— 各自 runner 里已归一成")
    L.append("  `cache_read / (cache_read + cache_miss)`。未报缓存时显示「—」而非 0%。")
    L.append("- **成本**：本机网关不计费，故不报美元；只报 token 与墙钟。")

    text = "\n".join(L)
    print(text)
    if args.out:
        p = Path(args.out) / "compare.md"
        p.write_text(text + "\n", encoding="utf-8")
        print(f"\n已写: {p}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
