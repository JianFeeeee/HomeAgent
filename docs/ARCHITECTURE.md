# HomeAgent 架构设计文档

> 基于 NextAgent 认知解耦架构，结合图记忆与工具调用系统的单二进制 AI 家庭助手。
> 参考设计：NextAgent — 严格的边界划分 + 工具调用；TrulyMEM — 自主图记忆系统

---

## 一、核心设计原则

### 1. 认知解耦架构（源自 NextAgent）

系统分为三个严格边界层：

```
┌──────────────────────────────────────────────────────────────────┐
│                    外层：IO 抽象层（唯一输入路径）                    │
│  IOManager ── Microphone / Camera / GPIO / HTTP / Sensors       │
│  所有外部输入 → InputEvent → inputCh                              │
└──────────────────────────┬───────────────────────────────────────┘
                           │
┌──────────────────────────▼───────────────────────────────────────┐
│                  中层：Agent 操作层（核心编排器）                    │
│  Agent Core ── 事件循环 consume inputCh                          │
│   ├─ 构建上下文（记忆索引 + 技能注入 + ToolDef）                    │
│   ├─ Provider.Chat() → CompletionResponse                       │
│   └─ 通过 IO 层输出 text + memory_candidate 事件                   │
└──────────────────────────┬───────────────────────────────────────┘
                           │
┌──────────────────────────▼───────────────────────────────────────┐
│                    内层：API 抽象层（唯一输出路径）                    │
│  Provider ── OpenAI / Ollama / LuaAdaptedProvider                 │
│  所有 LLM 调用通过 Provider.Chat() / ChatStream()                  │
└──────────────────────────────────────────────────────────────────┘
```

**三条核心规则：**
1. 所有外部输入 → 必须通过 `IOManager.InjectInput()` / `InjectText()` 注入
2. 所有 LLM 调用 → 必须通过 `Provider.Chat()` / `ChatStream()` 发出
3. Agent Core 不直接操作记忆系统，只发射 `memory_candidate` 事件，由 Memory Pipeline 异步消费

### 2. 自主记忆系统（源自 TrulyMEM）

```
User Input ──→ Agent Core ──→ Provider ──→ LLM Response
                      │
                      ▼
            IO 层发射 memory_candidate 事件
                      │
                      ▼
            Memory Pipeline Distiller 消费
                      │
                      ▼
            周期蒸馏 → Graph Memory (SQLite)
                      │
                      ▼
            删除原始会话文件
```

**关键约束：**
- Agent 上下文中只注入**记忆索引 + 摘要**，**永远不**注入原始文本
- Agent 必须通过 `memory_recall` tool call **主动查询** 获取完整记忆细节
- 记忆蒸馏完全异步、自主运行，不阻塞主流程

---

## 二、系统层次详解

### 2.1 IO 抽象层（`internal/agent/io/channel.go`）

**唯一输入路径。** 所有外部输入必须通过此层进入系统。

```
Device(Microphone) ─┐
Device(Camera)     ─┤
Device(GPIO)       ─┤──→ IOManager.InjectInput() → InputEvent → inputCh → Agent Core
Device(RobotArm)   ─┤
HTTP API           ─┘
```

**核心类型：**

| 类型 | 说明 |
|------|------|
| `InputEvent` | Source + Type + Payload — 所有外部输入的标准化格式 |
| `OutputEvent` | Target + Type + Payload — 所有输出的标准化格式 |
| `Device` | 接口：Name() / Type() / Tools() / Execute() |
| `DeviceType` | Input / Output / IO |
| `ToolDef` | Name + Description + Parameters — 与 LLM Function Calling 同构 |

**内置设备（目前为桩实现，待真正硬件接入）：**

| 设备 | 方向 | ToolDef 暴露 |
|------|------|-------------|
| Microphone | Input | `{name}_capture` — 录音 |
| Speaker | Output | `{name}_speak` — 语音播放 |
| Camera | Input | `{name}_capture` 拍照 + `{name}_stream` 视频流 |
| RobotArm | IO | `{name}_move` 移动 + `{name}_grip` 夹爪 |
| GPIO | IO | `{name}_gpio_write` + `{name}_gpio_read` |

**Device 与 Tool 同构原则：**
- 所有 Device 的 `Tools()` 返回 `[]ToolDef`，格式与 LLM Function Calling 完全一致
- Agent Core 自动收集所有 Device 的 ToolDef 合并到请求的 `tools` 字段
- Agent 通过 Function Calling 调用设备 capability

### 2.2 API 抽象层（`internal/agent/api/provider.go`）

**唯一输出路径。** 所有 LLM 请求通过此层发出。

```
Agent Core ──→ ProviderManager ──→ Provider.Chat()
                   │
         ┌─────────┼─────────┐
         ▼         ▼         ▼
   OpenAI    Ollama    LuaAdaptedProvider
                                 │
                         ┌───────┴───────┐
                         ▼               ▼
                   Lua Adapter    Base Provider
                   (transform)    (OpenAI/Ollama)
```

**Provider 接口：**

```go
type Provider interface {
    Name() string
    Chat(ctx, req) → (*CompletionResponse, error)
    ChatStream(ctx, req) → (<-chan StreamChunk, error)
}
```

| 实现 | 说明 |
|------|------|
| `OpenAIProvider` | 标准 OpenAI API 格式，支持 /chat/completions |
| `OllamaProvider` | Ollama /api/chat 格式，本地部署 |
| `LuaAdaptedProvider` | 通过 Lua 脚本转换请求/响应的适配 wrapper |

**Lua 适配器机制：**
- 适配器文件位于 `{dataDir}/adapters/*.lua`
- 每个适配器返回 Lua table 包含 `name` + `transform_request` + `transform_response`
- 首次运行时自动从 embed.FS 复制捆绑适配器（openai / deepseek / ollama / custom）
- 支持热重载（`POST /api/v1/adapters`）
- 用于兼容不同 API 供应商的请求/响应格式差异

### 2.3 Agent 操作层（`internal/agent/core/agent.go`）

**纯编排器，不涉及张量运算。**

```
eventLoop()
    │
    ▼  select on inputCh
handleInput(evt)
    │
    ▼
process(input)
    ├─ indexer.BuildContext(input) → 仅摘要+索引
    ├─ buildSystemPrompt() → 拼接 system prompt
    ├─ buildToolDefs() → 收集 IO 工具 + 记忆工具
    ├─ provider.Chat(req) → LLM 调用
    ├─ 记忆候选事件 → IO 层 EmitOutput("memory", "memory_candidate", ...)
    └─ 输出 → IO 层 EmitOutput(source, "text", ...)
```

**关键设计决策：**
- Agent 不直接持有 `memory.GraphDB` 引用 — 只通过 `Indexer` 构建上下文
- Agent 不直接调用 `memory.Commit()` — 只发射事件让 Pipeline 异步处理
- `Indexer.BuildContext()` 只返回内存索引摘要，不返回原始数据
- `buildSystemPrompt()` 中注入 `memory_recall` / `memory_commit` / `memory_introspect` 工具说明
- **所有输出走 IO 层** — 文本输出和记忆事件都通过 EmitOutput

### 2.4 图记忆系统（`internal/memory/graph.go`）

SQLite 三元组存储，支持实体-关系-实体的图遍历。

**数据库 Schema：**

```sql
entities(id PK, name UNIQUE, type, mention_count, created_at, updated_at)
relations(id PK, source_id FK→entities, target_id FK→entities,
          relation_type, confidence, status, session_id, turn_id,
          created_at, updated_at, date_bucket)
```

**核心操作：**

| 操作 | 说明 |
|------|------|
| `Commit(triples, sessionID, turnID)` | 写入三元组 → 自动 upsert 实体 + 插入关系 |
| `Recall(keywords, seedEntities, depth, session)` | 关键词搜索 → 图遍历 → 返回实体+关系 |
| `Purge(criteria, mode)` | 软/硬删除匹配的关系 |
| `Introspect()` | 统计信息：实体数、关系数、热点实体 |
| `Archive(days)` | 归档超过指定天数的关系 |

**Context Injection 机制（`internal/memory/indexer.go`）：**
- `BuildContext(userInput)` → 关键词提取 → `GraphDB.Recall()` → 构建摘要
- `FormatContext(context)` → 输出格式如：
  `【记忆索引】关联 N 个记忆实体，高频：A、B、C 索引: A, B, C | 需更多细节请用 memory_recall 查询`
- `BuildToolPrompt()` → 生成 `memory_recall/commit/introspect/purge` 工具的 prompt 说明
- `GetToolDefinitions()` → 返回 LLM Function Calling 格式的工具定义

### 2.5 记忆管道（`internal/memory/pipeline/pipeline.go`）

自主异步蒸馏管线。

```
Agent Core → EmitOutput("memory", "memory_candidate", {input, response})
    │
    ▼
onMemory(input, response) 回调
    │
    ▼
Append() → records[] 内存缓冲区
    │
    ▼ 每 10 分钟触发
distillLoop()
    │
    ▼
extractKeyTriples() → GraphDB.Commit()
    │
    ▼
cleanupRawFiles() 删除超期原始文件
```

**配置：**
- `Interval: 10m` — 每 10 分钟蒸馏一次
- `RetentionDays: 7` — 原始记录保留 7 天
- `BatchSize: 50` — 每批处理 50 条

### 2.6 技能系统（`internal/skill/manager.go`）

兼容 OpenClaw 格式的技能管理。

**技能格式：**
- `{skillsDir}/{name}/SKILL.md` — Markdown 描述文件
- `{skillsDir}/{name}/skill.json` — 可选的元数据文件

**注入机制：**
- `GetInjectedPrompt()` → 收集所有已启用的技能内容注入到 system prompt
- 支持安装/卸载/启用/禁用

### 2.7 Supervisor 守护进程（`internal/supervisor/daemon.go`）

```go
type Daemon struct {
    cfg       *types.Config
    cm        *container.Manager // Docker 容器管理
    nm        *network.Monitor   // 网络监控
    sm        *snapshot.Manager  // 快照管理
    agents    map[AgentID]*agentInstance
}
```

**职责：**
- Agent 生命周期管理（launch / restart / shutdown）
- 健康检查循环（心跳间隔 15s）
- 自动快照循环（快照间隔 10m）
- 故障恢复：失败 MaxRetries(3) 次后自动回滚到最近快照
- 网络监控：检查 LLM API 可达性，影响 Agent 健康状态

### 2.8 快照与回滚（`internal/snapshot/manager.go`）

基于 Docker commit/save/load 的版本管理。

**快照流程：**
`Docker Commit(container → image) → SaveImage(image → .tar) → 记录快照元数据`

**回滚流程：**
`Stop(container) → Remove(container) → LoadImage(.tar) → 创建新容器 → Start`

**策略：**
- 定时快照（每 10m）
- 操作前快照（PreAction — 可选）
- 限制最大保留（默认 20 个）
- 溢出时自动删除最旧的

### 2.9 网络监控（`internal/network/monitor.go`）

异步定时检查 LLM API 端点可达性。

**监控方式：**
- HTTP HEAD 请求到配置的 endpoints
- 并发检查（goroutine per endpoint）
- DNS 解析检查（fallback: google.com → baidu.com）
- 结果聚合：`LLMAPIReachable` + `DNSResolving` + 平均延迟

**影响：**
- 网络不可达 → Agent 状态变为 `HealthDegraded`
- 持续不可达 → 触发回滚策略

### 2.10 Lua 虚拟机（`internal/lua/vm.go`）

纯 Go 的 gopher-lua 5.1 VM，用于 API 格式适配器。

**功能：**
- 加载 `{adapterDir}/*.lua` 适配器脚本
- `CallTransform(name, input)` — 调用 adapter 的 transform_request
- `CallResponseTransform(name, raw)` — 调用 adapter 的 transform_response
- `ReloadAll()` — 热重载所有适配器
- 内置 mock 函数：`log()`, `json_encode()`, `http_get()`, `http_post()`

### 2.11 Embedder（`internal/embed/embedder.go`）

向量嵌入接口，支持文本相似度计算。

| 实现 | 说明 |
|------|------|
| `OllamaEmbedder` | 通过 Ollama API 获取嵌入向量（默认: nomic-embed-text, 768d） |
| `HashEmbedder` | 基于字符哈希的本地嵌入（无需外部依赖），用于备选方案 |

**可用性：** Embedder 已定义但尚未集成到记忆系统中。

### 2.12 Tokenizer（`internal/tokenizer/jieba.go`）

中文分词工具，基于 gojieba。全局单例，线程安全。

**功能：**
- `ExtractKeywords(text, topK)` — 提取关键词（TF-IDF 加权）
- `Cut(text)` — 分词
- `Tag(text)` — 词性标注

---

## 三、数据流全景

### 3.1 正常交互流程

```
外部输入（HTTP POST / voice / GPIO 事件）
          │
          ▼
IOManager.InjectInput()
  → InputEvent{Source, Type, Payload}
  → 推入 inputCh
          │
          ▼
Agent Core eventLoop()
  1. consume InputEvent
  2. Indexer.BuildContext(input) → 记忆摘要（仅索引+摘要）
  3. buildSystemPrompt() → 拼接 system + 记忆 + 技能 + 工具
  4. buildToolDefs() → IO 工具 + 记忆工具
  5. Provider.Chat(req) → LLM 响应
  6. EmitOutput(target, "text", response)
  7. EmitOutput("memory", "memory_candidate", {input, response})
          │
    ┌─────┴─────┐
    ▼           ▼
IO 输出      Memory Pipeline（异步）
    │           │
    ▼           ▼
HTTP        Append() → 周期蒸馏
Response    → GraphDB.Commit()
Speaker     → 清理原始文件
```

### 3.2 记忆查询流程

```
Agent 推理中决定调用 memory_recall
    │
    ▼
LLM 返回 tool_call: {name: "memory_recall", args: {query_intent: "..."}}
    │
    ▼
Agent Core 解析 tool_call → 调用 GraphDB.Recall(keywords)
    │
    ▼
返回实体+关系数据 → 注入后续 LLM 请求上下文
```

---

## 四、配置系统

配置文件: `/etc/homeagent/config.yaml`（YAML）。若文件不存在则使用默认配置。

```yaml
daemon:
  listen_addr: ":8080"
  data_dir: "/var/lib/homeagent"
  heartbeat_interval: 15s
  check_interval: 30s
  log_level: "info"

defaults:
  image: "homeagent/agent-base:latest"
  llm_endpoints:
    - "https://api.openai.com/v1"
  snapshot_policy:
    interval: 10m
    max_snapshots: 20
    pre_action: true
    post_action: false
  rollback_policy:
    max_retries: 3
    health_threshold: 3
    cooldown_period: 30s
    auto_rollback: true
  openclaw_enabled: true

agents: []
```

---

## 五、HTTP API

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/status` | 系统状态 |
| GET | `/api/v1/agents` | Agent 列表 |
| POST | `/api/v1/agents` | 创建 Agent |
| GET | `/api/v1/agents/{id}` | Agent 详情 |
| POST | `/api/v1/agents/{id}/start/stop/restart` | 操作 |
| GET | `/api/v1/agents/{id}/snapshots` | 快照列表 |
| POST | `/api/v1/agents/{id}/rollback/{snap}` | 回滚 |
| GET | `/api/v1/memory?q=关键词` | 记忆检索 |
| POST | `/api/v1/memory` | 写入三元组 |
| DELETE | `/api/v1/memory` | 删除记忆 |
| GET | `/api/v1/memory/context?q=...` | 获取上下文注入 |
| GET | `/api/v1/memory/tools` | 记忆工具定义 |
| GET | `/api/v1/skills` | 技能列表 |
| POST | `/api/v1/skills` | 安装技能 |
| DELETE | `/api/v1/skills?name=...` | 卸载技能 |
| GET | `/api/v1/adapters` | 适配器列表 |
| POST | `/api/v1/adapters` | 安装适配器 |
| DELETE | `/api/v1/adapters/{name}` | 删除适配器 |
| GET | `/api/v1/network` | 网络状态 |
| GET | `/api/v1/config` | 配置查看 |
| PUT | `/api/v1/config` | 配置更新 |
| GET | `/` | WebUI 仪表盘 |

---

## 六、数据目录结构

```
{dataDir}/
├── config.yaml          # 系统配置
├── memory/
│   ├── graph.db         # SQLite 图记忆数据库
│   └── raw/             # 原始会话记录文件
│       └── raw_*.jsonl
├── skills/              # 安装的技能
│   └── {name}/
│       ├── SKILL.md
│       └── skill.json
├── adapters/            # Lua API 格式适配器
│   ├── openai.lua
│   ├── deepseek.lua
│   ├── ollama.lua
│   └── custom.lua
└── snapshots/           # Docker 快照
    └── {agent_id}/
        └── snap_*.tar
```

---

## 七、构建与部署

**构建：**

```bash
make build       # 编译主二进制（~15MB）
make install     # 编译 + 安装到 /usr/local/bin
make test        # 运行测试
make fmt         # gofmt
make lint        # golangci-lint
```

**部署：**

- 单二进制：`homed -config /etc/homeagent/config.yaml -data /var/lib/homeagent`
- systemd：`deploy/homeagent.service`
- 依赖：Go 1.19+（CGo enabled，用于 go-sqlite3）；Docker 可选（快照/回滚）

---

## 八、设计限制与后续计划

### 已知限制
1. Agent Core 目前是单轮 tool_call 处理，尚未实现完整的多轮 tool 执行循环
2. Embedder 已定义但未接入图记忆 — 缺失向量相似度排序和 ANN 索引
3. 记忆蒸馏器使用简单启发式三元组提取，生产环境应调用 LLM 进行结构化抽取
4. Linux namespace 隔离（overlayfs）尚未实现作为 Docker 替代方案
5. Speaker / Microphone / Camera 均为桩实现，无实际 ALSA/PulseAudio/Video4Linux 驱动
6. 无真正的唤醒词检测
7. LuaAdaptedProvider 失败时无重试/降级逻辑
8. 无持久化消息历史管理（目前仅内存中保留最近 50 条）

### 路线图
- **近期**：完成 tool_call 执行循环 → 设备真正驱动 → 向量记忆增强
- **中期**：单二进制 namespace 隔离 → 唤醒词检测 → CI/CD 流水线
- **远期**：多 Agent 协作 → 分布式部署 → 联邦记忆
