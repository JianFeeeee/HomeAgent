#!/usr/bin/env python3
"""webui WebAPI 只读端点压测。

## 为什么只压只读端点

这是**生产实例**（192.168.2.60:8080）。压测绝不能改状态：
`/chat` 会真调 LLM、`/chat/interrupt` 会中断正在跑的任务、
`/settings/*` 与 `/plugins/*` 会改配置、`/device/*` 会控制真实设备。

⇒ 端点表**按实测逐个确认过**是 GET + 只读，不是照路由表抄的。
探测方式见下方 `--probe`。

## 判据（与 kernel-stress 那套一致）

不能只看"没报错"或墙钟时间。每一项都记：
- 成功率（HTTP 200 占比）
- 延迟分布（p50 / p95 / p99 / max）
- 失败状态码与响应体样本

**失败必须带样本**，否则「200 但返回空 JSON」这类退化看不见。

## 认证

webui 认两种：`X-API-Key` 头，或 `homeagent_session` cookie。
无认证时返回 **200 + 登录页 HTML**（含 `THEME_PLACEHOLDER`）——
**状态码是 200 但内容是登录页**，只统计状态码会得出「全部健康」的错误结论。
本脚本因此额外校验响应体，见 `looks_like_login_page()`。

用法：
    python3 webui-bench.py --key <API_KEY> [--scale 1|2|3] [--json out.json]
    python3 webui-bench.py --key <API_KEY> --probe      # 只探测端点可用性
"""

import argparse
import concurrent.futures as cf
import contextlib
import json
import statistics
import sys
import time
import urllib.error
import urllib.request

BASE = "http://127.0.0.1:8080"
# ★ 必须带 /api/v1 前缀。端点表里存的是**路径后半段**
#   （"/status"），拼 BASE + "/status" 会得到 "/status" ⇒ 全部 404。
#   首次跑 --probe 时正是这样：18 个端点全 404，而同一时刻 curl
#   /api/v1/status 是 200。⇒ 压测脚本**必须先 probe** 再压。
API_PREFIX = "/api/v1"

# 实测确认的只读端点（GET，200，不改状态）
READONLY = [
    ("/status", "小", "首页 KPI 之一"),
    ("/network", "小", "网络信息"),
    ("/terminals", "小", "终端列表"),
    ("/tracker", "小", "跟踪器"),
    ("/adapters", "小", "LLM 适配器"),
    ("/agents", "小", "agent 列表"),
    ("/config", "小", "配置"),
    ("/persona", "小", "人格"),
    ("/proxy/services", "小", "反代服务"),
    ("/plugins", "中", "插件清单"),
    ("/runtime", "中", "运行时"),
    ("/memory/context", "中", "记忆上下文"),
    ("/memory/text", "中", "文本记忆"),
    ("/memory", "中", "记忆"),
    ("/knowledge", "中", "知识库"),
    ("/kernel", "大", "★ 总览 8 个 KPI 的数据源"),
    ("/memory/graph", "大", "记忆图谱"),
    ("/knowledge/tree", "大", "★ 知识树（实测最大 425KB）"),
]


def looks_like_login_page(body: bytes) -> bool:
    """webui 无认证时返回 200 + 登录页 HTML。

    状态码骗人：只看 http_code 会把「登录页」当成健康响应。
    """
    head = body[:4096]
    return (
        b"THEME_PLACEHOLDER" in head
        or "统一门户登录".encode() in head
        or (b"<title>" in head and b"login" in head)
    )


def one_request(path: str, key: str, timeout: float) -> dict:
    url = BASE + API_PREFIX + path
    req = urllib.request.Request(url, headers={"X-API-Key": key})
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            body = r.read()
            dt = (time.perf_counter() - t0) * 1000
            return {
                "ok": r.status == 200 and not looks_like_login_page(body),
                "status": r.status,
                "bytes": len(body),
                "ms": dt,
                "login_page": looks_like_login_page(body),
            }
    except urllib.error.HTTPError as e:
        dt = (time.perf_counter() - t0) * 1000
        # ★ 读取错误体可能二次抛错（连接已断、body 为空），不能让它
        #   盖掉真正的状态码 —— 用 suppress 只吞这一处的异常。
        sample = ""
        with contextlib.suppress(Exception):
            sample = e.read()[:200].decode("utf-8", "replace")
        return {"ok": False, "status": e.code, "bytes": 0, "ms": dt, "err": sample}
    except Exception as e:
        dt = (time.perf_counter() - t0) * 1000
        return {"ok": False, "status": 0, "bytes": 0, "ms": dt, "err": repr(e)[:200]}


def pct(vals, p):
    """百分位。空列表 / 非法入参都返回 None 而不是抛错。

    调用方会把它直接拿去做 f-string 格式化（可能收到 None），
    所以**这里不能抛** —— 一个端点的空样本不该让整份报告崩掉。
    """
    if not vals:
        return None
    try:
        s = sorted(float(v) for v in vals)
        k = min(len(s) - 1, max(0, int(round((float(p) / 100) * (len(s) - 1)))))
    except (TypeError, ValueError):
        return None
    return s[k]


def probe(key: str) -> int:
    """逐个确认端点真的可 GET、返回 JSON 而非登录页。"""
    print("端点探测（GET / 只读）")
    bad = 0
    for path, size, note in READONLY:
        r = one_request(path, key, 15)
        flag = "✓" if r["ok"] else "✗"
        extra = ""
        if r.get("login_page"):
            extra = "  ← 返回登录页（认证有问题）"
            bad += 1
        elif r["status"] != 200:
            extra = f"  ← {r.get('err', '')[:80]}"
            bad += 1
        print(f"  {flag} {path:24} {r['status']:>3}  {r['bytes']:>7}B  {r['ms']:>7.1f}ms  [{size}] {note}{extra}")
    return 1 if bad else 0


def bench(key: str, scale: int, rounds: int, conc: int, timeout: float, out: str | None):
    jobs = []
    for _ in range(rounds):
        for path, _, _ in READONLY:
            jobs.append(path)
    print(f"\n压测：{len(READONLY)} 端点 × {rounds} 轮 = {len(jobs)} 请求，"
          f"并发 {conc}（scale={scale}）")
    print("  只读端点，不触碰 /chat、/settings、/plugins 写接口、/device\n")

    t0 = time.perf_counter()
    results = []
    with cf.ThreadPoolExecutor(max_workers=conc) as ex:
        futs = {ex.submit(one_request, p, key, timeout): p for p in jobs}
        for f in cf.as_completed(futs):
            results.append((futs[f], f.result()))
    wall = time.perf_counter() - t0

    by_path = {}
    for path, r in results:
        by_path.setdefault(path, []).append(r)

    print(f"{'端点':24} {'成功':>10} {'p50':>8} {'p95':>8} {'p99':>8} {'max':>8} {'平均KB':>8}")
    print("-" * 78)
    all_ms = []
    total_ok = total = 0
    failures = []
    for path, _, _ in READONLY:
        rs = by_path.get(path, [])
        if not rs:
            continue
        oks = [r for r in rs if r["ok"]]
        ms = [r["ms"] for r in oks] or [r["ms"] for r in rs]
        all_ms += ms
        total_ok += len(oks)
        total += len(rs)
        for r in rs:
            if not r["ok"]:
                failures.append((path, r))
        avg_kb = sum(r["bytes"] for r in rs) / len(rs) / 1024
        print(f"{path:24} {len(oks):>4}/{len(rs):<5} "
              f"{pct(ms,50):>8.1f} {pct(ms,95):>8.1f} {pct(ms,99):>8.1f} {max(ms):>8.1f} {avg_kb:>8.1f}")

    print("-" * 78)
    rps = total / wall if wall else 0
    print(f"总计 {total} 请求 / {wall:.2f}s  ⇒ {rps:.0f} req/s   "
          f"成功 {total_ok}/{total} = {total_ok/total*100:.2f}%")
    print(f"全局 p50={pct(all_ms,50):.1f}ms  p95={pct(all_ms,95):.1f}ms  "
          f"p99={pct(all_ms,99):.1f}ms  max={max(all_ms):.1f}ms")
    if all_ms:
        print(f"平均 {statistics.mean(all_ms):.1f}ms  "
              f"stdev {statistics.pstdev(all_ms):.1f}ms  "
              f"min {min(all_ms):.1f}ms")

    if failures:
        print(f"\n★ 失败 {len(failures)} 次（带样本，便于定位）：")
        seen = set()
        for path, r in failures[:10]:
            key2 = (path, r["status"])
            if key2 in seen:
                continue
            seen.add(key2)
            print(f"  {path:24} status={r['status']:>3} "
                  f"{r.get('err', 'login_page' if r.get('login_page') else '')[:90]}")

    if out:
        try:
            with open(out, "w", encoding="utf-8") as f:
                json.dump({
                    "wall_s": wall, "rps": rps, "total": total, "ok": total_ok,
                    "p50": pct(all_ms, 50), "p95": pct(all_ms, 95), "p99": pct(all_ms, 99),
                    "max": max(all_ms) if all_ms else None,
                    "per_path": {p: {"n": len(v),
                                     "ok": sum(1 for r in v if r["ok"]),
                                     "p50": pct([r["ms"] for r in v], 50),
                                     "p95": pct([r["ms"] for r in v], 95),
                                     "max": max((r["ms"] for r in v), default=None),
                                     "avg_bytes": sum(r["bytes"] for r in v) / len(v)}
                               for p, v in by_path.items()},
                }, f, ensure_ascii=False, indent=2)
        except OSError as e:
            # ★ 写不出去就**说清楚**，不能默默丢掉整份报告。
            print(f"\n★ 写入 {out} 失败: {e}", file=sys.stderr)
            return 1
        print(f"\n已写 {out}")

    return 0 if total_ok == total else 1


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--key", required=True, help="webui API key")
    ap.add_argument("--scale", type=int, default=1)
    ap.add_argument("--conc", type=int, default=0, help="0=按 scale 自动")
    ap.add_argument("--timeout", type=float, default=30.0)
    ap.add_argument("--json", default=None)
    ap.add_argument("--probe", action="store_true")
    a = ap.parse_args()

    if a.probe:
        sys.exit(probe(a.key))

    rounds = 20 * a.scale
    conc = a.conc or min(64, 8 * a.scale)
    sys.exit(bench(a.key, a.scale, rounds, conc, a.timeout, a.json))


if __name__ == "__main__":
    main()
