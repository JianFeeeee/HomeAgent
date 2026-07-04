package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const PackageExt = ".hmap"

// PluginManifest 每个插件目录中的 plugin.json 元数据。
type PluginManifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description,omitempty"`
	Author      string   `json:"author,omitempty"`
	License     string   `json:"license,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	Repository  string   `json:"repository,omitempty"`
	Entry       string   `json:"entry"` // "plugin.so" | "main.lua" | "SKILL.md"
	MinVersion  string   `json:"min_version,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Deprecated  bool     `json:"deprecated,omitempty"`
}

func ReadManifest(dir string) (*PluginManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return nil, err
	}
	var m PluginManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// IsPluginDir 判断目录是否为有效的插件目录（包含 plugin.json）
func IsPluginDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "plugin.json"))
	return err == nil
}
