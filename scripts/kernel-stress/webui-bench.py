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
import os
import statistics
import sys
import time
import urllib.error
import urllib.request

# ★ 8080 上注册 HTTP 路由的插件有 4 个，但**监听端口不止 8080**。
#   实测（ss -ltnp）：
#     127.0.0.1:8080  webui(/api/v1/*) + remotedevice(/api/v1/device/*)
#                      + kbtree 的 /api/v1/knowledge/tree/* 子路径
#     127.0.0.1:9876  pluginmgr 全部路由（/plugins）—— 无 /api/v1 前缀
#     127.0.0.1:9892  kbtree 根路由（/categories /counts /search）—— 无前缀
#     127.0.0.1:9890  remotedevice 的设备 WS 网关（不是 REST，不压）
#
#   ⇒ 我第一版只压 8080 上的 webui，**漏了 pluginmgr 与 kbtree 根路由**。
#     它们不在 /api/v1 下（实测 :8080/plugins = 404）。
#
#   三套认证各不相同：
#     webui / kbtree-subpath  → config_webui 的 api_key
#     remotedevice             → config_remotedevice 的 ws_token
#     kbtree 根路由            → config_kbtree 的 token
#     pluginmgr                → 无需认证（实测 200）
PORTS = {
    "webui": "http://127.0.0.1:8080",
    "pluginmgr": "http://127.0.0.1:9876",
    "kbtree": "http://127.0.0.1:9892",
    "remotedevice": "http://127.0.0.1:8080",
}
BASE = PORTS["webui"]
# ★ 必须带 /api/v1 前缀。端点表里存的是**路径后半段**
#   （"/status"），拼 BASE + "/status" 会得到 "/status" ⇒ 全部 404。
#   首次跑 --probe 时正是这样：18 个端点全 404，而同一时刻 curl
#   /api/v1/status 是 200。⇒ 压测脚本**必须先 probe** 再压。
API_PREFIX = "/api/v1"

# 实测确认的只读端点（GET，200，不改状态）
READONLY = [
    # ── webui（:8080，api_key）────────────────────────────────────
    ("webui", "/api/v1/status", "小", "首页 KPI 之一"),
    ("webui", "/api/v1/network", "小", "网络信息"),
    ("webui", "/api/v1/terminals", "小", "终端列表"),
    ("webui", "/api/v1/tracker", "小", "跟踪器"),
    ("webui", "/api/v1/adapters", "小", "LLM 适配器"),
    ("webui", "/api/v1/agents", "小", "agent 列表"),
    ("webui", "/api/v1/config", "小", "配置"),
    ("webui", "/api/v1/persona", "小", "人格"),
    ("webui", "/api/v1/proxy/services", "小", "反代服务"),
    ("webui", "/api/v1/plugins", "中", "插件清单"),
    ("webui", "/api/v1/runtime", "中", "运行时"),
    ("webui", "/api/v1/memory/context", "中", "记忆上下文"),
    ("webui", "/api/v1/memory/text", "中", "文本记忆"),
    ("webui", "/api/v1/memory", "中", "记忆"),
    ("webui", "/api/v1/knowledge", "中", "知识库"),
    ("webui", "/api/v1/kernel", "大", "★ 总览 8 个 KPI 的数据源"),
    ("webui", "/api/v1/memory/graph", "大", "记忆图谱"),
    ("webui", "/api/v1/knowledge/tree", "大", "★ 知识树（实测最大 425KB）"),
    # ── kbtree 子路径（:8080 前缀内，api_key）────────────────────
    ("webui", "/api/v1/knowledge/tree/categories", "小", "kbtree：分类列表"),
    ("webui", "/api/v1/knowledge/tree/counts", "小", "kbtree：各分类计数"),
    # ── remotedevice（:8080，ws_token）──────────────────────────
    # 仅 /online：实测仅 GET + registry.OnlineList()，纯读。
    # ★ 不收 /device/push（会向真实设备下发）、/device/ws（长连接）、
    #   /device/{id}（逐设备查询，语义未核实）。
    ("remotedevice", "/api/v1/device/online", "小", "在线设备列表（只读）"),
    # ── pluginmgr（:9876，无认证，无 /api/v1 前缀）──────────────
    ("pluginmgr", "/plugins", "中", "插件清单（7.4KB，独立端口）"),
    # ── kbtree 根路由（:9892，kbtree 自己的 token）─────────────
    ("kbtree", "/categories", "小", "分类列表（独立端口）"),
    ("kbtree", "/counts", "小", "各分类计数（独立端口）"),
]


def _cfg(table: str, key: str) -> str:
    """从生产 config.db 取插件自己的 token（不打印内容）。

    只读打开（mode=ro）—— 压测脚本绝不能碰生产库的写路径。
    """
    import sqlite3

    db = "/home/newqqagent/config.db"
    if not os.path.exists(db):
        return ""
    con = None
    try:
        con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
        row = con.execute(f"select value from {table} where key=?", (key,)).fetchone()
        return row[0] if row else ""
    except sqlite3.Error:
        # 库锁、schema 变了、表不存在……一律当作「没取到」，
        # 由调用方报「缺 token」而不是抛栈。
        return ""
    finally:
        if con is not None:
            con.close()


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


def one_request(group: str, path: str, keys: dict, timeout: float) -> dict:
    """打一次请求。group 决定用哪个端口与哪把 key。

    三套认证实测各不相同（见 PORTS 处的说明）：
      webui / remotedevice 在 :8080，但 key 不同；
      pluginmgr / kbtree 在各自独立端口，前缀也不同。
    """
    base = PORTS[group]
    # ★ 约定：端点表里存**完整路径**（含 /api/v1 前缀，若该组需要）。
    #   这里直接拼，不再按 group 补前缀 ——
    #   早先「表里存后半段 + 按 group 补前缀」导致 remotedevice 那条
    #   拼成 /api/v1/api/v1/device/online ⇒ 落到 webui 兜底路由、
    #   返回 200 + 登录页 HTML。
    #   反过来「表里含前缀 + 仍补前缀」又让 webui 组全 404。
    #   两种都踩过 ⇒ 统一成一种：表里写全，代码不补。
    url = base + path
    key = keys.get(group, "")
    headers = {"X-API-Key": key} if key else {}
    req = urllib.request.Request(url, headers=headers)
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


def probe(keys: dict) -> int:
    """逐个确认端点真的可 GET、返回 JSON 而非登录页。

    ★ 必须先 probe：脚本第一版漏了 /api/v1 前缀，18 个端点全 404，
      而同一时刻 curl /api/v1/status 是 200。不 probe 就直接压，
      得出的结论会是「webui 全挂」。
    """
    print("端点探测（GET / 只读）")
    bad = 0
    for group, path, size, note in READONLY:
        r = one_request(group, path, keys, 15)
        flag = "✓" if r["ok"] else "✗"
        extra = ""
        if r.get("login_page"):
            extra = "  ← 返回登录页（认证有问题）"
            bad += 1
        elif r["status"] != 200:
            extra = f"  ← {r.get('err', '')[:80]}"
            bad += 1
        print(f"  {flag} {group:12} {path:24} {r['status']:>3}  {r['bytes']:>7}B  {r['ms']:>7.1f}ms  [{size}] {note}{extra}")
    return 1 if bad else 0


def bench(keys: dict, scale: int, rounds: int, conc: int, timeout: float, out: str | None):
    jobs = []
    for _ in range(rounds):
        for group, path, _, _ in READONLY:
            jobs.append((group, path))
    print(f"\n压测：{len(READONLY)} 端点 × {rounds} 轮 = {len(jobs)} 请求，"
          f"并发 {conc}（scale={scale}）")
    print("  只读端点，不触碰 /chat、/settings、/plugins 写接口、/device\n")

    t0 = time.perf_counter()
    results = []
    with cf.ThreadPoolExecutor(max_workers=conc) as ex:
        futs = {ex.submit(one_request, g, p, keys, timeout): (g, p) for g, p in jobs}
        for f in cf.as_completed(futs):
            results.append((futs[f], f.result()))
    wall = time.perf_counter() - t0

    by_path = {}
    for path, r in results:
        by_path.setdefault(path, []).append(r)

    print(f"{'组':12} {'端点':24} {'成功':>10} {'p50':>8} {'p95':>8} {'p99':>8} {'max':>8} {'平均KB':>8}")
    print("-" * 78)
    all_ms = []
    total_ok = total = 0
    failures = []
    for group, path, _, _ in READONLY:
        rs = by_path.get((group, path), [])
        if not rs:
            continue
        oks = [r for r in rs if r["ok"]]
        ms = [r["ms"] for r in oks] or [r["ms"] for r in rs]
        all_ms += ms
        total_ok += len(oks)
        total += len(rs)
        for r in rs:
            if not r["ok"]:
                failures.append((group, path, r))
        avg_kb = sum(r["bytes"] for r in rs) / len(rs) / 1024
        print(f"{group:12} {path:24} {len(oks):>4}/{len(rs):<5} "
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
        for group, path, r in failures[:10]:
            key2 = (group, path, r["status"])
            if key2 in seen:
                continue
            seen.add(key2)
            print(f"  {group:12} {path:24} status={r['status']:>3} "
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
                               for (g, p), v in by_path.items()},
                }, f, ensure_ascii=False, indent=2)
        except OSError as e:
            # ★ 写不出去就**说清楚**，不能默默丢掉整份报告。
            print(f"\n★ 写入 {out} 失败: {e}", file=sys.stderr)
            return 1
        print(f"\n已写 {out}")

    return 0 if total_ok == total else 1


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--key", required=True, help="webui api_key（config_webui）")
    ap.add_argument("--device-token", default="", help="remotedevice ws_token（默认从 config.db 读）")
    ap.add_argument("--kbtree-token", default="", help="kbtree token（默认从 config.db 读）")
    ap.add_argument("--scale", type=int, default=1)
    ap.add_argument("--conc", type=int, default=0, help="0=按 scale 自动")
    ap.add_argument("--timeout", type=float, default=30.0)
    ap.add_argument("--json", default=None)
    ap.add_argument("--probe", action="store_true")
    a = ap.parse_args()

    keys = {
        "webui": a.key,
        "remotedevice": a.device_token or _cfg("config_remotedevice", "ws_token"),
        "kbtree": a.kbtree_token or _cfg("config_kbtree", "token"),
        "pluginmgr": "",  # 实测无需认证
    }
    for g in ("webui", "remotedevice", "kbtree"):
        if not keys[g]:
            print(f"★ 缺 {g} 的 token，该组端点会 401。先跑 --probe 确认。", file=sys.stderr)

    if a.probe:
        sys.exit(probe(keys))

    rounds = 20 * a.scale
    conc = a.conc or min(64, 8 * a.scale)
    sys.exit(bench(keys, a.scale, rounds, conc, a.timeout, a.json))


if __name__ == "__main__":
    main()
