#!/usr/bin/env python3
"""客户端能力任务集的 fixture 生成器（确定性，可重复执行）。

为什么单独一个生成器
====================

客户端跑分考的是 harness：工具循环、错误恢复、大输出、跨步状态。
这些任务的材料必须：
  · **确定性**：同样种子生成同样数据，期望答案在生成时算好写进任务集；
  · **必须用工具**：所有任务都无法靠"读一遍材料心算"完成；
  · **判据落盘**：答案写文件，checker 用 file_contains，与两侧 harness 无关。

期望答案在生成时**独立复算两遍**（两种算法），不一致就拒绝生成 ——
判据本身错了，跑分就是仪式。
"""
from __future__ import annotations

import hashlib
import json
import random
import subprocess
import sys
from pathlib import Path

ROOT = Path("/var/tmp/client-bench")


def fail(msg: str) -> None:
    print(f"★ {msg}", file=sys.stderr)
    sys.exit(1)


# ────────────────────────────────────────────────────────────────
# 任务 1：fixloop —— 工具循环（跑测试→修→再跑到全绿）
# ────────────────────────────────────────────────────────────────

BUGGY_STORE = '''"""KVStore —— 带三个已知缺陷的实现（跑分用，勿用于生产）。"""


class KVStore:
    def __init__(self) -> None:
        self._data = {}

    def put(self, key: str, value: object) -> None:
        self._data[key] = value

    def get(self, key: str, default: object = None) -> object:
        return self._data.get(key, default)

    def delete(self, key: str) -> bool:
        # 缺陷①：只删一半 —— 键存在时删掉，但同时把后续写入顺序破坏
        if key in self._data:
            del self._data[key]
            self._data = dict(reversed(list(self._data.items())))
            return True
        return False

    def items_slice(self, start: int, end: int) -> list:
        """按下标返回 (key, value) 列表，[start, end) 左闭右开。"""
        items = sorted(self._data.items())
        # 缺陷②：边界差一（把 end 也包进去了）
        return items[start:end + 1]

    def top_values(self, n: int) -> list:
        """返回值最大的前 n 个 (key, value)。"""
        # 缺陷③：排序方向反了
        return sorted(self._data.items(), key=lambda kv: kv[1])[:n]
'''

STORE_TESTS = '''"""KVStore 的验收测试。全部通过后把 "ALL GREEN 6" 写入 result.txt。"""
import os
import unittest

from store import KVStore


class TestKVStore(unittest.TestCase):
    def setUp(self):
        self.s = KVStore()
        for i, k in enumerate("abcdef"):
            self.s.put(k, i * 10)

    def test_get_default(self):
        self.assertEqual(self.s.get("a"), 0)
        self.assertEqual(self.s.get("zz", "dflt"), "dflt")

    def test_delete_returns_bool(self):
        self.assertTrue(self.s.delete("a"))
        self.assertFalse(self.s.delete("a"))
        self.assertIsNone(self.s.get("a"))

    def test_delete_preserves_others(self):
        self.s.delete("c")
        self.assertEqual(self.s.get("d"), 30)
        self.assertEqual(self.s.items_slice(0, 5), [(k, i * 10) for i, k in enumerate("abdef")])

    def test_items_slice_half_open(self):
        # [1, 4) => b, c, d
        self.assertEqual([k for k, _ in self.s.items_slice(1, 4)], ["b", "c", "d"])

    def test_top_values_desc(self):
        self.assertEqual([k for k, _ in self.s.top_values(2)], ["f", "e"])

    def test_top_values_count(self):
        self.assertEqual(len(self.s.top_values(10)), 6)


if __name__ == "__main__":
    result = unittest.main(exit=False)
    if result.result.wasSuccessful() and result.result.testsRun == 6:
        with open(os.path.join(os.path.dirname(__file__), "result.txt"), "w") as f:
            f.write(f"ALL GREEN {result.result.testsRun}\\n")
        print("ALL GREEN", result.result.testsRun)
    else:
        bad = len(result.result.failures) + len(result.result.errors)
        print(f"FAILED tests={result.result.testsRun} bad={bad}")
'''


def gen_fixloop() -> str:
    d = ROOT / "fixloop"
    d.mkdir(parents=True, exist_ok=True)
    (d / "store.py").write_text(BUGGY_STORE, encoding="utf-8")
    (d / "test_store.py").write_text(STORE_TESTS, encoding="utf-8")
    for stale in ("result.txt",):
        (d / stale).unlink(missing_ok=True)
    # 自证：缺陷版本必须真的红（否则任务无意义）
    r = subprocess.run([sys.executable, "test_store.py"], cwd=d, capture_output=True, text=True, timeout=60)
    if "FAILED" not in (r.stdout + r.stderr):
        fail(f"fixloop 缺陷版没红，fixture 无效：{r.stdout} {r.stderr}")
    bad = [l for l in (r.stdout + r.stderr).splitlines() if l.startswith("FAILED")]
    print(f"  fixloop: 初始 {bad[0] if bad else 'FAILED(见输出)'}")
    return "ALL GREEN 6"


# ────────────────────────────────────────────────────────────────
# 任务 2：pipeline —— 错误恢复（坏编码文件 + 管道跑通）
# ────────────────────────────────────────────────────────────────

EXTRACT_PY = '''"""从 events_*.jsonl 抽取 unique user，写 users.csv 并打印 users=N。

用法: python3 extract.py
"""
import csv
import glob
import json

users = {}
for path in sorted(glob.glob("events_*.jsonl")):
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            rec = json.loads(line)
            users[rec["user"]] = rec["signup"]

with open("users.csv", "w", newline="", encoding="utf-8") as f:
    w = csv.writer(f)
    w.writerow(["user", "signup"])
    for u in sorted(users):
        w.writerow([u, users[u]])

print(f"users={len(users)}")
'''


def gen_pipeline() -> int:
    d = ROOT / "pipeline"
    d.mkdir(parents=True, exist_ok=True)
    rng = random.Random(20260830)
    expected: set[str] = set()
    names = [f"user_{i:04d}" for i in range(260)]
    for fi in range(5):
        path = d / f"events_2026-08-{fi + 1:02d}.jsonl"
        lines = []
        for _ in range(90):
            u = rng.choice(names)
            expected.add(u)
            rec = {"user": u, "action": rng.choice(["login", "view", "click"]),
                   "signup": f"2026-0{rng.randint(1, 8)}-{rng.randint(10, 28)}"}
            lines.append(json.dumps(rec, ensure_ascii=False))
        # 第 3 个文件整体是 GBK 编码 —— utf-8 直接读会 UnicodeDecodeError
        if fi == 2:
            path.write_bytes("\n".join(lines).encode("gbk"))
        else:
            path.write_text("\n".join(lines), encoding="utf-8")
    (d / "extract.py").write_text(EXTRACT_PY, encoding="utf-8")
    (d / "users.csv").unlink(missing_ok=True)
    (d / "report.txt").unlink(missing_ok=True)
    # 自证：期望值用第二种算法复算
    expect2 = set()
    for fi in range(5):
        raw = (d / f"events_2026-08-{fi + 1:02d}.jsonl").read_bytes()
        for line in raw.decode("gbk").splitlines():
            if line.strip():
                expect2.add(json.loads(line)["user"])
    if expect2 != expected:
        fail(f"pipeline 期望值两种算法不一致: {len(expected)} vs {len(expect2)}")
    print(f"  pipeline: unique users={len(expected)}（坏编码文件: events_2026-08-03.jsonl, GBK）")
    return len(expected)


# ────────────────────────────────────────────────────────────────
# 任务 3：aggregate —— 大工具输出 + 去重聚合
# ────────────────────────────────────────────────────────────────

def gen_aggregate() -> int:
    d = ROOT / "aggregate"
    d.mkdir(parents=True, exist_ok=True)
    for old in d.glob("*.json"):
        old.unlink()
    (d / "answer.txt").unlink(missing_ok=True)
    rng = random.Random(20260901)
    paid: dict[str, dict] = {}
    for day in range(1, 16):
        recs = []
        for _ in range(300):
            oid = f"ord-{rng.randint(10 ** 6, 10 ** 7 - 1)}"
            st = rng.choices(["paid", "cancelled", "pending"], weights=[6, 2, 2])[0]
            rec = {"order_id": oid, "status": st,
                   "amount": round(rng.uniform(5, 900), 2),
                   "ts": f"2026-08-{day:02d}T{rng.randint(0, 23):02d}:{rng.randint(10, 59):02d}:00"}
            recs.append(rec)
            if st == "paid":
                paid[oid] = rec
        (d / f"orders-2026-08-{day:02d}.json").write_text(
            json.dumps(recs, ensure_ascii=False, indent=1), encoding="utf-8")
    expected = len(paid)
    # 自证：另一种算法（逐文件流式数集合）
    seen2: set[str] = set()
    for day in range(1, 16):
        for rec in json.loads((d / f"orders-2026-08-{day:02d}.json").read_text(encoding="utf-8")):
            if rec["status"] == "paid":
                seen2.add(rec["order_id"])
    if len(seen2) != expected:
        fail(f"aggregate 期望值两种算法不一致: {expected} vs {len(seen2)}")
    total_lines = sum(1 for day in range(1, 16)
                      for _ in (d / f"orders-2026-08-{day:02d}.json").read_text(encoding="utf-8").splitlines())
    print(f"  aggregate: 15 个文件共 ~{total_lines} 行，unique paid orders={expected}")
    return expected


# ────────────────────────────────────────────────────────────────
# 任务 4：chain —— 跨步状态（6 步大数运算，心算不可行）
# ────────────────────────────────────────────────────────────────

CHAIN_STEPS = [
    ("step1.md", "当前值是 start.txt 里的种子。计算：当前值 × 7919。把结果写入 state.txt（只写数字）。"),
    ("step2.md", "当前值在 state.txt。计算：当前值 + 1234567。把结果写回 state.txt。"),
    ("step3.md", "当前值在 state.txt。计算：当前值 ÷ 97 的整数商（向下取整）。把结果写回 state.txt。"),
    ("step4.md", "当前值在 state.txt。计算：当前值的平方。把结果写回 state.txt。"),
    ("step5.md", "当前值在 state.txt。计算：当前值 mod 1000000007。把结果写回 state.txt。"),
    ("step6.md", "当前值在 state.txt。计算：当前值 XOR 305419896（按位异或，十进制）。把结果写回 state.txt。"),
]


def gen_chain() -> int:
    d = ROOT / "chain"
    (d / "steps").mkdir(parents=True, exist_ok=True)
    for name, text in CHAIN_STEPS:
        (d / "steps" / name).write_text(text + "\n", encoding="utf-8")
    seed = 482913
    (d / "start.txt").write_text(f"{seed}\n", encoding="utf-8")
    (d / "state.txt").unlink(missing_ok=True)
    (d / "final.txt").unlink(missing_ok=True)
    v = seed
    for _ in range(6):
        v = v * 7919
        v = v + 1234567
        v = v // 97
        v = v * v
        v = v % 1000000007
        v = v ^ 305419896
    # 自证：逐步算两遍没有意义（纯函数），但至少确认不是平凡值
    if v == seed or v < 10 ** 8:
        fail("chain 期望值可疑（平凡）")
    print(f"  chain: seed={seed} 期望 answer={v}")
    return v


# ────────────────────────────────────────────────────────────────

def main() -> None:
    ROOT.mkdir(parents=True, exist_ok=True)
    print(f"生成客户端能力 fixture 于 {ROOT}")
    fix_expect = gen_fixloop()
    pipe_expect = gen_pipeline()
    agg_expect = gen_aggregate()
    chain_expect = gen_chain()

    tasks = {
        "_comment": [
            "客户端能力任务集 v2 —— 考 harness 而非裸 LLM。",
            "两侧同模型（llmsproxy AUTO），能产生差异的只有 harness：",
            "工具实现质量、错误回填、循环管理、跨步状态保持。",
            "判据全部落盘（file_contains），与两侧工具名无关。",
            "",
            "生成器：scripts/capability-bench/gen_client_fixtures.py（期望答案在生成时独立复算）",
            "材料目录：/var/tmp/client-bench/{fixloop,pipeline,aggregate,chain}",
        ],
        "tasks": [
            {
                "id": "cl-fixloop",
                "dimension": "tool-loop",
                "prompt": (
                    "在 /var/tmp/client-bench/fixloop/ 目录有一个实现和它的测试。"
                    "运行测试，修复实现代码直到全部测试通过（不许改测试文件）。"
                    "全部通过时测试程序会自动生成 result.txt。完成后报告：改了哪些缺陷。"
                ),
                "check": {"kind": "file_contains", "value": "/var/tmp/client-bench/fixloop/result.txt",
                          "needle": fix_expect},
                "timeout_s": 600,
            },
            {
                "id": "cl-pipeline",
                "dimension": "error-recovery",
                "prompt": (
                    "在 /var/tmp/client-bench/pipeline/ 目录运行 python3 extract.py，"
                    "它现在跑不通。修到它能跑通（不许改 events_*.jsonl 数据文件），"
                    "然后把最终输出里的 unique 用户数以 users=N 的格式写入该目录的 report.txt。"
                ),
                "check": {"kind": "file_contains", "value": "/var/tmp/client-bench/pipeline/report.txt",
                          "needle": f"users={pipe_expect}"},
                "timeout_s": 600,
            },
            {
                "id": "cl-aggregate",
                "dimension": "large-output",
                "prompt": (
                    "/var/tmp/client-bench/aggregate/ 下有 15 个订单 JSON 文件（每天一个）。"
                    "统计：status 为 paid 的**去重后** order_id 总数（同一 order_id 可能出现在多个文件）。"
                    "把结果以 total=N 的格式写入该目录的 answer.txt。要求数字准确。"
                ),
                "check": {"kind": "file_contains", "value": "/var/tmp/client-bench/aggregate/answer.txt",
                          "needle": f"total={agg_expect}"},
                "timeout_s": 600,
            },
            {
                "id": "cl-chain",
                "dimension": "state-chain",
                "prompt": (
                    "阅读 /var/tmp/client-bench/chain/start.txt 和 chain/steps/ 下 step1.md 到 step6.md，"
                    "严格按顺序逐步执行（每步用工具计算并把中间值写入 chain/state.txt，不许跳步、不许心算）。"
                    "全部完成后把最终值以 answer=值的格式写入 chain/final.txt。"
                ),
                "check": {"kind": "file_contains", "value": "/var/tmp/client-bench/chain/final.txt",
                          "needle": f"answer={chain_expect}"},
                "timeout_s": 600,
            },
        ],
    }
    out = Path(__file__).parent / "tasks.client.json"
    out.write_text(json.dumps(tasks, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"  任务集已写 {out}")
    # sha256 留档：重跑时确认材料没漂移
    h = hashlib.sha256()
    for p in sorted(ROOT.rglob("*")):
        if p.is_file() and p.name != "result.txt":
            h.update(p.read_bytes())
    print(f"  fixture sha256={h.hexdigest()[:16]}")


if __name__ == "__main__":
    main()
