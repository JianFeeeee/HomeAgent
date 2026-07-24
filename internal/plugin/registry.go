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
)

type NativeFactory func(name string, config map[string]interface{}) (sdk.Plugin, error)

// PluginMeta 插件显示名称元数据。
type PluginMeta struct {
	NameZh string `json:"name_zh"`
	NameEn string `json:"name_en"`
}

var globalPluginMeta sync.Map // name -> PluginMeta

// RegisterPluginMeta 供插件包在 init() 中调用，注册显示名称。
func RegisterPluginMeta(name, nameZh, nameEn string) {
	globalPluginMeta.Store(name, PluginMeta{NameZh: nameZh, NameEn: nameEn})
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

	iom       *agentIO.IOManager
	evBus     *events.Bus
	memDB     *memory.GraphDB
	textMem   *text.Memory
	docStore  *doc.Store
	ks        *knowledge.Store
	mgr       *agentAPI.ProviderManager
	cfgReg    *internalConfig.ConfigRegistry
	plgDir    string

	regTool  sdk.ToolRegistrar
	regStage sdk.StageRegistrar
	regAPI   sdk.APIRegistrar

	toolCleaner PluginToolCleaner

	knownDisabled map[string]bool
	textCleaners  []func(string) string
}

func NewRegistry() *Registry {
	return &Registry{
		plugins:           make(map[string]sdk.Plugin),
		factories:         make(map[string]NativeFactory),
		pluginAutoRestart: make(map[string]bool),
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
func (r *Registry) SetToolRegistrar(fn sdk.ToolRegistrar)        { r.regTool = fn }
func (r *Registry) SetStageRegistrar(fn sdk.StageRegistrar)      { r.regStage = fn }
func (r *Registry) SetAPIRegistrar(fn sdk.APIRegistrar)          { r.regAPI = fn }
func (r *Registry) SetToolCleaner(tc PluginToolCleaner)           { r.toolCleaner = tc }

// CleanText applies all registered text cleaners in order.
func (r *Registry) CleanText(text string) string {
	r.mu.RLock()
	cleaners := r.textCleaners
	r.mu.RUnlock()
	for _, fn := range cleaners {
		text = fn(text)
	}
	return text
}

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

	regOutput := func(chName string, caps int, desc string, handler sdk.ToolHandler) error {
		if r.iom == nil {
			return nil
		}
		return r.iom.RegisterDevice(&channelDevice{
			name:    chName,
			caps:    agentIO.OutputCapability(caps),
			desc:    desc,
			handler: handler,
		})
	}

	return sdk.New(name, sdk.SDKConfig{
		IOManager:  r.iom,
		EventBus:   r.evBus,
		Memory:     sdk.NewGraphMemory(r.memDB),
		TextMemory: sdk.NewTextMemory(r.textMem),
		DocMemory:  sdk.NewDocMemory(r.docStore),
		Knowledge:  sdk.NewKnowledge(r.ks),
		LLM:        sdk.NewLLM(r.mgr),
		Settings:   sett,
		RegTool:    regTool,
		RegStage:   regStage,
		RegAPI:     regAPI,
		RegOutput:  regOutput,
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
		r.textCleaners = append(r.textCleaners, plgSDK.TextCleaners()...)
		r.mu.Unlock()
		log.Printf("[plugin] loaded: %s", name)
	}

	return nil
}

func (r *Registry) isDisabled(name string) bool {
	if r.cfgReg == nil {
		return false
	}
	v, err := r.cfgReg.PluginConfig(name).Get("disabled")
	if err != nil || v == nil {
		return false
	}
	return fmt.Sprint(v) == "true"
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
	r.instances = append(r.instances, plg)
	r.textCleaners = append(r.textCleaners, plgSDK.TextCleaners()...)
	r.mu.Unlock()
	log.Printf("[plugin] loaded: %s", name)
	return true
}

func (r *Registry) StopAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.instances {
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s: %v", p.Name(), err)
		}
	}
	r.plugins = make(map[string]sdk.Plugin)
	r.instances = nil
	r.pluginAutoRestart = make(map[string]bool)
	r.textCleaners = nil
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
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s for reload: %v", name, err)
		}
		delete(r.plugins, name)
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
		r.cfgReg.PluginConfig(name).Set("disabled", "false")
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
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s for disable: %v", name, err)
		}
		delete(r.plugins, name)
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
		r.cfgReg.PluginConfig(name).Set("disabled", "true")
	}
	log.Printf("[plugin] disabled: %s", name)
	return nil
}

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

func (r *Registry) PluginMetas() map[string]PluginMeta {
	metas := make(map[string]PluginMeta)
	globalPluginMeta.Range(func(key, val interface{}) bool {
		metas[key.(string)] = val.(PluginMeta)
		return true
	})
	return metas
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
