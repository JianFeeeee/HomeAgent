[English](../en/ARCHITECTURE.md) | **中文**

# HomeAgent 架构

## 体系结构原则

HomeAgent 的认知架构由三个核心子系统构成：事件循环（eventLoop）、上下文窗口（RelevanceContext）与阶段管道（StageHost）。三者共同组成编排框架。LLM 在其中作为可调度的推理执行单元运行；认知连续性由事件循环、上下文窗口与阶段管道维持。

**事件循环（eventLoop）** 是一个三路 select 循环：`a.io.InputChan()` 接收外部用户输入并分发至 `processTextInput` / `processMediaInput`；`a.selfInputCh` 接收内部系统任务（如记忆合并、蒸馏回调），以 `_consolidation_` 输出通道标识区分，走 `processConsolidation` 路径；`a.ctx.Done()` 接受关闭信号。与之并行运行的 `interceptLoop` 协程独立监听 `a.io.InputInterruptChan()`，收到高优先级中断时先取消当前 LLM HTTP 请求（`a.cancelLLM()`），再将事件写入 `a.interceptCh`——该通道在 `process()` 每次 LLM 调用前由 `drainInterrupts()` 非阻塞排空，以 `[打断消息]` 格式注入消息历史。三条中断投递路径各具语义：`cancelLLM` 终结当前 HTTP 请求，`interceptCh` 在下一轮 LLM 调用前注入文本，`InjectInput` 在 eventLoop 空闲时触发新一轮处理。

**阶段管道（StageHost）** 管理两类注册：工具定义（ToolDef）与阶段处理器（StageHandler）。ToolDef 包含 `NoMemory bool` 和 `Cleaner func(string) string` 两个可选的记忆控制字段：`NoMemory=true` 时工具输出不参与向量化/jieba/蒸馏计算（原文保留）；`Cleaner` 在输出进入计算层前执行过滤（如提取 JSON 的 `content` 字段）。两者均不修改原文，只影响计算层输入。`RegisterTool` 拒绝同名注册，推断工具所属插件名，并维护工具到插件的映射表 `toolPlugins`。`RegisterStage` 将处理器追加至对应阶段的处理器列表。触发阶段执行时（`RunStage`），**所有已注册处理器通过 goroutine 并行执行**，共享同一 `*StageContext` 实例（通过 `sync.RWMutex` 保护并发访问）。单个处理器的 panic 被独立恢复，不影响其他处理器。短路语义通过检查 `ctx.Response != nil` 实现——任一阶段处理器可设置此值提前终止当前链路。工具执行 `ExecuteTool` 内置 panic 恢复与栈追踪记录。`UnregisterPluginTools` 在插件热重载时移除对应工具集。

**上下文窗口（RelevanceContext）** 维护一个按时间排序的事件列表。`Append` 在录入前经 `textForVector` 聚合工具输出：按 `NoMemory` 跳过、`Cleaner` 过滤（插件为各自工具注册的清洗函数，如 QQ 外置插件剥离工具调用模板），最后经 `CleanText` 做基本空白规范化，再通过三分支向量策略（agent 事件用 Response，用户事件用 Input，cold_storage 用 Input+Response）计算嵌入向量。`Prune` 在事件数超过 `topK` 时触发，**无条件保护最近 10 条事件不被裁剪**（recency bias），对剩余候选事件计算与当前输入的 CosineSimilarity，按评分降序保留 `topK - 10` 条（下限为 0），之后按时间戳重排序。裁剪出的事件中，过滤掉 `agentcli` 和 `terminal` 来源后，其余通过 `docStore.ContextToDoc` 归档至 Document 层，保留原始时间戳。持久化采用 5 秒防抖写入磁盘 JSON 文件。

**工具定义聚合自五个来源**：IOManager 注册的插件工具；StageHost 注册的 SDK 插件工具；Indexer 提供的记忆索引工具；内置条件工具（依据 memory / knowledge / docStore / social / pluginReg / providerManager 等模块的非空状态选择性添加，包括记忆操作、知识检索、文档查询、社交网络、插件重载、子代理生成、输出通道工具、LLM 源切换等）；以及按 `pendingMedia` 状态添加的媒体处理工具。`buildToolDefs()` 在每次 process 周期中重新聚合所有这些来源。

**Provider 调用采用有序降级策略**：`ProviderManager.OrderedProviders()` 返回按注册顺序排列的 provider 列表。`process()` 内循环遍历该列表依次尝试 `Chat()` 调用。401/403 状态码将对应 provider 标记为永久不可用；其他错误类型同样标记不可用但容忍度更高。全部 provider 失败时返回错误返回调用方。若调用因 context 取消而中断且 Agent 仍在运行，则重试（仅当处理非 consolidation 路径时）。

**记忆体系采用三级存储层级结构（Context → Document → Graph），按访问局部性与持久化需求进行数据分置**：Context 层为高速易失工作窗口，使用 StaticEmbedder（预训练词嵌入）进行语义相关性评分，以 TF-IDF 为回退策略；Document 层与 Context **共享同一 StaticEmbedder 向量空间**（agent 启动时将 embedder 注入 Document Store），使 Context 裁剪时的事件相关性评分与 Document 查询时的语义检索处于同一向量空间中，确保冷热数据之间的相似度可比——TF-IDF 仅在 embedder 未加载时作为兜底方案；Graph 层以 SQLite 为持久化载体，entities 表与 relations 表分别存储节点与有向边，支持 BFS 遍历召回。三级之间定义数据迁移策略：低分事件从 Context 下沉至 Document（以归档时相同的 embedder 向量化写入），冷文档经 72 小时未访问阈值判定后通过 `docToTriples` 蒸馏为三元组写入 Graph。Indexer 通过双路召回（实体向量相似度搜索 + jieba 关键词提取）构建 Graph 查询种子，结合 `MarkRecalled` 机制避免已被工具调用取回的实体重复注入系统提示。

**核心域与应用域的职责域分离**是一项系统级架构决策：内核的责任边界限定在 LLM 编排、记忆管理与知识检索三个维度内，不直接承载任何 IO 操作；所有外部交互通过插件域接入。该分离将核心域的复杂度控制在可验证范围内，同时赋予插件域独立的演化自由度——后者可独立开发、独立发布、热加载，且不对核心域的稳定性构成直接影响。

## 消息处理流程

### 完整链路

```
外部输入（通过插件 InjectInput）
        │
        ▼
eventLoop() → processTextInput()
        │
        ├── on_input stage          插件可拦截/改写/短路
        ├── Context.Append          记录到上下文窗口
        ├── Context.Prune           低相关性事件归档到 Document
        ├── buildMemoryContext()    Indexer 召回 → GraphDB BFS 遍历
        │
        ├── pre_action stage        插件可注入 system 消息
        │
        ├── [工具循环] process()
        │   ├── buildSystemPrompt   人格 + 记忆 + 知识 + 上下文
        │   ├── buildToolDefs       内置工具 + 插件工具
        │   ├── provider.Chat()     LLM 调用
        │   ├── post_action stage   插件可见 LLM 输出 + 工具列表
        │   ├── 有工具?
        │   │   ├── before_toolcall 插件可拒绝/改参
        │   │   ├── executeToolCall 路由到插件/内置
        │   │   ├── after_toolcall  插件可改结果
        │   │   └── → 回到 post_action
        │   └── 无工具 → 退出循环
        │
        ├── Context.Append(response)
        ├── before_output stage      插件可改最终文本
        ├── emitResponse()          通过 output_send 发送
        └── after_output stage       插件只读，收尾
```

代码：`internal/agent/core/process.go` — `process()` 是工具循环主体

### 7 个阶段钩子

| 阶段 | 触发时机 | 插件可做 |
|------|----------|----------|
| `on_input` | 消息到 Agent，零处理 | 黑名单/限流/短路回复 |
| `pre_action` | 上下文就绪，LLM 调用前 | 注入外部数据到 context |
| `post_action` | LLM 返回文本+工具列表 | 敏感词过滤/强制 redirect |
| `before_toolcall` | 单个工具执行前 | 审计/拒绝/改参 |
| `after_toolcall` | 单个工具执行完 | 脱敏/排序结果 |
| `before_output` | 最终文本就绪，发送前 | 格式适配 |
| `after_output` | 已发送 | 统计日志 |

代码：`internal/agent/core/stages.go` — `StageHost` 编排

### 循环规则

`post_action → [before_toolcall → 执行 → after_toolcall] → post_action` 构成内循环。
退出条件：LLM 无工具调用 / 全部被拒绝 / 超上限。

### 短路规则

任意阶段设 `ctx.Response` 即跳到 `after_output`。


## 三层记忆

### 记忆流转

```
① Context (工作窗口)
   RelevanceContext — 内存 events[] + JSON持久化
   Append: 每次输入, CleanText → 三分支向量(textForVector)
            agent事件→Response, 用户事件→Input, cold_storage→Input+Response
            StaticEmbedder 预训练词嵌入 / TF-IDF 回退
   Prune:  StaticEmbedder CosineSimilarity, 保留 topK + 最近10条
       ├── 保留 → timeline → 按时间排序 → system prompt
       └── 低分 → Document 层归档 (原始时间戳)
   Save: 5s debounce 写盘

         ↓ Prune 归档                          ↑ LLM 主动召回

② Document (文件记忆)
   DocStore — JSON文件 + 与 Context 共享的 StaticEmbedder 向量空间（兜底: TF-IDF InvertedIndex）
   写入: Prune归档 / doc_commit / Graph快照(syncGraphToDocs)
   读取:
       ├── 自动注入: Query(input, top3) → 同一向量空间下相似度摘要 → 【相关记忆文档】→ system prompt (只读)
       └── LLM主动:  doc_query → Consume(读取并删除)
                         → 逐条 context.Append{Timestamp: d.CreatedAt, Source: "cold_storage"}
                         → 文档以原始时间戳写入 context 时间线, 从 docStore 删除
   冷化: FindColdDocs(72h, ≤2次访问) → docToTriples → Graph

         ↓ 冷文档蒸馏                           ↑ 自动召回

③ Graph (图数据库)
   SQLite — entities + relations 表
   写入: memory_commit / 冷文档蒸馏 / Pipeline 规则蒸馏 / memory_merge
   读取:
       ├── 自动召回: Indexer.BuildContext(input)
       │     → CleanText → 向量实体搜索 + jieba关键词 → SQLite LIKE + BFS depth=2
       │     → 【记忆索引】→ system prompt
       └── LLM主动: memory_recall / memory_merge / memory_purge / memory_edit / memory_delete_entity
   Social: person_query / set_trait / relate (包装 GraphDB)

④ 四个独立心跳循环（各自独立的 ticker 和配置间隔）
   distillLoop  (distillInterval,  默认30m): 上下文裁剪 — Context.Prune → Document
   archiveLoop (archiveInterval, 默认60m): 冷文档归档 — docToTriples → GraphDB
   mergeLoop   (mergeInterval,   默认120m): 实体合并检测 — 相似度 → LLM 裁决
   reviewLoop  (reviewInterval,  默认120m): 关系复审 — SentenceRef 回溯 → LLM 修正

⑤ Pipeline 规则蒸馏器 (每10min心跳)
   distillOnce → 正则匹配个人信息:
     我叫X / 我住在X / 我喜欢X / 我X岁 / 我的工作是X
     → 三元组 → GraphDB.Commit
```

### 向量化：统一多模态空间（主）→ 词嵌入 → TF-IDF（回退）

向量化按可用性分三层降级，**每一层缺位都明确报错，不静默假装成功**：

**① 统一多模态空间（主路径，v1.2.0 起）**
文本与图像共用**同一模型、同一维度、同一指纹**（默认 `chineseclip`：512 维、Apache-2.0、中文原生；
亦可选 `qwen3vl` 或外部 `http` provider）。provider 经 `pkg/embedding` 公共 SPI 注册，
**内核不硬编码任何模型**。向量与指纹一起持久化（`dense_fp` / `vec_model`），
与当前指纹不一致即触发重算；融合时只接受**同指纹且同维度**的块向量
（跨坐标系的向量混进去会算出两边都不像的方向）。

**② 词嵌入（文本兜底）** — `StaticEmbedder`（`internal/memory/static_embedder.go`）：

**模型来源**（词对齐 300 维）
- 模型来源：ConceptNet Numberbatch（77 语对齐）/ fastText 中文 / fastText 英文
- 通过 `core.agent.embedding_model_path` 配置（逗号分隔多模型）
- 路径名含 `numberbatch` → 自动下载 ConceptNet，含 `cc.zh.` → fastText 中文，含 `cc.en.` → fastText 英文
- 不匹配则默认 ConceptNet
- **前处理**：`textForVector` 聚合工具输出时逐一应用各工具的 `Cleaner`（由插件注册），最后经 `CleanText` 做基本空白规范化
- **三分支向量来源**：agent→Response，用户→Input，cold_storage→Input+Response
- **TF-IDF 回退**：模型下载失败或未配置时自动回退词袋 TF-IDF，服务不中断

| 位置 | 文件 | 用途 | 算法 |
|------|------|------|------|
| Context Prune | `context.go:155` | 裁剪低相关性上下文事件 | VectorizeClean → CosineSimilarity(queryVec, evt.Vector) |
| DocStore Query | `document.go:206` | 文档记忆召回 | StaticEmbedder.Vectorize（首选）/ TF-IDF（兜底）→ vec.Search |
| Indexer 实体搜索 | `indexer.go:96+111` | Graph实体召回 | 向量实体搜索 + jieba关键词 → SQLite LIKE + BFS |
| 实体相似度检测 | `distill.go` | Graph中相似实体 | Bigram Jaccard (>0.75 → consolidation) |

### Context 层

`internal/agent/core/context.go` — `RelevanceContext`
- 维护最近事件列表，每次 Append/Prune 写入 JSON 防丢
- 向量化前统一经 `CleanText` 去模版噪声
- 三分支 `textForVector`：agent 事件用 Response、用户事件用 Input、cold_storage 用 Input+Response
- 预训练词嵌入 `StaticEmbedder` → CosineSimilarity，模型不可用时自动回退 TF-IDF
- 保护最近 10 条记录免于淘汰，超出部分按相关性排序归档到文档记忆
- 归档事件以原始时间戳写入文档记忆，后续 `doc_query` 召回时按原始时间戳插回时序

### Document 层

`internal/memory/document/document.go` — `Store`
- 消费即删模式：`doc_query` 检索到后删除
- 双路召回：与 Context 共享的 StaticEmbedder 语义向量搜索 + jieba 关键词提取（模型未加载时回退 char-bigram TF-IDF）

### Graph 层

`internal/memory/graph.go` — `GraphDB`
- SQLite WAL 模式，两张表（驱动：mattn/go-sqlite3，CGo）
- `Commit(triples)` — UPSERT entities + INSERT relations
- `Recall(keywords, depth)` — 关键词 LIKE 搜索 + BFS 遍历

### 记忆工具（LLM 可直接调用）

| 工具 | 作用 |
|------|------|
| `memory_recall` | 从 Graph 召回 |
| `memory_commit` | 写入 Graph 三元组 |
| `memory_merge` | 合并两个实体节点 |
| `memory_purge` | 删除指定实体 |
| `memory_edit` | 编辑已有实体/关系 |
| `memory_delete_entity` | 删除实体及其所有关系 |
| `memory_introspect` | 查看记忆统计 |
| `doc_query` | 从 Document 搜索 |
| `doc_commit` | 写入 Document |

`memory_commit` 与 `doc_commit` 自 v1.1.1 起接受 `media_digests`，并由内核把
`[<mime> <短digest>] <描述>` 标记补进句子/正文——**标记由内核拼，模型只给 digest**。
要求调用方知道格式，等于让一个拼写错误静默切断引用绑定而全链路无人报错。
`memory_commit` 同时新增 `sentence_text`：媒体引用挂在句子上，没有句子就无处可挂。

### 媒体记忆（v1.2.0 起：一等记忆块）

媒体不是外挂内容，而是**记忆的一等节点**：`internal/memory/media/` 是内容寻址仓储（CAS），
图数据库里的 block 节点携带它的 digest 与向量，结构边（如 `sentence --contains--> block`）表达归属。

| 关注点 | 做法 | 为何 |
|---|---|---|
| 寻址 | sha256 digest；元数据在 SQLite，blob 在磁盘（`blobs/<前2位>/<其余>` 两级分桶） | 相同字节只存一份；元数据要可查询，blob 不该进数据库 |
| 完整性 | 每次 `Get` 重校 digest | 磁盘损坏时静默返回脏数据比报错危险得多 |
| 写入原子性 | `.tmp` + rename | 半个文件被当成完整内容会永久污染那个 digest |
| 检索 | 块携带**自己的多模态向量与指纹**，直接参与向量检索 | 不需要描述文本做中介 |
| 生命周期 | **无独立 GC、无引用计数、无 keep-set**；删除块即删内容 | 媒体是记忆节点，不是需要保活的缓存 |

**不再有描述式索引**：旧实现在正文里写 `[<mime> <短digest>] <描述>` 标记，并把描述文本当作语义记忆
（检索靠描述）。该机制已在 v1.2.0 整体拆除：描述是模型生成的二手信息，
检索“别人转述的图片”不如检索图片自己的向量。现在图片只按自己的统一空间向量被检索，
正文里不再有 media marker。

**跨空间向量迁移**：媒体行的向量带 `vec_model`（空间指纹）。启动时
`reembedStaleMedia()` 把 `vec_model` 为空（从未嵌入）或与当前空间不一致（换过模型/维度）的行
批量重算并**写回库**；模态不在本空间覆盖范围时返回 `ErrModalityUnsupported`，
**绝不拿别的模型的向量顶替**。

媒体存储全程可选：`core.memory.media.enabled=false` 或未配置时，整条链路静默退化为纯文本行为，
不报错不 panic。

### 其他记忆层

- **Social** (`internal/memory/social/social.go`) — 人格特质和关系网，包装 GraphDB 实体类型
- **Text Memory** (`internal/memory/text/text.go`) — 原始对话 JSONL 日志，轮转策略
- **Memory Indexer** (`internal/memory/indexer.go`) — 实体向量化 + jieba 关键词提取，自动注入 system prompt

### 蒸馏管道

`internal/memory/pipeline/pipeline.go`
- 10 分钟 tick，7 天保留
- 规则提取三元组（name / location / likes / age / job 模式）
- 写入 GraphDB

### 上下文剪枝

```
四个独立心跳循环（各自可配置间隔）:
  ├── distillLoop  (distillInterval,  默认30m)
  │   └── distillContext() — Context.Prune → Document
  ├── archiveLoop (archiveInterval, 默认60m)
  │   └── archiveColdDocs() — 冷文档 → docToTriples → GraphDB
  ├── mergeLoop   (mergeInterval,   默认120m)
  │   └── detectEntityMerge() — 实体相似度检测 → LLM 裁决
  └── reviewLoop  (reviewInterval,  默认120m)
      └── reviewRelations() — 关系复审 → SentenceRef 回溯 → LLM 修正
```

实体冲突检测启发式（bigram Jaccard > 0.75），走 `selfInputCh` 内部通道，LLM 最终判断是否合并。


## 知识库

`internal/knowledge/knowledge.go`
- 文件目录 `knowledge/<name>/content.md`
- 独立 TF-IDF 索引，与记忆系统不冲突
- `knowledge_search` / `knowledge_create` / `knowledge_list`


## Provider 与 Lua 适配层

```
Agent
  │
  ▼
Provider 接口 (Name / Chat / ChatStream)
  │
  └── LuaAdaptedProvider (唯一实现)
      ├── 序列化 CompletionRequest → JSON
      ├── adapter.transform_request() → API 格式
      ├── HTTP 请求 + adapter.headers
      ├── adapter.transform_response() → 统一格式
      └── 反序列化
```

代码：`internal/agent/api/provider.go`

ProviderManager 管理多个源，按注册顺序 fallback。Lua 适配器位于 `internal/lua/adapters/`，每个 `.lua` 脚本定义 `transform_request` / `transform_response` / `transform_stream_chunk`。

VM 内置 `json.encode` / `json.decode` / `log` / `http_get` / `http_post`。


## 插件系统

### 四种加载方式

| 方式 | 注册机制 | 编译 | 用途 |
|------|----------|------|------|
| 内置插件 | `init()` → `RegisterFactory` | `internal/plugins/` 编译进内核 | webui/cli/timer/mcp 等 |
| 外部子进程插件 | 握手 + stdio JSON-RPC 反向注册 | `hmapdev build` → `plugin.bin`（普通 Go 二进制） | qq/browser/files 等 |
| Lua 脚本插件 | 执行 `main.lua` 注册工具 | 无需编译，重启/重载生效 | luademo 等 |
| SKILL 插件 | 解析 `SKILL.md` | Markdown 定义 | clawhubadapter 兼容加载 |

内置插件注册：`internal/plugins/all.go` 空白导入 → 各插件 `init()` → `Registry.Load()` 扫描目录匹配工厂。

外部插件加载（v1.0.0 起）：`internal/plugin/dynamic_proc.go` → `exec.Command(plugin.bin)`
→ 继承共享段 fd → 握手（比对 protocol 版本）→ `plugin.init` → `plugin.start`
（插件在此期间反向注册工具/阶段/通道）。
**C ABI 通道（`-buildmode=c-shared` + bridge）已在 v1.0.0 整体删除**——
旧的 `plugin.Open` 路径缓存绕行、SHA256 临时路径复制等手法随之退场。

Lua 脚本插件加载：`internal/plugin/` → gopher-lua 解释器执行 `main.lua`（加载期 `sdk.register_*` 仅暂存 handler），`Start()` 时替换为真实 SDK 实现并批量注册。脚本只在加载时读取一次，运行期通过回调执行。

### 内置插件 vs 外部插件

| 维度 | 内置插件 | 外部插件 |
|------|----------|----------|
| 注册方式 | `init()` 调用 `plugin.RegisterFactory(name, factory)` | 实现 `NewPluginFactory(name, config) (sdk.Plugin, error)` 入口函数 |
| 编译方式 | 编译进 `homed` 二进制，无需独立编译 | 通过 `hmapdev build` 编译为 `plugin.bin`（普通 Go 二进制，零 cgo），内核 spawn 为子进程 |
| 分发方式 | 随内核分发，不可独立安装/卸载 | `.hmap` 包（ZIP 归档），通过 WebUI 或 pluginmgr API 安装 |
| 元数据 | 通过 `plugin.RegisterPluginMeta()` 注册显示名 | `plugin.json` manifest 文件（name, version, entry, platforms, capabilities 等） |
| 插件目录 | 无独立目录，编译进二进制 | `plugins/<name>/` 独立目录，包含 `plugin.json` + `plugin.bin` |
| SDK 权限 | 完整 PluginSDK（SocialAPI 读写、Publish 事件） | 收窄的 `procCore` 能力面 + manifest capabilities 声明 + RPC 边界拒绝 |
| 生命周期 | 随内核启动/停止，不可单独热重载 | 独立进程，换 `plugin.bin` 即生效的真热重载（ReloadOne）和禁用/启用 |
| 崩溃恢复 | 无独立恢复机制 | 进程级隔离：崩溃不影响内核，内核摘除其注册面后按退避自动重启（`SetAutoRestart(false)` 可关） |

两者的联系：
- 内置插件的工厂函数 `RegisterFactory` 与外部插件的 `NewPluginFactory` 共用同一个 `NativeFactory` 类型签名
- `Registry.Load()` 统一处理两者的加载：先查工厂表（内置），无工厂则按 manifest 的 `entry` 分派到 proc / lua / skill 通道
- 两者使用相同的 `Plugin` 接口和公开 SDK API，工具注册、阶段钩子、输出通道等完全一致
- 两者共享同一个工具注册表（`StageHost`），LLM 调用时无差别

### 子进程插件的三个通信面（v1.0.0）

| 面 | 机制 | 为何这么选 |
|---|---|---|
| 控制面 | stdio JSON-RPC（NDJSON 帧），55 个 `core.*` method | 进程边界即 ABI 边界，无需维护三套平台特定的动态库加载代码 |
| 数据面 | 共享内存段，**全部子进程共用一块** | 每插件一段会让「内核 ctx → 段 → 插件改 → 回读 ctx」在多插件下退化成副本模型，lost update 原样复现 |
| 通知面 | 事件环 + 平台通知（Linux eventfd / macOS pipe / Windows Event） | 内核发事件绕不等消费者，流式输出逐 token 发布时任何等待都会造成卡顿 |

v1.1.1 新增 4 个 method（51 → 55）：`doc.insertWithMedia`、`io.injectMedia`、
`io.injectMediaSync`、`io.injectInterruptMedia`。**媒体块走 JSON 而非共享段二进制通道**——
data URL 本身已是 base64 文本，包进二进制传输省不了空间，还要跟其余 51 个 method 分道。

**子进程生命周期管理**：
- 每子进程一根专职 `waitLoop`（`cmd.Wait()` 唯一调用点）——不依赖 stdout EOF，
  因为插件 fork 的孙子进程（browser 拉 chromium、editdoc 拉 python）继承同一 stdout，
  插件本体死后 EOF 永不到来
- 集中台账 `proc.Supervisor`：握手成功即登记，退出即注销；`Host.Close()` 先 StopAll 再拆段
  （顺序反了插件还持有映射而段已 unmap，下次访问就是 SIGBUS）
- 崩溃自愈：摘注册面（工具 + stage handler + IO 通道）→ 移出注册表 → 退避重启
- Linux `Pdeathsig` 兜底 homed 被强杀时子进程不滞留为孤儿

### PluginSDK 四通道

```
插件 ──→ 核心

RegisterTool(name, fn)   ──→  buildToolDefs() / executeToolCall()
RegisterStage(stage, fn, scope...)  ──→  runStage() 在对应阶段调用（scope 控制全局/仅自己工具）
Subscribe(event, fn)      ──→  Publish() 通知所有订阅者
RegisterOutputChannel(name, caps, desc, handler) ──→ output_send__{name} 工具生成
```

`internal/sdk/` 桥接外部 SDK 接口到内核，定义完整 PluginSDK：

```go
sdk.RegisterTool(name, def, handler)
sdk.RegisterStage(stage, handler, scope...)
sdk.Publish(event)
sdk.InjectInput(source, channel, payload)
sdk.InjectInterrupt(source, channel, payload)
sdk.Memory().Recall/Commit
sdk.Knowledge().Search/Create
sdk.Settings().Get/Set/List
sdk.RegisterOutputChannel("qq", sdk.CapText|sdk.CapAudio|sdk.CapImage, "QQ消息通道，详见 output_send__qq_help", handler)

// v1.1.1 媒体接口（全部新增，无签名变更）
sdk.DocMemory().InsertWithMedia(doc, attachments)   // 带 Data 的落进 CAS，只给 Digest 的引用已有内容
sdk.InjectInputMedia(source, channel, text, blocks) // 媒体在「本轮」就发给模型
sdk.InjectInputMediaSync(...)                       // 同上并同步等回复
sdk.InjectInterruptMedia(...)                       // 带媒体的中断，可抢占当前处理
```

媒体注入与 `SetToolBlocks` 的区别：后者只能在工具处理函数内部调用，且媒体要等**下一条**
tool message 才到模型手上；前三个是插件**主动发起一轮带媒体的对话**，媒体随本轮消息发出，
并自动落进 CAS、挂上媒体记忆引用。`Triple` 与 `Doc` 相应新增 `MediaDigests`、`Attachments`。

`internal/sdk/` 是这层的桥接实现，不受公开接口冻结约束（见 `docs/git-branching.md` §六）。

### Plugin 接口

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
```


## 输出通道系统

每个输出通道生成两个工具：

| 工具 | 类型 | 作用 |
|------|------|------|
| `output_send__{name}` | function | 接受 `payload`(消息载荷)、`meta`(JSON 路由元数据)、`type`(枚举) 三个参，路由到插件 handler |
| `output_send__{name}_help` | function | 返回通道的 meta 格式和 type 枚举说明 |

能力标志位：

| 标志 | 值 | 含义 |
|------|-----|------|
| CapText | 1 | 纯文本 |
| CapFile | 2 | 文件 |
| CapImage | 4 | 图片 |
| CapAudio | 8 | 音频 |
| CapStructured | 16 | 结构化数据 |

系统提示注入：输出门控规则、多调用支持、长消息拆分。
子代理权限：`output_send__` 前缀工具允许使用。


## LLM 链事件

- 事件类型 `agent_llm_chain`，每次 LLM 轮次后发射
- 包含完整 LLM 响应（文本 + 工具调用 + 推理）
- WebUI 通过 SSE 订阅此事件实现实时显示
- 插件可通过 EventSubscriber 订阅（外部插件只读）


## 受限外部插件 API

分层架构：内部插件获得完整 PluginSDK，外部插件获得受限 SDK。

| API | 内部插件 | 外部插件 |
|-----|----------|----------|
| SocialAPI | 完整读写 | 只读（GetPerson / GetTrait / GetRelations / GetNetwork / ListPersons） |
| EventSubscriber | 订阅 + 发布 | 仅订阅（无 Publish 能力） |

扩展字段：
- Triple 扩展：Confidence、SubjectType、ObjectType
- Relation 扩展：Confidence


## 中断机制

```
interceptLoop (goroutine)
  ├── InputInterruptChan() ← 定时器/消息通知
  ├── (a) cancelLLM() → 取消 Provider HTTP 请求
  ├── (b) interceptCh → process() 轮前读 [打断消息]
  └── (c) InjectInput() → 空闲时触发新处理
```

三种投递路径：

| 路径 | 效果 | 时机 |
|------|------|------|
| cancelLLM | 取消当前 HTTP 请求 | 收到 context.Canceled |
| interceptCh | process() 中插入 `[打断消息]` | 每个 LLM call 前 |
| InjectInput | eventLoop 空闲时触发新处理 | 无进行中请求 |

代码：`internal/agent/core/eventloop.go` — `interceptLoop` / `drainInterrupts`


## 配置系统

`internal/config/registry.go` — ConfigRegistry

- SQLite 存储，`config` 表 + `config_<plugin>` 独立表
- 命名空间：`core.*` / `plugin.<name>.*`
- `RegisterDefault` 插入 ~80 个默认键（8 个 LLM 源的 seeds）
- WebUI 设置页 `/api/v1/settings` 读写


## 代码结构

```
cmd/homed/main.go          — 入口：组装所有子系统
cmd/waiter/main.go         — CLI 客户端 (Unix socket)
internal/
├── agent/
│   ├── core/              — Agent 核心 (eventLoop/process/stages/context)
│   │   └── plugin_health.go — 插件健康监控与自动重启
│   ├── api/               — Provider 接口 + LuaAdaptedProvider
│   ├── io/                — IOManager (排队/中断/输出)
│   └── personal.go        — 人格加载
├── plugin/
│   ├── registry.go        — 注册表 + 生命周期
│   ├── dynamic.go         — .so 动态加载器
│   └── manifest.go        — plugin.json 元数据
├── plugins/               — 内置插件实现
│   ├── all.go             — 空白导入
│   ├── webui/             — HTTP 服务器 + 嵌入式 SPA
│   ├── cli/               — Unix socket CLI
│   ├── timer/             — 定时器
│   ├── cmd/               — 命令执行
│   ├── mcp/               — MCP 协议
│   ├── files/             — 文件操作
│   ├── clawhubadapter/   — ClawHub 适配器（OC 插件/SKILL/JS/Python sidecar）
│   ├── agentcli/          — PTY 终端
│   ├── healthcheck/       — 健康检查
│   ├── pluginmgr/         — 插件管理器
│   └── cfgmgr/            — 配置管理
├── sdk/                   — PluginSDK 定义
│   ├── plugin.go          — Plugin 接口 + PluginSDK
│   ├── memory.go          — MemoryAPI
│   ├── knowledge.go       — KnowledgeAPI
│   ├── settings.go        — SettingsAPI
│   └── llm.go             — LLMAPI
├── memory/
│   ├── graph.go           — SQLite 图数据库
│   ├── indexer.go         — 图→向量索引
│   ├── vector/store.go    — TF-IDF 向量引擎
│   ├── document/document.go — 文档记忆
│   ├── text/text.go       — 文本日志
│   └── pipeline/          — 蒸馏器
├── knowledge/knowledge.go — 知识库
├── lua/
│   ├── vm.go              — Lua VM (json/log/http)
│   └── adapters/          — 8 个 LLM 适配器脚本
├── config/registry.go     — SQLite 配置中心
├── events/bus.go          — 事件总线
├── tracker/               — OverlayFS 变更追踪
├── supervisor/            — 守护进程管理
├── skill/                 — Skill 插件管理
│   └── manager.go         — Skill 加载/匹配
└── meta/                  — 元信息
    └── meta.go            — Agent 元数据
```
