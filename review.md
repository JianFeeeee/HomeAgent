# 记忆系统审查：NoMemory 与 TextCleaner 设计偏差

## 一、核心原则

**Context 和 Document 层始终保留原始文本。** Cleaner 和 NoMemory 不修改原文，只控制文本在**计算层**（向量化、jieba 分词、蒸馏）中的参与方式。原文完整性是 LLM 注意力分配的基础——清洗掉工具特征输出会干扰 LLM 对上下文的理解。

---

## 二、TextCleaner：应为工具级计算过滤，实为插件级原文清洗

### 当前实现

**SDK 定义** — `_sdk_local/sdk/plugin.go:349`
```go
type PluginSDK struct {
    textCleaners []func(text string) string  // 插件级列表
}

func (s *PluginSDK) RegisterTextCleaner(cleaner func(text string) string) {
    s.textCleaners = append(s.textCleaners, cleaner)
}

func (s *PluginSDK) TextCleaners() []func(text string) string {
    return s.textCleaners
}
```

**内核收集** — `internal/plugin/registry.go:273`
```go
r.textCleaners = append(r.textCleaners, plgSDK.TextCleaners()...)
```

**全局注入** — `cmd/homed/main.go:426`
```go
memory.SetTextCleaner(pluginReg.CleanText)
```

**在 `memory.CleanText` 中执行** — `internal/memory/clean_text.go:16`
```go
func CleanText(text string) string {
    if globalTextCleaner != nil {
        text = globalTextCleaner(text)  // ← 修改了原始文本
    }
    text = strings.TrimSpace(text)
    ...
}
```

### 问题

1. **粒度错** — 插件级无法区分工具。一个插件注册 6 个工具，输出格式各异，却只能共享一个 cleaner。

2. **时机错** — `CleanText` 在 `context.Append` 入口处直接修改文本，破坏了"原文保留"原则。

```
context.Append(evt)
  → evt.Input = memory.CleanText(evt.Input)   // ← 原文被改
  → evt.Vector = c.computeVector(&evt)         // ← 向量基于已改文本
```

3. **覆盖不全** — `Response` 不经过 `CleanText`。`context.Append` 中对 `Input` 调用了 `CleanText`，但对 `Response` 没有。

### 应然设计

`Cleaner` 应是 `ToolDef` 上的字段，且**只用于计算层，不用于存储层**：

```go
type ToolDef struct {
    Name        string
    Plugin      string
    Description string
    Parameters  map[string]interface{}
    NoMemory    bool                    // 此工具输出不参与任何记忆计算
    Cleaner     func(string) string    // 输出参与记忆计算前，先用此函数过滤噪音
}
```

使用方式：

```go
s.RegisterTool("files_read", sdk.ToolDef{
    Cleaner: func(output string) string {
        // 只影响计算层，原文不变
        var r struct{ Content string }
        json.Unmarshal([]byte(output), &r)
        return r.Content  // 去 JSON 包裹，供向量化/jieba 使用
    },
}, handler)
```

Cleaner 在内核中的注入点**不是**修改 `ContextEvent.Response` 或 `Doc.Content`，而是作为「计算时过滤函数」注册到 `StageHost`，在各计算环节按需调用：

```
存储层（原文不变）:           计算层（Cleaner 过滤后）:
  ContextEvent.Response  ──→  textForVector → Vectorize(Cleaner(text))
  Doc.Content            ──→  summarizeEntries → jieba(Cleaner(text))
                              extractTags → jieba(Cleaner(text))
                              extractEntities → jieba(Cleaner(text))
  Doc lines              ──→  docToTriples → CutExact(Cleaner(line))
```

---

## 三、NoMemory：应为输出跳过计算但原文保留，实为整轮跳过

### 当前实现

`_sdk_local/sdk/plugin.go:95`
```go
type ToolDef struct {
    NoMemory bool `json:"no_memory,omitempty"`
}
```

内核中 `hasNoMemoryTool`（`internal/agent/core/eventloop.go:389`）：
```go
func (a *Agent) hasNoMemoryTool(toolsUsed []string) bool {
    for _, name := range toolsUsed {
        if def := a.stageHost.ToolDef(name); def != nil && def.NoMemory {
            return true  // ← 工具 NoMemory → 整轮跳过
        }
    }
    return false
}
```

检查点：
- `eventloop.go:337` — `emitMemoryCandidate` 整轮跳过
- `context.go:204` — Prune 归档整条事件跳过
- `context.go:269` — `hasNoMemoryTool` 是二值判断

### 问题

```
用户: 帮我查一下 /etc/passwd
助手: 好的～ (工具: cmd_run → root:x:0:0:...)
助手: 第一行是 root，uid=0，超级管理员哦 (｀・ω・´)
                               ↑ 这句自然语言有记忆价值
```

`hasNoMemoryTool` 检查 `toolsUsed` 包含 `cmd_run`（NoMemory=true）→ **整轮不写记忆**，"root 是超级管理员"这条信息丢失。

**NoMemory 应该过滤的是工具输出文本在计算层的参与，不是跳过整轮对话。**

### 应然设计

NoMemory 在三层计算中的语义：

| 计算环节 | 行为 |
|---|---|
| **Context 向量化** | `textForVector` 对 `Response` 做向量化时，跳过其中属于 NoMemory 工具输出转述/引用的部分 |
| **Document 摘要/标签/实体** | jieba 分词时，跳过 NoMemory 工具的输出文本 |
| **Document→Graph 蒸馏** | `docToTriples` 跳过 NoMemory 工具输出对应行 |
| **Distiller Pipeline** | `extractKeyTriples` 跳过含 NoMemory 工具输出的片段 |

保留原文不变：

| 存储层 | 行为 |
|---|---|
| `ContextEvent.Response` | 保留 LLM 回复全文 |
| `Doc.Content` | 保留文档全文 |
| `toolCallRing.FullResult` | 保留完整工具输出 |
| `msgs` (LLM 对话历史) | 保留原始消息 |

---

## 四、设计对照表

| 机制 | 当前实现 | 应然设计 |
|---|---|---|
| `RegisterTextCleaner` | 插件级，`memory.CleanText` 入口直接改原文 | **删除**，拆为 `ToolDef.Cleaner` |
| `ToolDef.Cleaner` | 不存在 | 工具级，仅计算层生效，不改原文 |
| `NoMemory=true` | `hasNoMemoryTool` 二值 → 整轮跳过 | 工具输出不参与向量/jieba/蒸馏，原文保留 |

### 决策矩阵

```
工具输出 → 对 LLM 注意力有信号价值?
  ├── 否 → NoMemory=true
  │     输出原文保留在 Context/Document，
  │     但不参与向量计算、jieba 分词、图蒸馏
  │     例: cmd_run（任意命令、转义符、路径）
  │
  └── 是 → 有可控噪音?
       ├── 是 → Cleaner 注册计算层过滤
       │     过滤后的文本参与向量/jieba/蒸馏，
       │     原文不修改
       │     例: files_read（需去 JSON 包裹、截断）
       │         web_fetch（需提取正文、去 HTML）
       │
       └── 否 → 正常记忆，无额外处理
             例: memory_recall（结构化，无噪音）
                 person_set_trait（短文本，无噪音）
```

---

## 五、影响范围

| 层次 | 文件 | 改动 |
|---|---|---|
| **SDK** | `_sdk_local/sdk/plugin.go` | `ToolDef` 新增 `Cleaner func(string) string`；移除 `RegisterTextCleaner` / `TextCleaners` / `textCleaners` |
| **SDK** | `_sdk_local/sdk/plugin_test.go` | 移除 TextCleaner 测试，新增 NoMemory/Cleaner 组合测试 |
| **Registry** | `internal/plugin/registry.go` | 移除 `textCleaners` 收集逻辑；移除 `CleanText` 方法；构建 `StageHost` 时传入工具的 Cleaner 映射 |
| **全局 CleanText** | `internal/memory/clean_text.go` | 移除 `globalTextCleaner` / `SetTextCleaner`；`CleanText` 只保留 TrimSpace 等基础清洗 |
| **main** | `cmd/homed/main.go` | 移除 `memory.SetTextCleaner(pluginReg.CleanText)` |
| **内核入口** | `internal/agent/core/process.go:228` | 工具返回结果后，结果原文进 `msgs`，同时 `Cleaner(text)` 结果进后续记忆管道 |
| **工具调度** | `internal/agent/core/toolcall.go` | `executeToolCallInner` 返回值额外返回 cleaned 版本（或通过 `StageHost.ToolDef(name).Cleaner` 延迟计算） |
| **Context 向量** | `internal/agent/core/context.go:73-86` | `textForVector` 从 `stageHost` 获取 Cleaner，对文本做计算层过滤后再 `Vectorize` |
| **Context Prune** | `internal/agent/core/context.go:144-215` | 不再 `hasNoMemoryTool` 跳过整条；改为只传 Cleaner 过滤后的文本给 `docStore.ContextToDoc` |
| **Context Append** | `internal/agent/core/context.go:102-111` | `computeVector` 之前对 `Input`/`Response` 走 Cleaner 过滤，原文不修改 |
| **记忆候选** | `internal/agent/core/eventloop.go:337` | 不跳过 `emitMemoryCandidate`；`emitMemoryCandidate` 同时传出原始和 cleaned 版本 |
| **Document** | `internal/memory/document/document.go:132-200` | `ContextToDoc` 接收 cleaned 文本用于 `summarizeEntries`/`extractTags`/`extractEntities`/向量计算，`Doc.Content` 原文不变 |
| **Document→Graph** | `internal/agent/core/distill.go:332-378` | `docToTriples` 对每行走 Cleaner 后再 `CutExact`；跳过 NoMemory 工具输出行 |
| **Pipeline** | `internal/memory/pipeline/pipeline.go:235` | `distillBatch` 跳过 NoMemory 工具输出片段 |
| **StageHost** | `internal/agent/core/stages.go` | 新增 `ToolDefCleaner(name string) func(string) string` 查询 |
| **内置插件** | `internal/plugins/cmd/plugin.go` | `cmd_run`: `NoMemory=true`，不注册 Cleaner（噪音不可控） |
| **内置插件** | `internal/plugins/agentcli/plugin.go` | 6 个工具各注册专用 Cleaner + `NoMemory=true`（输出仍含不可控噪音，但 Cleaner 提取有价值信号） |
| **内置插件** | `internal/plugins/files/plugin.go` | `files_read/edit` 注册 Cleaner 截断长文本、去 JSON 包裹，正常记忆（NoMemory=false 或移除） |
