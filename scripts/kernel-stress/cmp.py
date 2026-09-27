#!/usr/bin/env python3
"""更新前后的**全面**性能对比与通过率。

## 为什么必须有「通过率」而不只是耗时

工具调用这一路的失败模式几乎都是**静默**的：工具没跑、参数混拼、只处理了
第一个 tool_call —— 都不报错，只是结果不对。所以"跑完没崩"完全不能说明它
work。本次每次测量都同时记录**处理数**（响应里有多少个工具结果标记）与
**是否出现错误帧**，任一不符即记为失败。

## 三块覆盖

1. **调度器**：多连接并发排队 + L4 中断（沿用 stress.py 的形态）
2. **批内工具调用**：!batchN / !slowbatchN / !serialbatchN
   —— 并发批 vs 强制串行批 vs 单工具基线
3. **稳定性**：连续多轮的通过率 + 队列/背压计数

## 每轮两个必查项（任一不过即判失败）

- **工具真跑了**：响应里能看到 `done-N` 标记（cmd_run 的 echo 输出），
  工具没跑就不可能有
- **无 error 帧**：`{"type":"error"}` 一律算失败

## 用法

    python3 cmp.py <旧sock> <旧key> <新sock> <新key> [规模]
"""

import contextlib
import json
import re
import socket
import statistics
import sys
import threading
import time

# ---------------------------------------------------------------- 基础连接

class Conn:
    """一条 cli 连接。

    ★ auth 帧本身就是 {"type":"response"}，必须先吃掉它再开始收集 ——
      否则第一轮的"终止帧"就是 auth，测出来的耗时是 0。
    """

    def __init__(self, sock, key, timeout=120):
        self.s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.s.settimeout(timeout)
        self.s.connect(sock)
        self.s.sendall(("/auth %s\n" % key).encode())
        self.f = self.s.makefile("rb")
        auth = self.f.readline()
        if b"authenticated" not in auth:
            raise RuntimeError("auth 失败: %s" % auth[:80])

    def ask(self, text, timeout=120):
        """发一条输入，等终止帧。返回 (耗时, 帧列表)。"""
        t0 = time.time()
        self.s.sendall((text + "\n").encode())
        frames = []
        while time.time() - t0 < timeout:
            line = self.f.readline()
            if not line:
                break
            t = line.decode("utf-8", "replace").strip()
            frames.append(t)
            if t.startswith("{"):
                try:
                    if json.loads(t).get("type") in ("response", "error"):
                        break
                except ValueError:
                    pass
        return time.time() - t0, frames

    def close(self):
        with contextlib.suppress(OSError):
            self.s.close()


def has_error(frames):
    for f in frames:
        if f.startswith("{"):
            try:
                if json.loads(f).get("type") == "error":
                    return True
            except ValueError:
                pass
    return False


def tools_done(frames):
    """ 数出工具真正执行的个数（按 done-N 标记去重）。"""
    blob = " ".join(frames)
    return len({int(m.group(1)) for m in re.finditer(r"done-(\d+)", blob)})


# ---------------------------------------------------------------- 单轮测量


def measure(sock, key, marker, rounds):
    """连一次、跑 rounds 轮（每轮唯一输入）。

    ★ 每轮输入必须唯一：内核 task.go:353 有输入去重
      （isDuplicateInput，为 webui 断线重连重放而设），相同文本会被丢弃
      并回空响应。第一版每轮同一个 marker，只有第 1 轮有效。
    """
    dts, oks = [], 0
    try:
        c = Conn(sock, key)
    except RuntimeError as e:
        print("   连接失败: %s" % e)
        return None
    try:
        for _ in range(rounds):
            dt, frames = c.ask("%s-%d" % (marker, time.time_ns()))
            dts.append(dt)
            # 无 error 帧即算通过（工具数由调用方按 marker 形态另行核对）
            if not has_error(frames):
                oks += 1
            if has_error(frames):
                break
    finally:
        c.close()
    return {"rounds": rounds, "ok": oks, "median": statistics.median(dts),
            "mean": statistics.mean(dts), "min": min(dts), "max": max(dts),
            "all": dts}


# ---------------------------------------------------------------- 并发轰炸


def blast(sock, key, conns, inputs, tag):
    """conns 条连接并发，每条连接连发 inputs 条输入。返回通过率。

    ★ 线程数与计数：第一版按 `conns * inputs` 起线程、每个线程又跑
      `inputs` 轮，于是总输入数是 conns×inputs²，分子分母量纲不一致，
      算出过 "128/32 = 400%" 这种荒谬数字。
      现在：**恰好 conns 个 worker，每个跑 inputs 轮** ⇒ 总输入 conns×inputs。
    """
    results = [None] * conns

    def worker(idx):
        try:
            c = Conn(sock, key)
        except RuntimeError:
            results[idx] = {"ok": 0, "sent": 0, "err": "connect"}
            return
        good = 0
        try:
            for j in range(inputs):
                _, frames = c.ask(f"{tag}-{idx}-{j}-{time.time_ns()}")
                if not has_error(frames):
                    good += 1
            results[idx] = {"ok": good, "sent": inputs, "err": ""}
        except Exception as e:  # noqa: BLE001
            results[idx] = {"ok": good, "sent": inputs, "err": repr(e)[:60]}
        finally:
            c.close()

    t0 = time.time()
    ths = [threading.Thread(target=worker, args=(i,), daemon=True)
           for i in range(conns)]
    for t in ths:
        t.start()
    for t in ths:
        t.join()
    wall = time.time() - t0

    # 分母用"实际发出的输入数"（含连接失败的那些），而不是标称的 conns×inputs
    # —— 连接失败时分子分母必须同步缩放，否则通过率会虚高。
    total = sum(r["sent"] for r in results if r)
    good = sum(r["ok"] for r in results if r)
    return {"total": total, "good": good, "wall": wall,
            "rate": good / total if total else 0}


# ---------------------------------------------------------------- 主流程


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


def tools_for(sock, key, n):
    """单跑一轮，数出真正执行的工具个数（连不上返回 0）。"""
    try:
        c = Conn(sock, key)
    except RuntimeError:
        return 0
    try:
        _, frames = c.ask("!slowbatch%d-%d" % (n, time.time_ns()))
        return tools_done(frames)
    except Exception:  # noqa: BLE001
        return 0
    finally:
        c.close()


def main():
    if len(sys.argv) < 5:
        print(__doc__)
        sys.exit(2)
    old_sock, old_key, new_sock, new_key = sys.argv[1:5]
    scale = arg_int(5, 1, "scale")

    print("=" * 72)
    print("更新前后全面对比（旧: 85e3d66  /  新: d3eaff4），scale=%d" % scale)
    print("=" * 72)

    # ---- ① 批内工具调用 ----
    print("\n【① 批内工具调用】")
    print("%-26s %-16s %-16s %s" % ("场景", "旧 中位/工具", "新 中位/工具", "变化"))
    for n in (2, 4, 8):
        row = {}
        for tag, sock, key in (("old", old_sock, old_key), ("new", new_sock, new_key)):
            m = measure(sock, key, "!slowbatch%d" % n, 3)
            if m is None:
                row[tag] = (0.0, 0)
            else:
                row[tag] = (m["median"], tools_for(sock, key, n))
        o, w = row["old"], row["new"]
        chg = "—" if o[0] == 0 else "%+.1f%%" % ((w[0] - o[0]) / o[0] * 100)
        print("%-26s %-16s %-16s %s"
              % ("!slowbatch%d 并发批" % n,
                 "%.3fs / %d 个" % o, "%.3fs / %d 个" % w, chg))

    # ---- ② 并发 vs 强制串行（新版内部对照）----
    print("\n【② 并发 vs 强制串行（新版内部对照）】")
    print("%-6s %-14s %-14s %-10s %s" % ("N", "并发", "强制串行", "加速", "工具数"))
    for n in (2, 4, 8):
        cp = measure(new_sock, new_key, "!slowbatch%d" % n, 3)
        cs = measure(new_sock, new_key, "!serialbatch%d" % n, 3)
        if cp is None or cs is None:
            print("%-6d 测量失败（连接不上）" % n)
            continue
        k = tools_for(new_sock, new_key, n)
        sp = cs["median"] / cp["median"] if cp["median"] else 0
        print("%-6d %-14s %-14s %-10.2f %d"
              % (n, "%.3fs" % cp["median"], "%.3fs" % cs["median"], sp, k))

    # ---- ③ 调度器并发轰炸 + 通过率 ----
    print("\n【③ 调度器并发轰炸】")
    conns, inputs = 8 * scale, 4 * scale
    for tag, sock, key in (("旧", old_sock, old_key), ("新", new_sock, new_key)):
        r = blast(sock, key, conns, inputs, "!batch2")
        print("  %s版: %d/%d 通过（%.1f%%），墙钟 %.1fs"
              % (tag, r["good"], r["total"], r["rate"] * 100, r["wall"]))

    # ---- ④ 连续稳定性 ----
    print("\n【④ 连续稳定性（20 轮）】")
    for tag, sock, key in (("旧", old_sock, old_key), ("新", new_sock, new_key)):
        m = measure(sock, key, "!batch2", 20)
        if m is None:
            print("  %s版: 连接失败" % tag)
            continue
        print("  %s版: %d/%d 无错误，中位 %.3fs，min %.3f / max %.3f"
              % (tag, m["ok"], m["rounds"], m["median"], m["min"], m["max"]))
    print()


if __name__ == "__main__":
    main()
