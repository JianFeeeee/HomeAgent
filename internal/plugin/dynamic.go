package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	soEntry   = "plugin.so"
	dllEntry  = "plugin.dll"
	luaEntry  = "main.lua"
	metaEntry = "plugin.json"
)

func readManifest(dir string) *PluginManifest {
	data, err := os.ReadFile(filepath.Join(dir, metaEntry))
	if err != nil {
		return nil
	}
	var m PluginManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return &m
}

var _ = json.Marshal
