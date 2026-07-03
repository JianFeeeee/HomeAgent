package sdk

import (
	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
)

type SettingsAPI interface {
	// 插件自身配置表 config_<name>
	Get(key string) (interface{}, error)
	Set(key string, value interface{}) error
	List(prefix string) ([]string, error)

	// 核心配置表 config
	GetCore(key string) (interface{}, error)
	SetCore(key string, value interface{}) error
	ListCore(prefix string) ([]string, error)

	// 任意插件配置表 config_<plugin>
	GetPlugin(plugin, key string) (interface{}, error)
	SetPlugin(plugin, key string, value interface{}) error
	ListPlugin(plugin, prefix string) ([]string, error)

	// 全局
	Dump() map[string]interface{}
	Plugins() []string
}

type settingsImpl struct {
	pluginName string
	reg        *internalConfig.ConfigRegistry
}

func NewSettings(name string, reg *internalConfig.ConfigRegistry) SettingsAPI {
	return &settingsImpl{pluginName: name, reg: reg}
}

func (s *settingsImpl) Get(key string) (interface{}, error) {
	if s.reg == nil {
		return nil, nil
	}
	return s.reg.PluginConfig(s.pluginName).Get(key)
}

func (s *settingsImpl) Set(key string, value interface{}) error {
	if s.reg == nil {
		return nil
	}
	return s.reg.PluginConfig(s.pluginName).Set(key, value)
}

func (s *settingsImpl) List(prefix string) ([]string, error) {
	if s.reg == nil {
		return nil, nil
	}
	return s.reg.PluginConfig(s.pluginName).List(prefix)
}

func (s *settingsImpl) GetCore(key string) (interface{}, error) {
	if s.reg == nil {
		return nil, nil
	}
	return s.reg.Get(key)
}

func (s *settingsImpl) SetCore(key string, value interface{}) error {
	if s.reg == nil {
		return nil
	}
	return s.reg.Set(key, value)
}

func (s *settingsImpl) ListCore(prefix string) ([]string, error) {
	if s.reg == nil {
		return nil, nil
	}
	return s.reg.List(prefix), nil
}

func (s *settingsImpl) Dump() map[string]interface{} {
	if s.reg == nil {
		return nil
	}
	return s.reg.Dump()
}

func (s *settingsImpl) GetPlugin(plugin, key string) (interface{}, error) {
	if s.reg == nil {
		return nil, nil
	}
	return s.reg.PluginConfig(plugin).Get(key)
}

func (s *settingsImpl) SetPlugin(plugin, key string, value interface{}) error {
	if s.reg == nil {
		return nil
	}
	return s.reg.PluginConfig(plugin).Set(key, value)
}

func (s *settingsImpl) ListPlugin(plugin, prefix string) ([]string, error) {
	if s.reg == nil {
		return nil, nil
	}
	return s.reg.PluginConfig(plugin).List(prefix)
}

func (s *settingsImpl) Plugins() []string {
	if s.reg == nil {
		return nil
	}
	keys := s.reg.List("config_")
	names := make([]string, 0, len(keys)+1)
	names = append(names, "core")
	for _, k := range keys {
		// config_xxx → xxx
		if len(k) > 7 {
			names = append(names, k[7:])
		}
	}
	return names
}
