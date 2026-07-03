package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
	"gopkg.in/yaml.v3"
)

const DefaultConfigPath = "/etc/homeagent/config.yaml"

func DefaultConfig() types.Config {
	return types.Config{
		Daemon: types.DaemonConfig{
			ListenAddr:        ":8080",
			DataDir:           "/var/lib/homeagent",
			HeartbeatInterval: 15 * time.Second,
			CheckInterval:     30 * time.Second,
			LogLevel:          "info",
		},
		LLM: types.LLMConfig{
			Provider:    "deepseek",
			Model:       "deepseek-v4-flash",
			BaseURL:     "https://api.deepseek.com",
			APIKey:      "",
			Adapter:     "deepseek",
			Temperature: 0.7,
			MaxTokens:   4096,
			Sources: []types.LLMSource{
				{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", Adapter: "deepseek", AdapterPath: "adapters/deepseek.lua"},
				{Name: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o", Adapter: "openai", AdapterPath: "adapters/openai.lua"},
				{Name: "anthropic", BaseURL: "https://api.anthropic.com", Model: "claude-sonnet-4-20250514", Adapter: "anthropic", AdapterPath: "adapters/anthropic.lua"},
				{Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com", Model: "gemini-2.0-flash", Adapter: "gemini", AdapterPath: "adapters/gemini.lua"},
				{Name: "mistral", BaseURL: "https://api.mistral.ai", Model: "mistral-large-latest", Adapter: "mistral", AdapterPath: "adapters/mistral.lua"},
				{Name: "groq", BaseURL: "https://api.groq.com", Model: "llama3-70b-8192", Adapter: "groq", AdapterPath: "adapters/groq.lua"},
				{Name: "github", BaseURL: "https://models.inference.ai.azure.com", Model: "gpt-4o", Adapter: "github", AdapterPath: "adapters/github.lua"},
				{Name: "ollama", BaseURL: "http://localhost:11434", Model: "llama3", Adapter: "ollama", AdapterPath: "adapters/ollama.lua"},
			},
		},
		Defaults: types.AgentConfig{
			Image:  "homeagent/agent-base:latest",
			LLMEndpoints: []string{"https://api.openai.com/v1"},
			SnapshotPolicy: types.SnapshotPolicy{
				Interval:     10 * time.Minute,
				MaxSnapshots: 20,
				PreAction:    true,
				PostAction:   false,
			},
			RollbackPolicy: types.RollbackPolicy{
				MaxRetries:      3,
				HealthThreshold: types.HealthDown,
				CooldownPeriod:  30 * time.Second,
				AutoRollback:    true,
			},
			ResourceLimit: types.ResourceLimit{
				CPU:     "2",
				Memory:  "2g",
				Disk:    "10g",
				Network: true,
			},
			OpenClawEnabled: true,
		},
	}
}

func Load(path string) (*types.Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	return &cfg, nil
}

func Save(path string, cfg *types.Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}
