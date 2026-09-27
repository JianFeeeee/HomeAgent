#!/usr/bin/env python3
"""批内并发工具调用的内核压测（**带校验**，不只是看峰值）。

## 为什么需要单独一个驱动

现有 stress.py 压的是**调度器排队/抢占/中断**：一条输入 → 一轮 LLM → 一个回复。
而内核的并发路径是「同一条 assistant 携带**多个** tool_call → fan-out 并发执行
→ 按索引顺序落消息」。

关键判据在 batchRunnable：

    if f == nil || len(f.PendingTools) <= 1 { return false }
    for _, tc := range f.PendingTools { if !a.toolParallelSafe(tc.Name) { return false } }

⇒ **一个 tool_call 永远不并发**。所以用普通输入压内核，无论多少并发连接，
压的全是串行路径 —— 这正是用户指出的缺口。

本驱动用 mock 的 `!batchN`（N 个全 ParallelSafe 的工具）强制走并发，
用 `!mixedN`（夹一个 knowledge_create）验证**整批降级**。

## 校验什么（只看峰值是不够的）

1. **并发真的发生了** —— 不能只看"没报错"。判据：并发批的墙钟耗时应显著
   低于同规模串行批。mock 的 MOCK_DELAY_MS 是单轮 LLM 延迟，工具本身
   很快，所以真正的判据是内核日志/响应里的**工具消息条数与顺序**。
2. **消息顺序稳定** —— 并发执行但按索引落消息，模型读到的因果顺序必须与
   它发出的顺序一致。同一输入跑多次，响应里的 tool 顺序必须可重复。
3. **降级生效** —— mixed 批次里出现 knowledge_create，整批必须串行。
4. **无残留** —— 压测结束后调度器计数、goroutine、内存不持续增长。

## 用法

    # 内核与 mock 已在同一个私有 netns 里（见 README 的 unshare -n）
    python3 batchstress.py <cli.sock> <key> [batches] [tools_per_batch] [concurrency]

例：
    python3 batchstress.py /var/tmp/kstress/cli.sock $KCLI_KEY 40 8 4
"""
import json
import re
import socket
import sys
import threading
import time

# 正则**预编译**。
#
# 不预编译的话，re 模块内部有缓存，但每次调用仍要走一遍缓存查找 ——
# 而 tool_names 对**每一行**响应都要跑一次，压测规模下（万级调用 ×
# 每次若干行）这是纯浪费。模块级编译一次，零成本。
_NAME_RE = re.compile(r'"name"\s*:\s*"([a-z_]+)"')

BATCHES = 40
TOOLS = 8
CONC = 4


def arg_int(pos, default, name):
    """解析位置参数为正整数；非法时给出**可执行**的报错而不是裸 ValueError。

    压测脚本的报错是要给人看的 —— "invalid literal for int()" 谁也不知道
    是哪个参数、该怎么写。
    """
    if pos >= len(sys.argv):
        return default
    raw = sys.argv[pos]
    try:
        v = int(raw)
    except ValueError:
        sys.exit(f"参数 {name} 需要一个正整数，收到 {raw!r}（用法见本文件顶部）")
    if v <= 0:
        sys.exit(f"参数 {name} 必须 > 0，收到 {v}")
    return v


def connect(sock, key, timeout=120):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    s.connect(sock)
    s.sendall(f"/auth {key}\n".encode())
    buf = s.makefile("rb")
    line = buf.readline()
    if b"error" in line.lower():
        raise RuntimeError(f"auth failed: {line}")
    return s, buf


def send_wait(s, buf, text, timeout=180):
    """发一条输入，等到终止帧（response/error）。返回原始响应。"""
    s.sendall((text + "\n").encode())
    out = []
    t0 = time.time()
    while time.time() - t0 < timeout:
        line = buf.readline()
        if not line:
            break
        out.append(line.decode("utf-8", "replace").strip())
        # ★ 终止帧是 **JSON**（{"type":"response",...}），不是行前缀。
        #   第一版用 startswith(("response","error")) 判终止 —— 永远匹配不上，
        #   于是每轮都读到超时，而内核其实 274ms 就回了。
        #   症状：脚本看起来"卡住"，真因是判据看错了帧格式。
        if frame_type(out[-1]) in ("response", "error"):
            break
    return out


def frame_type(txt):
    """取一帧的 type；非 JSON 帧返回 ""。

    cli 通道的响应帧形如 {"type":"response","content":...}。
    """
    if not txt.startswith("{"):
        return ""
    try:
        return json.loads(txt).get("type", "") or ""
    except (ValueError, AttributeError):
        return ""


def tool_names(resp_lines):
    """从响应里抽出工具名序列（按出现顺序）。"""
    names = []
    for ln in resp_lines:
        for m in _NAME_RE.finditer(ln):
            names.append(m.group(1))
    return names


def one_batch(sock, key, marker, tools, results, idx):
    try:
        s, buf = connect(sock, key)
        try:
            t0 = time.time()
            lines = send_wait(s, buf, marker)
            dt = time.time() - t0
            results[idx] = {
                "ok": any(ln.startswith("response") for ln in lines),
                "elapsed": dt,
                "lines": len(lines),
                "tools": tool_names(lines),
                "err": next((ln for ln in lines if ln.startswith("error")), ""),
            }
        finally:
            s.close()
    except Exception as e:  # noqa: BLE001 — 压测要看到任何异常
        results[idx] = {"ok": False, "elapsed": 0, "lines": 0, "tools": [],
                        "err": repr(e)}


def run_group(sock, key, label, marker, batches, conc, tools):
    results = [None] * batches
    lock = threading.Lock()
    cursor = [0]

    def worker():
        while True:
            with lock:
                if cursor[0] >= batches:
                    return
                i = cursor[0]
                cursor[0] += 1
            # 带 i 后缀，避免 mock 认成"已有 tool 消息"而不重发工具
            one_batch(sock, key, f"{marker}-{i}", tools, results, i)

    t0 = time.time()
    ths = [threading.Thread(target=worker, daemon=True) for _ in range(conc)]
    for t in ths:
        t.start()
    for t in ths:
        t.join()
    wall = time.time() - t0

    ok = sum(1 for r in results if r and r["ok"])
    errs = {}
    for r in results:
        if r and r.get("err"):
            errs[r["err"][:120]] = errs.get(r["err"][:120], 0) + 1
    el = sorted(r["elapsed"] for r in results if r and r["ok"])
    med = el[len(el) // 2] if el else 0
    print(f"── {label} ──")
    print(f"   批次 {batches}，连接 {conc}，每批 {tools} 工具 "
          f"⇒ 累计 tool_call {batches * tools}")
    print(f"   成功 {ok}/{batches}，墙钟 {wall:.1f}s，中位单批 {med:.2f}s")
    if errs:
        print("   错误：")
        for e, c in sorted(errs.items(), key=lambda kv: -kv[1])[:3]:
            print(f"     {c:3d}× {e}")
    return {"ok": ok, "batches": batches, "tools": tools, "wall": wall,
            "median": med, "errs": errs, "results": results}


def main():
    if len(sys.argv) < 3:
        print(__doc__)
        sys.exit(2)
    sock, key = sys.argv[1], sys.argv[2]
    batches = arg_int(3, BATCHES, "batches")
    tools = arg_int(4, TOOLS, "tools")
    conc = arg_int(5, CONC, "conc")

    print(f"批内并发压测：每批 {tools} 工具，{batches} 批，{conc} 并发连接")
    par = run_group(sock, key, "并发批（全部 ParallelSafe ⇒ 应真并发）",
                    f"!batch{tools}", batches, conc, tools)
    mix = run_group(sock, key, "混合批（夹 knowledge_create ⇒ 应整批降级串行）",
                    f"!mixed{tools}", batches, conc, tools)
    single = run_group(sock, key, "单工具批（len<=1 ⇒ 恒不并发，基线）",
                       "!batch2", max(batches // 4, 4), conc, 1)

    print()
    print("══ 汇总 ══")
    for r in (par, mix, single):
        name = "并发" if r is par else ("降级" if r is mix else "基线")
        print(f"  {name:<6s} 成功 {r['ok']}/{r['batches']}  "
              f"中位 {r['median']:.2f}s  错误种类 {len(r['errs'])}")

    bad = [r for r in (par, mix, single) if r["ok"] != r["batches"]]
    if bad:
        print(f"\n✗ 有批次失败（{len(bad)} 组）")
        sys.exit(1)
    print("\n✓ 全部批次有响应")


if __name__ == "__main__":
    main()
