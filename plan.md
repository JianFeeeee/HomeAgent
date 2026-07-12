# HomeAgent 修复计划

> 基于 `修复计划.md`，按优先级逐项推进。

---

## P0 — 功能正确性（必须修）

### P0-1 HTTP 401/403 检测靠字符串搜索
- **文件**: `internal/agent/core/agent.go:708-711`
- **问题**: `strings.Contains(errStr, "401")` 不可靠，Lua Adapter 返回格式不固定
- **修复**: `ProviderManager` 增加 `ReportStatus(name, statusCode)`，在 `LuaAdaptedProvider.Chat()` 中根据 `resp.StatusCode` 精确判断

### P0-2 deploy/homeagent.service 传递 -config 参数使 homed 启动崩溃
- **文件**: `deploy/homeagent.service:9`
- **问题**: `ExecStart` 含 `-config`，但 `cmd/homed/main.go` 未定义此 flag
- **修复**: 删除 `-config` 参数

---

## P2 — 中风险

### P2-1 context.go 每次 Append/Prune 全量写盘
- **文件**: `internal/agent/core/context.go:88-101`
- **问题**: 30 条事件 JSON 全量写入文件每次操作，高频 I/O 瓶颈
- **修复**: 增加 debounce 定时写入（每 5s flush）

### P2-2 output_set_channel 枚举硬编码
- **文件**: `internal/agent/core/agent.go:1927-1928`
- **问题**: channel enum 硬编码为 `{"voice", "email", "screen", "http"}`，与 Device 动态注册脱节
- **修复**: 从 `a.io.ListChannels()` 动态生成 enum

### P2-3 describe_image/transcribe_audio/ocr_image 三重复代码
- **文件**: `internal/agent/core/agent.go:2682-2828`
- **问题**: 三个函数共享相同的 base64/data URL 处理、timeout、消息构造逻辑
- **修复**: 抽取 `mediaRequest(mediaType, prompt, args) → string` 公共方法

### P2-4 两处 sources map 硬编码重复
- **文件**: `internal/config/registry.go:233-244` + `:302-313`
- **问题**: `seedDBValues` 和 `seedCoreDefs` 各写了一遍完全相同的 sources map
- **修复**: 抽取公共 var `defaultSources`

---

## P3 — 低风险（死代码删除/清理）

### P3-1 删除 messagesToMap 死函数
- **文件**: `internal/agent/api/provider.go:731-740`

### P3-2 删除 RunStageAll 死函数
- **文件**: `internal/agent/core/stages.go:106-108`

### P3-3 删除 internal/embed/ 整包死代码
- **文件**: `internal/embed/embedder.go`（162 行，无任何 import）

### P3-4 删除 internal/tokenizer/jieba.go 死代码
- **文件**: `internal/tokenizer/jieba.go`（81 行，Global() 从未被调用）

### P3-5 删除 internal/container/ 整包死代码
- **文件**: `internal/container/manager.go`（209 行，NewManager 从未被调用）

### P3-6 删除 internal/snapshot/ 整包死代码
- **文件**: `internal/snapshot/manager.go`（196 行，import container 但自身也死）

---

## 仓库清理

### CL-1 go.work 版本不一致
- **文件**: `go.work:1`
- **问题**: 声明 `go 1.19` 但 SDK 模块要求 `go 1.21`
- **修复**: 升级到 `go 1.21`

### CL-2 .gitignore 补充
- **文件**: `.gitignore`
- **修复**: 添加 `data/` 和 `*.db`

### CL-3 文档路径修正
- **文件**: `docs/ARCHITECTURE.md`
- **问题**: L283 `sdk/` 条目应指向 `internal/sdk/`

---

---

## C1 — 架构清理：删除 output_set_channel

> **背景**: 架构原则要求 LLM 主动调用输出工具进行输出。`output_set_channel` 作为一个全局隐式状态，
> 与 `output_send` 的精确指定模式重叠，且 LLM 可能忘记自己设过该状态导致回复走错通道。

### C1-1 删除 output_set_channel 工具定义
- **文件**: `internal/agent/core/agent.go` — `buildToolDefs()` 中删除 `output_set_channel` 的 tool definition block
- **影响**: `output_send` 和 `output_list_channels` 保留，它们的 `chanDesc` 动态生成逻辑不动

### C1-2 删除 executeOutputChannelTool 方法
- **文件**: `internal/agent/core/agent.go` — 删除 `executeOutputChannelTool()` 函数体

### C1-3 删除 output_set_channel 路由分支
- **文件**: `internal/agent/core/agent.go` — `executeToolCall()` 中删除 `case tc.Name == "output_set_channel"`

### C1-4 更新子 Agent 输出工具黑名单
- **文件**: `internal/agent/core/agent.go`
  - `outputTools` map 中移除 `"output_set_channel"`
  - 子工具检查中移除 `ct.Name == "output_set_channel"`

### C1-5 更新注释
- **文件**: `internal/agent/core/agent.go:555` — 移除"可能已被 AI 通过 output_set_channel 切换"的注释

### C1-6 清理测试文件
- **文件**: `internal/agent/core/agent_tools_test.go`
  - 删除 `TestExecuteOutputChannelTool` 和 `TestExecuteOutputChannelToolEmpty`
  - `TestBuildToolDefsOutputToolsAlwaysPresent` 中移除 `output_set_channel` 检查
  - `TestGetAllToolsEmpty` 最小工具数从 3 改为 2

### C1-7 验证编译与测试通过

---

## 执行顺序

```
P0-1 → P0-2 → P2-1 → P2-2 → P2-3 → P2-4 → P3-1~6 → CL-1~3 → C1-1~7
```
