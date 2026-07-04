# MILESTONE — 项目进展与路线图

## 已完成

### 插件系统基础
- [x] agentcli 插件：6 个 PTY 终端工具（create/write/read/resize/close/list），完整按键映射
- [x] cmd 插件：`cmd_run` 工具（command/timeout/workdir），8 单元测试
- [x] timer 插件：定时器工具
- [x] 12 个跨插件集成测试

### 非记忆 LLM 调用机制
- [x] `StageContext.NoMemory` 标记 — agent 跳过 `emitMemoryCandidate`
- [x] `InjectTextNoMemoryTo` / `InjectTextSyncNoMemoryTo` — IOManager 层
- [x] `InjectTextNoMemory` / `InjectTextSyncNoMemory` — PluginSDK 层
- [x] `ProviderManager.QuickChat()` — 直连 Provider 的快捷调用

### LLM 驱动健康检查（`healthcheck` 插件）
- [x] `healthcheck_report` 工具 — LLM 上报测试结果
- [x] `testLLMDriven()` — 独立工具循环：LLM 发现→调用→上报
- [x] 动态排除本插件工具（`selfToolNames`），不硬编码插件名
- [x] 核心遵循 OpenAI `/chat/completions` 请求格式，8 个 Lua adapter 脚本支持各厂商 API（DeepSeek/OpenAI/Anthropic/Gemini/Mistral/Groq/GitHub/Ollama），用户可编写自定义 adapter 接入任意 LLM

### 内核状态接口（`StatusProvider`）
- [x] `internal/agent/core/status.go` — `KernelStatus` 聚合快照
- [x] `Agent.GetKernelStatus()` — 实现 `StatusProvider` 接口
- [x] `healthcheck_kernel` 工具 — Agent 可自主查询内核状态
- [x] WebUI `/api/v1/kernel` 端点 — 状态 JSON API
- [x] 所有子系统：plugins / tools / channels / memory / knowledge / documents / text_memory / social / skills / LLM / runtime / tracker

### 技术债务清理
- [x] 移除旧 `test_deepseek` 插件
- [x] 修复 agentcli PTY readLoop 死锁（goroutine reader + close 顺序）
- [x] 修复 knowledge.Store.Delete 不存在方法

### P0 — WebUI 重构
- [x] 完整的 SPA 仪表盘（7 标签页：概览/对话/插件/记忆/知识/设置/内核）
- [x] 深色主题，响应式布局
- [x] 使用 `//go:embed dashboard.html` 替代硬编码 HTML 变量
- [x] 全部 REST API 端点保持兼容

### P1 — OpenClaw 兼容插件（Go 插件 + Node.js sidecar + 模拟器架构）
- [x] 设计 Go ↔ Node.js 通信协议（JSON-RPC 2.0 over stdio）
- [x] Go 侧 sidecar 管理器（进程启动、心跳、重启、`waitReady`）
- [x] 工具发现（`tools/list`）和执行（`tools/call`）转译
- [x] Node.js 测试插件 `echoplugin`（echo/add/ping 三个工具）
- [x] 5 个端到端测试（无 main.js 兜底、列表、调用 echo/add、不存在的工具、并发调用）
- [x] 现有 SKILL.md 加载路径保留（三通道：SKILL.md / sidecar / simulator）
- [x] Node.js OpenClaw 模拟进程（`simulator/main.js`）：完整实现 `OpenClawPluginApi`，加载任意遵循 OpenClaw 插件标准的真实插件
- [x] 模拟器内嵌于 Go 二进制（`//go:embed`），启动时自动提取
- [x] 支持 `package.json#openclaw.extensions/runtimeExtensions` 入口发现，兼容 `.ts`→`.js` 编译回退
- [x] 用 ClawHub 真实插件（chart-plot）验证端到端链路通过

### P2 — 进一步优化
- [x] Healthcheck 定时自动执行（`startAutoCheck` goroutine，默认 30 分钟）
- [x] Agent 自主健康状态感知（通过 `healthcheck_kernel`）
- [x] 更多 WebUI 可视化面板（7 标签页 SPA）
- [x] 性能监控与告警（`healthcheck_perf` 工具 + `PerfData` 历史记录 100 条）

---

## 当前状态

所有 P0 / P1 / P2 计划项已完成。项目处于维护和迭代阶段。

### 已注册的工具（14 个）
- `cmd_run` — 命令执行
- `terminal_create / write / read / resize / close / list` — PTY 终端
- `healthcheck` — 全面健康检查（含 LLM 驱动阶段）
- `healthcheck_plugins` — 列出已加载插件
- `healthcheck_tools` — 列出所有已注册工具
- `healthcheck_memory` — 图记忆系统测试
- `healthcheck_report` — LLM 汇报接口
- `healthcheck_kernel` — 内核状态快照
- `healthcheck_perf` — 性能监控数据
- `timer_set` — 定时器

### 内置插件（7 个）
- agentcli — PTY 终端管理
- cli — Unix socket 通信
- cmd — 命令执行
- healthcheck — 健康检查 + 性能监控 + 自动调度
- mcp — MCP 协议支持
- openclaw — SKILL.md + Node.js sidecar 双通道
- webui — HTTP 服务器 + SPA 仪表盘
- timer — 定时器
