// Package meta 收集 HomeAgent SDK 的全部元数据。
// 版本号应与核心 meta.Version 保持一致。
// ABI 版本与 Dispatch Method ID 应与核心仓 internal/meta/meta.go 保持一致。
package meta

var (
	// Version 是 HomeAgent SDK 版本号。
	// 通过 `-ldflags="-X gitcode.com/JianFeeeee/homeagent-sdk/meta.Version=vX.Y.Z"` 注入。
	Version = "0.9.0"

	// Commit 是构建时的 Git commit hash。
	Commit = "unknown"

	// BuildTime 是构建时间。
	BuildTime = "unknown"

	// SDKName 是 SDK 名称。
	SDKName = "HomeAgent SDK"

	// CoreModule 是核心仓的 Go module path，供 plugindev 生成 go.mod 时使用。
	CoreModule = "gitcode.com/JianFeeeee/HomeAgent"

	// CoreVersion 是此 SDK 所兼容的最低核心版本。
	CoreVersion = "0.9.0"
)

// FullVersion 返回完整的版本字符串。
func FullVersion() string {
	return SDKName + " v" + Version + " (" + Commit + ")"
}

// ---- ABI 版本（与核心仓 internal/meta/meta.go 同步） ----
// ABI 标识版本直接取内核版本号字符串（semver），与核心 Version 保持一致，不使用独立数字编码。
// 协商层（C 结构体 int version 字段）使用 CABINum：由版本字符串派生的整数（major*100 + minor）。
// 映射：v0.8.x → CABINum=800；v0.9.x → CABINum=900（invoke_stage 写回）。
// 小版本（patch）演进不影响 ABI，CABINum 不变。version_min 保证旧 ABI 插件仍可加载。

var (
	// ABIVersion 是 ABI 标识版本（字符串 semver，与 SDK CoreVersion 对齐）。
	ABIVersion = CoreVersion
	// ABIVersionMin 是兼容的最低 ABI 标识版本。
	ABIVersionMin = "0.8.0"
)

const (
	// CABINum 是 C 层协商用的整数版本（major*100 + minor），随 ABIVersion 派生。
	CABINum = 900
	// CABINumMin 是 C 层兼容的最低整数版本。
	// 旧工具链（v0.8 之前）写入的整数 version=1，无写回能力但与新内核结构兼容，
	// 因此最小值保持 1 以兼容全部旧插件（新插件 900 匹配，旧插件 1/2 通过）；
	// 仅当未来内核 ABI 破坏兼容时才提高该值。
	CABINumMin = 1
)

// ---- Dispatch Method IDs（与核心仓 internal/meta/meta.go 同步） ----
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
