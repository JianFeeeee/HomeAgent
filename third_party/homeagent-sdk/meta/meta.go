// Package meta 收集 HomeAgent SDK 的全部元数据。
// 版本号应与核心 meta.Version 保持一致。
package meta

var (
	// Version 是 HomeAgent SDK 版本号。
	// 通过 `-ldflags="-X gitcode.com/JianFeeeee/homeagent-sdk/meta.Version=vX.Y.Z"` 注入。
	//
	// 版本号语义：**SDK 版本跟随核心的中版本，patch 位恒为 .0**。
	// 整条核心 1.1.x 线（1.1.0、1.1.1、1.1.7…）共用 SDK 1.1.0；
	// 只有核心进入 1.2.0 这种中版本跃迁时 SDK 才升到 1.2.0。
	// 这样插件开发者只需关心「我在为哪个中版本写插件」，
	// 不必跟着核心的每个 bugfix 换 SDK 依赖（见 核心仓 docs/git-branching.md §七）。
	//
	// 1.0.0：插件运行模型从 C ABI 动态库改为子进程 + 共享内存。
	//        公开 SDK 接口零改动，但产物形态变了（plugin.so → plugin.bin）。
	// 1.1.0：多模态贯通插件边界。**全部是新增，无签名变更**：
	//          - Triple.SentenceText / Triple.MediaDigests
	//          - Doc.MediaDigests / Doc.Attachments、MediaAttachment
	//          - TextEvent.Attachments
	//          - DocMemoryAPI.InsertWithMedia
	//          - IOInjector 的 InjectInputMedia / InjectInputMediaSync /
	//            InjectInterruptMedia；PluginSDK 补上缺失的 SetToolBlocks 包装
	//        同版修掉两处并发竞态（sdk/stress_test.go 的 -race 实证，不是理论风险）：
	//        PluginSDK 的 API 字段与 autoRestart 标志此前无锁，而写方
	//        （内核注入 API、插件 SetAutoRestart）与读方（插件后台 goroutine
	//        注入、内核 registry 读 AutoRestart）天然跨 goroutine。
	//        存量插件不需要改一行也不需要重编：新增方法由**插件调用、内核实现**，
	//        不调就不受影响。想用新字段的插件重编即可。
	//
	// 1.2.0：注入行为的记忆/裁剪标志位。**全部是新增，无签名变更**：
	//          - InjectOptions{NoMemory, ContextPolicy}
	//          - IOInjector 的六个 *Opts 变体（排队/中断/同步/带媒体各一对）
	//          - ChannelDef.ContextPolicy（顺带给 ChannelDef 补上 JSON tag：
	//            它要跨进程传给内核，而 Cleaner 是函数必须忽略；无 tag 时只能
	//            手写字段白名单，新增字段会被静默丢掉）
	//        语义：零值 InjectOptions 与旧的三参数方法完全等价（记入记忆 +
	//        不裁剪），因此存量插件不需要改一行也不需要重编。
	//        裁剪（ContextPolicy=prune）必须显式声明——它会归档丢弃低相关事件。
	//
	// ❗main 分支上此值是**下一个未发布中版本**；已发布的值看对应的
	// release/vX.Y.x 分支与 tag（见 核心仓 docs/git-branching.md §2.1 与 §七.1）。
	//
	// 现为 1.3.0：1.2.x 线已开（release/v1.2.x 承载 1.2.0），main 在积攒 1.3 的东西。
	// 1.3.0 本身没有任何 tag；SDK 1.2.0 的定版与 tag 随核心 v1.2.0 正式发布一起做（§七.3）。
	Version = "1.3.0"

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
	//
	// ⚠️ 1.1.0 新增的媒体接口需要核心 **1.1.1+**（更早的核心没有
	// doc.insertWithMedia / io.injectMedia* 这些 RPC，调用会返回 unknown method）。
	// 这里仍写 1.0.0，因为它是「SDK 能在其上运行」的下限；
	// 媒体接口是可选能力，不用就不受影响。
	//
	// ⚠️ 1.2.0 新增的注入标志位同理需要核心 **1.2.0+**：内核在 1.2.0 之前会
	// 忽略注入参数里的 no_memory/context_policy 字段（不会报错，但不生效）。
	// 想用这些标志位的插件应当要求核心 1.2.0+；不用就不受影响。
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
