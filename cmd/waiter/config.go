package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Socket string `yaml:"socket"`
	Remote string `yaml:"remote"`
	APIKey string `yaml:"api_key"`
}

func discoverConfig(configPath string) *Config {
	if configPath != "" {
		if cfg := readFile(configPath); cfg != nil {
			return cfg
		}
	}

	candidates := configCandidates()
	for _, p := range candidates {
		if cfg := readFile(p); cfg != nil {
			return cfg
		}
	}

	return &Config{}
}

func configCandidates() []string {
	var cands []string

	home, _ := os.UserHomeDir()
	if home != "" {
		cands = append(cands, filepath.Join(home, ".config", "homeagent", "waiter.yaml"))
	}

	cands = append(cands, filepath.Join(".", "waiter.yaml"))

	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "waiter.yaml"))
	}

	return cands
}

func readFile(path string) *Config {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s: %v\n", path, err)
		return nil
	}
	return &cfg
}

func (c *Config) MergeCLI(socket, remote, apiKey string) {
	if socket != "" {
		c.Socket = socket
	}
	if remote != "" {
		c.Remote = remote
	}
	if apiKey != "" {
		c.APIKey = apiKey
	}
}
