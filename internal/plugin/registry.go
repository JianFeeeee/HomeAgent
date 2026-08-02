package plugin

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	doc "gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	luaVM "gitcode.com/JianFeeeee/HomeAgent/internal/lua"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

type NativeFactory func(name string, config map[string]interface{}) (sdk.Plugin, error)

// PluginMeta 插件显示名称元数据，来源于 plg.json / RegisterPluginMeta。
// 与内置 SDK 中的 PluginMeta 保持一致，供 PluginManager 接口返回。
type PluginMeta = sdk.PluginMeta

var globalPluginMeta sync.Map // name -> PluginMeta

// RegisterPluginMeta 供插件包在 init() 中调用，注册显示名称。
func RegisterPluginMeta(name, nameZh, nameEn string) {
	globalPluginMeta.Store(name, sdk.PluginMeta{NameZh: nameZh, NameEn: nameEn})
}

// GetPluginMeta 查询插件的显示名称。
func GetPluginMeta(name string) (PluginMeta, bool) {
	v, ok := globalPluginMeta.Load(name)
	if !ok {
		return PluginMeta{}, false
	}
	return v.(PluginMeta), true
}

// globalFactories 是插件通过 init() 自注册的全局工厂表。
// Registry.RegisterNative() 写入此表；Registry.Load() 从中查找。
var globalFactories sync.Map

// RegisterFactory 供插件包在 init() 中调用，实现自注册。
// plugin.RegisterFactory("timer", func(name string, cfg map[string]interface{}) (sdk.Plugin, error) { ... })
func RegisterFactory(name string, factory NativeFactory) {
	globalFactories.Store(name, factory)
}

// PluginToolCleaner 定义插件工具注销接口，由 StageHost 实现。
type PluginToolCleaner interface {
	UnregisterPluginTools(pluginName string)
}

type Registry struct {
	mu        sync.RWMutex
	plugins   map[string]sdk.Plugin
	instances []sdk.Plugin
	factories map[string]NativeFactory

	pluginAutoRestart map[string]bool
	sdkRefs           map[string]*sdk.PluginSDK

	iom       *agentIO.IOManager
	evBus     *events.Bus
	memDB     *memory.GraphDB
	textMem   *text.Memory
	docStore  *doc.Store
	ks        *knowledge.Store
	mgr       *agentAPI.ProviderManager
	cfgReg    *internalConfig.ConfigRegistry
	plgDir    string
	lua       *luaVM.VM
	baseKey   string

	regTool  sdk.ToolRegistrar
	regStage sdk.StageRegistrar
	regAPI   sdk.APIRegistrar

	toolCleaner PluginToolCleaner

	status    sdk.StatusAPI
	sup       sdk.SupervisorAPI
	trk       *tracker.Tracker
	cfg       *types.Config
	stageHost sdk.ToolSource
	idx       *memory.Indexer

	knownDisabled map[string]bool
}

func NewRegistry() *Registry {
	return &Registry{
		plugins:           make(map[string]sdk.Plugin),
		factories:         make(map[string]NativeFactory),
		pluginAutoRestart: make(map[string]bool),
		sdkRefs:           make(map[string]*sdk.PluginSDK),
		knownDisabled:     make(map[string]bool),
	}
}

func (r *Registry) SetIOManager(iom *agentIO.IOManager)          { r.iom = iom }
func (r *Registry) SetEventBus(evBus *events.Bus)                { r.evBus = evBus }
func (r *Registry) SetMemory(memDB *memory.GraphDB)              { r.memDB = memDB }
func (r *Registry) SetTextMemory(tm *text.Memory)                { r.textMem = tm }
func (r *Registry) SetDocStore(ds *doc.Store)                    { r.docStore = ds }
func (r *Registry) SetKnowledge(ks *knowledge.Store)             { r.ks = ks }
func (r *Registry) SetProviderManager(mgr *agentAPI.ProviderManager) { r.mgr = mgr }
func (r *Registry) SetConfigRegistry(cfgReg *internalConfig.ConfigRegistry) { r.cfgReg = cfgReg }
func (r *Registry) SetPluginDir(dir string)                      { r.plgDir = dir }
func (r *Registry) SetLuaVM(vm *luaVM.VM)                        { r.lua = vm }
func (r *Registry) SetBaseAPIKey(key string)                     { r.baseKey = key }
func (r *Registry) SetToolRegistrar(fn sdk.ToolRegistrar)        { r.regTool = fn }
func (r *Registry) SetStageRegistrar(fn sdk.StageRegistrar)      { r.regStage = fn }
func (r *Registry) SetAPIRegistrar(fn sdk.APIRegistrar)          { r.regAPI = fn }
func (r *Registry) SetToolCleaner(tc PluginToolCleaner)           { r.toolCleaner = tc }
func (r *Registry) SetStatusProvider(sp sdk.StatusAPI)            { r.status = sp }
func (r *Registry) SetSupervisor(sup sdk.SupervisorAPI)           { r.sup = sup }
func (r *Registry) SetTracker(trk *tracker.Tracker)               { r.trk = trk }
func (r *Registry) SetConfig(cfg *types.Config)                   { r.cfg = cfg }
func (r *Registry) SetStageHost(sh sdk.ToolSource)                { r.stageHost = sh }
func (r *Registry) SetIndexer(idx *memory.Indexer)                { r.idx = idx }

func (r *Registry) RegisterNative(name string, factory NativeFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
	globalFactories.Store(name, factory)
}

type channelDevice struct {
	name    string
	desc    string
	caps    agentIO.OutputCapability
	handler sdk.ToolHandler
	chDef   agentIO.ChannelDef
}

func (d *channelDevice) Name() string              { return d.name }
func (d *channelDevice) Type() agentIO.DeviceType  { return agentIO.DeviceOutput }
func (d *channelDevice) Description() string       { return d.desc }
func (d *channelDevice) Start() error              { return nil }
func (d *channelDevice) Stop() error               { return nil }
func (d *channelDevice) OutputCapabilities() agentIO.OutputCapability { return d.caps }
func (d *channelDevice) Tools() []agentIO.ToolDef  { return nil }
func (d *channelDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return d.handler(args)
}
func (d *channelDevice) ChannelDef() agentIO.ChannelDef { return d.chDef }

func (r *Registry) buildSDK(name string) *sdk.PluginSDK {
	sett := sdk.NewSettings(name, r.cfgReg)

	regTool := r.regTool
	if regTool == nil {
		regTool = func(toolName string, def sdk.ToolDef, handler sdk.ToolHandler) error {
			return nil
		}
	}
	regStage := r.regStage
	if regStage == nil {
		regStage = func(stage sdk.Stage, handler sdk.StageHandler) {}
	}
	regAPI := r.regAPI
	if regAPI == nil {
		regAPI = func(name string) error { return nil }
	}

	regOutput := func(chName string, caps int, desc string, def sdk.ChannelDef, handler sdk.ToolHandler) error {
		if r.iom == nil {
			return nil
		}
		return r.iom.RegisterDevice(&channelDevice{
			name:    chName,
			caps:    agentIO.OutputCapability(caps),
			desc:    desc,
			handler: handler,
			chDef:   agentIO.ChannelDef(def),
		})
	}

	regInput := func(name string, def sdk.ChannelDef) error {
		if r.iom == nil {
			return nil
		}
		r.iom.RegisterInputChannel(name, agentIO.ChannelDef(def))
		return nil
	}

	return sdk.New(name, sdk.SDKConfig{
		IOManager:  r.iom,
		EventBus:   r.evBus,
		Memory:     sdk.NewGraphMemory(r.memDB),
		TextMemory: sdk.NewTextMemory(r.textMem),
		DocMemory:  sdk.NewDocMemory(r.docStore),
		Knowledge:  sdk.NewKnowledge(r.ks),
		LLM:        sdk.NewLLM(r.mgr, r.cfgReg, r.lua, r.baseKey),
		Settings:   sett,
		RegTool:    regTool,
		RegStage:   regStage,
		RegAPI:     regAPI,
		RegOutput:  regOutput,
		RegInput:   regInput,
		PluginMgr:  r,

		Status:     r.status,
		Supervisor: r.sup,
		Adapter:    sdk.NewAdapter(r.lua),
		Tracker:    r.trk,
		Config:     sdk.NewConfig(r.cfg),
		Tool:       sdk.NewTool(r.stageHost, r.iom),
		Indexer:    sdk.NewIndexer(r.idx),
	})
}

func (r *Registry) Load(dir string) error {
	if dir == "" {
		dir = r.plgDir
	}
	os.MkdirAll(dir, 0755)

	// 1) 扫描已有目录
	loaded := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			os.MkdirAll(dir, 0755)
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		plgDir := filepath.Join(dir, name)
		if r.loadOne(plgDir, name) {
			loaded[name] = true
		}
	}

	// 2) 对已注册但尚无目录的工厂，创建目录并加载
	r.mu.RLock()
	allFactories := make(map[string]NativeFactory)
	for name, f := range r.factories {
		allFactories[name] = f
	}
	r.mu.RUnlock()

	globalFactories.Range(func(key, val interface{}) bool {
		name := key.(string)
		if _, ok := allFactories[name]; !ok {
			allFactories[name] = val.(NativeFactory)
		}
		return true
	})

	for name, factory := range allFactories {
		if loaded[name] {
			continue
		}
		if r.isDisabled(name) {
			log.Printf("[plugin] %s is disabled, skipping", name)
			r.mu.Lock()
			r.knownDisabled[name] = true
			r.mu.Unlock()
			continue
		}
		plgDir := filepath.Join(dir, name)
		os.MkdirAll(plgDir, 0755)

		cfg := r.readConfig(plgDir)
		p, err := factory(name, cfg)
		if err != nil {
			log.Printf("[plugin] factory %s: %v", name, err)
			continue
		}
		if p == nil {
			continue
		}

		plgSDK := r.buildSDK(name)
		if err := p.Start(plgSDK); err != nil {
			log.Printf("[plugin] start %s: %v", name, err)
			continue
		}

		r.mu.Lock()
		r.plugins[name] = p
		r.pluginAutoRestart[name] = plgSDK.AutoRestart()
		r.instances = append(r.instances, p)
		r.mu.Unlock()
		log.Printf("[plugin] loaded: %s", name)
	}

	return nil
}

func (r *Registry) isDisabled(name string) bool {
	if r.cfgReg == nil {
		return false
	}
	return r.cfgReg.IsPluginDisabled(name)
}

func (r *Registry) loadOne(plgDir, name string) bool {
	if r.isDisabled(name) {
		log.Printf("[plugin] %s is disabled, skipping", name)
		r.mu.Lock()
		r.knownDisabled[name] = true
		r.mu.Unlock()
		return false
	}

	// 1) 查找工厂（init 自注册或 RegisterNative）
	r.mu.RLock()
	factory, hasFactory := r.factories[name]
	r.mu.RUnlock()
	if !hasFactory {
		v, ok := globalFactories.Load(name)
		if ok {
			factory = v.(NativeFactory)
			hasFactory = true
		}
	}

	var plg sdk.Plugin

	// 读取 plugin.json 以获取插件显示名称元数据（主要用于外部插件）
	if mft := readManifest(plgDir); mft != nil {
		if mft.NameZh != "" || mft.NameEn != "" {
			RegisterPluginMeta(name, mft.NameZh, mft.NameEn)
		}
	}

	if hasFactory {
		cfg := r.readConfig(plgDir)
		p, err := factory(name, cfg)
		if err != nil {
			log.Printf("[plugin] factory %s: %v", name, err)
			return false
		}
		if p == nil {
			return false
		}
		plg = p
	} else {
		// 2) 无工厂，尝试动态加载 .so / .lua
		dynCfg := r.readConfig(plgDir)
		p, err := r.tryDynamic(plgDir, name, dynCfg)
		if err != nil {
			log.Printf("[plugin] dynamic %s: %v", name, err)
		}
		if p == nil {
			return false
		}
		plg = p
	}

	plgSDK := r.buildSDK(name)
	if err := plg.Start(plgSDK); err != nil {
		log.Printf("[plugin] start %s: %v", name, err)
		return false
	}

	r.mu.Lock()
	r.plugins[name] = plg
	r.pluginAutoRestart[name] = plgSDK.AutoRestart()
	r.sdkRefs[name] = plgSDK
	r.instances = append(r.instances, plg)
	r.mu.Unlock()
	log.Printf("[plugin] loaded: %s", name)
	return true
}

// runStopHandlers 执行插件注册的停止清理回调（SDK 层），须在调用插件 Stop() 之前执行。
func (r *Registry) runStopHandlers(name string) {
	if sdk, ok := r.sdkRefs[name]; ok {
		sdk.RunStopHandlers()
	}
}

// runOnRemoveHandlers 执行插件注册的删除清理回调（SDK 层），插件 Stop() 之后、从注册表移除前执行。
func (r *Registry) runOnRemoveHandlers(name string) {
	if sdk, ok := r.sdkRefs[name]; ok {
		sdk.RunOnRemoveHandlers()
	}
}

func (r *Registry) StopAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.instances {
		r.runStopHandlers(p.Name())
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s: %v", p.Name(), err)
		}
	}
	r.plugins = make(map[string]sdk.Plugin)
	r.instances = nil
	r.pluginAutoRestart = make(map[string]bool)
	r.sdkRefs = make(map[string]*sdk.PluginSDK)
}

func (r *Registry) Reload(dir string) (string, error) {
	r.StopAll()
	if err := r.Load(dir); err != nil {
		return "", err
	}
	return fmt.Sprintf("loaded %d plugins", len(r.instances)), nil
}

func (r *Registry) ReloadOne(name string) error {
	plgDir := filepath.Join(r.plgDir, name)

	r.mu.Lock()
	if p, ok := r.plugins[name]; ok {
		r.runStopHandlers(name)
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s for reload: %v", name, err)
		}
		delete(r.plugins, name)
		delete(r.sdkRefs, name)
		for i, inst := range r.instances {
			if inst.Name() == name {
				r.instances = append(r.instances[:i], r.instances[i+1:]...)
				break
			}
		}
	}
	r.mu.Unlock()

	ok := r.loadOne(plgDir, name)
	if !ok {
		return fmt.Errorf("reload plugin %s failed", name)
	}
	log.Printf("[plugin] reloaded: %s", name)
	return nil
}

func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]string, 0, len(r.plugins))
	for name := range r.plugins {
		list = append(list, name)
	}
	sort.Strings(list)
	return list
}

func (r *Registry) Get(name string) sdk.Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.plugins[name]
}

func (r *Registry) AutoRestartEnabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	enabled, ok := r.pluginAutoRestart[name]
	if !ok {
		return true
	}
	return enabled
}

func (r *Registry) IsDisabled(name string) bool {
	return r.isDisabled(name)
}

func (r *Registry) Enable(name string) error {
	if r.cfgReg != nil {
		r.cfgReg.RemoveDisabledPlugin(name)
	}
	r.mu.Lock()
	delete(r.knownDisabled, name)
	r.mu.Unlock()
	plgDir := filepath.Join(r.plgDir, name)
	if r.loadOne(plgDir, name) {
		log.Printf("[plugin] enabled: %s", name)
		return nil
	}
	return fmt.Errorf("enable plugin %s failed", name)
}

func (r *Registry) Disable(name string) error {
	r.mu.Lock()
	p, ok := r.plugins[name]
	if ok {
		r.runStopHandlers(name)
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s for disable: %v", name, err)
		}
		delete(r.plugins, name)
		delete(r.sdkRefs, name)
		for i, inst := range r.instances {
			if inst.Name() == name {
				r.instances = append(r.instances[:i], r.instances[i+1:]...)
				break
			}
		}
	}
	r.knownDisabled[name] = true
	r.mu.Unlock()

	if r.toolCleaner != nil {
		r.toolCleaner.UnregisterPluginTools(name)
	}

	if r.cfgReg != nil {
		r.cfgReg.AddDisabledPlugin(name, "system")
	}
	log.Printf("[plugin] disabled: %s", name)
	return nil
}

// ---- PluginManager interface ----

func (r *Registry) ListLoadedPlugins() []string { return r.List() }

func (r *Registry) ListDisabledPlugins() []sdk.DisabledPluginInfo {
	if r.cfgReg == nil {
		return nil
	}
	list, err := r.cfgReg.ListDisabledPlugins()
	if err != nil {
		return nil
	}
	result := make([]sdk.DisabledPluginInfo, len(list))
	for i, v := range list {
		result[i] = sdk.DisabledPluginInfo(v)
	}
	return result
}

func (r *Registry) IsPluginDisabled(name string) bool { return r.isDisabled(name) }

func (r *Registry) DisablePlugin(name, by string) error {
	// Check not disabling self if running
	if r.cfgReg != nil {
		// If already disabled, no-op
		if r.cfgReg.IsPluginDisabled(name) {
			return fmt.Errorf("plugin %s already disabled", name)
		}
	}

	r.mu.Lock()
	p, ok := r.plugins[name]
	if ok {
		r.runStopHandlers(name)
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s for disable: %v", name, err)
		}
		delete(r.plugins, name)
		delete(r.sdkRefs, name)
		for i, inst := range r.instances {
			if inst.Name() == name {
				r.instances = append(r.instances[:i], r.instances[i+1:]...)
				break
			}
		}
	}
	r.knownDisabled[name] = true
	r.mu.Unlock()

	if r.toolCleaner != nil {
		r.toolCleaner.UnregisterPluginTools(name)
	}

	if r.cfgReg != nil {
		r.cfgReg.AddDisabledPlugin(name, by)
	}
	log.Printf("[plugin] disabled: %s (by %s)", name, by)
	return nil
}

func (r *Registry) EnablePlugin(name string) error { return r.Enable(name) }

// RemovePlugin 卸载插件：先停止（stop handlers + Stop），再执行插件注册的 onRemove
// 回调（删除专用，重载不触发），最后从注册表移除并清理禁用/工具注册。
// 插件目录的物理删除由调用方（pluginmgr）负责。
func (r *Registry) RemovePlugin(name string) error {
	r.mu.Lock()
	p, ok := r.plugins[name]
	if ok {
		r.runStopHandlers(name)
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s for remove: %v", name, err)
		}
		delete(r.plugins, name)
		delete(r.sdkRefs, name)
		for i, inst := range r.instances {
			if inst.Name() == name {
				r.instances = append(r.instances[:i], r.instances[i+1:]...)
				break
			}
		}
	}
	r.runOnRemoveHandlers(name)
	r.mu.Unlock()

	if r.toolCleaner != nil {
		r.toolCleaner.UnregisterPluginTools(name)
	}
	if r.cfgReg != nil {
		r.cfgReg.RemoveDisabledPlugin(name)
	}
	log.Printf("[plugin] removed: %s", name)
	return nil
}

func (r *Registry) ReloadPlugins() (string, error) { return r.Reload(r.plgDir) }

// ListKnown 返回所有已知插件（已加载 + 已禁用 + 已安装但未加载）。
func (r *Registry) ListKnown() []string {
	r.mu.RLock()
	known := make(map[string]bool)
	for name := range r.plugins {
		known[name] = true
	}
	for name := range r.knownDisabled {
		known[name] = true
	}
	r.mu.RUnlock()

	if r.plgDir != "" {
		entries, _ := os.ReadDir(r.plgDir)
		for _, e := range entries {
			if e.IsDir() {
				known[e.Name()] = true
			}
		}
	}

	r.mu.RLock()
	for name := range r.factories {
		known[name] = true
	}
	r.mu.RUnlock()

	globalFactories.Range(func(key, val interface{}) bool {
		known[key.(string)] = true
		return true
	})

	list := make([]string, 0, len(known))
	for name := range known {
		list = append(list, name)
	}
	sort.Strings(list)
	return list
}

func (r *Registry) PluginMetas() map[string]sdk.PluginMeta {
	metas := make(map[string]sdk.PluginMeta)
	globalPluginMeta.Range(func(key, val interface{}) bool {
		metas[key.(string)] = val.(sdk.PluginMeta)
		return true
	})
	return metas
}

func (r *Registry) PluginDir() string {
	return r.plgDir
}

func (r *Registry) tryDynamic(plgDir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	// 尝试顺序：.so (Go plugin on Linux) → .dll (Windows) → .lua (跨平台)
	for _, try := range []struct {
		name string
		fn   func(string, string, map[string]interface{}) (sdk.Plugin, error)
	}{
		{"so", tryLoadSO},
		{"dll", tryLoadDLL},
		{"lua", tryLoadLua},
	} {
		plg, err := try.fn(plgDir, name, config)
		if err != nil {
			return nil, err
		}
		if plg != nil {
			return plg, nil
		}
	}
	return nil, nil
}

func (r *Registry) readConfig(plgDir string) map[string]interface{} {
	cfg := map[string]interface{}{}
	data, err := os.ReadFile(filepath.Join(plgDir, "skill.json"))
	if err != nil {
		return cfg
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(data, &meta); err == nil {
		return meta
	}
	return cfg
}
