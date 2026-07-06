# PluginSDK API 参考

HomeAgent 内核通过 `*sdk.PluginSDK` 向插件暴露所有能力。插件在 `Start(sdk *PluginSDK)` 中接收此对象。

## Plugin 接口

所有插件必须实现此接口：

```go
type Plugin interface {
    Name() string                          // 返回插件名称，与注册名一致
    Start(sdk *PluginSDK) error            // 初始化：注册工具、阶段钩子等
    Stop() error                           // 清理：关连接、停 goroutine
}
```

### 入口函数

`.so` 动态插件必须导出的工厂函数：

```go
func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error)
```

- `name`: 插件目录名，也是配置命名空间
- `config`: 插件依赖注入（预留，当前为空）
- 返回 `Plugin` 实例

## PluginSDK 总览

```
PluginSDK
├── 工具注册
│   └── RegisterTool(name, def, handler) error
├── 阶段钩子
│   └── RegisterStage(stage, handler)
├── 输入投递
│   ├── InjectInterruptText(source, channel, text)
│   ├── InjectText(source, channel, text)
│   └── InjectTextNoMemory(source, channel, text)
├── 配置管理 (SettingsAPI)
│   ├── Get(key) / Set(key, value)
│   ├── GetCore(key) / SetCore(key, value)
│   ├── GetPlugin(plugin, key) / SetPlugin(plugin, key, value)
│   ├── List(prefix) / ListCore(prefix)
│   ├── RegisterDef(def) / Defs(prefix)
│   ├── Dump() / Plugins()
├── 记忆访问
│   ├── Memory() -> MemoryAPI
│   ├── TextMemory() -> TextMemoryAPI
│   ├── DocMemory() -> DocMemoryAPI
├── 知识库
│   └── Knowledge() -> KnowledgeAPI
└── LLM 管理
    └── LLM() -> LLMAPI
```

## 工具注册

### RegisterTool

```go
func (s *PluginSDK) RegisterTool(name string, def ToolDef, handler ToolHandler) error
```

向 LLM 注册一个可调用的工具。`name` 必须全局唯一，建议用插件名前缀避免冲突。

### ToolDef

```go
type ToolDef struct {
    Name        string                 `json:"name"`        // 工具名
    Plugin      string                 `json:"plugin,omitempty"` // 工具所属插件
    Description string                 `json:"description"` // LLM 看到的描述
    Parameters  map[string]interface{} `json:"parameters"`  // JSON Schema
}
```

`Parameters` 使用 JSON Schema 格式描述参数。示例：

```go
sdk.ToolDef{
    Name:        "weather_query",
    Description: "查询指定城市的天气",
    Parameters: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "city": map[string]interface{}{
                "type":        "string",
                "description": "城市名称",
            },
        },
        "required": []string{"city"},
    },
}
```

### ToolHandler

```go
type ToolHandler func(args map[string]interface{}) (interface{}, error)
```

- `args`: LLM 传入的参数，key 为参数名，value 为对应值
- 返回值: `interface{}` 会被 JSON 序列化后返回给 LLM
- 返回 `error` 时 LLM 会收到错误信息并可能重试

```go
func(args map[string]interface{}) (interface{}, error) {
    city, _ := args["city"].(string)
    return map[string]interface{}{
        "temp": 25, "weather": "晴",
    }, nil
}
```

错误结果推荐返回含 `isError` 字段的 map，而非返回 error（避免 LLM 重试）：

```go
return map[string]interface{}{
    "isError": true,
    "content": "错误描述",
}, nil
```

### ToolCall / ToolResult

阶段钩子中访问的 LLM 工具调用和结果结构：

```go
type ToolCall struct {
    ID        string                 `json:"id"`        // 调用 ID
    Name      string                 `json:"name"`      // 工具名
    Plugin    string                 `json:"plugin,omitempty"` // 工具所属插件
    Arguments map[string]interface{} `json:"arguments"` // 参数
}

type ToolResult struct {
    CallID  string      `json:"call_id"` // 对应 ToolCall.ID
    Name    string      `json:"name"`    // 工具名
    Plugin  string      `json:"plugin,omitempty"` // 工具所属插件
    Success bool        `json:"success"`
    Result  interface{} `json:"result"`  // handler 返回值
}
```

## 阶段钩子

### RegisterStage

```go
func (s *PluginSDK) RegisterStage(stage Stage, handler StageHandler)
```

在消息处理管道的指定阶段注入逻辑。同一阶段可注册多个 handler，按注册顺序执行。

### Stage

```go
type Stage string

const (
    StageOnInput        Stage = "on_input"         // 消息到达，零处理
    StagePreAction      Stage = "pre_action"        // LLM 调用前，上下文就绪
    StagePostAction     Stage = "post_action"       // LLM 返回后
    StageBeforeToolcall Stage = "before_toolcall"   // 单个工具执行前
    StageAfterToolcall  Stage = "after_toolcall"    // 单个工具执行后
    StageBeforeOutput   Stage = "before_output"     // 最终输出前
    StageAfterOutput    Stage = "after_output"      // 输出发送后
)
```

### StageHandler

```go
type StageHandler func(ctx *StageContext) error
```

### RegisterStageOwnTools

```go
func (s *PluginSDK) RegisterStageOwnTools(stage Stage, handler StageHandler)
```

仅在 `before_toolcall` / `after_toolcall` 阶段监听**当前插件自己的工具调用**。

适用场景：
- QQ 插件只审核 `qq_send_*` 自己的发送工具
- Web 插件只改写 `web_fetch` 自己的结果
- Files 插件只审计 `files_write` 自己的写操作

其他阶段会退化成普通 `RegisterStage`。

### StageContext

```go
type StageContext struct {
    mu               sync.RWMutex
    RawMessage       string                    // 原始输入文本（on_input 可改写）
    UserID           string                    // 用户标识
    GroupID          string                    // 群组标识
    ContextMsgs      []map[string]interface{}  // 上下文消息列表（pre_action 可注入）
    LLMText          string                    // LLM 返回文本（post_action 可改写）
    ReasoningContent string                    // LLM 推理过程文本
    TokenUsage       map[string]int            // Token 用量
    ToolCalls        []ToolCall                // LLM 请求的工具调用
    ToolResults      []ToolResult              // 工具执行结果
    FinalText        string                    // 最终输出文本（before_output 可改写）
    Response         *string                   // 设置后短路管道
    Phase            Stage                     // 当前阶段
    Memory           []MemItem                 // 召回的记忆
    NoMemory         bool                      // 是否跳过记忆
    Extra            map[string]interface{}    // 扩展字段
}
```

**阶段权限矩阵**：

| 字段 | on_input | pre_action | post_action | before_toolcall | after_toolcall | before_output | after_output |
|------|----------|------------|-------------|-----------------|----------------|---------------|--------------|
| RawMessage | 读写 | - | - | - | - | - | - |
| ContextMsgs | - | 读写 | - | - | - | - | - |
| LLMText | - | - | 读写 | - | - | - | - |
| ToolCalls | - | - | 读写 | 读写 | - | - | - |
| ToolCall.deny | - | - | - | 读写 | - | - | - |
| ToolResults | - | - | - | - | 读写 | - | - |
| FinalText | - | - | - | - | - | 读写 | 只读 |
| Response | 读写 | 读写 | 读写 | 读写 | 读写 | 读写 | - |

**短路规则**：任意阶段设置 `ctx.Response` 后，管道立即跳到 `after_output`。

### 阶段示例

```go
// on_input: 拦截黑名单用户
s.RegisterStage(sdk.StageOnInput, func(ctx *sdk.StageContext) error {
    if ctx.UserID == "blocked_user" {
        resp := "已被限制使用"
        ctx.Response = &resp
    }
    return nil
})

// pre_action: 注入额外上下文
s.RegisterStage(sdk.StagePreAction, func(ctx *sdk.StageContext) error {
    ctx.Lock()
    ctx.ContextMsgs = append(ctx.ContextMsgs, map[string]interface{}{
        "role":    "system",
        "content": "当前时间: " + time.Now().Format("15:04"),
    })
    ctx.Unlock()
    return nil
})
```

### MemItem

```go
type MemItem struct {
    Role    string  `json:"role"`    // system / user / assistant
    Content string  `json:"content"` // 内容
    Score   float64 `json:"score"`   // TF-IDF 相关性评分
}
```

## 输入投递

插件可以向 Agent 投递输入消息。

```go
// 中断投递：可打断当前 LLM 处理
// - source: 来源标识（插件名）
// - channel: 通道名
// - text: 消息文本
func (s *PluginSDK) InjectInterruptText(source, channel, text string)

// 普通投递：排队等待处理
func (s *PluginSDK) InjectText(source, channel, text string)

// 投递但不触发记忆记录
func (s *PluginSDK) InjectTextNoMemory(source, channel, text string)
```

**两种投递方式的区别**：

| | InjectText | InjectInterruptText |
|---|---|---|
| 处理顺序 | 排队 | 优先 |
| 打断 LLM | 否 | 是（取消当前请求） |
| 适用场景 | 普通消息 | 定时器、重要通知 |

## 配置管理

### SettingsAPI

插件通过 `s.Settings()` 获取 `SettingsAPI`。每个插件拥有独立的 `config_<name>` SQLite 表。

```go
type SettingsAPI interface {
    // 自身配置（config_<name> 表）
    Get(key string) (interface{}, error)
    Set(key string, value interface{}) error
    List(prefix string) ([]string, error)

    // 核心配置（config 表）
    GetCore(key string) (interface{}, error)
    SetCore(key string, value interface{}) error
    ListCore(prefix string) ([]string, error)

    // 其他插件配置（config_<plugin> 表）
    GetPlugin(plugin, key string) (interface{}, error)
    SetPlugin(plugin, key string, value interface{}) error
    ListPlugin(plugin, prefix string) ([]string, error)

    // 配置定义（WebUI 显示用）
    RegisterDef(def ConfigDef)
    Defs(prefix string) []*ConfigDef

    // 全局
    Dump() map[string]interface{}
    Plugins() []string
}
```

### ConfigDef

```go
type ConfigDef struct {
    Key         string      `json:"key"`          // 配置键名
    Default     interface{} `json:"default,omitempty"`  // 默认值
    Type        string      `json:"type"`         // 类型：string / number / boolean
    DisplayName string      `json:"display_name"`  // WebUI 显示名称
    Description string      `json:"description,omitempty"` // 说明
    Category    string      `json:"category,omitempty"`   // 分组
    Options     []string    `json:"options,omitempty"`    // 选项列表（下拉框）
    Min         float64     `json:"min,omitempty"`
    Max         float64     `json:"max,omitempty"`
    Step        float64     `json:"step,omitempty"`
    Required    bool        `json:"required,omitempty"`
    Secret      bool        `json:"secret,omitempty"`    // 敏感信息（输入框掩码）
}
```

### 使用示例

```go
// 插件启动时注册配置定义
s.Settings().RegisterDef(sdk.ConfigDef{
    Key:         "provider_key",
    Type:        "string",
    DisplayName: "API Key",
    Description: "第三方服务 API 密钥",
    Secret:      true,
    Required:    true,
})

// 运行时读取配置
apiKey, err := s.Settings().Get("provider_key")

// 读取核心配置
dataDir, _ := s.Settings().GetCore("core.daemon.data_dir")

// 读取其他插件配置
qqNapcat, _ := s.Settings().GetPlugin("qq", "napcat_url")
```

## 记忆访问

### MemoryAPI（图记忆）

存储在 SQLite 图数据库中，entities + relations 表。

```go
type MemoryAPI interface {
    // 召回：query 为关键词列表，depth 为 BFS 遍历深度
    Recall(query []string, depth int) ([]Entity, []Relation, error)

    // 写入三元组
    Commit(triples []Triple) error

    // 统计：返回实体数、关系数等
    Introspect() (map[string]interface{}, error)

    // 合并实体（同义消歧）
    MergeEntities(source, target string) (int, error)

    // 清理：mode 为 "soft"（标记删除）或 "hard"（物理删除）
    Purge(criteria map[string]string, mode string) (int, error)
}
```

```go
type Entity struct {
    Name         string `json:"name"`          // 实体名称
    Type         string `json:"type"`          // 类型: Person / Location / Concept ...
    MentionCount int    `json:"mention_count"` // 提及次数
}

type Relation struct {
    SourceName   string `json:"source_name"`   // 主体
    TargetName   string `json:"target_name"`   // 客体
    RelationType string `json:"relation_type"` // 关系类型: likes / works_at / friend_of ...
}

type Triple struct {
    Subject  string `json:"subject"`  // 主体实体名
    Relation string `json:"relation"` // 关系
    Object   string `json:"object"`   // 客体实体名
}
```

### TextMemoryAPI（文本记忆）

按时间顺序的原始对话日志，JSONL 文件轮转存储。

```go
type TextMemoryAPI interface {
    Append(evt TextEvent) error
}

type TextEvent struct {
    Role      string `json:"role"`      // system / user / assistant
    Content   string `json:"content"`   // 内容
    Timestamp int64  `json:"timestamp"` // 时间戳
    Channel   string `json:"channel,omitempty"` // 来源通道
}
```

### DocMemoryAPI（文档记忆）

临时记忆层，JSON 文件 + TF-IDF 向量索引，消费即删。

```go
type DocMemoryAPI interface {
    // 搜索文档，返回 topK 条
    Query(text string, topK int) []*Doc

    // 插入文档
    Insert(doc *Doc) error

    // 删除文档
    Remove(id string)

    // 统计
    Stats() map[string]interface{}
}

type Doc struct {
    ID      string  `json:"id"`
    Title   string  `json:"title"`
    Content string  `json:"content"`
    Score   float64 `json:"score,omitempty"`
}
```

## 知识库

### KnowledgeAPI

文件系统 + TF-IDF 向量检索，独立于记忆系统的索引。

```go
type KnowledgeAPI interface {
    // 搜索知识条目，返回 topK 匹配
    Search(query string, topK int) ([]*Knowledge, error)

    // 添加知识
    Add(name, content string) error

    // 列出所有知识条目名
    List() ([]string, error)
}

type Knowledge struct {
    Name    string `json:"name"`
    Content string `json:"content"`
}
```

## LLM 管理

### LLMAPI

管理 LLM 提供者源。

```go
type LLMAPI interface {
    // 列出所有已注册的 LLM 源
    ListSources() []string

    // 切换默认 LLM 源
    SetSource(name string) error

    // 当前使用的 LLM 源
    CurrentSource() string
}
```

## IOInjector

SDK 内部的输入投递接口，`PluginSDK.InjectInterruptText` / `InjectText` / `InjectTextNoMemory` 底层调用。

```go
type IOInjector interface {
    InjectInterruptText(source, channel, text string)
    InjectText(source, channel, text string)
    InjectTextNoMemory(source, channel, text string)
}
```

内核在插件启动后调用 `sdk.SetIOInjector()` 注入此接口的实际实现。

## SDK 辅助类型

```go
// 工具注册回调类型
type ToolRegistrar func(name string, def ToolDef, handler ToolHandler) error

// 阶段注册回调类型
type StageRegistrar func(stage Stage, handler StageHandler)

// API 注册回调类型
type APIRegistrar func(name string) error
```

## 插件生命周期

```
内核启动
  │
  ├── plugin.Registry.Load(dir)
  │   ├── 扫描 plugins/ 目录
  │   ├── 匹配已注册工厂或动态加载 .so
  │   ├── 调用 NewPlugin(name, config)
  │   └── 调用 plugin.Start(sdk)    ← 插件注册工具/阶段/事件
  │
  ├── 正常运行
  │   ├── LLM 调用 → 路由到注册的工具
  │   └── 消息处理 → 触发注册的阶段钩子
  │
  └── 内核关闭
      └── plugin.Stop()              ← 插件清理资源
```

### 内置插件 vs 动态插件

| | 内置插件 | 动态 .so 插件 |
|---|---|---|
| 注册方式 | `init()` → `RegisterFactory` | `plugin.Open` 动态加载 |
| 存放位置 | `internal/plugins/` | `<dataDir>/plugins/<name>/` |
| 编译 | 编译进内核 | 独立 `go build -buildmode=plugin` |
| SDK 导入 | `gitcode.com/JianFeeeee/HomeAgent/internal/sdk` | `gitcode.com/JianFeeeee/homeagent-sdk/sdk` |
| 热加载 | 需重新编译 | 可运行时加载/卸载 |
