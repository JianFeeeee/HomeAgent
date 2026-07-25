# 记忆系统重构计划 — NoMemory 与 TextCleaner 设计修正

> **更新日期：** 2026-07-25
> **实施状态：** 全部完成 ✅

基于 `review.md` 审查结论，核心层两个设计偏差已全部修复：
- `RegisterTextCleaner` → 拆为 `ToolDef.Cleaner`（工具级，仅计算层生效）✅
- `hasNoMemoryTool` → 工具输出不参与向量/jieba/蒸馏，原文保留 ✅

---

## 第九阶段：SDK 示例插件更新（全部完成 ✅）

> **当前状态：** 所有示例插件已按以下分类添加 `NoMemory`/`Cleaner` 字段。其中多数插件由前期迭代完成，`editdoc`/`rss`/`qq` 三个插件在本次审查后补全。

### 9.1 背景

SDK 公有仓 `homeagentsdk/example/` 中的示例插件全部使用基础 `ToolDef`（仅 `Name`/`Description`/`Parameters`），未展示 `NoMemory`/`Cleaner` 用法，新插件开发者无从知晓这些字段。

### 9.2 QQ 插件分析

**文件：** `homeagentsdk/example/qq/plugin.go`

QQ 插件通过 `regTool` 包装方法注册（line 437），已扩展为直接透传 `sdk.ToolDef`：

```go
func (p *Plugin) regTool(s *sdk.PluginSDK, def sdk.ToolDef, handler sdk.ToolHandler) {
    s.RegisterTool(def.Name, def, handler)
}
```

备选方案（保持便捷性但增加可选参数）：
```go
func (p *Plugin) regTool(s *sdk.PluginSDK, name, desc string, params map[string]interface{}, handler sdk.ToolHandler, opts ...ToolOpt) {
    def := sdk.ToolDef{Name: name, Description: desc, Parameters: params}
    for _, o := range opts { o(&def) }
    s.RegisterTool(name, def, handler)
}
type ToolOpt func(*sdk.ToolDef)
func WithNoMemory() ToolOpt { return func(d *sdk.ToolDef) { d.NoMemory = true } }
func WithCleaner(fn func(string) string) ToolOpt { return func(d *sdk.ToolDef) { d.Cleaner = fn } }
```

### 9.3 示例插件完整清单

| 示例插件 | 工具数 | 实际状态 |
|---------|--------|---------|
| `files/plugin.go` | 4 (read/write/edit/ls) | `files_read` `NoMemory=false` + `Cleaner` ✅ |
| `memo/plugin.go` | 3 | 无需改动 ✅ |
| `weather/plugin.go` | 3 | 无需改动 ✅ |
| `browser/plugin.go` | 11 | `search/fetch/render` `Cleaner` ✅ |
| `qq/plugin.go` | 18 | `regTool` 已扩展 `def sdk.ToolDef`；12 查询类 `NoMemory=false`（6 个加 `Cleaner`），6 操作类 `NoMemory=true` ✅（本次补全） |
| `a2a/plugin.go` | 4 | `a2a_query` `Cleaner` ✅ |
| `ai_image/plugin.go` | 1 | 无需 Cleaner ✅ |
| `bili/plugin.go` | 1 | `bili_video` `Cleaner` ✅ |
| `calendar/plugin.go` | 6 | 无需改动 ✅ |
| `editdoc/plugin.go` | 1 | `edit_document` `NoMemory=true` ✅（本次补全） |
| `music/plugin.go` | 2 | `music_search` `Cleaner` ✅ |
| `ocr/plugin.go` | 1 | `ocr_image` `Cleaner` ✅ |
| `rss/plugin.go` | 4 | `subscribe/unsubscribe/check_now` `NoMemory=true` ✅（本次补全） |
| `sanitizer/plugin.go` | 0 (stage only) | 无需改动 ✅ |

### 9.4 各示例插件具体改动（均已实施 ✅）

**`files/plugin.go`** — `files_read` `NoMemory=false` + Cleaner（提取 JSON `.content` 字段），与核心仓内置插件对齐。✅

**`browser/plugin.go`** — `search/fetch/render` 注册 Cleaner 提取正文。✅

**`qq/plugin.go`** — 各工具分类（全部已实施）：
- 查询类（NoMemory=false，6 个加 Cleaner 提取 `.content`）: `get_message`, `get_history`, `read_document`, `video_download`, `get_group_files`, `get_download_tasks`, `get_groups`, `get_friends`, `get_recent_contacts`, `resolve_name`, `resolve_nickname`, `get_group_member_info`
- 操作类（NoMemory=true）: `send_file`, `download_file`, `upload_group_file`, `group_manage`, `friend_action`, `send_like`

**`a2a/plugin.go`** — `a2a_query` Cleaner 提取 response 文本。✅

**`bili/plugin.go`** — `bili_video` Cleaner 提取视频信息文本。✅

**`editdoc/plugin.go`** — `edit_document` NoMemory=true。✅（本次补全）

**`music/plugin.go`** — `music_search` Cleaner 提取纯文本。✅

**`ocr/plugin.go`** — `ocr_image` Cleaner 确保纯文本进入计算层。✅

**`rss/plugin.go`** — `subscribe/unsubscribe/check_now` NoMemory=true。✅（本次补全）

**无需改动：** `memo`, `weather`, `calendar`, `sanitizer`, `ai_image`（输出简短或结构化，无噪音）。

---

## 第十阶段：plugindev 工具链更新

> **当前状态：** 生成模板已更新 ✅，mock 调试框架尚未实施。

### 10.1 生成模板更新 ✅

**文件：** `homeagentsdk/tools/plugindev/templates.go`

`tmplPluginGo` 模板（line 48-57）生成的注册代码已展示 `NoMemory`/`Cleaner` 用法：

```go
// 修改前：
s.RegisterTool(tp+"hello", sdk.ToolDef{
    Name: tp + "hello", Description: "A hello world tool",
    Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
}, p.handleHello)

// 修改后：
s.RegisterTool(tp+"hello", sdk.ToolDef{
    Name:        tp + "hello",
    Description: "A hello world tool",
    Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
    NoMemory:    false,  // 工具输出对 LLM 注意力有信号价值时为 false，纯操作工具为 true
    // Cleaner:  func(output string) string {
    //     // 工具输出参与向量化/jieba/蒸馏前，在此过滤噪音
    //     return output
    // },
}, p.handleHello)
```

`tmplMainLua` Lua 模板同理，在注册工具时添加 `no_memory` 注释示范。

### 10.2 C ABI 桥接无需改动

- `Cleaner` 是 `func` 类型 + `json:"-"`，天然无法序列化过 C 边界（正确行为）
- `NoMemory` 是 `bool` + `json:"no_memory,omitempty"`，JSON 序列化后自动包含

### 10.3 新增 mock 调试框架

**背景：** 当前 `plugindev debug` 对 Go 插件仅打印 "use standard Go tooling"（`cmd_debug.go:123-132`），无任何 mock 能力。Lua 插件虽有 REPL，但无法模拟完整的 agent 消息管道。

**需求：** 在 `plugindev` 中新增一个 mock 调试框架，让插件开发者可以在本地模拟消息处理流程，无需连接真实 agent 内核。

**设计要点：**

```
plugindev debug --mock <dir>
```

Mock 框架应提供：

1. **Mock StageContext 构建器** — 通过 CLI 或 YAML/JSON 配置文件构建模拟的 `StageContext`，包含：
   - `RawMessage` / `UserID` / `GroupID`
   - `ToolCalls` / `ToolResults`
   - `LLMText` / `FinalText`
   - `NoMemory` / `Memory` / `Extra`

2. **Mock API 实现** — 为 `MemoryAPI`、`TextMemoryAPI`、`DocMemoryAPI`、`KnowledgeAPI`、`LLMAPI`、`SettingsAPI`、`SocialAPI` 提供内存 mock 实现：
   ```go
   // 内置 mock 实现，开发者可直接使用
   mockSDK := sdk.New("test-plugin", mockSettings, mockRegTool, mockRegStage, mockRegAPI, mockRegOutput)
   mockSDK.SetMemoryAPI(NewMockMemory())
   mockSDK.SetLLMAPI(NewMockLLM())
   ```

3. **Stage 触发模拟** — 支持手动触发各个 Stage：
   ```go
   // 模拟 before_toolcall 阶段
   ctx := sdk.StageContext{
       ToolCalls: []sdk.ToolCall{{Name: "files_read", Arguments: {"path": "/test.txt"}}},
   }
   host.RunStage(sdk.StageBeforeToolcall, &ctx)
   ```

4. **工具直接调用** — 按名称调用已注册的工具并验证返回值：
   ```go
   result, err := host.ExecuteTool("files_read", map[string]interface{}{"path": "/test.txt"})
   ```

5. **配置文件驱动** — 支持 YAML/JSON 测试场景文件：
   ```yaml
   # test_scenario.yaml
   stages:
     - stage: before_toolcall
       context:
         tool_calls:
           - name: files_read
             arguments:
               path: "/test.txt"
       expectations:
         - check: context.modified
           path: "tool_calls[0].arguments.path"
           equals: "/test.txt"
   ```

6. **集成 `go test`** — 提供 `mocktest` 包，插件开发者可在 `_test.go` 中直接使用：

   ```go
   // homeagentsdk/example/files/plugin_test.go
   func TestFilesReadTool(t *testing.T) {
       m := mocktest.New(t)
       p := &Plugin{name: "files", filesDir: t.TempDir()}
       os.WriteFile(filepath.Join(p.filesDir, "test.txt"), []byte("hello"), 0644)
       
       m.RegisterPlugin(p)
       m.ToolShouldReturn(t, "files_read", map[string]interface{}{"path": "test.txt"},
           map[string]interface{}{"content": "hello"})
   }
   ```

---

## 第十一阶段：文档更新（全部完成 ✅）

### 11.1 核心仓文档

**文件：**
- `docs/zh/PLUGIN_DEV.md:248` — 中文插件开发指南
- `docs/en/PLUGIN_DEV.md:250` — 英文插件开发指南
- `docs/zh/ARCHITECTURE.md:11` — 中文架构文档
- `docs/en/ARCHITECTURE.md:11` — 英文架构文档

**PLUGIN_DEV.md 改动：** 在注册工具示例中展示 `NoMemory`/`Cleaner` 用法：

```go
// 修改前：
s.RegisterTool("weather_query", sdk.ToolDef{
    Name:        "weather_query",
    Description: "Get current weather...",
    Parameters:  map[string]interface{}{...},
}, handler)

// 修改后：
s.RegisterTool("weather_query", sdk.ToolDef{
    Name:        "weather_query",
    Description: "Get current weather...",
    Parameters:  map[string]interface{}{...},
    NoMemory:    false,                           // ← 文档新增
    // Cleaner:  func(output string) string {     // ← 文档新增（注释示范）
    //     return extractJSON(output, "content")
    // },
}, handler)
```

并在文档中新增独立章节说明 NoMemory 和 Cleaner 的设计意图与使用场景。

**ARCHITECTURE.md 改动：** 在阶段管道说明中补充 ToolDef 的 NoMemory/Cleaner 字段描述。

### 11.2 SDK 仓文档

**文件：** `homeagentsdk/README.md` / `README_EN.md`

在 SDK README 的 ToolDef 说明中列出新增字段。

---

## SDK 公有仓实施状态清单

| 文件 | 操作 | 阶段 | 优先级 | 状态 |
|---|---|---|---|---|
| `homeagentsdk/sdk/plugin_test.go` | `TestToolDefCleaner`/`TestToolDefNoMemory`/`TestToolDefRegisterPreservesNoMemory` | 一 | P0 | ✅ |
| `homeagentsdk/example/qq/plugin.go` | `regTool` 签名扩展 `def sdk.ToolDef` + 各工具 NoMemory/Cleaner | 九 | P1 | ✅（本次补全） |
| `homeagentsdk/example/files/plugin.go` | `files_read` `NoMemory=false` + `Cleaner` | 九 | P1 | ✅ |
| `homeagentsdk/example/browser/plugin.go` | `search/fetch/render` `Cleaner` | 九 | P1 | ✅ |
| `homeagentsdk/example/a2a/plugin.go` | `a2a_query` `Cleaner` | 九 | P1 | ✅ |
| `homeagentsdk/example/bili/plugin.go` | `bili_video` `Cleaner` | 九 | P1 | ✅ |
| `homeagentsdk/example/editdoc/plugin.go` | `edit_document` `NoMemory=true` | 九 | P1 | ✅（本次补全） |
| `homeagentsdk/example/music/plugin.go` | `music_search` `Cleaner` | 九 | P1 | ✅ |
| `homeagentsdk/example/ocr/plugin.go` | `ocr_image` `Cleaner` | 九 | P1 | ✅ |
| `homeagentsdk/example/rss/plugin.go` | `subscribe/unsubscribe/check_now` `NoMemory=true` | 九 | P1 | ✅（本次补全） |
| `homeagentsdk/tools/plugindev/templates.go` | `tmplPluginGo` 展示 `NoMemory` + `Cleaner` | 十 | P2 | ✅（本次补全） |
| `homeagentsdk/tools/plugindev/...` | mock 调试框架 | 十 | P2 | ❌ |
| `docs/zh/PLUGIN_DEV.md` | 注册工具示例展示 NoMemory/Cleaner + 独立说明章节 | 十一 | P3 | ✅（本次补全） |
| `docs/en/PLUGIN_DEV.md` | 同上（英文版） | 十一 | P3 | ✅（本次补全） |
| `docs/zh/ARCHITECTURE.md` | 补充 ToolDef 新字段描述 | 十一 | P3 | ✅（本次补全） |
| `docs/en/ARCHITECTURE.md` | 同上（英文版） | 十一 | P3 | ✅（本次补全） |
| `homeagentsdk/README.md` / `README_EN.md` | 在 ToolDef 说明中列出新增字段 | 十一 | P3 | ✅（本次补全） |

### 实施顺序

```
P0 (单元测试) ── 已完成 ✅
P1 (示例插件) ── 全部完成 ✅
P2 (生成模板) ── tmplPluginGo 已完成 ✅，mock 框架待实施 ❌
P3 (文档)     ── 全部完成 ✅
```
