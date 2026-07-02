# HomeAgent 架构参考

> 基于 NextAgent 认知解耦架构 + TrulyMEM 自主图记忆 + OneBot 协议。

## 一、分层架构

```
┌──────────────────────────────────────────────────────────┐
│                    IO 抽象层（唯一输入路径）                   │
│  Device(Mic/Speaker/Camera/GPIO/OneBot-QQ/PluginDevice)  │
│  所有外部输入 → InputEvent → inputCh                       │
│  输出通道: 能力声明(text/file/image/audio/structured)       │
│  路由表: 输入源 → 默认输出通道                               │
└──────────────────────────┬───────────────────────────────┘
                           │
┌──────────────────────────▼───────────────────────────────┐
│                   Agent 操作层（核心编排器）                  │
│  eventLoop() consume inputCh                              │
│   ├─ RelevanceContext（TF-IDF 相关性管理）                   │
│   ├─ 工具循环（Provider.Chat → tool_calls → Execute → ...) │
│   ├─ 记忆索引注入（图索引 + 文档摘要）                        │
│   └─ 心跳蒸馏（30min：向量同步 + 冷归档 + 同义合并）          │
└──────────────────────────┬───────────────────────────────┘
                           │
┌──────────────────────────▼───────────────────────────────┐
│                  API 抽象层（唯一输出路径）                    │
│  Provider.Chat() → DeepSeek API / OpenAI / Ollama         │
│  LuaAdapter 做请求/响应格式转换                             │
└──────────────────────────────────────────────────────────┘
```

**三条核心规则：**
1. **所有外部输入** → 必须通过 `IOManager.InjectInput()` / `InjectText()` 注入
2. **所有 LLM 调用** → 必须通过 `Provider.Chat()` 发出
3. **Agent 不直接操作记忆系统**，只发射 `memory_candidate` 事件，由 Memory Pipeline 异步消费

## 二、子系统详解

### 2.1 IO 抽象层（唯一输入路径）

所有外部输入必须通过此层进入系统。

```
Device(Microphone) ─┐
Device(OneBot-QQ)  ─┤
Device(Plugin)     ─┤──→ IOManager → InputEvent → inputCh → Agent
Device(GPIO)       ─┤
HTTP API           ─┘
```

**核心类型：**

| 类型 | 说明 |
|------|------|
| `InputEvent` | Source + Type + Payload — 所有外部输入的标准化格式 |
| `OutputEvent` | Target + Type + Payload + OutputChannel — 输出路由 |
| `Device` | 接口：Name/Type/Description/Tools/Execute/Start/Stop/OutputCapabilities |
| `DeviceType` | Input / Output / IO |
| `ToolDef` | Name + Description + Parameters + Handler — 与 LLM 函数调用同构 |
| `OutputCapability` | 位掩码：text, file, image, audio, structured |

**内置设备：**

| 设备 | 方向 | 工具 |
|------|------|------|
| Microphone | Input | capture — 录音 |
| Speaker | Output | speak — 语音播放 |
| Camera | Input | capture 拍照 |
| OneBot QQ | IO | send_private/send_group/get_group_member_info/get_group_list |
| PluginDevice | IO | 插件声明工具 |
| GPIO | IO | gpio_write/gpio_read |

**输出通道能力校验：**
- 每个 Device 声明 `OutputCapabilities()` → 位掩码
- `output_send` 工具发送前校验通道是否支持文本
- `output_list_channels` 只列出有输出能力的通道

### 2.2 API 抽象层（唯一输出路径）

```
Agent Core → Provider.Chat()
                │
        ┌───────┴───────┐
        ▼               ▼
  OpenAIProvider    LuaAdapter
  (DeepSeek API)    (格式转换)
```

| 实现 | 说明 |
|------|------|
| `OpenAIProvider` | 标准 OpenAI API 格式，DeepSeek v4 flash 默认 |
| `LuaAdaptedProvider` | 通过 Lua 脚本转换请求/响应的适配 wrapper |

Lua 适配器位于 `{dataDir}/adapters/*.lua`，每个适配器返回 `name` + `transform_request` + `transform_response`。

### 2.3 Agent 操作层（核心编排器）

```
eventLoop() → select on inputCh
    │
    ▼
handleInput(evt) → processTextInput(input)
    │
    ├─ 1. RelevanceContext.Append(input)
    ├─ 2. 构建 prompt: personal.md + 图索引 + 文档摘要 + 上下文 + 工具
    ├─ 3. 工具循环（最多 10 轮）
    │     LLM → tool_calls → Execute → 结果注入 → 下一轮
    ├─ 4. 追加响应到上下文
    ├─ 5. RelevanceContext.Prune() → 低分事件→文档记忆归档
    ├─ 6. OutputEvent → 输出通道
    └─ 7. memory_candidate → TextMemory + Distiller → GraphDB
```

**人格注入：** personal.md 加载一次，固定在 system prompt 最前，永不漂移。

**工具分发：**
```
executeToolCall(tc)
  ├─ memory_* → executeMemoryTool
  ├─ knowledge_* → executeKnowledgeTool
  ├─ doc_* → executeDocTool
  ├─ output_* → executeOutputChannel/Send/ListChannels
  └─ 其他 → io.ExecuteTool → Device.Execute
```

### 2.4 记忆系统

三层分级，自顶向下逐渐持久化、抽象化：

```
Layer 1: 上下文（RelevanceContext）
  内存环形缓冲，TF-IDF 余弦相似度排序
  每次响应后保留 top 30，低分→文档记忆

Layer 2: 文档记忆（document.Store）
  JSON 文件 + TF-IDF 向量索引（字符 bigram）
  冷文档（72h 未访问 + ≤2 次）→ 图数据库

Layer 3: 图数据库（memory.GraphDB）
  SQLite 三元组（实体-关系-实体）
  只注入索引（实体名+类型+提及次数）到 prompt
  定期重整：向量同步 + bigram Jaccard 同义合并
```

### 2.5 知识系统

独立于记忆，agent 主动学习。

```
knowledge/{category}/content.md
    │
    ▼
TF-IDF 向量索引（字符 bigram）
    │
    ▼
工具: knowledge_search / knowledge_create / knowledge_list
```

### 2.6 插件系统

OpenClaw SKILL.md 兼容。

```
Plugin（容器）
  ├─ 元数据: name, version, author
  ├─ IOConfig（可选）: 声明 IO 端口
  ├─ Device（可选）: 原生 Go 设备
  └─ Tools: LLM 工具定义

Registry:
  ├─ NativeFactory: "qq" → onebot.NewDevice
  ├─ SetIOManager: 绑定 IO 管理器
  └─ Reload(): 原子化重载
```

**热插拔流程：**
1. 扫描 `plugins/` 目录
2. 原生工厂优先，无工厂则 LoadSKILL.md
3. 新设备 Start（预先启动）
4. `IOManager.AtomicSwapDevices()` 原子替换设备表 + 路由表
5. 旧设备 Stop（后台 goroutine）

### 2.7 OneBot QQ 通道

```
OneBot 前端（go-cqhttp/Lagrange）
    │  Reverse WebSocket
    ▼
OneBot Client（internal/onebot/）
  ├─ 事件: message/notice/request → IO InputEvent
  ├─ 动作: send_private_msg / send_group_msg / get_group_info / ...
  ├─ 自动重连（指数退避 1s→30s）
  └─ 事件 handler 注入 IO 层
    │
    ▼
Device（internal/onebot/device.go）
  ├─ Tools: qq_send_private_msg, qq_send_group_msg, etc.
  ├─ Execute → Client.SendAction
  └─ Start → Connect + 注册事件 handler
```

### 2.8 变更追踪器

Overlayfs 文件变更追踪：

```
PreAction(tool) → 记录文件 hash
PostAction(tool) → diff → ChangeSet{ID, Tool, Files[]SHA256}
Rollback() → 用 overlayfs 下层恢复
```

健康检查失败 → `tracker.Rollback()`。

### 2.9 Supervisor 守护进程

```
Daemon:
  ├─ SetTracker() → 绑定变更追踪器
  ├─ RegisterAgent("main") → 注册主 agent
  └─ 健康检查周期 → LLM API 可达性
       └─ 连续失败 → tracker.Rollback()
```

## 三、配置

```yaml
daemon:
  listen_addr: ":8080"
  data_dir: "/var/lib/homeagent"
  heartbeat_interval: 15s
  check_interval: 30s
llm:
  provider: "openai"
  model: "deepseek-v4-flash"
  base_url: "https://api.deepseek.com/v1"
  api_key: "${DEEPSEEK_API_KEY}"
  temperature: 0.7
  max_tokens: 4096
```

## 四、数据目录

```
{dataDir}/
├── memory/
│   ├── graph.db           # SQLite 图数据库
│   ├── text/              # JSONL 文本记忆
│   ├── documents/         # JSON 文档记忆
│   └── raw/               # 原始记录（蒸馏后删除）
├── knowledge/             # 知识库
│   └── {name}/content.md
├── plugins/               # 插件
│   └── {name}/
│       ├── SKILL.md
│       └── skill.json
├── skills/                # 技能
├── adapters/              # Lua 适配器
├── personal/              # 人格
│   └── personal.md
├── changesets/            # 变更追踪记录
└── snapshots/             # 快照
```

## 五、HTTP API

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/chat/completions` | OpenAI 兼容对话 |
| GET | `/api/v1/knowledge` | 知识查询/列表 |
| POST | `/api/v1/knowledge` | 创建知识 |
| DELETE | `/api/v1/knowledge?name=` | 删除知识 |
| GET | `/api/v1/status` | 系统状态 |
| GET | `/api/v1/config` | 配置查看 |

## 六、构建与部署

```bash
make build       # 编译主二进制
make run         # 编译 + 启动（数据 /tmp/homeagent）
make install     # 安装到系统
make test        # 运行测试
```

依赖：Go 1.19+ (CGo enabled for go-sqlite3)。单二进制部署。
