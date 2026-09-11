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
import gc
import json
import os
import shutil
import subprocess
import sys
import time

import numpy as np
import torch

INSTRUCTION = "Represent the user's input."
IMAGE_SIZE = 768
PATCH_SIZE = 16
TEMPORAL_PATCH = 2
SPATIAL_MERGE = 2
# 每个时间组合并后的视觉 token 数：(768/16/2)^2 = 576。
VISUAL_TOKENS_PER_GROUP = (IMAGE_SIZE // PATCH_SIZE // SPATIAL_MERGE) ** 2
MAX_LENGTH = 1024  # 768x768 有 576 个视觉 token；512 会截断视觉占位符
DEFAULT_MODEL_ID = "Qwen/Qwen3-VL-Embedding-2B"
REFERENCE_TEXT = "今天天气怎么样"
REFERENCE_IMAGE_RGB = (200, 30, 30)
# 视频参考：4 帧、4 种颜色 → 2 个时间组。用可区分的颜色，
# 这样帧顺序（组 g 的 tp0←帧2g、tp1←帧2g+1）写错时参考向量立刻不匹配。
REFERENCE_VIDEO_RGB = [(10, 10, 10), (200, 20, 20), (20, 200, 20), (20, 20, 200)]
DEFAULT_VIDEO_GROUPS = (2, 3, 4)

# MAX_LENGTH 由 main() 按 --video-groups 调大；做成模块级是因为文本/图像/视频
# 三个输入构造函数共用它。
MAX_LENGTH = 1024


def max_length_for(video_groups) -> int:
    """足够容纳最大视频档的序列长度。

    图像路径只需 598 token（1 组），但视频是 G×576：G=2 就要 1190，
    G=4 要 2342。实测过：若沿用图像的 1024，处理器会因截断而报
    「Mismatch in video token count between text and input_ids」。
    模板文本实测约 38 token，这里留 256 余量（允许将来插入更长的指令）。
    """
    return max(1024, max(video_groups) * VISUAL_TOKENS_PER_GROUP + 256)


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


def image_inputs(processor, model, image, groups=1):
    """构造视觉输入。

    groups=1 走图像路径（<|image_pad|>）；groups>1 走视频路径
    （<|video_pad|>，2×groups 帧，相邻两帧一个时间组）。
    两者模板结构一致，只差占位符与组数。
    """
    lm = model.model.language_model
    if groups == 1:
        conv = [{"role": "system", "content": [{"type": "text", "text": INSTRUCTION}]},
                {"role": "user", "content": [{"type": "image", "image": image}]}]
        rendered = processor.apply_chat_template([conv], add_generation_prompt=True, tokenize=False)
        x = processor(text=rendered, images=[image], do_resize=False,
                      return_tensors="pt", truncation=True, max_length=MAX_LENGTH)
        with torch.no_grad():
            vo = model.model.visual(x["pixel_values"], grid_thw=x["image_grid_thw"], return_dict=True)
        pos, _ = model.model.get_rope_index(
            x["input_ids"], x["mm_token_type_ids"],
            image_grid_thw=x["image_grid_thw"], attention_mask=x["attention_mask"])
        visual_mask = x["mm_token_type_ids"] == 1
    else:
        frames = video_frames(groups)
        conv = [{"role": "system", "content": [{"type": "text", "text": INSTRUCTION}]},
                {"role": "user", "content": [{"type": "video", "video": frames}]}]
        rendered = processor.apply_chat_template([conv], add_generation_prompt=True, tokenize=False)
        # do_sample_frames=False 至关重要：处理器默认按 fps 重采样视频，
        # 未提供 video_metadata 时回落到 fps=24，会把任何帧数都改成 grid_t=2
        # （实测 4/6/8 帧都变成 1152 个视觉 token）。那会把「G 帧」变成
        # 「2 帧」，且在导出阶段看起来一切正常。
        x = processor(text=rendered, videos=[frames], do_resize=False, do_sample_frames=False,
                      return_tensors="pt", truncation=True, max_length=MAX_LENGTH)
        got_groups = int(x["video_grid_thw"][0][0])
        if got_groups != groups:
            raise SystemExit(
                f"视频时间组数 {got_groups}，期望 {groups}（处理器重采样了帧？"
                "确认 do_sample_frames=False 未被覆盖）")
        with torch.no_grad():
            vo = model.model.visual(x["pixel_values_videos"], grid_thw=x["video_grid_thw"], return_dict=True)
        pos, _ = model.model.get_rope_index(
            x["input_ids"], x["mm_token_type_ids"],
            video_grid_thw=x["video_grid_thw"], attention_mask=x["attention_mask"])
        visual_mask = x["mm_token_type_ids"] == 2

    hidden = lm.embed_tokens(x["input_ids"]).clone()
    if int(visual_mask.sum()) != groups * VISUAL_TOKENS_PER_GROUP:
        # 截断、模板改动、占位符扩展异常都会落到这里。它能区分
        # 「真的错了」与「只是看起来像」，比后续 scatter 报形状不符清楚得多。
        raise SystemExit(
            f"groups={groups} 视觉 token 数 {int(visual_mask.sum())}，期望 "
            f"{groups * VISUAL_TOKENS_PER_GROUP}（max_length={MAX_LENGTH}；"
            "截断会导致此错，请提高 --video-groups 推导出的 max_length）")
    hidden[visual_mask] = vo.pooler_output
    deep = []
    for d in vo.deepstack_features:
        full = torch.zeros_like(hidden)
        full[visual_mask] = d
        deep.append(full)
    cos, sin = lm.rotary_emb(hidden, pos)
    return x, hidden, tuple(deep), cos, sin, causal_mask(hidden.shape[1]), pos


def video_frames(groups: int):
    from PIL import Image

    fps = list(REFERENCE_VIDEO_RGB)
    while len(fps) < 2 * groups:
        fps.append(fps[len(fps) % len(REFERENCE_VIDEO_RGB)])
    return [Image.new("RGB", (IMAGE_SIZE, IMAGE_SIZE), c) for c in fps[: 2 * groups]]


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


RESULT_PREFIX = "@@VERIFY_RESULT@@"


def onnx_session(path: str):
    import onnxruntime as ort

    return ort.InferenceSession(path, providers=["CPUExecutionProvider"])


def load_model(model_dir: str):
    """加载 FP32 CPU 全模型与处理器（导出与校验共用同一套加载参数）。"""
    from transformers import AutoProcessor
    from transformers.models.qwen3_vl.modeling_qwen3_vl import Qwen3VLForConditionalGeneration

    processor = AutoProcessor.from_pretrained(model_dir, trust_remote_code=True, padding_side="right")
    model = Qwen3VLForConditionalGeneration.from_pretrained(
        model_dir, dtype=torch.float32, low_cpu_mem_usage=True).eval()
    return model, processor


def verify_case(case: str, out_dir: str, model, processor) -> dict:
    """在**单个进程内**只校验一个用例，返回该用例的参考片段。

    一个用例一个进程是有意的：这里必须同时驻留 PyTorch 全模型（~8GB）与
    Transformer.onnx（~7.5GB）。若在同一进程里连着校验图像与各档视频，
    每档新建的视觉图（~1.6GB/张）不会及时释放，峰值是它们之和——
    在 17GB 内存的机器上会被 OOM 杀掉（实测：校验到视频档时 python3 被 kill，
    total-vm 26GB）。拆成子进程后峰值等于单个用例，且某一档崩了不影响其余档。
    """
    ts = onnx_session(os.path.join(out_dir, "TokenEmbedding.onnx"))
    xs = onnx_session(os.path.join(out_dir, "Transformer.onnx"))
    lm = model.model.language_model

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

    if case == "text":
        x, _h, _d, cos, sin, _cm, pos = text_inputs(processor, lm, REFERENCE_TEXT)
        with torch.no_grad():
            ref = model.model(input_ids=x["input_ids"], attention_mask=x["attention_mask"],
                              position_ids=pos, use_cache=False).last_hidden_state[:, -1].numpy()
        hidden = ts.run(None, {"input_ids": x["input_ids"].numpy().astype(np.int64)})[0]
        zero = np.zeros_like(hidden)
        got = run_transform(hidden, (zero, zero, zero), cos.numpy(), sin.numpy())
        cos_v = compare("text/onnx-vs-full", got, ref)
        return {"case": case, "cos": cos_v, "reference": {
            "text": REFERENCE_TEXT,
            "text_vector_prefix": [float(v) for v in unit(got[0])[:12]],
            "text_norm_raw": float(np.linalg.norm(got[0])),
            "dim": int(got.shape[1]),
        }}

    # 图像与视频共用同一条后半段（视觉塔 → 按掩码散射 → 语言 Transformer）：
    # 两者的差异只在「视觉图 + 输入张量名 + token 类型 + 视觉 token 数」。
    if case == "image":
        x, _h, _d, cos, sin, _cm, _ = image_inputs(processor, model, reference_image())
        pixels = x["pixel_values"]
        visual_path = os.path.join(out_dir, "Vision.onnx")
        token_type, want_tokens, name = 1, VISUAL_TOKENS_PER_GROUP, "image"
        full_kwargs = {"pixel_values": x["pixel_values"], "image_grid_thw": x["image_grid_thw"]}
        prefix_key, norm_key = "image_vector_prefix", "image_norm_raw"
        extra: dict[str, object] = {
            "image_rgb": list(REFERENCE_IMAGE_RGB),
            "image_size": IMAGE_SIZE,
        }
    elif case.startswith("video_g"):
        groups = int(case.split("_g", 1)[1])
        x, _h, _d, cos, sin, _cm, _ = image_inputs(processor, model, None, groups=groups)
        pixels = x["pixel_values_videos"]
        visual_path = os.path.join(out_dir, f"Vision_g{groups}.onnx")
        token_type, want_tokens, name = 2, groups * VISUAL_TOKENS_PER_GROUP, case
        full_kwargs = {"pixel_values_videos": x["pixel_values_videos"],
                       "video_grid_thw": x["video_grid_thw"]}
        prefix_key, norm_key = "video_vector_prefix", "video_norm_raw"
        extra = {
            "video_groups": groups,
            "video_frame_rgb": [list(c) for c in REFERENCE_VIDEO_RGB[: 2 * groups]],
        }
    else:
        raise SystemExit(f"未知校验用例: {case}")

    vs = onnx_session(visual_path)
    with torch.no_grad():
        ref = model.model(input_ids=x["input_ids"], attention_mask=x["attention_mask"],
                          mm_token_type_ids=x["mm_token_type_ids"],
                          use_cache=False, **full_kwargs).last_hidden_state[:, -1].numpy()
    hidden = ts.run(None, {"input_ids": x["input_ids"].numpy().astype(np.int64)})[0]
    vis = vs.run(None, {"pixel_values": pixels.numpy().astype(np.float32)})
    mask = x["mm_token_type_ids"].numpy() == token_type
    # 视觉区间长度不对，说明模板/占位符/档位三者有一处错了。单独报错比
    # 后面 scatter 抛「形状不符」清楚得多。
    if int(mask.sum()) != want_tokens:
        raise SystemExit(f"{name}: 视觉 token 数 {int(mask.sum())}，期望 {want_tokens}")
    hidden = hidden.copy()
    hidden[mask] = vis[3]
    deep = []
    for d in vis[:3]:
        full = np.zeros_like(hidden)
        full[mask] = d
        deep.append(full)
    got = run_transform(hidden, tuple(deep), cos.numpy(), sin.numpy())
    cos_v = compare(f"{name}/onnx-vs-full", got, ref)
    extra[prefix_key] = [float(v) for v in unit(got[0])[:12]]
    extra[norm_key] = float(np.linalg.norm(got[0]))
    extra["dim"] = int(got.shape[1])
    return {"case": case, "cos": cos_v, "reference": extra}

def verify_onnx(out_dir: str, video_groups, require_video: bool = True, model_dir: str = "") -> dict:
    """逐用例在子进程里校验导出后的 ONNX 图，汇总冻结参考向量。

    返回参考向量（供 Go 侧测试冻结使用）：Go 必须复现同一套预处理与模板，
    因此这里把同一输入下的期望向量前若干维导出。
    """
    log("校验②：导出后的 ONNX 三段图 vs 完整模型（每个用例一个进程）")
    cases = case_list(out_dir, video_groups, require_video)
    reference: dict[str, object] = {}
    verified: list = []
    for case in cases:
        # 先把子进程跑完，再决定要不要采它的参考值。
        # 历史教训：曾经把「参考只取第一档」写成在调用前 continue，
        # 结果 video_g3/g4 根本没被校验，而脚本仍然 exit 0 ——
        # 一个「通过」的假象比报错危险得多。
        res = run_verify_child(case, out_dir, model_dir)
        verified.append(case)
        log(f"  {case}: cos={res['cos']:.9f}")
        if case.startswith("video_g") and "video_groups" in reference:
            # 参考只取第一档（Go 侧回归用一档就够），但这一档本身已经真的校验过。
            continue
        reference.update(res["reference"])
    # 覆盖度必须与计划一致：少跑一个用例就不算校验完成。
    if verified != cases:
        raise SystemExit(f"校验覆盖不完整：计划 {cases}，实际 {verified}")
    log(f"校验覆盖 {len(verified)} 个用例: {', '.join(verified)}")
    if "dim" not in reference:
        raise SystemExit("校验没有产出 dim")
    return reference


def case_list(out_dir: str, video_groups, require_video: bool) -> list:
    """要校验的用例列表。视频档缺图时：刚导出完必须报错，校验旧目录则跳过。"""
    cases = ["text", "image"]
    for groups in video_groups:
        path = os.path.join(out_dir, f"Vision_g{groups}.onnx")
        if os.path.exists(path):
            cases.append(f"video_g{groups}")
        elif require_video:
            raise SystemExit(f"缺少 {path}（--video-groups 包含 {groups} 但未导出）")
        else:
            log(f"跳过视频档 G={groups}：目录里没有 {os.path.basename(path)}")
    return cases


def run_verify_child(case: str, out_dir: str, model_dir: str) -> dict:
    """在子进程里校验一个用例并取回它的参考片段。"""
    cmd = [sys.executable, os.path.abspath(__file__), "--out", out_dir, "--verify-case", case]
    if model_dir:
        cmd += ["--model-dir", model_dir]
    log(f"  校验 {case}（独立进程）")
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        out = ((proc.stderr or "") + (proc.stdout or "")).strip().splitlines()
        raise SystemExit(f"校验 {case} 失败（exit={proc.returncode}）:\n" + "\n".join(out[-20:]))
    for line in reversed((proc.stdout or "").splitlines()):
        if line.startswith(RESULT_PREFIX):
            return json.loads(line[len(RESULT_PREFIX):])
    raise SystemExit(
        f"校验 {case} 的子进程没有输出结果行；stdout 末尾: {(proc.stdout or '')[-300:]!r}")


def video_groups_from_config(out_dir: str):
    """读取产物自带的 video_groups，读不到则返回 None。

    max_length 由 video_groups 推导，而推导结果必须与导出时一致，否则校验
    会因截断而报「视觉 token 数不符」。产物自己的 config 是权威来源，
    比让调用方记得重传 --video-groups 可靠。
    """
    try:
        with open(os.path.join(out_dir, "embed_config.json")) as f:
            cfg = json.load(f)
    except (OSError, ValueError):
        return None
    groups = cfg.get("video_groups")
    if isinstance(groups, list) and groups and all(isinstance(g, int) and g >= 2 for g in groups):
        return sorted(groups)
    return None


def reference_image():
    from PIL import Image

    return Image.new("RGB", (IMAGE_SIZE, IMAGE_SIZE), REFERENCE_IMAGE_RGB)


# ─────────────────────────── 导出 ───────────────────────────

def export_graphs(out_dir: str, model, processor, model_dir: str, transformer, video_groups) -> None:
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

    log("导出 Vision.onnx（图像，固定 grid 1×48×48）")
    grid = torch.tensor([[1, IMAGE_SIZE // PATCH_SIZE, IMAGE_SIZE // PATCH_SIZE]], dtype=torch.long)
    xi, _h, _d, _c, _s, _cm, _p = image_inputs(processor, model, reference_image())
    pv = xi["pixel_values"]
    with torch.no_grad():
        torch.onnx.export(
            VisionTower(model.model.visual, grid).eval(), (pv,), os.path.join(out_dir, "Vision.onnx"),
            input_names=["pixel_values"],
            output_names=["deepstack_feature_0", "deepstack_feature_1",
                          "deepstack_feature_2", "vision_hidden_states"],
            opset_version=17, do_constant_folding=True, dynamo=False,
        )

    # 视频：每个时间组数一张图。grid_thw 被 legacy tracer 固化为常量，
    # 所以“动态时间轴”不可行（实测导出的图里根本没有 grid_thw 输入）；
    # 反过来，每档导一张则完全可验证。
    for groups in video_groups:
        name = f"Vision_g{groups}.onnx"
        log(f"导出 {name}（视频，固定 grid {groups}×48×48 ⇒ {2 * groups} 帧）")
        vgrid = torch.tensor([[groups, IMAGE_SIZE // PATCH_SIZE, IMAGE_SIZE // PATCH_SIZE]], dtype=torch.long)
        vx, _h, _d, _c, _s, _cm, _p = image_inputs(processor, model, None, groups=groups)
        vpv = vx["pixel_values_videos"]
        with torch.no_grad():
            torch.onnx.export(
                VisionTower(model.model.visual, vgrid).eval(), (vpv,), os.path.join(out_dir, name),
                input_names=["pixel_values"],
                output_names=["deepstack_feature_0", "deepstack_feature_1",
                              "deepstack_feature_2", "vision_hidden_states"],
                opset_version=17, do_constant_folding=True, dynamo=False,
            )

    for name in ("tokenizer.json", "tokenizer_config.json", "chat_template.jinja", "added_tokens.json"):
        src = os.path.join(model_dir, name)
        if os.path.exists(src):
            shutil.copy2(src, os.path.join(out_dir, name))


def write_config(out_dir: str, model, processor, video_groups) -> None:
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
        "supports_native_video": True,
        "video_groups": list(video_groups),
        "unsupported_modalities": ["audio"],
        "notes": ("视频每个时间组数（G）各一张 Vision 图：grid_thw 被 legacy tracer "
                  "固化为常量，无法做成运行时输入；用错档会因维度不符报错。"
                  "帧：相邻两帧构成一个时间组，temporal 槽 tp0←帧2g、tp1←帧2g+1。"
                  "音频不在 Qwen3-VL 原生模态内（无 audio_token_id），需另一模型。"),
    }
    with open(os.path.join(out_dir, "embed_config.json"), "w") as f:
        json.dump(meta, f, ensure_ascii=False, indent=2)
    log(f"写出 embed_config.json dim={meta['dim']} rope_theta={meta['rope_theta']} "
        f"mrope={meta['mrope_section']} video_groups={meta['video_groups']}")


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
    ap.add_argument("--video-groups", default=",".join(str(g) for g in DEFAULT_VIDEO_GROUPS),
                    help="逗号分隔的视频时间组数，每档导一张 Vision_g{N}.onnx"
                         f"（默认 {','.join(str(g) for g in DEFAULT_VIDEO_GROUPS)}；"
                         "G 组合 2G 帧，即默认 4/6/8 帧）")
    ap.add_argument("--verify-only", action="store_true",
                    help="不重新导出，只校验已存在的 <out> 并（重新）写出参考向量")
    ap.add_argument("--verify-case", default="",
                    help=argparse.SUPPRESS)  # 内部用：单用例校验子进程
    args = ap.parse_args()
    try:
        video_groups = [int(g) for g in str(args.video_groups).split(",") if str(g).strip()]
    except ValueError:
        raise SystemExit(f"--video-groups 必须是逗号分隔的整数，得到 {args.video_groups!r}")
    if not video_groups or any(g < 2 for g in video_groups):
        raise SystemExit("--video-groups 需至少一个 >=2 的整数（单图档是 Vision.onnx，不用列）")

    if args.verify_case or args.verify_only:
        cfg_groups = video_groups_from_config(args.out)
        if cfg_groups and cfg_groups != video_groups:
            log(f"按产物 embed_config.json 使用 video_groups={cfg_groups}（命令行是 {video_groups}）")
            video_groups = cfg_groups

    global MAX_LENGTH
    MAX_LENGTH = max_length_for(video_groups)
    log(f"max_length={MAX_LENGTH}（按最大档 {max(video_groups)} 组×{VISUAL_TOKENS_PER_GROUP} 推导）")

    if args.verify_case:
        model_dir = args.model_dir or pull_model(args.model_id, args.model_store)
        model, processor = load_model(model_dir)
        res = verify_case(args.verify_case, args.out, model, processor)
        print(RESULT_PREFIX + json.dumps(res, ensure_ascii=False), flush=True)
        return 0

    if args.verify_only:
        # 校验既有产物目录：既能确认线上在用的图没坏，也能给旧目录补上参考向量。
        for name in ("TokenEmbedding.onnx", "Transformer.onnx", "Vision.onnx"):
            if not os.path.exists(os.path.join(args.out, name)):
                raise SystemExit(f"{args.out} 下缺少 {name}，无法 --verify-only")
        model_dir = args.model_dir or pull_model(args.model_id, args.model_store)
        reference = verify_onnx(args.out, video_groups, require_video=False, model_dir=model_dir)
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

    # transformers / PIL 只在真正导出时才需要（拉取模型本身只用 huggingface_hub）；
    # 这里提前导入一次，缺依赖时给出清晰报错而不是走到深处才炸。
    from PIL import Image  # noqa: F401

    log("加载 processor / model（FP32，CPU）")
    model, processor = load_model(model_dir)
    transformer = Transformer(model.model.language_model).eval()

    verify_split_torch(model, processor, transformer)
    export_graphs(args.out, model, processor, model_dir, transformer, video_groups)
    write_config(args.out, model, processor, video_groups)

    # 校验在子进程里跑，父进程先把模型释放掉，把内存完全让给子进程。
    del transformer, model
    gc.collect()

    reference = None if args.skip_verify else verify_onnx(args.out, video_groups, model_dir=model_dir)
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
