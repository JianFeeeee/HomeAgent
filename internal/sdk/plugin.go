package sdk

import (
	"log"

	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
)

type Plugin interface {
	Name() string
	Start(sdk *PluginSDK) error
	Stop() error
}

type ToolHandler = pubsdk.ToolHandler
type StageHandler = pubsdk.StageHandler

type Stage = pubsdk.Stage

const (
	StageOnInput        = pubsdk.StageOnInput
	StagePreAction      = pubsdk.StagePreAction
	StagePostAction     = pubsdk.StagePostAction
	StageBeforeToolcall = pubsdk.StageBeforeToolcall
	StageAfterToolcall  = pubsdk.StageAfterToolcall
	StageBeforeOutput   = pubsdk.StageBeforeOutput
	StageAfterOutput    = pubsdk.StageAfterOutput
)

type StageContext = pubsdk.StageContext
type MemItem = pubsdk.MemItem
type ToolCall = pubsdk.ToolCall
type ToolResult = pubsdk.ToolResult
type ToolDef = pubsdk.ToolDef
type IOInjector = pubsdk.IOInjector
type ToolRegistrar = pubsdk.ToolRegistrar
type StageRegistrar = pubsdk.StageRegistrar
type APIRegistrar = pubsdk.APIRegistrar
type OutputChannelRegistrar = pubsdk.OutputChannelRegistrar

type PluginSDK struct {
	*pubsdk.PluginSDK
	iom      *agentIO.IOManager
	eventBus *events.Bus
	logger   *log.Logger
}

// ioAdapter 桥接 IOManager 到公共 SDK 的 IOInjector 接口，
// 确保外部插件通过 s.InjectText() 等方法的调用能被路由到内核 IO 层。
type ioAdapter struct{ iom *agentIO.IOManager }

func (a ioAdapter) InjectInterruptText(source, channel, text string) {
	if a.iom != nil {
		a.iom.InjectInterrupt(source, channel, map[string]interface{}{"type": "text", "content": text})
	}
}

func (a ioAdapter) InjectText(source, channel, text string) {
	if a.iom != nil {
		a.iom.InjectInputTo(source, channel, "text", map[string]interface{}{"content": text})
	}
}

func (a ioAdapter) InjectTextNoMemory(source, channel, text string) {
	if a.iom != nil {
		a.iom.InjectInputTo(source, channel, "text", map[string]interface{}{"content": text, "no_memory": true})
	}
}

// SDKConfig holds all dependencies for creating a PluginSDK.
type SDKConfig struct {
	IOManager  *agentIO.IOManager
	EventBus   *events.Bus
	Memory     MemoryAPI
	TextMemory TextMemoryAPI
	DocMemory  DocMemoryAPI
	Knowledge  KnowledgeAPI
	LLM        LLMAPI
	Settings   SettingsAPI
	RegTool    ToolRegistrar
	RegStage   StageRegistrar
	RegAPI     APIRegistrar
	RegOutput  OutputChannelRegistrar
}

func New(name string, cfg SDKConfig) *PluginSDK {
	base := pubsdk.New(name, cfg.Settings, cfg.RegTool, cfg.RegStage, cfg.RegAPI, cfg.RegOutput)
	if cfg.IOManager != nil {
		base.SetIOInjector(ioAdapter{iom: cfg.IOManager})
	}
	base.SetMemoryAPI(cfg.Memory)
	base.SetTextMemoryAPI(cfg.TextMemory)
	base.SetDocMemoryAPI(cfg.DocMemory)
	base.SetKnowledgeAPI(cfg.Knowledge)
	base.SetLLMAPI(cfg.LLM)
	return &PluginSDK{
		PluginSDK: base,
		iom:       cfg.IOManager,
		eventBus:  cfg.EventBus,
		logger:    log.Default(),
	}
}

func (s *PluginSDK) InjectInput(source, channel, eventType string, payload map[string]interface{}) {
	if s.iom != nil {
		s.iom.InjectInputTo(source, channel, eventType, payload)
	}
}

func (s *PluginSDK) InjectInputSync(source, channel, eventType string, payload map[string]interface{}) *agentIO.OutputEvent {
	if s.iom != nil {
		return s.iom.InjectInputSyncTo(source, channel, eventType, payload)
	}
	return nil
}

func (s *PluginSDK) InjectInterrupt(source, channel, eventType string, payload map[string]interface{}) {
	if s.iom != nil {
		if payload == nil {
			payload = map[string]interface{}{}
		}
		payload["type"] = eventType
		s.iom.InjectInterrupt(source, channel, payload)
	}
}

func (s *PluginSDK) InjectTextSync(source, channel, text string) *agentIO.OutputEvent {
	return s.InjectInputSync(source, channel, "text", map[string]interface{}{"content": text})
}

func (s *PluginSDK) InjectTextSyncNoMemory(source, channel, text string) *agentIO.OutputEvent {
	return s.InjectInputSync(source, channel, "text", map[string]interface{}{"content": text, "no_memory": true})
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
