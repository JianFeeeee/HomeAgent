package sdk

import (
	"log"

	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
)

// SDKVersion 是对外 SDK 版本号，与核心 meta.Version 保持一致。
var SDKVersion = pubsdk.SDKVersion

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
type StageScope = pubsdk.StageScope

const (
	StageScopeGlobal   = pubsdk.StageScopeGlobal
	StageScopeOwnTools = pubsdk.StageScopeOwnTools
)
type APIRegistrar = pubsdk.APIRegistrar
type OutputChannelRegistrar = pubsdk.OutputChannelRegistrar
type InputChannelRegistrar = pubsdk.InputChannelRegistrar
type ChannelDef = pubsdk.ChannelDef

type DisabledPluginInfo struct {
	Name       string `json:"name"`
	DisabledAt string `json:"disabled_at"`
	DisabledBy string `json:"disabled_by"`
}

// PluginMeta is the display-name metadata for a plugin (from plg.json / RegisterPluginMeta).
type PluginMeta struct {
	NameZh string `json:"name_zh"`
	NameEn string `json:"name_en"`
}

type PluginManager interface {
	ListLoadedPlugins() []string
	ListDisabledPlugins() []DisabledPluginInfo
	IsPluginDisabled(name string) bool
	DisablePlugin(name, by string) error
	EnablePlugin(name string) error
	ReloadPlugins() (string, error)
	PluginMetas() map[string]PluginMeta
	PluginDir() string
}

type PluginSDK struct {
	*pubsdk.PluginSDK
	settings SettingsAPI
	memory   MemoryAPI
	textMem  TextMemoryAPI
	docMem   DocMemoryAPI
	know     KnowledgeAPI
	llm      LLMAPI

	iom       *agentIO.IOManager
	eventBus  *events.Bus
	logger    *log.Logger
	pluginMgr PluginManager

	status     StatusAPI
	supervisor SupervisorAPI
	skill      SkillAPI
	adapter    AdapterAPI
	tracker    TrackerAPI
	config     ConfigAPI
	tool       ToolAPI
	indexer    IndexerAPI
}

func (s *PluginSDK) PluginMgr() PluginManager { return s.pluginMgr }

// 以下访问器遮蔽公共 SDK 的同名方法，返回内置插件可用的全量接口。

func (s *PluginSDK) Settings() SettingsAPI     { return s.settings }
func (s *PluginSDK) Memory() MemoryAPI         { return s.memory }
func (s *PluginSDK) TextMemory() TextMemoryAPI { return s.textMem }
func (s *PluginSDK) DocMemory() DocMemoryAPI   { return s.docMem }
func (s *PluginSDK) Knowledge() KnowledgeAPI   { return s.know }
func (s *PluginSDK) LLM() LLMAPI               { return s.llm }

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
	RegInput   InputChannelRegistrar
	PluginMgr  PluginManager

	Status     StatusAPI
	Supervisor SupervisorAPI
	Skill      SkillAPI
	Adapter    AdapterAPI
	Tracker    TrackerAPI
	Config     ConfigAPI
	Tool       ToolAPI
	Indexer    IndexerAPI
}

func New(name string, cfg SDKConfig) *PluginSDK {
	base := pubsdk.New(name, cfg.Settings, cfg.RegTool, cfg.RegStage, cfg.RegAPI, cfg.RegOutput)
	if cfg.IOManager != nil {
		base.SetIOInjector(ioAdapter{iom: cfg.IOManager})
	}
	if cfg.RegInput != nil {
		base.SetInputChannelRegistrar(cfg.RegInput)
	}
	base.SetMemoryAPI(cfg.Memory)
	base.SetTextMemoryAPI(cfg.TextMemory)
	base.SetDocMemoryAPI(cfg.DocMemory)
	base.SetKnowledgeAPI(cfg.Knowledge)
	base.SetLLMAPI(cfg.LLM)
	return &PluginSDK{
		PluginSDK: base,
		settings:  cfg.Settings,
		memory:    cfg.Memory,
		textMem:   cfg.TextMemory,
		docMem:    cfg.DocMemory,
		know:      cfg.Knowledge,
		llm:       cfg.LLM,

		iom:       cfg.IOManager,
		eventBus:  cfg.EventBus,
		logger:    log.Default(),
		pluginMgr: cfg.PluginMgr,

		status:     cfg.Status,
		supervisor: cfg.Supervisor,
		skill:      cfg.Skill,
		adapter:    cfg.Adapter,
		tracker:    cfg.Tracker,
		config:     cfg.Config,
		tool:       cfg.Tool,
		indexer:    cfg.Indexer,
	}
}

func (s *PluginSDK) Status() StatusAPI        { return s.status }
func (s *PluginSDK) Supervisor() SupervisorAPI { return s.supervisor }
func (s *PluginSDK) Skill() SkillAPI          { return s.skill }
func (s *PluginSDK) Adapter() AdapterAPI      { return s.adapter }
func (s *PluginSDK) Tracker() TrackerAPI      { return s.tracker }
func (s *PluginSDK) Config() ConfigAPI        { return s.config }
func (s *PluginSDK) Tool() ToolAPI            { return s.tool }
func (s *PluginSDK) Indexer() IndexerAPI      { return s.indexer }

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
