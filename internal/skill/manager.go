package skill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// Skill 已迁入内置 SDK，此处保留别名以兼容现有调用方。
type Skill = sdk.Skill

type Manager struct {
	mu          sync.RWMutex
	skillsDir   string
	skills      map[string]*Skill
}

func NewManager(skillsDir string) *Manager {
	return &Manager{
		skillsDir: skillsDir,
		skills:    make(map[string]*Skill),
	}
}

func (m *Manager) Init() error {
	if err := os.MkdirAll(m.skillsDir, 0755); err != nil {
		return fmt.Errorf("create skills dir: %w", err)
	}
	return m.loadAll()
}

func (m *Manager) loadAll() error {
	entries, err := os.ReadDir(m.skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillDir := filepath.Join(m.skillsDir, entry.Name())
		skill, err := m.loadSkill(skillDir)
		if err != nil {
			continue
		}
		m.skills[skill.Name] = skill
	}

	return nil
}

func (m *Manager) loadSkill(dir string) (*Skill, error) {
	skill := &Skill{
		Name:    filepath.Base(dir),
		Enabled: true,
	}

	skillFilePath := filepath.Join(dir, "SKILL.md")
	if data, err := os.ReadFile(skillFilePath); err == nil {
		skill.RawContent = string(data)
		skill.Description = extractDescription(skill.RawContent)
		skill.Version = extractField(skill.RawContent, "version")
		skill.Author = extractField(skill.RawContent, "author")
	}

	metaPath := filepath.Join(dir, "skill.json")
	if data, err := os.ReadFile(metaPath); err == nil {
		var meta struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Version     string `json:"version"`
			Author      string `json:"author"`
			Entry       string `json:"entry"`
		}
		if err := json.Unmarshal(data, &meta); err == nil {
			if meta.Name != "" {
				skill.Name = meta.Name
			}
			if meta.Description != "" {
				skill.Description = meta.Description
			}
			if meta.Version != "" {
				skill.Version = meta.Version
			}
			if meta.Author != "" {
				skill.Author = meta.Author
			}
			if meta.Entry != "" {
				skill.Entry = meta.Entry
			}
		}
	}

	return skill, nil
}

func (m *Manager) Install(name string, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	skillDir := filepath.Join(m.skillsDir, name)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}

	skill, err := m.loadSkill(skillDir)
	if err != nil {
		return fmt.Errorf("load installed skill: %w", err)
	}

	m.skills[name] = skill
	return nil
}

func (m *Manager) Uninstall(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.skills[name]; !ok {
		return fmt.Errorf("skill %s not found", name)
	}

	skillDir := filepath.Join(m.skillsDir, name)
	if err := os.RemoveAll(skillDir); err != nil {
		return fmt.Errorf("remove skill dir: %w", err)
	}

	delete(m.skills, name)
	return nil
}

func (m *Manager) List() []*Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()

	skills := make([]*Skill, 0, len(m.skills))
	for _, s := range m.skills {
		skills = append(skills, s)
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Name < skills[j].Name
	})

	return skills
}

func (m *Manager) Get(name string) *Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.skills[name]
}

func (m *Manager) Toggle(name string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.skills[name]; !ok {
		return fmt.Errorf("skill %s not found", name)
	}
	m.skills[name].Enabled = enabled
	return nil
}

func (m *Manager) GetInjectedPrompt() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var parts []string
	for _, s := range m.skills {
		if s.Enabled && s.RawContent != "" {
			parts = append(parts, fmt.Sprintf("=== Skill: %s ===\n%s", s.Name, s.RawContent))
		}
	}
	return strings.Join(parts, "\n\n")
}

// Manager 直接满足内置 SDK 的 SkillAPI（复用优先，无需独立适配器）。
var _ sdk.SkillAPI = (*Manager)(nil)

func extractDescription(content string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

func extractField(content string, field string) string {
	prefix := fmt.Sprintf("%s:", field)
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), prefix) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		}
	}
	return ""
}
