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

	// locks 是 host.locks 的引用，供 stage.lock/unlock 路由。
	locks *lockRegistry

	// invokeTool/invokeStageFn/invokeOutput 反向调用插件（内核 → 插件）。
	// 由 Plugin 注入，注册回调时用它们构造 handler。
	invokeTool    func(name string, args map[string]interface{}) (interface{}, error)
	invokeStageFn func(ctx context.Context, stage string, seq uint64) error
	invokeOutput  func(channel string, args map[string]interface{}) (interface{}, error)

	// evtRing 是事件环的订阅接口（实现由 internal/plugin 提供，避免循环依赖）。
	evtRing EvtRingSubscriber
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

	SetAutoRestart(enabled bool)
}

// Handle 分派一次插件 → 内核的调用。
func (h *coreHandler) Handle(method string, params json.RawMessage) (interface{}, error) {
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
			Name string            `json:"name"`
			Def  pubsdk.ChannelDef `json:"def"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		// 注意 ChannelDef.Cleaner 是函数，无法跨进程传递（§3.5 回调型资源）。
		// NoMemory 可传；Cleaner 若插件需要，须在插件侧对文本预处理后再注入。
		return nil, h.sdk.RegisterInputChannel(p.Name, pubsdk.ChannelDef{NoMemory: p.Def.NoMemory})

	// ---- IO 注入（原 case 5/6/7/47）----
	case MethodIOInjectText:
		var p injectParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		h.sdk.InjectText(p.Source, p.Channel, p.Text)
		return nil, nil
	case MethodIOInjectInterrupt:
		var p injectParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		h.sdk.InjectInterruptText(p.Source, p.Channel, p.Text)
		return nil, nil
	case MethodIOInjectTextNoMem:
		var p injectParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		h.sdk.InjectTextNoMemory(p.Source, p.Channel, p.Text)
		return nil, nil
	case MethodIOInjectSync:
		var p injectParams
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return map[string]interface{}{"reply": h.sdk.InjectInputSync(p.Source, p.Channel, p.Text)}, nil

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
			Doc *pubsdk.Doc `json:"doc"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if p.Doc == nil {
			return nil, fmt.Errorf("doc.insert: 缺少 doc 字段")
		}
		return nil, dm.Insert(p.Doc)

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
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := unmarshal(params, &p); err != nil {
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

	// ---- 多模态注入（C ABI 侧空实现）----
	case MethodIOSetToolBlocks:
		// Part 4 扩展：二进制落 arena、Slice 描述符回传（§3.8）。
		return nil, fmt.Errorf("%s: 多模态注入待共享段二进制通道落地", method)
	}

	return nil, fmt.Errorf("未知 method: %s", method)
}

type injectParams struct {
	Source  string `json:"source"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
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

// toolRegister 注册插件工具，handler 反向调用插件执行（原 case 1）。
func (h *coreHandler) toolRegister(params json.RawMessage) (interface{}, error) {
	var p struct {
		Name string         `json:"name"`
		Def  pubsdk.ToolDef `json:"def"`
	}
	if err := unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.Name == "" {
		return nil, fmt.Errorf("tool.register: 缺少 name")
	}
	p.Def.Plugin = h.name
	// ToolDef.Cleaner 是函数，跨进程无法传递（§3.5）——与 C ABI 路径行为一致。
	p.Def.Cleaner = nil

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
		Name string            `json:"name"`
		Caps int               `json:"caps"`
		Desc string            `json:"desc"`
		Def  pubsdk.ChannelDef `json:"def"`
	}
	if err := unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.Name == "" {
		return nil, fmt.Errorf("output.register: 缺少 name")
	}
	channel := p.Name
	return nil, h.sdk.RegisterOutputChannel(channel, p.Caps, p.Desc,
		pubsdk.ChannelDef{NoMemory: p.Def.NoMemory},
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
