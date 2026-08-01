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
	// ExecuteTool executes a tool by name, resolving across the stage host first.
	ExecuteTool(name string, args map[string]interface{}) (interface{}, error)
}
