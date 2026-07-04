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

			for _, td := range sk.Tools() {
				if err := s.RegisterTool(td.Name, sdk.ToolDef{
					Name:        td.Name,
					Description: td.Description,
					Parameters:  td.Parameters,
				}, nil); err != nil {
					log.Printf("[openclaw] register tool %s: %v", td.Name, err)
				}
			}

			if iocfg := sk.IOConfig(); iocfg != nil {
				log.Printf("[openclaw] skill %s io: type=%s in=%s out=%s caps=%v",
					sk.Name(), iocfg.Type, iocfg.InputRoute, iocfg.OutputRoute, iocfg.OutputCaps)
			}

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

	// 收集插件注册过程中模拟器推送的通知
	p.drainNotify(sp, name)

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
			log.Printf("[openclaw] register ocplugin tool %s: %v", toolName, err)
			continue
		}
		log.Printf("[openclaw] registered ocplugin tool: %s (from %s)", toolName, name)
	}

	p.mu.Lock()
	p.sidecars = append(p.sidecars, sp)
	p.mu.Unlock()
	log.Printf("[openclaw] ocplugin %s started with %d tools", name, len(tools))
	return nil
}

func (p *Plugin) drainNotify(sp *sidecarProcess, name string) {
	for {
		select {
		case n := <-sp.NotifyChan():
			p.handleNotify(n, name)
		default:
			return
		}
	}
}

func (p *Plugin) handleNotify(n OCNotification, name string) {
	if n.Method != "register" {
		log.Printf("[openclaw] ocplugin %s: unknown notify method: %s", name, n.Method)
		return
	}
	var params struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(n.Params, &params); err != nil {
		log.Printf("[openclaw] ocplugin %s: bad notify params: %v", name, err)
		return
	}
	dataStr := string(params.Data)
	if len(dataStr) > 200 {
		dataStr = dataStr[:200] + "..."
	}
	log.Printf("[openclaw] ocplugin %s: capability %s data=%s", name, params.Type, dataStr)
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
