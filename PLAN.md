# HomeAgent 实施计划

## 已完成

### Phase 0 — 核心基础设施 ✅

| 任务 | 文件 | 状态 |
|------|------|------|
| SDK 接口定义 | `internal/plugin/sdk/api.go` | ✅ |
| PluginAPI（RegisterTool/RegisterStage/Subscribe/Publish） | `internal/plugin/sdk/api.go` | ✅ |
| 插件内部 EventBus | `internal/plugin/sdk/bus.go` | ✅ |
| 系统 EventBus | `internal/events/bus.go` | ✅ |
| StageHost 编排器 | `internal/agent/core/stages.go` | ✅ |
| Agent 阶段注入（7 个 hook 点） | `internal/agent/core/agent.go` | ✅ |
| 插件注册表 SDK 支持 | `internal/plugin/plugin.go` | ✅ |
| main.go 接入 EventBus + StageHost | `cmd/homed/main.go` | ✅ |
| 架构文档 v4 | `docs/ARCHITECTURE.md` | ✅ |

---

## 待实施

### Phase 1 — 插件 SDK 迁移（当前）

| # | 任务 | 说明 | 优先级 |
|---|------|------|--------|
| 1.1 | SDK 添加 `ToolDef` 参数描述支持 | `RegisterTool` 接受 `ToolDef` 结构体（含 parameters）而非纯 handler | high |
| 1.2 | StageHost 收集完整 ToolDef | 目前只传 name，需传完整 description + parameters 给 LLM | high |
| 1.3 | Registry.AddPluginAPI 自动构建 StageHost | 替代手动 `syncFromRegistry` | high |
| 1.4 | 添加 `before_toolcall` deny 机制的测试 | 确保 `StageContext.Response` 在工具级别生效 | medium |
| 1.5 | 添加 `on_input` 改写消息的测试 | `stageCtx.RawMessage` 在阶段后被正确使用 | medium |

### Phase 2 — 迁移 WebUI 到 SDK 模式

| # | 任务 | 说明 | 优先级 |
|---|------|------|--------|
| 2.1 | WebUI 改为通过 `PluginAPI` 注册 | 不再依赖 `Device` 接口 | high |
| 2.2 | WebUI 通过 `Subscribe(EventAll)` 获取所有事件 | 取代 OutputChan 监听 | high |
| 2.3 | WebUI 注册 `output_send` 工具 | 通过 `RegisterTool` 暴露给 LLM | high |
| 2.4 | 删除 `internal/api/plugin.go` 的 Device 包装 | 不再需要 `Device` 适配器 | medium |
| 2.5 | Handler 改为通过 EventBus 获取 IOManager 引用 | 减少直接依赖 | low |

### Phase 3 — 迁移 QQ/OneBot 到 SDK 模式

| # | 任务 | 说明 | 优先级 |
|---|------|------|--------|
| 3.1 | OneBot 插件改为 `PluginAPI.RegisterTool` | 注册 `qq_send_private_msg` 等工具 | high |
| 3.2 | OneBot 接管后通过 `Publish(raw_input)` 发布事件 | 取代 IOManager.InjectInput | high |
| 3.3 | OneBot 注册阶段钩子 | 可接入群聊特定的 `pre_action` 逻辑 | medium |
| 3.4 | 删除 `internal/onebot/device.go` 的 Device 包装 | SDK 模式原生支持 | medium |

### Phase 4 — 迁移 OutputBus 到 SDK

| # | 任务 | 说明 | 优先级 |
|---|------|------|--------|
| 4.1 | 创建 `internal/outputbus/` 插件 | 管理 `output_send`/`output_list_channels` | high |
| 4.2 | 通过 `RegisterTool` 注册输出工具 | LLM 可直接调用 | high |
| 4.3 | 通过 `RegisterStage(before_output)` 拦截最终文本 | 渠道适配 | medium |
| 4.4 | Agent 内置的 output_* 工具改为委托给 outputbus | 解耦核心 | medium |

### Phase 5 — 清理旧组件

| # | 任务 | 说明 | 优先级 |
|---|------|------|--------|
| 5.1 | 删除 `Device` 接口定义 | 全部迁移后移除 | high |
| 5.2 | 删除 `IOManager.ExecuteTool` | 工具路由走 StageHost | high |
| 5.3 | 删除 `IOManager.EmitOutput`/`EmitOutputTo` | 走 EventBus | medium |
| 5.4 | 删除 `IOManager.AtomicSwapDevices` | 不再需要设备热替换 | medium |
| 5.5 | 删除 `PluginDevice` 包装器 | SDK 模式替代 | medium |
| 5.6 | 删除 `internal/onebot/device.go` | 已迁移到 SDK | high |
| 5.7 | 删除 `internal/api/plugin.go` | 已迁移到 SDK | medium |
| 5.8 | 精简 `cmd/homed/main.go` | 移除设备相关初始化 | medium |

### Phase 6 — 进程隔离

| # | 任务 | 说明 | 优先级 |
|---|------|------|--------|
| 6.1 | 实现 Unix Socket JSON-RPC 传输层 | 进程隔离模式 | low |
| 6.2 | `sdk.Run()` 自动检测 in-process/external | 开发 vs 生产 | low |
| 6.3 | 插件进程管理（启动/停止/健康检查） | Supervisor 扩展 | low |

### Phase 7 — 增强功能

| # | 任务 | 说明 | 优先级 |
|---|------|------|--------|
| 7.1 | WebUI D3.js 力导向图记忆星图 | 已有 API `GET /api/v1/memory/star` | low |
| 7.2 | Model Context Protocol (MCP) 支持 | 标准工具协议 | low |
| 7.3 | 多 Agent 支持 | 每个 Agent 独立上下文 | low |
| 7.4 | Python 插件 SDK | 扩展生态 | low |

---

## 文件最终结构（Phase 5 完成后）

```
HomeAgent/
├── cmd/homed/main.go          — 入口
├── internal/
│   ├── agent/
│   │   ├── core/
│   │   │   ├── agent.go       — Agent 核心
│   │   │   ├── context.go     — 相关性上下文
│   │   │   └── stages.go      — StageHost
│   │   └── api/
│   │       └── provider.go    — LLM Provider
│   ├── events/
│   │   └── bus.go             — 系统事件总线
│   ├── plugin/
│   │   └── sdk/
│   │       ├── api.go         — PluginAPI
│   │       └── bus.go         — 插件 EventBus
│   ├── memory/                — 三层记忆
│   ├── knowledge/             — 知识库
│   ├── tracker/               — 变更追踪
│   ├── supervisor/            — 守护进程
│   └── plugins/               — 插件实现
│       ├── webui/             — HTTP API + 仪表盘
│       ├── onebot/            — QQ 通道
│       └── outputbus/         — 输出通道管理
├── docs/
│   └── ARCHITECTURE.md        — 架构文档
├── DESIGN.md
├── PLAN.md
└── README.md
```

## 设计原则

1. **核心零 IO** — Core 不依赖任何插件、设备、通道实现
2. **三通道标准** — 所有插件通过 Tool/Stage/Event 与核心交互
3. **增量迁移** — 每阶段保持向后兼容，旧组件与新 SDK 并行运行
4. **测试覆盖** — 每阶段提交前确保全部测试通过
