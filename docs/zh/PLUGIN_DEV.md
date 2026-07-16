[English](../en/PLUGIN_DEV.md) | **中文**

# HomeAgent 插件开发指南

## 概述

HomeAgent 的所有外部交互能力都来自插件。插件通过 `PluginSDK`（Go API）与内核交互。

**SDK 仓库**：插件开发工具、模板代码和示例插件统一托管在
[homeagent-sdk](https://gitcode.com/JianFeeeee/homeagent-sdk) 仓库。

```bash
git clone https://gitcode.com/JianFeeeee/homeagent-sdk.git
cd homeagent-sdk
```

每个插件实现一个三方法接口：

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
| **动态 .so/.dll 插件（推荐）** | 独立分发的第三方插件 | 中等，使用 `plugindev` 工具链生成 |
| **内置插件** | 随 HomeAgent 一起发布 | 简单，需合入主仓库 |
| **Lua 脚本插件** | 轻量快速原型 | 简单，使用 `plugindev init --lua` 生成 |

---

## 一、快速开始：使用 plugindev 工具链

`plugindev` 是 SDK 仓库提供的统一插件开发工具链，支持 Go 和 Lua 两种插件类型。

### 安装

```bash
cd homeagent-sdk/tools/plugindev
go build -o plugindev.exe
# 将 plugindev.exe 加入 PATH 或直接使用
```

### 创建 Go 插件

```bash
plugindev init myplugin
cd myplugin
# 编辑插件代码
vim plugin.go
# 编译打包
plugindev build
# 输出: dist/myplugin_linux_amd64.hmap (或 windows_amd64)
```

### 创建 Lua 插件

```bash
plugindev init myluaplugin --lua
cd myluaplugin
# 编辑插件代码
vim main.lua
# 本地测试
lua main.lua
# 编译打包
plugindev build
# 输出: dist/myluaplugin_lua.hmap
```

### 模板项目结构

**Go 插件**：

```
myplugin/
├── plg.json       — 插件元信息（名称、版本、入口、目标平台）
├── main.go        — 入口点（非 Windows 或非 cgo 时编译）
├── plugin.go      — 插件实现（Plugin 接口）
├── go.mod         — Go 模块定义
└── README.md      — 说明文档
```

**Lua 插件**：

```
myluaplugin/
├── plg.json       — 插件元信息（entry: "main.lua", targets: "lua"）
├── main.lua       — 插件实现（Plugin 接口的 Lua 版本）
├── sdk.lua        — SDK 模拟层（支持 `lua main.lua` 独立测试）
└── README.md      — 说明文档
```

### 编译打包

`plugindev build` 会自动完成编译和打包：

```bash
cd myplugin
plugindev build
```

执行过程：
1. 读取 `plg.json` 确定目标平台
2. **Go 插件**：执行 `go build -buildmode=plugin`（Linux）或 `-buildmode=c-shared`（Windows）
3. **Lua 插件**：直接打包源码，无需编译
4. 生成 `plugin.json` 清单文件
5. 打包为 `.hmap` 分发包（zip 格式，内含 `plugin.json` + `plugin.so`/`plugin.dll`/`main.lua`）

输出在 `dist/` 目录：
```
dist/
├── myplugin_linux_amd64.hmap      # Go 插件 Linux 版
├── myplugin_windows_amd64.hmap    # Go 插件 Windows 版
└── myplugin_lua.hmap              # Lua 插件
```

### 安装部署

通过 PluginMgr HTTP API 安装：

```bash
# 内核 PluginMgr 监听 :9876
curl -X POST http://127.0.0.1:9876/plugins \
  -F "file=@dist/myplugin_linux_amd64.hmap"
```

或通过 WebUI 插件管理页面上传安装。

---

## 二、Go 插件开发详解

### 插件接口

```go
package main

import "gitcode.com/JianFeeeee/homeagent-sdk/sdk"

type Plugin struct {
    name string
    sdk  *sdk.PluginSDK
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
    p.sdk = s
    // 注册配置项、工具、阶段钩子等
    return nil
}

func (p *Plugin) Stop() error {
    // 清理资源
    return nil
}

// NewPluginFactory 创建插件实例（由 main.go 或 Windows bridge 调用）
func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return &Plugin{name: name}, nil
}
```

### 入口点

`main.go` 提供了 `NewPlugin` 导出函数，它是内核加载插件时的入口：

```go
//go:build !windows || !cgo

package main

import "gitcode.com/JianFeeeee/homeagent-sdk/sdk"

func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return NewPluginFactory(name, config)
}
```

对于 Windows `-buildmode=c-shared`，`plugindev build` 自动生成 C ABI bridge 代码，无需手动处理。

### PluginSDK 核心 API

#### 工具注册 — 让 LLM 可调用你的能力

```go
s.RegisterTool("weather_query", sdk.ToolDef{
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
    return map[string]interface{}{
        "city":    city,
        "temp":    25,
        "weather": "晴",
    }, nil
})
```

#### 阶段钩子 — 干预消息处理流

7 个阶段：

| 阶段 | 时机 | 用途 |
|------|------|------|
| `on_input` | 消息刚到达 Agent | 黑名单、限流、短路 |
| `pre_action` | 即将调用 LLM | 注入上下文 |
| `post_action` | LLM 返回结果 | 修改输出/工具列表 |
| `before_toolcall` | 工具调用前 | 审计、拒绝、改参 |
| `after_toolcall` | 工具执行后 | 脱敏、改写结果 |
| `before_output` | 输出前 | 格式适配 |
| `after_output` | 输出后 | 统计日志 |

```go
s.RegisterStage(sdk.StagePreAction, func(ctx *sdk.StageContext) error {
    ctx.Lock()
    ctx.ContextMsgs = append(ctx.ContextMsgs, map[string]interface{}{
        "role":    "system",
        "content": "注入的上下文内容",
    })
    ctx.Unlock()
    return nil
})
```

#### 配置管理

```go
// 注册配置项定义
s.Settings().RegisterDef(sdk.ConfigDef{
    Key:         "plugin.myplugin.api_key",
    Default:     "",
    Type:        "string",
    DisplayName: "API Key",
    Description: "API 密钥",
    Category:    "myplugin",
})

// 读写配置
val, err := s.Settings().Get("api_key")
s.Settings().Set("api_key", "new-value")

// 读取核心配置
s.Settings().GetCore("llm.model")

// 读取其他插件配置
s.Settings().GetPlugin("other_plugin", "some_key")
```

#### 输入投递

```go
// 排队投递（按序处理）
s.InjectInput(source, channel, eventType string, payload map[string]interface{})

// 中断投递（可打断当前 LLM 处理）
s.InjectInterrupt(source, channel, eventType string, payload map[string]interface{})

// 快捷方式
s.InjectText(source, channel, text string)
s.InjectInterruptText(source, channel, text string)
```

#### 事件订阅

```go
unsub := s.Subscribe("tool_call", func(evt *events.Event) {
    log.Printf("工具被调用: %v", evt.Payload)
})
defer unsub()
```

#### 能力访问

```go
// 记忆
s.Memory().Recall(query string) ([]MemItem, error)
s.Memory().Commit(triples []Triple) error

// 知识
s.Knowledge().Search(query string) ([]string, error)

// LLM 源管理
s.LLM().ListSources() []SourceInfo
s.LLM().SetSource(name string) error
```

#### 事件订阅（内置插件）

```go
// 订阅系统事件，返回取消订阅函数
unsub := s.Subscribe("tool_call", func(evt *events.Event) {
    log.Printf("工具被调用: %v", evt.Payload)
})
defer unsub()

// 发布事件
s.Publish(&events.Event{
    Type:    "custom_event",
    Payload: map[string]interface{}{"key": "value"},
})
```

#### IO 通道管理（内置插件）

```go
// 注册通道（绑定设备驱动），dev 必须实现 agentIO.Device 接口：
//   Name() string
//   Type() DeviceType
//   Description() string
//   Tools() []ToolDef
//   Execute(tool string, args map[string]interface{}) (interface{}, error)
//   Start() error
//   Stop() error
//   OutputCapabilities() OutputCapability
s.RegisterChannel("mydevice", deviceImpl)

// 注销通道
s.UnregisterChannel("mydevice")

// 列出所有通道
channels := s.ListChannels()
```

> **注意**：`Subscribe`、`Publish`、`RegisterChannel`、`UnregisterChannel`、`ListChannels`、`InjectInput`、`InjectInputSync`、`InjectInterrupt`、`InjectTextSync`、`InjectTextSyncNoMemory`、`OutputChan` 这些方法仅在内置插件中可用（`internal/sdk` 包），外部动态插件无法访问。外部插件请使用 `InjectText`、`InjectInterruptText`、`InjectTextNoMemory` 等公共 API。

---

## 三、Lua 插件开发详解

Lua 插件适合轻量级快速原型，无需 Go 编译环境，修改后直接重启内核即可生效。

### 插件结构

```lua
-- main.lua
local plugin = {
  name = "myluaplugin"
}

function plugin.start(sdk)
  sdk.log("info", "myluaplugin starting...")

  sdk.register_tool("myluaplugin_hello", {
    description = "A hello world tool",
    parameters = {
      type = "object",
      properties = {}
    }
  }, function(args)
    return { content = "Hello from myluaplugin plugin!" }
  end)

  sdk.log("info", "myluaplugin started")
end

function plugin.stop()
  sdk.log("info", "myluaplugin stopped")
end

return plugin
```

### SDK 模拟层

`sdk.lua` 提供纯 Lua 的 SDK 模拟实现，支持 `lua main.lua` 独立测试：

```bash
lua main.lua
# 输出:
# [lua-plugin] info: myluaplugin starting...
# [lua-plugin] register_tool: myluaplugin_hello
# [lua-plugin] info: myluaplugin started
```

在内核中运行时，`sdk.*` 全局变量由 Go 层注入，所有 `-- !impl` 标记的函数会被替换为真实实现。

### Lua SDK API

| 函数 | 说明 |
|------|------|
| `sdk.log(level, msg)` | 日志输出 |
| `sdk.register_tool(name, def, handler)` | 注册工具 |
| `sdk.register_stage(stage, handler)` | 注册阶段钩子 |
| `sdk.register_api(name)` | 注册 API |
| `sdk.get_setting(key)` | 读取配置 |
| `sdk.set_setting(key, value)` | 写入配置 |
| `sdk.inject_text(source, channel, text)` | 投递文本消息 |
| `sdk.inject_interrupt(source, channel, text)` | 中断投递 |
| `sdk.json.encode(val)` | JSON 编码 |
| `sdk.json.decode(str)` | JSON 解码 |
| `sdk.http.get(url)` | HTTP GET 请求（`-- !impl`） |
| `sdk.http.post(url, body, content_type)` | HTTP POST 请求（`-- !impl`） |

> **注意**：Lua 插件的 `sdk.register_stage` 阶段回调目前仅传递 `raw_message`、`user_id`、`phase` 三个字段，功能受限。复杂的阶段处理逻辑建议使用 Go 插件。

---

## 四、内置插件

内置插件使用 `init()` 自注册方式，编译进内核，无需单独部署。

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

---

## 五、最佳实践

1. `Start()` 非阻塞 — goroutine 启动长任务，不要阻塞 Start
2. `Stop()` 清理资源 — 关连接、停 goroutine、取消订阅
3. 工具名唯一 — 建议插件名前缀避免冲突
4. handler 返回 `error` 时 LLM 会收到并可能重试
5. 打断用 `InjectInterruptText`，普通投递用 `InjectText`
6. 配置用 `Settings().Get/Set`，不要硬编码
7. 外部 Go 插件独立编译，不依赖内核版本；内置插件才需随内核重新编译

---

## 六、示例插件参考

### SDK 仓库示例（`homeagent-sdk/example/`）

| 示例 | 类型 | 特点 |
|------|------|------|
| [memo](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/memo) | Go | 备忘管理，PreAction 注入 + 定时打断双提醒 |
| [files](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/files) | Go | 文件系统操作，4 种写入模式，沙箱隔离 |
| [web](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/web) | Go | DuckDuckGo 搜索 + 网页抓取，SSRF 防护 |
| [qq](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/qq) | Go | NapCat OneBot 对接，17 个工具 |
| [bili](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/bili) | Go | B 站视频下载（you-get） |
| [editdoc](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/editdoc) | Go | Office 文档编辑与格式转换 |
| [a2a](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/a2a) | Go | Agent-to-Agent 协议 |
| [ocr](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/ocr) | Go | 离线文字识别（Tesseract） |
| [sanitizer](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/sanitizer) | Go | 输出清洗过滤器 |
| [luaplugintest](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/luaplugintest) | Lua | Lua 插件 Hello World |
| [testlua](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/testlua) | Lua | Lua 插件示例 |

### 内置插件

| 插件 | 位置 | 特点 |
|------|------|------|
| Timer | `internal/plugins/timer/` | 最简单的完整示例，注册一个工具 + 中断反馈 |
| CLI | `internal/plugins/cli/` | Unix socket 监听 + 同步请求响应 |
| WebUI | `internal/plugins/webui/` | HTTP 服务 + 依赖注入 |

---

*了解项目整体目标？查看 [OVERVIEW.md](OVERVIEW.md)。*
*了解技术架构？查看 [ARCHITECTURE.md](ARCHITECTURE.md)。*
*SDK 仓库与开发工具？查看 [homeagent-sdk](https://gitcode.com/JianFeeeee/homeagent-sdk)。*
