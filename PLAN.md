# HomeAgent 架构与实施

## 架构概览

```
┌─────────────────────────────────────────────────────────────────────┐
│  homed (内核)                                                        │
│  ┌──────────┐ ┌──────────┐ ┌────────────┐ ┌───────────────────┐    │
│  │LLM源管理 │ │Agent编排 │ │记忆管理    │ │知识管理 (TF-IDF)   │    │
│  │Lua适配器 │ │主agent   │ │图/文本/    │ │                    │    │
│  │(协议转换) │ │子agent   │ │文档三层    │ │                    │    │
│  └──────────┘ └──────────┘ └────────────┘ └───────────────────┘    │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │ IO通道管理器 (IOManager)                                       │  │
│  │ 排队通道 (Queue) + 中断通道 (Interrupt)                        │  │
│  │ 核心回环 _consolidation_ (记忆消歧/系统维护)                    │  │
│  └──────────────────────────────────────────────────────────────┘   │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │ 阶段管道 (StageHost 7 阶段, 并行执行) + 事件总线 (EventBus)     │  │
│  └──────────────────────────────────────────────────────────────┘   │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │ PluginSDK — 内核对插件的完整 Go API                              │  │
│  │ IO/工具/阶段/事件/记忆/知识/LLM/配置                             │  │
│  └──────────────────────────────────────────────────────────────┘   │
├─────────────────────────────────────────────────────────────────────┤
│  内置插件 (编译入内核, init() 自注册)                                 │
│  internal/plugins/all.go (空白导入触发 init)                        │
│  ┌──────────┐ ┌──────────┐ ┌────────────┐ ┌──────────────┐        │
│  │ WebUI    │ │ CLI      │ │ OpenClaw   │ │ Timer        │        │
│  │ HTTP 服务│ │ Unix socket│ │SKILL.md→SDK│ │ timer_set 工具│        │
│  └──────────┘ └──────────┘ └────────────┘ └──────────────┘        │
├─────────────────────────────────────────────────────────────────────┤
│  动态插件 (<data>/plugins/<name>/ 按需加载)                          │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │ plugin.json 元数据 + plugin.so (Go -buildmode=plugin)         │  │
│  │ 或 main.lua (Lua 脚本, 预留)                                  │  │
│  │ Registry.Load() 自动扫描, 无 factory → tryLoadSO → tryLoadLua │  │
│  └──────────────────────────────────────────────────────────────┘   │
├─────────────────────────────────────────────────────────────────────┤
│  waiter (通用客户端)                                                 │
│  waiter -say "你好" → Unix socket → CLI 插件                        │
│  waiter (交互模式) → 同上                                            │
└─────────────────────────────────────────────────────────────────────┘
```

## 核心原则

1. **核心零 IO** — homed 不监听端口, 不启动 HTTP 服务, 不读 stdin
2. **一切外界交互都是插件** — 通过 PluginSDK 与核心交互
3. **插件自注册** — 每个插件的 `init()` 调用 `plugin.RegisterFactory()`, 内核无需硬编码
4. **动态 .so 加载** — 第三方插件编译为 `.so`, 放入 `plugins/<name>/plugin.so`
5. **PluginSDK 是内核"系统调用"** — 插件只能通过 SDK 访问核心能力
6. **Stage 并行执行** — 同阶段所有 handler goroutine 并发, StageContext 内嵌 RWMutex
7. **中断最高优先级** — 独立 `interceptLoop` 可随时打断进行中的 LLM 请求
8. **子 agent 是主 agent 的工具** — `spawn_child`/`child_result`, 不是 SDK 部分

## 插件注册体系

### 两种注册路径

| 路径 | 场景 | 实现 |
|------|------|------|
| **自注册 (init)** | 内置插件 (timer/cli/openclaw/webui) | 包 `init()` 调 `plugin.RegisterFactory(name, factory)` |
| **动态加载** | 第三方插件 | 扫描 `<data>/plugins/<name>/`, 读 manifest + .so |

### 自注册流程

```
internal/plugins/timer/plugin.go
  func init() {
      plugin.RegisterFactory("timer", func(name string, cfg map[string]interface{}) (sdk.Plugin, error) {
          return New(name), nil
      })
  }

internal/plugins/all.go
  package plugins
  import ( _ "timer" _ "cli" _ "openclaw" _ "webui" )
  // 空白导入触发所有 init() → RegisterFactory

cmd/homed/main.go
  cli.DefaultSocket = *cliSocket               // 注入运行时变量
  openclaw.SkillsDir = filepath.Join(...)
  webui.Configure(httpAddr, sup, mem, sk, ...)
  pluginReg.Load(plgDir)                       // 自动创建目录 + 加载
```

### 动态 .so 加载

插件目录结构:
```
<data>/plugins/myplugin/
    plugin.json    — 元数据 {name, version, description, author, entry}
    plugin.so      — Go -buildmode=plugin 编译, 导出 NewPlugin(name, config)
```

动态加载器 `internal/plugin/dynamic.go` 扫描 `.so`:
```go
func tryLoadSO(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
    p, _ := plugin.Open(filepath.Join(dir, "plugin.so"))
    sym, _ := p.Lookup("NewPlugin")
    fn := sym.(func(string, map[string]interface{}) (sdk.Plugin, error))
    return fn(name, config), nil
}
```

内置插件也可剥离为 .so, 当前保持 init 自注册。

## 输入双通道 + 中断打断

### 通道结构

```
IOManager
  ├── inputCh     (chan *InputEvent,  256) — 排队通道, 按序处理
  ├── interruptCh (chan *InputEvent,   64) — 中断通道, 可打断 LLM
  └── outputCh    (chan *OutputEvent, 256) — 输出通道
```

### 中断打断机制

```
timer 插件                          interceptLoop (独立 goroutine)
  │                                      │
  ├─ s.InjectInterruptText(...) ─────────┤
  │                                      │
  │                           ┌──────────┴──────────┐
  │                           │ (a) cancelLLM()      │ → 取消进行中的 HTTP 请求
  │                           │ (b) interceptCh <-   │ → process() 非阻塞读取
  │                           │ (c) InjectInput(...)  │ → 空闲时 eventLoop 消费
  │                           └─────────────────────┘
  │                                      │
  ▼                                      ▼
interruptCh                    process() 工具循环
                                    │
                         每个 turn 开始前:
                         drainInterrupt() → 注入 [打断消息] system msg
```

**三种投递路径 (interceptLoop)**:
- **(a)** `cancelLLM()` — 直接取消当前 Provider HTTP 请求, 捕获 `context.Canceled`
- **(b)** `interceptCh <- text` — `process()` 每轮 LLM 调用前 `drainInterrupt()`, 注入 `[打断消息]` 到上下文
- **(c)** `InjectInput("interrupt", "text", ...)` — `eventLoop` 在空闲时收到新输入, 启动新处理循环

## 阶段管道 (Stage Pipeline)

7 个阶段, **并行执行**:

```
on_input → pre_action → post_action ↔ before_toolcall/after_toolcall → before_output → after_output
```

| 阶段 | 触发时机 | 插件读写权限 | 典型用途 |
|---|---|---|---|
| `on_input` | 消息到 Agent，零处理 | 可读写 `raw_message`，可设置 `response` 短路 | 黑名单、限流、自定义指令前缀 |
| `pre_action` | Memory+Context 就绪，LLM 调用前 | 可读写 `context_messages`（追加/修改） | 注入 RAG 结果、插入时政 context |
| `post_action` | LLM 返回文本 + 工具调用列表 | 可读写 `llm_text`、`tool_calls`、`context_messages` | 敏感词过滤、强制 redirect 工具 |
| `before_toolcall` | 单个工具调用执行前 | 可读写 `tool_call.name`、`tool_call.args`，设置 `deny=true` 拒绝 | 审计高危操作、OS 命令白名单 |
| `after_toolcall` | 单个工具执行完毕 | 可读写 `tool_result` | 脱敏数据库结果、排序搜索结果 |
| `before_output` | 最终文本就绪，output_send 前 | 可读写 `final_text`，可设置 `skip_output=false` | 添加表情/at 前缀、多平台格式适配 |
| `after_output` | output_send 已调用 | 只读 `final_text` | 统计日志、触发后续流程 |

## 配置体系 (ConfigRegistry)

全部配置持久化在 SQLite:

| 表 | 用途 | 访问 |
|----|------|------|
| `config` | 核心配置 (LLM/daemon/agent/paths) | SettingsAPI.GetCore/SetCore |
| `config_<plugin>` | 插件独立配置 | SettingsAPI.Get/Set/List |
| | 跨插件读写 | GetPlugin/SetPlugin/ListPlugin/Dump |

### 配置元信息 (ConfigDef)

每个配置项注册时附带元数据（类型、中文描述、分类、选项等），WebUI/CLI 自动发现并渲染。

## 插件包系统

### 包格式 (.hmap)

标准 ZIP 文件，扩展名 `.hmap` (HomeAgent Plugin Package):

```
myplugin-1.0.0.hmap
├── plugin.json      必要 — {name, version, entry, description, author, ...}
├── plugin.so        Go 插件 (entry = "plugin.so")
├── main.lua         Lua 插件 (entry = "main.lua")
├── SKILL.md         Skill 插件 (entry = "SKILL.md")
├── skill.json       可选 — 默认配置
└── assets/          可选 — 插件资源
```

### pluginmgr 内置插件

| 工具 | 功能 | 关键参数 |
|------|------|---------|
| `plugin_install` | 从 URL 安装 `.hmap` | `{url: string}` |
| `plugin_list` | 列出已安装外部插件 | `{}` |
| `plugin_remove` | 卸载 | `{name: string}` |
| `plugin_info` | 详情（含文件清单） | `{name: string}` |

HTTP API（`127.0.0.1:{随机端口}`，默认无鉴权）：

| 方法 | 路径 | 作用 |
|------|------|------|
| `GET` | `/plugins` | 列表 |
| `GET` | `/plugins/{name}` | 详情 |
| `POST` | `/plugins` | 安装（JSON `{url}` 或二进制 .hmap 上传） |
| `DELETE` | `/plugins/{name}` | 卸载 |

## 内核入口 (cmd/homed/main.go)

初始化顺序:

```
1. 确定 dataDir (默认 ./data/ ，二进制同级)
2. 创建目录结构 (plugins/memory/knowledge/...)
3. 初始化 SQLite ConfigRegistry，SeedDefaults 写入默认路径
4. 基础设施 → 记忆/技能/Lua/监督/追踪/IO/事件
5. LLM Provider 管理
6. 阶段管道 StageHost + 插件注册表 Registry
7. 注入内置插件依赖 (cli/webui/healthcheck/pluginmgr)
8. Registry.Load(plgDir) → 自注册 + 动态加载
9. Agent 启动 (eventLoop + interceptLoop + distillLoop)
10. 等待信号 → 关机
```

### 目录结构

```
cmd/
  homed/main.go                — 内核入口 (零 IO)
  waiter/main.go               — CLI 客户端 (Unix socket)
internal/
  sdk/                          ★ PluginSDK (Go API)
    plugin.go                   — Plugin 接口 + PluginSDK 结构体
    memory.go / knowledge.go    — 记忆/知识包装
    settings.go / llm.go        — 配置/LLM 源
  agent/
    core/
      agent.go                  — Agent: eventLoop/interceptLoop/process
      context.go                — RelevanceContext (TF-IDF)
      stages.go                 — StageHost (并行阶段管道)
    api/provider.go             — Provider 接口 + LuaAdaptedProvider
    io/channel.go               — IOManager (Queue/Interrupt/Output)
  plugin/
    registry.go                 — 注册表: 生命周期, Load, RegisterFactory
    manifest.go                 — PluginManifest (plugin.json)
    dynamic.go                  — .so 动态加载器
    plugin.go                   — SKILL 插件解析
  plugins/
    all.go                      — 空白导入触发所有内置插件 init()
    timer/  cli/  openclaw/  webui/  pluginmgr/   — 内置插件
  events/bus.go                 — 系统事件总线
  memory/                        — 三层记忆 (Context→Document→Graph)
  knowledge/                     — 知识库
  config/registry.go              — 配置中心
  tracker/                       — overlayfs 变更追踪
  supervisor/                    — 守护管理
  skill/                         — 技能管理
  lua/vm.go                      — Lua VM (LLM 协议适配)
pkg/types/                       — 类型定义
docs/ARCHITECTURE.md             — 完整架构文档
```

## 目录路径配置化

### SeedDefaults 新增路径配置

| Key | 默认值 | 说明 |
|-----|--------|------|
| `core.plugin.dir` | `<dataDir>/plugins` | 插件安装目录 |
| `core.memory.graph` | `<dataDir>/memory/graph.db` | 图数据库 |
| `core.memory.text` | `<dataDir>/memory/text` | 文本记忆目录 |
| `core.memory.documents` | `<dataDir>/memory/documents` | 文档记忆目录 |
| `core.knowledge.path` | `<dataDir>/knowledge` | 知识库目录 |
| `core.skills.path` | `<dataDir>/skills` | 技能目录 |
| `core.log.path` | `<dataDir>/log` | 日志目录 |

所有路径配置项均有 `ConfigDef` 元信息 + `password` 类型保护敏感字段。

### 裸二进制启动

- `-data` 默认值从硬编码 `/var/lib/homeagent` 改为 `""`（自动检测）
- 自动检测：读取 `/proc/self/exe` 确定二进制所在目录 → `filepath.Join(exeDir, "data")`
- 首次运行自动创建完整目录结构
- 后续通过调整 ConfigRegistry 的值自定义各存储路径

## 构建与验证

```bash
make build              # 编译 homed + waiter
./build/homed -data /tmp/ha   # 启动
./build/waiter -say "你好"    # 发送消息
```

要求: Go 1.19+, CGo (go-sqlite3), Linux (Unix socket + overlayfs).
