package sdk

import (
	"fmt"
	"strings"

	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
)

type settingsImpl struct {
	pluginName string
	reg        *internalConfig.ConfigRegistry
}

func NewSettings(name string, reg *internalConfig.ConfigRegistry) SettingsAPI {
	return &settingsImpl{pluginName: name, reg: reg}
}

func (s *settingsImpl) Get(key string) (interface{}, error) {
	if s.reg == nil { return nil, nil }
	return s.reg.PluginConfig(s.pluginName).Get(key)
}
func (s *settingsImpl) Set(key string, value interface{}) error {
	if s.reg == nil { return nil }
	return s.reg.PluginConfig(s.pluginName).Set(key, value)
}
func (s *settingsImpl) List(prefix string) ([]string, error) {
	if s.reg == nil { return nil, nil }
	return s.reg.PluginConfig(s.pluginName).List(prefix)
}
func coreKey(key string) string {
	if strings.HasPrefix(key, "core.") {
		return key
	}
	return "core." + key
}

func (s *settingsImpl) GetCore(key string) (interface{}, error) {
	if s.reg == nil { return nil, nil }
	return s.reg.Get(coreKey(key))
}
func (s *settingsImpl) SetCore(key string, value interface{}) error {
	if s.reg == nil { return nil }
	return s.reg.Set(coreKey(key), value)
}
func (s *settingsImpl) ListCore(prefix string) ([]string, error) {
	if s.reg == nil { return nil, nil }
	p := coreKey(prefix)
	if p == "core." {
		p = "core."
	}
	return s.reg.List(p), nil
}
func (s *settingsImpl) GetPlugin(plugin, key string) (interface{}, error) {
	if s.reg == nil { return nil, nil }
	return s.reg.PluginConfig(plugin).Get(key)
}
func (s *settingsImpl) SetPlugin(plugin, key string, value interface{}) error {
	if s.reg == nil { return nil }
	return s.reg.PluginConfig(plugin).Set(key, value)
}
func (s *settingsImpl) ListPlugin(plugin, prefix string) ([]string, error) {
	if s.reg == nil { return nil, nil }
	return s.reg.PluginConfig(plugin).List(prefix)
}
func (s *settingsImpl) RegisterDef(def ConfigDef) {
	if s.reg == nil { return }
	s.reg.PluginConfig(s.pluginName).RegisterDef(internalConfig.ConfigDef{
		Key: def.Key, Type: def.Type, DisplayName: def.DisplayName, Description: def.Description,
		Category: def.Category, Options: def.Options,
		Default: stringifyDefault(def.Default),
	})
}
func (s *settingsImpl) Defs(prefix string) []*ConfigDef {
	if s.reg == nil { return nil }
	defs := s.reg.PluginConfig(s.pluginName).ListDefs(prefix)
	out := make([]*ConfigDef, len(defs))
	for i, d := range defs {
		// ConfigDef = pubsdk.ConfigDef (type alias), so direct conversion works
		cpy := ConfigDef{
			Key: d.Key, Type: d.Type, DisplayName: d.DisplayName, Description: d.Description,
			Category: d.Category, Options: d.Options,
		}
		out[i] = &cpy
	}
	return out
}
func (s *settingsImpl) Dump() map[string]interface{} {
	if s.reg == nil { return nil }
	return s.reg.Dump()
}
func (s *settingsImpl) Plugins() []string {
	if s.reg == nil { return nil }
	names := s.reg.ListPlugins()
	result := make([]string, 0, len(names)+1)
	result = append(result, "core")
	result = append(result, names...)
	return result
}

func stringifyDefault(v interface{}) string {
	if v == nil { return "" }
	switch x := v.(type) {
	case string: return x
	case bool:
		if x { return "true" }
		return "false"
	default: return fmt.Sprint(v)
	}
}

// Ensure settingsImpl satisfies SettingsAPI (pubsdk.SettingsAPI via type alias).
var _ SettingsAPI = (*settingsImpl)(nil)
