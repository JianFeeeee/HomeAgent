#!/usr/bin/env python3
"""串行 vs 并发的**定量**对比：多工具调用与平均轮次延迟。

## 测什么

同一批 N 个慢工具（cmd_run sleep），在两套内核上各跑一遍，比：

1. **平均轮次延迟** —— 从发输入到收到最终回复的墙钟时间。
   并发的理论收益 = 单工具耗时 × (N-1)：串行要 N×t，并发只要约 1×t。
2. **加速比** = 基线延迟 / 新版延迟。
3. **工具消息顺序** —— 内核声明「并发执行但按**声明序**落消息」，
   而 mock 故意让完成顺序与声明序**相反**（索引越大 sleep 越短）。
   所以：若响应里的工具顺序是 0,1,2,…，说明按声明序落对了；
   若是完成序，就暴露了「按完成顺序合并」这个 bug。

## 为什么工具要慢

并发的收益 = 单工具耗时 × (N-1)。工具若只跑几微秒，串行与并发的差异会
被 LLM 延迟（DELAY_MS，默认 200ms）整个淹没 —— 测出来全是噪声。
所以用 `sleep` 型 cmd_run：延迟可精确预期、无外部副作用、不受机器负载影响。

## 怎么保证对比公平

- 两套内核实测用**同一个** mock、同一个数据目录模板、同一批 N；
- 交替执行（A/B/A/B）而不是先跑完 A 再跑 B —— 抵消机器负载漂移；
- 每组取中位数，不用平均值（长尾会污染均值）。

## 用法

    python3 abtest.py <sockA> <keyA> <labelA> <sockB> <keyB> <labelB> [tools] [rounds]
"""
import json
import re
import socket
import statistics
import sys
import time

TOOLS = 8
ROUNDS = 12
SLEEP_MS = 200


def arg_int(pos, default, name):
    """解析位置参数为正整数；非法时给出可执行报错而不是裸 ValueError。"""
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


def connect(sock, key, timeout=180):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    s.connect(sock)
    s.sendall(f"/auth {key}\n".encode())
    buf = s.makefile("rb")
    line = buf.readline()
    if b"error" in line.lower():
        raise RuntimeError(f"auth failed: {line}")
    return s, buf


_NAME_RE = re.compile(r'"name"\s*:\s*"([a-z_]+)"')
_CMD_RE = re.compile(r"done-(\d+)")


def one_round(sock, key, marker, tools, out, idx):
    """跑一轮：发 !slowbatchN，等最终回复。返回 (延迟, 工具顺序, 错误)。"""
    t0 = None
    try:
        s, buf = connect(sock, key)
        try:
            t0 = time.time()
            # ★ 必须每轮唯一：内核 task.go:353 有输入去重
            #   （`isDuplicateInput`，为 webui 断线重连重放而设），
            #   相同文本会被直接丢弃并回空响应。
            #   我第一版每轮发同一个 marker ⇒ 只有第 1 轮有效，
            #   后面全是 0 秒 0 工具，加速比算出来是噪声。
            s.sendall(f"{marker}-{idx}-{time.time_ns()}\n".encode())
            lines = []
            while True:
                line = buf.readline()
                if not line:
                    break
                txt = line.decode("utf-8", "replace").strip()
                lines.append(txt)
                if txt.startswith(("response", "error")):
                    break
            dt = time.time() - t0
            err = next((x for x in lines if x.startswith("error")), "")
            # 从全部行里抽工具顺序与"工具真跑"的证据。
            #
            # ★ 关键：只看耗时是不够的。前面几轮出现过"内核 274ms 就回复、
            #   工具一个没跑"的情况 —— 那种情况下并发与串行都是 0.2s，
            #   加速比毫无意义。所以必须确认 done-N 标记真的出现在响应里：
            #   那是 cmd_run 执行完 echo 的输出，工具没跑就不可能有。
            names = []
            for x in lines:
                names.extend(_NAME_RE.findall(x))
            order = [int(m.group(1)) for x in lines for m in _CMD_RE.finditer(x)]
            if not order:
                out[idx] = {"dt": dt, "order": [], "names": names, "ran": 0,
                            "err": "响应里没有 done-N 标记：工具可能没真执行",
                            "ok": False}
                return
            out[idx] = {"dt": dt, "order": order, "names": names,
                        "ran": len(order), "err": err, "ok": not err}
        finally:
            s.close()
    except Exception as e:  # noqa: BLE001
        out[idx] = {"dt": 0.0, "order": [], "names": [], "ran": 0,
                    "err": repr(e), "ok": False}


def measure(sock, key, label, tools, rounds, warmup=2):
    out = [None] * rounds
    # 预热：首次调用会建连接、可能触发插件懒加载，不计入
    for _ in range(warmup):
        one_round(sock, key, "!warmup", tools, [None], 0)
    for i in range(rounds):
        one_round(sock, key, f"!slowbatch{tools}", out, i)
    dts = [r["dt"] for r in out if r and r["ok"]]
    errs = [r["err"] for r in out if r and r["err"]]
    orders = [r["order"] for r in out if r and r["ok"] and r["order"]]
    return {
        "label": label,
        "ok": len(dts),
        "rounds": rounds,
        "median": statistics.median(dts) if dts else 0.0,
        "mean": statistics.mean(dts) if dts else 0.0,
        "min": min(dts) if dts else 0.0,
        "max": max(dts) if dts else 0.0,
        "errs": errs[:3],
        "orders": orders,
        "ran": [r.get("ran", 0) for r in out if r],
        "tools": tools,
    }


def main():
    if len(sys.argv) < 6:
        print(__doc__)
        sys.exit(2)
    sockA, keyA, labelA, sockB, keyB, labelB = sys.argv[1:7]
    tools = arg_int(7, TOOLS, "tools")
    rounds = arg_int(8, ROUNDS, "rounds")

    print(f"A/B 定量对比：每批 {tools} 个 sleep 工具（单工具约 {SLEEP_MS}ms），"
          f"每组 {rounds} 轮")
    print(f"  A = {labelA}")
    print(f"  B = {labelB}")
    print()

    # 交替执行 A/B/A/B 抵消机器负载漂移
    a_runs, b_runs = [], []
    for _ in range(rounds):
        a_runs.append(measure(sockA, keyA, labelA, tools, 1, warmup=0))
        b_runs.append(measure(sockB, keyB, labelB, tools, 1, warmup=0))
    A = {
        "label": labelA, "tools": tools, "rounds": rounds,
        "dts": [x["median"] for x in a_runs if x["median"] > 0],
        "orders": [o for x in a_runs for o in x["orders"]],
        "ran": [x["ran"] for x in a_runs],
        "errs": [e for x in a_runs for e in x["errs"]],
    }
    B = {
        "label": labelB, "tools": tools, "rounds": rounds,
        "dts": [x["median"] for x in b_runs if x["median"] > 0],
        "orders": [o for x in b_runs for o in x["orders"]],
        "errs": [e for x in b_runs for e in x["errs"]],
    }

    def show(x):
        if not x["dts"]:
            print(f"  {x['label']:22s} 无有效样本（错误：{x['errs'][:1]}）")
            return
        d = sorted(x["dts"])
        med = statistics.median(d)
        print(f"  {x['label']:22s} 中位 {med*1000:7.1f}ms  "
              f"min {d[0]*1000:7.1f}ms  max {d[-1]*1000:7.1f}ms  "
              f"样本 {len(d)}/{x['rounds']}")
        if x["errs"]:
            print(f"  {'':22s} 错误 {x['errs'][0][:90]}")

    print("── 平均轮次延迟 ──")
    show(A)
    show(B)
    if A["dts"] and B["dts"]:
        ma, mb = statistics.median(A["dts"]), statistics.median(B["dts"])
        print(f"\n  加速比 A/B = {ma/mb:.2f}x"
              f"（理论上限 ≈{tools}x，单工具 {SLEEP_MS}ms × {tools-1}）")
        # 理论：串行 ≈ (t)*N，并发 ≈ t + 少量开销
        t = SLEEP_MS / 1000.0
        print(f"  参照：全串行 ≈{t*tools*1000:.0f}ms，完全并发 ≈{t*1000:.0f}ms")

    print("\n── 工具消息顺序（mock 让完成序与声明序相反）──")
    for x in (A, B):
        if not x["orders"]:
            print(f"  {x['label']:22s} （未捕获到工具顺序）")
            continue
        bad = [o for o in x["orders"] if o != sorted(o)]
        tag = "✗ 错乱" if bad else "✓ 声明序"
        print(f"  {x['label']:22s} {tag}  {len(x['orders'])} 轮"
              + (f"，例：{bad[0][:8]}" if bad else ""))


if __name__ == "__main__":
    main()
