# HomeAgent Plugin SDK

HomeAgent 外部插件开发工具包。用于开发独立于内核的 `.so` 动态插件。

## 目录结构

```
homeagent-sdk/
├── sdk/                    # Go SDK 包（import: gitcode.com/JianFeeeee/homeagent-sdk/sdk）
│   ├── plugin.go           # Plugin 接口、PluginSDK、ToolDef、ToolHandler
│   ├── settings.go         # SettingsAPI（插件配置读写）
│   ├── memory.go           # MemoryAPI / TextMemoryAPI / DocMemoryAPI
│   ├── knowledge.go        # KnowledgeAPI（知识库访问）
│   ├── llm.go              # LLMAPI（LLM 源管理）
│   └── API.md              # 完整 API 参考文档
├── hack/plugin-dev/        # 开发工具
│   ├── scaffold.sh         # 脚手架：生成新插件项目
│   ├── packager.sh         # 打包插件为 .hmap 分发包
│   └── testharness/        # 插件测试框架
├── example/                # 完整插件示例
│   ├── qq/                 # QQ 集成（对接 NapCat OneBot）
│   ├── files/              # 文件系统操作
│   ├── memo/               # 备忘提醒
│   └── web/                # 网络搜索与抓取
└── README.md
```

## 快速开始

### 前置条件

- Go 1.21+
- 运行中的 HomeAgent 内核（用于部署插件）

### 创建插件

```bash
git clone https://gitcode.com/JianFeeeee/homeagent-sdk.git
cd homeagent-sdk

# 用脚手架生成项目骨架
hack/plugin-dev/scaffold.sh myplugin ./plugins/myplugin

# 编辑插件代码
vim plugins/myplugin/plugin.go
```

### 插件接口

每个插件必须实现三个方法：

```go
type Plugin interface {
    Name() string                          // 插件名称
    Start(sdk *PluginSDK) error            // 启动：注册工具、阶段钩子等
    Stop() error                           // 停止：清理资源
}
```

入口函数签名（插件 .so 必须导出此函数）：

```go
func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error)
```

### 编译

```bash
# 从插件目录
cd plugins/myplugin && make

# 或手动编译
cd <SDK_REPO_ROOT> && go build -buildmode=plugin -o <PLUGIN_DIR>/plugin.so <PLUGIN_DIR>
```

### 部署

将插件目录放入 HomeAgent 内核的插件目录（`<dataDir>/plugins/<name>/`）：

```
<dataDir>/plugins/myplugin/
    plugin.json    — {"name": "myplugin", "version": "1.0", "entry": "plugin.so"}
    plugin.so      — 编译产物
```

内核启动时自动发现并加载。也可通过 WebUI 插件管理页面上传 `.hmap` 包安装。

## PluginSDK API 参考

完整 API 文档见 [sdk/API.md](sdk/API.md)，涵盖：

- **工具注册** — `RegisterTool`、`ToolDef`、`ToolHandler`
- **阶段钩子** — 7 个阶段的 `StageContext` 读写权限、工具归属插件字段和短路规则
- **输入投递** — `InjectText` / `InjectInterruptText` 两种投递方式
- **配置管理** — `SettingsAPI`，含自身/核心/跨插件配置
- **记忆访问** — 图记忆（`MemoryAPI`）、文档记忆（`DocMemoryAPI`）、文本记忆（`TextMemoryAPI`）
- **知识库** — `KnowledgeAPI` 搜索/添加/列表
- **LLM 管理** — `LLMAPI` 源切换
- **所有 SDK 类型定义** — `ToolCall`、`StageContext`、`Entity`、`Triple`、`ConfigDef` 等

## 打包分发

```bash
hack/plugin-dev/packager.sh plugins/myplugin
# 输出: dist/myplugin-0.1.0.hmap
```

`.hmap` 文件是一个 zip 包，内含：
- `plugin.json` — 清单文件（名称、版本、入口）
- `plugin.so` — 编译好的 Go 插件

通过 WebUI 插件管理器上传安装。

## 测试

SDK 提供测试框架 `testharness`，可加载 .so 并模拟调用：

```go
import "gitcode.com/JianFeeeee/homeagent-sdk/hack/plugin-dev/testharness"

func TestMyPlugin(t *testing.T) {
    h := testharness.New(t, "./plugin.so")
    defer h.Close()

    result, err := h.CallTool("myplugin_my_tool", map[string]interface{}{
        "input": "hello",
    })
    // ...
}
```

## 示例插件

每个示例目录下都有对应的 `README.md`，包含详细的设计讲解和源码引用。

| 示例 | 说明 | 详细文档 |
|------|------|----------|
| [QQ](example/qq/) | 对接 NapCat OneBot，15 个工具，涵盖消息/群/好友/文件/OCR | [讲解](example/qq/README.md) |
| [Files](example/files/) | 文件系统操作，4 种写入模式，分段读取，沙箱隔离 | [讲解](example/files/README.md) |
| [Memo](example/memo/) | 备忘管理，PreAction 注入 + 定时打断双提醒 | [讲解](example/memo/README.md) |
| [Web](example/web/) | DuckDuckGo 搜索 + 网页抓取，SSRF 防护，代理支持 | [讲解](example/web/README.md) |

## License

MIT