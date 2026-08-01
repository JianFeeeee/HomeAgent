package sdk

// SkillAPI provides access to the skill manager.
type SkillAPI interface {
	List() []*Skill
	Get(name string) *Skill
	Install(name, content string) error
	Uninstall(name string) error
	Toggle(name string, enabled bool) error
}

// Skill is a neutral description of an installed skill.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Author      string `json:"author,omitempty"`
	Entry       string `json:"entry,omitempty"`
	Source      string `json:"source,omitempty"`
	Enabled     bool   `json:"enabled"`
	RawContent  string `json:"-"`
}
