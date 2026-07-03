package plugin

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/plugin/sdk"
)

type PluginType string

const (
	PluginTypeSKILL  PluginType = "skill"
	PluginTypeNative PluginType = "native"
)

// IOConfig 定义插件作为 IO 通道时的配置
// 每个插件通过此配置声明自己的 I/O 端口
type IOConfig struct {
	Type        string   `json:"type"`         // "input" / "output" / "io"
	InputRoute  string   `json:"input_route"`  // 输入源标识，如 "qq", "email"
	OutputRoute string   `json:"output_route"` // 输出通道标识，默认等于 InputRoute
	OutputCaps  []string `json:"output_caps"`  // 支持的输出能力: "text","file","image","audio","structured"
}

type Plugin interface {
	Name() string
	PluginType() PluginType
	Description() string
	Version() string
	Tools() []ToolDef
	Enabled() bool
	SetEnabled(bool)
	IOConfig() *IOConfig
	Device() agentIO.Device // 内嵌的 IO 设备，nil 表示纯技能插件
}

// ToolDef 复用 IO 抽象层的定义，确保 Plugin 和 Device 使用同一类型
type ToolDef = agentIO.ToolDef

type SKILLPlugin struct {
	mu          sync.RWMutex
	name        string
	description string
	version     string
	author      string
	enabled     bool
	rawContent  string
	sourceDir   string
	toolDefs    []ToolDef
	ioConfig    *IOConfig
}

func LoadSKILL(path string) (*SKILLPlugin, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	name := filepath.Base(path)
	p := &SKILLPlugin{
		name:      name,
		sourceDir: path,
		enabled:   true,
	}

	if info.IsDir() {
		skillFile := filepath.Join(path, "SKILL.md")
		if data, err := os.ReadFile(skillFile); err == nil {
			p.rawContent = string(data)
			p.description = extractDescription(p.rawContent)
			p.version = extractField(p.rawContent, "version")
			p.author = extractField(p.rawContent, "author")
			p.ioConfig = extractIOConfig(p.rawContent)
		}

		metaFile := filepath.Join(path, "skill.json")
		if data, err := os.ReadFile(metaFile); err == nil {
			var meta struct {
				Name        string   `json:"name"`
				Description string   `json:"description"`
				Version     string   `json:"version"`
				Author      string   `json:"author"`
				IO          *IOConfig `json:"io,omitempty"`
			}
			if err := json.Unmarshal(data, &meta); err == nil {
				if meta.Name != "" {
					p.name = meta.Name
				}
				if meta.Description != "" {
					p.description = meta.Description
				}
				if meta.Version != "" {
					p.version = meta.Version
				}
				if meta.Author != "" {
					p.author = meta.Author
				}
				if meta.IO != nil {
					p.ioConfig = meta.IO
				}
			}
		}
	} else if filepath.Ext(path) == ".md" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		p.rawContent = string(data)
		p.name = name[:len(name)-3]
		p.description = extractDescription(p.rawContent)
		p.ioConfig = extractIOConfig(p.rawContent)
	}

	if p.rawContent != "" {
		p.toolDefs = extractToolDefs(p.rawContent)
	}

	log.Printf("[plugin] loaded SKILL: %s v%s", p.name, p.version)
	return p, nil
}

func (p *SKILLPlugin) Name() string          { return p.name }
func (p *SKILLPlugin) PluginType() PluginType { return PluginTypeSKILL }
func (p *SKILLPlugin) Description() string   { return p.description }
func (p *SKILLPlugin) Version() string     { return p.version }
func (p *SKILLPlugin) Enabled() bool       { p.mu.RLock(); defer p.mu.RUnlock(); return p.enabled }
func (p *SKILLPlugin) SetEnabled(v bool)   { p.mu.Lock(); defer p.mu.Unlock(); p.enabled = v }
func (p *SKILLPlugin) Tools() []ToolDef      { return p.toolDefs }
func (p *SKILLPlugin) IOConfig() *IOConfig   { return p.ioConfig }
func (p *SKILLPlugin) Device() agentIO.Device { return nil } // SKILL 插件无原生 IO 设备
func (p *SKILLPlugin) RawContent() string    { return p.rawContent }

// NativeFactory 是内置插件构造器，用于需要原生 Go 实现的插件（如 OneBot QQ）
// 返回 agentIO.Device 以直接注册到 IO 管理层
type NativeFactory func(name string, config map[string]interface{}, iom *agentIO.IOManager) (agentIO.Device, error)

type Registry struct {
	mu        sync.RWMutex
	plugins   map[string]Plugin
	ioMgr     *agentIO.IOManager
	factories map[string]NativeFactory // 名称匹配的插件使用原生实现
	sdkAPIs   map[string]*sdkAPI       // SDK 插件 API 实例
	cfgReg    *internalConfig.ConfigRegistry
}

// SetConfigRegistry 注入全局配置注册表，使 SDK 插件可访问配置
func (r *Registry) SetConfigRegistry(cr *internalConfig.ConfigRegistry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfgReg = cr
}

type sdkAPI struct {
	api    *sdk.PluginAPI
	tools  map[string]sdk.ToolHandler
	stages map[sdk.Stage][]sdk.StageHandler
}

func NewRegistry() *Registry {
	return &Registry{
		plugins:   make(map[string]Plugin),
		factories: make(map[string]NativeFactory),
		sdkAPIs:   make(map[string]*sdkAPI),
	}
}

// RegisterPluginAPI 注册一个 SDK 插件 API 实例
func (r *Registry) RegisterPluginAPI(api *sdk.PluginAPI) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sdkAPIs[api.Name]; ok {
		return fmt.Errorf("sdk api %s already registered", api.Name)
	}

	// 注入 SettingsAPI（读取/修改核心与其他插件配置）
	if r.cfgReg != nil {
		api.SetSettings(&pluginSettings{
			reg:  r.cfgReg,
			name: api.Name,
		})
	}

	r.sdkAPIs[api.Name] = &sdkAPI{
		api:    api,
		tools:  api.Tools(),
		stages: make(map[sdk.Stage][]sdk.StageHandler),
	}
	for stage := range sdk.AllStages() {
		if handlers := api.StageHandlers(stage); len(handlers) > 0 {
			r.sdkAPIs[api.Name].stages[stage] = handlers
		}
	}
	log.Printf("[plugin] registered SDK plugin: %s (tools=%d, stages=%d)",
		api.Name, len(api.Tools()), len(r.sdkAPIs[api.Name].stages))
	return nil
}

// pluginSettings 实现 SettingsAPI，以插件名为命名空间
type pluginSettings struct {
	reg  *internalConfig.ConfigRegistry
	name string
}

func (s *pluginSettings) Get(key string) (interface{}, error) {
	return s.reg.Get(key)
}

func (s *pluginSettings) Set(key string, value interface{}) error {
	return s.reg.Set(key, value)
}

func (s *pluginSettings) List(prefix string) ([]string, error) {
	return s.reg.List(prefix), nil
}

// GetAllSDKToolDefs 收集所有 SDK 插件的工具定义
func (r *Registry) GetAllSDKToolDefs() []sdk.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var defs []sdk.ToolDef
	for _, sa := range r.sdkAPIs {
		for name := range sa.tools {
			defs = append(defs, sdk.ToolDef{Name: name})
		}
	}
	return defs
}

// ExecuteSDKTool 执行 SDK 插件工具
func (r *Registry) ExecuteSDKTool(name string, args map[string]interface{}) (interface{}, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, sa := range r.sdkAPIs {
		if handler, ok := sa.tools[name]; ok {
			return handler(args)
		}
	}
	return nil, fmt.Errorf("sdk tool %s not found", name)
}

// GetStageHandlers 获取所有 SDK 插件在指定阶段的处理器
func (r *Registry) GetStageHandlers(stage sdk.Stage) []sdk.StageHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var handlers []sdk.StageHandler
	for _, sa := range r.sdkAPIs {
		if h, ok := sa.stages[stage]; ok {
			handlers = append(handlers, h...)
		}
	}
	return handlers
}

// SDKPluginCount 返回已注册的 SDK 插件数量
func (r *Registry) SDKPluginCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sdkAPIs)
}

// RegisterNative 注册内置原生插件工厂。当从 plugins/ 加载插件时，
// 如果插件名称匹配已注册的工厂，优先使用原生设备注册。
// 例如: r.RegisterNative("qq", onebot.NewDeviceFactory)
func (r *Registry) RegisterNative(name string, factory NativeFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
}

// SetIOManager 绑定 IO 管理器，启用 IO 设备自动注册
func (r *Registry) SetIOManager(mgr *agentIO.IOManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ioMgr = mgr
}

func (r *Registry) Register(p Plugin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins[p.Name()] = p
	log.Printf("[plugin] registered: %s (%s)", p.Name(), p.PluginType())

	// 注册内嵌 IO 设备到 IOManager
	if r.ioMgr != nil {
		dev := p.Device()
		if dev != nil {
			// 原生设备直接注册
			if err := r.ioMgr.RegisterDevice(dev); err != nil {
				log.Printf("[plugin] register native device %s: %v", p.Name(), err)
				return
			}
		} else if p.IOConfig() != nil {
			// 有 IO 配置但无原生设备 → 用 PluginDevice 包装
			dev = NewPluginDevice(p)
			if err := r.ioMgr.RegisterDevice(dev); err != nil {
				log.Printf("[plugin] register plugin device %s: %v", p.Name(), err)
				return
			}
		} else {
			return // 纯技能插件，无 IO 通道
		}

		if cfg := p.IOConfig(); cfg != nil {
			log.Printf("[plugin] io device %s active (type=%s, caps=%v)",
				p.Name(), cfg.Type, cfg.OutputCaps)
		}
	}
}

// nativePlugin 包装原生 IO 设备为 Plugin 接口
// Plugin 是容器，Device 是组件
type nativePlugin struct {
	name string
	dev  agentIO.Device
	cfg  *IOConfig
}

func (p *nativePlugin) Name() string                { return p.name }
func (p *nativePlugin) PluginType() PluginType       { return PluginTypeNative }
func (p *nativePlugin) Description() string          { return p.dev.Description() }
func (p *nativePlugin) Version() string              { return "1.0.0" }
func (p *nativePlugin) Tools() []ToolDef             { return p.dev.Tools() }
func (p *nativePlugin) Enabled() bool                { return true }
func (p *nativePlugin) SetEnabled(v bool)            {}
func (p *nativePlugin) IOConfig() *IOConfig          { return p.cfg }
func (p *nativePlugin) Device() agentIO.Device       { return p.dev }

// Reload 原子化重载 plugins/ 目录：
// 1. 扫描磁盘加载新插件，构造并启动新 IO 设备
// 2. 原子替换 IOManager 的设备表与路由表
// 3. 停止并清理旧设备
// 全程无通信中断：旧设备持续服务直到路由切换完成
func (r *Registry) Reload(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "插件目录不存在", nil
		}
		return "", fmt.Errorf("扫描插件目录: %w", err)
	}

	// 1. 扫描磁盘，并行加载新插件
	type loadedPlugin struct {
		name string
		p    Plugin
		err  error
	}
	var loaded []loadedPlugin

	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && !strings.HasSuffix(name, ".md") {
			continue
		}
		if entry.IsDir() {
			// 略过非插件目录（不含 SKILL.md）
			if _, err := os.Stat(filepath.Join(dir, name, "SKILL.md")); os.IsNotExist(err) {
				continue
			}
		} else {
			name = name[:len(name)-3]
		}

		r.mu.RLock()
		_, exists := r.plugins[name]
		r.mu.RUnlock()
		if exists {
			continue // 已存在，跳过（后续可用 diff 检测变更）
		}

		r.mu.RLock()
		factory, hasFactory := r.factories[name]
		r.mu.RUnlock()

		if hasFactory {
			dev, err := factory(name, r.configFor(name, dir), r.ioMgr)
			if err != nil {
				loaded = append(loaded, loadedPlugin{name: name, err: err})
				continue
			}
			np := &nativePlugin{name: name, dev: dev}
			np.cfg = extractIOConfig(r.readFile(filepath.Join(dir, name)))
			loaded = append(loaded, loadedPlugin{name: name, p: np})
		} else {
			p, err := LoadSKILL(filepath.Join(dir, entry.Name()))
			if err != nil {
				loaded = append(loaded, loadedPlugin{name: name, err: err})
				continue
			}
			loaded = append(loaded, loadedPlugin{name: name, p: p})
		}
	}

	// 2. 启动新设备的 IO 通道
	newDevices := make(map[string]agentIO.Device)
	for _, lp := range loaded {
		if lp.err != nil {
			log.Printf("[plugin] skip %s: %v", lp.name, lp.err)
			continue
		}
		dev := lp.p.Device()
		if dev == nil && lp.p.IOConfig() != nil {
			dev = NewPluginDevice(lp.p)
		}
		if dev != nil {
			dev.Start() // 新设备预先启动
			newDevices[lp.name] = dev
		}
	}

	// 3. 原子切换
	r.mu.Lock()
	var oldPlugins map[string]Plugin
	if r.ioMgr != nil {
		// 获取旧设备并原子替换
		oldDevices := r.ioMgr.AtomicSwapDevices(newDevices)
		// 停止旧设备
		for _, dev := range oldDevices {
			go dev.Stop()
		}
	} else {
		for _, dev := range newDevices {
			dev.Stop()
		}
	}
	// 替换插件表
	oldPlugins = r.plugins
	r.plugins = make(map[string]Plugin)
	for _, lp := range loaded {
		if lp.p != nil {
			r.plugins[lp.name] = lp.p
		}
	}
	pluginCount := len(r.plugins)
	ioCount := len(newDevices)
	r.mu.Unlock()

	// 4. 清理旧插件资源
	for _, p := range oldPlugins {
		_ = p
	}

	log.Printf("[plugin] atomic reload: %d plugins, %d io channels", pluginCount, ioCount)
	return fmt.Sprintf("插件重载完成: %d 个插件, %d 个 IO 通道", pluginCount, ioCount), nil
}

func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.plugins[name]; ok && r.ioMgr != nil {
		r.ioMgr.UnregisterDevice(name)
		log.Printf("[plugin] unregistered io device: %s", name)
	}
	delete(r.plugins, name)
}

func (r *Registry) Get(name string) Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.plugins[name]
}

func (r *Registry) List() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]Plugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Name() < list[j].Name()
	})
	return list
}

func (r *Registry) ListEnabled() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var list []Plugin
	for _, p := range r.plugins {
		if p.Enabled() {
			list = append(list, p)
		}
	}
	return list
}

func (r *Registry) GetAllToolDefs() []ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var defs []ToolDef
	for _, p := range r.plugins {
		if !p.Enabled() {
			continue
		}
		defs = append(defs, p.Tools()...)
	}
	return defs
}

// HotReload 定期扫描插件目录，检测新增/变更/删除的插件并动态注册/注销
// interval=0 表示只执行一次扫描
func (r *Registry) HotReload(dir string, interval time.Duration, done <-chan struct{}) {
	if interval <= 0 {
		r.scanAndSync(dir)
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.scanAndSync(dir)
		case <-done:
			return
		}
	}
}

// scanAndSync 扫描插件目录并与当前注册表同步
func (r *Registry) scanAndSync(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[plugin] scan error: %v", err)
		}
		return
	}

	// 收集当前磁盘上的插件名
	diskSet := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			diskSet[name] = true
		} else if strings.HasSuffix(name, ".md") {
			diskSet[name[:len(name)-3]] = true
		}
	}

	r.mu.Lock()
	// 移除已不存在的插件
	for name := range r.plugins {
		if !diskSet[name] {
			if r.ioMgr != nil {
				r.ioMgr.UnregisterDevice(name)
			}
			delete(r.plugins, name)
			log.Printf("[plugin] hot-unload: %s", name)
		}
	}
	r.mu.Unlock()

	// 加载新增的插件
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		name := entry.Name()
		if entry.IsDir() {
			name = entry.Name()
		} else if strings.HasSuffix(name, ".md") {
			name = name[:len(name)-3]
		} else {
			continue
		}

		r.mu.RLock()
		exists := r.plugins[name]
		r.mu.RUnlock()
		if exists != nil {
			continue
		}

		// 检查是否有原生工厂
		r.mu.RLock()
		factory, hasFactory := r.factories[name]
		r.mu.RUnlock()

		if hasFactory {
			// 使用原生设备，构造 Plugin 容器
			dev, err := factory(name, r.configFor(name, dir), r.ioMgr)
			if err != nil {
				log.Printf("[plugin] native factory %s: %v", name, err)
				continue
			}
			np := &nativePlugin{name: name, dev: dev}
			np.cfg = extractIOConfig(r.readFile(path))
			r.Register(np)
			log.Printf("[plugin] hot-load native: %s", name)
		} else {
			var p Plugin
			p, err = LoadSKILL(path)
			if err != nil {
				log.Printf("[plugin] hot-load skip %s: %v", entry.Name(), err)
				continue
			}
			r.Register(p)
			log.Printf("[plugin] hot-load: %s", p.Name())
		}
	}
}

// configFor 读取插件目录的 skill.json 作为原生工厂的配置
func (r *Registry) configFor(name, dir string) map[string]interface{} {
	cfg := map[string]interface{}{}
	path := filepath.Join(dir, name, "skill.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	var meta map[string]interface{}
	if json.Unmarshal(data, &meta) == nil {
		for k, v := range meta {
			cfg[k] = v
		}
	}
	return cfg
}

// readFile 读取插件目录的 SKILL.md
func (r *Registry) readFile(path string) string {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		data, _ := os.ReadFile(filepath.Join(path, "SKILL.md"))
		return string(data)
	}
	data, _ := os.ReadFile(path)
	return string(data)
}

func (r *Registry) LoadDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		name := entry.Name()
		if entry.IsDir() {
			// 原生插件优先
			r.mu.RLock()
			factory, hasFactory := r.factories[name]
			r.mu.RUnlock()
			if hasFactory {
				dev, err := factory(name, r.configFor(name, dir), r.ioMgr)
				if err != nil {
					log.Printf("[plugin] native factory %s: %v", name, err)
					continue
				}
				r.mu.Lock()
				r.plugins[name] = &nativePlugin{name: name, dev: dev}
				r.mu.Unlock()
				if r.ioMgr != nil {
					r.ioMgr.RegisterDevice(dev)
					log.Printf("[plugin] native: %s", name)
				}
				continue
			}
			p, err := LoadSKILL(path)
			if err != nil {
				log.Printf("[plugin] skip dir %s: %v", entry.Name(), err)
				continue
			}
			r.Register(p)
		} else if filepath.Ext(entry.Name()) == ".md" {
			p, err := LoadSKILL(path)
			if err != nil {
				log.Printf("[plugin] skip file %s: %v", entry.Name(), err)
				continue
			}
			r.Register(p)
		}
	}
	return nil
}

// extractIOConfig parses IO port metadata from SKILL.md content
// Supported fields:
//   io_type: input|output|io
//   io_input_route: qq
//   io_output_route: qq   (optional, defaults to input_route)
//   io_output_caps: text,file,image
func extractIOConfig(content string) *IOConfig {
	ioType := extractField(content, "io_type")
	if ioType == "" {
		return nil
	}
	cfg := &IOConfig{
		Type:       ioType,
		InputRoute: extractField(content, "io_input_route"),
	}
	if cfg.InputRoute == "" {
		cfg.InputRoute = extractField(content, "io_route")
	}
	cfg.OutputRoute = extractField(content, "io_output_route")
	if cfg.OutputRoute == "" {
		cfg.OutputRoute = cfg.InputRoute
	}
	capsStr := extractField(content, "io_output_caps")
	if capsStr != "" {
		for _, c := range strings.Split(capsStr, ",") {
			cfg.OutputCaps = append(cfg.OutputCaps, strings.TrimSpace(c))
		}
	}
	return cfg
}

// PluginDevice 将 Plugin 包装为 IO Device，实现热插拔
type PluginDevice struct {
	plugin Plugin
	caps   agentIO.OutputCapability
}

func NewPluginDevice(p Plugin) *PluginDevice {
	var caps agentIO.OutputCapability
	if cfg := p.IOConfig(); cfg != nil {
		// 有 IO 配置时使用声明的能力
		for _, c := range cfg.OutputCaps {
			switch strings.ToLower(c) {
			case "text":
				caps |= agentIO.CapText
			case "file":
				caps |= agentIO.CapFile
			case "image":
				caps |= agentIO.CapImage
			case "audio":
				caps |= agentIO.CapAudio
			case "structured":
				caps |= agentIO.CapStructured
			}
		}
	}
	// 无 IO 配置时 caps == 0 → 纯工具插件，不暴露为输出通道
	return &PluginDevice{plugin: p, caps: caps}
}

func (d *PluginDevice) Name() string             { return d.plugin.Name() }
func (d *PluginDevice) Type() agentIO.DeviceType {
	if cfg := d.plugin.IOConfig(); cfg != nil {
		switch cfg.Type {
		case "input":
			return agentIO.DeviceInput
		case "output":
			return agentIO.DeviceOutput
		case "io":
			return agentIO.DeviceIO
		}
	}
	// 无 IO 配置 → 纯工具插件，归为 DeviceInput（无输出能力）
	return agentIO.DeviceInput
}
func (d *PluginDevice) Description() string       { return d.plugin.Description() }

func (d *PluginDevice) Tools() []agentIO.ToolDef {
	pts := d.plugin.Tools()
	defs := make([]agentIO.ToolDef, len(pts))
	copy(defs, pts)
	return defs
}

func (d *PluginDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	for _, t := range d.plugin.Tools() {
		if t.Name == tool {
			if t.Handler != nil {
				return t.Handler(args)
			}
			return nil, fmt.Errorf("plugin %s: tool %s has no handler", d.plugin.Name(), tool)
		}
	}
	return nil, fmt.Errorf("plugin %s: unknown tool %s", d.plugin.Name(), tool)
}

func (d *PluginDevice) Start() error { return nil }
func (d *PluginDevice) Stop() error  { return nil }
func (d *PluginDevice) OutputCapabilities() agentIO.OutputCapability { return d.caps }

// extractDescription returns the first non-empty, non-header line
func extractDescription(content string) string {
	for _, line := range splitLines(content) {
		line = trimSpace(line)
		if line != "" && !hasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

// extractField finds `field: value` pattern in content
func extractField(content string, field string) string {
	prefix := field + ":"
	lowerPrefix := toLower(prefix)
	for _, line := range splitLines(content) {
		trimmed := trimSpace(line)
		if hasPrefix(toLower(trimmed), lowerPrefix) {
			// 找到冒号位置，提取冒号后的内容
			colonIdx := -1
			for i := 0; i < len(trimmed); i++ {
				if trimmed[i] == ':' {
					colonIdx = i
					break
				}
			}
			if colonIdx >= 0 {
				return trimSpace(trimmed[colonIdx+1:])
			}
		}
	}
	return ""
}

// extractToolDefs 从 SKILL.md 中解析工具定义（OpenClaw 格式）
//
// 格式：
//   ## tool_name
//   工具描述
//   - param1: 参数描述
//   - param2: 参数描述
//
// 也支持：
//   ### Tool: tool_name
//   格式（三级标题）
func extractToolDefs(content string) []ToolDef {
	lines := splitLines(content)
	var defs []ToolDef
	var currentTool *ToolDef
	inCodeBlock := false

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := trimSpace(line)

		// 跳过代码块
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}

		// 检测工具定义开始: ## tool_name 或 ### Tool: tool_name
		if strings.HasPrefix(trimmed, "## ") && !strings.HasPrefix(trimmed, "### ") {
			// 结束上一个工具
			if currentTool != nil && currentTool.Name != "" {
				defs = append(defs, *currentTool)
			}
			currentTool = &ToolDef{}
			namePart := strings.TrimPrefix(trimmed, "## ")
			// 跳过已知的非工具章节
			if isNonToolSection(namePart) {
				currentTool = nil
				continue
			}
			currentTool.Name = namePart
			currentTool.Parameters = map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			}
			continue
		}

		// 也支持 ### Tool: name 格式
		if strings.HasPrefix(trimmed, "### Tool: ") {
			if currentTool != nil && currentTool.Name != "" {
				defs = append(defs, *currentTool)
			}
			currentTool = &ToolDef{}
			namePart := strings.TrimPrefix(trimmed, "### Tool: ")
			currentTool.Name = namePart
			currentTool.Parameters = map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			}
			continue
		}

		if currentTool == nil {
			continue
		}

		// 如果 name 为空且已进入工具段，跳过
		if currentTool.Name == "" {
			continue
		}

		// 描述行：第一个非空、非标题、非列表行
		if currentTool.Description == "" && trimmed != "" &&
			!strings.HasPrefix(trimmed, "- ") &&
			!strings.HasPrefix(trimmed, "#") {
			currentTool.Description = trimmed
			continue
		}

		// 参数行：- param: description
		if strings.HasPrefix(trimmed, "- ") {
			paramLine := strings.TrimPrefix(trimmed, "- ")
			colonIdx := strings.Index(paramLine, ":")
			if colonIdx > 0 {
				paramName := strings.TrimSpace(paramLine[:colonIdx])
				paramDesc := strings.TrimSpace(paramLine[colonIdx+1:])
				if paramName != "" {
					props := currentTool.Parameters["properties"].(map[string]interface{})
					props[paramName] = map[string]interface{}{
						"type":        "string",
						"description": paramDesc,
					}
				}
			}
		}
	}

	// 收尾最后一个工具
	if currentTool != nil && currentTool.Name != "" {
		defs = append(defs, *currentTool)
	}

	return defs
}

// isNonToolSection 判断是否为非工具章节（如 Usage、Examples、Installation 等）
func isNonToolSection(name string) bool {
	lower := strings.ToLower(name)
	skip := []string{
		"tools", "usage", "examples", "installation", "setup",
		"configuration", "overview", "description", "notes",
		"parameters", "return", "returns", "options", "syntax",
	}
	for _, s := range skip {
		if lower == s || strings.HasPrefix(lower, s+" ") || strings.HasPrefix(lower, s+":") {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if i > start {
				lines = append(lines, s[start:i])
			}
			start = i + 1
		}
	}
	return lines
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func hasPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return s[:len(prefix)] == prefix
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			b[i] = s[i] + 32
		} else {
			b[i] = s[i]
		}
	}
	return string(b)
}

func trimPrefix(s, prefix string) string {
	if hasPrefix(s, prefix) {
		return s[len(prefix):]
	}
	return s
}
