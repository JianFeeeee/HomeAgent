package cabi

// ABI version constants
const (
	ABIVersion    = 1
	ABIVersionMin = 1
)

// Dispatch method IDs (mirrors the plugin side constants)
const (
	CoreRegisterTool          = 1
	CoreRegisterStage         = 2
	CoreRegisterOutputCh      = 3
	CoreRegisterPluginAPI     = 4
	CoreInjectText            = 5
	CoreInjectInterruptText   = 6
	CoreInjectTextNoMemory    = 7
	CoreSetAutoRestart        = 8
	CoreMemoryRecall          = 9
	CoreMemoryCommit          = 10
	CoreMemoryIntrospect      = 11
	CoreMemoryMerge           = 12
	CoreMemoryPurge           = 13
	CoreDocQuery              = 14
	CoreKnowledgeSearch       = 15
	CoreSettingsGet           = 16
	CoreSettingsSet           = 17
	CoreSettingsRegisterDef   = 18
	CoreLLMListSources        = 19
	CoreLLMSetSource          = 20
	CoreSocialGetPerson       = 21
	CoreSocialGetNetwork      = 22
	CoreSubscribe             = 23
	CoreUnsubscribe           = 24
	CoreFreeString            = 25
	CoreSettingsGetCore       = 26
	CoreSettingsSetCore       = 27
	CoreSettingsListCore      = 28
	CoreSettingsGetPlugin     = 29
	CoreSettingsSetPlugin     = 30
	CoreSettingsListPlugin    = 31
	CoreDocInsert             = 32
	CoreDocRemove             = 33
	CoreDocStats              = 34
	CoreKnowledgeAdd          = 35
	CoreKnowledgeList         = 36
	CoreLLMCurrentSource      = 37
	CoreSocialGetTrait        = 38
	CoreSocialGetRelations    = 39
	CoreSocialListPersons     = 40
	CoreTextMemoryAppend      = 41
	CoreSettingsList          = 42
	CoreSettingsDefs          = 43
	CoreSettingsDump          = 44
	CoreSettingsPlugins       = 45
)
