# 记忆系统重构计划 — NoMemory 与 TextCleaner 设计修正

基于 `review.md` 审查结论，当前实现存在两个设计偏差：

| 机制 | 当前（错误） | 应然（目标） |
|---|---|---|
| `RegisterTextCleaner` | 插件级全局 cleaner，`CleanText` 入口直接改原文 | **删除**，拆为 `ToolDef.Cleaner`（工具级，仅计算层生效） |
| `ToolDef.Cleaner` | 不存在 | 工具级，仅向量化/jieba/蒸馏时调用，不改原文 |
| `NoMemory=true` | `hasNoMemoryTool` 二值判断 → 整轮不写记忆 | 工具输出不参与向量/jieba/蒸馏，但原文保留在 Context/Document |

---

## 阶段零：理解当前数据流（现状确认）

```
用户输入 → processTextInput()
             → context.Append(Input)              // evt.Input = CleanText(evt.Input) ← 原文被改
             → a.process() → LLM call loop
                              → tool execution → result → msgs  ← 工具输出在此
             → context.Append(Response, ToolsUsed) // evt.Response 是 LLM 回复，不是工具输出
             → emitMemoryCandidate(input, response, toolsUsed)
                 → main.go goroutine
                     → textMem.Append(Event{Input, Response})   // JSONL 原文
                     → distiller.Append("assistant", response)  // 仅 LLM 回复
                         → extractKeyTriples → graph memory

蒸馏心跳:
  context.Prune() → ContextToDoc(entries) → Doc.Content ← hasNoMemoryTool 跳过整条
  reorgGraph() → docToTriples(Doc) → graph memory
```

关键发现：**工具输出从未直接进入 pipeline**（pipeline 只存 user/assistant 的原始文本）。`hasNoMemoryTool` 跳过了整个 LLM 回复，这是过度保守的。

---

## 第一阶段：SDK 定义修改（外部包 `homeagent-sdk`）

**文件：** `_sdk_local/sdk/plugin.go`

### 1.1 ToolDef 增加 Cleaner 字段

```go
type ToolDef struct {
    Name        string                 `json:"name"`
    Plugin      string                 `json:"plugin,omitempty"`
    Description string                 `json:"description"`
    Parameters  map[string]interface{} `json:"parameters"`
    NoMemory    bool                   `json:"no_memory,omitempty"`
    Cleaner     func(string) string    `json:"-"` // ← 新增：计算层过滤函数，不改原文
}
```

- `Cleaner` 是函数类型，不序列化（`json:"-"`）
- 仅在向量化/jieba/蒸馏等计算环节使用
- 默认 nil = 不过滤

### 1.2 删除 PluginSDK 中的 TextCleaner

```go
// PluginSDK 中删除：
//   textCleaners []func(text string) string  // ← 删除
//   RegisterTextCleaner()                    // ← 删除
//   TextCleaners()                           // ← 删除
```

### 1.3 相应修改单元测试

**文件：** `_sdk_local/sdk/plugin_test.go`

- 删除 `TestRegisterTextCleaner`、`TestTextCleanersEmpty`
- `TestToolDefNoMemory` 保留
- 新增 `TestToolDefCleaner` 验证 Cleaner 字段

### 1.4 go.mod 确认 replace 指令

`go.mod` 已有 `replace gitcode.com/JianFeeeee/homeagent-sdk => ./_sdk_local`，无需改动。

---

## 第二阶段：内部适配层更新

### 2.1 Registry：删除 textCleaners 聚合

**文件：** `internal/plugin/registry.go`

删除项：
- 字段 `textCleaners []func(string) string`
- 字段 `CleanText(text string) string` 方法
- `buildSDK` 不再收集 cleaner（cleaner 附着在 ToolDef 上，由 StageHost 管理）
- `loadOne` 和 `Load` 中的 `r.textCleaners = append(r.textCleaners, plgSDK.TextCleaners()...)` 移除
- `StopAll` 中的重置移除

改动点：
| 行号 | 当前 | 改为 |
|---|---|---|
| 87 | `textCleaners []func(string) string` | 删除 |
| 113-122 | `CleanText()` 方法 | 删除 |
| 273 | `r.textCleaners = append(...)` | 删除 |
| 356 | `r.textCleaners = append(...)` | 删除 |
| 373 | `r.textCleaners = nil` | 删除 |

### 2.2 memory/clean_text.go：删除全局 cleaner

**文件：** `internal/memory/clean_text.go`

```go
// 删除：
var globalTextCleaner func(string) string   // ← 删除
func SetTextCleaner(fn func(string) string) { // ← 删除
    globalTextCleaner = fn
}

func CleanText(text string) string {
    // if globalTextCleaner != nil {          // ← 删除
    //     text = globalTextCleaner(text)     // ← 删除
    // }                                       // ← 删除
    text = strings.TrimSpace(text)
    if text == "" {
        return ""
    }
    text = strings.TrimPrefix(text, "，")
    text = strings.TrimPrefix(text, "，")
    text = strings.TrimSpace(text)
    return text
}
```

`VectorizeClean` 保持不变（使用精简后的 `CleanText`）。

### 2.3 StageHost：暴露 ToolDef Cleaner 查询

**文件：** `internal/agent/core/stages.go`

`ToolDef(name)` 方法已有，返回 `*sdk.ToolDef`。由于 `ToolDef` 现在有 `Cleaner` 字段，调用方可直接通过 `stageHost.ToolDef(name).Cleaner` 获取。

新增便捷方法：

```go
func (h *StageHost) ToolDefCleaner(name string) func(string) string {
    h.mu.RLock()
    defer h.mu.RUnlock()
    for _, def := range h.toolDefs {
        if def.Name == name {
            return def.Cleaner
        }
    }
    return nil
}

// 新增：返回所有 NoMemory 工具名集合，供计算环节跳过
func (h *StageHost) NoMemoryToolNames() map[string]bool {
    h.mu.RLock()
    defer h.mu.RUnlock()
    set := make(map[string]bool, len(h.toolDefs))
    for _, def := range h.toolDefs {
        if def.NoMemory {
            set[def.Name] = true
        }
    }
    return set
}
```

### 2.4 StageHost：Import 更新

需 import `sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"`（已有）。

---

## 第三阶段：核心逻辑修正

### 3.1 context.Append：不再修改原文

**文件：** `internal/agent/core/context.go`

```go
func (c *RelevanceContext) Append(evt ContextEvent) {
    c.mu.Lock()
    defer c.mu.Unlock()

    // evt.Input = memory.CleanText(evt.Input)  // ← 删除：不修改原文
    evt.Vector = c.computeVector(&evt)          // 向量化仍使用 textForVector
    c.events = append(c.events, &evt)
    c.save()
}
```

### 3.2 textForVector：计算层使用 Cleaner

```go
func textForVector(evt *ContextEvent, toolDefLookup func(name string) *sdk.ToolDef) string {
    text := ""
    switch {
    case evt.Source == "agent" && evt.Response != "":
        text = evt.Response
    case evt.Source == "cold_storage":
        text = evt.Input + " " + evt.Response
    default:
        text = evt.Input
    }

    // 基础清洗（不修改原文）
    text = memory.CleanText(text)

    // 若 evt 关联了 NoMemory 工具，不再跳过整个事件
    // 但若工具注册了 Cleaner，在计算层过滤
    // textForVector 是对整个事件的向量化，不拆到工具级别
    return text
}
```

关键变更：
- `textForVector` 的入参增加 `toolDefLookup`（通过 `RelevanceContext.toolDefLookup` 已有）
- 移除 `textForVector` 对 `memory.CleanText` 的依赖（因为 `CleanText` 不再做插件级过滤）
- `computeVector` 传入 `toolDefLookup`

### 3.3 context.Prune：不再跳过 NoMemory 事件

```go
func (c *RelevanceContext) Prune(currentInput string, topK int, docStore *document.Store) int {
    // ... 前面的排序逻辑不变 ...

    archived := 0
    if docStore != nil && len(archive) > 0 {
        // 删除 hasNoMemoryTool 跳过整条的逻辑
        // for _, s := range archive {
        //     if hasNoMemoryTool(s.event.ToolsUsed, c.toolDefLookup) {
        //         continue
        //     }
        // }
        // 改为：所有事件都归档，工具级过滤在 ContextToDoc 内部处理
        entries := make([]document.ContextEntry, len(archive))
        for i, s := range archive {
            entries[i] = document.ContextEntry{
                Timestamp: s.event.Timestamp,
                Source:    s.event.Source,
                Content:   s.event.Input,
                Response:  s.event.Response,
            }
        }
        doc, err := docStore.ContextToDoc("context_archived", entries, c.embedder)
        if err == nil && doc != nil {
            archived = len(entries)
        }
    }
    // ...
}
```

删除 `hasNoMemoryTool` 辅助函数（`context.go:269-278`），迁移到 StageHost 的 `NoMemoryToolNames()`。

### 3.4 eventloop：移除 NoMemory 跳过记忆候选

**文件：** `internal/agent/core/eventloop.go`

```go
func (a *Agent) processTextInput(evt *agentIO.InputEvent, input string) {
    // ... 前面的逻辑不变 ...

    // 删除 NoMemory 跳过：
    // if !stageCtx.NoMemory && !a.hasNoMemoryTool(toolsUsed) {
    //     a.emitMemoryCandidate(evt.Source, input, response, toolsUsed)
    // }
    // 改为：始终 emit，工具过滤在消费端处理
    // 但保留 stageCtx.NoMemory（IO 注入的 no_memory flag）
    if !stageCtx.NoMemory {
        a.emitMemoryCandidate(evt.Source, input, response, toolsUsed)
    }
}
```

同理修改 `processMediaInput` 中的对应检查。

删除 `hasNoMemoryTool` 方法（`eventloop.go:389-396`）。

### 3.5 main.go：删除 SetTextCleaner

**文件：** `cmd/homed/main.go`

```go
// 删除：
// memory.SetTextCleaner(pluginReg.CleanText)
```

### 3.6 context.go：textForVector 签名更新

`computeVector` 需传入 `toolDefLookup`：

```go
func (c *RelevanceContext) computeVector(evt *ContextEvent) vector.Vector {
    return c.embedder.Vectorize(textForVector(evt, c.toolDefLookup))
}
```

`c.toolDefLookup` 已有（通过 `SetToolDefLookup` 注入）。

---

## 第四阶段：文档记忆与蒸馏修正

### 4.1 document.ContextToDoc：接收 cleaned entries

**文件：** `internal/memory/document/document.go`

```go
// ContextToDoc 入参增加 cleanFn，在 summarizeEntries/extractTags/extractEntities 前过滤
func (s *Store) ContextToDoc(source string, entries []ContextEntry, vec vector.Vectorizer, cleanFn func(string) string) (*Doc, error) {
    if len(entries) == 0 {
        return nil, nil
    }

    var parts []string
    for _, e := range entries {
        // Content 用 cleanFn 过滤后拼接，原文保留
        cleaned := e.Content
        if cleanFn != nil {
            cleaned = cleanFn(e.Content)
        }
        line := fmt.Sprintf("[%s] %s: %s", e.Timestamp.Format("15:04"), e.Source, cleaned)
        if e.Response != "" {
            line += fmt.Sprintf(" → %s", truncate(e.Response, 100))
        }
        parts = append(parts, line)
    }
    // ... 后续不变
}
```

### 4.2 context.Prune：传入 cleaner

```go
// 在传 entries 给 ContextToDoc 前，先构建工具→cleaner 映射
toolCleaners := make(map[string]func(string)string)
for _, s := range archive {
    for _, name := range s.event.ToolsUsed {
        if c.toolDefLookup != nil {
            if def := c.toolDefLookup(name); def != nil && def.Cleaner != nil {
                toolCleaners[name] = def.Cleaner
            }
        }
    }
}
// 构建清理函数：对所有工具输出依次应用对应 Cleaner
entryCleanFn := func(text string) string {
    // 此处 text 是 Content（用户输入），不含工具输出，所以不需要 Cleaner
    // Cleaner 在 distill.go 的 docToTriples 中使用
    return text
}
```

实际上，`ContextToDoc` 中 `Content` 是用户输入，不包含工具输出。工具输出过滤主要发生在 `docToTriples`。

### 4.3 docToTriples：应用 Cleaner

**文件：** `internal/agent/core/distill.go`

```go
func docToTriples(doc *document.Doc, toolCleaners map[string]func(string) string) []memory.Triple {
    var triples []memory.Triple
    if doc == nil {
        return triples
    }

    triples = append(triples, memory.Triple{...})

    lines := strings.Split(doc.Content, "\n")
    for _, line := range lines {
        line = strings.TrimSpace(line)
        if line == "" {
            continue
        }
        // 应用 Cleaner（如果匹配工具输出行）
        // 实际 doc.Content 是 `[时间] source: content → response` 格式，
        // 其中 content 是用户输入，不直接包含工具输出
        // Cleaner 在此暂不应用，保留后续扩展
        terms := memory.CutExact(line)
        // ...
    }
    // ...
}
```

注意：`Doc.Content` 中的内容是用户在 Prune 时输入的文本和 LLM 回复，不包含原始工具输出。工具输出在 `msgs` 中（LLM 对话历史），但不在 `Doc.Content` 中。因此 `docToTriples` 不需要直接应用 Cleaner。

---

## 第五阶段：NoMemory 语义修正

### 5.1 NoMemory 的新语义

| 场景 | 旧行为 | 新行为 |
|---|---|---|
| `emitMemoryCandidate` | `hasNoMemoryTool` → 跳过 | 始终写入 text memory + pipeline |
| `context.Prune` → `ContextToDoc` | `hasNoMemoryTool` → 跳过 | 全部归档，统一进入文档记忆 |
| `textForVector` | `CleanText` 改原文后向量化 | 基础 Trim + 向量化（原文不变） |
| `docToTriples` | 无 Cleaner 直接 jieba | 无 Cleaner 直接 jieba（同理） |
| `StageContext.NoMemory` | `InjectTextNoMemory` → 整轮跳过 | 保留（IO 层控制的整轮跳过） |

NoMemory 的声明式语义变为：
- `NoMemory=true` 是一个**工具元数据标记**，当前不在内核层面做特殊跳过
- 为后续精确过滤（如管道层跳过 NoMemory 工具的输出）预留标记
- 未来管道增强时可读取此标记，跳过对应工具输出片段

### 5.2 内置插件标记更新

**文件：** `internal/plugins/cmd/plugin.go`

```go
s.RegisterTool("cmd_run", sdk.ToolDef{
    Name:        "cmd_run",
    Description: "执行 Shell 命令...",
    NoMemory:    true,    // 保留：标记工具输出对 LLM 注意力无信号价值
    // Cleaner:  nil,     // 不注册 Cleaner：命令输出噪音不可控
}, p.handleCmdRun)
```

**文件：** `internal/plugins/agentcli/plugin.go`

```go
s.RegisterTool("terminal_create", sdk.ToolDef{
    Name:     "terminal_create",
    NoMemory: true,        // 保留：终端交互噪音
}, p.handleCreate)
// 同理其它 5 个工具
```

**文件：** `internal/plugins/files/plugin.go`（示例插件 `_sdk_local/example/files/plugin.go`）

```go
s.RegisterTool(tp+"read", sdk.ToolDef{
    Name:     tp + "read",
    NoMemory: false,        // 明确 false：文件内容对 LLM 注意力有信号价值
    Cleaner: func(output string) string {
        var r struct{ Content string }
        if err := json.Unmarshal([]byte(output), &r); err != nil {
            return output
        }
        return r.Content  // 去 JSON 包裹，供未来向量化使用
    },
}, p.handleRead)
```

---

## 第六阶段：代码清理

### 6.1 删除无用代码

| 文件 | 删除内容 |
|---|---|
| `internal/plugin/registry.go` | `textCleaners` 字段、`CleanText()` 方法、相关的 append 逻辑 |
| `internal/memory/clean_text.go` | `globalTextCleaner`、`SetTextCleaner()` |
| `internal/agent/core/context.go` | `hasNoMemoryTool()` 辅助函数 |
| `internal/agent/core/eventloop.go` | `hasNoMemoryTool()` 方法 |
| `cmd/homed/main.go` | `memory.SetTextCleaner(pluginReg.CleanText)` |

### 6.2 _sdk_local 清理

| 文件 | 操作 |
|---|---|
| `_sdk_local/sdk/plugin.go` | `ToolDef` 增 `Cleaner`；删除 `RegisterTextCleaner`/`TextCleaners`/`textCleaners` |
| `_sdk_local/sdk/plugin_test.go` | 替换 TextCleaner 测试为 Cleaner 测试 |

---

## 涉及文件清单

| 文件 | 操作 | 阶段 |
|---|---|---|
| `_sdk_local/sdk/plugin.go` | `ToolDef` 增 `Cleaner`；删 `RegisterTextCleaner`/`TextCleaners` | 一 |
| `_sdk_local/sdk/plugin_test.go` | 更新测试 | 一 |
| `internal/plugin/registry.go` | 删 textCleaners + CleanText | 二 |
| `internal/memory/clean_text.go` | 删 globalTextCleaner + SetTextCleaner | 二 |
| `internal/agent/core/stages.go` | 增 `ToolDefCleaner` + `NoMemoryToolNames` | 二 |
| `internal/agent/core/context.go` | `Append` 不改原文；`Prune` 不跳 NoMemory；`textForVector` 入参 toolDefLookup | 三 |
| `internal/agent/core/eventloop.go` | 删 `hasNoMemoryTool` 调用 + 方法 | 三 |
| `internal/agent/core/distill.go` | `emitMemoryCandidate` 不加过滤（已在 eventloop 处理） | 三 |
| `internal/memory/document/document.go` | `ContextToDoc` 可选 cleanFn 参数 | 四 |
| `internal/plugins/cmd/plugin.go` | 确认 NoMemory=true | 五 |
| `internal/plugins/agentcli/plugin.go` | 确认 NoMemory=true | 五 |
| `_sdk_local/example/files/plugin.go` | 示例 Cleaner 注册 | 五 |
| `cmd/homed/main.go` | 删 `memory.SetTextCleaner(pluginReg.CleanText)` | 六 |

---

## 实施顺序

```
阶段一 (SDK 定义)     →  阶段二 (内部适配删除)  →  阶段三 (核心逻辑)
                                                      ↓
阶段六 (代码清理)  ←  阶段五 (NoMemory 语义)  ←  阶段四 (文档记忆)
```

### Step-by-step 实施步骤

1. **`_sdk_local/sdk/plugin.go`** — `ToolDef` 加 `Cleaner` 字段；删除 `RegisterTextCleaner`/`TextCleaners`/`textCleaners` 字段及方法
2. **`_sdk_local/sdk/plugin_test.go`** — 更新测试用例
3. **`internal/plugin/registry.go`** — 删除 `textCleaners`、`CleanText()`、收集逻辑
4. **`internal/memory/clean_text.go`** — 删除 `globalTextCleaner`、`SetTextCleaner`
5. **`internal/agent/core/stages.go`** — 增 `ToolDefCleaner`、`NoMemoryToolNames`
6. **`internal/agent/core/context.go`** —
   - `Append`: 删除 `evt.Input = memory.CleanText(evt.Input)`
   - `textForVector`: 入参加 `toolDefLookup`
   - `computeVector`: 传入 `c.toolDefLookup`
   - `Prune`: 删除 `hasNoMemoryTool` 跳过逻辑
   - 删除 `hasNoMemoryTool` 辅助函数
   - `load()`: 不再对已加载事件调用 `CleanText`
7. **`internal/agent/core/eventloop.go`** —
   - `processTextInput`: `emitMemoryCandidate` 前删 `!a.hasNoMemoryTool(toolsUsed)` 条件
   - `processMediaInput`: 同上
   - 删除 `hasNoMemoryTool` 方法
8. **`internal/memory/document/document.go`** — `ContextToDoc` 入参加 `cleanFn`
9. **`cmd/homed/main.go`** — 删除 `memory.SetTextCleaner(pluginReg.CleanText)`
10. **内置插件** — 确认 NoMemory 标记，示例插件加 Cleaner
11. **编译测试** — `go build ./...` 确认无编译错误

---

## 验证方法

### 编译检查
```bash
go build ./...
go vet ./...
```

### 单元测试
```bash
# SDK 测试
cd _sdk_local && go test ./sdk/...

# 内核测试
cd /home/program/TrueAgent && go test ./internal/agent/core/...
go test ./internal/memory/...
go test ./internal/plugin/...
```

### 行为验证

**场景 1：NoMemory 工具调用后记忆仍然产生**
1. 用户输入："查一下 /etc/passwd"
2. Agent 调用 `cmd_run`（NoMemory=true）
3. LLM 回复："第一行是 root，uid=0，超级管理员哦～"
4. ✅ `textMem.Append(Event{Response: "第一行是 root..."})` — 写入
5. ✅ `distiller.Append("assistant", "第一行是 root...")` — 写入
6. ✅ `context.Append(ContextEvent{Response: "第一行是 root..."})` — 可见

**场景 2：Cleaner 仅影响计算层**
1. Agent 调用 `files_read` 返回 `{"content": "敏感数据"}`
2. ✅ 原文 `msgs` 中保留 `{"content": "敏感数据"}`
3. ✅ LLM 回复中可见原始内容
4. `textForVector` 使用 Cleaner 提取 `content` 字段（注：实际 textForVector 对 Response 操作，Response 已是 LLM 的自然语言，不是 JSON 包裹。Cleaner 在工具输出进入 msgs 时注释即可。）

实际上，Cleaner 的调用时机需要斟酌。工具输出通过 `executeToolCallInner` 返回字符串，进入 `msgs` 中的 `role: "tool"` 消息。`msgs` 用于 LLM 上下文，不需要 Cleaner。Cleaner 是在"工具输出单独进入记忆计算"时才需要。

当前架构中，工具输出不单独进入记忆计算，而是通过 LLM 的回复间接影响记忆。所以 Cleaner 的实际用途是**预留扩展**：当未来有直接对工具输出进行向量化/摘要的环节时，使用 Cleaner 过滤。

### 回归测试
- 确认旧有 `InjectTextNoMemory`（IO 层整轮跳过，通过 `stageCtx.NoMemory` 控制）不受影响
- 确认 context prune 不再因 NoMemory 工具跳过低相关性事件归档
- 确认 text memory 日志完整记录所有对话轮次