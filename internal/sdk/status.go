package sdk

// StatusAPI provides a snapshot of the kernel runtime status.
type StatusAPI interface {
	GetKernelStatus() *KernelStatus
}

// KernelStatus is the aggregated runtime snapshot of all kernel subsystems.
type KernelStatus struct {
	Uptime    string `json:"uptime"`
	StartTime string `json:"start_time"`

	// Build 是内核自身的版本与构建信息。
	//
	// 为何需要：此前 WebUI 展示的“版本”取自 `sdk.SDKVersion`，那是 **SDK 仓
	// meta.Version 的硬编码值**；而 `-ldflags` 注入的是内核的
	// `internal/meta.Version`——两条链完全不相交。于是：
	//   - 构建时注入的真实版本号、commit、构建时间全部丢失；
	//   - 两仲版本号碰巧相等时看不出问题，一旦不等就报错版本；
	//   - 无法回答“现网跑的是哪个 commit 构出来的”。
	Build BuildStatus `json:"build"`

	AgentID string `json:"agent_id"`

	Plugins  []PluginInfo  `json:"plugins"`
	Tools    []ToolDef     `json:"tools"`
	Channels []ChannelInfo `json:"channels"`

	Memory     MemoryStatus     `json:"memory"`
	Knowledge  KnowledgeStatus  `json:"knowledge"`
	Documents  DocumentStatus   `json:"documents"`
	TextMemory TextMemoryStatus `json:"text_memory"`
	Social     SocialStatus     `json:"social"`

	LLM LLMStatus `json:"llm"`

	Context ContextStatus `json:"context"`

	Runtime RuntimeStatus `json:"runtime"`

	Tracker TrackerStatus `json:"tracker"`
}

type PluginInfo struct {
	Name   string `json:"name"`
	Loaded bool   `json:"loaded"`
}

type ChannelInfo struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Ready bool   `json:"ready"`
}

type MemoryStatus struct {
	Available     bool `json:"available"`
	EntityCount   int  `json:"entity_count"`
	RelationCount int  `json:"relation_count"`
	EntityTypes   int  `json:"entity_types"`
}

type KnowledgeStatus struct {
	Available bool     `json:"available"`
	ItemCount int      `json:"item_count"`
	Items     []string `json:"items,omitempty"`
}

type DocumentStatus struct {
	Available   bool `json:"available"`
	DocCount    int  `json:"doc_count"`
	VectorCount int  `json:"vector_count"`
}

type TextMemoryStatus struct {
	Available bool `json:"available"`
	FileCount int  `json:"file_count"`
}

type SocialStatus struct {
	Available   bool `json:"available"`
	PersonCount int  `json:"person_count"`
}

type LLMStatus struct {
	Available bool   `json:"available"`
	Provider  string `json:"provider,omitempty"`
	Sources   int    `json:"sources,omitempty"`
}

type ContextStatus struct {
	EventCount int `json:"event_count,omitempty"`
}

type RuntimeStatus struct {
	Goroutines int    `json:"goroutines"`
	MemoryMB   int64  `json:"memory_mb"`
	GoVersion  string `json:"go_version"`
}

// BuildStatus 是内核二进制的构建身份，全部来自 `internal/meta`
// （由 -ldflags 在构建时注入，未注入时为源码默认值/unknown）。
type BuildStatus struct {
	// Version 是内核语义版本（如 1.0.0）。
	Version string `json:"version"`
	// Commit 是构建时的 Git 短 hash；未注入为 unknown。
	Commit string `json:"commit"`
	// BuildTime 是 UTC 构建时间；未注入为 unknown。
	BuildTime string `json:"build_time"`
	// SDKCompatible 是本内核可兼容的最高 SDK 版本。
	// 插件用旧 SDK 编译时靠它判断能不能加载。
	SDKCompatible string `json:"sdk_compatible"`
	// KernelName 是内核名（HomeAgent）。
	KernelName string `json:"kernel_name"`
	// SourceURL 是本次构建对应的源码地址。AGPL-3.0 §13 要求：向使用者提供网络
	// 服务时，要给他们拿到 Corresponding Source 的机会——WebUI 状态页会把它渲染成
	// 可见链接，所以**修改后对外部署的分支必须把这个值改指向自己的源码仓库**。
	SourceURL string `json:"source_url,omitempty"`
}

type TrackerStatus struct {
	Available bool   `json:"available"`
	Dir       string `json:"dir,omitempty"`
}
