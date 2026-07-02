package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

type Personality struct {
	Content string
	Path    string
}

func LoadPersonality(path string) (*Personality, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Personality{}, nil
		}
		return nil, fmt.Errorf("read personal.md: %w", err)
	}
	return &Personality{
		Content: string(data),
		Path:    path,
	}, nil
}

func SavePersonality(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create personality dir: %w", err)
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func (p *Personality) InjectPrompt() string {
	if p.Content == "" {
		return ""
	}
	return fmt.Sprintf("【人格设定】\n%s\n", p.Content)
}
