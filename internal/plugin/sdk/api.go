package sdk

import "fmt"

type Stage string

const (
	StageOnInput       Stage = "on_input"
	StagePreAction     Stage = "pre_action"
	StagePostAction    Stage = "post_action"
	StageBeforeToolcall Stage = "before_toolcall"
	StageAfterToolcall  Stage = "after_toolcall"
	StageBeforeOutput  Stage = "before_output"
	StageAfterOutput   Stage = "after_output"
)

type EventType string

const (
	EventRawInput    EventType = "raw_input"
	EventAgentOutput EventType = "agent_output"
	EventToolCall    EventType = "tool_call"
	EventReasoning   EventType = "reasoning"
	EventSystem      EventType = "system"
	EventAll         EventType = "*"
)

type Event struct {
	Type      EventType              `json:"type"`
	Source    string                 `json:"source"`
	Payload   map[string]interface{} `json:"payload"`
	Timestamp int64                  `json:"timestamp"`
}

type MemItem struct {
	Content string  `json:"content"`
	Score   float64 `json:"score"`
	Source  string  `json:"source"`
}

type StageContext struct {
	RawMessage  string
	UserID      string
	GroupID     string
	ContextMsgs []map[string]interface{}
	LLMText     string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
	FinalText   string
	Response    *string
	Phase       Stage
	Memory      []MemItem
	Extra       map[string]interface{}
}

type ToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type ToolResult struct {
	CallID  string      `json:"call_id"`
	Name    string      `json:"name"`
	Success bool        `json:"success"`
	Result  interface{} `json:"result"`
}

type ToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type MemoryAPI interface {
	Recall(query string, topK int) ([]MemItem, error)
	Commit(triples []map[string]string) error
	Introspect() (map[string]interface{}, error)
}

type KnowledgeAPI interface {
	Search(query string, topK int) ([]MemItem, error)
	Create(name, content string) error
	List() ([]string, error)
}

type EventHandler func(event *Event)
type StageHandler func(ctx *StageContext) error
type ToolHandler func(args map[string]interface{}) (interface{}, error)

type PluginAPI struct {
	Name    string
	Version string

	tools    map[string]ToolHandler
	stages   map[Stage][]StageHandler
	events   map[EventType][]EventHandler
	eventBus EventBus
	memAPI   MemoryAPI
	knowAPI  KnowledgeAPI
}

func NewPluginAPI(name, version string, bus EventBus, mem MemoryAPI, know KnowledgeAPI) *PluginAPI {
	return &PluginAPI{
		Name:      name,
		Version:   version,
		tools:     make(map[string]ToolHandler),
		stages:    make(map[Stage][]StageHandler),
		events:    make(map[EventType][]EventHandler),
		eventBus:  bus,
		memAPI:    mem,
		knowAPI:   know,
	}
}

func (p *PluginAPI) RegisterTool(name string, handler ToolHandler) error {
	if _, ok := p.tools[name]; ok {
		return fmt.Errorf("tool %s already registered by plugin %s", name, p.Name)
	}
	p.tools[name] = handler
	if p.eventBus != nil {
		p.eventBus.Publish(&Event{
			Type:    EventSystem,
			Source:  p.Name,
			Payload: map[string]interface{}{"action": "register_tool", "tool": name},
		})
	}
	return nil
}

func (p *PluginAPI) RegisterStage(stage Stage, handler StageHandler) {
	p.stages[stage] = append(p.stages[stage], handler)
}

func (p *PluginAPI) Subscribe(eventType EventType, handler EventHandler) {
	p.events[eventType] = append(p.events[eventType], handler)
	if p.eventBus != nil {
		p.eventBus.Subscribe(eventType, handler)
	}
}

func (p *PluginAPI) Publish(evt *Event) {
	if p.eventBus != nil {
		p.eventBus.Publish(evt)
	}
}

func (p *PluginAPI) Tools() map[string]ToolHandler {
	return p.tools
}

func (p *PluginAPI) StageHandlers(stage Stage) []StageHandler {
	return p.stages[stage]
}

func (p *PluginAPI) Memory() MemoryAPI    { return p.memAPI }
func (p *PluginAPI) Knowledge() KnowledgeAPI { return p.knowAPI }

func AllStages() map[Stage]bool {
	return map[Stage]bool{
		StageOnInput:       true,
		StagePreAction:     true,
		StagePostAction:    true,
		StageBeforeToolcall: true,
		StageAfterToolcall:  true,
		StageBeforeOutput:  true,
		StageAfterOutput:   true,
	}
}
