package sdk

// ToolSource 描述内核工具注册表/执行器的只读视图（由 StageHost 实现）。
// 用接口而非具体类型，避免 sdk 依赖内核包（内核包反向依赖 sdk）。
type ToolSource interface {
	GetToolDefs() []ToolDef
	ToolDef(name string) *ToolDef
	ExecuteTool(name string, args map[string]interface{}) (interface{}, error)
}

// ToolAPI exposes the kernel tool registry and executor
// (StageHost for plugin tools + IOManager for device/channel tools).
type ToolAPI interface {
	// GetToolDefs returns all tools registered on the stage host (plugin tools).
	GetToolDefs() []ToolDef
	// GetAllTools returns all tools exposed by IO devices/channels.
	GetAllTools() []ToolDef
	// CanUse 报告「执行该工具是否被授权」。
	//
	// 存在的理由（D4）：设备类工具的授权闸原本只存在于 core 的
	// executeToolCallInner，即**「agent 收到模型 tool_call」那条路径**。
	// 而 ExecuteTool 是**另一条**独立入口，不经那道闸 —— 于是凡是走
	// ToolAPI 的调用都能绕过 AllowedOutputs。实测范围不止序列：
	// cli 插件的 /terminal 就直接经 ToolAPI 调 agentcli 的终端工具
	// （cli/plugin.go:1038 自陈"ToolAPI 已允许跨插件调用工具"）。
	//
	// 语义与内核那道闸**必须一致**（core 的 TestCanUseAgreesWithInnerPath
	// 钉住这一点），否则两条路径判定不同同样是漏洞。
	//
	// 零值实现返回 true：未实现者行为不变（存量插件与测试替身不受影响）。
	CanUse(toolName string, args map[string]interface{}) bool
	// ToolDefByName 按名字查任一来源（StageHost 插件工具 / IO 设备工具）的声明。
	// 插件需要它来在**运行前**判断目标是否存在、是否并发安全 ——
	// 而工具是动态注册的，"不存在"是常态（见 seq 包的 missing 策略）。
	// 查不到返回 nil（调用方按"不存在"处理，不得 panic）。
	ToolDefByName(name string) *ToolDef
	// ExecuteTool executes a tool by name, resolving across the stage host first.
	ExecuteTool(name string, args map[string]interface{}) (interface{}, error)
}
