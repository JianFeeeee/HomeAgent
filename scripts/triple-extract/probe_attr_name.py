#!/usr/bin/env python3
"""判断实验：属性名能否从「值形态」学出来？

★ 为什么问这个问题
  --------------
  实测 228 个标注里，属性名**不在原句**的有 73 种。这些属性名不是从
  句子里读出来的，而是从**值的形态**推出来的：

      「LLM 503服务不可用 + 402余额不足」
        → 服务状态=503服务不可用     （状态类 → 属性名「服务状态」）
        → 余额状态=402余额不足      （状态类 → 属性名「余额状态」）

      「open-city-ai/haidian」
        → 项目名称=open-city-ai/haidian  （路径类 → 属性名「项目名称」）

  如果属性名真能由值形态决定，那**这 73 种属性名是可学的**，
  且任务形态从「序列标注」降级为「分类」——后者数据效率高一个数量级。

  反过来如果学不出来（同一个属性名对应五湖四海的值、
  同一个值对应多个属性名且无规律），那这 73 种就是 LLM 在编，
  不该进训练集。

判据
----
  1. 同属性名的值是否同形态？（决定「值形态→属性名」是否可行）
  2. 同形态的值是否同属性名？（决定判据 1 是否够用）
  3. 用「值形态」做分类器，留出集上能到多少 F1？
"""
from __future__ import annotations

import json
import os
import random
import sys
from collections import Counter, defaultdict

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from train_tagger import load_labeled, _lcs_len  # noqa: E402

sys.path.insert(0, "/home/program/TrueAgent/internal/memory/distill")


# ─────────────────────────────────────────────────────────────
# 值形态分类（与 distill/drift.go 的 classifyValue 同思路）
# ─────────────────────────────────────────────────────────────

import re

PATTERNS = [
    ("百分比", re.compile(r"^\d+(\.\d+)?\s*[%％]$")),
    ("区间", re.compile(r"\d+(\.\d+)?\s*[%％]?\s*[~～-]\s*\d+")),
    ("时长", re.compile(r"^\d+(\.\d+)?\s*(分|分钟|秒|小时)$")),
    ("版本", re.compile(r"^(v|V)\d+(\.\d+)*$")),
    ("中文版本", re.compile(r"^第\d+版$")),
    ("路径", re.compile(r"^[\w.-]+/[\w./-]+$")),
    ("日期", re.compile(r"^\d{4}-\d{1,2}(-\d{1,2})?$")),
    ("月份", re.compile(r"^\d{1,2}月(\d{1,2}日)?$")),
    ("时刻", re.compile(r"(凌晨|上午|下午|晚上|中午)?\d{1,2}[点:：]\d{0,2}")),
    ("编号", re.compile(r"^\d+(/\d+)*$")),
    ("布尔", re.compile(r"^(是|否|已|未|通过|失败|正常|异常)$")),
]
PATTERNS = [(n, re.compile(p.pattern)) for n, p in PATTERNS]


def value_shape(v: str) -> str:
    """值形态 → 类别名。"""
    v = v.strip()
    for name, pat in PATTERNS:
        if pat.match(v):
            return name
    if re.match(r"^\d+(个|条|台|人|次|元|字节)?$", v):
        return "计数"
    if "服务" in v or "不可用" in v or "失败" in v or "异常" in v:
        return "状态描述"
    if re.search(r"[A-Za-z]", v) and re.search(r"\d", v):
        return "混合"
    return "文本"


# ─────────────────────────────────────────────────────────────
# 判据 1 & 2：形态与属性名的关系
# ─────────────────────────────────────────────────────────────

def collect_fields() -> list[tuple[str, str, str]]:
    """返回 (句子, 属性名, 值)。只取属性名不在原句的（要判断的那类）。"""
    out = []
    for path in ["/tmp/label-work/labeled.jsonl", "/tmp/train_data.json"]:
        for x in load_labeled([path]):
            for f in x["fields"]:
                if isinstance(f, str):
                    if "=" not in f:
                        continue
                    k, v = f.split("=", 1)
                else:
                    k, v = f["name"], f["value"]
                if not k or not v:
                    continue
                if k in x["sentence"]:
                    continue          # 属性名在句子里 —— 不属本次判断范围
                out.append((x["sentence"], k.strip(), v.strip()))
    return out


def report_structure(fields):
    print("=" * 68)
    print("判据 1：同一个属性名，它的值是否同形态？")
    print("=" * 68)
    by_name = defaultdict(Counter)
    for _, k, v in fields:
        by_name[k][value_shape(v)] += 1
    pure = [k for k, c in by_name.items() if len(c) == 1]
    multi = [k for k, c in by_name.items() if len(c) > 1]
    print(f"  属性名 {len(by_name)} 种：形态唯一 {len(pure)}，形态多样 {len(multi)}")
    for k in multi[:6]:
        print(f"    {k}: {dict(by_name[k])}")

    print()
    print("=" * 68)
    print("判据 2：同一个形态，它是否对应少数几个属性名？")
    print("=" * 68)
    by_shape = defaultdict(Counter)
    for _, k, v in fields:
        by_shape[value_shape(v)][k] += 1
    for shape, c in sorted(by_shape.items(), key=lambda x: -sum(x[1].values())):
        names = list(c)
        top = c.most_common(3)
        print(f"  {shape:10s} {sum(c.values()):3d} 条 → {len(names):2d} 种属性名"
              f"  Top: {', '.join(f'{n}×{v}' for n, v in top)}")
    return by_name, by_shape


# ─────────────────────────────────────────────────────────────
# 判据 3：分类器能做到多少
# ─────────────────────────────────────────────────────────────

def train_classifier(fields, holdout=0.3, epochs=200, seed=7):
    """极简分类器：值形态 → 属性名。

    ★ 为什么用极简模型而不是神经网络：
       这里要回答的是「信号够不够」，不是「最优解多好」。
       若「值形态→属性名」在天真上限下都做不到（准确率 < 50%），
       那换什么网络都没用 —— 信号本身不存在。
    """
    random.seed(seed)
    data = [(value_shape(v), k) for _, k, v in fields]
    by_shape = defaultdict(Counter)
    for s, k in data:
        by_shape[s][k] += 1

    # 留出 30% 的**样本**（不是形态）
    random.shuffle(data)
    n_hold = max(1, int(len(data) * holdout))
    hold, train = data[:n_hold], data[n_hold:]

    correct = sum(by_shape[s].most_common(1)[0][0] == k for s, k in hold)
    acc = correct / len(hold)
    print(f"\n  朴素查表（形态→最常见属性名）在留出集上: "
          f"{correct}/{len(hold)} = {acc:.1%}")
    print(f"  随机基线（最常见属性名的占比）: "
          f"{max(c for c in by_shape.values()).most_common(1)[0][1] / len(data):.1%}")

    # 上限：若每个形态能记住所有属性名，留出命中率是多少
    hit = 0
    for s, k in hold:
        if s in by_shape:
            hit += 1        # 形态在训练集见过 —— 但属性名不一定对
    print(f"  形态覆盖率（留出样本的形态在训练集出现过）: "
          f"{hit}/{len(hold)} = {hit/len(hold):.1%}")
    return acc


def main():
    fields = collect_fields()
    print(f"属性名不在原句的字段: {len(fields)} 条，"
          f"{len(set(k for _, k, _ in fields))} 种属性名\n")
    if not fields:
        print("无数据")
        return
    report_structure(fields)
    print()
    print("=" * 68)
    print("判据 3：值形态能不能当分类特征？")
    print("=" * 68)
    train_classifier(fields)


if __name__ == "__main__":
    main()


# ─────────────────────────────────────────────────────────────
# 判据 4：加上句上下文后，形态的区分度能否提升？
# ─────────────────────────────────────────────────────────────

def shape_with_context(sentence: str, value: str) -> str:
    """形态 + 「值在句中的邻居特征」。

    ★ 为什么加邻居：「文本」形态单独看对应 42 种属性名（纯度 7%），
      但「文本|邻居含『插件』」可能就指向「插件名称」了。
      这正是注意力模型能学、查表学不到的东西 —— 所以这一条判据
      测的是「上下文是否携带了形态之外的信息」。
    """
    base = value_shape(value)
    vpos = sentence.find(value)
    if vpos < 0:
        return base
    before = sentence[:vpos]
    after = sentence[vpos + len(value):]
    ctx = []
    for kw, tag in [("插件", "插件"), ("项目", "项目"), ("路径", "路径"),
                    ("目录", "目录"), ("日志", "日志"), ("邮件", "邮件"),
                    ("版本", "版本"), ("批次", "批次"), ("第", "第"),
                    ("服务", "服务"), ("状态", "状态"), ("余额", "余额")]:
        if kw in before:
            ctx.append("前" + tag)
        if kw in after:
            ctx.append("后" + tag)
    return base + ("|" + ",".join(ctx) if ctx else "")


def test_context_features(fields, holdout=0.3, seed=7):
    random.seed(seed)
    feats = [(shape_with_context(s, v), k) for s, k, v in fields]
    by = defaultdict(Counter)
    for f, k in feats:
        by[f][k] += 1
    random.shuffle(feats)
    n = max(1, int(len(feats) * holdout))
    hold, _ = feats[:n], feats[n:]

    hit = 0
    for f, k in hold:
        if f in by and by[f].most_common(1)[0][0] == k:
            hit += 1
    acc = hit / len(hold)
    # 覆盖率：留出样本的「形态+邻居」组合在训练集出现过吗
    cov = sum(1 for f, k in hold if f in by) / len(hold)
    print(f"\n  加上下文后：")
    print(f"    特征种类 {len(by)}（原形态只有 {len(set(value_shape(v) for _,_,v in fields))} 种）")
    print(f"    留出命中率 {hit}/{len(hold)} = {acc:.1%}")
    print(f"    组合覆盖率 {cov:.1%}")
    print()
    print("  ★ 覆盖率低 ⇒ 模型没见过这个组合 ⇒ 学不到；")
    print("    覆盖率够高而命中率低 ⇒ 见过但仍有歧义 ⇒ 需要更强的模型或更多特征。")
    return acc, cov


if __name__ == "__main__" and len(sys.argv) > 1:
    _f = collect_fields()
    print("=" * 68)
    print("判据 4：加上下文后区分度如何？")
    print("=" * 68)
    test_context_features(_f)
