#!/usr/bin/env python3
"""HomeAgent 能力标定台 —— 经 cli.sock 驱动部署实例，记录账目与结果。

为什么要它
==========

「全面标定能力」需要一个**可复现的驱动 + 记账**，而不是手工敲几句看回复。
本台子只做三件事，每件都对应一个曾经缺失的环节：

1. **驱动**：连 cli.sock（唯一无头入口），发任务，收帧直到终态。
2. **记账**：从 `/kernel` 读累计用量，取**任务前后差值** ⇒ 单任务真实成本。
   这一步依赖内核把 usage 放进 /kernel（本轮刚补）与 cli 帧
   （本轮刚补）—— 没有它们本台子只能报「跑了多久」。
3. **判定**：按任务自带的 checker 判成功，不靠肉眼看回复。

计费口径
========

用 /kernel 的**差分**而不是单次回包的 usage，原因：
一个任务往往触发多轮 LLM 调用（工具回环），单次回包只反映最后一段。
差分是任务的真实总账。两者都会记录，便于对照。

用法
====

    # 对一个实例跑一套任务（该实例的 data 目录决定 socket 路径）
    python3 bench.py --data "${HA_DATA}" --tasks tasks.example.json --out /var/tmp/bench

    # 长时任务：把超时放大（默认 600s）
    python3 bench.py --data ... --tasks tasks.long.json --timeout 7200

安全
====

本台子**只发提示词**。任务是否写盘/发邮件由提示词与实例授权决定，
所以内置任务集刻意是只读或写入临时目录的。请勿把破坏性提示词放进来。
"""
from __future__ import annotations

import argparse
import json
import os
import socket
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

# 帧类型（与 internal/plugins/cli/plugin.go 的 writeLine 一一对应）
FRAME_RESPONSE = "response"
FRAME_ERROR = "error"
FRAME_TOOL_CALL = "tool_call"
FRAME_REASONING = "reasoning"
FRAME_CONTENT_DELTA = "content_delta"


class BenchError(RuntimeError):
    """驱动层错误（连不上、认证失败、内核没起来）。"""


class CliSession:
    """一条 cli.sock 连接。

    协议（读 internal/plugins/cli/plugin.go 的 handleConn 得到）：
      1. 连上后先发 ``/auth <key>``，等一条 ``{"type":"response","content":"authenticated"}``；
      2. 之后每发一行就是一个请求，收帧直到出现 response / error 终态。
      3. 以 ``/`` 开头的是内置命令（/kernel、/status 等），同样以 response 终结。
    """

    def __init__(self, sock_path: str, api_key: str, timeout: float):
        self.timeout = timeout
        try:
            self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self.sock.settimeout(timeout)
            self.sock.connect(sock_path)
        except OSError as exc:
            raise BenchError(f"连接 {sock_path} 失败: {exc}") from exc
        self.buf = b""
        if api_key:
            self._write(f"/auth {api_key}")
            reply = self._read_until_terminal(collect=None)
            if reply.get("type") == FRAME_ERROR:
                raise BenchError(f"认证失败: {reply.get('error')}")

    # ---- 低层读写 ----

    def _write(self, line: str) -> None:
        self.sock.sendall((line + "\n").encode("utf-8"))

    def _read_line(self) -> dict:
        """读一条 JSON 帧。非 JSON 行按旧版协议包成 response。"""
        while b"\n" not in self.buf:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise BenchError("连接被对端关闭")
            self.buf += chunk
        raw, self.buf = self.buf.split(b"\n", 1)
        text = raw.decode("utf-8", "replace").strip()
        if not text:
            return {}
        try:
            obj = json.loads(text)
            return obj if isinstance(obj, dict) else {"type": "response", "content": text}
        except json.JSONDecodeError:
            # 旧版服务器会直接回文本行
            return {"type": FRAME_RESPONSE, "content": text}

    # ---- 高层语义 ----

    def _read_until_terminal(self, collect: list | None) -> dict:
        """收帧直到终态（response/error），过程帧交给 collect 收集。"""
        while True:
            frame = self._read_line()
            if not frame:
                continue
            ftype = frame.get("type")
            if ftype in (FRAME_RESPONSE, FRAME_ERROR):
                return frame
            if collect is not None:
                collect.append(frame)

    def send(self, text: str, timeout: float | None = None) -> dict:
        """发一条消息，收全部过程帧与终态。

        返回 ``{"terminal":帧, "frames":[...], "wall_s":秒, "timed_out":bool}``。
        """
        self.sock.settimeout(timeout or self.timeout)
        frames: list = []
        started = time.monotonic()
        self._write(text)
        timed_out = False
        try:
            terminal = self._read_until_terminal(collect=frames)
        except TimeoutError:
            # 超时是长时任务的常态，不能当崩溃：如实标记并返回已有过程帧。
            timed_out = True
            terminal = {"type": FRAME_ERROR, "error": f"timeout after {timeout or self.timeout}s"}
        except BenchError as exc:
            timed_out = True
            terminal = {"type": FRAME_ERROR, "error": str(exc)}
        return {
            "terminal": terminal,
            "frames": frames,
            "wall_s": time.monotonic() - started,
            "timed_out": timed_out,
        }

    def close(self) -> None:
        try:
            self.sock.close()
        except OSError:
            pass


def read_usage(session: CliSession) -> dict:
    """用 /kernel 读内核累计用量。取不到就返回空 dict（不编数字）。"""
    try:
        res = session.send("/kernel")
    except BenchError:
        return {}
    content = (res.get("terminal") or {}).get("content") or ""
    try:
        status = json.loads(content)
    except (json.JSONDecodeError, TypeError):
        return {}
    usage = status.get("usage")
    return usage if isinstance(usage, dict) else {}


def usage_delta(before: dict, after: dict) -> dict:
    """任务前后差分。任何一侧缺数就返回 {}（宁可不报，不编）。"""
    if not before or not after:
        return {}
    keys = ("calls", "prompt", "completion", "total", "cache_read", "cache_miss", "reasoning")
    out = {}
    for k in keys:
        b, a = before.get(k), after.get(k)
        if isinstance(b, int) and isinstance(a, int):
            out[k] = a - b
    # 命中率按差分重算：分母只算差分里报过缓存的调用。
    read, miss = out.get("cache_read", 0), out.get("cache_miss", 0)
    if read or miss:
        out["cache_hit_rate"] = read / (read + miss)
    return out


def fmt_rate(rate: float | None) -> str:
    """把命中率格式化成可读文本。None（无分母）显示「—」而不是 0%。"""
    return "—" if rate is None else f"{rate * 100:.1f}%"


def check(task: dict, terminal: dict, frames: list, wall_s: float) -> tuple[bool, str]:
    """按任务自带的 checker 判成功。返回 (是否成功, 说明)。

    checker 是**显式**的：没有 checker 的任务一律判 False ——
    缺判据时「看起来答对了」不算成功，那正是假绿的来源。
    """
    chk = task.get("check")
    if not chk:
        return False, "任务没有 check 判据"
    kind = chk.get("kind")
    text = (terminal.get("content") or "")
    if kind == "output_contains":
        needle = chk.get("value", "")
        return (needle in text), f"输出{'含' if needle in text else '不含'} {needle!r}"
    if kind == "output_regex":
        import re
        m = re.search(chk.get("value", ""), text)
        return (m is not None), f"正则 {chk.get('value')!r} {'匹配' if m else '不匹配'}"
    if kind == "file_exists":
        p = Path(os.path.expanduser(chk.get("value", "")))
        return p.exists(), f"{p} {'存在' if p.exists() else '不存在'}"
    if kind == "file_contains":
        p = Path(os.path.expanduser(chk.get("value", "")))
        needle = chk.get("needle", "")
        if not p.exists():
            return False, f"{p} 不存在"
        body = p.read_text(encoding="utf-8", errors="replace")
        return (needle in body), f"{p} {'含' if needle in body else '不含'} {needle!r}"
    if kind == "tool_used":
        used = {f.get("tool") for f in frames if f.get("type") == FRAME_TOOL_CALL}
        want = chk.get("value", "")
        return (want in used), f"工具 {want!r} {'用过' if want in used else '未用'}（实际: {sorted(x for x in used if x)}）"
    if kind == "completed":
        # 只要求没超时、没报错 —— 用于「跑得完」这类长时任务。
        ok = not terminal.get("error")
        return ok, "正常结束" if ok else f"异常: {terminal.get('error')}"
    return False, f"未知 checker 种类: {kind}"


def run_task(session: CliSession, task: dict, default_timeout: float) -> dict:
    """跑一个任务并返回它的完整记录。"""
    try:
        timeout = float(task.get("timeout_s") or default_timeout)
    except (TypeError, ValueError):
        # 任务集里写了非法超时就退回默认值，而不是让整个跑批崩掉。
        timeout = default_timeout
    before = read_usage(session)
    started = time.monotonic()
    res = session.send(task["prompt"], timeout=timeout)
    after = read_usage(session)
    wall_s = time.monotonic() - started

    frames = res["frames"]
    tool_calls = [f for f in frames if f.get("type") == FRAME_TOOL_CALL]
    terminal = res["terminal"] or {}
    ok, why = check(task, terminal, frames, wall_s)

    return {
        "id": task.get("id"),
        "dimension": task.get("dimension", "uncategorized"),
        "ok": ok,
        "why": why,
        "wall_s": round(wall_s, 3),
        "timed_out": res["timed_out"],
        "error": terminal.get("error"),
        "tool_calls": len(tool_calls),
        "tools": sorted({f.get("tool") for f in tool_calls if f.get("tool")}),
        # 单次回包的用量：仅最后一段 LLM 调用（口径与 /kernel 差分不同）
        "usage_single": (terminal.get("usage") or {}),
        # 任务真实总账：/kernel 差分（含全部工具回环）
        "usage_task": usage_delta(before, after),
        "reply": (terminal.get("content") or "")[:2000],
    }


def summarize(records: list[dict]) -> dict:
    """汇总。缓存命中率只在**真的有分母**时给（否则 None，不画成 0）。

    全部走 .get()：汇总不能因为某条记录缺一个键就整个崩掉 ——
    那会把「跑了 20 个任务、第 20 个异常终止」变成「什么都没跑成」。
    """
    n = len(records)
    ok_n = sum(1 for r in records if r.get("ok"))
    read = sum((r.get("usage_task") or {}).get("cache_read", 0) for r in records)
    miss = sum((r.get("usage_task") or {}).get("cache_miss", 0) for r in records)
    report_calls = sum(1 for r in records
                       if (r.get("usage_task") or {}).get("cache_read")
                       or (r.get("usage_task") or {}).get("cache_miss"))
    return {
        "tasks": n,
        "passed": ok_n,
        "pass_rate": (ok_n / n) if n else None,
        "wall_s_total": round(sum(r.get("wall_s", 0.0) for r in records), 2),
        "tool_calls_total": sum(r.get("tool_calls", 0) for r in records),
        "prompt_total": sum((r.get("usage_task") or {}).get("prompt", 0) for r in records),
        "completion_total": sum((r.get("usage_task") or {}).get("completion", 0) for r in records),
        "total_tokens": sum((r.get("usage_task") or {}).get("total", 0) for r in records),
        "cache_read_total": read,
        "cache_miss_total": miss,
        "cache_hit_rate": (read / (read + miss)) if (read or miss) else None,
        "tasks_with_cache_data": report_calls,
        "timed_out": sum(1 for r in records if r.get("timed_out")),
        "usage_available": any(r.get("usage_task") for r in records),
    }


def render_markdown(meta: dict, summary: dict, records: list[dict]) -> str:
    """人读的报告。数字缺失时写「—」而不是 0。"""

    def fmt(v, suffix=""):
        return "—" if v is None else f"{v}{suffix}"

    rate = fmt_rate(summary["cache_hit_rate"])
    pr = "—" if summary["pass_rate"] is None else f"{summary['pass_rate'] * 100:.0f}%"
    lines = [
        "# HomeAgent 能力标定报告",
        "",
        f"- 时间：{meta['started_at']}",
        f"- socket：`{meta['socket']}`",
        f"- 任务集：`{meta['tasks_file']}`",
        f"- 单任务默认超时：{meta['timeout_s']}s",
        "",
        "## 汇总",
        "",
        "| 指标 | 值 |",
        "|---|---|",
        f"| 任务数 / 通过 | {summary['tasks']} / {summary['passed']}（{pr}）|",
        f"| 总墙钟 | {summary['wall_s_total']}s |",
        f"| 工具调用总数 | {summary['tool_calls_total']} |",
        f"| 输入 token（差分合计）| {fmt(summary['prompt_total'])} |",
        f"| 输出 token | {fmt(summary['completion_total'])} |",
        f"| 合计 token | {fmt(summary['total_tokens'])} |",
        f"| 缓存命中输入 | {fmt(summary['cache_read_total'])} |",
        f"| 缓存未命中输入 | {fmt(summary['cache_miss_total'])} |",
        f"| **缓存命中率** | **{rate}**（{summary['tasks_with_cache_data']} 个任务有缓存数据）|",
        f"| 超时任务 | {summary['timed_out']} |",
        "",
        "## 逐任务",
        "",
        "| 任务 | 维度 | 结果 | 墙钟 | 工具 | token | 命中率 | 说明 |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for r in records:
        u = r["usage_task"]
        lines.append(
            f"| {r['id']} | {r['dimension']} | {'✅' if r['ok'] else '❌'} | {r['wall_s']}s | "
            f"{r['tool_calls']} | {u.get('total', '—')} | {fmt_rate(u.get('cache_hit_rate'))} | {r['why']} |"
        )
    if not summary["usage_available"]:
        lines += [
            "",
            "> ⚠️ **账目不可用**：所有任务的 /kernel 差分都为空。",
            "> 说明目标实例的内核还没带上 usage 段（/kernel 暴露用量是 2026-09-30 的改动），",
            "> 或者适配器没有把上游 usage 透传上来。此时 token 列全为「—」，",
            "> 请先部署新内核再重跑 —— 不要把这些空白当成「消耗为 0」。",
        ]
    lines += ["", "## 逐任务明细", ""]
    for r in records:
        lines += [
            f"### {r['id']}（{r['dimension']}）",
            "",
            f"- 结果：{'通过' if r['ok'] else '失败'} — {r['why']}",
            f"- 墙钟：{r['wall_s']}s，工具调用 {r['tool_calls']} 次（{', '.join(r['tools']) or '无'}）",
            f"- 任务差分用量：`{json.dumps(r['usage_task'], ensure_ascii=False)}`",
            f"- 单次回包用量：`{json.dumps(r['usage_single'], ensure_ascii=False)}`",
            "",
            "回复节选：",
            "",
            "```",
            (r["reply"] or "").strip()[:800],
            "```",
            "",
        ]
    return "\n".join(lines)


def self_test() -> int:
    """自检：不连实例，验证本台子的纯逻辑。

    为何要有：usage_delta 与 check 一旦错了，标定结果会**系统地**错，
    而它们跟实例无关、完全可以离线验证。跑一次就少一类假数据。
    """
    fails: list[str] = []

    def eq(name: str, got, want) -> None:
        if got != want:
            fails.append(f"{name}: got={got!r} want={want!r}")

    # ---- usage_delta ----
    before = {"calls": 1, "prompt": 100, "completion": 10, "total": 110,
              "cache_read": 50, "cache_miss": 50, "reasoning": 0}
    after = {"calls": 4, "prompt": 700, "completion": 70, "total": 770,
             "cache_read": 500, "cache_miss": 200, "reasoning": 0}
    d = usage_delta(before, after)
    eq("delta.calls", d.get("calls"), 3)
    eq("delta.prompt", d.get("prompt"), 600)
    eq("delta.total", d.get("total"), 660)
    # ❗命中率按**差分**算，不是拿 after 的绝对值：
    #   Δread = 500-50 = 450，Δmiss = 200-50 = 150 ⇒ 450/600 = 0.75。
    # （我第一版判据写成 500/700，被自检拓住了。）
    eq("delta.hit_rate", round(d.get("cache_hit_rate", 0), 4), 0.75)
    # 任一侧缺数 ⇒ 空（宁可不报不编）
    eq("delta.empty_before", usage_delta({}, after), {})
    eq("delta.empty_after", usage_delta(before, {}), {})
    # 全零差分不该报命中率（分母为 0）
    zero = usage_delta({"cache_read": 0, "cache_miss": 0}, {"cache_read": 0, "cache_miss": 0})
    eq("delta.no_denominator", "cache_hit_rate" in zero, False)

    # ---- check ----
    eq("check.no_signature", check({}, {"content": "x"}, [], 1.0)[0], False)
    eq("check.contains_hit", check({"check": {"kind": "output_contains", "value": "ok"}},
                                   {"content": "all ok"}, [], 1.0)[0], True)
    eq("check.contains_miss", check({"check": {"kind": "output_contains", "value": "nope"}},
                                    {"content": "all ok"}, [], 1.0)[0], False)
    eq("check.regex", check({"check": {"kind": "output_regex", "value": "tool\\.go:\\d+"}},
                            {"content": "internal/sdk/tool.go:72"}, [], 1.0)[0], True)
    eq("check.tool_used_hit", check({"check": {"kind": "tool_used", "value": "cmd_run"}},
                                    {"content": ""},
                                    [{"type": "tool_call", "tool": "cmd_run"}], 1.0)[0], True)
    eq("check.tool_used_miss", check({"check": {"kind": "tool_used", "value": "cmd_run"}},
                                     {"content": ""}, [], 1.0)[0], False)
    # completed：有 error 就不算完成
    eq("check.completed_ok", check({"check": {"kind": "completed"}}, {"content": ""}, [], 1.0)[0], True)
    eq("check.completed_err", check({"check": {"kind": "completed"}},
                                    {"error": "boom"}, [], 1.0)[0], False)
    eq("check.unknown_kind", check({"check": {"kind": "wat"}}, {"content": ""}, [], 1.0)[0], False)

    # ---- 命中率格式化：None 必须是「—」而不是 0% ----
    eq("fmt_rate.none", fmt_rate(None), "—")
    eq("fmt_rate.zero", fmt_rate(0.0), "0.0%")

    # ---- summarize：没数据时不编命中率 ----
    recs = [{"ok": True, "wall_s": 1.0, "tool_calls": 0, "usage_task": {}}]
    s = summarize(recs)
    eq("summary.hit_rate_none", s["cache_hit_rate"], None)
    eq("summary.usage_unavailable", s["usage_available"], False)

    if fails:
        print("自检失败:", file=sys.stderr)
        for f in fails:
            print("  " + f, file=sys.stderr)
        return 1
    print("自检通过（usage_delta / check / fmt_rate / summarize）")
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="HomeAgent 能力标定台（经 cli.sock）")
    ap.add_argument("--data", help="实例 data 目录（用它的 cli.sock）")
    ap.add_argument("--socket", help="直接指定 cli.sock 路径（覆盖 --data）")
    ap.add_argument("--api-key", default=os.environ.get("HOMEAGENT_CLI_KEY", ""),
                    help="cli 认证密钥（默认读环境变量 HOMEAGENT_CLI_KEY）")
    # --tasks/--out 不设为 required：--self-test 与 --dry-run 都不需要它们。
    # 缺参由下面显式校验（报错更清楚，也让自检能在无任务集时跑）。
    ap.add_argument("--tasks", help="任务集 JSON")
    ap.add_argument("--out", help="输出目录")
    ap.add_argument("--timeout", type=float, default=600.0, help="单任务默认超时（秒）")
    ap.add_argument("--only", help="只跑逗号分隔的这些任务 id")
    ap.add_argument("--dry-run", action="store_true", help="只打印将跑什么，不连实例")
    ap.add_argument("--self-test", action="store_true", help="离线自检本台子的纯逻辑（不连实例）")
    args = ap.parse_args(argv)

    if args.self_test:
        return self_test()

    if not args.tasks or not args.out:
        print("--tasks 与 --out 为必填（除非用 --self-test）", file=sys.stderr)
        return 2

    sock = args.socket or (str(Path(args.data) / "cli.sock") if args.data
                           else str(Path.home() / ".homeagent" / "cli.sock"))
    try:
        tasks = json.loads(Path(args.tasks).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        # 给出可行动的报错，而不是一屏 traceback。
        print(f"读任务集失败 {args.tasks}: {exc}", file=sys.stderr)
        return 2
    if isinstance(tasks, dict):
        tasks = tasks.get("tasks") or []
    if args.only:
        keep = {s.strip() for s in args.only.split(",") if s.strip()}
        tasks = [t for t in tasks if t.get("id") in keep]

    if args.dry_run:
        print(f"socket: {sock}")
        for t in tasks:
            print(f"  [{t.get('dimension','?'):<12}] {t.get('id'):<24} "
                  f"timeout={t.get('timeout_s') or args.timeout}s  check={t.get('check',{}).get('kind')}")
        print(f"共 {len(tasks)} 个任务（dry-run，未连接实例）")
        return 0

    if not tasks:
        print("任务集为空", file=sys.stderr)
        return 2
    if not Path(sock).exists():
        print(f"socket 不存在: {sock}（实例没起来？或 --data 指错了）", file=sys.stderr)
        return 2

    started_at = datetime.now(timezone.utc).astimezone().isoformat(timespec="seconds")
    print(f"连接 {sock} …")
    session = CliSession(sock, args.api_key, args.timeout)
    print("已认证，开始跑任务\n")

    records: list[dict] = []
    try:
        for i, task in enumerate(tasks, 1):
            tid = task.get("id")
            print(f"[{i}/{len(tasks)}] {tid} ({task.get('dimension','?')}) …", flush=True)
            rec = run_task(session, task, args.timeout)
            records.append(rec)
            mark = "PASS" if rec["ok"] else "FAIL"
            u = rec["usage_task"]
            tok = u.get("total", "—")
            print(f"        {mark} {rec['wall_s']}s tools={rec['tool_calls']} tokens={tok} — {rec['why']}",
                  flush=True)
    except BenchError as exc:
        print(f"\n驱动层失败: {exc}", file=sys.stderr)
        return 3
    finally:
        session.close()

    summary = summarize(records)
    meta = {
        "started_at": started_at,
        "socket": sock,
        "tasks_file": args.tasks,
        "timeout_s": args.timeout,
    }
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    (out / "results.json").write_text(
        json.dumps({"meta": meta, "summary": summary, "records": records},
                   ensure_ascii=False, indent=2), encoding="utf-8")
    (out / "report.md").write_text(render_markdown(meta, summary, records), encoding="utf-8")

    rate = fmt_rate(summary["cache_hit_rate"])
    print(f"\n{'=' * 60}")
    print(f"通过 {summary['passed']}/{summary['tasks']}  墙钟 {summary['wall_s_total']}s  "
          f"token {summary['total_tokens']}  缓存命中率 {rate}")
    if not summary["usage_available"]:
        print("⚠️  账目不可用（/kernel 差分全空）—— 实例内核可能还是旧版")
    print(f"报告: {out / 'report.md'}")
    print(f"原始: {out / 'results.json'}")
    return 0 if summary["passed"] == summary["tasks"] else 1


if __name__ == "__main__":
    sys.exit(main())
