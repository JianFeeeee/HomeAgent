package sdk

// StatusAPI provides a snapshot of the kernel runtime status.
type StatusAPI interface {
	GetKernelStatus() *KernelStatus
}

// KernelStatus is the aggregated runtime snapshot of all kernel subsystems.
type KernelStatus struct {
	Uptime    string `json:"uptime"`
	StartTime string `json:"start_time"`

	AgentID string `json:"agent_id"`

	Plugins  []PluginInfo  `json:"plugins"`
	Tools    []ToolDef     `json:"tools"`
	Channels []ChannelInfo `json:"channels"`

	Memory     MemoryStatus     `json:"memory"`
	Knowledge  KnowledgeStatus  `json:"knowledge"`
	Documents  DocumentStatus   `json:"documents"`
	TextMemory TextMemoryStatus `json:"text_memory"`
	Social     SocialStatus     `json:"social"`
	Skills     SkillsStatus     `json:"skills"`

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

type SkillsStatus struct {
	Available bool     `json:"available"`
	SkillList []string `json:"skill_list,omitempty"`
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

type TrackerStatus struct {
	Available bool   `json:"available"`
	Dir       string `json:"dir,omitempty"`
}
