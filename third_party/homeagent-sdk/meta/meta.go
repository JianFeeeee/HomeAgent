// Package meta 收集 HomeAgent SDK 的全部元数据。
// 版本号应与核心 meta.Version 保持一致。
package meta

var (
	// Version 是 HomeAgent SDK 版本号。
	// 通过 `-ldflags="-X gitcode.com/JianFeeeee/homeagent-sdk/meta.Version=vX.Y.Z"` 注入。
	//
	// 1.0.0：插件运行模型从 C ABI 动态库改为子进程 + 共享内存。
	// 公开 SDK 接口（sdk/ 目录）**零改动**——插件业务代码不需要改一行，
	// 但产物形态变了（plugin.so → plugin.bin），必须用新版 plugindev 重编。
	Version = "1.0.0"

	// Commit 是构建时的 Git commit hash。
	Commit = "unknown"

	// BuildTime 是构建时间。
	BuildTime = "unknown"

	// SDKName 是 SDK 名称。
	SDKName = "HomeAgent SDK"

	// CoreModule 是核心仓的 Go module path，供 plugindev 生成 go.mod 时使用。
	CoreModule = "gitcode.com/JianFeeeee/HomeAgent"

	// CoreVersion 是此 SDK 所兼容的最低核心版本。
	//
	// 1.0.0 是硬下限而非建议值：0.9.x 内核只会 dlopen `.so`，
	// 本版工具链产出的 `plugin.bin` 在旧内核上根本不会被识别。
	CoreVersion = "1.0.0"
)

// FullVersion 返回完整的版本字符串。
func FullVersion() string {
	return SDKName + " v" + Version + " (" + Commit + ")"
}

// ---- 协议版本 ----
//
// 子进程 RPC 的协议版本是一个独立的小整数，与 SDK/内核语义版本解耦：
// 语义版本变动频繁（修 bug、加字段），而 wire 协议只在**帧格式或握手语义**
// 变化时才升。当前值见核心仓 internal/plugin/proc/protocol.go 的 ProtocolVersion。
//
// C ABI 时代的 ABIVersion / CABINum / 51 个 Core<Method> 整数 ID 已随
// Part 6.2 删除 internal/plugin/cabi/ 一并退场：
//   - 整数 method id 平移为 method 名字符串（proc/protocol.go 的 Method* 常量）
//   - 版本协商改为握手帧里的 protocol 字段
//
// 保留那些常量只会让人以为它们还在生效。
