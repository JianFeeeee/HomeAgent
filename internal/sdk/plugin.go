package sdk

import (
	"log"

	sdkext "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
)

type Plugin interface {
	Name() string
	Start(sdk *PluginSDK) error
	Stop() error
}

type ToolHandler = sdkext.ToolHandler
type StageHandler = sdkext.StageHandler

type Stage = sdkext.Stage

const (
	StageOnInput        = sdkext.StageOnInput
	StagePreAction      = sdkext.StagePreAction
	StagePostAction     = sdkext.StagePostAction
	StageBeforeToolcall = sdkext.StageBeforeToolcall
	StageAfterToolcall  = sdkext.StageAfterToolcall
	StageBeforeOutput   = sdkext.StageBeforeOutput
	StageAfterOutput    = sdkext.StageAfterOutput
)

type StageContext = sdkext.StageContext
type MemItem = sdkext.MemItem
type ToolCall = sdkext.ToolCall
type ToolResult = sdkext.ToolResult
type ToolDef = sdkext.ToolDef

type ToolRegistrar = func(name string, def ToolDef, handler ToolHandler) error
type StageRegistrar = func(stage Stage, handler StageHandler)
type APIRegistrar = func(name string) error

type ioAdapter struct{ iom *agentIO.IOManager }

func (i ioAdapter) InjectInterruptText(source, channel, text string) {
	if i.iom != nil {
		i.iom.InjectInterrupt(source, channel, map[string]interface{}{"type": "text", "content": text})
	}
}

func (i ioAdapter) InjectText(source, channel, text string) {
	if i.iom != nil {
		i.iom.InjectInputTo(source, channel, "text", map[string]interface{}{"content": text})
	}
}

func (i ioAdapter) InjectTextNoMemory(source, channel, text string) {
	if i.iom != nil {
		i.iom.InjectInputTo(source, channel, "text", map[string]interface{}{"content": text, "no_memory": true})
	}
}

type PluginSDK struct {
	*sdkext.PluginSDK
	iom      *agentIO.IOManager
	eventBus *events.Bus
	logger   *log.Logger
}

func New(name string, iom *agentIO.IOManager, eventBus *events.Bus, mem MemoryAPI, textMem TextMemoryAPI, docMem DocMemoryAPI, know KnowledgeAPI, llm LLMAPI, sett SettingsAPI, regTool ToolRegistrar, regStage StageRegistrar, regAPI APIRegistrar) *PluginSDK {
	base := sdkext.New(name, sett, regTool, regStage, regAPI)
	base.SetIOInjector(ioAdapter{iom: iom})
	base.SetMemoryAPI(mem)
	base.SetTextMemoryAPI(textMem)
	base.SetDocMemoryAPI(docMem)
	base.SetKnowledgeAPI(know)
	base.SetLLMAPI(llm)
	return &PluginSDK{
		PluginSDK: base,
		iom:       iom,
		eventBus:  eventBus,
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

func (s *PluginSDK) Logger() *log.Logger { return s.logger }
