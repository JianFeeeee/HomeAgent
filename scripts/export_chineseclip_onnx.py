#!/usr/bin/env python3
"""把 Chinese-CLIP ViT-B/16 导出成 HomeAgent 的规范 ONNX 产物。

为什么是 Chinese-CLIP：
  text+image 的默认向量空间要同时满足「小、可商用、中文原生」。
  Chinese-CLIP ViT-B/16 = 188M 参数 / 721MB ONNX / 实测常驻 1.15GB，
  许可是 Apache-2.0（可随发行版分发），且原生在 2 亿中文图文对上训练。
  对比：jina-v5-omni-nano 2.23GB 但 CC BY-NC（不可商用）；Qwen3-VL-Emb-2B
  9.4GB（质量最好，保留为可选 provider）。

产物（--out 目录，会被清空重建）：
  TextEncoder.onnx     input_ids[·,52] + attention_mask[·,52] → text_features[·,512]
  VisionEncoder.onnx   pixel_values[·,3,224,224]              → image_features[·,512]
  embed_config.json    维度/预处理/分词超参/文件名（provider 侧的唯一权威）
  vocab.txt            分词器词表（来自官方模型目录）
  reference.json       冻结参考：逐文本 token id + 逐样本参考向量（Go 侧回归用）
  SHA256SUMS

自检纪律（对齐 export_qwen3vl_embedding_onnx.py）：
  1. 每个用例都真跑一次 ONNX 并与 PyTorch 对比，打印逐用例 cos；
  2. 计划用例集合与实际执行集合必须相等，否则非零退出（防「先跳过再校验」的假通过）；
  3. 不采信退出码，失败一律非零退出并说明原因。

用法：
  scripts/export_chineseclip_onnx.py --out DIR [--model-dir DIR|--model-id REPO]
                                     [--verify-only] [--no-reference] [--skip-verify]
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import sys
import time

import numpy as np

DEFAULT_MODEL_ID = "OFA-Sys/chinese-clip-vit-base-patch16"
TEXT_FILE = "TextEncoder.onnx"
VISION_FILE = "VisionEncoder.onnx"
CONFIG_FILE = "embed_config.json"
REFERENCE_FILE = "reference.json"
OPSET = 17
MAX_LENGTH = 52
IMAGE_SIZE = 224

# 冻结用例：文本覆盖纯中文/中英混/长文本/标点，图像覆盖纯色与渐变。
FIXTURE_TEXTS = [
    "一张红色方块的图片",
    "一只猫在草地上",
    "蓝色的天空",
    "HomeAgent 是一个本地 AI 管家",
    "这是一段比较长的中文文本，用来验证分词器在超过五十个 token 时的截断行为是否正确，"
    "同时检查标点符号、数字 12345 和英文单词 embedding 的处理。",
]
FIXTURE_IMAGES = [
    ("red", (220, 30, 30)),
    ("green", (60, 120, 60)),
    ("blue", (30, 30, 220)),
    ("gray", (128, 128, 128)),
]


def solid(color: tuple[int, int, int]) -> np.ndarray:
    from PIL import Image

    img = Image.new("RGB", (320, 320), color)
    return np.asarray(img, dtype=np.uint8)


def gradient() -> np.ndarray:
    """确定性的横向渐变，避免只有纯色导致区分度不足。"""
    row = np.linspace(0, 255, 320, dtype=np.uint8)
    img = np.zeros((320, 320, 3), dtype=np.uint8)
    img[:, :, 0] = row[None, :]
    img[:, :, 1] = row[:, None]
    img[:, :, 2] = 64
    return img


def norm_cos(a: np.ndarray, b: np.ndarray) -> float:
    a = a.reshape(-1).astype(np.float64)
    b = b.reshape(-1).astype(np.float64)
    na, nb = np.linalg.norm(a), np.linalg.norm(b)
    if na == 0 or nb == 0:
        return 0.0
    return float(np.dot(a, b) / (na * nb))


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def load_model(model_dir: str | None, model_id: str, local_only: bool):
    import torch
    from transformers import ChineseCLIPModel, ChineseCLIPProcessor

    src = model_dir or model_id
    kw = {"local_files_only": True} if local_only else {}
    print(f"[load] {src}")
    t0 = time.time()
    processor = ChineseCLIPProcessor.from_pretrained(src, **kw)
    model = ChineseCLIPModel.from_pretrained(src, **kw).eval()
    print(f"[load] 用时 {time.time() - t0:.1f}s")
    return model, processor


def tower_forward_text(model, input_ids, attention_mask):
    import torch

    with torch.inference_mode():
        out = model.text_model(input_ids=input_ids, attention_mask=attention_mask)
        pooled = out.pooler_output
        if pooled is None:
            pooled = out.last_hidden_state[:, 0]
        return model.text_projection(pooled)


def tower_forward_vision(model, pixel_values):
    import torch

    with torch.inference_mode():
        out = model.vision_model(pixel_values=pixel_values)
        pooled = out.pooler_output
        if pooled is None:
            pooled = out.last_hidden_state[:, 0]
        return model.visual_projection(pooled)


class TextTowerWrapper:
    """torch.onnx.export 需要 nn.Module，这里在函数内构造以避免顶层 import torch。"""


def make_wrappers(model):
    import torch

    class TextTower(torch.nn.Module):
        def __init__(self, m):
            super().__init__()
            self.m = m

        def forward(self, input_ids, attention_mask):
            out = self.m.text_model(input_ids=input_ids, attention_mask=attention_mask)
            pooled = out.pooler_output
            if pooled is None:
                pooled = out.last_hidden_state[:, 0]
            return self.m.text_projection(pooled)

    class VisionTower(torch.nn.Module):
        def __init__(self, m):
            super().__init__()
            self.m = m

        def forward(self, pixel_values):
            out = self.m.vision_model(pixel_values=pixel_values)
            pooled = out.pooler_output
            if pooled is None:
                pooled = out.last_hidden_state[:, 0]
            return self.m.visual_projection(pooled)

    return TextTower(model).eval(), VisionTower(model).eval()


def export_onnx(model, processor, out_dir: str) -> None:
    import torch

    text_tower, vision_tower = make_wrappers(model)
    tok = processor.tokenizer

    enc = tok(["占位"], padding="max_length", truncation=True,
              max_length=MAX_LENGTH, return_tensors="pt")
    pixel = torch.zeros(1, 3, IMAGE_SIZE, IMAGE_SIZE, dtype=torch.float32)

    print(f"[export] {TEXT_FILE}")
    torch.onnx.export(
        text_tower,
        (enc["input_ids"], enc["attention_mask"]),
        os.path.join(out_dir, TEXT_FILE),
        input_names=["input_ids", "attention_mask"],
        output_names=["text_features"],
        dynamic_axes={"input_ids": {0: "batch"}, "attention_mask": {0: "batch"},
                      "text_features": {0: "batch"}},
        opset_version=OPSET,
        do_constant_folding=True,
        dynamo=False,
    )
    print(f"[export] {VISION_FILE}")
    torch.onnx.export(
        vision_tower,
        (pixel,),
        os.path.join(out_dir, VISION_FILE),
        input_names=["pixel_values"],
        output_names=["image_features"],
        dynamic_axes={"pixel_values": {0: "batch"}, "image_features": {0: "batch"}},
        opset_version=OPSET,
        do_constant_folding=True,
        dynamo=False,
    )


def preprocess_images(processor, images: list[np.ndarray]):
    """用官方 processor 做图像预处理，得到与 PyTorch 完全一致的像素张量。"""
    from PIL import Image

    pil = [Image.fromarray(a) for a in images]
    enc = processor(images=pil, return_tensors="pt")
    return enc["pixel_values"]


def run_verification(model, processor, out_dir: str, plan: list[str]) -> dict:
    """逐个用例真跑 ONNX 并与 PyTorch 比对；返回参考数据。"""
    import onnxruntime as ort

    tok = processor.tokenizer
    text_sess = ort.InferenceSession(os.path.join(out_dir, TEXT_FILE),
                                     providers=["CPUExecutionProvider"])
    vision_sess = ort.InferenceSession(os.path.join(out_dir, VISION_FILE),
                                       providers=["CPUExecutionProvider"])

    executed: list[str] = []
    reference: dict = {"texts": [], "images": []}

    print("\n[verify] 文本塔")
    for text in FIXTURE_TEXTS:
        enc = tok([text], padding="max_length", truncation=True,
                  max_length=MAX_LENGTH, return_tensors="pt")
        ids = enc["input_ids"].numpy().astype(np.int64)
        mask = enc["attention_mask"].numpy().astype(np.int64)
        pt = tower_forward_text(model, enc["input_ids"], enc["attention_mask"]).numpy()
        ox = text_sess.run(["text_features"], {"input_ids": ids, "attention_mask": mask})[0]
        cos = norm_cos(pt, ox)
        name = f"text:{text[:24]}"
        executed.append(name)
        print(f"  cos={cos:.9f}  ids[:8]={ids[0][:8].tolist()}  {text[:28]}")
        if cos < 0.9999:
            raise SystemExit(f"文本塔导出不一致: {name} cos={cos}")
        reference["texts"].append({"text": text, "input_ids": ids[0].tolist(),
                                   "attention_mask": mask[0].tolist(),
                                   "vector": [float(v) for v in ox.reshape(-1)]})

    print("\n[verify] 视觉塔")
    images = [solid(c) for _, c in FIXTURE_IMAGES] + [gradient()]
    names = [n for n, _ in FIXTURE_IMAGES] + ["gradient"]
    pixel = preprocess_images(processor, images)
    import torch

    for i, (nm, _) in enumerate(zip(names, images)):
        px = pixel[i:i + 1]
        pt = tower_forward_vision(model, px).numpy()
        ox = vision_sess.run(["image_features"],
                             {"pixel_values": px.numpy().astype(np.float32)})[0]
        cos = norm_cos(pt, ox)
        executed.append(f"image:{nm}")
        print(f"  cos={cos:.9f}  {nm}")
        if cos < 0.9999:
            raise SystemExit(f"视觉塔导出不一致: {nm} cos={cos}")
        # 参考向量直接存像素张量的 sha256，Go 侧用同一预处理即可复算
        reference["images"].append({
            "name": nm,
            "pixels_sha256": hashlib.sha256(px.numpy().astype(np.float32).tobytes()).hexdigest(),
            "vector": [float(v) for v in ox.reshape(-1)],
        })

    missing = [c for c in plan if c not in executed]
    extra = [c for c in executed if c not in plan]
    if missing or extra:
        raise SystemExit(f"用例覆盖不一致: 缺 {missing} 多 {extra}")
    print(f"\n[verify] 覆盖度 OK（{len(executed)} 个用例，计划 {len(plan)}）")
    return reference


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--model-dir", default="")
    ap.add_argument("--model-id", default=DEFAULT_MODEL_ID)
    ap.add_argument("--verify-only", action="store_true")
    ap.add_argument("--no-reference", action="store_true")
    ap.add_argument("--skip-verify", action="store_true")
    args = ap.parse_args()

    plan = [f"text:{t[:24]}" for t in FIXTURE_TEXTS] + \
           [f"image:{n}" for n, _ in FIXTURE_IMAGES] + ["image:gradient"]

    if not args.verify_only:
        # 清空重建，避免旧产物被当成这次的成果
        if os.path.isdir(args.out):
            shutil.rmtree(args.out)
        os.makedirs(args.out, exist_ok=True)

    model, processor = load_model(args.model_dir or None, args.model_id,
                                  local_only=bool(args.model_dir))
    if not os.path.isdir(args.out):
        os.makedirs(args.out, exist_ok=True)

    if not args.verify_only:
        export_onnx(model, processor, args.out)

    # 词表随产物一起放：provider 只依赖这个目录
    src_vocab = os.path.join(args.model_dir, "vocab.txt") if args.model_dir else None
    if src_vocab and os.path.exists(src_vocab):
        shutil.copy2(src_vocab, os.path.join(args.out, "vocab.txt"))

    reference = None
    if not args.skip_verify:
        reference = run_verification(model, processor, args.out, plan)
    else:
        print("[verify] 已按 --skip-verify 跳过（不据此宣布成功）")

    if not args.verify_only:
        cfg = {
            "arch": "chinese-clip-vit-base-patch16",
            "dim": 512,
            "text_onnx": TEXT_FILE,
            "vision_onnx": VISION_FILE,
            "max_length": MAX_LENGTH,
            "image_size": IMAGE_SIZE,
            "resample": "bicubic",
            "rescale": 1.0 / 255.0,
            "image_mean": [0.48145466, 0.4578275, 0.40821073],
            "image_std": [0.26862954, 0.26130258, 0.27577711],
            "normalize_vector": True,   # provider 必须 L2 归一化后再入库
            "tokenizer": {
                "type": "bert-wordpiece",
                "vocab": "vocab.txt",
                "do_lower_case": True,
                "tokenize_chinese_chars": True,
                "cls_id": 101, "sep_id": 102, "pad_id": 0, "unk_id": 100,
            },
            "modalities": ["text", "image"],
            "unsupported_modalities": ["audio", "video"],
            "notes": "Chinese-CLIP ViT-B/16：视觉 ViT-B/16 + 文本 RoBERTa-wwm-base，"
                     "输出 512 维共享空间。文本塔取 CLS（pooler）后过 text_projection，"
                     "视觉塔取 CLS 后过 visual_projection；两者均未在图中归一化，"
                     "归一化由 provider 负责。",
        }
        with open(os.path.join(args.out, CONFIG_FILE), "w") as f:
            json.dump(cfg, f, ensure_ascii=False, indent=2)

        if reference is not None and not args.no_reference:
            reference["source"] = {"model_id": args.model_id,
                                   "model_dir": args.model_dir or "(hub)"}
            reference["artifacts"] = {n: sha256_file(os.path.join(args.out, n))
                                      for n in (TEXT_FILE, VISION_FILE)}
            with open(os.path.join(args.out, REFERENCE_FILE), "w") as f:
                json.dump(reference, f, ensure_ascii=False, indent=2)

        with open(os.path.join(args.out, "SHA256SUMS"), "w") as f:
            for n in sorted(os.listdir(args.out)):
                if n == "SHA256SUMS":
                    continue
                p = os.path.join(args.out, n)
                if os.path.isfile(p):
                    f.write(f"{sha256_file(p)}  {n}\n")

    print(f"\n[out] {args.out}")
    for n in sorted(os.listdir(args.out)):
        p = os.path.join(args.out, n)
        if os.path.isfile(p):
            print(f"  {n:<20} {os.path.getsize(p) / 1e6:9.1f} MB")
    return 0


if __name__ == "__main__":
    sys.exit(main())
