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

// ChannelDef 描述通道在记忆计算层的行为，与 ToolDef.NoMemory/Cleaner 语义一致。
// NoMemory: 此通道输入/输出不参与记忆计算（向量化/关键词提取/蒸馏），但原文保留在上下文中
// Cleaner:  计算层过滤函数，不改原文；仅在向量化/jieba/蒸馏/存档提取关键词时调用
type ChannelDef struct {
	NoMemory bool
	Cleaner  func(string) string
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
	Errors           []string               // 阶段处理过程中的错误信息
}

func (c *StageContext) RLock()                         { c.mu.RLock() }
func (c *StageContext) RUnlock()                       { c.mu.RUnlock() }
func (c *StageContext) Lock()                          { c.mu.Lock() }
func (c *StageContext) Unlock()                        { c.mu.Unlock() }
func (c *StageContext) IsResponded() bool               { c.mu.RLock(); defer c.mu.RUnlock(); return c.Response != nil }

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
	Name        string                 `json:"name"`
	Plugin      string                 `json:"plugin,omitempty"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
	NoMemory    bool                   `json:"no_memory,omitempty"`  // 此工具输出不参与记忆计算，但原文保留
	Cleaner     func(string) string    `json:"-"`                    // 计算层过滤函数，不改原文；仅在向量化/jieba/蒸馏时调用
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
	name      string
	regTool   ToolRegistrar
	regStage  StageRegistrar
	regAPI    APIRegistrar
	regOutput OutputChannelRegistrar
	regInput  InputChannelRegistrar
	io        IOInjector
	mem       MemoryAPI
	textMem   TextMemoryAPI
	docMem    DocMemoryAPI
	know      KnowledgeAPI
	llm       LLMAPI
	sett      SettingsAPI
	social    SocialAPI
	events    EventSubscriber
	plgMgr    PluginMgrAPI

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
func (s *PluginSDK) Settings() SettingsAPI { return s.sett }

// Memory returns the graph memory API (may be nil if not available).
func (s *PluginSDK) Memory() MemoryAPI { return s.mem }

// TextMemory returns the text memory API (may be nil if not available).
func (s *PluginSDK) TextMemory() TextMemoryAPI { return s.textMem }

// DocMemory returns the document memory API (may be nil if not available).
func (s *PluginSDK) DocMemory() DocMemoryAPI { return s.docMem }

// Knowledge returns the knowledge store API (may be nil if not available).
func (s *PluginSDK) Knowledge() KnowledgeAPI { return s.know }

// LLM returns the LLM provider API (may be nil if not available).
func (s *PluginSDK) LLM() LLMAPI { return s.llm }

// Social returns the social graph API (may be nil if not available).
func (s *PluginSDK) Social() SocialAPI { return s.social }

// Events returns the event subscriber for listening to kernel events (may be nil if not available).
func (s *PluginSDK) Events() EventSubscriber { return s.events }

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
//   scope: StageScopeGlobal (default) — receives all stage events.
//          StageScopeOwnTools — only before_toolcall/after_toolcall for this plugin's tools.
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
// name: channel name (e.g. "qq", "webui")
// caps: bitmask of supported output capabilities (CapText, CapFile, etc.)
// desc: description of the channel, expected meta format, and type enum
// def:  通道在记忆计算层的行为（NoMemory/Cleaner）
// handler: receives args map with keys: payload (string), type (string), meta (string|optional)
func (s *PluginSDK) RegisterOutputChannel(name string, caps int, desc string, def ChannelDef, handler ToolHandler) error {
	if s.regOutput != nil {
		return s.regOutput(name, caps, desc, def, handler)
	}
	return nil
}

// RegisterInputChannel registers an input channel with its memory behavior.
// def.NoMemory: 此通道输入不参与记忆计算
// def.Cleaner:  计算层对输入文本清洗后（不改原文）再向量化/提关键词
func (s *PluginSDK) RegisterInputChannel(name string, def ChannelDef) error {
	if s.regInput != nil {
		return s.regInput(name, def)
	}
	return nil
}

// SetOutputChannelRegistrar sets the output channel registrar (called by the core at startup).
func (s *PluginSDK) SetOutputChannelRegistrar(r OutputChannelRegistrar) { s.regOutput = r }

// SetInputChannelRegistrar sets the input channel registrar (called by the core at startup).
func (s *PluginSDK) SetInputChannelRegistrar(r InputChannelRegistrar) { s.regInput = r }

// SetIOInjector sets the IO injector (called by the core at startup).
func (s *PluginSDK) SetIOInjector(io IOInjector) { s.io = io }

// SetMemoryAPI sets the memory API (called by the core at startup).
func (s *PluginSDK) SetMemoryAPI(mem MemoryAPI)       { s.mem = mem }
func (s *PluginSDK) SetTextMemoryAPI(tm TextMemoryAPI) { s.textMem = tm }
func (s *PluginSDK) SetDocMemoryAPI(dm DocMemoryAPI)   { s.docMem = dm }
func (s *PluginSDK) SetKnowledgeAPI(kn KnowledgeAPI)   { s.know = kn }
func (s *PluginSDK) SetLLMAPI(llm LLMAPI)              { s.llm = llm }
func (s *PluginSDK) SetSocialAPI(social SocialAPI)      { s.social = social }
func (s *PluginSDK) SetEventSubscriber(es EventSubscriber) { s.events = es }

// SetPluginMgrAPI sets the plugin manager API (called by the bridge at startup).
func (s *PluginSDK) SetPluginMgrAPI(pm PluginMgrAPI) { s.plgMgr = pm }

// PluginMgr returns the plugin manager API (ReloadOne / ReloadPlugins / list).
// May be nil if the host did not wire it.
func (s *PluginSDK) PluginMgr() PluginMgrAPI { return s.plgMgr }

// ---- IO Convenience Methods ----

// InjectInterruptText injects a text interrupt that can preempt current LLM processing.
func (s *PluginSDK) InjectInterruptText(source, channel, text string) {
	if s.io != nil {
		s.io.InjectInterruptText(source, channel, text)
	}
}

// InjectText injects a text message into the agent pipeline.
func (s *PluginSDK) InjectText(source, channel, text string) {
	if s.io != nil {
		s.io.InjectText(source, channel, text)
	}
}

// InjectTextNoMemory injects a text message without generating memory.
func (s *PluginSDK) InjectTextNoMemory(source, channel, text string) {
	if s.io != nil {
		s.io.InjectTextNoMemory(source, channel, text)
	}
}

// InjectInputSync injects a text message and synchronously waits for the agent reply,
// returning the reply text (empty string if none). Replies must be dispatched back
// to the source channel by the caller.
func (s *PluginSDK) InjectInputSync(source, channel, text string) string {
	if s.io == nil {
		return ""
	}
	return s.io.InjectInputSync(source, channel, text)
}

// SetAutoRestart 设置插件是否允许内核自动重启（崩溃后自动重载）。
// 默认 true。如果插件有无法恢复的状态（如外部连接），应设为 false。
func (s *PluginSDK) SetAutoRestart(enabled bool) { s.autoRestart = enabled }

// AutoRestart 返回插件是否允许自动重启。
func (s *PluginSDK) AutoRestart() bool { return s.autoRestart }

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
