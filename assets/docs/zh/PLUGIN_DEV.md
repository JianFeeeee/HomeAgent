[English](../en/PLUGIN_DEV.md) | **中文**

# HomeAgent 插件开发指南

<img src="../../assets/branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

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

<img src="../../assets/branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 一、快速开始：使用 plugindev 工具链

`plugindev` 是 SDK 仓库提供的统一插件开发工具链，支持 Go 和 Lua 两种插件类型。

### 安装

```bash
cd homeagent-sdk/tools/plugindev
go build -o plugindev
# 将 plugindev 加入 PATH 或直接使用
```

### SDK 版本管理

`plugindev sdk` 子命令管理本地 SDK 版本：

```bash
plugindev sdk list       # 列出已安装的 SDK 版本
plugindev sdk current    # 显示当前使用的 SDK 版本
plugindev sdk latest     # 显示最新可用版本
plugindev sdk install v0.8.0  # 安装指定版本
plugindev sdk use v0.8.0      # 切换使用版本
plugindev sdk path       # 显示当前 SDK 路径
```

SDK 存储在 `~/.homeagent/plugindev/sdk/<version>/`，`plugindev init` 自动读取当前 SDK 版本填充 `go.mod`。

### 源码调试

`plugindev debug` 直接用解释器执行插件源码并输出调用轨迹，无需编译环境：

```bash
plugindev debug [dir]   # dir 默认当前目录
```

### 创建 Go 插件

```bash
plugindev init myplugin
cd myplugin
# 编辑插件代码
vim plugin.go
# 编译打包
plugindev build          # 默认多平台 bundle（见下节）
# 输出: dist/myplugin_bundle.hmap
# 单平台构建：
plugindev build --no-bundle
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
├── plg.json       — 插件元信息（名称、版本、入口、目标平台 targets）
├── plugin.go      — 插件实现（Plugin 接口 + 导出函数 NewPlugin）
├── go.mod         — Go 模块定义
├── README.md      — 说明文档
└── thirdpart/     — 外部源码存放目录（可选）
```

编译时自动生成 C ABI bridge 文件（`z_bridge_gen.go` + `z_entry.c`），无需手动创建。

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
plugindev build                      # 默认 bundle 模式（多平台合集）
plugindev build --no-bundle          # 单平台构建（仅当前 plg.json targets）
plugindev build --target linux/amd64 # 在 targets 基础上追加一个目标
plugindev build --outdir dist        # 指定输出目录（默认 dist）
plugindev build --sdk-path <path>    # 指定 SDK 路径（覆盖 go.mod replace）
plugindev build --replace <mod@path> # 追加 go.mod replace 指令（可多次）
```

执行过程：
1. 读取 `plg.json` 的 `targets`/`bundle` 字段确定构建目标（bundle 模式优先，见下节）
2. 自动生成 C ABI bridge 代码（`z_bridge_gen.go` + `z_entry.c`，Windows 仅 `z_bridge_gen.go`）
3. **Go 插件**：执行 `go build -buildmode=c-shared`（生成 `.so` / `.dylib` / `.dll`）
4. **Lua 插件**：直接打包源码，无需编译（打包内容：`plugin.json` + `main.lua`，以及可选的 `README.md`、`LICENSE`、`thirdpart/*.lua`）
5. 生成 `plugin.json` 输出清单
6. 打包为 `.hmap` 分发包（zip 格式，内含 `plugin.json` + 二进制）

### plg.json（项目配置）vs plugin.json（输出清单）

| 文件 | 用途 | 关键字段 |
|------|------|---------|
| `plg.json` | 项目元信息，由开发者维护 | `targets` — 单平台构建目标（如 `"linux/amd64,windows/amd64"`）；`bundle` — 多平台合集开关（默认 `true`）|
| `plugin.json` | 构建产物清单，`plugindev build` 自动生成 | `entry` — 入口文件名；`platforms` — 声明的支持平台 |

每个目标生成单独的 `.hmap`，二进制文件名由平台决定：

| 平台 | 二进制 |
|------|--------|
| Linux | `plugin.so` |
| macOS | `plugin.dylib` |
| Windows | `plugin.dll` |

### 构建目标与多平台打包（bundle）

**`plugindev build` 默认就是 bundle 模式**（`plg.json` 未显式写 `"bundle": false` 时）：一次编译 linux/amd64 + darwin/amd64 + windows/amd64，生成包含所有平台二进制的单 `.hmap`，输出清单自动添加 `platforms` 字段。安装时核心自动选择当前平台的二进制，跳过其他平台。

```bash
plugindev build              # 默认 bundle，输出 dist/myplugin_bundle.hmap
plugindev build --bundle     # 显式开启 bundle（同上）
plugindev build --no-bundle  # 关闭 bundle，按 plg.json 的 targets 逐平台构建
```

注意：
- bundle 模式下 `plg.json` 的 `targets` 字段被忽略，固定构建上述三个平台
- 跨平台交叉编译需要对应工具链（如 Linux 上构建 darwin 需 clang/macOS SDK）；缺少工具链时编译会失败，此时使用 `--no-bundle` 只构建当前平台
- 单平台输出文件名：`{name}_{os}_{arch}.hmap`，如 `myplugin_linux_amd64.hmap`

输出在 `dist/` 目录：
```
dist/
├── myplugin_bundle.hmap           # 默认 bundle：多平台合集
├── myplugin_linux_amd64.hmap      # --no-bundle 后：Linux 版
├── myplugin_windows_amd64.hmap    # --no-bundle 后：Windows 版
├── myplugin_darwin_amd64.hmap     # --no-bundle 后：macOS 版
└── myplugin_lua.hmap              # Lua 插件
```

### 安装部署

通过 PluginMgr HTTP API 安装，支持三种方式：

```bash
# 1. 从 URL 安装（仅支持 http/https，流式下载不落盘）
curl -X POST http://127.0.0.1:9876/plugins \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/myplugin.hmap"}'

# 2. 从本地路径安装（读取指定文件，不移动原文件）
curl -X POST http://127.0.0.1:9876/plugins \
  -H "Content-Type: application/json" \
  -d '{"path": "/path/to/myplugin.hmap"}'

# 3. 直接上传二进制
curl -X POST http://127.0.0.1:9876/plugins \
  --data-binary @dist/myplugin.hmap
```

`9876` 为 pluginmgr 本地监听端口（默认仅监听 127.0.0.1，无鉴权）。

安装后需调用 `/api/v1/plugins/reload` 或重启内核生效。

也可通过 WebUI 插件管理页面上传安装，或走 WebUI 的 HTTP API（端口默认 8080，需 `api_key` 鉴权，内部代理到 pluginmgr）：

```bash
curl -X POST http://127.0.0.1:8080/api/v1/plugins \
  -H "Authorization: Bearer <api_key>" \
  -H "Content-Type: application/json" \
  -d '{"path": "/path/to/myplugin.hmap"}'
```

---

<img src="../../assets/branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

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

`plugindev init` 生成的 `plugin.go` 中直接包含 `NewPlugin` 导出函数，它是内核加载插件时的入口：

```go
func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return &Plugin{name: name}, nil
}
```

编译时 `plugindev build` 根据目标平台自动生成 C ABI bridge 代码（`z_bridge_gen.go` + `z_entry.c`），无需手动编写。Windows DLL 和 Linux/macOS .so 共享同一入口。

### PluginSDK 核心 API

#### 工具注册 — 让 LLM 可调用你的能力

```go
s.RegisterTool("weather_query", sdk.ToolDef{
    Name:        "weather_query",
    Description: "查询指定城市的天气",
    NoMemory:    false,                            // false=输出参与记忆计算，true=跳过计算
    // Cleaner:  func(output string) string {     // 可选：输出参与向量化/jieba/蒸馏前的清洗
    //     return extractJSON(output, "content")
    // },
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

##### NoMemory 与 Cleaner 说明

`NoMemory` 和 `Cleaner` 是 `ToolDef` 上的两个可选字段，控制工具输出在**记忆计算层**（向量化、jieba 分词、蒸馏）中的行为：

- **`NoMemory`**（默认 `false`）：设为 `true` 时，工具输出不参与任何记忆计算（向量、分词、蒸馏），但原文保留在 Context 和 Document 中，LLM 注意力不受影响。适用场景：`cmd_run`（命令输出含不可控噪音）、文件上传/删除等纯操作工具。

- **`Cleaner`**（可选）：函数签名 `func(output string) string`。注册后，工具输出在参与向量化/jieba/蒸馏前先经过此函数过滤。典型用途：SQL 查询去前缀、JSON 包裹提取 `content` 字段。原文始终不变，Cleaner 只影响计算层输入。

决策矩阵：

```
工具输出 → 对 LLM 注意力有信号价值？
  ├── 否 → NoMemory=true（输出保留原文，跳过计算层）
  └── 是 → 有可控噪音？
       ├── 是 → Cleaner 过滤后参与计算
       └── 否 → 正常记忆，无需额外处理
```

> **注意**：`Cleaner` 是 Go `func` 类型（`json:"-"`），不能跨 C ABI 边界序列化，因此 C/C++/Rust 等远程插件无法使用。**Lua 插件不受此限**：def 表中直接传 Lua 函数即可（`cleaner = function(text) return text end`），Go 桥接层会在计算层调用时逐次回调 Lua。

#### 阶段钩子 — 干预消息处理流

7 个阶段：

| 阶段 | 时机 | 用途 |
|------|------|------|
| `on_input` | 消息刚到达 Agent | 黑名单、限流、短路 |
| `pre_action` | 即将调用 LLM | 注入上下文 |
| `post_action` | LLM 返回结果 | 修改输出/工具列表 |
| `before_toolcall` | 工具调用前 | 审计、拒绝、改参 |
| `after_toolcall` | 工具执行后 | 脱敏、改写结果 |
| `before_output` | 输出前 | 格式适配、泄漏清洗 |
| `after_output` | 输出后 | 统计日志 |

```go
// 全局监听：所有插件的阶段事件
s.RegisterStage(sdk.StagePreAction, func(ctx *sdk.StageContext) error {
    ctx.Lock()
    ctx.ContextMsgs = append(ctx.ContextMsgs, map[string]interface{}{
        "role":    "system",
        "content": "注入的上下文内容",
    })
    ctx.Unlock()
    return nil
})

// 仅自己工具：仅监听自己注册的 tool 的 before_toolcall/after_toolcall
s.RegisterStage(sdk.StageBeforeToolcall, myHandler, sdk.StageScopeOwnTools)
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
// 普通投递（按序处理）
s.InjectText(source, channel, text string)

// 中断投递（可打断当前 LLM 处理）
s.InjectInterruptText(source, channel, text string)

// 不记入记忆
s.InjectTextNoMemory(source, channel, text string)
```

#### 输入通道注册 — 声明外部消息源

```go
s.RegisterInputChannel("qq", sdk.ChannelDef{
    NoMemory: true,
    Cleaner: func(text string) string {
        return strings.TrimSpace(text)
    },
})
```

`ChannelDef` 控制通道在记忆计算层的行为：

| 字段 | 默认 | 说明 |
|------|------|------|
| `NoMemory` | `false` | 通道输入/输出不参与向量化/关键词/蒸馏，原文保留 |
| `Cleaner` | `nil` | `func(string) string` 计算层过滤（不改原文） |

噪声源通道（QQ 群消息、RSS 等）建议 `NoMemory: true`。

#### 输出通道注册 — 声明输出目的地

```go
s.RegisterOutputChannel("email", 1, "邮件发送", sdk.ChannelDef{
    NoMemory: true,
}, func(args map[string]interface{}) (interface{}, error) {
    to, _ := args["to"].(string)
    subject, _ := args["subject"].(string)
    body, _ := args["body"].(string)
    return map[string]interface{}{"status": "sent"}, nil
})
```

参数：`name`（路由名）、`caps`（1=文本/2=富文本/4=文件/8=图片）、`desc`、`def`（ChannelDef）、`handler`（处理函数）。

#### 事件订阅

```go
import "gitcode.com/JianFeeeee/homeagent-sdk/sdk"

unsub := s.Events().Subscribe(sdk.EventToolCall, func(evt *sdk.Event) {
    log.Printf("工具被调用: %v", evt.Payload)
})
defer unsub()
```

#### 能力访问

```go
// 图记忆（实体-关系存储）
entities, relations, err := s.Memory().Recall([]string{"关键词"}, 2)

// 文档记忆（向量存储）
docs := s.DocMemory().Query("查询文本", 3)

// 知识库
results, err := s.Knowledge().Search("查询", 5)

// LLM 源管理
s.LLM().ListSources() // 返回 []string
s.LLM().SetSource("deepseek")
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

#### 输入投递（内置插件）

```go
// 排队投递（按序处理）
s.InjectInput(source, channel, eventType string, payload map[string]interface{})

// 同步投递（等待响应）
resp := s.InjectInputSync(source, channel, eventType string, payload map[string]interface{})

// 中断投递（可打断当前 LLM 处理）
s.InjectInterrupt(source, channel, eventType string, payload map[string]interface{})

// 同步文本投递（快捷方式）
resp := s.InjectTextSync(source, channel, text string)
resp := s.InjectTextSyncNoMemory(source, channel, text string)

// 获取输出通道
outputCh := s.OutputChan()
```

> **注意**：`Subscribe`、`Publish`、`RegisterChannel`、`UnregisterChannel`、`ListChannels`、`InjectInput`、`InjectInputSync`、`InjectInterrupt`、`InjectTextSync`、`InjectTextSyncNoMemory`、`OutputChan` 这些方法仅在内置插件中可用（`internal/sdk` 包），外部动态插件无法访问。外部插件请使用 `InjectText`、`InjectInterruptText`、`InjectTextNoMemory` 等公共 API。

---

<img src="../../assets/branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 三、Lua 插件开发详解

Lua 插件适合轻量级快速原型，无需 Go 编译环境，修改后直接重启内核即可生效。

### 执行模型

Lua 插件运行在内核进程内的 gopher-lua 解释器中（单 Lua 状态 + 互斥锁），与 Go 插件的执行模型有本质区别：

- **被动回调模型**：`main.lua` 仅在加载时执行一次，此后插件的工具、阶段钩子、输出/输入通道、注册 API 全部由内核事件驱动回调 Lua 函数；插件不能自己启动后台任务。
- **无并发/无常驻服务能力**：Lua 侧没有 goroutine、协程调度、`os`/`io` 库和 socket 监听能力，唯一主动出站通道是 `sdk.http.get/post`（同步请求）。任何阻塞循环都会持锁卡死该插件的所有调用。
- **常驻服务（如监听端口、后台轮询、定时任务）请使用 Go 插件**（工具链编译的 `.so`/`.dll`，可自行启动 goroutine，参见 webui/cli 插件）。Lua 插件的等价做法是事件驱动：注册工具/阶段钩子/通道由内核回调，或经 `sdk.http` 与外部进程交互。

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

Lua 插件的 `sdk.*` API 与外部插件（C ABI / 工具链编译的 `.so`/`.dll`）能力完全对齐：注册类函数调用即时报错（抛 Lua error），数据类函数统一返回 `(result, err)`，`err` 为 nil 表示成功。核心未装配的子系统（如 SocialAPI）返回空值而非报错。

**注册类**

| 函数 | 说明 |
|------|------|
| `sdk.log(level, msg)` | 日志输出 |
| `sdk.register_tool(name, def, handler)` | 注册工具；`def` 支持 `description`、`parameters`、`no_memory`、`cleaner` |
| `sdk.register_stage(stage, handler, scope)` | 注册阶段钩子；`scope` 为 `nil`/`"global"`（默认）或 `"own_tools"`（仅 `before_toolcall`/`after_toolcall` 且工具属于本插件时触发） |
| `sdk.register_api(name)` | 注册 API |
| `sdk.register_output_channel(name, caps, desc, def, handler)` | 注册输出通道；`def` 支持 `no_memory`、`cleaner` |
| `sdk.register_input_channel(name, def)` | 注册输入通道；`def` 同上 |
| `sdk.set_auto_restart(enabled)` | 崩溃时内核自动拉起插件 |

**阶段钩子上下文**

`register_stage` 的 handler 收到完整上下文（与外部插件一致）：`raw_message`、`user_id`、`group_id`、`phase`、`llm_text`、`final_text`、`no_memory`、`response`（已响应时）、`tool_calls`、`tool_results`。

**IO 与配置**

| 函数 | 说明 |
|------|------|
| `sdk.get_setting(key)` / `sdk.set_setting(key, value)` | 本插件配置读写 |
| `sdk.settings.get_core/set_core/list_core(key)` | 核心配置读写 |
| `sdk.settings.get_plugin/set_plugin/list_plugin(plugin, key)` | 其他插件配置读写 |
| `sdk.settings.list/defs/dump/plugins(prefix)` | 配置查询 |
| `sdk.settings.register_def(def)` | 注册配置项定义（WebUI 展示） |
| `sdk.inject_text(source, channel, text)` | 投递文本消息 |
| `sdk.inject_interrupt(source, channel, text)` | 中断投递 |
| `sdk.inject_text_no_memory(source, channel, text)` | 免记忆投递 |

**数据类（与 C ABI 对齐，均返回 `(result, err)`）**

| 子表 | 函数 |
|------|------|
| `sdk.memory.*` | `recall(query, depth)`、`commit({triples})`、`introspect()`、`merge(source, target)`、`purge(criteria, hard)` |
| `sdk.doc.*` | `query(text, top_k)`、`insert({id,title,content})`、`remove(id)`、`stats()` |
| `sdk.knowledge.*` | `search(query, limit)`、`add(tag, content)`、`list()` |
| `sdk.text_memory.*` | `append({role,content,timestamp,channel})` |
| `sdk.llm.*` | `list_sources()`、`set_source(name)`、`current_source()` |
| `sdk.social.*`（只读） | `get_person(name)`、`get_network(name, depth)`、`get_trait(name, trait)`、`get_relations(name)`、`list_persons()` |
| `sdk.json.*` | `encode(val)`、`decode(str)` |
| `sdk.http.*` | `get(url)`、`post(url, body, content_type)` |

---

<img src="../../assets/branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

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

<img src="../../assets/branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 五、最佳实践

1. `Start()` 非阻塞 — goroutine 启动长任务，不要阻塞 Start
2. `Stop()` 清理资源 — 关连接、停 goroutine、取消订阅
3. 工具名唯一 — 建议插件名前缀避免冲突
4. handler 返回 `error` 时 LLM 会收到并可能重试
5. 打断用 `InjectInterruptText`，普通投递用 `InjectText`
6. 配置用 `Settings().Get/Set`，不要硬编码
7. 外部 Go 插件独立编译，不依赖内核版本；内置插件才需随内核重新编译

---

<img src="../../assets/branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 六、插件管理

### CLI 命令

```bash
/plugin list                  # 列出所有插件及状态（已加载/已禁用）
/plugin disable <name>        # 禁用插件（立即生效，不再接收输入）
/plugin enable <name>         # 启用插件（重启后恢复加载）
/plugin reload                # 重载所有插件
```

### WebUI

Dashboard 插件列表的操作列提供「禁用/启用」按钮。禁用 WebUI 自身时会弹出确认对话框，防止误操作。

### 内置插件 API

```go
pmgr := s.PluginMgr()
pmgr.DisablePlugin("qq", "admin")       // 禁用
pmgr.EnablePlugin("qq")                  // 启用
list := pmgr.ListDisabledPlugins()       // 列出已禁用插件
loaded := pmgr.ListLoadedPlugins()       // 列出已加载插件
pmgr.IsPluginDisabled("qq")              // 检查是否已禁用
pmgr.ReloadPlugins()                     // 重载所有插件
```

内部机制：禁用记录存储在 SQLite `disabled_plugins` 表（`name`, `disabled_at`, `disabled_by`），禁用立即生效（插件不再接收输入），完全卸载需重启内核。

> **注意**：`PluginMgr()` 仅内置插件可用，外部动态插件无法直接调用。

---

<img src="../../assets/branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 七、示例插件参考

### SDK 仓库示例（`homeagent-sdk/example/`）

| 示例 | 类型 | 特点 |
|------|------|------|
| [weather](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/weather) | Go | 天气查询（wttr.in），演示 NoMemory/Cleaner/阶段钩子/通道/文本记忆 |
| [luademo](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/luademo) | Lua | Lua 全功能示例，覆盖 v0.8.0 Lua SDK 全部 API 面 |
| [qq](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/qq) | Go | NapCat OneBot 对接，17 个工具，输入/输出通道完整对接 |
| [memo](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/memo) | Go | 备忘管理，PreAction 注入 + 定时打断双提醒 |
| [files](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/files) | Go | 文件系统操作，4 种写入模式，沙箱隔离 |
| [browser](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/browser) | Go | 网络搜索、网页抓取（SSRF）、浏览器渲染（合并自 web/webfetch） |
| [bili](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/bili) | Go | B 站视频下载（yt-dlp） |
| [editdoc](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/editdoc) | Go | Office 文档编辑与格式转换 |
| [a2a](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/a2a) | Go | Agent-to-Agent 协议 |
| [ocr](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/ocr) | Go | 离线文字识别（Tesseract） |
| [sanitizer](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/sanitizer) | Go | 输出清洗过滤器 |
| [calendar](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/calendar) | Go | 日历管理 |
| [rss](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/rss) | Go | RSS 订阅 |
| [ai_image](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/ai_image) | Go | AI 图片生成 |
| [music](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/music) | Go | 音乐播放 |

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
