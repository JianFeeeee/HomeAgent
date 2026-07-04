# HomeAgent 架构设计 v4

## 一、核心理念

24 小时陪伴用户的智能管家。**单会话·单 Agent**，身份不漂移。

### 设计原则
- **核心零 IO** — Core 没有任何硬编码 IO 能力，所有 IO 来自插件
- **所有输出是工具调用** — Agent 必须显式调用 `output_send` 才能通信，推理不自动路由
- **所有 LLM 调用走 Provider 接口** — 不直连 API
- **DeepSeek v4 flash** 为默认 LLM，thinking 模式关闭
- **人格固定**（personal.md），记忆分层管理防止性格突变
- **知识独立于记忆**，agent 主动学习
- **插件 = 三通道**：工具、阶段钩子、事件订阅

---

## 二、核心域 vs 插件域

```
┌──────────────────────────────────────────────────────────────────┐
│                        核心域 (Core Domain)                         │
│                                                                    │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌───────────────────┐ │
│  │ Provider  │  │  Memory   │  │Knowledge │  │  Pipeline Stages  │ │
│  │  (LLM)   │  │ (T/D/G)  │  │ (TF-IDF) │  │   编排器          │ │
│  └──────────┘  └──────────┘  └──────────┘  └───────────────────┘ │
│  ┌──────────┐  ┌──────────┐  ┌──────────────────────────────────┐ │
│  │ Relevance│  │  Context  │  │    Plugin Host                   │ │
│  │  Tf-Idf  │  │ Persist  │  │  (调用钩子 + 路由工具)            │ │
│  └──────────┘  └──────────┘  └──────────────────────────────────┘ │
│                    核心无任何 IO 能力                               │
├──────────────────────────────────────────────────────────────────┤
│                        边界 (Plugin API)                           │
│  ┌──────────────────────────────────────────────────────────────┐ │
│  │  RegisterTool(name, handler)   ← 插件注册工具给 LLM          │ │
│  │  RegisterStage(stage, handler) ← 插件挂入消息处理阶段         │ │
│  │  Subscribe(eventType, handler) ← 插件订阅系统事件             │ │
│  │  Publish(event)                → 插件发布事件                 │ │
│  │  Settings().Get/Set/List       ← 读写核心/插件配置            │ │
│  │  Memory().Recall/Commit        ← 图记忆访问                   │ │
│  │  Knowledge().Search/Create     ← 知识库访问                   │ │
│  └──────────────────────────────────────────────────────────────┘ │
├──────────────────────────────────────────────────────────────────┤
│                        插件域 (Plugin Domain)                      │
│                                                                    │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────────────┐ │
│  │  WebUI   │  │   QQ     │  │OutputBus │  │ 未来插件: ……     │ │
│  │ HTTP/WS  │  │ OneBot   │  │通道管理   │  │                  │ │
│  └──────────┘  └──────────┘  └──────────┘  └──────────────────┘ │
│                    所有 IO 都在这里                                 │
└──────────────────────────────────────────────────────────────────┘
```

### 核心职责
- LLM 调用编排（Provider → Agent 工具循环）
- 三层记忆管理（Context → Document → Graph）
- 知识库维护（Knowledge Store）
- 阶段管道编排（Stage Pipeline）
- Context 相关性管理（TF-IDF 余弦相似度）
- 心跳蒸馏 + 图重整

### 插件职责
- 接收外部输入（WebSocket、HTTP、硬件等）
- 提供输出能力（文本发送、文件传输等）
- 干预消息处理流（阶段钩子）
- 观察系统状态（事件订阅）

---

## 三、消息处理阶段管道

```
                       ┌──────────────────────────────────────────┐
                       │            on_input                      │
                       │  消息到达，Agent 未做任何处理               │
                       │  └→ 插件可鉴权/拉黑/改写/短路回复           │
                       └──────────┬───────────────────────────────┘
                                  │ 通过
                       ┌──────────▼───────────────────────────────┐
                       │  内部：Context Append + Memory Recall     │
                       │         + Context 组装                    │
                       └──────────┬───────────────────────────────┘
                                  │ 就绪
                       ┌──────────▼───────────────────────────────┐
                       │            pre_action                     │
                       │  上下文已就绪，即将调用 LLM                 │
                       │  └→ 插件可注入 system 消息 / 修改 context  │
                       └──────────┬───────────────────────────────┘
                                  │ LLM 调用
                       ┌──────────▼───────────────────────────────┐
                       │            post_action                    │
                       │  LLM 返回文本 + 工具调用列表                │
                       │  └→ 插件可审查/修改文本、增删工具调用        │
                       └──────────┬───────────────────────────────┘
                                  │ 判断有无工具调用
                    ╱─────────────┴─────────────╲
              有工具调用                      无工具调用
                  │                              │
       ┌──────────▼──────────────┐               │
       │    before_toolcall       │               │
       │  即将执行某个工具调用     │               │
       │  └→ 插件可拒绝/放行/     │               │
       │     修改参数/审计        │               │
       └──────────┬──────────────┘               │
                  │ 执行工具                      │
       ┌──────────▼──────────────┐               │
       │    after_toolcall       │               │
       │  工具执行完毕，准备喂回   │               │
       │  └→ 插件可脱敏/改写结果  │               │
       └──────────┬──────────────┘               │
                  │ 回到 post_action 继续循环     │
                  └──────────────────────────────┘
                                                  │
                       ┌──────────────────────────┘
                       ▼
            ┌──────────────────────────────────────────┐
            │          before_output                    │
            │  最终文本就绪，即将调用 output_send        │
            │  └→ 插件可改写回复/添加格式/适配渠道       │
            └──────────────────┬───────────────────────┘
                               │ output_send 调用
            ┌──────────────────▼───────────────────────┐
            │          after_output                     │
            │  输出完成                                  │
            │  └→ 记录/统计/清理资源                     │
            └──────────────────────────────────────────┘
```

### 7 个阶段总表

| 阶段 | 触发时机 | 插件读写权限 | 典型用途 |
|---|---|---|---|
| `on_input` | 消息到 Agent，零处理 | 可读写 `raw_message`，可设置 `response` 短路 | 黑名单、限流、自定义指令前缀 |
| `pre_action` | Memory+Context 就绪，LLM 调用前 | 可读写 `context_messages`（追加/修改） | 注入 RAG 结果、插入时政 context |
| `post_action` | LLM 返回文本 + 工具调用列表 | 可读写 `llm_text`、`tool_calls`、`context_messages` | 敏感词过滤、强制 redirect 工具 |
| `before_toolcall` | 单个工具调用执行前 | 可读写 `tool_call.name`、`tool_call.args`，设置 `deny=true` 拒绝 | 审计高危操作、OS 命令白名单 |
| `after_toolcall` | 单个工具执行完毕 | 可读写 `tool_result` | 脱敏数据库结果、排序搜索结果 |
| `before_output` | 最终文本就绪，output_send 前 | 可读写 `final_text`，可设置 `skip_output=false` | 添加表情/at 前缀、多平台格式适配 |
| `after_output` | output_send 已调用 | 只读 `final_text` | 统计日志、触发后续流程 |

### 循环规则

`post_action → before_toolcall → after_toolcall → 回到 post_action` 构成**内循环**。Agent 在以下条件退出循环进入 `before_output`：
- LLM 返回纯文本（无工具调用）
- `before_toolcall` 拒绝所有剩余工具且 LLM 无可执行工具
- 循环超过 `max_tool_rounds` 上限

### 短路规则

每个阶段插件都可设置 `ctx.Response`，一旦设置管道立即短路到 `after_output`：
```
on_input → ctx.Response = "hello" → 跳过后面的所有阶段 → after_output
```

---

## 四、记忆体系（三层递进）

```
输入消息
    │
    ▼
┌─────────────────────────────────────────────────────┐
│  Layer 1: Context (RelevanceContext)                 │
│  内存中维护最近 topK 条事件，TF-IDF 评分，JSON 持久化   │
│  每次 Append/Prune → save() 防崩溃丢数据              │
│  keep=30 条活跃，多余 → 归档到 Document               │
└────────────────────┬────────────────────────────────┘
                     │  Prune 时
                     ▼
┌─────────────────────────────────────────────────────┐
│  Layer 2: Document (document.Store)                  │
│  文件系统 JSON + TF-IDF 向量索引                      │
│  冷文档（72h 未访问 + access ≤ 2）→ 蒸馏到 Graph     │
│  也可以由用户主动 commit（doc_commit 工具）           │
└────────────────────┬────────────────────────────────┘
                     │  reorg 心跳
                     ▼
┌─────────────────────────────────────────────────────┐
│  Layer 3: Graph (GraphDB + Indexer)                  │
│  SQLite: entities + relations                        │
│  Entity: name, type, mention_count                   │
│  Relation: source → target, relation_type, confidence│
│  搜索: 关键词 → 向量搜索实体 → BFS 遍历邻居          │
│  蒸馏: 原始记录 → Distiller → 三元组提交             │
└─────────────────────────────────────────────────────┘
```

### 数据流关系

```
Context 修剪 → Document 归档 → reorg 心跳 → Graph 消化
                                              ↑
                                     Distiller (原始记录 → 三元组)
```

### 工具入口（Agent 暴露给 LLM）

- `memory_recall(query)` → 从 Graph 召回
- `memory_commit(triples)` → 写入 Graph
- `memory_introspect()` → 查看统计
- `doc_query(query)` → 从 Document 搜索
- `doc_commit(title, content)` → 写入 Document

---

## 五、知识体系

独立于记忆，agent 主动学习。

```
knowledge/<name>/
    content.md

knowledge.Store
    └─ TF-IDF 向量索引 (character bigram)
    └─ 独立于 memory 的 vector.Store 实例
    └─ Start() 时扫描目录训练索引
    └─ Add(name, content) 时增量更新
```

### 工具入口

- `knowledge_search(query)` → 向量搜索
- `knowledge_create(name, content)` → 新增
- `knowledge_list()` → 列出所有

### 为什么独立于 memory？

- Memory 是 LLM 的"对话记忆"——谁说过什么、上下文
- Knowledge 是 LLM 的"知识库"——外部注入的固定知识
- 两者 TF-IDF 索引实例隔离，不互相污染

---

## 六、三通道插件交互

```
插件 ──→ 核心                  核心 ──→ 插件
──────────────────────────────────────────────────
RegisterTool(name, fn)  ──→  buildToolDefs()
                             executeToolCall() → fn
                             （Tracker 自动包裹 Pre/PostAction）

RegisterStage(stage, fn) ──→  runStage() 在对应阶段调用 fn(ctx)
                             返回后检查 ctx.Response 决定是否短路

Subscribe(eventType, fn) ──→  Publish(event)
                             所有订阅者收到（观察型）
```

### 通道对比

| 通道 | 方向 | 用途 | 可否拦截 |
|---|---|---|---|
| **工具** (RegisterTool) | 插件→核心→LLM | LLM 主动调用插件功能 | 否 |
| **阶段** (RegisterStage) | 核心→插件 | 核心触发插件干预消息流 | 是（response 短路） |
| **事件** (Subscribe/Publish) | 双方向 | 审计/日志/状态通知 | 否 |

---

## 七、Agent 内部完整流程

```
processTextInput(input)
    │
    ├── on_input stage ────────────── 插件可拦截/改写
    │
    ├── context.Append(input)
    ├── context.Prune(input)         → 归档到 Document
    ├── buildMemoryContext()         → Indexer.BuildContext → Graph Recall
    │
    ├── pre_action stage ──────────── 插件可注入 context
    │
    ├── [循环] process(input)
    │   ├── buildSystemPrompt (人格+记忆+技能+上下文)
    │   ├── buildToolDefs (内置工具 + 插件工具)
    │   ├── provider.Chat() → LLM
    │   │
    │   ├── post_action stage ─────── 插件可见 LLM 输出 + 工具列表
    │   │
    │   ├── 有工具调用？
    │   │   ├── 每个工具:
    │   │   │   ├── before_toolcall stage ── 插件可拒绝/改参
    │   │   │   ├── Tracker.PreAction
    │   │   │   ├── executeToolCall() ──── 路由到插件或内置
    │   │   │   ├── Tracker.PostAction
    │   │   │   └── after_toolcall stage ── 插件可改结果
    │   │   └── → 回到 post_action (继续循环)
    │   │
    │   └── 无工具调用 → 退出循环
    │
    ├── context.Append(response)
    ├── before_output stage ───────── 插件可改写最终文本
    ├── Publish(agent_output event)
    ├── output_send (调用插件注册的 output 工具)
    │
    └── after_output stage ────────── 插件只读，做统计/日志
```

---

## 八、记忆整理（心跳 LLM 驱动消歧）

图数据库在长期运行中会积累**同义实体**（如「张三」与「张先生」指同一人）和**矛盾关系**。心跳流程如下：

### 流程

```
心跳 tick (30min)
    │
    ├── distillContext()         — 蒸馏上下文
    ├── syncGraphToDocs()        — 图→文档
    │
    └── reorgGraph()
        ├── Indexer.Sync()       — 图→向量（自动）
        ├── DocStore.Reindex()   — 文档重建索引（自动）
        ├── 冷文档→图归化        — 将冷文档归档为图三元组（自动）
        │
        └── 实体冲突检测 → 发现相似实体对
                │ 如：「张三」(person, 5次) vs 「张先生」(person, 3次)  相似度 0.75
                │
                ▼
            enqueueConsolidationTask()
                │ 通过 IO 层注入 Agent 输入队列
                │ channel = "_consolidation_"（内部通道，不对外输出）
                ▼
            Agent 处理 (processConsolidation)
                │ 如同普通用户消息，走完整 LLM 工具循环
                │ 但输出仅写记忆，不发外部通道
                ▼
            LLM 决策：
                ├─ 判断为同一实体 → 调用 memory_merge 合并
                │   → "已将「张先生」合并到「张三」，3 条关系已重定向"
                ├─ 判断为不同实体 → 回复"跳过"
                └─ 不确定 → 回复"待定，需更多上下文"
```

### 关键设计

| 特性 | 说明 |
|---|---|
| **启发式检测，LLM 决策** | bigram Jaccard 仅做候选筛选（低门槛 0.5），LLM 做最终判断 |
| **走 IO 输入队列** | 不阻塞心跳，不抢占用户输入，享受完整 Agent 上下文 |
| **`_consolidation_` 通道** | 内部专用通道，输出只写记忆层，不被外部插件路由 |
| **`memory_merge` 工具** | LLM 通过此工具执行合并，自动重定向关系 + 累积 mention_count |
| **异步非阻塞** | 整理任务排队在 inputCh 尾部，Agent 按序处理，不影响用户体验 |

### 类比

类似人类睡眠时大脑的海马体回放——白天经历的记忆在休息时被自发整理、关联、去重。HomeAgent 的心跳就是它的"睡眠周期"，而 LLM 的参与相当于前额叶皮层执行语义判断。类比：

```diff
- 人类:  白天经历 → 海马体暂存 → 睡眠 → 前额叶整理 → 长期记忆
+ Agent: 用户交互 → Context缓存 → 心跳 → LLM 消歧 → GraphDB 存储
```

---

## 九、Child Agent

不走阶段管道，独立轻量 Agent：

```
spawn_child(task) → 新建轻量 Agent
    ├── 独立 system prompt（仅有任务描述）
    ├── 仅 output_send 工具
    ├── 无 persistent memory
    ├── 无 Graph/Document 访问
    ├── 上限 5 轮工具循环
    └── 销毁时返回结果文本
```

---

## 九、LLM Provider 与 Lua 适配层

LLM 调用全部通过 `Provider` 接口，核心实现是 `LuaAdaptedProvider`：

```
Agent
  │
  ▼
Provider 接口 (Name / Chat / ChatStream)
  │
  ▼
LuaAdaptedProvider
  ├── 1. 序列化 CompletionRequest → raw JSON
  ├── 2. adapter.transform_request(rawJSON) → 协议特定请求体
  ├── 3. 读取 adapter.endpoint + adapter.headers 发 HTTP
  ├── 4. adapter.transform_response(rawHTTPBody) → 统一响应格式
  └── 5. 反序列化为 CompletionResponse
```

### Lua 适配器契约

每个适配器是一个返回 table 的 Lua 脚本，位于 `data/adapters/*.lua`：

```lua
adapter.name = "deepseek"
adapter.version = "2.0.0"
adapter.endpoint = "/chat/completions"
adapter.headers = {}  -- 静态头（Go 自动加 Authorization）

-- 请求变换：raw JSON → 协议格式
function adapter.transform_request(raw_body) return transformed end

-- 响应变换：HTTP body → 统一格式 {content, reasoning_content, finish_reason, token_usage, tool_calls}
function adapter.transform_response(raw_body) return unified end

-- 流变换（可选）：SSE data line → {content, done}
function adapter.transform_stream_chunk(raw_line) return chunk end
```

### Lua VM 能力

- `json.encode(table)` → 使用 Go `json.Marshal` 的 JSON 序列化
- `json.decode(string)` → 使用 Go `json.Unmarshal` 的 JSON 反序列化
- 全局函数 `log(level, msg)` / `http_get(url)` / `http_post(url, body)`
- 适配器内置 3 个：`openai.lua`、`deepseek.lua`、`ollama.lua`

## 十、配置中心 (ConfigRegistry)

配置不再分散在各处——通过 `ConfigRegistry` 统一管理：

```
Plugin (通过 SDK)
  │
  ├─ Settings().Get("core.llm.model")        → 读核心配置
  ├─ Settings().Set("plugin.qq.token", x)    → 写插件配置
  ├─ Settings().List("plugin.")              → 列出所有插件键
  │
  ▼
ConfigRegistry (线程安全 KV 存储)
  ├── 持久化到 data/settings.json
  ├── 键命名空间: core.* / plugin.<name>.*
  ├── Register(key, default)  ← 注册默认值（不标记 dirty）
  ├── Get/Set/List/Delete     ← 运行时读写
  └── Flush()                 ← 写回磁盘
```

### WebUI 配置编辑

```
┌──────────────────────────────────┐
│  侧边栏         编辑区            │
│  ┌──────┐  ┌──────────────────┐  │
│  │ core │  │ core.llm.model   │  │
│  │plugin│  │ [input field]    │  │
│  │.qq   │  │ [保存]           │  │
│  │plugin│  ├──────────────────┤  │
│  │.webui│  │ core.llm.base_url│  │
│  └──────┘  │ [input field]    │  │
│            │ [保存]           │  │
│            └──────────────────┘  │
└──────────────────────────────────┘
```

- `GET /api/v1/settings?prefix=core.` → 列出配置键值 + 插件列表
- `PUT /api/v1/settings` → `{key, value}` 写入配置

## 十一、SDK API 定义

### PluginAPI (`internal/plugin/sdk/api.go`)

```go
type PluginAPI struct {
    Name    string
    Version string
}

func NewPluginAPI(name, version string, bus EventBus, mem MemoryAPI, know KnowledgeAPI) *PluginAPI

// 三通道
func (p *PluginAPI) RegisterTool(name string, handler ToolHandler) error
func (p *PluginAPI) RegisterStage(stage Stage, handler StageHandler)
func (p *PluginAPI) Subscribe(eventType EventType, handler EventHandler)
func (p *PluginAPI) Publish(evt *Event)

// 访问子系统的快捷方式
func (p *PluginAPI) Memory() MemoryAPI
func (p *PluginAPI) Knowledge() KnowledgeAPI
```

### 阶段上下文 (`StageContext`)

```go
type StageContext struct {
    RawMessage  string                    // 当前输入（可改写 on_input）
    UserID      string
    GroupID     string
    ContextMsgs []map[string]interface{}  // 可注入的消息
    LLMText     string                    // LLM 返回文本（可改写 post_action）
    ToolCalls   []ToolCall                // 工具调用列表（可增删 post_action/before_toolcall）
    ToolResults []ToolResult              // 工具执行结果（可改写 after_toolcall）
    FinalText   string                    // 最终输出文本（可改写 before_output）
    Response    *string                   // 设置后短路管道
    Phase       Stage                     // 当前阶段
    Memory      []MemItem                 // 召回的记忆
    Extra       map[string]interface{}    // 扩展字段
}
```

---

## 十二、事件系统

### 事件类型

| 类型 | 发布时机 | 用途 |
|---|---|---|
| `raw_input` | 消息到达 Agent | 记录输入日志 |
| `agent_output` | 最终输出发送后 | 记录输出日志 |
| `tool_call` | 每个工具调用完成 | 审计工具调用 |
| `reasoning` | LLM 推理文本 | 展示推理过程 |
| `system` | 系统状态变更 | 健康检查、插件变更 |

### Event Bus (`internal/events/bus.go`)

```go
type Bus struct{}
func NewBus() *Bus
func (b *Bus) Publish(event *Event)
func (b *Bus) Subscribe(eventType EventType, handler Handler) func()
```

---

## 十三、自循环输入通道

核心维护一个独立的 `selfInputCh (chan string)`，用于内部任务（记忆消歧、系统维护），不经过 IO 层：

```
心跳检测 → entitySimilarity() → enqueueConsolidationTask()
                                         │
                                    injectSelf(msg)
                                         │
                                    selfInputCh ─→ eventLoop ─→ processTextInput
                                         │
                                   不经过 IOManager，不经过任何插件
```

区别于 IOManager.InputChan（外部输入），selfInputCh 是核心自有的纯 Go channel，
确保即使没有 IO 插件，记忆整理等维护任务也能正常执行。

## 十四、数据流全景

```
外部 (QQ/HTTP/硬件)
    │ 通过插件
    ▼
IOManager.InjectInput() → inputCh
    │
    ▼
Agent.eventLoop() → handleInput → processTextInput
    │
    ├── 1. on_input stage（插件可拦截）
    ├── 2. Context.Append
    ├── 3. Memory Recall (Indexer → Graph)
    ├── 4. pre_action stage（插件可注入）
    ├── 5. 工具循环 (最多 10 轮)
    │      LLM → post_action → [before_toolcall → 执行 → after_toolcall] → LLM ...
    ├── 6. Context.Append(response)
    ├── 7. Prune（不相关 → Document）
    ├── 8. before_output stage（插件可改写）
    ├── 9. EmitOutput (通过 output_send 到对应通道)
    ├── 10. after_output stage（插件只读）
    └── 11. memory_candidate → TextMemory + Distiller → GraphDB

心跳（30min）:
    ├── distillContext()
    ├── syncGraphToDocs()
    └── reorgGraph()
        ├── Indexer.Sync() — 图→向量（自动）
        ├── DocStore.Reindex() — 文档向量重建（自动）
        ├── 冷文档→图归化（自动）
        └── 实体冲突检测 → 走 LLM 消歧（详见第八章）
```

---

## 十五、代码结构

```
cmd/homed/main.go                        — 入口：组装所有子系统, 零 IO
cmd/waiter/main.go                       — CLI 客户端 (Unix socket)
internal/
├── agent/
│   ├── core/
│   │   ├── agent.go                     — Agent: eventLoop/interceptLoop/process/distillLoop
│   │   ├── context.go                   — RelevanceContext：TF-IDF 上下文管理
│   │   └── stages.go                    — StageHost：阶段管道编排 (并行执行)
│   ├── api/
│   │   └── provider.go                 — Provider 接口 + DeepSeek/Ollama/LuaAdaptedProvider
│   ├── io/
│   │   └── channel.go                  — IOManager (排队/中断/输出三通道)
│   └── personal.go                     — 人格加载
├── sdk/                                 ★ PluginSDK: 核心 Go API
│   ├── plugin.go                       — Plugin 接口 + PluginSDK 结构体
│   ├── memory.go                       — MemoryAPI (图/文本/文档)
│   ├── knowledge.go                    — KnowledgeAPI
│   ├── settings.go                     — SettingsAPI (配置)
│   └── llm.go                          — LLMAPI (源管理)
├── config/
│   └── registry.go                     — ConfigRegistry：统一配置中心 (SQLite)
├── events/
│   └── bus.go                          — 系统事件总线 (Publish/Subscribe)
├── plugin/
│   ├── registry.go                     — 注册表：生命周期 Load/StopAll/Reload, RegisterFactory
│   ├── manifest.go                     — PluginManifest (plugin.json 元数据)
│   ├── dynamic.go                      — .so 动态加载器 (Go plugin.Open)
│   └── plugin.go                       — SKILL 插件解析 (OpenClaw 兼容)
├── plugins/
│   ├── all.go                          — 空白导入触发所有内置插件 init()
│   ├── all_test.go                     — 集成测试（14 工具跨插件）
│   ├── agentcli/plugin.go              — PTY 终端 (6 个 terminal_* 工具)
│   ├── cli/plugin.go                   — CLI 插件 (Unix socket, InjectTextSync)
│   ├── cmd/plugin.go                   — 命令执行 (cmd_run 工具)
│   ├── healthcheck/plugin.go           — 健康检查 + 性能监控 + 自动调度
│   ├── mcp/plugin.go                   — MCP 协议支持
│   ├── openclaw/                       — OpenClaw 兼容
│   │   ├── plugin.go                   — SKILL.md + sidecar + simulator 三通道
│   │   ├── sidecar.go                  — JSON-RPC over stdio 侧车管理
│   │   └── simulator/
│   │       └── main.js                 — OpenClaw 插件模拟器 (go:embed)
│   ├── timer/plugin.go                 — 定时器 (timer_set 工具 + 中断反馈)
│   └── webui/                          — WebUI 插件 (HTTP 服务器 + SPA 仪表盘)
│       ├── plugin.go
│       ├── handler.go
│       └── dashboard.html              — 嵌入式 SPA (go:embed)
├── memory/
│   ├── graph.go                        — SQLite 图数据库
│   ├── indexer.go                      — 图索引器
│   ├── vector/store.go                 — TF-IDF 向量存储
│   ├── document/doc.go                 — 文档记忆
│   ├── text/text.go                    — 文本记忆 (JSONL)
│   └── pipeline/                       — 蒸馏器
├── knowledge/
│   └── knowledge.go                    — 知识系统
├── onebot/                             — OneBot V11 QQ 协议实现
├── tracker/                            — 变更追踪 (overlayfs)
├── supervisor/                         — 守护进程
├── skill/                              — 技能管理器
├── lua/
│   ├── vm.go                           — Lua VM (json.encode/decode, transform)
│   └── adapters/                       — LLM 协议适配器脚本
├── network/                            — 网络监控
├── container/                          — 容器管理
├── snapshot/                           — 快照
├── embed/                              — 嵌入
└── tokenizer/                          — 分词器
config/                                 — 顶层配置加载
├── config.go                           — Config 结构
└── config.yaml
pkg/types/                              — 类型定义
docs/
├── ARCHITECTURE.md                     — 本架构文档
├── ADAPTER.md                          — Lua 适配器文档
└── PLAN.md                             — 实施计划/概览
```

---

## 十六、与旧设计 (v3) 的关键区别

| 维度 | v3 | v4 |
|------|-----|-----|
| 插件交互 | Device 接口 + IOManager 路由 | 三通道：Tool/Stage/Event/Settings |
| 消息流编辑 | 无（纯事件推送） | 阶段管道 7 个 hook 点 |
| Event Bus | 无 | `internal/events/bus.go` |
| SDK | 无 | `internal/plugin/sdk/` (含 SettingsAPI) |
| LLM 适配 | 硬编码 Provider | Lua 脚本 raw JSON 变换 |
| 配置管理 | 分散在各处 | ConfigRegistry 统一 KV 存储 |
| 核心 IO | IOManager `EmitOutput` 直出 | 全部走 `output_send` 工具 |
| 插件工具路由 | IOManager `ExecuteTool` 链 | StageHost + Registry 双层路由 |
| 内部任务 | 无 | selfInputCh 自循环通道（不经过 IO） |

---

## 十七、插件自注册与动态加载

### 自注册机制

内置插件通过 `init()` 自注册，无需 `main.go` 硬编码：

```go
// internal/plugins/timer/plugin.go
func init() {
    plugin.RegisterFactory("timer", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
        return New(name), nil
    })
}
```

空白导入文件 `internal/plugins/all.go` 触发所有内置插件的 `init()`：

```go
package plugins
import (
    _ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/cli"
    _ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/openclaw"
    _ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/timer"
    _ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/webui"
)
```

`main.go` 不再需要 `RegisterNative` 调用，只需设置包级变量注入运行时依赖：

```go
cli.DefaultSocket = *cliSocket
openclaw.SkillsDir = filepath.Join(*dataDir, "skills")
webui.Configure(httpAddr, sup, memDB, ...)
pluginReg.Load(plgDir)  // 自动扫描目录 + 使用已注册的工厂
```

`Load()` 分两步执行：
1. 扫描 `plugins/` 下已有子目录，匹配已注册工厂加载
2. 对已注册工厂但尚无目录的，自动创建目录并加载

### 动态 .so 加载

第三方插件编译为 `.so` 文件，放入 `<data>/plugins/<name>/`：

```
<data>/plugins/myplugin/
    plugin.json   { "name": "myplugin", "version": "1.0", "entry": "plugin.so" }
    plugin.so     (Go -buildmode=plugin, 导出 NewPlugin 函数)
```

加载器 (`internal/plugin/dynamic.go`) 流程：

```go
tryLoadSO(dir, name, config):
    1. plugin.Open("plugin.so")
    2. Lookup("NewPlugin") — 签名 func(name string, config map[string]interface{}) (sdk.Plugin, error)
    3. 调用 factory, 包装为 dynamicPlugin
```

内置插件保持 init() 自注册编译进内核，第三方插件以 .so 形式热加载。

---

## 十八、中断打断机制

### 架构

```
interceptLoop (独立 goroutine)
    ├── InputInterruptChan() ← 定时器/消息通知等
    │
    ├── (a) cancelLLM() → Provider HTTP 请求取消
    ├── (b) interceptCh < text → process() turn 前 drainInterrupt()
    └── (c) InjectInput("interrupt", "text", ...) → 空闲时触发新处理
```

### 三种投递路径

| 路径 | 目标 | 触发时机 |
|------|------|---------|
| **(a) cancelLLM** | 取消进行中的 Provider HTTP 请求 | 拦截到 `context.Canceled` |
| **(b) interceptCh** | process() 工具循环中注入 `[打断消息]` | 每个 LLM call 前 `drainInterrupt()` |
| **(c) InjectInput** | eventLoop 空闲时启动新处理循环 | 无进行中请求时 |

### process() 内中断注入

```go
for turn := 0; turn < maxTurns; turn++ {
    if text := a.drainInterrupt(); text != "" {
        msgs = append(msgs, agentAPI.Message{
            Role: "system",
            Content: fmt.Sprintf("[打断消息] %s", text),
        })
    }
    // LLM call with cancellable context
    reqCtx, cancel := context.WithCancel(a.ctx)
    a.cancelLLM = cancel       // interceptLoop 可调用
    resp, err := provider.Chat(reqCtx, req)
    a.cancelLLM = nil
    cancel()
    // ... tool call loop ...
}
```
