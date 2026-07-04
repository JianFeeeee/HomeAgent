package openclaw

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

//go:embed simulator/main.js
var simulatorSrc string

var SkillsDir string
var SimulatorDir string

func init() {
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
	mu            sync.Mutex
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
	entries, err := os.ReadDir(p.skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read skills dir %s: %w", p.skillsDir, err)
	}

	for _, entry := range entries {
		skillPath := filepath.Join(p.skillsDir, entry.Name())
		subs, err := os.ReadDir(skillPath)
		if err != nil {
			continue
		}

		hasMainJS := false
		hasOCManifest := false
		hasOCPackage := false
		for _, f := range subs {
			switch f.Name() {
			case "main.js":
				hasMainJS = true
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
		case hasOCManifest || hasOCPackage:
			if err := p.loadOCPlugin(s, skillPath, entry.Name()); err != nil {
				log.Printf("[openclaw] ocplugin %s: %v", entry.Name(), err)
			}
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

	return nil
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
			Name:        d.Name,
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
		log.Printf("[openclaw] %s: provider registration (no HomeAgent equivalent, logged only)", pluginName)

	case "channel":
		log.Printf("[openclaw] %s: channel registration (no HomeAgent equivalent, logged only)", pluginName)

	default:
		log.Printf("[openclaw] %s: %s capability (no HomeAgent equivalent, logged only)", pluginName, params.Type)
	}
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
	return nil
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
