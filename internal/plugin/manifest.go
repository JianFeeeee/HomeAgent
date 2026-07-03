package plugin

// PluginManifest 每个插件目录中的 plugin.json 元数据。
type PluginManifest struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Author      string `json:"author,omitempty"`
	Entry       string `json:"entry,omitempty"`  // "plugin.so" | "main.lua" | ""
	Deprecated  bool   `json:"deprecated,omitempty"`
}
