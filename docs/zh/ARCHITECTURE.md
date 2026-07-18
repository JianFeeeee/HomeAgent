[English](../en/ARCHITECTURE.md) | **中文**

# HomeAgent 架构

内核零 IO，一切外界交互来自插件。

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

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

代码：`internal/agent/core/agent.go` — `process()` 是工具循环主体

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

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 三层记忆

### 记忆流转

```
① Context (工作窗口)
   RelevanceContext — 内存 events[] + JSON持久化
   Append: 每次输入, CleanTemplateText → 三分支向量(textForVector)
            agent事件→Response, 用户事件→Input, cold_storage→Input+Response
            StaticEmbedder 预训练词嵌入 / TF-IDF 回退
   Prune:  StaticEmbedder CosineSimilarity, 保留 topK + 最近10条
       ├── 保留 → timeline → 按时间排序 → system prompt
       └── 低分 → Document 层归档 (原始时间戳)
   Save: 5s debounce 写盘

         ↓ Prune 归档                          ↑ LLM 主动召回

② Document (文件记忆)
   DocStore — JSON文件 + TF-IDF InvertedIndex
   写入: Prune归档 / doc_commit / Graph快照(syncGraphToDocs)
   读取:
       ├── 自动注入: Query(input, top3) → 相似度摘要 → 【相关记忆文档】→ system prompt (只读)
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
       │     → CleanTemplateText → 向量实体搜索 + jieba关键词 → SQLite LIKE + BFS depth=2
       │     → 【记忆索引】→ system prompt
       └── LLM主动: memory_recall / memory_merge / memory_purge / memory_edit / memory_delete_entity
   Social: person_query / set_trait / relate (包装 GraphDB)

④ 蒸馏管道 (每30min心跳)
   distillContext → 窗口>2×maxSize → 强制Prune
   syncGraphToDocs → Graph 快照写入 Document(跨层可搜索)
   reorgGraph:
       Step1: indexer.Sync — 重建实体向量索引
       Step2: docStore.Reindex — 重建文档向量索引
       Step3: 冷文档 → docToTriples → GraphDB.Commit
       Step4: 实体相似度(Bigram Jaccard>0.75) → consolidation → LLM判断合并
       Step5: evaluateGraphQuality → LLM判断保留/删除

⑤ Pipeline 规则蒸馏器 (每心跳)
   distillOnce → 正则匹配个人信息:
     我叫X / 我住在X / 我喜欢X / 我X岁 / 我的工作是X
     → 三元组 → GraphDB.Commit
```

### 向量化：预训练词嵌入 + TF-IDF 回退

所有向量化统一使用 `StaticEmbedder`（`internal/memory/static_embedder.go`）：

**主策略 — 预训练词嵌入（词对齐 300 维）**
- 模型来源：ConceptNet Numberbatch（77 语对齐）/ fastText 中文 / fastText 英文
- 通过 `core.agent.embedding_model_path` 配置（逗号分隔多模型）
- 路径名含 `numberbatch` → 自动下载 ConceptNet，含 `cc.zh.` → fastText 中文，含 `cc.en.` → fastText 英文
- 不匹配则默认 ConceptNet
- **前处理**：`CleanTemplateText` 剥离 QQ 工具调用模版、时间戳噪声，避免垃圾干扰相似度
- **三分支向量来源**：agent→Response，用户→Input，cold_storage→Input+Response
- **TF-IDF 回退**：模型下载失败或未配置时自动回退词袋 TF-IDF，服务不中断

| 位置 | 文件 | 用途 | 算法 |
|------|------|------|------|
| Context Prune | `context.go:155` | 裁剪低相关性上下文事件 | VectorizeClean → CosineSimilarity(queryVec, evt.Vector) |
| DocStore Query | `document.go:206` | 文档记忆召回 | TF-IDF Vectorize → vec.Search |
| Indexer 实体搜索 | `indexer.go:96+111` | Graph实体召回 | 向量实体搜索 + jieba关键词 → SQLite LIKE + BFS |
| 实体相似度检测 | `agent.go` | Graph中相似实体 | Bigram Jaccard (>0.75 → consolidation) |

### Context 层

`internal/agent/core/context.go` — `RelevanceContext`
- 维护最近事件列表，每次 Append/Prune 写入 JSON 防丢
- 向量化前统一经 `CleanTemplateText` 去模版噪声
- 三分支 `textForVector`：agent 事件用 Response、用户事件用 Input、cold_storage 用 Input+Response
- 预训练词嵌入 `StaticEmbedder` → CosineSimilarity，模型不可用时自动回退 TF-IDF
- 保护最近 10 条记录免于淘汰，超出部分按相关性排序归档到文档记忆
- 归档事件以原始时间戳写入文档记忆，后续 `doc_query` 召回时按原始时间戳插回时序

### Document 层

`internal/memory/document/document.go` — `Store`
- 消费即删模式：`doc_query` 检索到后删除
- 双路召回：char-bigram TF-IDF 向量搜索 + jieba 关键词提取

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
心跳 30min:
  ├── distillContext()        — 蒸馏当前上下文
  ├── syncGraphToDocs()       — Graph→Document 同步
  └── reorgGraph()
      ├── Indexer.Sync()
      ├── DocStore.Reindex()
      ├── 冷文档→Graph
      └── 实体冲突 → enqueueConsolidationTask()
                          │
                    selfInputCh → LLM 判断合并/跳过
```

实体冲突检测启发式（bigram Jaccard > 0.75），走 `selfInputCh` 内部通道，LLM 最终判断是否合并。

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 知识库

`internal/knowledge/knowledge.go`
- 文件目录 `knowledge/<name>/content.md`
- 独立 TF-IDF 索引，与记忆系统不冲突
- `knowledge_search` / `knowledge_create` / `knowledge_list`

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Provider 与 Lua 适配层

```
Agent
  │
  ▼
Provider 接口 (Name / Chat / ChatStream)
  │
  ├── OpenAIProvider   — 标准 OpenAI API
  ├── OllamaProvider   — 本地 Ollama
  └── LuaAdaptedProvider (主要)
      ├── 序列化 CompletionRequest → JSON
      ├── adapter.transform_request() → API 格式
      ├── HTTP 请求 + adapter.headers
      ├── adapter.transform_response() → 统一格式
      └── 反序列化
```

代码：`internal/agent/api/provider.go`

ProviderManager 管理多个源，按注册顺序 fallback。Lua 适配器位于 `internal/lua/adapters/`，每个 `.lua` 脚本定义 `transform_request` / `transform_response` / `transform_stream_chunk`。

VM 内置 `json.encode` / `json.decode` / `log` / `http_get` / `http_post`。

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 插件系统

### 四种加载方式

| 方式 | 注册机制 | 编译 | 用途 |
|------|----------|------|------|
| 内置插件 | `init()` → `RegisterFactory` | `internal/plugins/` 编译进内核 | webui/cli/timer/mcp 等 |
| 外部 `.so` | C ABI 动态加载 | `-buildmode=c-shared` + bridge | qq/files/web/memo 等 |
| Lua 脚本插件 | 解析 `main.lua` 注册工具 | 无需编译，热加载 | luaplugintest/testlua 等 |
| SKILL 插件 | 解析 `SKILL.md` | Markdown 定义 | OpenClaw 兼容 |

内置插件注册：`internal/plugins/all.go` 空白导入 → 各插件 `init()` → `Registry.Load()` 扫描目录匹配工厂。
外部插件加载：`internal/plugin/dynamic.go` → 复制到 SHA256 临时路径（绕过 `plugin.Open` 路径缓存）→ `Open` + `Lookup("NewPlugin")`。
Lua 脚本插件加载：`internal/lua/` → 通过 Lua VM 解析 `main.lua`，调用 `start()` 注册工具。

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
sdk.RegisterOutputChannel("qq", sdk.CapText, "QQ消息通道，content为JSON: {text, group_id, user_id}", handler)
```

### Plugin 接口

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
```

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 输出通道系统

每个输出通道生成两个工具：

| 工具 | 类型 | 作用 |
|------|------|------|
| `output_send__{name}` | function | 接受 `content` JSON 字符串参数，透明路由到插件注册的 handler |
| `output_send__{name}_help` | function | 返回通道的 JSON 格式文档（desc 字段） |

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

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## LLM 链事件

- 事件类型 `agent_llm_chain`，每次 LLM 轮次后发射
- 包含完整 LLM 响应（文本 + 工具调用 + 推理）
- WebUI 通过 SSE 订阅此事件实现实时显示
- 插件可通过 EventSubscriber 订阅（外部插件只读）

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 受限外部插件 API

分层架构：内部插件获得完整 PluginSDK，外部插件获得受限 SDK。

| API | 内部插件 | 外部插件 |
|-----|----------|----------|
| SocialAPI | 完整读写 | 只读（GetPerson / GetTrait / GetRelations / GetNetwork / ListPersons） |
| EventSubscriber | 订阅 + 发布 | 仅订阅（无 Publish 能力） |

扩展字段：
- Triple 扩展：Confidence、SubjectType、ObjectType
- Relation 扩展：Confidence

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

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

代码：`internal/agent/core/agent.go` — `interceptLoop` / `drainInterrupt`

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 配置系统

`internal/config/registry.go` — ConfigRegistry

- SQLite 存储，`config` 表 + `config_<plugin>` 独立表
- 命名空间：`core.*` / `plugin.<name>.*`
- `RegisterDefault` 插入 ~80 个默认键（8 个 LLM 源的 seeds）
- WebUI 设置页 `/api/v1/settings` 读写

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

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
│   ├── openclaw/          — OpenClaw 兼容
│   ├── agentcli/          — PTY 终端
│   ├── healthcheck/       — 健康检查
│   └── pluginmgr/         — 插件管理器
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
