package sdk

import (
	"sync"

	"gitcode.com/JianFeeeee/homeagent-sdk/meta"
)

// SDKVersion 是对外暴露的 SDK 版本号。
var SDKVersion = meta.Version

// Plugin is the interface every plugin must implement.
type Plugin interface {
	Name() string
	Start(sdk *PluginSDK) error
	Stop() error
}

// ToolHandler is a function that handles a tool call.
type ToolHandler func(args map[string]interface{}) (interface{}, error)

// StageHandler is a function that handles a pipeline stage event.
type StageHandler func(ctx *StageContext) error

// Stage represents a point in the message processing pipeline.
type Stage string

const (
	StageOnInput        Stage = "on_input"
	StagePreAction      Stage = "pre_action"
	StagePostAction     Stage = "post_action"
	StageBeforeToolcall Stage = "before_toolcall"
	StageAfterToolcall  Stage = "after_toolcall"
	StageBeforeOutput   Stage = "before_output"
	StageAfterOutput    Stage = "after_output"
)

// 上下文策略：决定一次工具调用/输入/注入是否依据其内容裁剪上下文。
//
// 默认（空串或 ContextPolicyNone）**不裁剪**：裁剪会归档丢弃低相关事件，
// 必须由工具/通道/注入点显式声明才发生——否则一个只想往上下文里塞内容的
// 插件会在背后把别人的内容挤掉，且看不出是谁干的。
const (
	ContextPolicyNone  = "none"
	ContextPolicyPrune = "prune"
)

// ValidContextPolicy 校验策略取值；空串等价于 ContextPolicyNone。
func ValidContextPolicy(policy string) bool {
	switch policy {
	case "", ContextPolicyNone, ContextPolicyPrune:
		return true
	}
	return false
}

// 召回策略：决定一次工具调用/输入/注入是否据其内容**召回**（注入）相关记忆。
//
// 与 ContextPolicy **正交**：ContextPolicy 管「裁剪」（把低相关 L0 事件归档），
// RecallPolicy 管「召回」（把 L2/L3 的相关记忆注入本轮）。两者默认值刻意相反——
// 裁剪是破坏性的，默认关（必须显式声明）；召回是只读增量、日常对话本就需要，
// 默认 auto（输入/注入），仅**工具**默认 none（工具输出多为噪声，按需声明）。
const (
	RecallPolicyNone = "none"
	RecallPolicyAuto = "auto"
)

// ValidRecallPolicy 校验召回策略取值；空串按调用面取默认值。
func ValidRecallPolicy(policy string) bool {
	switch policy {
	case "", RecallPolicyNone, RecallPolicyAuto:
		return true
	}
	return false
}

// InjectOptions 声明一次注入行为在记忆层与上下文层的表现。
//
// 零值 = 记入记忆 + 不裁剪上下文，与历史行为（三参数注入方法）完全一致，
// 因此调用方只有在确实需要改变行为时才需要填它。
//
// 为什么注入也要这两个标志：注入的内容来源千差万别——轮询到的频道消息
// 属于真实对话（该记），而“任务还在跑”“连接已重连”这类提醒不该污染记忆，
// 也不该把上下文按它的内容裁一遍。按调用点声明比按通道一刀切准确。
//
// NoMemory:       此次注入不参与记忆计算（向量化/关键词提取/蒸馏），原文仍留在上下文
// ContextPolicy:  此次注入后是否依据（清洗后的）内容裁剪上下文；默认不裁剪。
// RecallPolicy:   此次注入是否依据（清洗后的）内容召回相关记忆；默认 auto（召回）。
//
//	中断注入也允许声明 prune——它同样会携带内容进入上下文。
//
// CleanerName:    此次注入的内容用哪个**已注册的通道 cleaner** 清洗。
//
//	空串 = 按注入的 source 查通道定义（既有行为）。
//	为什么要能显式指定：注入的 source 未必是注册过的输入通道名，
//	而注入内容往往带 ANSI/JSON 包装，需要清洗后才是有效内容；
//	不指定就只能退到「按 source 查不到就不清洗」。
type InjectOptions struct {
	NoMemory      bool
	ContextPolicy string
	// RecallPolicy 声明此次注入是否据其内容召回相关记忆。
	// 空串 = 默认（输入/注入 auto，即保持既有「每条输入都召回」的行为）；
	// RecallPolicyNone 显式关闭（如中断通知的 meta 文本不该据它召回）。
	RecallPolicy string
	CleanerName  string

	// Priority 声明**中断注入**的优先级（仅 InjectInterrupt* 有意义）。
	//
	// 取值 PriorityL1..PriorityL4；空等同 L1（默认级）。
	// L4 只有**内核级插件**能用（见 PriorityL4 注释）；外部插件的 L4 会被夹到 L3。
	//
	// 排队注入（InjectText*/InjectInputSync）没有级别：它们本就是“不需及时处理”
	// 的那一类，可被任何中断打断。
	Priority string
}

// 中断优先级取值。
//
// L1..L3 任何插件都可声明；**L4 只有内核级插件**（编译期内置插件，
// 如 cli/webui/timer）才能声明——它用于实现真正的“立即打断”能力，
// 例如 WebUI 的终止按钮。外部插件（走 proc 桥）声明 L4 会被内核夹到 L3。
const (
	PriorityL1 = "L1"
	PriorityL2 = "L2"
	PriorityL3 = "L3"
	// PriorityL4 仅内核级（内置）插件可用；外部插件声明会被夹到 L3。
	PriorityL4 = "L4"
)

// ChannelDef 描述通道在记忆计算层的行为，与 ToolDef.NoMemory/Cleaner 语义一致。
// NoMemory: 此通道输入/输出不参与记忆计算（向量化/关键词提取/蒸馏），但原文保留在上下文中
// Cleaner:  计算层过滤函数，不改原文；仅在向量化/jieba/蒸馏/存档提取关键词时调用
// ContextPolicy: 此通道的输入到达后是否据此裁剪上下文，默认 none（不裁剪）
// RecallPolicy:  此通道的输入到达后是否据此召回相关记忆，默认 auto（召回）
//
// JSON tag 是必需的：通道定义要跨进程传给内核，而 Cleaner 是函数（必须忽略）。
// 没有 tag 时既无法整体 marshal（func 不支持），又会诱使调用方手写字段白名单——
// 那样新增字段会被静默丢掉。
type ChannelDef struct {
	NoMemory      bool                `json:"no_memory,omitempty"`
	Cleaner       func(string) string `json:"-"`
	ContextPolicy string              `json:"context_policy,omitempty"`
	// RecallPolicy 见 InjectOptions.RecallPolicy；空串等价 auto（保持既有行为）。
	RecallPolicy string `json:"recall_policy,omitempty"`
}

// StageContext provides context for stage handlers.
type StageContext struct {
	mu               sync.RWMutex
	RawMessage       string
	UserID           string
	GroupID          string
	ContextMsgs      []map[string]interface{}
	LLMText          string
	ReasoningContent string
	TokenUsage       map[string]int
	ToolCalls        []ToolCall
	ToolResults      []ToolResult
	FinalText        string
	Response         *string
	Phase            Stage
	Memory           []MemItem
	NoMemory         bool
	Extra            map[string]interface{}
	Errors           []string // 阶段处理过程中的错误信息
}

func (c *StageContext) RLock()   { c.mu.RLock() }
func (c *StageContext) RUnlock() { c.mu.RUnlock() }
func (c *StageContext) Lock()    { c.mu.Lock() }
func (c *StageContext) Unlock()  { c.mu.Unlock() }
func (c *StageContext) IsResponded() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Response != nil
}

// MemItem represents a memory item in stage context.
type MemItem struct {
	Role    string  `json:"role"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// ToolCall represents a model's request to call a tool.
type ToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Plugin    string                 `json:"plugin,omitempty"`
	Arguments map[string]interface{} `json:"arguments"`
}

// ToolResult represents the result of a tool call.
type ToolResult struct {
	CallID  string      `json:"call_id"`
	Name    string      `json:"name"`
	Plugin  string      `json:"plugin,omitempty"`
	Success bool        `json:"success"`
	Result  interface{} `json:"result"`
}

// ToolDef describes a tool that the plugin exposes.
type ToolDef struct {
	Name          string                 `json:"name"`
	Plugin        string                 `json:"plugin,omitempty"`
	Description   string                 `json:"description"`
	Parameters    map[string]interface{} `json:"parameters"`
	NoMemory      bool                   `json:"no_memory,omitempty"`      // 此工具输出不参与记忆计算，但原文保留
	Cleaner       func(string) string    `json:"-"`                        // 计算层过滤函数，不改原文；仅在向量化/jieba/蒸馏时调用
	ContextPolicy string                 `json:"context_policy,omitempty"` // 上下文策略：""(默认，不裁剪) / ContextPolicyNone / ContextPolicyPrune
	// RecallPolicy 声明此工具输出是否触发一次记忆召回（注入）。
	// ""(默认 none) / RecallPolicyNone / RecallPolicyAuto。
	// 默认 none：多数工具输出是噪声；需要「取回真实内容后据它召回」的工具（如 qq_get_message）应显式声明 auto。
	RecallPolicy string `json:"recall_policy,omitempty"`
}

// IOInjector provides methods for injecting input and interrupts into the agent pipeline.
// All methods accept (source, channel) where channel is the target output channel
// for routing the agent's response.
type IOInjector interface {
	InjectInterruptText(source, channel, text string)
	InjectText(source, channel, text string)
	InjectTextNoMemory(source, channel, text string)
	// InjectInputSync 注入输入事件并同步等待 agent 回复，返回回复文本（无回复时返回空串）。
	// 用于通道消息的完整闭环：收到入站 → agent 处理 → 回复取回 → 送回通道。
	InjectInputSync(source, channel, text string) string
	// SetToolBlocks 插件工具注入多模态内容块（image_url/audio_url），内核在下一条
	// tool message 的 content 数组里带上这些块，让模型在后续轮次看到图/听到音频。
	SetToolBlocks(blocks []ContentBlock)
	InjectInputMedia(source, channel, text string, blocks []ContentBlock)
	InjectInputMediaSync(source, channel, text string, blocks []ContentBlock) string
	InjectInterruptMedia(source, channel, text string, blocks []ContentBlock)

	// 以下 Opts 变体让调用点在**这一次注入**上声明记忆与裁剪行为。
	//
	// 上面那些不带 opts 的方法等价于传零值 InjectOptions（记入记忆 + 不裁剪），
	// 保留它们是为了不破坏已有插件；新代码应当用 Opts 变体把意图写清楚。
	InjectTextOpts(source, channel, text string, opts InjectOptions)
	InjectInterruptTextOpts(source, channel, text string, opts InjectOptions)
	InjectInputSyncOpts(source, channel, text string, opts InjectOptions) string
	InjectInputMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions)
	InjectInputMediaSyncOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions) string
	InjectInterruptMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions)
}

// EventType identifies the kind of system event.
type EventType string

const (
	EventRawInput      EventType = "raw_input"
	EventAgentOutput   EventType = "agent_output"
	EventAgentLLMChain EventType = "agent_llm_chain"
	EventToolCall      EventType = "tool_call"
	EventReasoning     EventType = "reasoning"
	EventStage         EventType = "stage"
	EventSystem        EventType = "system"

	// 流式增量事件（token 级）：核心 process() 流式化后每收到一个增量块发布。
	// 客户端可选订做真逐 token 渲染；聚合事件仍照常发布，旧订阅者不受影响。
	EventReasoningDelta EventType = "reasoning_delta"
	EventContentDelta   EventType = "content_delta"
)

// Event represents a system event published by the kernel.
type Event struct {
	Type      EventType              `json:"type"`
	Source    string                 `json:"source"`
	Payload   map[string]interface{} `json:"payload"`
	Timestamp int64                  `json:"timestamp"`
}

// EventHandler processes a system event.
type EventHandler func(evt *Event)

// EventSubscriber allows plugins to subscribe to kernel events.
// This is a restricted interface: plugins can subscribe but the kernel
// controls which events are delivered.
type EventSubscriber interface {
	Subscribe(eventType EventType, handler EventHandler) func()
}

// PluginMgrAPI 提供插件管理能力（外部插件可调用）。
// 由 bridge 注入 dispatch 实现，走 C ABI CORE_PLUGIN_RELOAD_ONE 等。
type PluginMgrAPI interface {
	// ReloadOne 重载单个插件（停止后重新加载）。
	ReloadOne(name string) error
	// ListLoadedPlugins 列出已加载插件。
	ListLoadedPlugins() []string
	// IsPluginDisabled 查询插件是否被禁用。
	IsPluginDisabled(name string) bool
}

// StageScope controls which events a stage handler receives.
type StageScope int

const (
	// StageScopeGlobal receives all stage events (default).
	StageScopeGlobal StageScope = 0
	// StageScopeOwnTools only receives events for this plugin's own tool calls
	// (before_toolcall / after_toolcall only). Other stages degrade to global.
	StageScopeOwnTools StageScope = 1
)

// ToolRegistrar registers a tool dynamically.
type ToolRegistrar func(name string, def ToolDef, handler ToolHandler) error

// StageRegistrar registers a stage handler.
type StageRegistrar func(stage Stage, handler StageHandler)

// APIRegistrar registers a plugin API for external access.
type APIRegistrar func(name string) error

// InputChannelRegistrar registers an input channel with its memory behavior.
type InputChannelRegistrar func(name string, def ChannelDef) error

// OutputChannelRegistrar registers an output channel that the output_send tool can use.
type OutputChannelRegistrar func(name string, caps int, desc string, def ChannelDef, handler ToolHandler) error

// OutputChannelUnregistrar 注销一个输出通道。
//
// 为什么需要它：输出通道不止有"启动时注册一次"的静态通道，还有**随外部资源生灭**的
// 动态通道 —— 典型是远程设备：`device/<id>` 只在设备在线期间存在，设备掉线后
// 必须注销，否则 output_list_channels 会一直列着它、模型会往一个死通道发消息。
type OutputChannelUnregistrar func(name string) error

// Output capability flags
const (
	CapText       = 1
	CapFile       = 2
	CapImage      = 4
	CapAudio      = 8
	CapStructured = 16
)

// PluginSDK is the main API surface provided to plugins at runtime.
// It wraps tool registration, settings, memory, knowledge, LLM, and IO injection.
type PluginSDK struct {
	name           string
	regTool        ToolRegistrar
	regStage       StageRegistrar
	regAPI         APIRegistrar
	regOutput      OutputChannelRegistrar
	regOutputUnreg OutputChannelUnregistrar
	regInput       InputChannelRegistrar
	io             IOInjector
	mem            MemoryAPI
	textMem        TextMemoryAPI
	docMem         DocMemoryAPI
	know           KnowledgeAPI
	llm            LLMAPI
	sett           SettingsAPI
	social         SocialAPI
	events         EventSubscriber
	plgMgr         PluginMgrAPI

	// apiMu 保护上面这些由内核注入的 API 字段，以及 autoRestart。
	//
	// 这些字段的写方与读方天然跨 goroutine：
	//   - 写方是内核（加载/重载插件时注入 API）与插件自己（SetAutoRestart）；
	//   - 读方是插件在 Start() 里起的后台 goroutine（轮询、监听、定时器
	//     都要拿 injector 往管道里注消息），以及内核 registry —— 它在
	//     另一个 goroutine 读 AutoRestart() 决定崩溃后是否重启。
	// SetAutoRestart 的文档用法本身就是「连接建立后再决定能否自动重启」，
	// 而连接建立通常发生在后台 goroutine 里，于是这对读写必然并发。
	//
	// sdk/stress_test.go 的 -race 实测确认这是真竞态，不是理论风险。
	// 未加锁时的生产表现是偶发 nil 解引用崩溃（读到半个接口值）。
	//
	// 约定：只在持锁期间取字段值，取完立刻释放再调用。
	// 持锁调用会把 InjectInputSync 这类阻塞到 agent 回复（可达数分钟）的
	// 方法与 SetIOInjector 串到一起，让插件重载卡死。
	apiMu sync.RWMutex

	autoRestart bool

	stopMu       sync.Mutex
	stopHandlers []func()

	removeMu       sync.Mutex
	removeHandlers []func()
}

// New creates a PluginSDK with the given dependencies.
func New(name string, sett SettingsAPI, regTool ToolRegistrar, regStage StageRegistrar, regAPI APIRegistrar, regOutput OutputChannelRegistrar) *PluginSDK {
	return &PluginSDK{
		name:        name,
		sett:        sett,
		regTool:     regTool,
		regStage:    regStage,
		regAPI:      regAPI,
		regOutput:   regOutput,
		autoRestart: true,
	}
}

// PluginName returns the name of the plugin.
func (s *PluginSDK) PluginName() string { return s.name }

// Settings returns the settings API for reading/writing plugin configuration.
// sett 在 New 时一次性写入且无 setter，故不需要加锁。
func (s *PluginSDK) Settings() SettingsAPI { return s.sett }

// Memory returns the graph memory API (may be nil if not available).
func (s *PluginSDK) Memory() MemoryAPI {
	s.apiMu.RLock()
	defer s.apiMu.RUnlock()
	return s.mem
}

// TextMemory returns the text memory API (may be nil if not available).
func (s *PluginSDK) TextMemory() TextMemoryAPI {
	s.apiMu.RLock()
	defer s.apiMu.RUnlock()
	return s.textMem
}

// DocMemory returns the document memory API (may be nil if not available).
func (s *PluginSDK) DocMemory() DocMemoryAPI {
	s.apiMu.RLock()
	defer s.apiMu.RUnlock()
	return s.docMem
}

// Knowledge returns the knowledge store API (may be nil if not available).
func (s *PluginSDK) Knowledge() KnowledgeAPI {
	s.apiMu.RLock()
	defer s.apiMu.RUnlock()
	return s.know
}

// LLM returns the LLM provider API (may be nil if not available).
func (s *PluginSDK) LLM() LLMAPI {
	s.apiMu.RLock()
	defer s.apiMu.RUnlock()
	return s.llm
}

// Social returns the social graph API (may be nil if not available).
func (s *PluginSDK) Social() SocialAPI {
	s.apiMu.RLock()
	defer s.apiMu.RUnlock()
	return s.social
}

// Events returns the event subscriber for listening to kernel events (may be nil if not available).
func (s *PluginSDK) Events() EventSubscriber {
	s.apiMu.RLock()
	defer s.apiMu.RUnlock()
	return s.events
}

// RegisterTool registers a tool that the LLM can call.
func (s *PluginSDK) RegisterTool(name string, def ToolDef, handler ToolHandler) error {
	if def.Plugin == "" {
		def.Plugin = s.name
	}
	if s.regTool != nil {
		return s.regTool(name, def, handler)
	}
	return nil
}

// RegisterStage registers a handler for a pipeline stage.
//
//	scope: StageScopeGlobal (default) — receives all stage events.
//	       StageScopeOwnTools — only before_toolcall/after_toolcall for this plugin's tools.
func (s *PluginSDK) RegisterStage(stage Stage, handler StageHandler, scope ...StageScope) {
	if s.regStage == nil {
		return
	}
	sc := StageScopeGlobal
	if len(scope) > 0 {
		sc = scope[0]
	}
	if sc == StageScopeGlobal {
		s.regStage(stage, handler)
		return
	}
	// OwnTools scope — only for before_toolcall / after_toolcall
	if stage != StageBeforeToolcall && stage != StageAfterToolcall {
		s.regStage(stage, handler)
		return
	}
	s.regStage(stage, func(ctx *StageContext) error {
		ctx.RLock()
		match := false
		switch stage {
		case StageBeforeToolcall:
			match = len(ctx.ToolCalls) > 0 && ctx.ToolCalls[0].Plugin == s.name
		case StageAfterToolcall:
			match = len(ctx.ToolResults) > 0 && ctx.ToolResults[0].Plugin == s.name
		}
		ctx.RUnlock()
		if !match {
			return nil
		}
		return handler(ctx)
	})
}

// RegisterPluginAPI registers this plugin's API for access by other plugins.
func (s *PluginSDK) RegisterPluginAPI(name string) error {
	if s.regAPI != nil {
		return s.regAPI(name)
	}
	return nil
}

// RegisterOutputChannel registers an output channel that the output_send tool can route to.
//
// 与 RegisterInputChannel 的分工：本函数声明**出站**（output_send__<name> 的回复发给谁）；
// 入站（谁会往 <name> 注入输入）是另一件事，用 RegisterInputChannel 声明。
// 若该通道同时也是你的注入入口，两个都要登记。
//
// name: channel name (e.g. "qq", "webui")。
//
// ❗**命名约束**：内核会把通道名拼进 LLM 的函数名（`output_send__<name>`），
// 而上游对函数名的规范是 `^[a-zA-Z0-9_-]{1,64}$`。违反的后果不是"这个工具不可用"，
// 而是**整条请求被上游 400 拒绝**（`Invalid 'tools[N].function.name'`），
// 网关的 auto tier 会全链条失败 —— 表现成"整个 agent 不说话了"。
// 所以通道名只能用 `[A-Za-z0-9_-]`，且总长要留出 `output_send__`（13 字符）的余量。
// 若通道名来自外部输入（设备自报 id 之类），请**在插件侧派生一个合规且唯一的名字**，
// 而不是把原始值直接当通道名。
// caps: bitmask of supported output capabilities (CapText, CapFile, etc.)
// desc: description of the channel, expected meta format, and type enum
// def:  通道在记忆计算层的行为（NoMemory/Cleaner）
// handler: receives args map with keys: payload (string), type (string), meta (string|optional)
func (s *PluginSDK) RegisterOutputChannel(name string, caps int, desc string, def ChannelDef, handler ToolHandler) error {
	s.apiMu.RLock()
	reg := s.regOutput
	s.apiMu.RUnlock()
	if reg != nil {
		return reg(name, caps, desc, def, handler)
	}
	return nil
}

// UnregisterOutputChannel 注销一个输出通道（动态通道随资源生灭时必须调用）。
func (s *PluginSDK) UnregisterOutputChannel(name string) error {
	s.apiMu.RLock()
	reg := s.regOutputUnreg
	s.apiMu.RUnlock()
	if reg != nil {
		return reg(name)
	}
	return nil
}

// RegisterInputChannel registers an input channel with its memory behavior.
//
// 契约：**凡是用 InjectText*/InjectInput*/InjectInterrupt*(source, "<name>", ...)
// 注入的通道名，都应当在这里登记**。inputch 是内核里最基本的**输入路由单位**：
// 只有登记过的通道才能在 inputch 登记表里出现，父 agent 才能"把某个 inputch 划给驻留子"；
// 没登记就划分会直接失败（`inputch 未注册`）。
//
// 只登记输出通道（RegisterOutputChannel）而没登记输入通道时，内核会兜底登记同名
// inputch 并打告警日志 —— 兜底只为兼容老插件，新插件请显式登记。
//
// def.NoMemory: 此通道输入不参与记忆计算
// def.Cleaner:  计算层对输入文本清洗后（不改原文）再向量化/提关键词
func (s *PluginSDK) RegisterInputChannel(name string, def ChannelDef) error {
	s.apiMu.RLock()
	reg := s.regInput
	s.apiMu.RUnlock()
	if reg != nil {
		return reg(name, def)
	}
	return nil
}

// 以下 setter 由内核在启动/重载时调用，与插件后台 goroutine 的读并发，故加锁。

// SetOutputChannelRegistrar sets the output channel registrar (called by the core at startup).
func (s *PluginSDK) SetOutputChannelRegistrar(r OutputChannelRegistrar) {
	s.apiMu.Lock()
	s.regOutput = r
	s.apiMu.Unlock()
}

// SetOutputChannelUnregistrar sets the output channel unregistrar (called by the core at startup).
func (s *PluginSDK) SetOutputChannelUnregistrar(r OutputChannelUnregistrar) {
	s.apiMu.Lock()
	s.regOutputUnreg = r
	s.apiMu.Unlock()
}

// SetInputChannelRegistrar sets the input channel registrar (called by the core at startup).
func (s *PluginSDK) SetInputChannelRegistrar(r InputChannelRegistrar) {
	s.apiMu.Lock()
	s.regInput = r
	s.apiMu.Unlock()
}

// SetIOInjector sets the IO injector (called by the core at startup).
func (s *PluginSDK) SetIOInjector(io IOInjector) {
	s.apiMu.Lock()
	s.io = io
	s.apiMu.Unlock()
}

// SetMemoryAPI sets the memory API (called by the core at startup).
func (s *PluginSDK) SetMemoryAPI(mem MemoryAPI) {
	s.apiMu.Lock()
	s.mem = mem
	s.apiMu.Unlock()
}

func (s *PluginSDK) SetTextMemoryAPI(tm TextMemoryAPI) {
	s.apiMu.Lock()
	s.textMem = tm
	s.apiMu.Unlock()
}

func (s *PluginSDK) SetDocMemoryAPI(dm DocMemoryAPI) {
	s.apiMu.Lock()
	s.docMem = dm
	s.apiMu.Unlock()
}

func (s *PluginSDK) SetKnowledgeAPI(kn KnowledgeAPI) {
	s.apiMu.Lock()
	s.know = kn
	s.apiMu.Unlock()
}

func (s *PluginSDK) SetLLMAPI(llm LLMAPI) {
	s.apiMu.Lock()
	s.llm = llm
	s.apiMu.Unlock()
}

func (s *PluginSDK) SetSocialAPI(social SocialAPI) {
	s.apiMu.Lock()
	s.social = social
	s.apiMu.Unlock()
}

func (s *PluginSDK) SetEventSubscriber(es EventSubscriber) {
	s.apiMu.Lock()
	s.events = es
	s.apiMu.Unlock()
}

// SetPluginMgrAPI sets the plugin manager API (called by the bridge at startup).
func (s *PluginSDK) SetPluginMgrAPI(pm PluginMgrAPI) {
	s.apiMu.Lock()
	s.plgMgr = pm
	s.apiMu.Unlock()
}

// PluginMgr returns the plugin manager API (ReloadOne / ReloadPlugins / list).
// May be nil if the host did not wire it.
func (s *PluginSDK) PluginMgr() PluginMgrAPI {
	s.apiMu.RLock()
	defer s.apiMu.RUnlock()
	return s.plgMgr
}

// ---- IO Convenience Methods ----

// injector 取当前 injector 的快照。
//
// 取完即释放锁再调用：InjectInputSync 会阻塞到 agent 回复（可达数分钟），
// 若持锁调用，插件重载时的 SetIOInjector 会一起卡住。
func (s *PluginSDK) injector() IOInjector {
	s.apiMu.RLock()
	defer s.apiMu.RUnlock()
	return s.io
}

// InjectInterruptText injects a text interrupt that can preempt current LLM processing.
// 等价于 InjectInterruptTextOpts(..., InjectOptions{})：记入记忆、不裁剪。
func (s *PluginSDK) InjectInterruptText(source, channel, text string) {
	s.InjectInterruptTextOpts(source, channel, text, InjectOptions{})
}

// InjectText injects a text message into the agent pipeline.
// 等价于 InjectTextOpts(..., InjectOptions{})：记入记忆、不裁剪。
func (s *PluginSDK) InjectText(source, channel, text string) {
	s.InjectTextOpts(source, channel, text, InjectOptions{})
}

// InjectTextNoMemory injects a text message without generating memory.
// 等价于 InjectTextOpts(..., InjectOptions{NoMemory: true})。
func (s *PluginSDK) InjectTextNoMemory(source, channel, text string) {
	s.InjectTextOpts(source, channel, text, InjectOptions{NoMemory: true})
}

// InjectInputSync injects a text message and synchronously waits for the agent reply,
// returning the reply text (empty string if none). Replies must be dispatched back
// to the source channel by the caller.
func (s *PluginSDK) InjectInputSync(source, channel, text string) string {
	return s.InjectInputSyncOpts(source, channel, text, InjectOptions{})
}

// InjectInputMedia 注入带媒体内容块（image_url/audio_url）的输入。
// blocks 会落进媒体存储被记忆引用捕获，同时作为当前轮 content 数组
// 发给 LLM，让模型在「本轮」就看到图/听到音频——区别于 SetToolBlocks
// 的「下一轮 tool message」语义。
// 等价于 InjectInputMediaOpts(..., InjectOptions{})。
func (s *PluginSDK) InjectInputMedia(source, channel, text string, blocks []ContentBlock) {
	s.InjectInputMediaOpts(source, channel, text, blocks, InjectOptions{})
}

// InjectInputMediaSync 注入带媒体内容块的输入并同步等待 agent 回复。
// 等价于 InjectInputMediaSyncOpts(..., InjectOptions{})。
func (s *PluginSDK) InjectInputMediaSync(source, channel, text string, blocks []ContentBlock) string {
	return s.InjectInputMediaSyncOpts(source, channel, text, blocks, InjectOptions{})
}

// ---- 带 InjectOptions 的注入（声明记忆/裁剪行为）----

// InjectTextOpts 注入文本到 agent，并在这一次注入上声明记忆与裁剪行为。
func (s *PluginSDK) InjectTextOpts(source, channel, text string, opts InjectOptions) {
	if io := s.injector(); io != nil {
		io.InjectTextOpts(source, channel, text, opts)
	}
}

// InjectInterruptTextOpts 注入可抢占当前处理的中断文本。
//
// 中断也允许声明 ContextPolicyPrune：中断同样携带内容进入上下文，
// 是否需要据此裁剪由调用方决定（默认不裁剪）。
func (s *PluginSDK) InjectInterruptTextOpts(source, channel, text string, opts InjectOptions) {
	if io := s.injector(); io != nil {
		io.InjectInterruptTextOpts(source, channel, text, opts)
	}
}

// InjectInputSyncOpts 注入输入并同步等待回复，同时在这次注入上声明记忆/裁剪行为。
func (s *PluginSDK) InjectInputSyncOpts(source, channel, text string, opts InjectOptions) string {
	io := s.injector()
	if io == nil {
		return ""
	}
	return io.InjectInputSyncOpts(source, channel, text, opts)
}

// InjectInputMediaOpts 注入带媒体块的输入，并声明记忆/裁剪行为。
func (s *PluginSDK) InjectInputMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions) {
	if io := s.injector(); io != nil {
		io.InjectInputMediaOpts(source, channel, text, blocks, opts)
	}
}

// InjectInputMediaSyncOpts 注入带媒体块的输入并同步等待回复，同时声明记忆/裁剪行为。
func (s *PluginSDK) InjectInputMediaSyncOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions) string {
	io := s.injector()
	if io == nil {
		return ""
	}
	return io.InjectInputMediaSyncOpts(source, channel, text, blocks, opts)
}

// InjectInterruptMediaOpts 注入带媒体块的中断，并声明记忆/裁剪行为。
func (s *PluginSDK) InjectInterruptMediaOpts(source, channel, text string, blocks []ContentBlock, opts InjectOptions) {
	if io := s.injector(); io != nil {
		io.InjectInterruptMediaOpts(source, channel, text, blocks, opts)
	}
}

// InjectInterruptMedia 注入带媒体内容块的中断，可抢占当前 LLM 处理。
// blocks 随中断消息一起发给模型。
func (s *PluginSDK) InjectInterruptMedia(source, channel, text string, blocks []ContentBlock) {
	if io := s.injector(); io != nil {
		io.InjectInterruptMedia(source, channel, text, blocks)
	}
}

// SetToolBlocks 在工具处理函数内注入多模态内容块，内核在下一条 tool message
// 的 content 数组里带上它们。需要「本轮就让模型看到」时用 InjectInputMedia。
func (s *PluginSDK) SetToolBlocks(blocks []ContentBlock) {
	if io := s.injector(); io != nil {
		io.SetToolBlocks(blocks)
	}
}

// SetAutoRestart 设置插件崩溃后内核是否自动重启它。
// 默认 true。如果插件有无法恢复的状态（如外部连接），应设为 false。
//
// 重启是有限度的：线性退避（第 n 次等 n×1s，即 1s→2s→3s），
// 且同一 5 分钟窗口内第 4 次崩溃就停下不再拉起（详见 README）。
// 注意这与「重载」（换 plugin.bin 后重新加载）是两回事。
func (s *PluginSDK) SetAutoRestart(enabled bool) {
	s.apiMu.Lock()
	s.autoRestart = enabled
	s.apiMu.Unlock()
}

// AutoRestart 返回插件是否允许自动重启。
func (s *PluginSDK) AutoRestart() bool {
	s.apiMu.RLock()
	defer s.apiMu.RUnlock()
	return s.autoRestart
}

// RegisterStopHandler 注册插件停止阶段的清理回调。
// 注册的 handler 会在插件 Stop() 之前按"后注册先执行"的顺序调用，
// 适用于释放资源、落盘状态、关闭子进程等停止时清理操作。
// 可注册多个；执行后清空（进程停止前只执行一次）。
func (s *PluginSDK) RegisterStopHandler(fn func()) {
	if fn == nil {
		return
	}
	s.stopMu.Lock()
	s.stopHandlers = append(s.stopHandlers, fn)
	s.stopMu.Unlock()
}

// RunStopHandlers 执行全部已注册的 stop handler（后注册先执行，执行后清空，幂等）。
// 由内核（内置插件）或插件桥接层（外部插件 z_bridge 的 StopPlugin）在调用插件 Stop() 前执行。
func (s *PluginSDK) RunStopHandlers() {
	s.stopMu.Lock()
	handlers := append([]func(){}, s.stopHandlers...)
	s.stopHandlers = nil
	s.stopMu.Unlock()
	for i := len(handlers) - 1; i >= 0; i-- {
		handlers[i]()
	}
}

// RegisterOnRemoveHandler 注册插件被删除（卸载）时的清理回调。
// 注册的 handler 会在插件目录被移除前按"后注册先执行"的顺序调用，
// 适用于清理外部资源、删除配置表、下线状态等删除后处理。
// 可注册多个；执行后清空（一次删除只执行一次）。
func (s *PluginSDK) RegisterOnRemoveHandler(fn func()) {
	if fn == nil {
		return
	}
	s.removeMu.Lock()
	s.removeHandlers = append(s.removeHandlers, fn)
	s.removeMu.Unlock()
}

// RunOnRemoveHandlers 执行全部已注册的 onRemove handler（后注册先执行，执行后清空，幂等）。
// 由内核在卸载插件（registry.RemovePlugin）时、插件 Stop() 之后执行。
func (s *PluginSDK) RunOnRemoveHandlers() {
	s.removeMu.Lock()
	handlers := append([]func(){}, s.removeHandlers...)
	s.removeHandlers = nil
	s.removeMu.Unlock()
	for i := len(handlers) - 1; i >= 0; i-- {
		handlers[i]()
	}
}

// ContentBlock 是多模态内容块（OpenAI 格式：text/image_url/audio_url）。
// 插件工具返回结果时可用 PluginSDK.SetToolBlocks 注入，让下一轮 LLM
// 请求在 tool message 的 content 数组里带上图片/音频，实现"模型看图/听音频"。
type ContentBlock struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
	AudioURL *AudioURL `json:"audio_url,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type AudioURL struct {
	URL string `json:"url"`
}
