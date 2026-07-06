package sdk

import "sync"

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
}

// IOInjector provides methods for injecting input and interrupts into the agent pipeline.
type IOInjector interface {
	InjectInterruptText(source, channel, text string)
	InjectText(source, channel, text string)
	InjectTextNoMemory(source, channel, text string)
}

// ToolRegistrar registers a tool dynamically.
type ToolRegistrar func(name string, def ToolDef, handler ToolHandler) error

// StageRegistrar registers a stage handler.
type StageRegistrar func(stage Stage, handler StageHandler)

// APIRegistrar registers a plugin API for external access.
type APIRegistrar func(name string) error

// PluginSDK is the main API surface provided to plugins at runtime.
// It wraps tool registration, settings, memory, knowledge, LLM, and IO injection.
type PluginSDK struct {
	name    string
	regTool  ToolRegistrar
	regStage StageRegistrar
	regAPI   APIRegistrar
	io       IOInjector
	mem      MemoryAPI
	textMem  TextMemoryAPI
	docMem   DocMemoryAPI
	know     KnowledgeAPI
	llm      LLMAPI
	sett     SettingsAPI
}

// New creates a PluginSDK with the given dependencies.
func New(name string, sett SettingsAPI, regTool ToolRegistrar, regStage StageRegistrar, regAPI APIRegistrar) *PluginSDK {
	return &PluginSDK{
		name:     name,
		sett:     sett,
		regTool:  regTool,
		regStage: regStage,
		regAPI:   regAPI,
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
func (s *PluginSDK) RegisterStage(stage Stage, handler StageHandler) {
	if s.regStage != nil {
		s.regStage(stage, handler)
	}
}

// RegisterStageOwnTools only listens to this plugin's own tool calls/results in
// before_toolcall / after_toolcall stages. Other stages degrade to RegisterStage.
func (s *PluginSDK) RegisterStageOwnTools(stage Stage, handler StageHandler) {
	if s.regStage == nil {
		return
	}
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

// SetIOInjector sets the IO injector (called by the core at startup).
func (s *PluginSDK) SetIOInjector(io IOInjector) { s.io = io }

// SetMemoryAPI sets the memory API (called by the core at startup).
func (s *PluginSDK) SetMemoryAPI(mem MemoryAPI)       { s.mem = mem }
func (s *PluginSDK) SetTextMemoryAPI(tm TextMemoryAPI) { s.textMem = tm }
func (s *PluginSDK) SetDocMemoryAPI(dm DocMemoryAPI)   { s.docMem = dm }
func (s *PluginSDK) SetKnowledgeAPI(kn KnowledgeAPI)   { s.know = kn }
func (s *PluginSDK) SetLLMAPI(llm LLMAPI)              { s.llm = llm }

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
