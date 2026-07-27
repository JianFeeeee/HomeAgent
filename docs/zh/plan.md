# 三元组提取系统 — 施工方案

## 一、背景与目标

### 现状

- HomeAgent 已通过 systemd 托管运行，数据目录 `/home/newqqagent`
- 已积累 **62 万条原始对话记录**（125 个 raw TSV 文件）
- 当前三元组提取通过 `extractKeyTriples()` 硬编码 5 条规则完成（姓名/年龄/喜好/居住地/职业）
- `docToTriples()` 用相邻词机械拼接三元组，语义噪音大

### 目标

构建 **"句法定界 + 向量验义"** 双路三元组提取系统：

1. 用本机积累的对话语料训练一个依存句法分析模型
2. 模型以 ONNX 格式发布到 HuggingFace，Go 运行时启动时拉取
3. 依赖：Go 侧仅需 `onnxruntime_go`（纯 Go binding，无 CGO/Python）
4. 降级：模型不可用时退回现有 gojieba POS + 模板方案

---

## 二、整体架构

```
┌─────────────────────────────────────────────────────────────┐
│                   训练流水线 (Python，一次性)                  │
│                                                             │
│  /home/newqqagent/memory/raw/*.tsv                          │
│         │                                                   │
│         ▼                                                   │
│  数据导出 → 提取 user 语句 → 去重 → 句长过滤                   │
│         │                                                   │
│         ▼                                                   │
│  Baidu DDParser (教师模型) → 银标依存树                       │
│         │                                                   │
│         ▼                                                   │
│  UD Chinese Treebank (金标) + 银标混合 → supar 训练           │
│         │                                                   │
│         ▼                                                   │
│  ONNX 导出 → 上传 HuggingFace (your-org/chinese-dep-parser)  │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│                   推理流水线 (Go，运行时)                       │
│                                                             │
│  HomeAgent 启动                                              │
│         │                                                   │
│         ▼                                                   │
│  HuggingFace 下载 ONNX 模型 → onnxruntime_go 加载             │
│         │                                                   │
│         ▼                                                   │
│  用户输入 → gojieba 分词 + POS                               │
│         │                                                   │
│         ▼                                                   │
│  ONNX 推理 → 依存树解码 (head index + dep label)              │
│         │                                                   │
│         ▼                                                   │
│  句法模板提取三元组 (SBV-VOB / SBV-IOB / ATT-VOB / ...)      │
│         │                                                   │
│         ▼                                                   │
│  融合现有向量验证层 (StaticEmbedder + TransE) → 输出三元组    │
│                                                             │
│  模型缺失/加载失败 → 降级 gojieba POS + 模板                  │
└─────────────────────────────────────────────────────────────┘
```

---

## 三、阶段一：数据导出与探索

### 3.1 数据位置

```
/home/newqqagent/memory/raw/raw_*.tsv
格式: id \t session_id \t role \t content \t timestamp
```

### 3.2 导出脚本

脚本：`tools/export_conversations.py`

功能：
- 扫描所有 raw_*.tsv，提取 `role=user` 的语句
- 基础过滤：去除纯标点/极短句（<4 字），按 MD5 去重
- 输出 JSONL：`{text, session_id, timestamp, length}`
- 统计输出：句长分布直方图、总句数、唯一句数

### 3.3 DDParser 快速验证

在导出后的数据中随机抽 500 条，用 DDParser 标注后人工抽样检查：
- 依存树的句法合理性（主语/谓语/宾语是否能对齐）
- 常见错误模式（疑问句、省略句、口语化表达）
- 决定需过滤的句式黑名单（如有）

---

## 四、阶段二：训练流水线搭建

### 4.1 教师模型标注

```python
# 使用 Baidu LAC + DDParser 联合标注
# LAC：分词 + 词性标注
# DDParser：依存句法分析

文本: "我在杭州读书"
LAC → ['我', '在', '杭州', '读书'] / ['r', 'p', 'ns', 'v']
DDParser → [{'id':0,'head':2,'deprel':'SBV'},  # 我 → 在(主语)
            {'id':1,'head':3,'deprel':'ADV'},   # 在 → 杭州(状语)
            {'id':2,'head':3,'deprel':'ADV'},   # 杭州 → 读书(状语)
            {'id':3,'head':0,'deprel':'ROOT'}]  # 读书 → ROOT
```

产出格式：标准 CoNLL-U
```
1	我	_	r	_	_	2	SBV	_	_
2	在	_	p	_	_	3	ADV	_	_
3	杭州	_	ns	_	_	4	ADV	_	_
4	读书	_	v	_	_	0	ROOT	_	_
```

### 4.2 训练方案

**框架**: [supar](https://github.com/yzhangcs/parser) (PyTorch, BiLSTM Biaffine)

**数据组成**:

| 来源 | 句数 | 标签 | 用途 |
|------|------|------|------|
| UD_Chinese-GSD | ~4K | 金标 | dev/test 锚点 |
| UD_Chinese-HK | ~1K | 金标 | dev/test 锚点 |
| DDParser 标注本机对话 | 10K-20K | 银标 | train 主体 |

**模型配置**:

| 参数 | 值 |
|------|-----|
| encoder | BiLSTM |
| hidden | 200 |
| layers | 3 |
| embed_dim | 50 |
| dropout | 0.33 |
| epochs | 50 (early stop) |
| batch_size | 32 |

**预期指标**:
- LAS (标注依存): ≥80 (金标测试集)
- UAS (未标注依存): ≥85 (金标测试集)

### 4.3 ONNX 导出

```python
torch.onnx.export(
    model,
    (input_ids, pos_ids, char_ids),
    "dep_parser.onnx",
    input_names=["input_ids", "pos_ids", "char_ids"],
    output_names=["head_logits", "label_logits"],
    dynamic_axes={"input_ids": {0: "batch", 1: "seq"}},
)
```

模型包结构：

```
dep_parser.onnx          # ~15MB
vocab.json               # token → id 映射
pos_vocab.json           # POS tag → id 映射
config.json              # 模型超参 + 版本信息
```

### 4.4 发布到 HuggingFace

```bash
huggingface-cli upload your-org/chinese-dep-parser \
    dep_parser.onnx \
    vocab.json \
    pos_vocab.json \
    config.json \
    --repo-type model
```

模型页面附加信息：
- 训练数据来源（UD + HomeAgent 对话语料）
- 模型结构与超参
- 已验证的输入/输出格式
- 降级建议

---

## 五、阶段三：Go 推理集成

### 5.1 目录结构

```
internal/nlp/
├── dep_parser.go         # ONNX 模型管理 + 推理
├── decode.go             # 依存解码算法（argmax + MST）
├── triple_extractor.go   # 句法模板 → 三元组
├── fallback.go           # gojieba POS + 模板降级
└── model.go              # 数据模型定义
```

### 5.2 模型生命周期管理

```go
// 启动时：
// 1. 检查 {dataDir}/models/dep_parser.onnx 是否存在
// 2. 不存在 → 从 HuggingFace 下载
//    GET https://huggingface.co/your-org/chinese-dep-parser/resolve/main/dep_parser.onnx
// 3. onnxruntime_go.NewDynamicAdvancedModel() 加载
// 4. 加载失败 → 启用 fallback，日志告警
// 5. 检查可选的版本更新（按 config.json 的 version 字段）
```

### 5.3 推理接口

```go
type DepParseResult struct {
    Tokens  []string    // 分词结果
    POS     []string    // 词性标签
    Heads   []int       // 每个词的父节点索引（0=ROOT）
    DepRels []string    // 依存关系标签
}

type Triple struct {
    Subject   string
    Relation  string
    Object    string
    Score     float64
}

type Extractor struct {
    parser *DepParser
    embed  *memory.StaticEmbedder
}

func (e *Extractor) Extract(text string) []Triple {
    // 1. DepParser.Parse(text) → DepParseResult
    // 2. 句法模板匹配 → 候选三元组
    // 3. 向量验证（cos(h+r, t)）→ 过滤
    // 4. 融合打分 → 输出
}
```

### 5.4 句法模板（初版）

| 模板 | 依存模式 | 先验置信度 |
|------|----------|-----------|
| SBV-VOB | `(SBV) → VOB` | 0.9 |
| SBV-IOB | `(SBV) → IOB → VOB` | 0.85 |
| ATT-VOB | `(ATT) → VOB` | 0.8 |
| SBV-POB | `(SBV) → POB` | 0.75 |
| COO 链 | 并列结构扩展 | 0.6 |

### 5.5 降级策略

| 故障场景 | 行为 |
|---------|------|
| ONNX 模型文件不存在 | 启动时下载，下载失败则进 fallback |
| onnxruntime_go 加载失败 | 日志告警 + 进 fallback |
| 单句推理超时/panic | 返回空三元组，不中断流水线 |
| 全部正常 | 优先 ONNX 模式 |

Fallback 模式沿用现有的 gojieba POS 局部模板提取（POS 序列匹配），不需要额外依赖。

---

## 六、阶段四：集成到现有蒸馏管线

### 6.1 修改点

| 文件 | 改动 |
|------|------|
| `internal/agent/core/distill.go` | `docToTriples()` 改用新 Extractor |
| `internal/memory/pipeline/pipeline.go` | `extractKeyTriples()` 替换为新 Extract |
| `internal/agent/core/process.go` | 系统提示注入时走新提取器（可选） |

### 6.2 蒸馏管线的三个触发点

```
1. 实时 (process.go): 用户输入经过 NLU 时，即时提取三元组写入 Graph
2. 周期蒸馏 (pipeline.go): 10 分钟心跳，批量处理 7天前的原始记录
3. 冷文档归档 (distill.go): 72h 未访问的文档 → docToTriples
```

新的 `Extractor` 在三个触发点统一使用，上游调用方无需感知底层是 ONNX 还是 fallback。

---

## 七、时间线

| 阶段 | 内容 | 预估工时 |
|------|------|----------|
| 一 | 数据导出 + DDParser 快速验证 | 1 天 |
| 二 | 训练流水线搭建 + v0.1 训练 + ONNX 导出 | 2 天 |
| 三 | Go 推理集成 + 句法模板 | 2 天 |
| 四 | 蒸馏管线接入 + 降级测试 | 1 天 |
| 五 | HuggingFace 发布 + 文档 + 回测 | 1 天 |
| **总计** | | **7 天** |

---

## 八、模型维护策略

### 8.1 版本迭代

| 版本 | 触发条件 | 训练数据 |
|------|---------|---------|
| v0.1 | 初始版 | UD + 10K 本机对话 |
| v0.2 | 累计 50K 新对话 | 增量合并 retrain |
| v1.0 | 对话域 LAS ≥85 | 全量 + 人工抽检 |

### 8.2 更新机制

```
HomeAgent 启动 → 检查 HuggingFace 模型版本
    ├── 本地版本 < 远端版本 → 后台下载新模型，下次重启生效
    └── 本地版本 == 远端版本 → 跳过
```

通过 `config.json` 中的 `version` 字段比对，采用先下载后原子替换的策略。

### 8.3 回滚

```
/data/newqqagent/models/
├── dep_parser.onnx       # 当前版本 (symlink)
├── dep_parser_v0.1.onnx  # 历史版本
└── dep_parser_v0.2.onnx  # 历史版本
```

启动失败时自动 rollback 到上一个可用版本。

---

## 九、与现有系统的交互

### 9.1 Context 向量层关联

之前讨论的 **TF-IDF 加权词向量平均** 与三元组提取是两条独立优化线路：

```
三元组提取 (本计划)          Context 向量 (之前已改完)
─────────────────            ────────────────────────
句法定界 + 向量验义          jieba 精确模式 + TF-IDF 加权
输出: (sub, rel, obj)        输出: 300d 语义向量
用于: GraphDB 写入            用于: Context 裁剪评分
```

两者共享 gojieba 分词结果和 StaticEmbedder 词向量，但不直接耦合。

### 9.2 向量验证层的复用

`StaticEmbedder` 的 `Vectorize()` 可以直接用于 TransE 验证：
```go
h := embed.Vectorize(subject)
r := embed.Vectorize(relation)  // 谓语子树语义中心
t := embed.Vectorize(object)
score := CosineSimilarity(h + r, t)
```

无需额外加载词向量模型，与 Context 层在同一向量空间。

---

## 十、风险与缓解

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| DDParser 标注质量低 | 中 | 模型学偏 | 混入 UD 金标 + 抽检 500 条先行验证 |
| 对话语料句式单一 | 中 | 泛化差 | 数据增强（依存树扰动/回译） |
| onnxruntime_go 兼容问题 | 低 | Go 侧无法加载 | fallback 模式独立完整，不影响已有功能 |
| 模型体积大 | 低 | 启动慢/占用高 | ~15MB ONNX，可接受 |
| HuggingFace 下载失败 | 低 | 首次启动受阻 | 支持本地预下载 + fallback |
