package proc

import "encoding/json"

// RPC 协议定义：控制面（§3.2 method id 平移为 method 名）。
//
// 帧格式：**换行分隔的 JSON**（NDJSON），双向复用同一对 stdio 管道。
//   内核 → 插件 stdin ：请求 / 响应
//   插件 → 内核 stdout：请求 / 响应
//
// 为什么不用 length-prefixed 二进制帧：工具调用结果中位数仅 93B（§2.5），
// JSON 序列化 3-8 µs 对比 LLM 单轮 2-8 秒占 0.0001%，可读性与可调试性更值。
// 大 payload（多媒体二进制）走共享内存 arena，不进 RPC 帧（§3.3 实验 10：18-22x）。

// 协议版本：与共享段版本独立演进。
// 插件握手时上报，内核校验——不匹配显式拒绝，避免半兼容导致的诡异行为。
const ProtocolVersion = 1

// Direction 无需显式字段：靠 Method 是否为空区分请求与响应
// （与 clawhubadapter/sidecar 的成熟做法一致）。

// Request 是一次 RPC 调用。
//
// ID 语义：
//   - ID > 0  ：需要响应，调用方在 pending 表等待
//   - ID == 0 ：通知（fire-and-forget），被调方不得回响应
//
// 通知用于事件投递等不关心结果的路径（§2.4 约束 B：内核发通知绝不等待消费者）。
type Request struct {
	ID     uint64          `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response 是对 Request 的应答。Error 非空表示失败。
type Response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// ---- kernel → plugin（内核调用插件，对应今日 7 个 //export）----
const (
	// MethodPluginInit 传插件名与配置，插件构造实例但不启动。
	MethodPluginInit = "plugin.init"
	// MethodPluginStart 插件注册工具/阶段/通道（其间会反向发起大量 core.* 调用）。
	MethodPluginStart = "plugin.start"
	// MethodPluginStop 优雅停止：插件侧先跑 RunStopHandlers 再 Stop()。
	MethodPluginStop = "plugin.stop"
	// MethodToolInvoke 执行插件工具。
	MethodToolInvoke = "tool.invoke"
	// MethodStageInvoke 执行阶段处理器。数据经共享段传递，参数只带阶段名与段世代号。
	MethodStageInvoke = "stage.invoke"
	// MethodOutputInvoke 经插件输出通道发送。
	MethodOutputInvoke = "output.invoke"
	// MethodHandshake 建链首帧：交换协议版本、SDK 版本、共享段规格。
	MethodHandshake = "handshake"
)

// ---- plugin → kernel（51 个 method id 平移，§3.2）----
//
// 编号本身扔掉：不再维护"下一个可用 id 是 52"，加能力不用改两边常量表，
// 也不再出现 47 夹在 7 和 8 之间的历史痕迹。
const (
	// 注册面（原 case 1/2/3/4/46）
	MethodToolRegister   = "tool.register"   // 1  CORE_REGISTER_TOOL
	MethodStageRegister  = "stage.register"  // 2  CORE_REGISTER_STAGE
	MethodOutputRegister = "output.register" // 3  CORE_REGISTER_OUTPUT_CH
	MethodAPIRegister    = "api.register"    // 4  CORE_REGISTER_PLUGIN_API
	MethodInputRegister  = "input.register"  // 46 CORE_REGISTER_INPUT_CH

	// IO 注入（原 case 5/6/7/47）
	MethodIOInjectText      = "io.injectText"      // 5  CORE_INJECT_TEXT
	MethodIOInjectInterrupt = "io.injectInterrupt" // 6  CORE_INJECT_INTERRUPT_TEXT
	MethodIOInjectTextNoMem = "io.injectTextNoMem" // 7  CORE_INJECT_TEXT_NO_MEMORY
	MethodIOInjectSync      = "io.injectInputSync" // 47 CORE_INJECT_INPUT_SYNC
	// 带媒体的注入：blocks 随参数 JSON 一并过来，内核侧转成
	// payload["media_blocks"]，由 resolveInput 归一进统一输入主干。
	// 与 SetToolBlocks 的区别：这三个是「主动发起一轮带图的对话」，
	// 后者是「工具返回值里带图」，只能在工具调用内部用。
	MethodIOInjectMedia          = "io.injectMedia"
	MethodIOInjectMediaSync      = "io.injectMediaSync"
	MethodIOInjectInterruptMedia = "io.injectInterruptMedia"
	// MethodIOSetToolBlocks 多模态注入——今日 C ABI 侧是空实现（§1.4），
	// 子进程下二进制落 arena、描述符回传，首次真正可用。
	MethodIOSetToolBlocks = "io.setToolBlocks"

	// 生命周期（原 case 8）
	MethodLifecycleAutoRestart = "lifecycle.autoRestart" // 8 CORE_SET_AUTO_RESTART

	// 图记忆（原 case 9/10/11/12/13）
	MethodMemoryRecall     = "memory.recall"     // 9
	MethodMemoryCommit     = "memory.commit"     // 10
	MethodMemoryIntrospect = "memory.introspect" // 11
	MethodMemoryMerge      = "memory.merge"      // 12
	MethodMemoryPurge      = "memory.purge"      // 13

	// 文档记忆（原 case 14/32/33/34）
	MethodDocQuery  = "doc.query"  // 14
	MethodDocInsert = "doc.insert" // 32
	MethodDocRemove = "doc.remove" // 33
	MethodDocStats  = "doc.stats"  // 34
	// MethodDocInsertMedia 写入文档并关联媒体（附件带 data 则落盘去重，
	// 只带 digest 则引用已有内容）。
	MethodDocInsertMedia = "doc.insertWithMedia"

	// 知识库（原 case 15/35/36）
	MethodKnowledgeSearch = "knowledge.search" // 15
	MethodKnowledgeAdd    = "knowledge.add"    // 35
	MethodKnowledgeList   = "knowledge.list"   // 36

	// 文本记忆（原 case 41）
	MethodTextMemoryAppend = "textmemory.append" // 41

	// 设置（原 case 16/17/18/26/27/28/29/30/31/42/43/44/45/51）
	MethodSettingsGet         = "settings.get"         // 16
	MethodSettingsSet         = "settings.set"         // 17
	MethodSettingsRegisterDef = "settings.registerDef" // 18
	MethodSettingsGetCore     = "settings.getCore"     // 26
	MethodSettingsSetCore     = "settings.setCore"     // 27
	MethodSettingsListCore    = "settings.listCore"    // 28
	MethodSettingsGetPlugin   = "settings.getPlugin"   // 29
	MethodSettingsSetPlugin   = "settings.setPlugin"   // 30
	MethodSettingsListPlugin  = "settings.listPlugin"  // 31
	MethodSettingsList        = "settings.list"        // 42
	MethodSettingsDefs        = "settings.defs"        // 43
	MethodSettingsDump        = "settings.dump"        // 44
	MethodSettingsPlugins     = "settings.plugins"     // 45
	MethodSettingsDataDir     = "settings.dataDir"     // 51

	// LLM 源（原 case 19/20/37）
	MethodLLMListSources   = "llm.listSources"   // 19
	MethodLLMSetSource     = "llm.setSource"     // 20
	MethodLLMCurrentSource = "llm.currentSource" // 37

	// 社交图（只读，原 case 21/22/38/39/40）
	MethodSocialGetPerson   = "social.getPerson"    // 21
	MethodSocialGetNetwork  = "social.getNetwork"   // 22
	MethodSocialGetTrait    = "social.getTrait"     // 38
	MethodSocialGetRelation = "social.getRelations" // 39
	MethodSocialListPersons = "social.listPersons"  // 40

	// 事件（原 case 23/24 —— 今日均为空实现「给不了」，
	// 子进程下经事件环 + eventfd 首次真正可用，见 §3.6/§3.8）
	MethodEventsSubscribe   = "events.subscribe"   // 23
	MethodEventsUnsubscribe = "events.unsubscribe" // 24

	// 插件管理（原 case 48/49/50）
	MethodPluginReloadOne  = "plugin.reloadOne"  // 48
	MethodPluginListLoaded = "plugin.listLoaded" // 49
	MethodPluginIsDisabled = "plugin.isDisabled" // 50

	// 共享段锁仲裁（新增，无对应 method id —— C ABI 下不存在跨进程锁概念）
	MethodStageLock   = "stage.lock"
	MethodStageUnlock = "stage.unlock"
)

// 原 case 25（CORE_FREE_STRING）无对应 RPC method：
// C ABI 下需要显式释放跨边界字符串，进程模型下由各自 GC 管理，概念消失。

// HandshakeParams 是内核 → 插件的建链首帧：告知内核侧规格。
type HandshakeParams struct {
	Protocol    int    `json:"protocol"`     // 内核支持的协议版本
	CoreVersion string `json:"core_version"` // 内核版本（诊断用）
	PluginName  string `json:"plugin_name"`  // 内核分配的插件名
	ShmVersion  uint32 `json:"shm_version"`
	ShmSize     int    `json:"shm_size"`
	// EvtRingSize 是事件环段大小（0 表示不支持事件环）。插件据此 mmap fd 4。
	EvtRingSize int `json:"evt_ring_size,omitempty"`
}

// HandshakeResult 是插件 → 内核的建链应答：上报自身信息。
type HandshakeResult struct {
	Protocol   int    `json:"protocol"`    // 必须等于 ProtocolVersion
	SDKVersion string `json:"sdk_version"` // 插件编译时链接的公开 SDK 版本
	PluginName string `json:"plugin_name"`
	PID        int    `json:"pid"`
}

// StageInvokeParams 是 stage.invoke 的参数。
//
// **注意：不含 StageContext 数据本身**——数据在共享段，此处只带定位信息。
// 这是共享内存数据面的意义：并发改写同一份状态，而非各持副本
// （副本模型实测 35.8~36.8% lost update，§8.4）。
type StageInvokeParams struct {
	Stage string `json:"stage"`
	// Seq 是内核写入共享段后的世代号，插件读到的 seq 应 >= 此值。
	Seq uint64 `json:"seq"`
}

// StageInvokeResult 是插件执行 stage 后的应答。
type StageInvokeResult struct {
	// DirtyFields 是插件实际写回共享段的字段数，0 表示只读插件。
	// 内核据此判断是否需要重读共享段，也用于诊断"谁改了什么"。
	DirtyFields int `json:"dirty_fields"`
	// Seq 是插件写回后的世代号。
	Seq uint64 `json:"seq"`
}

// ToolInvokeParams / ToolInvokeResult：工具调用（原 go_invoke_tool）。
type ToolInvokeParams struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args,omitempty"`
}

type ToolInvokeResult struct {
	Result interface{} `json:"result,omitempty"`
}

// OutputInvokeParams：输出通道发送（原 go_invoke_output）。
//
// 与 C ABI 路径的关键差异：**可同步等待真实结果**。
// C ABI 下因 cgo 不可嵌套，只能异步 fire-and-forget，导致 output_send
// 永远返回成功（§9.4，现网 2 次消息发不出而模型以为成功）。
// 进程模型下 RPC 天然可等应答，该缺陷从根上消失。
type OutputInvokeParams struct {
	Channel string                 `json:"channel"`
	Args    map[string]interface{} `json:"args,omitempty"`
}

// PluginInitParams：插件构造参数（原 case init_plugin）。
type PluginInitParams struct {
	Name   string                 `json:"name"`
	Config map[string]interface{} `json:"config,omitempty"`
}
