# HomeAgent 插件开发指南

## 概述

HomeAgent 的所有外部交互能力都来自插件。插件是独立运行的 Go 包，通过 `PluginSDK`（Go API）与内核交互。

每个插件需要实现一个非常简单的接口：

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
```

### 三种开发方式

| 方式 | 适用场景 | 复杂度 |
|------|---------|--------|
| **内置插件** | 随 HomeAgent 一起发布 | 简单，需合入主仓库 |
| **动态 .so 插件** | 独立分发的第三方插件 | 中等，需编译为 .so |
| **Lua 脚本插件** | 轻量快速原型 | 简单（预留功能） |

---

## 一、快速开始：内置插件

### 目录结构

```
internal/plugins/yourplugin/
    plugin.go       — 插件主文件
```

### 最小插件示例

```go
package yourplugin

import (
    "gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
    sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// init() 将插件注册到全局工厂表，内核启动时自动发现并加载。
func init() {
    plugin.RegisterFactory("yourplugin", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
        return New(name), nil
    })
}

type Plugin struct {
    name string
}

func New(name string) *Plugin {
    return &Plugin{name: name}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
    // 在这里初始化插件：启动 goroutine、注册工具、订阅事件等
    return nil
}

func (p *Plugin) Stop() error {
    // 清理资源
    return nil
}
```

### 注册到内核

在 `internal/plugins/all.go` 中添加空白导入：

```go
package plugins

import (
    _ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/yourplugin"
    // ... 其他插件
)
```

### 完整示例：定时器插件

`internal/plugins/timer/plugin.go` 是一个完整的内置插件示例：

```go
package timer

import (
    "fmt"
    "log"
    "sync"
    "time"

    "gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
    sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func init() {
    plugin.RegisterFactory("timer", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
        return New(name), nil
    })
}

type Plugin struct {
    name string
    mu   sync.Mutex
    wg   sync.WaitGroup
}

func New(name string) *Plugin {
    return &Plugin{name: name}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
    // 注册一个工具给 LLM 调用
    return s.RegisterTool("timer_set", sdk.ToolDef{
        Name:        "timer_set",
        Description: "设置一个定时提醒。倒计时结束后通过中断通道通知 agent。",
        Parameters: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "duration": map[string]interface{}{
                    "type":        "string",
                    "description": "持续时间，例如 5s, 2m, 1h",
                },
                "message": map[string]interface{}{
                    "type":        "string",
                    "description": "提醒内容",
                },
            },
            "required": []string{"duration", "message"},
        },
    }, func(args map[string]interface{}) (interface{}, error) {
        durStr, _ := args["duration"].(string)
        message, _ := args["message"].(string)
        dur, _ := time.ParseDuration(durStr)

        go func() {
            time.Sleep(dur)
            // 通过中断通道通知 agent
            s.InjectInterruptText("timer", "timer",
                fmt.Sprintf("timer: %s", message))
        }()

        return map[string]interface{}{
            "status":   "timer_set",
            "duration": durStr,
            "message":  message,
        }, nil
    })
}

func (p *Plugin) Stop() error {
    p.wg.Wait()
    return nil
}
```

---

## 二、插件开发详解

### PluginSDK 核心 API

#### 📤 IO — 输入输出

```go
// 向排队通道投递输入（按序处理）
sdk.InjectInput(source, channel string, payload map[string]interface{})

// 向中断通道投递输入（可打断当前 LLM 处理）
sdk.InjectInterrupt(source, channel string, payload map[string]interface{})

// 快捷方式：投递文本到排队通道
sdk.InjectText(source, channel, text string)

// 快捷方式：投递文本到中断通道
sdk.InjectInterruptText(source, channel, text string)

// 同步请求-响应：发送文本并等待回复（CLI 插件使用）
sdk.InjectTextSync(source, channel, text string) *OutputEvent

// 注册一个输出通道（LLM 可通过 output_send 工具选择发送到此通道）
sdk.RegisterChannel(name string, dev Device) error
sdk.UnregisterChannel(name string)
sdk.ListChannels() []ChannelInfo
```

#### 🛠️ 工具 — 让 LLM 可调用你的能力

```go
sdk.RegisterTool(name string, def ToolDef, handler ToolHandler) error
```

- `name`: 工具名称（LLM 通过此名称调用）
- `def`: 工具定义（描述 + 参数 JSON Schema）
- `handler`: 调用时执行的函数

工具定义示例：

```go
sdk.RegisterTool("weather_query", sdk.ToolDef{
    Name:        "weather_query",
    Description: "查询指定城市的天气",
    Parameters: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "city": map[string]interface{}{
                "type":        "string",
                "description": "城市名称，如 北京",
            },
        },
        "required": []string{"city"},
    },
}, func(args map[string]interface{}) (interface{}, error) {
    city, _ := args["city"].(string)
    // 查询天气并返回
    return map[string]interface{}{
        "city":    city,
        "temp":    25,
        "weather": "晴",
    }, nil
})
```

#### 🔌 阶段钩子 — 干预消息处理流

7 个阶段, 按执行顺序：

| 阶段 | 时机 | 用途 |
|------|------|------|
| `on_input` | 消息刚到达 Agent | 黑名单、限流、短路回复 |
| `pre_action` | 即将调用 LLM | 注入额外上下文 |
| `post_action` | LLM 返回结果 | 修改 LLM 输出 |
| `before_toolcall` | 工具调用前 | 审计、拒绝、改参 |
| `after_toolcall` | 工具执行后 | 脱敏、改写结果 |
| `before_output` | 输出前 | 调整格式 |
| `after_output` | 输出后 | 统计、记录 |

```go
sdk.RegisterStage(sdk.StageOnInput, func(ctx *sdk.StageContext) error {
    input := ctx.RawMessage
    // 检查是否是黑名单用户
    if ctx.UserID == "blocked_user" {
        resp := "你已被限制使用"
        ctx.Response = &resp  // 设置 Response 会短路后续阶段
        return nil
    }
    return nil
})
```

#### 📡 事件 — 订阅/发布系统事件

```go
// 订阅事件
unsub := sdk.Subscribe(events.EventType("tool_call"), func(evt *events.Event) {
    log.Printf("工具被调用: %v", evt.Payload)
})
defer unsub()  // 插件 Stop 时取消订阅

// 发布事件
sdk.Publish(&events.Event{
    Type: "my_event",
    Payload: map[string]interface{}{"key": "value"},
})
```

#### 🧠 能力访问

```go
// 记忆
sdk.Memory().Recall(query string) ([]MemItem, error)
sdk.Memory().Commit(triples []Triple) error

// 知识
sdk.Knowledge().Search(query string) ([]string, error)

// LLM 源管理
sdk.LLM().ListSources() []SourceInfo
sdk.LLM().SetSource(name string) error

// 配置（插件自身的配置表 config_<plugin_name>）
sdk.Settings().Get(key string) (interface{}, error)
sdk.Settings().Set(key string, value interface{}) error
sdk.Settings().List(prefix string) ([]string, error)
```

### 读取插件配置

插件有自己的配置表 `config_<插件名>`，例如 `config_mcp`：

```go
// 在 Start() 中
val, err := s.Settings().Get("api_key")
if err != nil {
    // 未配置
}
```

用户通过 WebUI 或 CLI 设置：

```go
// 读取其他插件的配置
s.Settings().GetPlugin("other_plugin", "some_key")

// 读取核心配置
s.Settings().GetCore("llm.model")
```

---

## 三、插件需要外部依赖时的做法

有些插件在初始化时需要内核中的组件（数据库、LLM 管理器等）。采用**包级变量注入**模式：

```go
package myplugin

var DataDir string  // 由 main.go 在 Load() 前设置

func init() {
    plugin.RegisterFactory("myplugin", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
        return New(name, DataDir), nil
    })
}
```

在 `cmd/homed/main.go` 中：

```go
myplugin.DataDir = filepath.Join(*dataDir, "myplugin_data")
pluginReg.Load(plgDir)  // 之后调用
```

---

## 四、动态 .so 插件

### 编译插件为 .so

```go
// myplugin/plugin.go
package main

import (
    sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return &myPlugin{name: name}, nil
}

type myPlugin struct {
    name string
}

func (p *myPlugin) Name() string { return p.name }
func (p *myPlugin) Start(s *sdk.PluginSDK) error {
    // 注册工具...
    return nil
}
func (p *myPlugin) Stop() error { return nil }
```

编译：
```bash
go build -buildmode=plugin -o plugin.so ./myplugin/
```

### 部署

```
<dataDir>/plugins/myplugin/
    plugin.json    — {"name": "myplugin", "version": "1.0", "description": "..."}
    plugin.so      — 编译产物
```

内核扫描时会自动发现并加载。无需修改 `main.go` 或 `all.go`。

---

## 五、最佳实践

1. **Start() 非阻塞** — 长时间运行的任务用 goroutine 启动，不要在 Start() 中阻塞
2. **Stop() 清理资源** — 关闭网络连接、停止 goroutine、取消订阅
3. **工具 name 唯一** — 工具名不能与其他插件冲突，建议用插件名前缀
4. **错误处理** — 工具 handler 返回 `error` 时，LLM 会收到错误信息并可能重试
5. **中断 vs 排队** — 需要打断当前 LLM 处理的用 `InjectInterruptText`，普通的用 `InjectText`
6. **配置优先** — 不要硬编码配置，用 `Settings().Get/Set` 读写插件配置

---

## 六、现有插件参考

| 插件 | 位置 | 特点 |
|------|------|------|
| Timer | `internal/plugins/timer/` | 最简单的完整示例，注册一个工具 + 中断反馈 |
| CLI | `internal/plugins/cli/` | Unix socket 监听 + 同步请求响应 |
| OpenClaw | `internal/plugins/openclaw/` | 解析 SKILL.md 文件注册工具 |
| WebUI | `internal/plugins/webui/` | HTTP 服务 + 依赖注入（Configure 模式） |
| MCP | `internal/plugins/mcp/` | JSON-RPC over stdio/SSE，连接 MCP 服务器 |

---

*了解项目整体目标？查看 [OVERVIEW.md](OVERVIEW.md)。*
*了解技术架构？查看 [ARCHITECTURE.md](ARCHITECTURE.md)。*
