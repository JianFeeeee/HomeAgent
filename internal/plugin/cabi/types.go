package cabi

// ABI version constants
const (
	ABIVersion    = 1
	ABIVersionMin = 1
)

// Dispatch method IDs (mirrors the plugin side constants)
const (
	CoreRegisterTool        = 1
	CoreRegisterStage       = 2
	CoreRegisterOutputCh    = 3
	CoreRegisterPluginAPI   = 4
	CoreInjectText          = 5
	CoreInjectInterruptText = 6
	CoreInjectTextNoMemory  = 7
	CoreSetAutoRestart      = 8
	CoreMemoryRecall        = 9
	CoreMemoryCommit        = 10
	CoreMemoryIntrospect    = 11
	CoreMemoryMerge         = 12
	CoreMemoryPurge         = 13
	CoreDocQuery            = 14
	CoreKnowledgeSearch     = 15
	CoreSettingsGet         = 16
	CoreSettingsSet         = 17
	CoreSettingsRegisterDef = 18
	CoreLLMListSources      = 19
	CoreLLMSetSource        = 20
	CoreSocialGetPerson     = 21
	CoreSocialGetNetwork    = 22
	CoreSubscribe           = 23
	CoreUnsubscribe         = 24
	CoreFreeString          = 25
)
