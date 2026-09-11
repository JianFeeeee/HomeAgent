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
# 默认导出 图像 + 视频 G=2,3,4（即 4/6/8 帧）
python3 scripts/export_qwen3vl_embedding_onnx.py \
    --out /home/newqqagent/models/qwen3-vl-embed-multimodal-onnx

# 只要 4 帧的视频档（省磁盘、省内存）
python3 scripts/export_qwen3vl_embedding_onnx.py --video-groups 2 --out ...

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
| `Vision_g{N}.onnx` | `pixel_values` `[N×2304,1536]` | 同上，`[N×576,2048]` |

外加 `tokenizer.json`、`tokenizer_config.json`、`chat_template.jinja`、`embed_config.json`、
`qwen_reference.json`。

`Vision.onnx` 是图像（单时间组）；`Vision_g{N}.onnx` 是视频（N 个时间组 = 2N 帧）。
**没有 `Vision_g1.onnx`**——单组就是图像那张。

三段只是部署形式，不是三个向量空间：图文共用同一 token embedding、同一 28 层
Transformer、同一 last-token 池化。RoPE 与视觉特征散射故意留在 Go 计算，
因为旧式 tracer 会把 `seq=598 / visual=576` 烘焙进图里——签名上写着 dynamic
axis，实际却只能用导出的那个长度运行。

### ⚠️ max_length 必须按最大视频档推导

`embed_config.json` 的 `max_length` 是**整条序列**的上限，包含视觉占位符：
图像只需 598 token（1×576 + 模板），而视频是 G×576——G=2 就要 1190，G=4 要 2342。
沿用图像的 1024 会让处理器静默截断，然后在 transformers 内部报
`Mismatch in video token count between text and input_ids`。
导出脚本因此用 `max_length_for(video_groups) = max(1024, max(G)×576 + 256)` 自动推导，
并在构造视觉输入后显式断言视觉 token 数，把错误提前到导出阶段。

### 导出脚本自检（不可省）

脚本内部跑两道校验，任一道 cos < 0.999999 就以非零码退出：

1. 分段 PyTorch（三段组合）对比完整模型前向；
2. 用 onnxruntime 跑**导出后**的三段图，再对比完整模型前向。

「能加载」不等于「算得对」：形状错、输入名错、池化位置错的图都能正常 load。

## 二、启用

核心不识别任何具体模型：它只按配置里的 **provider 名**从公共注册表
（`pkg/embedding`）打开一个 provider，并把 `options.*` 原样交给它。
模型文件布局、预处理、媒体解码、运行时都在 provider 内部。

```bash
# 配置库（config.db）或 WebUI 设置页
core.memory.multimodal_space.provider = qwen3vl
core.memory.multimodal_space.options.model_dir = /home/newqqagent/models/qwen3-vl-embed-multimodal-onnx

# 或换成一个外部向量服务（任何语言写的都行）
core.memory.multimodal_space.provider = http
core.memory.multimodal_space.options.endpoint = http://127.0.0.1:18999/embed
core.memory.multimodal_space.options.dimension = 2048
```

`options.*` 是 provider 自己的命名空间，核心不做任何解释（对 `qwen3vl` 是
`model_dir`，对 `http` 是 `endpoint`/`dimension`/`api_key`/…）。第三方 provider
可以定义自己的选项，无需改核心。

注意事项：

- 内置 provider `qwen3vl` 要求 `homed` 带 `onnxruntime` build tag 构建，且
  `libonnxruntime.so` 可被找到（`/opt/onnxruntime/libonnxruntime.so` 等）。
  未带 tag 时该 provider 会注册但打开时报「requires build tag」，而不是静默降级。
- `provider` 为空时禁用多模态向量检索，退回纯 fastText 文本路径。
- 改配置后需重启进程生效。
- 未配置时优雅降级：文档层退到 TF-IDF 稀疏检索，媒体块仍按结构边关联，只是没有跨模态召回。

## 二·补、给核心接自己的模型

核心只依赖一个很小的公共接口（`pkg/embedding`）：

```go
// 输入对核心是不透明字节：modality 决定语义，Data+MIME 由 provider 解释。
type Input struct {
    Modality Modality   // text / image / audio / video / …
    Purpose  Purpose    // query / document
    Text     string
    Data     []byte
    MIME     string
    Metadata map[string]string
}

type Provider interface {
    Embed(ctx context.Context, in Input) ([]float64, error)
    Info() Info                     // Dimension, Fingerprint, Modalities
    Close()
}
```

接入步骤：新建一个包，在 `init()` 里 `embedding.Register("your-model", factory)`，
再把这个包空白导入你的发行版 `main`（或替换内置 provider 的导入行）。
分词、预处理、解码、显存/内存管理、模型文件命名全部由你的 provider 决定。

两条原则值得强调：

- **能力是数据，不是接口方法**：支持哪些模态写在 `Info().Modalities` 里。
  这样新增模态不需要改核心接口，核心也不需要为每个新模态做类型断言。
- **不支持的模态返回 `embedding.ErrUnsupportedModality`**，而不要拿别的模型顶替，
  也不要降级成一个普通错误——调用方靠它区分「永远不会有向量」与「本次失败可重试」。

## 三、模态覆盖范围

### Qwen3-VL-Embedding-2B（本空间，2048 维）

模型卡明载支持 **Text / images / screenshots / videos**；`config.json` 有
`image_token_id` 与 `video_token_id`，**没有 `audio_token_id`/`audio_config`**。

| 模态 | 状态 | 说明 |
|---|---|---|
| 文本 | ✅ 原生 | `VectorizeDense` |
| 图像 | ✅ 原生 | `EmbedImageDense`，`Vision.onnx`，固定 768×768 |
| 视频 | ⚠️ 视觉侧已导出并校验，**Go 模板未完成** | `EmbedVideoDense` + `Vision_g{N}.onnx`；见下节 |
| 音频 | ❌ 本轮明确不做 | 决策结果；该模型也不具备（无 `audio_token_id`） |

### 视频：帧 → 时间组 → M-RoPE（均已实测对齐）

| 项 | 值 | 验证方式 |
|---|---|---|
| 占位符 | `<|video_pad|>` = **151656**（图像是 `<|image_pad|>` = 151655） | 处理器实测 |
| 模板 | 与图像同构，只换占位符 | `apply_chat_template` repr 逐字符比对 |
| 帧→槽位 | 组 g 的 tp0←帧2g、tp1←帧2g+1 | PyTorch `torch.equal == True`，maxdiff=0；反向对照 False |
| patch 布局 | `[G,24,24,2,2,3,2,16,16]`，即图像排列以 grid_t 为最外层堆叠 | 纯色视频于图像张量 `torch.equal == True` |
| 视觉 token | `G×576` | 处理器实测（G=2 → 1152） |
| M-RoPE | 每组独立：`base=start+24g`；`t=base`、`h=base+j/24`、`w=base+j%24` | 对应 `get_rope_index` 把 video grid 展开成 G 个 `t=1` 项 |
| 用错档 | onnxruntime 报 `InvalidArgument`（维度不符） | 实验实测，**不会静默算错** |

同步注意事项：

- **帧数必须恰好是 `2×G`**（G 取已导出的档）。奇数帧时只用得上前 `2×floor(n/2)` 帧，
  多出的丢弃——不补重复帧，那会改变跳帧注意力看到的运动。
- **`video/*`（视频文件）不能直接喂给图像入口**：Go 侧没有视频解码器，
  `EmbedImageDense(raw, "video/mp4")` 返回 `ErrModalityUnsupported`。调用方必须先抽帧。
- 视觉图按需懒加载（每张约 1.6GB），未用到的档位不占内存。

### 导出视频时踩过的两个坑（都已加断言）

两个坑都会让产物「看起来正常、实际是错的」，且都不会在导出时报错：

1. **处理器会静默重采样帧**。不给 `video_metadata` 时它回落到 `fps=24`，
   把**任何**帧数都改成 `grid_t=2`：实测 4/6/8 帧全部得到 1152 个视觉 token。
   修法：`processor(..., videos=[frames], do_sample_frames=False)`。
2. **`max_length` 只按图像算是不够的**。它是整条序列（含视觉占位符）的上限：
   图像只需 598 token，而视频是 `G×576`——G=2 要 1190、G=4 要 2342。
   沿用 1024 会截断并报
   `Mismatch in video token count between text and input_ids`。
   修法：`max_length_for(G) = max(1024, max(G)×576 + 256)`。

两个坑都会在导出脚本里显式断言（视觉 token 数、`video_grid_thw` 的组数），
把错误提前到导出阶段而不是留给运行时。

### 音频（本轮决策：不加）

**Qwen3-VL 不支持音频**，由模型卡与 `config.json` 双重确认：

```
模型卡：Supported Input Modalities: Text, images, screenshots, videos, and …
config：image_token_id ✓ / video_token_id ✓ / audio_token_id ✗ / audio_config ✗
```

本机有音频能力的是另一个模型（**jina-v5-omni-nano**，768 维，含
`modeling_llava_eurobert_audio.py` 与 `audio_token_id=128256`），与 Qwen 空间
**不同维度、不同坐标系，绝不可互相比较**。决定：**本轮不接入**；
其侧车（`scripts/embed_sidecar.py`）也仍只实现 `text`/`image`，`audio` 返回 400。

无论何时接入，都**不允许**：拿视觉塔去编码音频字节、或用另一个模型的向量
冒充某空间的音频向量——那会把两套坐标系混进同一空间，且错误是静默的。
音频在原空间返回 `vector.ErrModalityUnsupported`，使调用方区分
「永远不会有向量」与「本次失败可重试」。

Qwen3-VL 视觉塔把 `grid_thw` 当 Python 值消费（源码里是 `grid_thw.tolist()`），
legacy tracer（`dynamo=False`）会把它固化成常量：实测把 `grid_thw` 声明为图输入后，
导出的 ONNX 图里**根本没有该输入**，换帧数调用直接报 `Invalid input name: grid_thw`；
导出时的 TracerWarning 明确提示
`Converting a tensor to a Python list might cause the trace to be incorrect`。

因此视频的可行做法是：**在导出时固定时间组数 G，每个 G 一张 Vision 图**
（grid = `[G, 48, 48]`），Go 侧按实际帧数选用匹配的图；用 G=2 的图去喂 G=3 的
数据属于未定义行为。视频文件本身不能直接喂进本空间（`video/*` 返回
`ErrModalityUnsupported`），必须由上层先抽帧。

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

## 视频：当前状态（未完成，不得当作已验证）

**视觉侧**：`Vision_g2/g3/g4.onnx` 已导出，且每一档都与完整 PyTorch 模型逐档对过
（`cos` 分别为 1.000000119 / 1.000000119 / 1.000000000，覆盖度断言通过）。

**Go 侧模板**：与 HuggingFace processor 产出**不相等**，因此冻结回归
（`TestEmbedderVideoMatchesONNXReference`）当前**显式跳过**并注明原因，不算通过。

已定位的差异：processor 会按时间组插入字面时间戳文本。逐 token 实测：

```
<|vision_start|> <0.0 seconds> <|vision_start|> {576×<|video_pad|>} <|vision_end|>
<1.0 seconds>   <|vision_start|> {576×<|video_pad|>} <|vision_end|>
```

而 Go 侧只生成 `<|vision_start|>{G×576 pads}<|vision_end|>`。同一输入下
Python `seq=1190`（1152 视觉 + **38** 文本），Go 侧只有 **22** 个文本 token。

注意两点：

- 时间戳文本**也占用 M-RoPE 位置**，所以 `TestVideoModelInputMRope` 的自洽断言
  通过**不能**证明与官方实现一致（它是拿自己算的序列验自己算的位置）。
- 修复位置在 provider 内部（模型专属模板本就属于 provider），不是核心。

另外，公共 provider 契约把 `Data+MIME` 交给 provider 自行解码；本 provider
没有视频解码器（Go 标准库不含 H.264/MP4），因此 `Info().Modalities` **不声明 video**，
`Embed(video)` 返回 `ErrUnsupportedModality`。视频走 provider 自己的
`EmbedVideoDense`（接收已解码帧）。待核心有了对 provider 不透明的多帧容器后，
再把视频纳入公共契约。
