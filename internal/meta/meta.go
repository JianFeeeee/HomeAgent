// Package meta 收集 HomeAgent 内核的全部元数据。
// 版本号通过 `go build -ldflags` 注入，默认值为 dev 版本。
// 此文件是 ABI 版本号与 dispatch method ID 的唯一数据源。
// SDK 仓的 meta/meta.go 应与此保持同步。
package meta

var (
	// Version 是 HomeAgent 内核版本号。
	// 通过 `-ldflags="-X gitcode.com/JianFeeeee/HomeAgent/internal/meta.Version=vX.Y.Z"` 注入。
	Version = "0.7.2"

	// Commit 是构建时的 Git commit hash。
	Commit = "unknown"

	// BuildTime 是构建时间。
	BuildTime = "unknown"

	// KernelName 是内核名称。
	KernelName = "HomeAgent"

	// SDKCompatibleVersion 是此内核可兼容的最高 SDK 版本（semver）。
	SDKCompatibleVersion = "0.7.2"
)

// FullVersion 返回完整的版本字符串。
func FullVersion() string {
	return KernelName + " v" + Version + " (" + Commit + ")"
}

// ---- ABI 版本（C ABI 协议版本，插件与内核通信用） ----

const (
	ABIVersion    = 1
	ABIVersionMin = 1
)

// ---- Dispatch Method IDs ----
// 核心→插件：这些 ID 通过 CoreAPI.dispatch 传递，标识 SDK 调用。
// 插件端的 C enum 定义在 plugindev 的 C ABI header 模板中。
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
