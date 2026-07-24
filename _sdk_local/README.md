# HomeAgent SDK

HomeAgent 插件开发 SDK，用于构建与 HomeAgent 平台交互的智能插件。

## SDK API 接口

### Plugin 接口

插件需实现 `Plugin` 接口：

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
```

### PluginSDK 方法

通过 `Start(sdk *PluginSDK)` 注入的 SDK 实例提供以下方法：

| 分类 | 方法 | 说明 |
|------|------|------|
| 阶段钩子 | `RegisterStage(stage, handler, scope...)` | 注册阶段回调，scope 可选：`StageScopeGlobal`（全局，默认）或 `StageScopeOwnTools`（仅自己工具） |
| 输出通道 | `RegisterOutputChannel(name, caps, desc, handler)` | 注册输出通道，caps 为能力位掩码 |
| 工具注册 | `RegisterTool(name, def, handler)` | 注册工具供 LLM 调用 |
| 插件 API | `RegisterPluginAPI(name)` | 注册插件 API 供其他插件访问 |
| 图记忆 | `Memory()` | 访问图记忆 API（实体-关系存储） |
| 文本记忆 | `TextMemory()` | 访问文本记忆 API（时序事件） |
| 文档记忆 | `DocMemory()` | 访问文档记忆 API（向量存储） |
| 社交图谱 | `Social()` | 访问社交图谱 API（外部插件只读） |
| 知识库 | `Knowledge()` | 访问知识库 API |
| LLM | `LLM()` | 访问 LLM 提供商管理 API |
| 设置 | `Settings()` | 访问设置 API |
| 事件 | `Events()` | 访问事件订阅器（外部插件仅订阅） |
| 注入 | `InjectText(source, channel, text)` / `InjectInterruptText(source, channel, text)` / `InjectTextNoMemory(source, channel, text)` | 向管道注入文本 |
| 自动重启 | `SetAutoRestart(enabled)` / `AutoRestart()` | 控制崩溃自动重启 |

### 阶段钩子

```go
// 全局监听所有插件的阶段事件
sdk.RegisterStage(StagePreAction, func(ctx *StageContext) error { return nil })

// 仅监听自己注册的工具的 before_toolcall / after_toolcall
sdk.RegisterStage(StageBeforeToolcall, myHandler, StageScopeOwnTools)
```

### 输出通道

```go
sdk.RegisterOutputChannel("my-channel", CapText|CapFile, "通道描述", handler)
```

handler 接收三个参数：
- `payload` (string) — 消息载荷。`type=text` 时直接填文字，`type=file/image` 时填 URL
- `meta` (string) — 可选的 JSON 路由元数据（如 `{"group_id":123,"user_id":456}`）
- `type` (string) — 载荷类型，枚举值见下

能力标志位：

| 标志 | 值 | 说明 |
|------|----|------|
| `CapText` | 1 | 纯文本输出 |
| `CapFile` | 2 | 文件输出 |
| `CapImage` | 4 | 图片输出 |
| `CapAudio` | 8 | 音频输出 |
| `CapStructured` | 16 | 结构化数据输出 |

type 枚举值：

| 值 | 说明 |
|----|------|
| `text` | 纯文本 |
| `voice` / `audio` | 语音 |
| `image` | 图片 |
| `file` | 文件 |

### IOInjector 通道路由

| 方法 | 说明 |
|------|------|
| `InjectText(source, channel, text)` | 注入文本，记入内存，路由到指定通道 |
| `InjectInterruptText(source, channel, text)` | 注入中断文本，打断当前处理，路由到指定通道 |
| `InjectTextNoMemory(source, channel, text)` | 注入文本，不记入内存，路由到指定通道 |

`source` 标识来源，`channel` 指定目标输出通道。

### Triple 扩展字段

Triple 数据结构新增字段：

- `Confidence` — 置信度（0.0~1.0）
- `SubjectType` — 主体类型
- `ObjectType` — 客体类型

### New 构造函数

`New()` 由内核在加载插件时调用，插件开发者无需手动构造 PluginSDK：

```go
func New(name string, sett SettingsAPI, regTool ToolRegistrar, regStage StageRegistrar, regAPI APIRegistrar, regOutput OutputChannelRegistrar) *PluginSDK
```

插件开发者只需实现 `Plugin` 接口并导出 `NewPlugin()` 入口函数。

## plugindev 工具链

`plugindev` 提供插件开发全流程支持：

| 命令 | 说明 |
|------|------|
| `plugindev init` | 初始化插件项目（生成 plg.json、入口模板） |
| `plugindev build` | 构建插件，输出 .hmap 包 |
| `plugindev clean` | 清理构建产物 |
| `plugindev debug` | 本地调试模式运行插件 |

支持 **Go** 和 **Lua** 两种插件语言。

### plg.json 清单格式

```json
{
  "name": "my-plugin",
  "version": "1.0.0",
  "lang": "go",
  "entry": "main.go",
  "description": "插件描述",
  "channels": ["my-channel"],
  "dependencies": {}
}
```

### .hmap 包格式

`.hmap` 为 ZIP 归档，包含：

- `plugin.json` — 插件元数据
- `plugin.so` — Go 编译产物（Linux）
- `plugin.dll` — Go 编译产物（Windows）
- `main.lua` — Lua 插件入口（Lua 插件时）

## 插件生命周期

### 启动与停止

- `Start(sdk *PluginSDK) error` — 插件启动，接收 SDK 实例
- `Stop() error` — 插件停止，释放资源

### 自动重启

```go
sdk.SetAutoRestart(true)
// 查询状态
enabled := sdk.AutoRestart()
```

插件崩溃时平台自动拉起，保障服务可用性。

## 受限 SDK vs 完整 SDK

外部插件（第三方分发）使用**受限 SDK**，仅暴露安全子集：

| 受限 API | 允许操作 |
|----------|----------|
| `SocialAPI` | 只读：`GetPerson`、`GetTrait`、`GetRelations`、`GetNetwork`、`ListPersons` |
| `EventSubscriber` | 仅订阅：`Subscribe`（无 `Publish`） |

内部插件（平台内置）拥有完整 SDK 访问权限，包括 SocialAPI 写操作和 EventPublisher。

## 示例插件

| 插件 | 说明 |
|------|------|
| a2a | Agent-to-Agent 协议通信 |
| bili | Bilibili 视频下载 |
| browser | 网络搜索、网页抓取、浏览器渲染（合并自 web/webfetch） |
| editdoc | 文档编辑 |
| files | 文件管理 |
| memo | 备忘录/记忆 |
| ocr | 光学字符识别 |
| qq | QQ 消息集成 |
| sanitizer | 内容清洗/安全过滤 |

## 构建与安装

### 构建

```bash
plugindev build
```

输出 `.hmap` 包到项目目录。

### 安装

通过 pluginmgr HTTP API 安装：

```bash
curl -X POST http://<host>:<port>/api/plugins/install \
  -F "package=@my-plugin.hmap"
```

或手动将 `.hmap` 放入插件目录后重启平台。
