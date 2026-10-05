#!/usr/bin/env python3
"""pi 基线 runner —— 与 HomeAgent 标定台跑**同一套任务、同一个模型**。

为什么要它
==========

「全面标定能力」要有对照。pi 与本机内核走的是**同一个 llmsproxy 网关、
同一个 AUTO 模型**，所以这是一个干净的「同模型、只变 harness」对比 ——
与 HarnessTax 的做法一致。

复用 bench.py 的判据与汇总（`check` / `summarize` / `render_markdown`），
确保两侧的判定逻辑**不是两份实现** —— 否则对比结果本身就不可信
（本项目刚在缓存规则上吃过「两套实现互相掩盖同一个错」的亏）。

pi 的接口（实测，非猜）
======================

    pi -p --mode json --provider llmsproxy --model AUTO "<prompt>"

stdout 是 JSON Lines，关键事件：
  message_end        一条消息结束（assistant 的 usage 在这里）
  turn_end           一轮结束（带该轮 usage）
  agent_settled      任务真正结束（终态）

★ 两个实测得到的坑：
  1. **`pi -p` 跑完不会自己退出** —— A2A/ACP 两个扩展把服务挂着。
     必须读到 `agent_settled` 后主动 kill，否则会一直等到超时。
  2. **必须给 A2A/ACP 指定空闲端口**：本会话守护进程占着 PI_A2A_PORT=14010，
     而另一个 pi 实例占着默认 12010 ⇒ 子进程继承环境后会 EADDRINUSE 崩掉。
     所以下面显式分配端口。

计费口径
========

把每个 `turn_end` 的 usage 求和 = 该任务的真实总账（一个任务可能多轮工具回环）。
与 HomeAgent 侧「/kernel 差分」口径一致，两者可直接比。
"""
from __future__ import annotations

import argparse
import contextlib
import json
import os
import signal
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import bench

# agent_settled 是 pi 的终态事件（实测）。
TERMINAL_EVENT = "agent_settled"


def _int(v: object) -> int:
    """尽力取整数。上游字段可能是字符串/None/浮点，取不到就记 0。"""
    try:
        return int(v)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return 0


def _usage_of(obj: object) -> dict:
    """从 pi 的 usage 归一成与 HomeAgent 同口径的字段。

    ★★ 关键：两边**同名字段的含义不同**，不统一就不能比。

    pi（Anthropic 口径，已用受控实验实证 two 条恒等式）：
        totalTokens == input + cacheRead + output
      ⇒ `input` 是**未命中**的输入（不含缓存），`cacheRead` 是命中侧。

    HomeAgent（OpenAI 口径）：
        prompt_tokens **包含**缓存，cache_read 是其中命中那部分。

    所以归一：
        prompt     = input + cacheRead   （总输入，含缓存）
        cache_miss = input               （未命中侧）
        cache_read = cacheRead
    这样两侧的命中率分母都是 cache_read + cache_miss，语义一致。

    ❌ 不归一的后果（我第一版就错了）：把 input 当 prompt、miss 记 0，
      命中率算成 read/read ⇒ **恒 100%** —— 与刚在 HomeAgent 修掉的
      「分母被抽掉」是同一个结构性假绿。且因为 read 可能大于 input，
      推导 miss 会得到负数（实测 -24611），荒谬到肉眼可见。
    """
    u = obj if isinstance(obj, dict) else {}
    uncached_in = _int(u.get("input"))     # pi 的 input = 未命中侧
    cache_read = _int(u.get("cacheRead"))
    return {
        "prompt": uncached_in + cache_read,   # 总输入（含缓存）
        "completion": _int(u.get("output")),
        "total": _int(u.get("totalTokens")) or (uncached_in + cache_read + _int(u.get("output"))),
        "cache_read": cache_read,
        "cache_miss": uncached_in,            # 未命中侧
        "reasoning": _int(u.get("reasoning")),
    }


def _parse_line(line: str) -> dict | None:
    """解析一行 JSON；不是 JSON 对象就返回 None（调用方跳过）。"""
    try:
        obj = json.loads(line)
    except json.JSONDecodeError:
        return None
    return obj if isinstance(obj, dict) else None


def isolated_env(args: argparse.Namespace, port_base: int) -> dict:
    """构造一个**隔离**的环境变量集。

    为何不能直接继承 os.environ（实测踩到）：
      · 本会话守护进程占着 PI_A2A_PORT=14010、另有 pi 实例占 12010
        ⇒ 子 pi 继承后会 EADDRINUSE 直接崩；
      · 继承 PI_SESSION_ID / PI_CODING_AGENT_DIR 会让它读写**我的**会话状态。

    隔离做法：给一个干净的环境（只留 PATH/HOME）+ 独立的配置目录。
    副作用（好的一侧）：隔离配置里没有扩展 ⇒ 不会起 A2A/ACP 服务
    ⇒ `pi -p` 跑完**自己退出**，不必再靠 kill 收尾。
    端口仍显式分配，作为万一加载了扩展的兜底。
    """
    env = {
        "PATH": os.environ.get("PATH", "/usr/bin:/bin:/usr/local/bin"),
        "HOME": os.environ.get("HOME", "/root"),
        "PI_CODING_AGENT_DIR": args.iso_dir,
        "PI_CODING_AGENT_SESSION_DIR": str(Path(args.iso_dir).parent / "sessions"),
        "PI_A2A_PORT": str(port_base),
        "PI_ACP_PORT": str(port_base + 1),
        "PI_TELEMETRY": "0",
        "PI_SKIP_VERSION_CHECK": "1",
    }
    for k in ("https_proxy", "http_proxy", "no_proxy"):
        if k in os.environ:
            env[k] = os.environ[k]
    return env


def run_pi(task: dict, args: argparse.Namespace, port_base: int) -> dict:
    """跑一个任务，返回与 bench.run_task 同形状的记录。"""
    prompt = task["prompt"]
    env = isolated_env(args, port_base)

    cmd = [
        "pi", "-p", "--mode", "json", "--no-session",
        "--provider", args.provider, "--model", args.model,
    ]
    if args.pi_args:
        cmd += args.pi_args.split()
    cmd += [prompt]

    started = time.monotonic()
    proc = subprocess.Popen(
        cmd, cwd=args.cwd, env=env,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        start_new_session=True,   # 便于整组清理（pi 可能再起子进程）
    )
    # 读盘超时：若 pi 用 stdin 等输入而没收到，会挂住。
    # timeout 由外层 deadline + 显式 kill 保证，这里不设，避免长时任务被误杀。

    events: list[dict] = []
    turn_usages: list[dict] = []
    final_text = ""
    settled = False
    deadline = started + args.timeout

    try:
        for line in proc.stdout:  # type: ignore[union-attr]
            line = line.strip()
            if not line:
                continue
            ev = _parse_line(line)
            if ev is None:
                continue
            events.append(ev)
            etype = ev.get("type")

            if etype == "turn_end":
                msg = ev.get("message") or {}
                turn_usages.append(_usage_of(msg.get("usage")))
                # 该轮 assistant 的文本（最终回复以最后一轮为准）
                txt = _text_of(msg)
                if txt:
                    final_text = txt
            elif etype == "message_end":
                msg = ev.get("message") or {}
                if msg.get("role") == "assistant":
                    txt = _text_of(msg)
                    if txt:
                        final_text = txt
            elif etype == TERMINAL_EVENT:
                # 见文件头坑 1：settled 后进程不会自己退，立刻收工。
                settled = True
                break

            if time.monotonic() > deadline:
                break
    except Exception as exc:  # noqa: BLE001 - 驱动层异常一律如实记录
        events.append({"type": "_runner_error", "error": str(exc)})
    finally:
        # 无论何种路径都要收掉整组进程，否则残留的 pi 会一直占端口。
        if proc.poll() is None:
            with contextlib.suppress(ProcessLookupError, PermissionError):
                os.killpg(os.getpgid(proc.pid), signal.SIGTERM)
            # 先给优雅退出的机会，再升级到 SIGKILL（两步都要：
            # 只 TERM 可能留下挂着的 pi，只 KILL 则不给它清理现场的机会）。
            with contextlib.suppress(subprocess.TimeoutExpired):
                proc.wait(timeout=5)
            if proc.poll() is None:
                with contextlib.suppress(ProcessLookupError, PermissionError):
                    os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
                with contextlib.suppress(subprocess.TimeoutExpired):
                    proc.wait(timeout=5)
        with contextlib.suppress(Exception):
            proc.stdout.close()  # type: ignore[union-attr]
        with contextlib.suppress(Exception):
            proc.stderr.close()  # type: ignore[union-attr]

    wall_s = time.monotonic() - started
    timed_out = (not settled) and wall_s >= args.timeout - 1

    # 任务总账 = 各轮求和（与 HomeAgent 的 /kernel 差分同口径）。
    task_usage: dict = {}
    if turn_usages:
        for k in ("prompt", "completion", "total", "cache_read", "cache_miss", "reasoning"):
            task_usage[k] = sum(u.get(k, 0) for u in turn_usages)
        task_usage["calls"] = len(turn_usages)

    tool_calls = [
        e for e in events
        # pi 的事件名是 tool_execution_start（**不是** tool_call），
        # 且工具名字段是 toolName——三处都实测确认过。
        # 用错字段不会报错，只是工具名恒为空（本 runner 第一版就是这样）。
        if e.get("type") in ("tool_execution_start", "tool_call", "tool_start")
    ]

    terminal = {"type": "response" if settled else "error",
                "content": final_text}
    if timed_out:
        terminal = {"type": "error", "error": f"timeout after {args.timeout}s"}

    ok, why = bench.check(
        task,
        terminal,
        [{"type": "tool_call", "tool": (t.get("name") or t.get("tool") or "")} for t in tool_calls],
        wall_s,
    )

    return {
        "id": None,  # 由调用方填
        "dimension": None,
        "ok": ok,
        "why": why,
        "wall_s": round(wall_s, 3),
        "timed_out": timed_out,
        "error": terminal.get("error"),
        "tool_calls": len(tool_calls),
        "tools": sorted({_tool_name(t) for t in tool_calls if _tool_name(t)}),
        "usage_single": _usage_of(turn_usages[-1] if turn_usages else {}),
        "usage_task": task_usage,
        "reply": (final_text or "")[:2000],
        "events": len(events),
    }


def _tool_name(ev: dict) -> str:
    """取工具名。

    pi 用 toolName（实测：tool_execution_start 事件带 toolName/args/toolCallId），
    其他 harness 可能是 name/tool。用错字段不会报错，只是工具名恒为空 ——
    本 runner 第一版就踩到（tool_calls=30 但 tools=[]）。
    """
    for k in ("toolName", "name", "tool"):
        v = ev.get(k)
        if isinstance(v, str) and v:
            return v
    return ""


def _text_of(msg: dict) -> str:
    c = msg.get("content")
    if isinstance(c, str):
        return c
    if isinstance(c, list):
        return "".join(
            b.get("text", "") for b in c
            if isinstance(b, dict) and b.get("type") == "text"
        )
    return ""


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="pi 基线 runner（与 HomeAgent 标定台同任务同模型）")
    ap.add_argument("--tasks", required=True, help="任务集 JSON（与 bench.py 同一份）")
    ap.add_argument("--out", required=True, help="输出目录")
    ap.add_argument("--provider", default="llmsproxy", help="pi 的 provider（默认 llmsproxy）")
    ap.add_argument("--model", default="AUTO", help="pi 的模型（默认 AUTO）")
    ap.add_argument("--timeout", type=float, default=300.0, help="单任务超时（秒）")
    ap.add_argument("--cwd", default="/var/tmp/pi-iso/work", help="pi 的工作目录")
    ap.add_argument("--iso-dir", default="/var/tmp/pi-iso/agent",
                    help="pi 的独立配置目录（隔离会话/扩展；需含 models.json）")
    ap.add_argument("--pi-args", default="", help="额外传给 pi 的参数（如 --no-extensions）")
    ap.add_argument("--port-base", type=int, default=18110, help="A2A/ACP 起始端口")
    ap.add_argument("--only", help="只跑逗号分隔的任务 id")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args(argv)

    try:
        spec = json.loads(Path(args.tasks).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        print(f"读任务集失败 {args.tasks}: {exc}", file=sys.stderr)
        return 2
    tasks = spec.get("tasks") if isinstance(spec, dict) else spec
    if not isinstance(tasks, list) or not tasks:
        print(f"任务集为空或格式不对: {args.tasks}", file=sys.stderr)
        return 2
    if args.only:
        keep = {s.strip() for s in args.only.split(",") if s.strip()}
        tasks = [t for t in tasks if t.get("id") in keep]

    if args.dry_run:
        for t in tasks:
            print(f"  [{t.get('dimension','?'):<11}] {t.get('id'):<20} "
                  f"timeout={t.get('timeout_s') or args.timeout}s")
        print(f"共 {len(tasks)} 个任务（dry-run）")
        return 0

    Path(args.cwd).mkdir(parents=True, exist_ok=True)
    if not (Path(args.iso_dir) / "models.json").exists():
        print(f"隔离配置目录缺少 models.json: {args.iso_dir}", file=sys.stderr)
        return 2
    started_at = datetime.now(timezone.utc).astimezone().isoformat(timespec="seconds")
    print(f"pi 基线：provider={args.provider} model={args.model}")
    print(f"  cwd={args.cwd}")
    print(f"  iso_dir={args.iso_dir}\n")

    records: list[dict] = []
    for i, task in enumerate(tasks, 1):
        tid = task.get("id")
        print(f"[{i}/{len(tasks)}] {tid} ({task.get('dimension','?')}) …", flush=True)
        # 任务集里写了非法超时就退回默认值，不因一个字段崩掉整批。
        if task.get("timeout_s"):
            with contextlib.suppress(TypeError, ValueError):
                args.timeout = float(task["timeout_s"])
        rec = run_pi(task, args, args.port_base + 2 * i)
        rec["id"] = tid
        rec["dimension"] = task.get("dimension", "uncategorized")
        records.append(rec)
        u = rec["usage_task"]
        print(f"        {'PASS' if rec['ok'] else 'FAIL'} {rec['wall_s']}s "
              f"tools={rec['tool_calls']} tokens={u.get('total','—')} — {rec['why']}", flush=True)

    summary = bench.summarize(records)
    meta = {"started_at": started_at, "socket": f"pi:{args.provider}/{args.model}",
            "tasks_file": args.tasks, "timeout_s": args.timeout,
            "pi_args": args.pi_args or "(默认)"}
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    (out / "results.json").write_text(
        json.dumps({"meta": meta, "summary": summary, "records": records},
                   ensure_ascii=False, indent=2), encoding="utf-8")
    (out / "report.md").write_text(bench.render_markdown(meta, summary, records), encoding="utf-8")

    print(f"\n{'=' * 60}")
    print(f"通过 {summary['passed']}/{summary['tasks']}  墙钟 {summary['wall_s_total']}s  "
          f"token {summary['total_tokens']}  缓存命中率 {bench.fmt_rate(summary['cache_hit_rate'])}")
    print(f"报告: {out / 'report.md'}")
    return 0 if summary["passed"] == summary["tasks"] else 1


if __name__ == "__main__":
    sys.exit(main())
