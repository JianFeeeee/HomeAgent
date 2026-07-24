# 插件化改造 — 实施计划

## 问题

当前 core 层两处硬编码耦合：

1. **文本清洗**：`internal/memory/clean_text.go` 硬编码 6 个 QQ 正则，context/indexer/cut/VertorizeClean 各环节直接调用 `CleanTemplateText`

2. **工具输出不进记忆**：`internal/agent/core/context.go:196` 硬编码 source 名过滤
   ```go
   if s.event.Source == "agentcli" || s.event.Source == "terminal" {
       continue
   }
   ```
   意图是 `cmd_run`、`terminal_*` 等工具的产出不归档到 document memory，但手段是查 source 而不是查工具定义，且只覆盖了 Prune 路径（document），没有覆盖 `emitMemoryCandidate` 路径（text + graph memory）。

   同时 `eventloop.go:337` 的 `!stageCtx.NoMemory` 检查是命令式的（`InjectTextNoMemory` 设 payload flag），不是声明式的工具级控制。

## 目标

| 问题 | 方案 |
|------|------|
| 文本清洗 | 插件通过 `s.RegisterTextCleaner()` 注册清洗函数，core 遍历执行 |
| 工具记忆控制 | `ToolDef` 增加 `NoMemory bool` 字段，插件声明式标记工具；core 在 emitMemoryCandidate 和 Prune 两处自动跳过 |

## SDK 修改（外部包 `homeagent-sdk`）

**位置：** `sdk/plugin.go`

### 1.1 ToolDef 增加 NoMemory

```go
type ToolDef struct {
    Name        string                 `json:"name"`
    Plugin      string                 `json:"plugin,omitempty"`
    Description string                 `json:"description"`
    Parameters  map[string]interface{} `json:"parameters"`
    NoMemory    bool                   `json:"no_memory,omitempty"` // ← 新增
}
```

### 1.2 PluginSDK 增加 TextCleaner

```go
// PluginSDK 新增字段
textCleaners []func(text string) string

func (s *PluginSDK) RegisterTextCleaner(cleaner func(text string) string) {
    s.textCleaners = append(s.textCleaners, cleaner)
}

// 框架用 — 提取所有 cleaner
func (s *PluginSDK) TextCleaners() []func(text string) string {
    return s.textCleaners
}
```

### SDK 管理策略

复制 SDK 到 `_sdk_local/`，`go.mod` 加 `replace gitcode.com/JianFeeeee/homeagent-sdk => ./_sdk_local`

---

## 内部变更

### 2.1 Registry：聚合 text cleaners

**文件：** `internal/plugin/registry.go`

- 新增字段 `textCleaners []func(string) string`
- `buildSDK()` 中 `plg.Start(plgSDK)` 成功后提取 `plgSDK.TextCleaners()` 到 `textCleaners`
- 新增 `CleanText(text string) string` 依次执行所有 cleaner，无 cleaner 时返回原文本
- `StopAll()` / `Reload()` 时重置

### 2.2 Memory：动态 cleaner 入口

**文件：** `internal/memory/clean_text.go`

替换硬编码 QQ 正则为动态 cleaner：

```go
var globalTextCleaner func(string) string

func SetTextCleaner(fn func(string) string)  { globalTextCleaner = fn }
func CleanText(text string) string {
    if globalTextCleaner != nil {
        text = globalTextCleaner(text)
    }
    // 保留通用 trim
    text = strings.TrimSpace(text)
    text = strings.TrimPrefix(text, "，")
    text = strings.TrimPrefix(text, "，")
    text = strings.TrimSpace(text)
    return text
}
```

删除 `reQQ*` 正则变量。

### 2.3 StageHost：按名查 ToolDef

**文件：** `internal/agent/core/stages.go`

新增方法：

```go
func (h *StageHost) ToolDef(name string) *sdk.ToolDef {
    h.mu.RLock()
    defer h.mu.RUnlock()
    for _, def := range h.toolDefs {
        if def.Name == name {
            return &def
        }
    }
    return nil
}
```

### 2.4 Agent：两处 NoMemory 检查

#### 2.4a emitMemoryCandidate 路径

**文件：** `internal/agent/core/eventloop.go`

`processTextInput()` 和 `processMediaInput()` 中的 `emitMemoryCandidate` 调用前，除了检查 `stageCtx.NoMemory`，还检查本次调用的工具是否有 `NoMemory`：

```go
hasNoMemoryTool := false
for _, name := range toolsUsed {
    if def := a.stageHost.ToolDef(name); def != nil && def.NoMemory {
        hasNoMemoryTool = true
        break
    }
}
if !stageCtx.NoMemory && !hasNoMemoryTool {
    a.emitMemoryCandidate(...)
}
```

这样不论通过 `InjectText` 还是 `InjectTextNoMemory`，只要 LLM 调用了 `NoMemory: true` 的工具，整轮对话就不进 text memory 和 graph memory。

#### 2.4b Prune 归档路径

**文件：** `internal/agent/core/context.go`

删除 hardcoded source 过滤，改为检查事件中 `ToolsUsed` 是否有 `NoMemory` 工具：

```go
for _, s := range archive {
    if hasNoMemoryTool(s.event.ToolsUsed) {
        continue
    }
    filtered = append(filtered, s)
}
```

`hasNoMemoryTool` 通过 `StageHost.ToolDef` 查每个工具名是否有 `NoMemory: true`。

### 2.5 caller CleanTemplateText → CleanText

| 文件 | 替换 |
|------|------|
| `internal/agent/core/context.go` | `memory.CleanTemplateText` → `memory.CleanText` |
| `internal/memory/indexer.go` | 同上 |
| `internal/memory/cut.go` | 同上 |
| `internal/memory/clean_text.go` | 删除函数 `CleanTemplateText`，新增 `CleanText` + `SetTextCleaner` |

### 2.6 main.go 串联

**文件：** `cmd/homed/main.go`

`pluginReg.Load()` 之后：

```go
memory.SetTextCleaner(pluginReg.CleanText)
```

### 2.7 agentcli / cmd 插件：标记 NoMemory

**文件：** `internal/plugins/agentcli/plugin.go` 和 `internal/plugins/cmd/plugin.go`

在 `RegisterTool` 调用的 `ToolDef` 中加 `NoMemory: true`：

- `terminal_create`, `terminal_write`, `terminal_read`, `terminal_resize`, `terminal_close`, `terminal_list`
- `cmd_run`

---

## 外部插件变更

### QQ 插件

在 `Start()` 中：

```go
s.RegisterTextCleaner(func(text string) string {
    text = reQQGroupSuffix.ReplaceAllString(text, "")
    text = reQQPrivateSuffix.ReplaceAllString(text, "")
    text = reQQOldReply.ReplaceAllString(text, "")
    text = reQQOldForbid.ReplaceAllString(text, "")
    text = reQQGeneral.ReplaceAllString(text, "")
    text = reTimestamp.ReplaceAllString(text, "")
    text = reMultiSpace.ReplaceAllString(text, " ")
    return text
})

s.RegisterTool("qq_get_message", sdk.ToolDef{
    Description: "获取QQ消息正文",
    Parameters:  map[string]interface{}{...},
    NoMemory:    true,
}, handler)
```

---

## 文件变更清单

| 文件 | 操作 |
|------|------|
| `sdk/plugin.go`（外部 SDK） | 修改：`ToolDef` 增 `NoMemory`、`PluginSDK` 增 `RegisterTextCleaner`/`TextCleaners` |
| `go.mod` | 修改：增 `replace` 指令 |
| `internal/plugin/registry.go` | 修改：增 cleaners 聚合 + `CleanText` |
| `internal/memory/clean_text.go` | 修改：硬编码 → 动态 cleaner，删除 `CleanTemplateText` |
| `internal/agent/core/stages.go` | 修改：增 `ToolDef(name)` 查找 |
| `internal/agent/core/eventloop.go` | 修改：`emitMemoryCandidate` 前查 `ToolDef.NoMemory` |
| `internal/agent/core/context.go` | 修改：source 过滤 → `ToolsUsed` NoMemory 检查 + `CleanTemplateText` → `CleanText` |
| `internal/agent/core/process.go` | 修改（可能需要）：传递 toolsUsed 到 Prune 或加辅助方法 |
| `internal/memory/indexer.go` | 修改：`CleanTemplateText` → `CleanText` |
| `internal/memory/cut.go` | 修改：同上 |
| `internal/plugins/agentcli/plugin.go` | 修改：工具标记 `NoMemory: true` |
| `internal/plugins/cmd/plugin.go` | 修改：`cmd_run` 标记 `NoMemory: true` |
| `cmd/homed/main.go` | 修改：`memory.SetTextCleaner(pluginReg.CleanText)` |

---

## 实施顺序

1. SDK 本地副本 + 加 `NoMemory`/`RegisterTextCleaner`
2. Registry 聚合 cleaners + `CleanText`
3. Memory 动态 cleaner + 改名调用者
4. StageHost `ToolDef(name)` 
5. eventloop + context Prune + process 加入 NoMemory 检查
6. 标记 agentcli/cmd 工具的 `NoMemory: true`
7. main.go 串联
8. 删除旧代码（QQ 正则、source 硬编码）
9. 构建 + 测试
