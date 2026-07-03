package openclaw

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// SkillsDir 由 main.go 在 Load() 前设置，指向 SKILL.md 存放目录。
var SkillsDir string

func init() {
	plugin.RegisterFactory("openclaw", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		dir := SkillsDir
		if dir == "" {
			dir = filepath.Join(config["data_dir"].(string), "skills")
		}
		return New(name, dir), nil
	})
}

type Plugin struct {
	name    string
	skillsDir string
	skills  []*plugin.SKILLPlugin
}

func New(name, skillsDir string) *Plugin {
	return &Plugin{
		name:      name,
		skillsDir: skillsDir,
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
		sk, err := plugin.LoadSKILL(skillPath)
		if err != nil {
			log.Printf("[openclaw] load skill %s: %v", entry.Name(), err)
			continue
		}
		p.skills = append(p.skills, sk)

		// Register each tool defined in the SKILL
		for _, td := range sk.Tools() {
			name := td.Name
			def := sdk.ToolDef{
				Name:        name,
				Description: td.Description,
				Parameters:  td.Parameters,
			}
			// SKILL tools are informational (advisory) — no handler
			if err := s.RegisterTool(name, def, nil); err != nil {
				log.Printf("[openclaw] register tool %s: %v", name, err)
			}
		}

		// Register IO config as a channel if defined
		if iocfg := sk.IOConfig(); iocfg != nil {
			log.Printf("[openclaw] skill %s io: type=%s in=%s out=%s caps=%v",
				sk.Name(), iocfg.Type, iocfg.InputRoute, iocfg.OutputRoute, iocfg.OutputCaps)
		}

		log.Printf("[openclaw] loaded skill: %s v%s", sk.Name(), sk.Version())
	}

	return nil
}

func (p *Plugin) Stop() error {
	p.skills = nil
	return nil
}
