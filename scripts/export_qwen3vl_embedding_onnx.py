#!/usr/bin/env python3
"""自动拉取 Qwen3-VL-Embedding-2B 并导出 HomeAgent 用的三段式 ONNX 统一向量空间。

产物（写入 --out 目录，约 8GB）::

    TokenEmbedding.onnx   input_ids                -> hidden
    Transformer.onnx      hidden + deepstack + RoPE + causal mask -> embedding
    Vision.onnx(+.data)   pixel_values             -> 3 层 DeepStack + 主视觉特征
    tokenizer.json / tokenizer_config.json / chat_template.jinja
    embed_config.json     Go 侧读取的布局与契约常量

为什么是「三段」而不是一张图
--------------------------
文本与图像共用同一 token embedding、同一 28 层 Transformer、同一 last-token
池化与同一 fingerprint；分段只是部署形式。把 RoPE 与视觉特征散射留在 Go 计算，
是为了避开旧式 tracer 把 seq=598 / visual=576 烘焙进图里——那样签名上写着
dynamic_axes，实际却只能用导出的那个长度运行。

Vision 为什么固定 768×768（不支持原生多帧视频）
--------------------------------------------
Qwen3-VL 的视觉塔把 ``grid_thw`` 当 Python 值消费（``grid_thw.tolist()``），
legacy tracer 会把它的内容固化成常量：实测导出后 ONNX 图里根本没有
``grid_thw`` 输入，用别的帧数调用会直接报 Invalid input name。因此这里把
grid 固定为 (1, 48, 48)，并在导出处做 PyTorch↔ONNX 一致性校验。
视频由上层抽帧后逐帧按图像编码——同一模型、同一维度、同一 fingerprint，
只是不做跨帧时序注意力；音频不在本空间覆盖范围内（见 vector.ErrModalityUnsupported）。

自检是不可省的
------------
导出脚本必须自己证明产物正确，而不是只比较有没有报错：
  1. 分段 PyTorch（TokenEmbedding+Transformer+Vision 的组合）对比完整模型前向；
  2. 再用 onnxruntime 跑导出后的三段图，对比完整模型前向。
两步都要求 cos ≥ 0.999999，否则以非零码退出——绝不产出一个「能加载但算错」的模型。
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import sys
import time

import numpy as np
import torch

INSTRUCTION = "Represent the user's input."
IMAGE_SIZE = 768
PATCH_SIZE = 16
TEMPORAL_PATCH = 2
SPATIAL_MERGE = 2
MAX_LENGTH = 1024  # 768x768 有 576 个视觉 token；512 会截断视觉占位符
DEFAULT_MODEL_ID = "Qwen/Qwen3-VL-Embedding-2B"
REFERENCE_TEXT = "今天天气怎么样"
REFERENCE_IMAGE_RGB = (200, 30, 30)


def log(msg: str) -> None:
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


# ─────────────────────────── 模型获取 ───────────────────────────

def pull_model(model_id: str, store: str) -> str:
    """把模型拉到本地并返回快照目录。优先 HuggingFace，失败回落 ModelScope。

    两个源都必须显式给出目标目录：默认的 HF blobs+snapshots 结构会把权重存成
    符号链接树，导出脚本按固定文件名读取时很容易踩到不存在的路径。
    HF_ENDPOINT 会被 huggingface_hub 自动识别，因此国内镜像（如
    https://hf-mirror.com ）无需额外参数。
    """
    os.makedirs(store, exist_ok=True)
    local = os.path.join(store, model_id.replace("/", "--"))
    marker = os.path.join(local, "config.json")
    if os.path.exists(marker):
        log(f"复用已下载模型: {local}")
        return local

    errors = []
    try:
        from huggingface_hub import snapshot_download

        log(f"从 HuggingFace 拉取 {model_id} -> {local}（HF_ENDPOINT={os.environ.get('HF_ENDPOINT', '默认')}）")
        snapshot_download(repo_id=model_id, local_dir=local, max_workers=4)
        if os.path.exists(marker):
            return local
        errors.append("huggingface: 下载完成但缺少 config.json")
    except Exception as exc:  # noqa: BLE001 - 需要回落到 ModelScope
        errors.append(f"huggingface: {exc}")
        log(f"HuggingFace 拉取失败：{exc}")

    try:
        from modelscope import snapshot_download as ms_snapshot

        log(f"从 ModelScope 拉取 {model_id} -> {local}")
        ms_snapshot(model_id, local_dir=local)
        if os.path.exists(marker):
            return local
        errors.append("modelscope: 下载完成但缺少 config.json")
    except Exception as exc:  # noqa: BLE001
        errors.append(f"modelscope: {exc}")
        log(f"ModelScope 拉取失败：{exc}")

    raise SystemExit("模型拉取失败：\n  - " + "\n  - ".join(errors))


# ─────────────────────────── 模型分段 ───────────────────────────

class TokenEmbedding(torch.nn.Module):
    def __init__(self, embed_tokens):
        super().__init__()
        self.embed_tokens = embed_tokens

    def forward(self, input_ids):
        return self.embed_tokens(input_ids)


class Transformer(torch.nn.Module):
    """28 层语言模型 + 前置 3 层 DeepStack 相加 + final norm + last-token 池化。

    池化放在图里（而不是 Go）是有意的：last-token 的位置由 attention_mask 决定，
    一旦 Go 侧算错位置就会静默取到 padding 的 hidden，而向量照样归一化、照样
    能比余弦——那种错误只能靠与参考向量对比才能发现。
    """

    def __init__(self, lm):
        super().__init__()
        self.layers = lm.layers
        self.norm = lm.norm

    def forward(self, hidden, deepstack_0, deepstack_1, deepstack_2,
                rotary_cos, rotary_sin, causal_mask):
        deep = (deepstack_0, deepstack_1, deepstack_2)
        for i, layer in enumerate(self.layers):
            hidden = layer(
                hidden_states=hidden,
                attention_mask=causal_mask,
                position_embeddings=(rotary_cos, rotary_sin),
                use_cache=False,
            )
            if i < 3:
                hidden = hidden + deep[i]
        return self.norm(hidden)[:, -1]


class VisionTower(torch.nn.Module):
    """视觉塔：固定 grid 的 patch 张量 -> 主视觉特征 + 3 层 DeepStack。

    grid_thw 作为 buffer 固化。原因见模块 docstring：legacy tracer 无法把
    grid_thw 保留为运行时输入，写成输入只会得到一个实际不含该输入的图。
    """

    def __init__(self, visual, grid_thw):
        super().__init__()
        self.visual = visual
        self.register_buffer("grid_thw", grid_thw)

    def forward(self, pixel_values):
        out = self.visual(pixel_values, grid_thw=self.grid_thw, return_dict=True)
        d = out.deepstack_features
        return d[0], d[1], d[2], out.pooler_output


# ─────────────────────────── 输入构造 ───────────────────────────

def render_text(instruction: str, text: str) -> str:
    """与 Go 侧 tokenizer.renderInstructionInput 逐字符一致。

    指令放 system、正文放 user、以 assistant 起始符结尾。差一个特殊 token，
    last-token 池化取到的位置就变了，嵌入也就不同——而且不会报错。
    """
    return (f"<|im_start|>system\n{instruction}<|im_end|>\n"
            f"<|im_start|>user\n{text}<|im_end|>\n<|im_start|>assistant\n")


def text_position_ids(attention_mask):
    """无 padding 的单批语义下，三个 RoPE 轴都等于累计可见位置。"""
    pos = attention_mask.long().cumsum(-1) - 1
    return pos.clamp(min=0).unsqueeze(0).expand(3, -1, -1)


def text_inputs(processor, lm, text):
    rendered = render_text(INSTRUCTION, text)
    x = processor.tokenizer([rendered], return_tensors="pt", truncation=True,
                            max_length=MAX_LENGTH, padding=True)
    pos = text_position_ids(x["attention_mask"])
    hidden = lm.embed_tokens(x["input_ids"])
    cos, sin = lm.rotary_emb(hidden, pos)
    zero = torch.zeros_like(hidden)
    return x, hidden, (zero, zero, zero), cos, sin, causal_mask(hidden.shape[1]), pos


def image_inputs(processor, model, image):
    conv = [{"role": "system", "content": [{"type": "text", "text": INSTRUCTION}]},
            {"role": "user", "content": [{"type": "image", "image": image}]}]
    rendered = processor.apply_chat_template([conv], add_generation_prompt=True, tokenize=False)
    x = processor(text=rendered, images=[image], do_resize=False,
                  return_tensors="pt", truncation=True, max_length=MAX_LENGTH)
    lm = model.model.language_model
    with torch.no_grad():
        vo = model.model.visual(x["pixel_values"], grid_thw=x["image_grid_thw"], return_dict=True)
    pos, _ = model.model.get_rope_index(
        x["input_ids"], x["mm_token_type_ids"],
        image_grid_thw=x["image_grid_thw"], attention_mask=x["attention_mask"])
    mask = x["mm_token_type_ids"] == 1
    hidden = lm.embed_tokens(x["input_ids"])
    hidden = hidden.clone()
    hidden[mask] = vo.pooler_output
    deep = []
    for d in vo.deepstack_features:
        full = torch.zeros_like(hidden)
        full[mask] = d
        deep.append(full)
    cos, sin = lm.rotary_emb(hidden, pos)
    return x, hidden, tuple(deep), cos, sin, causal_mask(hidden.shape[1]), pos


def causal_mask(seq: int) -> torch.Tensor:
    m = torch.full((1, 1, seq, seq), torch.finfo(torch.float32).min)
    return torch.triu(m, diagonal=1)


def pool_last(hidden, mask):
    last = mask.shape[1] - mask.flip(1).argmax(1) - 1
    return hidden[torch.arange(hidden.shape[0], device=hidden.device), last]


# ─────────────────────────── 校验 ───────────────────────────

def compare(name: str, a, b, floor: float = 0.999999) -> float:
    a = torch.nn.functional.normalize(torch.as_tensor(a).float(), dim=-1)
    b = torch.nn.functional.normalize(torch.as_tensor(b).float(), dim=-1)
    cos = float((a * b).sum())
    md = float((a - b).abs().max())
    log(f"  {name}: cos={cos:.9f} maxdiff={md:.3e}")
    if cos < floor:
        raise SystemExit(f"导出校验失败：{name} 与完整模型不等价 (cos={cos:.9f})")
    return cos


def verify_split_torch(model, processor, transformer) -> None:
    log("校验①：分段 PyTorch vs 完整模型")
    x, h, d, cos, sin, cm, pos = text_inputs(processor, model.model.language_model, REFERENCE_TEXT)
    with torch.no_grad():
        got = transformer(h, *d, cos, sin, cm)
        ref = model.model(input_ids=x["input_ids"], attention_mask=x["attention_mask"],
                          position_ids=pos, use_cache=False).last_hidden_state[:, -1]
    compare("text/split-torch", got, ref)

    img = reference_image()
    x, h, d, cos, sin, cm, _ = image_inputs(processor, model, img)
    with torch.no_grad():
        got = transformer(h, *d, cos, sin, cm)
        ref = model.model(input_ids=x["input_ids"], attention_mask=x["attention_mask"],
                          pixel_values=x["pixel_values"], image_grid_thw=x["image_grid_thw"],
                          mm_token_type_ids=x["mm_token_type_ids"],
                          use_cache=False).last_hidden_state[:, -1]
    compare("image/split-torch", got, ref)
    log(f"  形状: seq={h.shape[1]} visual={(x['mm_token_type_ids'] == 1).sum().item()}")


def unit(vec):
    """L2 归一化。

    必须对**写出的参考向量**归一化：ONNX 图返回的是 final norm 之后的原始 last hidden，
    而 Go 侧的 public 接口返回的是归一化后的向量。若参考用原始值，Go 测试会全线不匹配——
    且这个差异看起来像“模型不对”，实际上只是两边对“向量”的定义不同。
    """
    v = np.asarray(vec, dtype=np.float64)
    n = float(np.linalg.norm(v))
    return v / n if n > 0 else v


def verify_onnx(out_dir: str, model, processor) -> dict:
    """用 onnxruntime 跑导出后的三段图，对比完整模型前向。

    返回参考向量（供 Go 侧测试冻结使用）：Go 必须复现同一套预处理与模板，
    因此这里把同一输入下的期望向量前若干维导出。
    """
    import onnxruntime as ort

    log("校验②：导出后的 ONNX 三段图 vs 完整模型")
    ts = ort.InferenceSession(os.path.join(out_dir, "TokenEmbedding.onnx"), providers=["CPUExecutionProvider"])
    xs = ort.InferenceSession(os.path.join(out_dir, "Transformer.onnx"), providers=["CPUExecutionProvider"])
    vs = ort.InferenceSession(os.path.join(out_dir, "Vision.onnx"), providers=["CPUExecutionProvider"])
    lm = model.model.language_model

    reference: dict[str, object] = {}

    def run_transform(hidden, deep, cos, sin):
        seq = hidden.shape[1]
        return xs.run(None, {
            "hidden": hidden.astype(np.float32),
            "deepstack_0": deep[0].astype(np.float32),
            "deepstack_1": deep[1].astype(np.float32),
            "deepstack_2": deep[2].astype(np.float32),
            "rotary_cos": cos.astype(np.float32),
            "rotary_sin": sin.astype(np.float32),
            "causal_mask": causal_mask(seq).numpy(),
        })[0]

    x, h, d, cos, sin, _cm, pos = text_inputs(processor, lm, REFERENCE_TEXT)
    with torch.no_grad():
        ref_text = model.model(input_ids=x["input_ids"], attention_mask=x["attention_mask"],
                               position_ids=pos, use_cache=False).last_hidden_state[:, -1].numpy()
    h_onnx = ts.run(None, {"input_ids": x["input_ids"].numpy().astype(np.int64)})[0]
    zero = np.zeros_like(h_onnx)
    got = run_transform(h_onnx, (zero, zero, zero), cos.numpy(), sin.numpy())
    compare("text/onnx-vs-full", got, ref_text)
    reference["text"] = REFERENCE_TEXT
    reference["text_vector_prefix"] = [float(v) for v in unit(got[0])[:12]]
    reference["text_norm_raw"] = float(np.linalg.norm(got[0]))

    img = reference_image()
    x, _h, _d, cos, sin, _cm, _ = image_inputs(processor, model, img)
    with torch.no_grad():
        ref_img = model.model(input_ids=x["input_ids"], attention_mask=x["attention_mask"],
                              pixel_values=x["pixel_values"], image_grid_thw=x["image_grid_thw"],
                              mm_token_type_ids=x["mm_token_type_ids"],
                              use_cache=False).last_hidden_state[:, -1].numpy()
    h_onnx = ts.run(None, {"input_ids": x["input_ids"].numpy().astype(np.int64)})[0]
    vo = vs.run(None, {"pixel_values": x["pixel_values"].numpy().astype(np.float32)})
    mask = x["mm_token_type_ids"].numpy() == 1
    h_onnx = h_onnx.copy()
    h_onnx[mask] = vo[3]
    deep = []
    for d in vo[:3]:
        full = np.zeros_like(h_onnx)
        full[mask] = d
        deep.append(full)
    got = run_transform(h_onnx, tuple(deep), cos.numpy(), sin.numpy())
    compare("image/onnx-vs-full", got, ref_img)
    reference["image_rgb"] = list(REFERENCE_IMAGE_RGB)
    reference["image_size"] = IMAGE_SIZE
    reference["image_vector_prefix"] = [float(v) for v in unit(got[0])[:12]]
    reference["image_norm_raw"] = float(np.linalg.norm(got[0]))
    reference["dim"] = int(got.shape[1])
    return reference


def reference_image():
    from PIL import Image

    return Image.new("RGB", (IMAGE_SIZE, IMAGE_SIZE), REFERENCE_IMAGE_RGB)


# ─────────────────────────── 导出 ───────────────────────────

def export_graphs(out_dir: str, model, processor, model_dir: str, transformer) -> None:
    os.makedirs(out_dir, exist_ok=True)
    # 清掉旧产物，避免 fingerprint 把死文件算进去（旧图/旧外部权重会让
    # 空间指纹变化，触发一次毫无意义的全量重算）。
    for name in os.listdir(out_dir):
        p = os.path.join(out_dir, name)
        if os.path.isfile(p):
            os.remove(p)

    lm = model.model.language_model

    log("导出 TokenEmbedding.onnx")
    ids = torch.tensor([[151643, 151643]], dtype=torch.long)
    with torch.no_grad():
        torch.onnx.export(
            TokenEmbedding(lm.embed_tokens).eval(), (ids,), os.path.join(out_dir, "TokenEmbedding.onnx"),
            input_names=["input_ids"], output_names=["hidden"],
            dynamic_axes={"input_ids": {1: "seq"}, "hidden": {1: "seq"}},
            opset_version=17, do_constant_folding=True, dynamo=False,
        )

    log("导出 Transformer.onnx")
    x, h, d, cos, sin, cm, _ = image_inputs(processor, model, reference_image())
    with torch.no_grad():
        torch.onnx.export(
            transformer, (h, *d, cos, sin, cm), os.path.join(out_dir, "Transformer.onnx"),
            input_names=["hidden", "deepstack_0", "deepstack_1", "deepstack_2",
                         "rotary_cos", "rotary_sin", "causal_mask"],
            output_names=["embedding"],
            dynamic_axes={
                "hidden": {1: "seq"}, "deepstack_0": {1: "seq"}, "deepstack_1": {1: "seq"},
                "deepstack_2": {1: "seq"}, "rotary_cos": {1: "seq"}, "rotary_sin": {1: "seq"},
                "causal_mask": {2: "seq", 3: "seq"},
            },
            opset_version=17, do_constant_folding=True, dynamo=False,
        )

    log("导出 Vision.onnx（固定 grid 1×48×48）")
    grid = torch.tensor([[1, IMAGE_SIZE // PATCH_SIZE, IMAGE_SIZE // PATCH_SIZE]], dtype=torch.long)
    pv = x["pixel_values"]
    with torch.no_grad():
        torch.onnx.export(
            VisionTower(model.model.visual, grid).eval(), (pv,), os.path.join(out_dir, "Vision.onnx"),
            input_names=["pixel_values"],
            output_names=["deepstack_feature_0", "deepstack_feature_1",
                          "deepstack_feature_2", "vision_hidden_states"],
            opset_version=17, do_constant_folding=True, dynamo=False,
        )

    for name in ("tokenizer.json", "tokenizer_config.json", "chat_template.jinja", "added_tokens.json"):
        src = os.path.join(model_dir, name)
        if os.path.exists(src):
            shutil.copy2(src, os.path.join(out_dir, name))


def write_config(out_dir: str, model, processor) -> None:
    cfg = model.config
    text_cfg = getattr(cfg, "text_config", cfg)
    rope_scaling = getattr(text_cfg, "rope_scaling", None) or {}
    mrope_section = rope_scaling.get("mrope_section") or [24, 20, 20]
    vision = cfg.vision_config
    meta = {
        "arch": "qwen3-vl-embedding-2b-multimodal",
        "runtime": "homeagent-onnx-three-part",
        "dim": int(getattr(text_cfg, "hidden_size", 2048)),
        "max_length": MAX_LENGTH,
        "instruction": INSTRUCTION,
        "pooling": "last_token",
        "normalize": True,
        "image_size": IMAGE_SIZE,
        "patch_size": int(vision.patch_size),
        "temporal_patch_size": int(vision.temporal_patch_size),
        "spatial_merge_size": int(vision.spatial_merge_size),
        "image_mean": [0.5, 0.5, 0.5],
        "image_std": [0.5, 0.5, 0.5],
        "rope_theta": float(getattr(text_cfg, "rope_theta", 5000000)),
        "mrope_section": [int(v) for v in mrope_section],
        "num_layers": int(getattr(text_cfg, "num_hidden_layers", 28)),
        "supports_native_video": False,
        "unsupported_modalities": ["audio", "video"],
        "notes": ("视频由上层抽帧后逐帧按图像编码（同模型/同维度/同 fingerprint）；"
                  "音频需未来接入真正的统一音频模型。grid_thw 被 legacy tracer 固化为常量，"
                  "故视觉塔固定 1×48×48，详见导出脚本 docstring。"),
    }
    with open(os.path.join(out_dir, "embed_config.json"), "w") as f:
        json.dump(meta, f, ensure_ascii=False, indent=2)
    log(f"写出 embed_config.json dim={meta['dim']} rope_theta={meta['rope_theta']} "
        f"mrope={meta['mrope_section']}")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", required=True, help="ONNX 产物目录（写入约 8GB）")
    ap.add_argument("--model-dir", default="", help="已下载的模型目录；给出则跳过自动拉取")
    ap.add_argument("--model-id", default=DEFAULT_MODEL_ID, help=f"模型仓库 id（默认 {DEFAULT_MODEL_ID}）")
    ap.add_argument("--model-store", default=os.path.expanduser("~/.cache/homeagent-qwen-models"),
                    help="自动拉取时的模型存放目录")
    ap.add_argument("--no-reference", action="store_true",
                    help="不写 <out>/qwen_reference.json（默认会写；Go 测试靠它做冻结回归）")
    ap.add_argument("--skip-verify", action="store_true", help="跳过导出后校验（仅调试用，不推荐）")
    ap.add_argument("--verify-only", action="store_true",
                    help="不重新导出，只校验已存在的 <out> 并（重新）写出参考向量")
    args = ap.parse_args()

    if args.verify_only:
        # 校验既有产物目录：既能确认线上在用的图没坏，也能给旧目录补上参考向量。
        for name in ("TokenEmbedding.onnx", "Transformer.onnx", "Vision.onnx"):
            if not os.path.exists(os.path.join(args.out, name)):
                raise SystemExit(f"{args.out} 下缺少 {name}，无法 --verify-only")
        model_dir = args.model_dir or pull_model(args.model_id, args.model_store)
        from transformers import AutoProcessor
        from transformers.models.qwen3_vl.modeling_qwen3_vl import Qwen3VLForConditionalGeneration

        log("加载 processor / model（--verify-only）")
        processor = AutoProcessor.from_pretrained(model_dir, trust_remote_code=True, padding_side="right")
        model = Qwen3VLForConditionalGeneration.from_pretrained(
            model_dir, dtype=torch.float32, low_cpu_mem_usage=True).eval()
        reference = verify_onnx(args.out, model, processor)
        if not args.no_reference:
            path = os.path.join(args.out, "qwen_reference.json")
            with open(path, "w") as f:
                json.dump(reference, f, ensure_ascii=False, indent=2)
            log(f"写出冻结参考向量: {path}")
        log(f"校验完成: {args.out}")
        return 0

    if args.model_dir:
        model_dir = args.model_dir
        if not os.path.exists(os.path.join(model_dir, "config.json")):
            raise SystemExit(f"--model-dir {model_dir} 下没有 config.json")
        log(f"使用本地模型: {model_dir}")
    else:
        model_dir = pull_model(args.model_id, args.model_store)

    # transformers 只在真正导出时才需要（拉取模型本身只用 huggingface_hub）。
    from PIL import Image  # noqa: F401  确保依赖存在并给出清晰报错
    from transformers import AutoProcessor
    from transformers.models.qwen3_vl.modeling_qwen3_vl import Qwen3VLForConditionalGeneration

    log("加载 processor / model（FP32，CPU）")
    processor = AutoProcessor.from_pretrained(model_dir, trust_remote_code=True, padding_side="right")
    model = Qwen3VLForConditionalGeneration.from_pretrained(
        model_dir, dtype=torch.float32, low_cpu_mem_usage=True).eval()
    transformer = Transformer(model.model.language_model).eval()

    verify_split_torch(model, processor, transformer)
    export_graphs(args.out, model, processor, model_dir, transformer)
    write_config(args.out, model, processor)

    reference = None if args.skip_verify else verify_onnx(args.out, model, processor)
    # 参考写进产物目录本身：这样任何一个 ONNX 目录都自带「它应当给出什么输出」，
    # Go 测试无需额外配置就能找到，也不会出现「模型换了、参考还是旧的」的错配。
    if reference is not None and not args.no_reference:
        path = os.path.join(args.out, "qwen_reference.json")
        with open(path, "w") as f:
            json.dump(reference, f, ensure_ascii=False, indent=2)
        log(f"写出冻结参考向量: {path}")

    total = sum(os.path.getsize(os.path.join(args.out, n))
                for n in os.listdir(args.out) if os.path.isfile(os.path.join(args.out, n)))
    log(f"完成: {args.out}（{total / 2**30:.2f} GiB）")
    log("Go 侧用法: core.memory.multimodal_space.type=onnx + "
        f"core.memory.multimodal_space.onnx.model_dir={args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
