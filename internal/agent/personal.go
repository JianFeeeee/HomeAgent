package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// 会随时间腐坏的人格内容特征。现场：人格卡写死 v0.9.0 与早已删除的 C ABI v2，
// 实例被问版本时自述错误（v1.2.0 压测发现）。
var (
	personaVersionRe    = regexp.MustCompile(`\bv?\d+\.\d+\.\d+\b`)
	personaStalePhrases = []string{"C ABI v2", "plugin.so", "c-shared", "描述式索引", "引用计数式"}
)

// PersonaStaleHints 返回人格文本里会腐坏的内容（空 = 干净）。
// 供启动时告警：引导改用配置项 core.agent.personal_prompt（默认模板不含这些）。
func PersonaStaleHints(content string) []string {
	var out []string
	if m := personaVersionRe.FindAllString(content, -1); len(m) > 0 {
		out = append(out, fmt.Sprintf("版本号字面量 %v（版本应来自运行时快照）", m))
	}
	for _, p := range personaStalePhrases {
		if strings.Contains(content, p) {
			out = append(out, "可能已过期的说法: "+p)
		}
	}
	return out
}
