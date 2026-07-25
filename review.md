# 记忆系统审查：NoMemory 与 TextCleaner 设计偏差

> **审查日期：** 2026-07-25
> **审查范围：** 核心仓 `homeagent/`（`internal/agent/core/`、`internal/memory/`、`internal/plugins/`、`internal/plugin/`、`cmd/homed/`）及 SDK 仓 `homeagentsdk/`（`sdk/`、`example/`、`tools/`）
> **当前状态：** 核心层全部修复完成 ✅，SDK 示例插件及模板 **全部更新完成** ✅

---

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

| 机制 | 旧实现（已废弃） | 当前实现（已修复） |
|---|---|---|
| `RegisterTextCleaner` | 插件级，`memory.CleanText` 入口直接改原文 | **已删除**，拆为 `ToolDef.Cleaner` ✅ |
| `ToolDef.Cleaner` | 不存在 | 工具级字段，`textForVector`/`summarizeEntries`/`extractTags`/`extractEntities` 中调用 ✅ |
| `NoMemory=true` | `hasNoMemoryTool` 二值 → 整轮跳过 | `textForVector` 跳过对应 `ToolResults` 条目，原文保留 ✅ |

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

## 五、影响范围（已实施）

### 5.1 核心仓（已全部修复 ✅）

| 层次 | 文件 | 改动 | 状态 |
|---|---|---|---|
| **SDK 适配层** | `internal/sdk/plugin.go` | 类型别名 `ToolDef = pubsdk.ToolDef` 透传 `NoMemory`/`Cleaner` | ✅ |
| **Registry** | `internal/plugin/registry.go` | 移除 `textCleaners` 收集；移除 `CleanText` 方法；`buildSDK` 不再收集 cleaner | ✅ |
| **全局 CleanText** | `internal/memory/clean_text.go` | 移除 `globalTextCleaner`/`SetTextCleaner`；仅保留 TrimSpace 等基础清洗 | ✅ |
| **main** | `cmd/homed/main.go` | 移除 `memory.SetTextCleaner(pluginReg.CleanText)` | ✅ |
| **StageHost** | `internal/agent/core/stages.go` | 新增 `ToolDefCleaner()`/`NoMemoryToolNames()` | ✅ |
| **ContextEvent** | `internal/agent/core/context.go:24-32` | 新增 `ToolResults []ToolResultItem` 字段 | ✅ |
| **process 返回值** | `internal/agent/core/process.go:18` | 新增 `toolResults []ToolResultItem` 返回值；line 229 收集 | ✅ |
| **textForVector** | `internal/agent/core/context.go:78-111` | 遍历 `evt.ToolResults`：NoMemory 跳过，其余经 Cleaner 过滤后拼入 | ✅ |
| **Append** | `internal/agent/core/context.go:132-140` | 不再调用 `CleanText` 修改原文 | ✅ |
| **Prune** | `internal/agent/core/context.go:173-250` | 不再 `hasNoMemoryTool` 跳过；`ToolResults` 传入 `ContextEntry` | ✅ |
| **eventloop** | `internal/agent/core/eventloop.go` | `context.Append`/`emitMemoryCandidate` 传入 `toolResults`；删除 `hasNoMemoryTool` | ✅ |
| **emitMemoryCandidate** | `internal/agent/core/distill.go:380-390` | 签名扩展传 `toolResults`；payload 含 `tool_results` | ✅ |
| **ContextToDoc** | `internal/memory/document/document.go:128-210` | 可选 `cleanFn` 参数；`summarizeEntries`/`extractTags`/`extractEntities` 消费 ToolResults | ✅ |
| **ContextEntry** | `internal/memory/document/document.go:434-440` | 新增 `ToolResults []ToolResultItem` | ✅ |
| **内置插件 cmd** | `internal/plugins/cmd/plugin.go:110-114` | `cmd_run`: `NoMemory=true` | ✅ |
| **内置插件 agentcli** | `internal/plugins/agentcli/plugin.go` | 6 个工具: `NoMemory=true` | ✅ |
| **内置插件 files** | `internal/plugins/files/plugin.go:62-72` | `files_read`: `NoMemory=false` + `Cleaner` 去 JSON 包裹 | ✅ |

### 5.2 SDK 公有仓（全部更新完成 ✅）

| 层次 | 文件 | 改动 | 状态 |
|---|---|---|---|
| **ToolDef 定义** | `homeagentsdk/sdk/plugin.go:90-97` | `NoMemory bool` + `Cleaner func(string) string` | ✅ |
| **版本号** | `homeagentsdk/meta/meta.go:8` | `v0.7.1` → `v0.8.0` | ✅ |
| **单元测试** | `homeagentsdk/sdk/plugin_test.go` | 已含 `TestToolDefCleaner`/`TestToolDefNoMemory`/`TestToolDefRegisterPreservesNoMemory` | ✅ |
| **示例插件** | `homeagentsdk/example/qq/plugin.go` | `regTool` 签名已扩展为 `def sdk.ToolDef`；12 查询工具 `NoMemory=false`（含 Cleaner 6 个），6 操作工具 `NoMemory=true` | ✅ |
| **示例插件** | `homeagentsdk/example/files/plugin.go` | `files_read` `NoMemory=false` + `Cleaner` | ✅ |
| **示例插件** | `homeagentsdk/example/browser/plugin.go` | `search/fetch/render` 加 `Cleaner` | ✅ |
| **示例插件** | `homeagentsdk/example/a2a/plugin.go` | `a2a_query` 加 `Cleaner` | ✅ |
| **示例插件** | `homeagentsdk/example/bili/plugin.go` | `bili_video` 加 `Cleaner` | ✅ |
| **示例插件** | `homeagentsdk/example/editdoc/plugin.go` | `edit_document` `NoMemory=true` | ✅ |
| **示例插件** | `homeagentsdk/example/music/plugin.go` | `music_search` 加 `Cleaner` | ✅ |
| **示例插件** | `homeagentsdk/example/ocr/plugin.go` | `ocr_image` 加 `Cleaner` | ✅ |
| **示例插件** | `homeagentsdk/example/rss/plugin.go` | `subscribe/unsubscribe/check_now` `NoMemory=true` | ✅ |
| **生成模板** | `homeagentsdk/tools/plugindev/templates.go` | `tmplPluginGo` 展示 `NoMemory` + `Cleaner`（注释） | ✅ |

---

## 六、重构后二次审查：工具输出未接入记忆管道（已修复 ✅）

> **原始发现（历史记录）：** 前一 agent 只改了"删除坏逻辑"（删 TextCleaner、加字段），没改"接入好逻辑"。
> **当前状态：** 以下 7 项缺陷已在后续迭代中全部修复。详情参见 `plan.md §7`。

### 6.1 审查背景（历史）

前一 agent 按 `plan.md` 实施了重构。审查发现：**删旧代码的工作完成，但"接新数据流"的工作未做**。Cleaner 和 NoMemory 的消费端全是空壳。

### 6.2 修复后数据流（当前现状 ✅）

```
process.go:228  result = a.executeToolCall(tc)
       │
       ├──→ msgs (line 248)                    ← LLM 对话上下文
       │
       ├──→ toolResults = append(...)           ← ✅ 已收集到返回值
       │
       └──→ return (response, toolsUsed, toolResults)

eventloop.go:328-335
a.context.Append(ContextEvent{
    Input:       input,
    Response:    response,
    ToolsUsed:   toolsUsed,
    ToolResults: toolResults,                  ← ✅ 已传入
})
  → computeVector → textForVector
      → 遍历 ToolResults, NoMemory 跳过, Cleaner 过滤 ✅

emitMemoryCandidate(source, input, response, toolResults, toolsUsed)  ✅

Prune → ContextToDoc:
  ContextEntry.ToolResults → summarizeEntries/extractTags/extractEntities  ✅
```

### 6.3 修复清单（7 项断点全部修复 ✅）

| # | 位置 | 原缺陷 | 修复状态 |
|---|---|---|---|
| **1** | `context.go:19-26` `ContextEvent` | 缺 `ToolResults` 字段 | ✅ `ToolResults []ToolResultItem` 已新增 |
| **2** | `process.go:18` 返回值签名 | 没返回工具输出 | ✅ 签名增加 `toolResults []ToolResultItem` |
| **3** | `process.go:228` 工具执行后 | 未收集到返回值 | ✅ `toolResults = append(toolResults, ...)` |
| **4** | `eventloop.go:327-333` `context.Append` | 工具输出未进存储层 | ✅ 传入 `ToolResults` |
| **5** | `context.go:72-82` `textForVector` | `_ = toolDefLookup` 空壳 | ✅ 遍历 ToolResults，应用 Cleaner/NoMemory |
| **6** | `distill.go:380-389` `emitMemoryCandidate` | 没传工具输出 | ✅ 签名扩展为 `(..., toolResults, toolsUsed)` |
| **7** | `context.go:200-210` `Prune→ContextToDoc` | 归档时工具输出丢失 | ✅ `ContextEntry.ToolResults` + `convertToolResults()` |

### 6.4 修复后数据流示例

```
用户: "服务器上 Python 文件有哪些？"
→ process()
  → executeToolCall("files_read")
    → result = "main.py, utils.py, deploy.py"
  → toolResults = [{Name:"files_read", Output:"main.py, utils.py, deploy.py"}]
  → LLM 回复 "有好几个呢～"
  → return (response, toolsUsed, toolResults)

→ context.Append({..., ToolResults: [{Name:"files_read", Output:"..."}]})
  → computeVector → textForVector
    → "有好几个呢～ deploy.py, main.py, utils.py"  (Cleaner 去 JSON 包裹)
    → Vectorize → 向量包含工具输出内容 ✅

→ emitMemoryCandidate(input, response, toolResults, toolsUsed)
  → textMem: 记录了工具输出 ✅

用户: "deploy.py 在哪个目录？"
→ context.Prune → 语义检索匹配到 deploy.py ✅
```

### 6.5 修复验证

| 场景 | 行为 | 状态 |
|---|---|---|
| NoMemory 工具 (`cmd_run`) | 工具输出进 `ContextEvent.ToolResults`，但 `textForVector` 跳过；LLM 回复正常向量化 | ✅ |
| Cleaner 工具 (`files_read`) | `ToolResults[0].Output` 保留原文 JSON，`textForVector` 中 Cleaner 提取 `content` 字段后参与向量化 | ✅ |
| Prune 归档 | 工具输出通过 `ContextEntry.ToolResults` 传入 `ContextToDoc`，`summarizeEntries`/`extractTags`/`extractEntities` 消费 | ✅ |
| 文档蒸馏 | `Doc.Content` 包含 `[工具] name: output` 行，`docToTriples` 直接 `CutExact`（Cleaner/NoMemory 在此暂未应用，因 doc.Content 不含原始工具输出结构） | ⚠️ 按设计保留 |
