package cabi

import "gitcode.com/JianFeeeee/HomeAgent/internal/meta"

// ABI version constants — single source of truth is meta.go
// ABIVersion/ABIVersionMin 是字符串 semver（var 转发，因 meta 侧 Version 为注入变量）；
// CABINum/CABINumMin 是 C 层整数协商版本。
var (
	ABIVersion    = meta.ABIVersion
	ABIVersionMin = meta.ABIVersionMin
)

const (
	CABINum    = meta.CABINum
	CABINumMin = meta.CABINumMin
)

// Dispatch method IDs — single source of truth is meta.go
const (
	CoreRegisterTool          = meta.CoreRegisterTool
	CoreRegisterStage         = meta.CoreRegisterStage
	CoreRegisterOutputCh      = meta.CoreRegisterOutputCh
	CoreRegisterPluginAPI     = meta.CoreRegisterPluginAPI
	CoreInjectText            = meta.CoreInjectText
	CoreInjectInterruptText   = meta.CoreInjectInterruptText
	CoreInjectTextNoMemory    = meta.CoreInjectTextNoMemory
	CoreSetAutoRestart        = meta.CoreSetAutoRestart
	CoreMemoryRecall          = meta.CoreMemoryRecall
	CoreMemoryCommit          = meta.CoreMemoryCommit
	CoreMemoryIntrospect      = meta.CoreMemoryIntrospect
	CoreMemoryMerge           = meta.CoreMemoryMerge
	CoreMemoryPurge           = meta.CoreMemoryPurge
	CoreDocQuery              = meta.CoreDocQuery
	CoreKnowledgeSearch       = meta.CoreKnowledgeSearch
	CoreSettingsGet           = meta.CoreSettingsGet
	CoreSettingsSet           = meta.CoreSettingsSet
	CoreSettingsRegisterDef   = meta.CoreSettingsRegisterDef
	CoreLLMListSources        = meta.CoreLLMListSources
	CoreLLMSetSource          = meta.CoreLLMSetSource
	CoreSocialGetPerson       = meta.CoreSocialGetPerson
	CoreSocialGetNetwork      = meta.CoreSocialGetNetwork
	CoreSubscribe             = meta.CoreSubscribe
	CoreUnsubscribe           = meta.CoreUnsubscribe
	CoreFreeString            = meta.CoreFreeString
	CoreSettingsGetCore       = meta.CoreSettingsGetCore
	CoreSettingsSetCore       = meta.CoreSettingsSetCore
	CoreSettingsListCore      = meta.CoreSettingsListCore
	CoreSettingsGetPlugin     = meta.CoreSettingsGetPlugin
	CoreSettingsSetPlugin     = meta.CoreSettingsSetPlugin
	CoreSettingsListPlugin    = meta.CoreSettingsListPlugin
	CoreDocInsert             = meta.CoreDocInsert
	CoreDocRemove             = meta.CoreDocRemove
	CoreDocStats              = meta.CoreDocStats
	CoreKnowledgeAdd          = meta.CoreKnowledgeAdd
	CoreKnowledgeList         = meta.CoreKnowledgeList
	CoreLLMCurrentSource      = meta.CoreLLMCurrentSource
	CoreSocialGetTrait        = meta.CoreSocialGetTrait
	CoreSocialGetRelations    = meta.CoreSocialGetRelations
	CoreSocialListPersons     = meta.CoreSocialListPersons
	CoreTextMemoryAppend      = meta.CoreTextMemoryAppend
	CoreSettingsList          = meta.CoreSettingsList
	CoreSettingsDefs          = meta.CoreSettingsDefs
	CoreSettingsDump          = meta.CoreSettingsDump
	CoreSettingsPlugins       = meta.CoreSettingsPlugins
	CoreRegisterInputCh       = meta.CoreRegisterInputCh
)
