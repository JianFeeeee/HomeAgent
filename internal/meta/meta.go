// Package meta 收集 HomeAgent 内核的全部元数据。
// 版本号通过 `go build -ldflags` 注入，默认值为 dev 版本。
// SDK 仓的 meta/meta.go 应与此保持同步。
package meta

var (
	// Version 是 HomeAgent 内核版本号。
	// 通过 `-ldflags="-X gitcode.com/JianFeeeee/HomeAgent/internal/meta.Version=vX.Y.Z"` 注入。
	//
	// 1.0.0：外部插件从 C ABI 动态库迁到子进程 + 共享内存。
	// 这是首个不再加载 `.so`/`.dll` 的版本，与 0.9.x 不兼容（存量插件必须
	// 用新版 plugindev 重编），故跃到主版本号。
	// 1.1.0：记忆系统支持二进制多媒体节点——CAS 媒体存储 + L0/L2/L3 贯通。
	// 1.1.1：多模态贯通**插件边界**。内核实现公开 SDK 1.1.0 新增的媒体接口
	//        （doc.insertWithMedia、io.injectMedia / injectMediaSync /
	//        injectInterruptMedia），并把 text/image/audio 三条输入路径归一成
	//        一条 processInput 主干。
	// 1.2.0：模型中立的多模态 provider SPI（pkg/embedding）——内核不再适配任何
	//        具体模型，Qwen 实现移到 providers/qwen3vl；插件运行协议升到 2
	//        （统一共享内存区，fd3 布局改变，不支持滚动升级）；并实现 SDK 1.2.0
	//        新增的注入行为标志位（InjectOptions：no_memory / context_policy）
	//        与 ChannelDef.ContextPolicy，使输入/排队注入/中断注入/同步注入都能
	//        声明「是否记入记忆」与「是否据此裁剪上下文」（默认都是否）。
	//
	// ❗main 上此值始终是**下一个未发布中版本**，不随 patch 发布变动
	//（见 docs/git-branching.md §2.1）；已发布的版本号看对应的 release/vX.Y.x 与 tag。
	Version = "1.2.0"

	// Commit 是构建时的 Git commit hash。
	Commit = "unknown"

	// BuildTime 是构建时间。
	BuildTime = "unknown"

	// KernelName 是内核名称。
	KernelName = "HomeAgent"

	// SDKCompatibleVersion 是此内核可兼容的最高 SDK 版本（semver）。
	//
	// 1.1.0：本内核实现了 SDK 1.1.0 的全部新增方法。
	// 1.2.0：本内核实现了 SDK 1.2.0 的全部新增方法（IOInjector 的六个 *Opts
	//        注入变体、InjectOptions、ChannelDef.ContextPolicy），因此声明为
	//        1.2.0。用 SDK 1.0.0/1.1.0 编的存量插件照旧可用——新增方法由
	//        **插件调用、内核实现**，不调就不受影响，无需重编。
	SDKCompatibleVersion = "1.2.0"
)

// FullVersion 返回完整的版本字符串。
func FullVersion() string {
	return KernelName + " v" + Version + " (" + Commit + ")"
}

// ---- 协议版本 ----
//
// 子进程 RPC 的协议版本是一个独立的小整数，与内核语义版本解耦：
// 语义版本变动频繁（修 bug、加字段），而 wire 协议只在**帧格式或握手语义**
// 变化时才升。当前值见 internal/plugin/proc/protocol.go 的 ProtocolVersion。
//
// C ABI 时代的 ABIVersion / CABINum / 51 个 Core<Method> 整数 ID 已随
// Part 6.2 删除 internal/plugin/cabi/ 一并退场：
//   - 整数 method id 平移为 method 名字符串（proc/protocol.go 的 Method* 常量）
//   - 版本协商改为握手帧里的 protocol 字段
//
// 保留那些常量只会让人以为它们还在生效。
