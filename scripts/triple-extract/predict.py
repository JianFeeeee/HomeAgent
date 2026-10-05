#!/usr/bin/env python3
"""三元组抽取的推理侧：神经网络 + 规则兜底 → BlockPayload 形态。

★ 为什么必须混合而不是纯网络
  --------------------------
  实测（probe_attr_name.py 的四道判据）：

      属性名不在原句的 229 条标注里，只有 92 条（40%）的属性名
      能从「值形态」学出来；「文本」一个形态就对应 42 种属性名。

  所以纯网络必然漏掉一半。而那一半恰恰是**主语派生**的
  （批次号=「第N批」里的 N、版本=v2.31.5），`DeriveSubjects` 规则
  已经能推出主语并从主语结构取值 —— 那部分不需要神经网络。

  分工：
      网络   负责「句子里表面就有属性名与值」的 82%
      规则   负责「值藏在主语结构里」的 18%
      校验   两边共用同一道闸门（值必须原样出现在原句）

★ 确定性
  --------
  同输入连跑 N 次输出必须完全一致 —— 这是相对 LLM 路径的核心收益
  （LLM 路径实测同批 146 条两次跑出 259 vs 362 个字段块）。
  训练后推理是逐位确定的，只要 checkpoint 与词表不变。
"""
from __future__ import annotations

import json
import os
import re
import sys
from dataclasses import dataclass

import torch

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from train_tagger import (  # noqa: E402
    CharTagger, L2I, LABELS, Example, align, find_dim_in, normalize_name,
)


# ─────────────────────────────────────────────────────────────
# 模型加载
# ─────────────────────────────────────────────────────────────

@dataclass
class Tagger:
    model: CharTagger
    vocab: dict
    str_to_id: dict

    @classmethod
    def load(cls, path: str) -> "Tagger":
        ck = torch.load(path, map_location="cpu", weights_only=False)
        a = ck["args"]
        vocab = ck["vocab"]
        model = CharTagger(len(vocab), a["d_model"], a["layers"], a["heads"])
        model.load_state_dict(ck["state"])
        model.eval()
        return cls(model, vocab, {v: k for k, v in vocab.items()})

    def predict(self, sentence: str) -> list[tuple[str, int, int]]:
        """返回 [(标签, 起, 止)]，逐字符。"""
        ids = [self.vocab.get("<bos>")] + [
            self.vocab.get(c, self.vocab["<unk>"]) for c in sentence]
        t = torch.tensor([ids])
        mask = torch.zeros(1, len(ids), dtype=torch.bool)
        with torch.no_grad():
            pred = self.model(t, mask).argmax(-1)[0].tolist()
        out = []
        for i, p in enumerate(pred[1:]):      # 跳过 <bos>
            if i < len(sentence):
                out.append((LABELS[p], i, i + 1))
        return out


# ─────────────────────────────────────────────────────────────
# 解码：BIO 序列 → (维度, 值) 对
# ─────────────────────────────────────────────────────────────

def decode_spans(preds: list[tuple[str, int, int]], sentence: str,
                 want: str) -> list[tuple[str, int, int]]:
    """按 B-xxx/I-xxx 拼出连续片段（want 是 "DIM" 或 "VAL"）。"""
    spans, cur = [], []
    for lab, s, e in preds:
        if lab == f"B-{want}":
            if cur:
                spans.append(cur)
            cur = [(s, e, lab)]
        elif lab == f"I-{want}" and cur:
            cur.append((s, e, lab))
        else:
            if cur:
                spans.append(cur)
            cur = []
    if cur:
        spans.append(cur)
    return spans


def predict_fields(tagger: Tagger, sentence: str) -> list[dict]:
    """网络预测的字段列表。

    ★ 关键：维度名取**句子表层的字面**（find_dim_in 的反向 ——
      网络标的是「哪个 span 是属性名」，那 span 本身就是名字），
      归一交给 normalize_name。
    """
    preds = tagger.predict(sentence)
    dim_spans = decode_spans(preds, sentence, "DIM")
    val_spans = decode_spans(preds, sentence, "VAL")
    if not dim_spans or not val_spans:
        return []
    # 按出现顺序配对（第 k 个属性名 ↔ 第 k 个值）
    pairs = []
    for ds, vs in zip(dim_spans, val_spans):
        dim = sentence[ds[0][0]:ds[-1][1]]
        val = sentence[vs[0][0]:vs[-1][1]]
        if dim and val:
            pairs.append({"name": normalize_name(dim), "value": val,
                          "dim_pos": ds[0][0], "val_pos": vs[0][0]})
    return pairs


# ─────────────────────────────────────────────────────────────
# 规则兜底：主语派生的维度
# ─────────────────────────────────────────────────────────────

DERIVED_RULES = [
    # (正则, 维度名, 主语组名)
    (re.compile(r"^第\s*(\d+)\s*批"), "批次号", "第N批"),
    (re.compile(r"(v\d+(?:\.\d+)*)"), "版本", "第N批"),
    (re.compile(r"(凌晨|上午|下午|晚上|中午)?\d{1,2}[点:：]\d{0,2}"), "发布窗口", "第N批"),
]


def rule_derive(sentence: str) -> list[dict]:
    """从主语结构派生维度（网络学不了的那 18%）。"""
    out = []
    m = re.search(r"^第\s*(\d+)\s*批", sentence)
    if m:
        out.append({"name": "批次号", "value": m.group(1), "from": "主语"})
    mv = re.search(r"(v\d+(?:\.\d+)*)", sentence)
    if mv:
        out.append({"name": "版本", "value": mv.group(1), "from": "主语"})
    mt = re.search(r"(凌晨|上午|下午|晚上|中午)?\d{1,2}[点:：]\d{0,2}", sentence)
    if mt:
        out.append({"name": "发布窗口", "value": mt.group(0), "from": "主语"})
    return out


# ─────────────────────────────────────────────────────────────
# 幻觉闸门：与 LLM 路径同一道
# ─────────────────────────────────────────────────────────────

def gate(fields: list[dict], sentence: str) -> tuple[list[dict], list[dict]]:
    """返回 (通过, 被拒)。

    ★ 闸门与 distill.Split 完全一致 —— 网络不比 LLM 宽松：
      1. 值必须原样出现在原句
      2. 值长度 ≤ 句子的 60%（拦「值=整句」）
      3. 属性名必须能在原句里对上（LLM 造的维度名一律拒）
    """
    ok, bad = [], []
    for f in fields:
        v = f.get("value", "")
        if not v or v not in sentence:
            bad.append((f, "值不在原句")); continue
        if len(v) > 0.6 * len(sentence):
            bad.append((f, "值占句子过半")); continue
        name = f.get("name", "")
        if f.get("from") != "主语":
            if not find_dim_in(name, sentence):
                bad.append((f, "属性名对不上原句")); continue
        ok.append(f)
    return ok, bad


# ─────────────────────────────────────────────────────────────
# 对外入口：产出 BlockPayload 形态
# ─────────────────────────────────────────────────────────────

def extract(tagger: Tagger, sentence: str) -> dict:
    """抽取一个句子的三元组字段。

    返回 {"sentence", "fields":[{subject,dimension,value}], "stats": {...}}
    —— fields 的形态与 distill.BlockPayload.Fields 完全一致，
    可直接交给 WritePayload。
    """
    net = predict_fields(tagger, sentence)
    rule = rule_derive(sentence)

    # 去重：网络与规则可能同时产出「批次号」
    seen, merged = set(), []
    for f in net + rule:
        key = (f["name"], f["value"])
        if key in seen:
            continue
        seen.add(key)
        merged.append(f)

    passed, rejected = gate(merged, sentence)

    # 主语：从句首的「第N批」或受控别名取（与 DeriveSubjects 同思路）
    subject = ""
    m = re.match(r"^(第\d+批)", sentence)
    if m:
        subject = m.group(1)

    fields = [{"subject": subject, "dimension": f["name"], "value": f["value"]}
              for f in passed]
    return {
        "sentence": sentence,
        "fields": fields,
        "stats": {"net": len(net), "rule": len(rule),
                  "passed": len(passed), "rejected": len(rejected)},
        "rejected_detail": [{"field": f, "reason": r} for f, r in rejected],
    }


# ─────────────────────────────────────────────────────────────
# CLI
# ─────────────────────────────────────────────────────────────

def main():
    import argparse
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="/tmp/nnet/best.pt")
    ap.add_argument("--sentence", default="")
    ap.add_argument("--file", default="", help="每行一句的输入文件")
    args = ap.parse_args()

    if not os.path.exists(args.model):
        print(f"模型不存在 {args.model}（先跑 train_tagger.py）")
        return

    tagger = Tagger.load(args.model)
    print(f"模型已加载：{args.model}\n")

    if args.sentence:
        r = extract(tagger, args.sentence)
        print(f"句子: {args.sentence}")
        print(f"统计: {r['stats']}")
        for f in r["fields"]:
            print(f"  {f['subject']}|{f['dimension']}={f['value']}")
        for d in r["rejected_detail"]:
            print(f"  ✘ {d['field']}  {d['reason']}")
        return

    if args.file:
        for i, line in enumerate(open(args.file)):
            s = line.strip()
            if not s:
                continue
            r = extract(tagger, s)
            fs = "  ".join(f"{f['dimension']}={f['value']}" for f in r["fields"])
            print(f"[{i}] {s[:44]} → {fs or '(空)'}")


if __name__ == "__main__":
    main()