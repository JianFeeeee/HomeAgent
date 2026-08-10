package sdk

import pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"

// SettingsAPI 是内置插件使用的全量配置接口。
type SettingsAPI interface {
	pubsdk.SettingsAPI
	// DefsCore 返回核心配置表中匹配前缀的配置定义。
	DefsCore(prefix string) []*ConfigDef
	// DefsPlugin 返回另一个插件的配置定义。
	DefsPlugin(plugin, prefix string) []*ConfigDef
	// Remove 删除本插件配置中的单个键（插件删除时清理自身配置用）。
	Remove(key string) error
	// RemoveCore 删除核心配置表中的单个键。
	RemoveCore(key string) error
	// RemovePlugin 删除另一个插件配置表中的单个键。
	RemovePlugin(plugin, key string) error
}

type ConfigDef = pubsdk.ConfigDef
