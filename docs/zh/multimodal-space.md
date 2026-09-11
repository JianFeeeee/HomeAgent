# 统一多模态向量空间（Qwen3-VL-Embedding-2B）

文本、图像、**视频帧** 在同一模型、同一 2048 维、同一 fingerprint 空间里被编码。
记忆系统用它做三件事：多模态图记忆的跨模态召回、multimodal doc 的向量融合、
multimodal context 的相关性裁剪/淘汰。

统一空间取代了此前「把图片交给视觉模型生成文字描述、再按描述检索」的做法。
那条链路有三个致命缺陷：描述是异步生成的（未生成前媒体等于不存在）、语义检索
实际上只搜描述文字、图库里的「媒体节点」只是描述文本的投影而不是媒体本身。
**不要再引入任何描述式索引。**

## 一、产物与获取

产物约 8 GB（含外部权重），**不进仓库**；用导出脚本自动拉取模型并导出：

```bash
# 自动拉取（HuggingFace 优先，失败回落 ModelScope）+ 导出 + 自检
python3 scripts/export_qwen3vl_embedding_onnx.py \
    --out /home/newqqagent/models/qwen3-vl-embed-multimodal-onnx

# 已下载过模型：跳过拉取
python3 scripts/export_qwen3vl_embedding_onnx.py \
    --model-dir /path/to/Qwen3-VL-Embedding-2B \
    --out /home/newqqagent/models/qwen3-vl-embed-multimodal-onnx

# 参考向量默认直接写进产物目录（<out>/qwen_reference.json），无需额外参数
python3 scripts/export_qwen3vl_embedding_onnx.py --model-dir ... --out ...
```

国内镜像：导出脚本沿用 `huggingface_hub` 的约定，直接 `export HF_ENDPOINT=https://hf-mirror.com` 即可。
依赖：`torch`（CPU 版即可）、`transformers>=4.57`、`onnx`、`onnxruntime`、`pillow`、`numpy`，
以及可选的 `huggingface_hub` / `modelscope`。显存不需要，内存建议 ≥ 16 GB（FP32 加载约 8 GB）。

导出脚本**会清空 --out 目录**后重写，避免旧图/旧外部权重污染 fingerprint
（fingerprint 变化会触发一次无意义的全量向量重算）。因此不要直接覆盖线上正在使用的目录，
先导出到新目录再切换。

### 产物契约（Go 侧按此读取）

| 文件 | 输入 | 输出 |
|---|---|---|
| `TokenEmbedding.onnx` | `input_ids` int64 `[1,seq]` | `hidden` float `[1,seq,2048]` |
| `Transformer.onnx` | `hidden`、`deepstack_0/1/2` `[1,seq,2048]`、`rotary_cos/sin` `[1,seq,128]`、`causal_mask` `[1,1,seq,seq]` | `embedding` `[1,2048]` |
| `Vision.onnx(+.data)` | `pixel_values` `[2304,1536]` | `deepstack_feature_0/1/2`、`vision_hidden_states` `[576,2048]` |

外加 `tokenizer.json`、`tokenizer_config.json`、`chat_template.jinja`、`embed_config.json`。

三段只是部署形式，不是三个向量空间：图文共用同一 token embedding、同一 28 层
Transformer、同一 last-token 池化。RoPE 与视觉特征散射故意留在 Go 计算，
因为旧式 tracer 会把 `seq=598 / visual=576` 烘焙进图里——签名上写着 dynamic
axis，实际却只能用导出的那个长度运行。

### 导出脚本自检（不可省）

脚本内部跑两道校验，任一道 cos < 0.999999 就以非零码退出：

1. 分段 PyTorch（三段组合）对比完整模型前向；
2. 用 onnxruntime 跑**导出后**的三段图，再对比完整模型前向。

「能加载」不等于「算得对」：形状错、输入名错、池化位置错的图都能正常 load。

## 二、启用

```bash
# 配置库（config.db）或 WebUI 设置页
core.memory.multimodal_space.type = onnx
core.memory.multimodal_space.onnx.model_dir = /home/newqqagent/models/qwen3-vl-embed-multimodal-onnx
```

注意事项：

- `homed` 必须带 `onnxruntime` build tag 构建，且 `libonnxruntime.so` 可被找到
  （`/opt/onnxruntime/libonnxruntime.so` 等）。未带 tag 时 `qwen` 是 no-op stub。
- 改配置后需重启进程生效。
- 未配置时优雅降级：文档层退到 TF-IDF 稀疏检索，媒体块仍按结构边关联，只是没有跨模态召回。

## 三、模态覆盖范围

| 模态 | 状态 | 说明 |
|---|---|---|
| 文本 | ✅ 原生 | `VectorizeDense` |
| 图像 | ✅ 原生 | `EmbedImageDense`，固定 768×768 视觉塔 |
| 视频 | ⚠️ 逐帧 | 上层抽帧后**逐帧按图像编码**，同模型/同维度/同 fingerprint；不做跨帧时序注意力 |
| 音频 | ❌ 明确不支持 | 返回 `vector.ErrModalityUnsupported` |

**音频不得用视觉塔硬编码**，也**不得**拿另一个模型的向量顶替——那会把两套坐标系
混进同一空间，检索出的相似度没有任何意义，而且错误是静默的。未来接入真正的统一
音频模型后再扩展。

### 为什么视频不做原生时序（已实测，勿重复尝试）

Qwen3-VL 视觉塔把 `grid_thw` 当 Python 值消费（源码里是 `grid_thw.tolist()`）。
legacy tracer（`dynamo=False`）会把它固化成常量：实测导出后 ONNX 图里**根本没有**
`grid_thw` 输入，用别的帧数调用直接报 `Invalid input name: grid_thw`；
导出时的 TracerWarning 明确提示 `Converting a tensor to a Python list might cause
the trace to be incorrect`。

因此视觉塔固定 `grid=(1,48,48)`。要做到原生多帧需要换 `torch.export`/dynamo 路径，
而该路径此前已产生过「形状看似动态、实际错误」的静默故障（Core.onnx 的
`3 by 23 / 3 by 598` 广播错误），在时序维度上重试的收益不足以抵消风险。
视频价值由「逐帧进入同一空间」提供：帧是真实媒体块，按自己的向量被召回。

## 四、验证

```bash
# Go 侧：ONNX 路径（模型目录缺失时自动 skip）
QWEN_ONNX_MODEL_DIR=/home/newqqagent/models/qwen3-vl-embed-multimodal-onnx \
  go test -tags onnxruntime ./internal/memory/qwen/ -v

# 排除二进制交付问题的替代：先单独验证模型与 CSV 无关的 ONNX 图
go vet -tags onnxruntime ./...
```

Go 测试覆盖：冻结参考向量（文本/图像各 12 维）、同输入确定性、不同输入敏感性、
图像与文本向量必须不同、以及音频/视频必须返回 `ErrModalityUnsupported`。

冻结参考向量由导出脚本写入**产物目录本身**（`<out>/qwen_reference.json`），
来源可追溯：同一脚本既产出模型，也产出「这个模型对固定输入应有的输出」。
重新导出后若参考值变化，说明权重或图结构变了，必须显式更新参考而不是放宽阈值。

> **参考向量是 L2 归一化后的值。** ONNX 图返回的是 final norm 之后的原始
> last hidden（量级约 100），而 Go 侧 `VectorizeDense` / `EmbedImageDense`
> 返回归一化向量。写参考时忘归一化，Go 测试会全线不匹配，而现象看起来
> 像“模型不对”，实际只是两边对“向量”的定义不同。

验证既有产物（不重新导出）：

```bash
python3 scripts/export_qwen3vl_embedding_onnx.py --verify-only --model-dir <model> \
    --out /home/newqqagent/models/qwen3-vl-embed-multimodal-onnx
```

脚本会顺便把归一化后的参考向量写入该目录。

### 与现有部署产物的等价性

本仓库脚本对同一源模型导出时，`TokenEmbedding.onnx` 与 `Transformer.onnx` 与
线上在用的产物**逐字节相同**（sha256 一致）；`Vision.onnx` 差异仅在打包形式：
旧产物把权重量到外部 `Vision.onnx.data`，新脚本内联在图里。两者数值等价。

注意这会带来一个**操作性**差异：Go 的结构指纹（`computeFingerprint`）把
`*.onnx.data` 的文件名与大小算在内，因此「外部权重版 ↔ 内联版」互换会让
fingerprint 变化，从而触发一次全量向量重算。重算不会**算错**（数值等价），
只是白花一次 CPU；若不想触发，就保持产物打包形式不变。

## 五、资源成本

- 产物磁盘约 8 GB；导出过程峰值内存约 10–12 GB（FP32 加载）。
- 单次 CPU 推理：文本约几十毫秒量级，图像（2304 patch 过 24 层视觉塔 + 28 层语言模型）
  明显更重，因此入库时不阻塞对话，靠 `reembedStaleMedia` 在启动时并发迁移
  （ONNX 路径 4 worker）。
- fingerprint 由三段图 + `embed_config.json` + 外部权重文件名/大小共同决定；
  换模型或重新导出都会让它变化，从而触发历史向量重算——这是预期行为。
