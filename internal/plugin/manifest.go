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
	NameZh      string   `json:"name_zh,omitempty"`
	NameEn      string   `json:"name_en,omitempty"`
	Version     string   `json:"version"`
	Description string   `json:"description,omitempty"`
	Author      string   `json:"author,omitempty"`
	License     string   `json:"license,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	Repository  string   `json:"repository,omitempty"`
	Entry       string   `json:"entry"`               // "plugin.bin"(子进程) | "main.lua" | "SKILL.md"
	Platforms   []string `json:"platforms,omitempty"` // 声明的支持平台: ["linux","darwin","windows"]
	MinVersion  string   `json:"min_version,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Deprecated  bool     `json:"deprecated,omitempty"`

	// Capabilities 声明本插件需要的内核能力组（§3.8 权限梯度）。
	//
	// 取值见 internal/plugin/proc.KnownCapabilities()：
	// io / memory / doc_memory / knowledge / text_memory / llm / social /
	// events / plugin_mgr / settings_cross
	//
	// **省略或为空 = 不受限**，而不是「只有基础能力」。
	// 理由：17 个存量插件的 plugin.json 都没有这个字段，若空声明当作最小权限，
	// 它们会全部失去 IO 注入、记忆读写等能力而**静默降级**——
	// 违反「外部插件零改动」的硬约束。收紧的路径是让插件显式声明。
	//
	// core（注册自身工具/阶段/通道 + 读写自己的配置）无需声明，始终可用。
	Capabilities []string `json:"capabilities,omitempty"`
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
