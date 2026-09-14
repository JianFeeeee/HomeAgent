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
	// InputChannels 是 inputch 的**登记与归属**视图（谁注册、划给了哪个 agent、
	// 容量、默认回程输出）。
	//
	// 为何单列：Channels 只描述设备侧能力，回答不了「这条输入归谁」。驻留子 agent
	// 出现之后，归属才是运行态里最该看见的东西——只画设备能力，等于把
	// 「输入路由发生在进内核之前」这条设计事实藏了起来。
	InputChannels []InputChannelInfo `json:"input_channels"`

	Memory     MemoryStatus     `json:"memory"`
	Knowledge  KnowledgeStatus  `json:"knowledge"`
	Documents  DocumentStatus   `json:"documents"`
	TextMemory TextMemoryStatus `json:"text_memory"`
	Social     SocialStatus     `json:"social"`

	LLM LLMStatus `json:"llm"`

	Context ContextStatus `json:"context"`

	Runtime RuntimeStatus `json:"runtime"`

	// ONNX 报告统一多模态向量空间（ONNX 模型）是否**真的在用**。
	//
	// 为何单列：内核的向量能力是三层降级（统一多模态空间 → 词嵌入 → TF-IDF），
	// 只报「向量可用/不可用」分不清「ONNX 模型已加载」与「退回了纯文本路径」。
	// 模型缺失 / 运行时缺失 / provider 打开失败时这里是 enabled=false + reason。
	ONNX ONNXStatus `json:"onnx"`

	Tracker TrackerStatus `json:"tracker"`

	// Residents 是驻留式子 agent 的运行时视图（数量 = len(Residents)）。
	Residents []ResidentStatus `json:"residents"`

	// Scheduler 是输入调度器的运行时快照（可观测性，设计文档 §11 O1/O2）。
	// M2 起输入不再直接排队在 channel 上，而是经 readyQueue/pendingInterrupts/
	// suspendStack 三集合按优先级调度；这里把这些状态暴露出来。
	Scheduler SchedulerStatus `json:"scheduler"`
}

// SchedulerStatus 是调度器的原子快照 DTO。
type SchedulerStatus struct {
	// Running 是当前执行的任务（空表示空闲）。
	Running *SchedulerTask `json:"running,omitempty"`
	// Immediate 是刚抢占成功、将在下一个安全点立即运行的中断（最多一个）。
	Immediate *SchedulerTask `json:"immediate,omitempty"`
	// ReadyQueueDepth / PendingInterrupts / SuspendStack 是三个集合的深度。
	ReadyQueueDepth   int `json:"ready_queue_depth"`
	PendingInterrupts int `json:"pending_interrupts"`
	SuspendStack      int `json:"suspend_stack"`
	MaxSuspendDepth   int `json:"max_suspend_depth"`

	// InterruptQueues 是**四条中断队列各自的深度**，下标即中断级别（1..4）；
	// 下标 0 恒为 0，这样 level 可以直接当数组下标用，省掉调用方 ±1 的翻译。
	//
	// 为什么单列：PendingInterrupts 只是总数，看不清"堵在哪一级"——
	// 四级中断是抢占优先级，堵在 L1 还是 L4 是完全不同的运行状态。
	InterruptQueues [5]int `json:"interrupt_queues"`

	// SuspendFrames 是中断栈的帧，**栈底 → 栈顶**（只暴露任务标识，不含帧内容）。
	// 深度见 SuspendStack；帧的顺序回答了"谁被谁打断"。
	SuspendFrames []SchedulerFrame `json:"suspend_frames,omitempty"`

	// InterruptsByLevel / PreemptsByLevel 是各级中断的累计计数（下标 1..4）：
	// 前者=被登记次数（含没抢成的），后者=判定可抢占并进入 immediate 的次数。
	InterruptsByLevel [5]uint64 `json:"interrupts_by_level"`
	PreemptsByLevel   [5]uint64 `json:"preempts_by_level"`

	Enqueued  uint64 `json:"enqueued"`
	Executed  uint64 `json:"executed"`
	Rejected  uint64 `json:"rejected"`
	Suspended uint64 `json:"suspended"`
	Resumed   uint64 `json:"resumed"`
	// Preempted = Σ PreemptsByLevel[1..4]，即「真正抢占成功」的次数。
	// 它与 Suspended 不等价（受害者可能先自行结束），因此不是 Suspended 的别名。
	Preempted uint64 `json:"preempted"`
	// Backpressure 是就绪队列满、输入被挡回 channel 的次数（暂时不收，不是丢弃）。
	Backpressure uint64 `json:"backpressure"`
}

// SchedulerFrame 是中断栈里的一帧（供图形化展示"压了几层现场"）。
type SchedulerFrame struct {
	Task SchedulerTask `json:"task"`
}

// ResidentStatus 是驻留式子 agent 的运行时视图。
//
// 为什么进状态面：驻留子是"常驻的独立 agent"，它们的数量、轮次与上下文占用
// 是运行态里最需要一眼看到的东西（此前只在日志里，WebUI 只能显示文字）。
type ResidentStatus struct {
	ID             string   `json:"id"`
	State          string   `json:"state"`
	Rounds         int      `json:"rounds"`
	ContextFull    bool     `json:"context_full"`
	InputChs       []string `json:"input_channels,omitempty"`
	AllowedOutputs []string `json:"allowed_outputs,omitempty"`
	InputChTable   int      `json:"input_ch_table"`
	CreatedAt      string   `json:"created_at,omitempty"`

	// 以下四项是该驻留子**自己的**调度器积压摘要，用于 per-agent 负载环形图。
	//
	// 根 agent 的积压看 KernelStatus.Scheduler；每个驻留子是独立 agent、
	// 各跑各的调度器，必须分别给，否则「哪个子忙」在界面无从判断。
	ReadyQueueDepth   int    `json:"ready_queue_depth"`
	PendingInterrupts int    `json:"pending_interrupts"`
	SuspendStack      int    `json:"suspend_stack"`
	InterruptQueues   [5]int `json:"interrupt_queues"`
}

// SchedulerTask 是任务的最小标识（不暴露帧内容）。
type SchedulerTask struct {
	ID    uint64 `json:"id"`
	Level int    `json:"level"`
	Kind  string `json:"kind"`
}

// ONNXStatus 是统一多模态向量空间（ONNX 模型）的启用状态与身份。
type ONNXStatus struct {
	// Enabled 是 provider 真正打开且元数据合法（不是「配置里写了 provider」）。
	Enabled bool `json:"enabled"`
	// Provider 是配置指定的 provider 名（如 chineseclip / qwen3vl / http）。
	Provider    string   `json:"provider,omitempty"`
	Dim         int      `json:"dim,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	Modalities  []string `json:"modalities,omitempty"`
	// Reason 是未启用时的原因（未配置 / 打开失败的具体错误 / 其它）。
	Reason string `json:"reason,omitempty"`
}

type PluginInfo struct {
	Name   string `json:"name"`
	Loaded bool   `json:"loaded"`
}

type ChannelInfo struct {
	Name string `json:"name"`
	// Type 保持为 DeviceType 的数字字符串（历史字段，别改语义）。
	Type string `json:"type"`
	// Direction 是方向的可读名：in（只进）/ out（只出）/ io（双向）。
	Direction string `json:"direction"`
	Ready     bool   `json:"ready"`
	// 以下三项供通道拓扑展示：此前 collectKernelStatus 只透传了 Name/Type，
	// 把 Description/Tools/OutputCaps 全丢了，前端只能画出一排光秃秃的名字。
	Description string   `json:"description,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	OutputCaps  int      `json:"output_caps"`
	CapsText    string   `json:"caps_text,omitempty"`
}

// InputChannelInfo 是一条 inputch 的登记与归属视图。
//
// 字段取自 agent/io 的 ChannelRegistry（根 agent 与驻留子**共用同一份**登记表），
// 因此它天然覆盖驻留子的通道分配。
type InputChannelInfo struct {
	Name string `json:"name"`
	// Plugin 是注册它的插件名（归属可追溯）。
	Plugin string `json:"plugin,omitempty"`
	// Owner 是被划给的 agent id；"" = 未分配，归根 agent / 内核默认。
	Owner string `json:"owner,omitempty"`
	// Capacity 是该 inputch 的队列容量；0 = 用内核默认值。
	Capacity int `json:"capacity,omitempty"`
	// Output 是该 inputch 的默认回程输出通道；"" = 由来源决定。
	Output string `json:"output,omitempty"`
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
