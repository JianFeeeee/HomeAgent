# 三元组蒸馏接入小 LLM：设计与实现边界

> 状态：设计中，待实现
> 依据：2026-10-02 实测（本机 ha-c 生产库、qwen3:1.7b、60 条真实语料）

## 一、要解决的实测问题

### 1.1 自动蒸馏产出为零

```
extractKeyTriples()  ← internal/memory/pipeline/pipeline.go:398
  └─ nlp.NewExtractor(nil)   defaultParser 实测为 nil
     └─ extractFromPOS       gojieba Tag() + posTemplates 匹配
        └─ 实测 4 条真实语料 → 0 条三元组
```

**库里 188 个实体、143 条关系全部不是这条路写的。** 证据：

- 95/143 条关系是 `交接文档整理`/`发布窗口与回滚`/`发布评审` 这类语义化类型，
  而 `extractFromPOS` 只能产出「是/有/被标为」这类模板词。
- 长实体是原句的**改写压缩版**：
  ```
  实体: 第112批周四凌晨2点·停机4分·回滚v2.29.5·灰度10%观察48分后放50%
  原句: 第112批，30 日：发布窗口定在周四凌晨 2 点，预计停机 4 分钟；回滚版本锁定为 v2.29.5；…
  ```
  jieba 模板做不出改写。

**结论：记忆实际由模型主动调 `memory_commit` 写入，模型把看到的整句改写后塞进去。**

### 1.2 根因在工具 description

`memory_commit` 的 description（internal/memory/indexer.go:462）讲的是
`media_digests` 和 `scene` 怎么填，**完全没要求实体名原子化**。
模型写出 43–50 字符的整句实体是 description 的必然结果，不是模型的错。

### 1.3 后果：跨维度检索全失效

严格判据（生产库 188 实体 + 同域硬负样本）：

```
基线 jieba + LIKE          0/5     针排名 5~6
问「第181批的值班手册是第几版」→ 完全答不出
```

原因：库里实体是**一个实体塞多个事实**的复合值——
`admin服务端口8861·billing服务端口8499·oauth服务端口8271`。
问 billing 端口时，针 `billing服务端口8499` 被这种大杂烩压在后面。

**换任何向量模型都救不了，这是入库粒度问题。**

## 二、为什么用小 LLM 而不用现有 nlp 抽取器

实测排除了两条路：

| 方案 | 判据 | 结果 |
|---|---|---|
| 规则拆分（按 `·`/`，`） | 跨维度定位 | **0/20** — 拆开后主语和值变成互不相关的实体，答案丢失 |
| `extractFromPOS`（jieba 模板） | 真实语料产出 | **0 条** |
| 小 LLM 拆分（qwen3:1.7b, think=False） | 值是否原样 | **0% 幻觉**（待修正版复核） |

规则拆分失败的机制值得记住：`值班手册第4版` 和 `第183批` 原本在**同一句话**里
（隐含"这批的值班手册是第4版"），拆开后它们是两个独立实体，图中无边可循。
**拆分必须保留主语锚。**

## 三、架构位置

```
Distiller.distillOnce()  30 分钟定时
  └─ distillBatch()  ≤50 条
     └─ extractKeyTriples(user, assistant)   ← 改这里
        ├─ 现状：nlp.NewExtractor(nil) → 0 条
        └─ 改后：LLM 拆分 → []memory.Triple
              └─ db.Commit(triples, sessionID, 0)
```

`Distiller` 已有的基础设施：
- `go d.distillLoop()`（pipeline.go:77）定时循环
- `distillOnce` 失败回退重试（pipeline.go:270）
- `removeRawRecords` 成功后清盘（pipeline.go:347）
- `flush`/`loadExisting` 进程重启后不重蒸

**按需加载的天然位置就在这里**：每 tick 先看有没有待蒸馏记录，
0 条就不碰模型，有记录才加载、拆完释放。

## 四、要改的四处（按依赖顺序）

### 4.1 `providers/qwen3vl`：加生成侧图与推理

**现状**：`Transformer.onnx` 的输出被硬编码为 `Shape{1, Dimension}`
（embedder.go:441），last-token 池化烘焙在图里。

导出脚本的理由是对的：
> 池化放在图里（而不是 Go）是有意的：last-token 的位置由 attention_mask 决定，
> 一旦 Go 侧算错位置就会静默取到 padding 的 hidden，而向量照样归一化、照样能比余弦
> ——那种错误只能靠与参考向量对比才能发现。

**这个判断要保留**（静默错误比崩溃更糟），所以**不能改这张图**，
只能**再导一张**：

| 图 | 输出 | 用途 | 复用 |
|---|---|---|---|
| `Transformer.onnx`（现有） | `[1, dim]`（池化后） | 检索向量 | — |
| `LMHead.onnx`（新增） | `[1, seq, vocab]` | 三元组生成 | **TokenEmbedding.onnx 共享** |

`lm_head` 靠 tied embeddings（实测 625 张量里没有独立 lm_head，
`tie_word_embeddings=True`），所以 `LMHead.onnx` 只是
`matmul(norm(hidden), embed_tokens^T)` + 可选采样，**几乎不占空间**。

已有可直接复用的（providers/qwen3vl 里全部现成）：
- `Tokenizer`（tokenizer.go:152 的 chat template 三段拼接）
- `rotary()` / `causalMask()`（embedder.go:458/490）
- M-RoPE 三分段位置（model_input.go:44）
- `close sync.Once` 生命周期（embedder.go:70）

**要加的**：
- `Generate(ctx, prompt, opts)` — 循环自回归，KV cache 逐步追加
- `lmHead *ort.DynamicAdvancedSession` — 与 `token`/`vision` 同样的懒加载
- 采样（temperature=0 时退化为 argmax，蒸馏场景够用）

### 4.2 `pkg/embedding`：契约要扩，还是另起一个 SPI

**现状**：`pkg/embedding.Provider` 只有 `Embed` + `Info` + `Close`，
能力声明是数据（`Info.Modalities`）。

**不能把生成塞进这个接口**——embedding 是"输入→向量"，生成是"输入→文本"，
塞进去会让所有 provider 都得实现生成。

**建议**：新增 `pkg/generation` SPI，与 `pkg/embedding` 并列，
`providers/qwen3vl` 同时实现两者（共享同一份权重与 Runtime 生命周期）。

**不选**「在 embedding SPI 上加可选接口 + 类型断言」——
那会让调用方写 `if g, ok := p.(Generator); ok`，
每个调用点都要判一次，而蒸馏只有一个调用点需要它。

### 4.3 `pipeline.go`：换掉 extractKeyTriples 的实现

**保留 jieba 版**作为降级路径（ONNX 缺失、加载失败、LLM 超时时用它，
哪怕产出 0 条也比整个蒸馏停摆好）。要能区分「LLM 拆了 0 条」和
「LLM 没跑」——前者是正常结果，后者才回退。

**批量而非逐条**：实测 23.4s/条 × 50 条 = 19.5 分钟，几乎吃掉整个
`DistillInterval`（30 分钟）。必须一次调用处理整批，
并对 `num_predict` 设上限防止失控。

### 4.4 拆分输出的后处理

**必须做的两件事**（实验暴露的）：

1. **值必须原样来自原文** — 出现不在原文里的值就丢弃该条。
   这是唯一能挡住幻觉的闸门，因为生成侧没有向量可验。
2. **维度名归一** — 实测同义异名严重（`停机`/`停机时间`、
   `回滚`/`回滚版本`），不归一就没法建边。
   受控词表从现有数据归纳（跑分语料里出现 29 个维度）。

**不做**：不试图让 LLM 自由命名维度后靠后处理纠正——那是把不确定性留在系统里。

## 五、待验证（实验进行中）

`bb69252e6`：qwen3:1.7b 拆 60 条真实语料，三个独立判据。
**上一版的 20/20 是循环验证**（探针答案取自 LLM 输出再查同一份输出，
temperature=0 下必然全中），已作废。

| 判据 | 内容 | 决定什么 |
|---|---|---|
| Q1 原子覆盖率 | vs 强分隔符切出的原子 | 有没有丢信息 |
| Q2 值是否原样 | 值必须出现在原文中 | 有没有编造 |
| **Q3 跨维度定位** | 手写问句，与 LLM 输出无关 | **拆分是否值得做** |

Q3 若不显著高于现状的 0/20，本设计不成立，应回头修 `memory_commit`
的 description 让模型自己拆，而不是加一层 LLM 蒸馏。

## 六、明确不做

- **不改 `Transformer.onnx`**：它把池化烘焙进图是正确设计（防 Go 侧静默错位）。
- **不把生成塞进 `pkg/embedding`**：契约不同，见 4.2。
- **不常驻加载模型**：靠 30 分钟定时 + 有记录才加载。
- **不删 jieba**：降级路径要用；`fallbackParser` 也依赖它做 POS。
  但实测它的 135MB 常驻（4.8MB 词典 → 135MB 前缀树）是真实成本，
  等生成侧稳定后再单独评估。
- **不迁移现有向量维度**：embedding 侧是 2048 维，与 chineseclip 的 512 不同，
  但这是 embedding provider 之间的事，与本设计无关。
