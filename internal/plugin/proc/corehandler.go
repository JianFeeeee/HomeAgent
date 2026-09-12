package proc

import (
	"context"
	"encoding/json"
	"fmt"

	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// coreHandler 把插件发来的 RPC 调用路由到内核 PluginSDK。
//
// 这是 cabi/loader.go 那 51 个 case 体的**整块平移**（§3.2）：
// 参数解析、调用、错误处理逻辑不变，只把「整数 method id + 三个 C 字符串槽」
// 换成「method 名 + 结构化 JSON 参数」。语义不变，回归风险最小。
//
// 平移带来的直接改善：
//   - 参数不再挤进 s1/s2/s3 + i1/i2 五个固定槽（C ABI 的形状约束）
//   - 不需要 CORE_FREE_STRING：跨进程各自 GC
//   - 错误可携带结构化信息，不只是一个字符串
type coreHandler struct {
	// sdk 是内核为该插件构建的 PluginSDK（与内置插件同一类型）。
	sdk CoreSDK
	// name 是插件名，用于工具归属推断与日志。
	name string

	// host 持有被全部插件共享的 StageContext 段与锁仲裁（§3.3/§3.7）。
	// ❗ 必须是"全部插件共享一个 Host"——每插件一段会退化成副本模型。
	host *Host

	// owner 是本插件在共享槽池里的身份。内核用它校验 arena.free 的归属，
	// 并在插件退出时 ReclaimOwner 回收残留槽。
	owner uint32

	// locks 是 host.locks 的引用，供 stage.lock/unlock 路由。
	locks *lockRegistry

	// invokeTool/invokeCleaner/invokeStageFn/invokeOutput 反向调用插件（内核 → 插件）。
	// 由 Plugin 注入，注册回调时用它们构造 handler。
	invokeTool    func(name string, args map[string]interface{}) (interface{}, error)
	invokeCleaner func(params CleanerInvokeParams) (CleanerInvokeResult, error)
	invokeStageFn func(ctx context.Context, stage string, seq uint64) error
	invokeOutput  func(channel string, args map[string]interface{}) (interface{}, error)

	// evtRing 是事件环的订阅接口（实现由 internal/plugin 提供，避免循环依赖）。
	evtRing EvtRingSubscriber

	// caps 是本插件被授予的能力集（§3.8 权限梯度）。
	// nil 或 unrestricted 时不限制——存量插件未声明 capabilities，
	// 若按最小权限处理会让它们静默降级。
	caps *capabilitySet
}

// EvtRingSubscriber 是事件环订阅接口，由 internal/plugin.EventRing 实现。
// proc 包不依赖 internal/plugin，通过接口解耦。
// EvtRingSubscribe 返回一个取消函数（与 Bus.Subscribe 约定一致）。
type EvtRingSubscriber interface {
	EvtRingSubscribe(types []pubsdk.EventType) func()
}

func (h *coreHandler) invokeStageWithCtx(ctx context.Context, stage string, seq uint64) error {
	if h.invokeStageFn == nil {
		return fmt.Errorf("插件 %s: stage 调用通道未就绪", h.name)
	}
	return h.invokeStageFn(ctx, stage, seq)
}

// CoreSDK 是 coreHandler 依赖的内核能力面。
//
// 定义为接口而非直接依赖 internal/sdk.PluginSDK，原因：
//  1. 避免 internal/plugin/proc → internal/sdk 的强耦合（后者已依赖 internal/plugin 的类型）
//  2. 单测可注入假实现，无需构造完整内核
//
// 方法集**刻意只包含外部插件应得的能力**——`Selftest`/`Supervisor`/`Tracker`/
// `Status`/`Adapter`/`Config`/`Tool`/`Indexer`/`OutputChan`/`Publish` 不在此列。
// 这正是把权限梯度从「C ABI 表达能力的意外产物」变成「显式声明并强制的策略」（§3.8）。
type CoreSDK interface {
	PluginName() string

	Settings() pubsdk.SettingsAPI
	Memory() pubsdk.MemoryAPI
	TextMemory() pubsdk.TextMemoryAPI
	DocMemory() pubsdk.DocMemoryAPI
	Knowledge() pubsdk.KnowledgeAPI
	LLM() pubsdk.LLMAPI
	Social() pubsdk.SocialAPI
	PluginMgr() pubsdk.PluginMgrAPI

	RegisterTool(name string, def pubsdk.ToolDef, handler pubsdk.ToolHandler) error
	RegisterStage(stage pubsdk.Stage, handler pubsdk.StageHandler, scope ...pubsdk.StageScope)
	RegisterPluginAPI(name string) error
	RegisterOutputChannel(name string, caps int, desc string, def pubsdk.ChannelDef, handler pubsdk.ToolHandler) error
	RegisterInputChannel(name string, def pubsdk.ChannelDef) error

	InjectText(source, channel, text string)
	InjectInterruptText(source, channel, text string)
	InjectTextNoMemory(source, channel, text string)
	InjectInputSync(source, channel, text string) string
	// 带媒体的注入：子进程插件也能主动发起一轮带图/音频的对话。
	InjectInputMedia(source, channel, text string, blocks []pubsdk.ContentBlock)
	InjectInputMediaSync(source, channel, text string, blocks []pubsdk.ContentBlock) string
	InjectInterruptMedia(source, channel, text string, blocks []pubsdk.ContentBlock)

	// 带标志位的注入：声明这一次注入是否记入记忆、是否据此裁剪上下文。
	// 上面的三参数方法是它们的零值糖。
	InjectTextOpts(source, channel, text string, opts pubsdk.InjectOptions)
	InjectInterruptTextOpts(source, channel, text string, opts pubsdk.InjectOptions)
	InjectInputSyncOpts(source, channel, text string, opts pubsdk.InjectOptions) string
	InjectInputMediaOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts pubsdk.InjectOptions)
	InjectInputMediaSyncOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts pubsdk.InjectOptions) string
	InjectInterruptMediaOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts pubsdk.InjectOptions)

	// SetToolBlocks 注入媒体块，内核在下一条 tool message 携带（§3.8）。
	SetToolBlocks(blocks []pubsdk.ContentBlock)

	SetAutoRestart(enabled bool)
}

// Handle 分派一次插件 → 内核的调用。
//
// 权限梯度在此强制（§3.8）：manifest 未声明的能力组被**明确拒绝**。
// 不静默忽略：C ABI 时代 case 23/24 返回成功但永远收不到事件
// （§1.3 的「给不了」而非「不给」），插件作者无从得知。
func (h *coreHandler) Handle(method string, params json.RawMessage) (interface{}, error) {
	if ok, cap := h.caps.allows(method); !ok {
		return nil, errCapabilityDenied(h.name, method, cap)
	}

	switch method {

	// ---- 注册面（原 case 1/2/3/4/46）----
	case MethodToolRegister:
		return h.toolRegister(params)
	case MethodStageRegister:
		return h.stageRegister(params)
	case MethodOutputRegister:
		return h.outputRegister(params)
	case MethodAPIRegister:
		var p struct {
			Name string `json:"name"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, h.sdk.RegisterPluginAPI(p.Name)
	case MethodInputRegister:
		var p struct {
			Name       string            `json:"name"`
			Def        pubsdk.ChannelDef `json:"def"`
			HasCleaner bool              `json:"has_cleaner"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if p.Name == "" {
			return nil, fmt.Errorf("input.register: 缺少 name")
		}
		cleaner, err := h.cleanerProxy(CleanerScopeInput, p.Name, p.HasCleaner)
		if err != nil {
			return nil, fmt.Errorf("input.register: %w", err)
		}
		if err := validateContextPolicy("input.register", p.Def.ContextPolicy); err != nil {
			return nil, err
		}
		// 整体传 p.Def（只是把函数型的 Cleaner 换成代理），不要手写字段白名单：
		// 白名单会让新增字段静默丢失。
		def := p.Def
		def.Cleaner = cleaner
		return nil, h.sdk.RegisterInputChannel(p.Name, def)

	// ---- IO 注入（原 case 5/6/7/47）----
	//
	// 注入标志位（no_memory / context_policy）由插件在调用点声明，默认
	// 记入记忆 + 不裁剪。策略值在入口校验：静默降级成 none 会让调用方
	// 以为自己声明的裁剪在生效。
	case MethodIOInjectText:
		var p injectParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectText", p.ContextPolicy); err != nil {
			return nil, err
		}
		h.sdk.InjectTextOpts(p.Source, p.Channel, h.resolveText(p), pubSdkInjectOpts(p.NoMemory, p.ContextPolicy, p.CleanerName, p.Priority))
		return nil, nil
	case MethodIOInjectInterrupt:
		var p injectParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectInterrupt", p.ContextPolicy); err != nil {
			return nil, err
		}
		h.sdk.InjectInterruptTextOpts(p.Source, p.Channel, h.resolveText(p), pubSdkInjectOpts(p.NoMemory, p.ContextPolicy, p.CleanerName, p.Priority))
		return nil, nil
	case MethodIOInjectTextNoMem:
		var p injectParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectTextNoMem", p.ContextPolicy); err != nil {
			return nil, err
		}
		// 旧 RPC 语义就是「不进记忆」，显式标志位只可能再叠上 context_policy。
		h.sdk.InjectTextOpts(p.Source, p.Channel, h.resolveText(p), pubSdkInjectOpts(true, p.ContextPolicy, p.CleanerName, p.Priority))
		return nil, nil
	case MethodIOInjectSync:
		var p injectParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectInputSync", p.ContextPolicy); err != nil {
			return nil, err
		}
		reply := h.sdk.InjectInputSyncOpts(p.Source, p.Channel, h.resolveText(p), pubSdkInjectOpts(p.NoMemory, p.ContextPolicy, p.CleanerName, p.Priority))
		return map[string]interface{}{"reply": reply}, nil

	case MethodIOInjectMedia:
		var p injectMediaParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectMedia", p.ContextPolicy); err != nil {
			return nil, err
		}
		blocks, err := h.resolveBlocks(p)
		if err != nil {
			return nil, err
		}
		h.sdk.InjectInputMediaOpts(p.Source, p.Channel, p.Text, blocks, pubSdkInjectOpts(p.NoMemory, p.ContextPolicy, p.CleanerName, p.Priority))
		return nil, nil

	case MethodIOInjectMediaSync:
		var p injectMediaParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectMediaSync", p.ContextPolicy); err != nil {
			return nil, err
		}
		blocks, err := h.resolveBlocks(p)
		if err != nil {
			return nil, err
		}
		reply := h.sdk.InjectInputMediaSyncOpts(p.Source, p.Channel, p.Text, blocks, pubSdkInjectOpts(p.NoMemory, p.ContextPolicy, p.CleanerName, p.Priority))
		return map[string]interface{}{"reply": reply}, nil

	case MethodIOInjectInterruptMedia:
		var p injectMediaParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := validateContextPolicy("io.injectInterruptMedia", p.ContextPolicy); err != nil {
			return nil, err
		}
		blocks, err := h.resolveBlocks(p)
		if err != nil {
			return nil, err
		}
		h.sdk.InjectInterruptMediaOpts(p.Source, p.Channel, p.Text, blocks, pubSdkInjectOpts(p.NoMemory, p.ContextPolicy, p.CleanerName, p.Priority))
		return nil, nil

	// ---- 生命周期（原 case 8）----
	case MethodLifecycleAutoRestart:
		var p struct {
			Enabled bool `json:"enabled"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		h.sdk.SetAutoRestart(p.Enabled)
		return nil, nil

	// ---- 图记忆（原 case 9/10/11/12/13）----
	case MethodMemoryRecall:
		mem := h.sdk.Memory()
		if mem == nil {
			return nil, errUnavailable("memory")
		}
		var p struct {
			Query []string `json:"query"`
			Depth int      `json:"depth"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		entities, relations, err := mem.Recall(p.Query, p.Depth)
		if err != nil {
			return nil, err
		}
		if entities == nil {
			entities = []pubsdk.Entity{}
		}
		if relations == nil {
			relations = []pubsdk.Relation{}
		}
		return map[string]interface{}{"entities": entities, "relations": relations}, nil

	case MethodMemoryCommit:
		mem := h.sdk.Memory()
		if mem == nil {
			return nil, errUnavailable("memory")
		}
		var p struct {
			Triples []pubsdk.Triple `json:"triples"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, mem.Commit(p.Triples)

	case MethodMemoryIntrospect:
		mem := h.sdk.Memory()
		if mem == nil {
			return nil, errUnavailable("memory")
		}
		return mem.Introspect()

	case MethodMemoryMerge:
		mem := h.sdk.Memory()
		if mem == nil {
			return nil, errUnavailable("memory")
		}
		var p struct {
			Source string `json:"source"`
			Target string `json:"target"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		n, err := mem.MergeEntities(p.Source, p.Target)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"merged": n}, nil

	case MethodMemoryPurge:
		mem := h.sdk.Memory()
		if mem == nil {
			return nil, errUnavailable("memory")
		}
		var p struct {
			Criteria map[string]string `json:"criteria"`
			Mode     string            `json:"mode"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if p.Mode == "" {
			p.Mode = "soft"
		}
		n, err := mem.Purge(p.Criteria, p.Mode)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"purged": n}, nil

	// ---- 文档记忆（原 case 14/32/33/34）----
	case MethodDocQuery:
		dm := h.sdk.DocMemory()
		if dm == nil {
			return nil, errUnavailable("doc memory")
		}
		var p struct {
			Text string `json:"text"`
			TopK int    `json:"top_k"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		docs := dm.Query(p.Text, p.TopK)
		if docs == nil {
			docs = []*pubsdk.Doc{}
		}
		return map[string]interface{}{"docs": docs}, nil

	case MethodDocInsert:
		dm := h.sdk.DocMemory()
		if dm == nil {
			return nil, errUnavailable("doc memory")
		}
		var p struct {
			Doc    *pubsdk.Doc `json:"doc,omitempty"`
			DocRef SharedRef   `json:"doc_ref,omitempty"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		// 文档全文可达几十 KB～数 MB，优先走共享内存。
		if err := h.resolveJSONRef(p.DocRef, &p.Doc); err != nil {
			return nil, err
		}
		if p.Doc == nil {
			return nil, fmt.Errorf("doc.insert: 缺少 doc 字段")
		}
		return nil, dm.Insert(p.Doc)

	case MethodDocInsertMedia:
		dm := h.sdk.DocMemory()
		if dm == nil {
			return nil, errUnavailable("doc memory")
		}
		var p struct {
			Doc         *pubsdk.Doc              `json:"doc,omitempty"`
			Attachments []pubsdk.MediaAttachment `json:"attachments,omitempty"`
			DocRef      SharedRef                `json:"doc_ref,omitempty"`
			AttachRef   SharedRef                `json:"attachments_ref,omitempty"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		// 文档正文 + 附件（含媒体二进制/data URL）都优先走共享内存。
		if err := h.resolveJSONRef(p.DocRef, &p.Doc); err != nil {
			return nil, err
		}
		if err := h.resolveJSONRef(p.AttachRef, &p.Attachments); err != nil {
			return nil, err
		}
		if p.Doc == nil {
			return nil, fmt.Errorf("doc.insertWithMedia: 缺少 doc 字段")
		}
		if err := dm.InsertWithMedia(p.Doc, p.Attachments); err != nil {
			return nil, err
		}
		// 回传内核补过的字段：ID 新建时才生成，Content 含内核补的媒体标记，
		// MediaDigests 是附件落盘后的完整 digest——插件靠它们后续引用同一份媒体。
		return map[string]interface{}{"doc": p.Doc}, nil

	case MethodDocRemove:
		dm := h.sdk.DocMemory()
		if dm == nil {
			return nil, errUnavailable("doc memory")
		}
		var p struct {
			ID string `json:"id"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		dm.Remove(p.ID)
		return nil, nil

	case MethodDocStats:
		dm := h.sdk.DocMemory()
		if dm == nil {
			return nil, errUnavailable("doc memory")
		}
		return dm.Stats(), nil

	// ---- 知识库（原 case 15/35/36）----
	case MethodKnowledgeSearch:
		kn := h.sdk.Knowledge()
		if kn == nil {
			return nil, errUnavailable("knowledge")
		}
		var p struct {
			Query string `json:"query"`
			TopK  int    `json:"top_k"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		results, err := kn.Search(p.Query, p.TopK)
		if err != nil {
			return nil, err
		}
		if results == nil {
			results = []*pubsdk.Knowledge{}
		}
		return map[string]interface{}{"results": results}, nil

	case MethodKnowledgeAdd:
		kn := h.sdk.Knowledge()
		if kn == nil {
			return nil, errUnavailable("knowledge")
		}
		var p struct {
			Name       string    `json:"name"`
			Content    string    `json:"content,omitempty"`
			ContentRef SharedRef `json:"content_ref,omitempty"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		// 知识正文可达数十 KB，优先走共享内存。内容是 JSON 字符串，
		// 所以从 ref 读出后需再解一层。
		if err := h.resolveJSONRef(p.ContentRef, &p.Content); err != nil {
			return nil, err
		}
		return nil, kn.Add(p.Name, p.Content)

	case MethodKnowledgeList:
		kn := h.sdk.Knowledge()
		if kn == nil {
			return nil, errUnavailable("knowledge")
		}
		names, err := kn.List()
		if err != nil {
			return nil, err
		}
		if names == nil {
			names = []string{}
		}
		return map[string]interface{}{"names": names}, nil

	// ---- 文本记忆（原 case 41）----
	case MethodTextMemoryAppend:
		tm := h.sdk.TextMemory()
		if tm == nil {
			return nil, errUnavailable("text memory")
		}
		var p struct {
			Event pubsdk.TextEvent `json:"event"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, tm.Append(p.Event)

	// ---- 设置（原 case 16/17/18/26~31/42~45/51）----
	case MethodSettingsGet, MethodSettingsSet, MethodSettingsRegisterDef,
		MethodSettingsGetCore, MethodSettingsSetCore, MethodSettingsListCore,
		MethodSettingsGetPlugin, MethodSettingsSetPlugin, MethodSettingsListPlugin,
		MethodSettingsList, MethodSettingsDefs, MethodSettingsDump,
		MethodSettingsPlugins, MethodSettingsDataDir:
		return h.settings(method, params)

	// ---- LLM 源（原 case 19/20/37）----
	case MethodLLMListSources:
		llm := h.sdk.LLM()
		if llm == nil {
			return nil, errUnavailable("llm")
		}
		sources := llm.ListSources()
		if sources == nil {
			sources = []string{}
		}
		return map[string]interface{}{"sources": sources}, nil
	case MethodLLMSetSource:
		llm := h.sdk.LLM()
		if llm == nil {
			return nil, errUnavailable("llm")
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, llm.SetSource(p.Name)
	case MethodLLMCurrentSource:
		llm := h.sdk.LLM()
		if llm == nil {
			return nil, errUnavailable("llm")
		}
		return map[string]interface{}{"source": llm.CurrentSource()}, nil

	// ---- 社交图（只读，原 case 21/22/38/39/40）----
	case MethodSocialGetPerson, MethodSocialGetNetwork, MethodSocialGetTrait,
		MethodSocialGetRelation, MethodSocialListPersons:
		return h.social(method, params)

	// ---- 插件管理（原 case 48/49/50）----
	case MethodPluginReloadOne:
		pm := h.sdk.PluginMgr()
		if pm == nil {
			return nil, errUnavailable("plugin manager")
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, pm.ReloadOne(p.Name)
	case MethodPluginListLoaded:
		pm := h.sdk.PluginMgr()
		if pm == nil {
			return nil, errUnavailable("plugin manager")
		}
		list := pm.ListLoadedPlugins()
		if list == nil {
			list = []string{}
		}
		return map[string]interface{}{"plugins": list}, nil
	case MethodPluginIsDisabled:
		pm := h.sdk.PluginMgr()
		if pm == nil {
			return nil, errUnavailable("plugin manager")
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return map[string]interface{}{"disabled": pm.IsPluginDisabled(p.Name)}, nil

	// ---- 共享段锁仲裁（新增，§3.7）----
	case MethodStageLock:
		if h.locks == nil {
			return nil, fmt.Errorf("stage.lock: 锁仲裁未就绪")
		}
		return nil, h.locks.acquire(h.name)
	case MethodStageUnlock:
		if h.locks == nil {
			return nil, fmt.Errorf("stage.unlock: 锁仲裁未就绪")
		}
		return nil, h.locks.release(h.name)

	// ---- 事件订阅（原 case 23/24，子进程下首次真正可用，§3.6）----
	case MethodEventsSubscribe:
		var p struct {
			Types []pubsdk.EventType `json:"types"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if h.evtRing == nil {
			return nil, fmt.Errorf("%s: 事件环未就绪", method)
		}
		// 订阅请求来自子进程——handler 直接注册到 Bus，
		// 事件经 EventRing 写入环后由子进程消费。
		h.evtRing.EvtRingSubscribe(p.Types)
		return nil, nil

	case MethodEventsUnsubscribe:
		// 事件环的订阅没有持久化句柄（取消函数由 Subscribe 返回但子进程未保存）。
		// 当前设计：子进程 Stop 时由内核统一清理其订阅。
		return nil, nil

	// ---- 共享槽池（内部传输层，见 protocol.go 注释）----
	case MethodArenaAlloc:
		var p ArenaAllocParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		ref, err := h.arenaAlloc(p.Size)
		if err != nil {
			return nil, err
		}
		return ArenaAllocResult{Ref: ref}, nil

	case MethodArenaFree:
		var p ArenaFreeParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, h.arenaFree(p.Ref)

	// ---- 多模态注入 ----
	//
	// 之前这里是桩：返回“待共享段二进制通道落地”。后果是**子进程插件调
	// SetToolBlocks 必然失败**（模板只 log 一行），只有内置插件能用。
	// 现在媒体块经共享内存传递，该能力对两种插件形态等价。
	case MethodIOSetToolBlocks:
		var p injectMediaParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		blocks, err := h.resolveBlocks(p)
		if err != nil {
			return nil, err
		}
		if len(blocks) == 0 {
			return nil, fmt.Errorf("%s: blocks 为空", method)
		}
		h.sdk.SetToolBlocks(blocks)
		return nil, nil
	}

	return nil, fmt.Errorf("未知 method: %s", method)
}

// resolveText 从 injectParams 中提取 text：优先使用 TextRef（共享槽），
// 否则使用内联 Text。兼容新旧两种协议。
//
// 子进程分配自己的槽并在收到应答后释放，因此这里读到的一定是 busy 槽。
func (h *coreHandler) resolveText(p injectParams) string {
	if !p.TextRef.IsZero() {
		data, err := h.host.Arena().Read(p.TextRef, h.host.Generation())
		if err != nil {
			return ""
		}
		return string(data)
	}
	return p.Text
}

type injectParams struct {
	Source        string    `json:"source"`
	Channel       string    `json:"channel"`
	Text          string    `json:"text,omitempty"`
	TextRef       SharedRef `json:"text_ref,omitempty"`
	NoMemory      bool      `json:"no_memory,omitempty"`
	ContextPolicy string    `json:"context_policy,omitempty"`
	CleanerName   string    `json:"cleaner_name,omitempty"`
	// Priority 声明中断注入的优先级（L1..L3）；L4 内核独占，见 InjectOptions。
	Priority string `json:"priority,omitempty"`
}

// injectMediaParams 是带媒体注入/工具块注入的参数。
//
// blocks 优先经共享内存传递（BlocksRef）。旧的注释说“data URL 已是 base64
// 文本、再套一层二进制不会更小，所以走 JSON”——那只算了体积，漏了两件更重要
// 的事：① 内联时整份 base64 要在 RPC 报文里再编码/再拷贝一遍（一张本地生图
// 可达数 MB），② 内容本体不在共享段里，插件回调就无法就地改写，只能各自
// 持一份拷贝。共享内存的意义是后者。
//
// 没有 BlocksRef 时（直连 RPC 测试、arena 不可用）回退内联 Blocks。
type injectMediaParams struct {
	Source        string                `json:"source"`
	Channel       string                `json:"channel"`
	Text          string                `json:"text,omitempty"`
	Blocks        []pubsdk.ContentBlock `json:"blocks,omitempty"`
	BlocksRef     SharedRef             `json:"blocks_ref,omitempty"`
	NoMemory      bool                  `json:"no_memory,omitempty"`
	ContextPolicy string                `json:"context_policy,omitempty"`
	CleanerName   string                `json:"cleaner_name,omitempty"`
	Priority      string                `json:"priority,omitempty"`
}

// pubSdkInjectOpts 把 RPC 报文里的三个字段转成公开 SDK 的 InjectOptions。
//
// 单独提一个转换函数是为了让「默认值」只有一个出处：零值即记入记忆 + 不裁剪，
// 与旧三参数注入等价。
func pubSdkInjectOpts(noMemory bool, policy, cleanerName, priority string) pubsdk.InjectOptions {
	return pubsdk.InjectOptions{
		NoMemory: noMemory, ContextPolicy: policy, CleanerName: cleanerName,
		Priority: clampExternalPriority(priority),
	}
}

// clampExternalPriority 把外部插件声明的优先级夹到 L1..L3。
//
// 走本桥的必然是外部插件（独立进程/动态库），它们**不是内核级插件**，
// 因此不能声明 L4——“立即打断”那类能力只属于编译期内置插件（如 WebUI 终止按钮）。
//
// 为什么在这里夹而不是只在内核里按 source 判：source 是插件自报的字段，
// 外部插件可以冒用 "webui" 之名；而本函数所在的位置能确知“这来自外部进程”。
// 内核侧的 isKernelLevelSource 是第二道闸（纵深防御）。
func clampExternalPriority(priority string) string {
	switch priority {
	case "L4", "l4":
		return pubsdk.PriorityL3
	default:
		return priority
	}
}

// validateContextPolicy 校验上下文策略取值，与 tool.register 同一套规则。
//
// 空串等价于 none（不裁剪）。非法值必须报错而不是当成 none：把拼写错误
// 静默降级成「不裁剪」会让调用方以为自己声明的裁剪在生效。
func validateContextPolicy(where, policy string) error {
	if !pubsdk.ValidContextPolicy(policy) {
		return fmt.Errorf("%s: context_policy 只允许 none/prune，实际 %q", where, policy)
	}
	return nil
}

// resolveJSONRef 若 ref 非零则从共享内存读取并 JSON 反序列化到 out；
// ref 为零时不动 out（调用方已填的内联值生效）。
//
// 供「大 payload 优先走共享内存、否则内联」的字段对共用。
func (h *coreHandler) resolveJSONRef(ref SharedRef, out interface{}) error {
	if ref.IsZero() {
		return nil
	}
	data, err := h.host.Arena().Read(ref, h.host.Generation())
	if err != nil {
		return fmt.Errorf("读取共享内容失败: %w", err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("解析共享内容失败: %w", err)
	}
	return nil
}

// resolveBlocks 取出媒体块：优先共享内存，否则内联。
func (h *coreHandler) resolveBlocks(p injectMediaParams) ([]pubsdk.ContentBlock, error) {
	if p.BlocksRef.IsZero() {
		return p.Blocks, nil
	}
	var blocks []pubsdk.ContentBlock
	if err := h.resolveJSONRef(p.BlocksRef, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func unmarshal(params json.RawMessage, out interface{}) error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, out); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	return nil
}

func errUnavailable(what string) error {
	return fmt.Errorf("%s 能力在当前内核实例中不可用", what)
}

// cleanerProxy 把进程内函数式 Cleaner 恢复成内核侧透明代理。
// RPC 失败时返回原文：清洗是计算层优化，不能因插件暂时离线而丢失内容。
func (h *coreHandler) cleanerProxy(scope, name string, enabled bool) (func(string) string, error) {
	if !enabled {
		return nil, nil
	}
	if h.invokeCleaner == nil {
		return nil, fmt.Errorf("%s %s 声明 Cleaner，但清洗回调通道未就绪", scope, name)
	}
	return func(text string) string {
		out, err := h.invokeCleanerText(scope, name, text)
		if err != nil {
			return text
		}
		return out
	}, nil
}

// invokeCleanerText 完成一次 Cleaner 往返，使用与工具调用相同的 funccall 帧模型。
//
// 内核（caller）标定帧：输入段 + 结果预算段，插件在帧内写结果；
// 只有结果超出预算时插件才向内核申请扩容块（插件只申请，回收由内核做）。
func (h *coreHandler) invokeCleanerText(scope, name, text string) (string, error) {
	arena := h.host.Arena()
	gen := h.host.Generation()

	frame, err := arena.Alloc(OwnerHost, len(text)+cleanerResultBudget, gen)
	if err != nil {
		return "", fmt.Errorf("%s %s Cleaner 分配调用帧失败: %w", scope, name, err)
	}
	defer func() { _ = arena.Free(OwnerHost, frame) }()

	area, err := arena.Read(frame, gen)
	if err != nil {
		return "", err
	}
	copy(area[:len(text)], text)

	params := CleanerInvokeParams{
		Scope:    scope,
		Name:     name,
		Frame:    frame,
		InputLen: uint32(len(text)),
	}

	res, err := h.invokeCleaner(params)
	if err != nil {
		return "", err
	}

	// 插件申请了扩容块：内核负责归还（插件只会申请，回收由内核做）。
	if res.TextRef.IsZero() {
		return "", fmt.Errorf("%s %s Cleaner 未返回结果引用", scope, name)
	}
	if res.TextRef.Flags&sharedRefFlagExpand != 0 {
		defer func() { _ = arena.Free(OwnerHost, res.TextRef) }()
	}
	data, err := arena.Read(res.TextRef, gen)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ---- 共享内存的 syscall 风格接口（插件 RPC）----
//
// 共享内存是内部实现，不向插件开发者暴露；模板运行时在传输层调用它们，
// 公开 SDK 仍是普通字符串/Map。

// arenaAlloc 给本插件分配一块共享内存。
//
// 用途：结果超出内核标定帧的预算时，插件据此申请扩容块。
func (h *coreHandler) arenaAlloc(size uint32) (SharedRef, error) {
	return h.host.Arena().Alloc(h.owner, int(size), h.host.Generation())
}

// arenaFree 归还本插件申请的块。
//
// 内核会走块链校验 offset 确实是某个已分配块的数据起点、owner 匹配，
// 伪造引用不能改动分配器状态。
func (h *coreHandler) arenaFree(ref SharedRef) error {
	return h.host.Arena().Free(h.owner, ref)
}

// toolRegister 注册插件工具，handler 反向调用插件执行（原 case 1）。
func (h *coreHandler) toolRegister(params json.RawMessage) (interface{}, error) {
	var p struct {
		Name       string         `json:"name"`
		Def        pubsdk.ToolDef `json:"def"`
		HasCleaner bool           `json:"has_cleaner"`
	}
	if err := unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.Name == "" {
		return nil, fmt.Errorf("tool.register: 缺少 name")
	}
	if err := validateContextPolicy("tool.register", p.Def.ContextPolicy); err != nil {
		return nil, err
	}
	p.Def.Plugin = h.name
	// 函数本身不进 JSON；has_cleaner 只声明其存在，实际执行回到插件进程。
	cleaner, err := h.cleanerProxy(CleanerScopeTool, p.Name, p.HasCleaner)
	if err != nil {
		return nil, fmt.Errorf("tool.register: %w", err)
	}
	p.Def.Cleaner = cleaner

	name := p.Name
	return nil, h.sdk.RegisterTool(name, p.Def, func(args map[string]interface{}) (interface{}, error) {
		return h.invokeTool(name, args)
	})
}

// stageRegister 注册阶段处理器（原 case 2）。
//
// **与 C ABI 路径的本质差异**：这里不做「快照 → 副本 → 写回」。
// StageContext 的数据在共享段，插件直接在同一份状态上读改写，
// 由锁仲裁串行化——消除了副本模型的 lost update（§8.4 实测 35.8~36.8%）。
func (h *coreHandler) stageRegister(params json.RawMessage) (interface{}, error) {
	var p struct {
		Stage string `json:"stage"`
		Scope string `json:"scope"`
	}
	if err := unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.Stage == "" {
		return nil, fmt.Errorf("stage.register: 缺少 stage")
	}

	scope := pubsdk.StageScopeGlobal
	if p.Scope == "own_tools" {
		scope = pubsdk.StageScopeOwnTools
	}
	stage := p.Stage
	h.sdk.RegisterStage(pubsdk.Stage(stage), func(sc *pubsdk.StageContext) error {
		return h.runStage(stage, sc)
	}, scope)
	return nil, nil
}

// outputRegister 注册输出通道（原 case 3）。
//
// **与 C ABI 路径的本质差异**：可同步等真实结果。
// C ABI 下因 cgo 不可嵌套，只能异步 fire-and-forget，导致 output_send
// 永远返回成功（§9.4，现网 2 次消息发不出而模型以为成功）。
func (h *coreHandler) outputRegister(params json.RawMessage) (interface{}, error) {
	var p struct {
		Name       string            `json:"name"`
		Caps       int               `json:"caps"`
		Desc       string            `json:"desc"`
		Def        pubsdk.ChannelDef `json:"def"`
		HasCleaner bool              `json:"has_cleaner"`
	}
	if err := unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.Name == "" {
		return nil, fmt.Errorf("output.register: 缺少 name")
	}
	channel := p.Name
	cleaner, err := h.cleanerProxy(CleanerScopeOutput, channel, p.HasCleaner)
	if err != nil {
		return nil, fmt.Errorf("output.register: %w", err)
	}
	return nil, h.sdk.RegisterOutputChannel(channel, p.Caps, p.Desc,
		pubsdk.ChannelDef{NoMemory: p.Def.NoMemory, Cleaner: cleaner},
		func(args map[string]interface{}) (interface{}, error) {
			return h.invokeOutput(channel, args)
		})
}

func (h *coreHandler) settings(method string, params json.RawMessage) (interface{}, error) {
	sett := h.sdk.Settings()
	if sett == nil {
		return nil, errUnavailable("settings")
	}
	var p struct {
		Key    string           `json:"key"`
		Value  interface{}      `json:"value"`
		Prefix string           `json:"prefix"`
		Plugin string           `json:"plugin"`
		Def    pubsdk.ConfigDef `json:"def"`
	}
	if err := unmarshal(params, &p); err != nil {
		return nil, err
	}

	switch method {
	case MethodSettingsGet:
		v, err := sett.Get(p.Key)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"value": v}, nil
	case MethodSettingsSet:
		return nil, sett.Set(p.Key, p.Value)
	case MethodSettingsRegisterDef:
		sett.RegisterDef(p.Def)
		return nil, nil
	case MethodSettingsGetCore:
		v, err := sett.GetCore(p.Key)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"value": v}, nil
	case MethodSettingsSetCore:
		return nil, sett.SetCore(p.Key, p.Value)
	case MethodSettingsListCore:
		keys, err := sett.ListCore(p.Prefix)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"keys": orEmpty(keys)}, nil
	case MethodSettingsGetPlugin:
		v, err := sett.GetPlugin(p.Plugin, p.Key)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"value": v}, nil
	case MethodSettingsSetPlugin:
		return nil, sett.SetPlugin(p.Plugin, p.Key, p.Value)
	case MethodSettingsListPlugin:
		keys, err := sett.ListPlugin(p.Plugin, p.Prefix)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"keys": orEmpty(keys)}, nil
	case MethodSettingsList:
		keys, err := sett.List(p.Prefix)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"keys": orEmpty(keys)}, nil
	case MethodSettingsDefs:
		defs := sett.Defs(p.Prefix)
		if defs == nil {
			defs = []*pubsdk.ConfigDef{}
		}
		return map[string]interface{}{"defs": defs}, nil
	case MethodSettingsDump:
		return sett.Dump(), nil
	case MethodSettingsPlugins:
		return map[string]interface{}{"plugins": orEmpty(sett.Plugins())}, nil
	case MethodSettingsDataDir:
		return map[string]interface{}{"dir": sett.DataDir()}, nil
	}
	return nil, fmt.Errorf("未知 settings method: %s", method)
}

func (h *coreHandler) social(method string, params json.RawMessage) (interface{}, error) {
	social := h.sdk.Social()
	if social == nil {
		return nil, errUnavailable("social")
	}
	var p struct {
		Name  string `json:"name"`
		Trait string `json:"trait"`
		Depth int    `json:"depth"`
	}
	if err := unmarshal(params, &p); err != nil {
		return nil, err
	}

	switch method {
	case MethodSocialGetPerson:
		profile, err := social.GetPerson(p.Name)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"person": profile}, nil
	case MethodSocialGetNetwork:
		profiles, err := social.GetNetwork(p.Name, p.Depth)
		if err != nil {
			return nil, err
		}
		if profiles == nil {
			profiles = []*pubsdk.PersonProfile{}
		}
		return map[string]interface{}{"network": profiles}, nil
	case MethodSocialGetTrait:
		v, ok := social.GetTrait(p.Name, p.Trait)
		return map[string]interface{}{"value": v, "found": ok}, nil
	case MethodSocialGetRelation:
		rels, err := social.GetRelations(p.Name)
		if err != nil {
			return nil, err
		}
		if rels == nil {
			rels = []pubsdk.SocialRelation{}
		}
		return map[string]interface{}{"relations": rels}, nil
	case MethodSocialListPersons:
		names, err := social.ListPersons()
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"persons": orEmpty(names)}, nil
	}
	return nil, fmt.Errorf("未知 social method: %s", method)
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
