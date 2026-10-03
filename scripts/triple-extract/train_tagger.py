#!/usr/bin/env python3
"""三元组抽取的序列标注训练脚本（CPU）。

架构（见 docs/zh/triple-extract-attention.md）：
  字符级嵌入 → 4 层 Transformer encoder → 9 类 BIO 标签

★ 为什么从零训而不是微调预训练模型
  任务词汇封闭（运维数字/端口/批次/分机号），要学的是**版式**
  （哪里是属性名、哪里是值），而版式没有通用先验。
  预训练模型的 2~3 亿参数在 CPU 上既慢又无益。

★ 为什么字符级而不是词级
  词级分词会切错「值班室分机号」（jieba 给 3 个词），
  而属性名的边界恰恰是这个整体。

★ 只学「字面可对齐」的那 82%
  实测 260 个 LLM 标注里 48 个（18%）是「主语派生」——
  如「第130批」的批次号=130，值在原句里不是连续片段。
  那部分由 DeriveSubjects 规则处理，不进网络。
"""
from __future__ import annotations

import json
import math
import os
import random
import re
import sys
import time
from collections import Counter
from dataclasses import dataclass

import torch
import torch.nn as nn
import torch.nn.functional as F

# ─────────────────────────────────────────────────────────────
# 标签集
# ─────────────────────────────────────────────────────────────

LABELS = ["<pad>", "<unk>", "<bos>",
          "O",       # 其它/连接词（从、改为、·）
          "B-DIM", "I-DIM",   # 属性名
          "B-VAL", "I-VAL",   # 值
          "B-SUBJ", "I-SUBJ"] # 主语
L2I = {l: i for i, l in enumerate(LABELS)}
N_LABELS = len(LABELS)
DIM_LABELS = {L2I["B-DIM"], L2I["I-DIM"]}
VAL_LABELS = {L2I["B-VAL"], L2I["I-VAL"]}
SUBJ_LABELS = {L2I["B-SUBJ"], L2I["I-SUBJ"]}


# ─────────────────────────────────────────────────────────────
# 数据准备
# ─────────────────────────────────────────────────────────────

def load_labeled(paths: list[str]) -> list[dict]:
    """读入多来源标注（JSON 与 JSONL 都支持）。"""
    samples = []
    for p in paths:
        if not os.path.exists(p):
            continue
        if p.endswith(".jsonl"):
            for line in open(p):
                line = line.strip()
                if not line:
                    continue
                d = json.loads(line)
                if d.get("fields"):
                    samples.append(d)
        else:
            for d in json.load(open(p)):
                if d.get("fields"):
                    samples.append(d)
    return samples


def normalize_name(name: str) -> str:
    """把 LLM 给的属性名归一到受控形态（与 distill.NormalizeDimension 同思路）。

    ★ 为什么要这一步：LLM 给的是「告警规则数」，原句里是「告警规则」。
       直接按 LLM 的名字去原句里找会找不到（实测 55% 对齐失败）。
    """
    name = name.strip()
    aliases = {
        "告警规则": "告警规则数", "告警规则条数": "告警规则数",
        "值班手册": "值班手册版本", "值班手册版": "值班手册版本",
        "容量": "容量预警", "容量阈值": "容量预警",
        "时间": "发布窗口", "发布时间": "发布窗口", "窗口": "发布窗口",
        "停机": "停机时长", "停机时间": "停机时长",
        "灰度": "灰度比例", "灰度百分比": "灰度比例",
        "排期月份": "排期",
        "批号": "批次号",
    }
    return aliases.get(name, name)


def _lcs_len(a: str, b: str) -> int:
    """最长公共子序列长度。"""
    if not a or not b:
        return 0
    prev = [0] * (len(b) + 1)
    for i in range(1, len(a) + 1):
        cur = [0] * (len(b) + 1)
        for j in range(1, len(b) + 1):
            cur[j] = prev[j - 1] + 1 if a[i - 1] == b[j - 1] else max(prev[j], cur[j - 1])
        prev = cur
    return prev[len(b)]


def find_dim_in(name: str, before: str, min_score: float = 0.5) -> str | None:
    """在 before 里找与 name 最像的连续片段（模糊匹配）。

    ★ 为什么必须模糊匹配而不是硬编码别名表
      --------------------------------------
      LLM 给的是**归一后**的名字，原句里是**口语形态**：

          告警规则数     ← 原句「告警规则9条」      （多了「数」）
          值班手册版本   ← 原句「值班手册第6版」   （多了「版本」且位置不同）
          发布窗口       ← 原句「凌晨2点」        （名字完全不同）

      别名表要人工维护且必然不全（实测 17 种维度里至少 4 种需要反向映射），
      而模糊匹配是通用的。

    ★ 实测收益（260 个标注）：可对齐 105 → 180（+71%）
      剩下的 62 条是真正的主语派生（批次号 / 版本 / 发布窗口）——
      它们的值不在句子表面（「第**130**批」的 130 不是连续片段），
      那部分交给 DeriveSubjects 规则，不进网络。
    """
    if name in before:
        return name
    if len(name) < 2:
        return None
    best, best_score = None, 0.0
    for L in range(max(2, len(name) - 2), len(name) + 3):
        if L > len(before):
            continue
        for i in range(len(before) - L + 1):
            cand = before[i:i + L]
            score = _lcs_len(name, cand) / max(len(name), len(cand))
            if score > best_score:
                best, best_score = cand, score
    return best if best_score >= min_score else None


def align(sentence: str, fields: list[dict]) -> tuple[list[str] | None, str | None]:
    """把字段对齐到字符位置，产出 BIO 标签序列。

    返回 (labels, err)。err 非 nil 时该样本是坏数据，不进训练集。

    ★ 三道过滤（都是实测踩出来的）：
      1. 值必须原样出现在原句 —— LLM 幻觉闸门
      2. 值长度 ≤ 句子的 60% —— 拦住「值 = 整句」（实测 3% 的标注如此）
      3. 属性名要能在原句里模糊对上（find_dim_in）——
         对不上说明是「主语派生」维度，交给规则不交给网络
    """
    labels = ["O"] * len(sentence)
    for f in fields:
        # 兼容两种输入：LLM 标注（dict）与旧导出（"维度=值" 字符串）
        if isinstance(f, str):
            if "=" not in f:
                return None, "字段格式错（无 =）"
            raw_name, val = f.split("=", 1)
        else:
            raw_name = f.get("name") or ""
            val = f.get("value") or ""
        raw_name, val = raw_name.strip(), val.strip()
        if not val:
            return None, "空值"
        if val not in sentence:
            return None, "值不在原句"
        if len(val) > 0.6 * len(sentence):
            return None, "值占句子过半"
        vpos = sentence.find(val)
        before = sentence[:vpos]
        surface = find_dim_in(raw_name, before)
        if surface is None:
            return None, "主语派生（属性名对不上）"
        npos = before.rfind(surface)
        for i in range(npos, npos + len(surface)):
            labels[i] = "I-DIM" if i > npos else "B-DIM"
        for i in range(vpos, vpos + len(val)):
            labels[i] = "I-VAL" if i > vpos else "B-VAL"
    return labels, None


@dataclass
class Example:
    ids: list[int]
    labels: list[int]
    raw: str


def build_dataset(samples: list[dict], vocab: dict) -> tuple[list[Example], Counter]:
    out, reasons = [], Counter()
    for s in samples:
        labels, err = align(s["sentence"], s["fields"])
        if labels is None:
            reasons[err] += 1
            continue
        ids = [vocab.get("<bos>")] + [vocab.get(c, vocab["<unk>"]) for c in s["sentence"]]
        out.append(Example(ids, [L2I["<pad>"]] + [L2I[l] for l in labels], s["sentence"]))
    return out, reasons


def build_vocab(samples: list[dict], min_freq: int = 1) -> dict:
    c = Counter()
    for s in samples:
        c.update(s["sentence"])
    vocab = {"<pad>": 0, "<unk>": 1, "<bos>": 2}
    for ch, n in c.most_common():
        if n >= min_freq:
            vocab[ch] = len(vocab)
    return vocab


# ─────────────────────────────────────────────────────────────
# 模型
# ─────────────────────────────────────────────────────────────

class CharTagger(nn.Module):
    """字符级 BIO 标注器。

    ★ 注意力在这里做什么（本方案的核心假设）：
       句子里每一处属性名只该关注它自己那个值。
       「停机」关注「4分」、「灰度」关注「10%」——
       不同的头分工不同，这是固定向量做不到的。
    """

    def __init__(self, vocab_size: int, d_model: int = 128, n_layers: int = 4,
                 n_heads: int = 4, d_ff: int = 256, dropout: float = 0.2,
                 max_len: int = 128):
        super().__init__()
        self.emb = nn.Embedding(vocab_size, d_model, padding_idx=0)
        self.pos = nn.Embedding(max_len, d_model)
        layer = nn.TransformerEncoderLayer(
            d_model=d_model, nhead=n_heads, dim_feedforward=d_ff,
            dropout=dropout, batch_first=True, norm_first=True)
        self.enc = nn.TransformerEncoder(layer, num_layers=n_layers)
        self.norm = nn.LayerNorm(d_model)
        self.drop = nn.Dropout(dropout)
        self.out = nn.Linear(d_model, N_LABELS)
        self.max_len = max_len

    def forward(self, ids: torch.Tensor, pad_mask: torch.Tensor) -> torch.Tensor:
        b, t = ids.shape
        pos = torch.arange(t, device=ids.device).unsqueeze(0).expand(b, t)
        h = self.emb(ids) + self.pos(pos)
        h = self.drop(h)
        h = self.enc(h, src_key_padding_mask=pad_mask)
        return self.out(self.norm(h))


def count_params(m: nn.Module) -> int:
    return sum(p.numel() for p in m.parameters() if p.requires_grad)


# ─────────────────────────────────────────────────────────────
# 解码：标签序列 → 三元组
# ─────────────────────────────────────────────────────────────

def decode(ids: torch.Tensor, preds: torch.Tensor, sentence: str) -> list[dict]:
    """从 BIO 序列解出 (维度, 值) 对。

    ★ 用 B-/I- 拼接（不是取每段首尾），因为属性名可能跨多个词：
      「等待队列长度告警阈值」是一个整体，切成「等待队列」+「告警阈值」
      就会建错边。
    """
    seq = [LABELS[i] for i in preds.tolist()]
    dims, vals = [], []
    for i, lab in enumerate(seq):
        if i == 0 or i >= len(sentence):
            continue
        if lab == "B-DIM":
            dims.append([i])
        elif lab == "I-DIM" and dims:
            dims[-1].append(i)
        elif lab == "B-VAL":
            vals.append([i])
        elif lab == "I-VAL" and vals:
            vals[-1].append(i)
    dim_str = ["".join(sentence[i] for i in g) for g in dims]
    val_str = ["".join(sentence[i] for i in g) for g in vals]
    out = []
    for d, v in zip(dim_str, val_str):
        if d and v:
            out.append({"name": normalize_name(d), "value": v})
    return out


# ─────────────────────────────────────────────────────────────
# 训练
# ─────────────────────────────────────────────────────────────

def batches(data: list[Example], bs: int, shuffle: bool, pad_id: int = 0):
    idx = list(range(len(data)))
    if shuffle:
        random.shuffle(idx)
    for k in range(0, len(idx), bs):
        chunk = [data[i] for i in idx[k:k + bs]]
        t = max(len(c.ids) for c in chunk)
        ids = torch.full((len(chunk), t), pad_id, dtype=torch.long)
        lab = torch.full((len(chunk), t), L2I["<pad>"], dtype=torch.long)
        mask = torch.ones((len(chunk), t), dtype=torch.bool)
        for r, c in enumerate(chunk):
            ids[r, :len(c.ids)] = torch.tensor(c.ids)
            lab[r, :len(c.labels)] = torch.tensor(c.labels)
            mask[r, :len(c.ids)] = False
        yield ids, lab, mask


def evaluate(model: nn.Module, data: list[Example], pad_id: int = 0) -> dict:
    """逐 token 的 precision/recall/f1（只算实体内部的 token）。"""
    model.eval()
    tp = fp = fn = 0
    with torch.no_grad():
        for ids, lab, mask in batches(data, 16, False, pad_id):
            logits = model(ids, mask)
            pred = logits.argmax(-1)
            for b in range(ids.size(0)):
                for t in range(ids.size(1)):
                    if mask[b, t]:
                        continue
                    gold, pr = lab[b, t].item(), pred[b, t].item()
                    gold_in = gold in DIM_LABELS | VAL_LABELS | SUBJ_LABELS
                    pr_in = pr in DIM_LABELS | VAL_LABELS | SUBJ_LABELS
                    if gold_in and pr_in:
                        if gold == pr:
                            tp += 1
                        else:
                            fp += 1; fn += 1
                    elif gold_in:
                        fn += 1
                    elif pr_in:
                        fp += 1
    p = tp / (tp + fp) if tp + fp else 0.0
    r = tp / (tp + fn) if tp + fn else 0.0
    f1 = 2 * p * r / (p + r) if p + r else 0.0
    return {"p": p, "r": r, "f1": f1, "tp": tp, "fp": fp, "fn": fn}


def main():
    import argparse
    ap = argparse.ArgumentParser()
    ap.add_argument("--data", nargs="+", default=["/tmp/train_data.json"])
    ap.add_argument("--epochs", type=int, default=60)
    ap.add_argument("--bs", type=int, default=8)
    ap.add_argument("--lr", type=float, default=3e-3)
    ap.add_argument("--d-model", type=int, default=128)
    ap.add_argument("--layers", type=int, default=4)
    ap.add_argument("--heads", type=int, default=4)
    ap.add_argument("--seed", type=int, default=42)
    ap.add_argument("--holdout", type=float, default=0.3)
    args = ap.parse_args()

    random.seed(args.seed)
    torch.manual_seed(args.seed)
    torch.set_num_threads(12)

    samples = load_labeled(args.data)
    print(f"原始样本 {len(samples)}")
    vocab = build_vocab(samples)
    data, reasons = build_dataset(samples, vocab)
    print(f"对齐成功 {len(data)}，词表 {len(vocab)}")
    for k, v in reasons.most_common():
        print(f"   丢弃 {k}: {v}")
    if not data:
        print("没有可用样本")
        return

    random.shuffle(data)
    n_hold = max(1, int(len(data) * args.holdout))
    hold, train = data[:n_hold], data[n_hold:]
    print(f"训练 {len(train)}  留出 {len(hold)}")

    model = CharTagger(len(vocab), args.d_model, args.layers, args.heads)
    print(f"参数量 {count_params(model):,}")
    opt = torch.optim.AdamW(model.parameters(), lr=args.lr, weight_decay=0.01)
    sched = torch.optim.lr_scheduler.OneCycleLR(
        opt, max_lr=args.lr, total_steps=args.epochs * max(1, len(train) // args.bs))

    best = 0.0
    for ep in range(args.epochs):
        model.train()
        tot = n = 0
        for ids, lab, mask in batches(train, args.bs, True):
            logits = model(ids, mask)
            loss = F.cross_entropy(logits.reshape(-1, N_LABELS), lab.reshape(-1),
                                   ignore_index=L2I["<pad>"])
            opt.zero_grad()
            loss.backward()
            torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
            opt.step()
            sched.step()
            tot += loss.item() * ids.size(0); n += ids.size(0)
        if (ep + 1) % 10 == 0 or ep == args.epochs - 1:
            m = evaluate(model, hold)
            print(f"  ep{ep+1:3d} loss {tot/max(n,1):.4f}  "
                  f"留出 P {m['p']:.3f} R {m['r']:.3f} F1 {m['f1']:.3f}")
            if m["f1"] > best:
                best = m["f1"]
                torch.save({"state": model.state_dict(), "vocab": vocab,
                            "args": vars(args)}, "/tmp/nnet/best.pt")
    print(f"最佳留出 F1 {best:.3f}  →  /tmp/nnet/best.pt")

    # ★ 判据：留出集 F1 必须 > 0（学到了东西）
    # 词法基线是字段级 25%（见 distill 的量化实验），token 级 F1 的量级不同，
    # 但「是否过拟合到全 O」是可以直接判的。
    if best <= 0.0:
        print("★ 留出 F1 为 0 —— 模型只学到了 O，需要更多数据或更小模型")
    else:
        print("★ 留出 F1 > 0，模型学到了东西")


if __name__ == "__main__":
    main()
