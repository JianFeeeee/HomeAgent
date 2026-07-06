package sdk

import (
	"fmt"
	sdkext "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
)

type ConfigDef = sdkext.ConfigDef
type SettingsAPI = sdkext.SettingsAPI

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
func (s *settingsImpl) GetCore(key string) (interface{}, error) {
	if s.reg == nil { return nil, nil }
	return s.reg.Get(key)
}
func (s *settingsImpl) SetCore(key string, value interface{}) error {
	if s.reg == nil { return nil }
	return s.reg.Set(key, value)
}
func (s *settingsImpl) ListCore(prefix string) ([]string, error) {
	if s.reg == nil { return nil, nil }
	return s.reg.List(prefix), nil
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
func (s *settingsImpl) RegisterDef(def sdkext.ConfigDef) {
	if s.reg == nil { return }
	s.reg.PluginConfig(s.pluginName).RegisterDef(internalConfig.ConfigDef{
		Key: def.Key, Type: def.Type, DisplayName: def.DisplayName, Description: def.Description,
		Category: def.Category, Options: def.Options,
		Default: stringifyDefault(def.Default),
	})
}
func (s *settingsImpl) Defs(prefix string) []*sdkext.ConfigDef {
	if s.reg == nil { return nil }
	defs := s.reg.PluginConfig(s.pluginName).ListDefs(prefix)
	out := make([]*sdkext.ConfigDef, len(defs))
	for i, d := range defs {
		cpy := sdkext.ConfigDef{
			Key: d.Key, Default: d.Default, Type: d.Type, DisplayName: d.DisplayName,
			Description: d.Description, Category: d.Category, Options: d.Options,
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
	keys := s.reg.List("config_")
	names := make([]string, 0, len(keys)+1)
	names = append(names, "core")
	for _, k := range keys {
		if len(k) > 7 { names = append(names, k[7:]) }
	}
	return names
}

func stringifyDefault(v interface{}) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x { return "true" }
		return "false"
	default:
		return fmt.Sprint(v)
	}
}
