package sdk

import pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"

// SettingsAPI 是内置插件使用的全量配置接口。
type SettingsAPI interface {
	pubsdk.SettingsAPI
	// DefsCore 返回核心配置表中匹配前缀的配置定义。
	DefsCore(prefix string) []*ConfigDef
	// DefsPlugin 返回另一个插件的配置定义。
	DefsPlugin(plugin, prefix string) []*ConfigDef
}

type ConfigDef = pubsdk.ConfigDef
