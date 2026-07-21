package clawhubadapter

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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
	plugin.RegisterPluginMeta("clawhubadapter", "ClawHub 适配器", "ClawHub Adapter")
	plugin.RegisterFactory("clawhubadapter", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		dir := SkillsDir
		if dir == "" {
			dataDir, ok := config["data_dir"].(string)
			if !ok {
				return nil, fmt.Errorf("clawhubadapter plugin: config missing 'data_dir' or not a string")
			}
			dir = filepath.Join(dataDir, "skills")
		}
		return New(name, dir), nil
	})
}

type Plugin struct {
	name         string
	skillsDir    string
	simulatorDir string
	skills       []*plugin.SKILLPlugin
	sidecars     []*sidecarProcess
	manager      *sidecarProcess
	mu           sync.Mutex
	sdk          *sdk.PluginSDK
	dispatcher   *RegistryDispatcher
	httpClient   *http.Client
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
		dispatcher:   NewDispatcher(),
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
	p.httpClient = &http.Client{}
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
		log.Printf("[clawhubadapter] launch manager: %v", err)
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
					log.Printf("[clawhubadapter] sidecar %s: %v", entry.Name(), err)
				}
			case hasMainPy:
				if err := p.loadPySidecar(s, skillPath, entry.Name()); err != nil {
					log.Printf("[clawhubadapter] pysidecar %s: %v", entry.Name(), err)
				}
			case hasOCManifest || hasOCPackage:
				log.Printf("[clawhubadapter] ocplugin %s handled by manager", entry.Name())
			default:
				sk, err := plugin.LoadSKILL(skillPath)
				if err != nil {
					log.Printf("[clawhubadapter] load skill %s: %v", entry.Name(), err)
					continue
				}
				p.skills = append(p.skills, sk)
				log.Printf("[clawhubadapter] loaded skill: %s v%s", sk.Name(), sk.Version())
			}
		}
	}

	// Register plugin management tools that talk to the manager (always, even if skills dir is empty)
	tp := p.name + "_"
	s.RegisterTool(tp+"npm_install", sdk.ToolDef{
		Name:        tp + "npm_install",
		Description: "安装 ClawHub 适配器插件管理器中的插件。支持 npm: 前缀（npm 包）和 clawhub: 前缀（ClawHub 市场）。安装后立即可用。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"package": map[string]interface{}{"type": "string", "description": "插件包标识。npm:<pkg> 从 npm 安装，clawhub:<pkg> 从 ClawHub 市场安装（如 clawhub:openclaw-codex-app-server）"},
			},
			"required": []string{"package"},
		},
	}, p.handlePluginInstall)

	s.RegisterTool(tp+"npm_uninstall", sdk.ToolDef{
		Name:        tp + "npm_uninstall",
		Description: "从插件管理器中移除已安装的插件。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "要移除的插件名称"},
			},
			"required": []string{"name"},
		},
	}, p.handlePluginUninstall)

	s.RegisterTool(tp+"search", sdk.ToolDef{
		Name:        tp + "search",
		Description: "搜索 ClawHub 插件市场，查找可安装的插件。返回插件名称、描述和安装命令提示。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{"type": "string", "description": "搜索关键词"},
			},
			"required": []string{"query"},
		},
	}, p.handleClawHubSearch)

	s.RegisterTool(tp+"list", sdk.ToolDef{
		Name:        tp + "list",
		Description: "列出所有已安装的 ClawHub 插件及其工具。",
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

	log.Printf("[clawhubadapter] OC plugin manager started")
	return nil
}

// ─── 管理工具处理 ────────────────────────────────────────────

func (p *Plugin) handlePluginInstall(args map[string]interface{}) (interface{}, error) {
	pkg, _ := args["package"].(string)
	if pkg == "" {
		return errorResult("package is required"), nil
	}

	if strings.HasPrefix(pkg, "clawhub:") {
		return p.installFromClawHub(pkg)
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

const clawhubAPI = "https://clawhub.ai/api/v1"

func (p *Plugin) installFromClawHub(spec string) (interface{}, error) {
	name := strings.TrimPrefix(spec, "clawhub:")
	if name == "" {
		return errorResult("clawhub package name is required"), nil
	}

	pkgURL := fmt.Sprintf("%s/packages/%s/download", clawhubAPI, url.PathEscape(name))
	resp, err := p.httpClient.Get(pkgURL)
	if err != nil {
		return errorResult(fmt.Sprintf("download failed: %v", err)), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return errorResult(fmt.Sprintf("ClawHub API error (status %d): %s", resp.StatusCode, string(body))), nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorResult(fmt.Sprintf("read download: %v", err)), nil
	}

	extractDir := filepath.Join(p.skillsDir, name)
	os.MkdirAll(extractDir, 0755)

	if err := extractArchive(data, extractDir); err != nil {
		return errorResult(fmt.Sprintf("extract failed: %v", err)), nil
	}

	if err := p.reloadPlugin(name); err != nil {
		return errorResult(fmt.Sprintf("loaded but with warning: %v", err)), nil
	}

	return map[string]interface{}{
		"content": fmt.Sprintf("已从 ClawHub 安装插件: %s", name),
	}, nil
}

func (p *Plugin) handleClawHubSearch(args map[string]interface{}) (interface{}, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return errorResult("query is required"), nil
	}

	searchURL := fmt.Sprintf("%s/search?q=%s", clawhubAPI, url.QueryEscape(query))
	resp, err := p.httpClient.Get(searchURL)
	if err != nil {
		return errorResult(fmt.Sprintf("search failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Results []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"displayName"`
			Summary     string `json:"summary"`
			Version     string `json:"version"`
			Downloads   int    `json:"downloads"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return map[string]interface{}{
			"content": fmt.Sprintf("Search results (raw):\n%s", string(body)),
		}, nil
	}

	if len(result.Results) == 0 {
		return map[string]interface{}{
			"content": fmt.Sprintf("未找到匹配 \"%s\" 的 ClawHub 插件", query),
		}, nil
	}

	var lines []string
	for _, pkg := range result.Results {
		ver := pkg.Version
		if ver == "" {
			ver = "latest"
		}
		name := pkg.DisplayName
		if name == "" {
			name = pkg.Slug
		}
		lines = append(lines, fmt.Sprintf("- %s (%s) v%s | ⬇ %d\n  %s\n  安装: clawhubadapter_npm_install package=clawhub:%s",
			name, pkg.Slug, ver, pkg.Downloads, pkg.Summary, pkg.Slug))
	}

	return map[string]interface{}{
		"content": fmt.Sprintf("在 ClawHub 找到 %d 个插件:\n%s", len(result.Results), strings.Join(lines, "\n")),
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
			caps := p.dispatcher.Capabilities()
			if len(caps) > 0 {
				parts = append(parts, fmt.Sprintf("\nCapabilities (%d):", len(caps)))
				for _, c := range caps {
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

caps := p.dispatcher.Capabilities()
if len(caps) > 0 {
	parts = append(parts, fmt.Sprintf("\nCapabilities (%d):", len(caps)))
	for _, c := range caps {
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
	log.Printf("[clawhubadapter] ocplugin %s verified %d tools via ListTools", name, len(tools))

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
	p.dispatcher.Dispatch(params.Type, params.Data, pluginName, sp, s)
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
	log.Printf("[clawhubadapter] pysidecar %s registered %d tools", name, len(tools))

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
			log.Printf("[clawhubadapter] register sidecar tool %s: %v", toolName, err)
			continue
		}
		log.Printf("[clawhubadapter] registered sidecar tool: %s (from %s)", toolName, name)
	}

	p.mu.Lock()
	p.sidecars = append(p.sidecars, sp)
	p.mu.Unlock()
	log.Printf("[clawhubadapter] sidecar %s started with %d tools", name, len(tools))
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

func (p *Plugin) reloadPlugin(name string) error {
	pluginDir := filepath.Join(p.skillsDir, name)
	info, err := os.Stat(pluginDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("plugin dir not found: %s", pluginDir)
	}

	if p.sdk == nil {
		return fmt.Errorf("sdk not initialized")
	}

	subs, err := os.ReadDir(pluginDir)
	if err != nil {
		return err
	}

	hasMainJS := false
	hasMainPy := false
	hasOCManifest := false
	for _, f := range subs {
		switch f.Name() {
		case "main.js":
			hasMainJS = true
		case "main.py":
			hasMainPy = true
		case "openclaw.plugin.json":
			hasOCManifest = true
		}
	}

	os.MkdirAll(p.simulatorDir, 0755)

	switch {
	case hasMainJS:
		return p.loadSidecar(p.sdk, pluginDir, name)
	case hasMainPy:
		return p.loadPySidecar(p.sdk, pluginDir, name)
	case hasOCManifest:
		return p.loadOCPlugin(p.sdk, pluginDir, name)
	default:
		sk, err := plugin.LoadSKILL(pluginDir)
		if err != nil {
			return fmt.Errorf("load skill: %w", err)
		}
		p.mu.Lock()
		p.skills = append(p.skills, sk)
		p.mu.Unlock()
		return nil
	}
}

func extractArchive(data []byte, dest string) error {
	if len(data) < 4 {
		return fmt.Errorf("archive too small (%d bytes)", len(data))
	}

	if data[0] == 0x50 && data[1] == 0x4B && data[2] == 0x03 && data[3] == 0x04 {
		return extractZIP(data, dest)
	}

	return extractTGZ(data, dest)
}

func extractZIP(data []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("zip: %w", err)
	}
	for _, f := range zr.File {
		target := filepath.Join(dest, f.Name)
		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0755)
			continue
		}
		os.MkdirAll(filepath.Dir(target), 0755)
		r, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			r.Close()
			return err
		}
		_, err = io.Copy(out, r)
		r.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTGZ(data []byte, dest string) error {
	gzr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(dest, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0755)
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}
