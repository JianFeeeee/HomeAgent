package openclaw

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

//go:embed simulator/main.js
var simulatorSrc string

//go:embed manager/main.js
var managerSrc string

//go:embed pysimulator/main.py
var pySimulatorSrc string

var SkillsDir string
var SimulatorDir string

func init() {
	plugin.RegisterPluginMeta("openclaw", "开放式交互", "OpenClaw")
	plugin.RegisterFactory("openclaw", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		dir := SkillsDir
		if dir == "" {
			dataDir, ok := config["data_dir"].(string)
			if !ok {
				return nil, fmt.Errorf("openclaw plugin: config missing 'data_dir' or not a string")
			}
			dir = filepath.Join(dataDir, "skills")
		}
		return New(name, dir), nil
	})
}

type Plugin struct {
	name          string
	skillsDir     string
	simulatorDir  string
	skills        []*plugin.SKILLPlugin
	sidecars      []*sidecarProcess
	manager       *sidecarProcess
	capabilities  []string
	mu            sync.Mutex
	sdk           *sdk.PluginSDK
}

func New(name, skillsDir string) *Plugin {
	sd := SimulatorDir
	if sd == "" {
		sd = filepath.Join(skillsDir, ".simulator")
	}
	return &Plugin{
		name:         name,
		skillsDir:    skillsDir,
		simulatorDir: sd,
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "skills_dir", Type: "string", DisplayName: "Skill 加载目录",
		Description: "OpenClaw 技能加载目录路径（留空则使用默认路径）",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "simulator_dir", Type: "string", DisplayName: "模拟器工作目录",
		Description: "OpenClaw 模拟器工作目录路径（留空则使用默认路径）",
	})
	if v, _ := s.Settings().Get("skills_dir"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			p.skillsDir = s
		}
	}
	if v, _ := s.Settings().Get("simulator_dir"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			p.simulatorDir = s
		}
	}

	// Launch OC plugin manager first (handles OC-format plugin installation and lifecycle)
	os.MkdirAll(p.skillsDir, 0755)
	if err := p.launchManager(s); err != nil {
		log.Printf("[openclaw] launch manager: %v", err)
	}

	// Load existing plugins from skills dir
	if entries, err := os.ReadDir(p.skillsDir); err == nil {
		for _, entry := range entries {
			skillPath := filepath.Join(p.skillsDir, entry.Name())
			subs, err := os.ReadDir(skillPath)
			if err != nil {
				continue
			}

			hasMainJS := false
			hasMainPy := false
			hasOCManifest := false
			hasOCPackage := false
			for _, f := range subs {
				switch f.Name() {
				case "main.js":
					hasMainJS = true
				case "main.py":
					hasMainPy = true
				case "openclaw.plugin.json":
					hasOCManifest = true
				case "package.json":
					hasOCPackage = hasOCExtensions(filepath.Join(skillPath, "package.json"))
				}
			}

			switch {
			case hasMainJS:
				if err := p.loadSidecar(s, skillPath, entry.Name()); err != nil {
					log.Printf("[openclaw] sidecar %s: %v", entry.Name(), err)
				}
			case hasMainPy:
				if err := p.loadPySidecar(s, skillPath, entry.Name()); err != nil {
					log.Printf("[openclaw] pysidecar %s: %v", entry.Name(), err)
				}
			case hasOCManifest || hasOCPackage:
				log.Printf("[openclaw] ocplugin %s handled by manager", entry.Name())
			default:
				sk, err := plugin.LoadSKILL(skillPath)
				if err != nil {
					log.Printf("[openclaw] load skill %s: %v", entry.Name(), err)
					continue
				}
				p.skills = append(p.skills, sk)
				log.Printf("[openclaw] loaded skill: %s v%s", sk.Name(), sk.Version())
			}
		}
	}

	// Register plugin management tools that talk to the manager (always, even if skills dir is empty)
	tp := p.name + "_"
	s.RegisterTool(tp+"npm_install", sdk.ToolDef{
		Name:        tp + "npm_install",
		Description: "安装 OpenClaw 插件管理器中的 npm 插件。通过 npm 安装包，自动检测并加载到模拟器中。安装后立即可用。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"package": map[string]interface{}{"type": "string", "description": "npm 包名或 git 地址 (例如 @openclaw/voice-call, npm:@openclaw/matrix)"},
			},
			"required": []string{"package"},
		},
	}, p.handlePluginInstall)

	s.RegisterTool(tp+"npm_uninstall", sdk.ToolDef{
		Name:        tp + "npm_uninstall",
		Description: "从 OpenClaw 插件管理器中移除已安装的插件。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "要移除的插件名称"},
			},
			"required": []string{"name"},
		},
	}, p.handlePluginUninstall)

	s.RegisterTool(tp+"list", sdk.ToolDef{
		Name:        tp + "list",
		Description: "列出插件管理器中所有已安装的 OpenClaw 插件及其工具。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handlePluginList)

	return nil
}

// ─── Manager ────────────────────────────────────────────────

// launchManager 启动 OC 插件管理器（Node.js 进程），用于安装/卸载/加载 OC 格式插件
func (p *Plugin) launchManager(s *sdk.PluginSDK) error {
	managerPath := filepath.Join(p.simulatorDir, "manager.js")
	if err := os.MkdirAll(p.simulatorDir, 0755); err != nil {
		return fmt.Errorf("create simulator dir: %w", err)
	}
	if err := os.WriteFile(managerPath, []byte(managerSrc), 0644); err != nil {
		return fmt.Errorf("write manager: %w", err)
	}

	// Ensure skills dir exists for the manager to scan
	os.MkdirAll(p.skillsDir, 0755)

	sp, err := launchProcess("node", managerPath, p.skillsDir, "manager")
	if err != nil {
		return fmt.Errorf("launch manager: %w", err)
	}
	if sp == nil {
		return nil
	}

	// Drain initial registration notifications from already-loaded plugins
	for done := false; !done; {
		select {
		case n := <-sp.NotifyChan():
			p.translateAndRegister(n, sp, s, "manager")
		default:
			done = true
		}
	}
	go p.notifyLoop(sp, s, "manager")

	p.mu.Lock()
	p.manager = sp
	p.sidecars = append(p.sidecars, sp)
	p.mu.Unlock()

	log.Printf("[openclaw] OC plugin manager started")
	return nil
}

// ─── 管理工具处理 ────────────────────────────────────────────

func (p *Plugin) handlePluginInstall(args map[string]interface{}) (interface{}, error) {
	pkg, _ := args["package"].(string)
	if pkg == "" {
		return errorResult("package is required"), nil
	}

	p.mu.Lock()
	mgr := p.manager
	p.mu.Unlock()

	if mgr == nil {
		return errorResult("plugin manager not available"), nil
	}

	data, err := mgr.call("plugins/install", map[string]interface{}{
		"package": pkg,
	})
	if err != nil {
		return errorResult(fmt.Sprintf("install failed: %v", err)), nil
	}

	var result struct {
		Name  string   `json:"name"`
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return map[string]interface{}{
			"content": fmt.Sprintf("Plugin installed. Raw response: %s", string(data)),
		}, nil
	}

	return map[string]interface{}{
		"content": fmt.Sprintf("已安装插件: %s\n  工具: %s", result.Name, strings.Join(result.Tools, ", ")),
	}, nil
}

func (p *Plugin) handlePluginUninstall(args map[string]interface{}) (interface{}, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return errorResult("name is required"), nil
	}

	p.mu.Lock()
	mgr := p.manager
	p.mu.Unlock()

	if mgr == nil {
		return errorResult("plugin manager not available"), nil
	}

	data, err := mgr.call("plugins/uninstall", map[string]interface{}{
		"name": name,
	})
	if err != nil {
		return errorResult(fmt.Sprintf("uninstall failed: %v", err)), nil
	}

	return map[string]interface{}{
		"content": fmt.Sprintf("已卸载插件: %s\n  %s", name, string(data)),
	}, nil
}

func (p *Plugin) handlePluginList(args map[string]interface{}) (interface{}, error) {
	p.mu.Lock()
	mgr := p.manager
	p.mu.Unlock()

	if mgr != nil {
		data, err := mgr.call("plugins/list", nil)
		if err == nil && data != nil {
			var result struct {
				Plugins []struct {
					Name  string `json:"name"`
					Tools []struct {
						Name        string `json:"name"`
						Description string `json:"description"`
					} `json:"tools"`
				} `json:"plugins"`
			}
			if err := json.Unmarshal(data, &result); err == nil {
				var parts []string
				parts = append(parts, fmt.Sprintf("Skills dir: %s\n", p.skillsDir))

				if len(result.Plugins) > 0 {
					parts = append(parts, fmt.Sprintf("\nOC 插件 (%d):", len(result.Plugins)))
					for _, pl := range result.Plugins {
						var names []string
						for _, t := range pl.Tools {
							names = append(names, t.Name)
						}
						parts = append(parts, fmt.Sprintf("  %s: %s", pl.Name, strings.Join(names, ", ")))
					}
				}

				p.mu.Lock()
				if len(p.sidecars) > 0 {
					sidecarCount := 0
					for _, sp := range p.sidecars {
						if sp != p.manager {
							sidecarCount++
						}
					}
					if sidecarCount > 0 {
						parts = append(parts, fmt.Sprintf("\nSidecar 插件 (%d):", sidecarCount))
						for _, sp := range p.sidecars {
							if sp == p.manager {
								continue
							}
							tools, _ := sp.ListTools()
							var names []string
							for _, t := range tools {
								names = append(names, t.Name)
							}
							parts = append(parts, fmt.Sprintf("  %s: %s", sp.name, strings.Join(names, ", ")))
						}
					}
				}
				if len(p.skills) > 0 {
					parts = append(parts, fmt.Sprintf("\nSKILL 插件 (%d):", len(p.skills)))
					for _, sk := range p.skills {
						parts = append(parts, fmt.Sprintf("  %s v%s", sk.Name(), sk.Version()))
					}
				}
				if len(p.capabilities) > 0 {
					parts = append(parts, fmt.Sprintf("\nCapabilities (%d):", len(p.capabilities)))
					for _, c := range p.capabilities {
						parts = append(parts, fmt.Sprintf("  %s", c))
					}
				}
				if len(result.Plugins) == 0 && len(p.sidecars) <= 1 && len(p.skills) == 0 {
					parts = append(parts, "没有已安装的插件。")
				}
				p.mu.Unlock()

				return map[string]interface{}{
					"content": strings.Join(parts, "\n"),
				}, nil
			}
		}
	}

	// Fallback: list known plugins from Go side
	p.mu.Lock()
	defer p.mu.Unlock()

	var parts []string
	parts = append(parts, fmt.Sprintf("Skills dir: %s\n", p.skillsDir))

	if len(p.sidecars) > 0 {
		parts = append(parts, fmt.Sprintf("\nSidecar/OC 插件 (%d):", len(p.sidecars)))
		for _, sp := range p.sidecars {
			tools, err := sp.ListTools()
			toolList := ""
			if err == nil {
				var names []string
				for _, t := range tools {
					names = append(names, t.Name)
				}
				toolList = strings.Join(names, ", ")
			}
			parts = append(parts, fmt.Sprintf("  %s: %s", sp.name, toolList))
		}
	}

	if len(p.skills) > 0 {
		parts = append(parts, fmt.Sprintf("\nSKILL 插件 (%d):", len(p.skills)))
		for _, sk := range p.skills {
			parts = append(parts, fmt.Sprintf("  %s v%s", sk.Name(), sk.Version()))
		}
	}

	if len(p.capabilities) > 0 {
		parts = append(parts, fmt.Sprintf("\nCapabilities (%d):", len(p.capabilities)))
		for _, c := range p.capabilities {
			parts = append(parts, fmt.Sprintf("  %s", c))
		}
	}

	if len(p.sidecars) == 0 && len(p.skills) == 0 {
		parts = append(parts, "没有已安装的插件。")
	}

	return map[string]interface{}{
		"content": strings.Join(parts, "\n"),
	}, nil
}

func (p *Plugin) loadOCPlugin(s *sdk.PluginSDK, dir, name string) error {
	simPath := filepath.Join(p.simulatorDir, "main.js")
	if err := os.MkdirAll(p.simulatorDir, 0755); err != nil {
		return fmt.Errorf("create simulator dir: %w", err)
	}
	if err := os.WriteFile(simPath, []byte(simulatorSrc), 0644); err != nil {
		return fmt.Errorf("write simulator: %w", err)
	}

	sp, err := launchProcess("node", simPath, dir, name)
	if err != nil {
		return fmt.Errorf("launch simulator: %w", err)
	}
	if sp == nil {
		return nil
	}

	// 通知是唯一注册路径。
	// 插件 register(api) 期间模拟器将所有 register* 调用以通知推送给 Go。
	// waitReady 之后所有初始化通知已缓冲在 notifyCh 中，同步排空处理。
	for done := false; !done; {
		select {
		case n := <-sp.NotifyChan():
			p.translateAndRegister(n, sp, s, name)
		default:
			done = true
		}
	}

	// 启动持久通知协程：后续模拟器推送的注册通知持续转译注册到核心
	go p.notifyLoop(sp, s, name)

	// ListTools 仅验证日志，不参与注册
	tools, err := sp.ListTools()
	if err != nil {
		sp.Close()
		return fmt.Errorf("list tools: %w", err)
	}
	log.Printf("[openclaw] ocplugin %s verified %d tools via ListTools", name, len(tools))

	p.mu.Lock()
	p.sidecars = append(p.sidecars, sp)
	p.mu.Unlock()
	return nil
}

// notifyLoop 持续监听模拟器的注册通知，即时转译注册到核心。
// 生命周期绑定 sidecarProcess.NotifyChan，sp.Close() 时会关闭通道使循环退出。
func (p *Plugin) notifyLoop(sp *sidecarProcess, s *sdk.PluginSDK, pluginName string) {
	for n := range sp.NotifyChan() {
		p.translateAndRegister(n, sp, s, pluginName)
	}
}

func (p *Plugin) translateAndRegister(n OCNotification, sp *sidecarProcess, s *sdk.PluginSDK, pluginName string) {
	if n.Method != "register" {
		return
	}
	var params struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(n.Params, &params); err != nil {
		return
	}

	switch params.Type {

	case "tool":
		var d struct {
			Name        string                 `json:"name"`
			Label       string                 `json:"label"`
			Description string                 `json:"description"`
			Parameters  map[string]interface{} `json:"parameters"`
		}
		if err := json.Unmarshal(params.Data, &d); err != nil || d.Name == "" {
			return
		}
		toolName := fmt.Sprintf("%s_%s", pluginName, d.Name)
		tDef := sdk.ToolDef{
			Name:        toolName,
			Description: d.Description,
			Parameters:  d.Parameters,
		}
		handler := func(sp *sidecarProcess, ocToolName string) sdk.ToolHandler {
			return func(args map[string]interface{}) (interface{}, error) {
				return sp.CallTool(ocToolName, args)
			}
		}(sp, d.Name)
		if err := s.RegisterTool(toolName, tDef, handler); err != nil {
			log.Printf("[openclaw] translate register tool %s: %v", toolName, err)
		}

	case "provider":
		var d struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		json.Unmarshal(params.Data, &d)
		cap := fmt.Sprintf("[%s] provides %s provider", pluginName, d.Name)
		if d.Description != "" {
			cap += ": " + d.Description
		}
		p.mu.Lock()
		p.capabilities = append(p.capabilities, cap)
		p.mu.Unlock()

	case "channel":
		var d struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		json.Unmarshal(params.Data, &d)
		cap := fmt.Sprintf("[%s] registers channel: %s (type: %s)", pluginName, d.Name, d.Type)
		p.mu.Lock()
		p.capabilities = append(p.capabilities, cap)
		p.mu.Unlock()

	case "image_generation_provider":
		var d struct{ Name string `json:"name"` }
		json.Unmarshal(params.Data, &d)
		p.mu.Lock()
		p.capabilities = append(p.capabilities, fmt.Sprintf("[%s] image generation provider: %s", pluginName, d.Name))
		p.mu.Unlock()

	case "web_fetch_provider":
		var d struct{ Name string `json:"name"` }
		json.Unmarshal(params.Data, &d)
		p.mu.Lock()
		p.capabilities = append(p.capabilities, fmt.Sprintf("[%s] web fetch provider: %s", pluginName, d.Name))
		p.mu.Unlock()

	case "web_search_provider":
		var d struct{ Name string `json:"name"` }
		json.Unmarshal(params.Data, &d)
		p.mu.Lock()
		p.capabilities = append(p.capabilities, fmt.Sprintf("[%s] web search provider: %s", pluginName, d.Name))
		p.mu.Unlock()

	default:
		var d struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		json.Unmarshal(params.Data, &d)
		cap := fmt.Sprintf("[%s] capability: %s", pluginName, params.Type)
		if d.Name != "" {
			cap += " (" + d.Name + ")"
		}
		p.mu.Lock()
		p.capabilities = append(p.capabilities, cap)
		p.mu.Unlock()
	}
}

func (p *Plugin) loadPySidecar(s *sdk.PluginSDK, dir, name string) error {
	simPath := filepath.Join(p.simulatorDir, "pysim.py")
	if err := os.MkdirAll(p.simulatorDir, 0755); err != nil {
		return fmt.Errorf("create simulator dir: %w", err)
	}
	if err := os.WriteFile(simPath, []byte(pySimulatorSrc), 0644); err != nil {
		return fmt.Errorf("write pysimulator: %w", err)
	}

	// 查找可用的 Python 解释器
	pythonBin := "python3"
	for _, candidate := range []string{"/usr/bin/python3", "/usr/local/bin/python3"} {
		if _, err := os.Stat(candidate); err == nil {
			pythonBin = candidate
			break
		}
	}

	sp, err := launchProcess(pythonBin, simPath, dir, name)
	if err != nil {
		return fmt.Errorf("launch pysimulator: %w", err)
	}
	if sp == nil {
		return nil
	}

	// 处理注册通知 (同 OC 插件流程)
	for done := false; !done; {
		select {
		case n := <-sp.NotifyChan():
			p.translateAndRegister(n, sp, s, name)
		default:
			done = true
		}
	}
	go p.notifyLoop(sp, s, name)

	tools, err := sp.ListTools()
	if err != nil {
		sp.Close()
		return fmt.Errorf("list tools: %w", err)
	}
	log.Printf("[openclaw] pysidecar %s registered %d tools", name, len(tools))

	p.mu.Lock()
	p.sidecars = append(p.sidecars, sp)
	p.mu.Unlock()
	return nil
}

func (p *Plugin) loadSidecar(s *sdk.PluginSDK, dir, name string) error {
	sp, err := launchSidecar(dir, name)
	if err != nil {
		return fmt.Errorf("launch: %w", err)
	}
	if sp == nil {
		return nil
	}

	tools, err := sp.ListTools()
	if err != nil {
		sp.Close()
		return fmt.Errorf("list tools: %w", err)
	}

	for _, tool := range tools {
		toolName := fmt.Sprintf("%s_%s", name, tool.Name)
		tDef := sdk.ToolDef{
			Name:        toolName,
			Description: fmt.Sprintf("[%s] %s", name, tool.Description),
			Parameters:  tool.InputSchema,
		}
		handler := func(sp *sidecarProcess, toolName string) sdk.ToolHandler {
			return func(args map[string]interface{}) (interface{}, error) {
				return sp.CallTool(toolName, args)
			}
		}(sp, tool.Name)
		if err := s.RegisterTool(toolName, tDef, handler); err != nil {
			log.Printf("[openclaw] register sidecar tool %s: %v", toolName, err)
			continue
		}
		log.Printf("[openclaw] registered sidecar tool: %s (from %s)", toolName, name)
	}

	p.mu.Lock()
	p.sidecars = append(p.sidecars, sp)
	p.mu.Unlock()
	log.Printf("[openclaw] sidecar %s started with %d tools", name, len(tools))
	return nil
}

func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sp := range p.sidecars {
		sp.Close()
	}
	p.sidecars = nil
	p.skills = nil
	p.manager = nil
	p.sdk = nil
	return nil
}

func errorResult(msg string) interface{} {
	return map[string]interface{}{
		"isError": true,
		"content": msg,
	}
}

// hasOCExtensions 检测 package.json 中是否有 openclaw.extensions 或 openclaw.runtimeExtensions
func hasOCExtensions(pkgPath string) bool {
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return false
	}
	var pkg struct {
		OpenClaw *struct {
			Extensions         interface{} `json:"extensions"`
			RuntimeExtensions  interface{} `json:"runtimeExtensions"`
		} `json:"openclaw"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	return pkg.OpenClaw != nil && (pkg.OpenClaw.Extensions != nil || pkg.OpenClaw.RuntimeExtensions != nil)
}
