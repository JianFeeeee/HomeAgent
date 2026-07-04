package sdk

import (
	"log"
	"sync"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
)

type Plugin interface {
	Name() string
	Start(sdk *PluginSDK) error
	Stop() error
}

type ToolHandler func(args map[string]interface{}) (interface{}, error)
type StageHandler func(ctx *StageContext) error

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

type StageContext struct {
	mu              sync.RWMutex
	RawMessage      string
	UserID          string
	GroupID         string
	ContextMsgs     []map[string]interface{}
	LLMText         string
	ReasoningContent string
	TokenUsage      map[string]int
	ToolCalls       []ToolCall
	ToolResults     []ToolResult
	FinalText       string
	Response        *string
	Phase           Stage
	Memory          []MemItem
	NoMemory        bool
	Extra           map[string]interface{}
}

func (c *StageContext) RLock()           { c.mu.RLock() }
func (c *StageContext) RUnlock()         { c.mu.RUnlock() }
func (c *StageContext) Lock()            { c.mu.Lock() }
func (c *StageContext) Unlock()          { c.mu.Unlock() }
func (c *StageContext) IsResponded() bool { c.mu.RLock(); defer c.mu.RUnlock(); return c.Response != nil }

type MemItem struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Score   float64 `json:"score"`
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

type ToolRegistrar func(name string, def ToolDef, handler ToolHandler) error
type StageRegistrar func(stage Stage, handler StageHandler)
type APIRegistrar func(name string) error

type PluginSDK struct {
	name string

	iom       *agentIO.IOManager
	eventBus  *events.Bus
	mem       MemoryAPI
	textMem   TextMemoryAPI
	docMem    DocMemoryAPI
	know      KnowledgeAPI
	llm       LLMAPI
	sett      SettingsAPI
	regTool   ToolRegistrar
	regStage  StageRegistrar
	regAPI    APIRegistrar
	logger    *log.Logger
}

func New(name string, iom *agentIO.IOManager, eventBus *events.Bus, mem MemoryAPI, textMem TextMemoryAPI, docMem DocMemoryAPI, know KnowledgeAPI, llm LLMAPI, sett SettingsAPI, regTool ToolRegistrar, regStage StageRegistrar, regAPI APIRegistrar) *PluginSDK {
	return &PluginSDK{
		name:     name,
		iom:      iom,
		eventBus: eventBus,
		mem:      mem,
		textMem:  textMem,
		docMem:   docMem,
		know:     know,
		llm:      llm,
		sett:     sett,
		regTool:  regTool,
		regStage: regStage,
		regAPI:   regAPI,
		logger:   log.Default(),
	}
}

// === IO 双通道 ===

func (s *PluginSDK) InjectInput(source, channel string, payload map[string]interface{}) {
	if s.iom != nil {
		s.iom.InjectInputTo(source, channel, "text", payload)
	}
}

func (s *PluginSDK) InjectInterrupt(source, channel string, payload map[string]interface{}) {
	if s.iom != nil {
		p := payload
		if p == nil {
			p = map[string]interface{}{}
		}
		if _, ok := p["type"]; !ok {
			p["type"] = "text"
		}
		s.iom.InjectInterrupt(source, channel, p)
	}
}

func (s *PluginSDK) InjectText(source, channel, text string) {
	if s.iom != nil {
		s.iom.InjectTextTo(source, channel, text)
	}
}

// InjectTextNoMemory 注入文本输入（不产生记忆）。适用于健康检查等无需记忆碎片的场景。
func (s *PluginSDK) InjectTextNoMemory(source, channel, text string) {
	if s.iom != nil {
		s.iom.InjectTextNoMemoryTo(source, channel, text)
	}
}

func (s *PluginSDK) InjectTextSync(source, channel, text string) *agentIO.OutputEvent {
	if s.iom != nil {
		return s.iom.InjectTextSyncTo(source, channel, text)
	}
	return nil
}

// InjectTextSyncNoMemory 注入文本输入（同步等待，不产生记忆）。
func (s *PluginSDK) InjectTextSyncNoMemory(source, channel, text string) *agentIO.OutputEvent {
	if s.iom != nil {
		return s.iom.InjectTextSyncNoMemoryTo(source, channel, text)
	}
	return nil
}

func (s *PluginSDK) InjectInterruptText(source, channel, text string) {
	if s.iom != nil {
		s.iom.InjectInterruptText(source, channel, text)
	}
}

func (s *PluginSDK) OutputChan() <-chan *agentIO.OutputEvent {
	if s.iom != nil {
		return s.iom.OutputChan()
	}
	return nil
}

func (s *PluginSDK) RegisterChannel(name string, dev agentIO.Device) error {
	if s.iom != nil {
		return s.iom.RegisterDevice(dev)
	}
	return nil
}

func (s *PluginSDK) UnregisterChannel(name string) {
	if s.iom != nil {
		s.iom.UnregisterDevice(name)
	}
}

func (s *PluginSDK) ListChannels() []agentIO.ChannelInfo {
	if s.iom != nil {
		return s.iom.ListChannels()
	}
	return nil
}

// === 三通道 ===

func (s *PluginSDK) Publish(evt *events.Event) {
	if s.eventBus != nil {
		s.eventBus.Publish(evt)
	}
}

func (s *PluginSDK) Subscribe(eventType events.EventType, handler events.Handler) func() {
	if s.eventBus != nil {
		return s.eventBus.Subscribe(eventType, handler)
	}
	return func() {}
}

// === 能力 ===

func (s *PluginSDK) Memory() MemoryAPI         { return s.mem }
func (s *PluginSDK) TextMemory() TextMemoryAPI  { return s.textMem }
func (s *PluginSDK) DocMemory() DocMemoryAPI    { return s.docMem }
func (s *PluginSDK) Knowledge() KnowledgeAPI    { return s.know }
func (s *PluginSDK) LLM() LLMAPI                { return s.llm }
func (s *PluginSDK) Settings() SettingsAPI      { return s.sett }

func (s *PluginSDK) RegisterTool(name string, def ToolDef, handler ToolHandler) error {
	if s.regTool != nil {
		return s.regTool(name, def, handler)
	}
	return nil
}

func (s *PluginSDK) RegisterStage(stage Stage, handler StageHandler) {
	if s.regStage != nil {
		s.regStage(stage, handler)
	}
}

func (s *PluginSDK) RegisterPluginAPI(name string) error {
	if s.regAPI != nil {
		return s.regAPI(name)
	}
	return nil
}

func (s *PluginSDK) PluginName() string   { return s.name }
func (s *PluginSDK) Logger() *log.Logger  { return s.logger }
