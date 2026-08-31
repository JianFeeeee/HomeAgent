//go:build linux || darwin

package cabi

/*
#cgo LDFLAGS: -ldl
#include <stdlib.h>

// HOMEAGENT_ABI_VERSION 是当前内核的 C ABI 整数协商版本，由 internal/meta/meta.go CABINum 派生
// （major*100 + minor，随核心版本号映射：v0.8.x→800，v0.9.x→900）。
// 旧插件使用低整数版本不受影响——C ABI wrapper 通过 version/version_min 字段协商兼容。
#define HOMEAGENT_ABI_VERSION 900

// PluginAPI — provided by the plugin via plugin_init()
typedef struct {
    int version; int version_min;
    int (*init_plugin)(char*, char*, char**);
    int (*start_plugin)(void*, int, char**);
    int (*stop_plugin)(char**);
    int (*invoke_tool)(char*, char*, char**, char**);
    int (*invoke_stage)(char*, char*, char**, char**);
    int (*invoke_output)(char*, char*, char*, char**);
    void (*free_string)(char*);
} plugin_api_t;

// CoreAPI — provided by the core via start_plugin()
typedef struct {
    int version; int version_min;
    int (*dispatch)(int, void*, char*, char*, char*, int, int, char**, char**);
    void* ctx;
} core_api_t;

// Functions implemented in loader.c
extern core_api_t* make_core_api(void);
extern void free_core_api(core_api_t* api);
extern int call_init_plugin(plugin_api_t*, char*, char*, char**);
extern int call_start_plugin(plugin_api_t*, void*, int, char**);
extern int call_stop_plugin(plugin_api_t*, char**);
extern int call_invoke_tool(plugin_api_t*, char*, char*, char**, char**);
extern int call_invoke_stage(plugin_api_t*, char*, char*, char**, char**);
extern int call_invoke_output(plugin_api_t*, char*, char*, char*, char**);
extern void api_free_string(plugin_api_t*, char*);
extern void* lib_open(const char*);
extern plugin_api_t* lib_get_api(void*);
extern void lib_close(void*);
extern char* lib_err(void);
*/
import "C"
import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// outputSendTimeout 是 output_send 等待通道真实发送确认的超时。
// 超过该时间仍未收到插件确认，返回 unconfirmed（结果未知）而非谎报成功。
// （plan.md 11.1）
const outputSendTimeout = 10 * time.Second

var (
	pluginMap sync.Map // int32 pluginID → *pluginState
	nextID    int32
)

type pluginState struct {
	id   int32
	name string
	sdk  *sdk.PluginSDK
	api  *C.plugin_api_t
}

// Handle represents a loaded C ABI plugin.
type Handle struct {
	soPath string
	lib    unsafe.Pointer
	api    *C.plugin_api_t
	core   *C.core_api_t
	pstate *pluginState
}

// Load opens a .so plugin and initializes it via C ABI.
func Load(soPath, name string, config map[string]interface{}) (*Handle, error) {
	cPath := C.CString(soPath)
	defer C.free(unsafe.Pointer(cPath))

	lib := C.lib_open(cPath)
	if lib == nil {
		return nil, fmt.Errorf("dlopen %s: %s", soPath, C.GoString(C.lib_err()))
	}

	api := C.lib_get_api(lib)
	if api == nil {
		C.lib_close(lib)
		return nil, fmt.Errorf("dlsym plugin_init in %s: %s", soPath, C.GoString(C.lib_err()))
	}
	if int(api.version) < 1 || api.init_plugin == nil {
		C.lib_close(lib)
		return nil, fmt.Errorf("plugin %s: invalid PluginAPI (version=%d)", name, int(api.version))
	}
	if int(api.version) > CABINum {
		C.lib_close(lib)
		return nil, fmt.Errorf("plugin %s: ABI version %d > core %d (v%s), requires newer HomeAgent core", name, int(api.version), CABINum, ABIVersion)
	}

	if int(api.version) < CABINumMin {
		C.lib_close(lib)
		return nil, fmt.Errorf("plugin %s: ABI version %d < core min %d (v%s), plugin too old", name, int(api.version), CABINumMin, ABIVersionMin)
	}

	id := atomic.AddInt32(&nextID, 1)
	ps := &pluginState{id: id, name: name, api: api}
	pluginMap.Store(id, ps)

	handle := &Handle{soPath: soPath, lib: lib, api: api, pstate: ps}

	// Create CoreAPI later — done via CreateCoreAPI

	// Initialize plugin
	configJSON, _ := json.Marshal(config)
	cName := C.CString(name)
	cConfig := C.CString(string(configJSON))
	var initErr *C.char
	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cConfig))

	if ret := int(C.call_init_plugin(api, cName, cConfig, &initErr)); ret != 0 {
		errMsg := ""
		if initErr != nil {
			errMsg = C.GoString(initErr)
			C.api_free_string(api, initErr)
		}
		handle.Close()
		return nil, fmt.Errorf("init_plugin %s: %s", name, errMsg)
	}

	return handle, nil
}

// CreateCoreAPI creates a CoreAPI struct for this plugin.
// The CoreAPI dispatches all SDK calls back to Go, routing to the plugin's PluginSDK.
func (h *Handle) CreateCoreAPI(s *sdk.PluginSDK) unsafe.Pointer {
	core := C.make_core_api()
	if core == nil {
		return nil
	}
	h.core = core
	h.pstate.sdk = s

	// Store plugin ID as context (safe integer, not a Go pointer)
	core.ctx = unsafe.Pointer(uintptr(h.pstate.id))

	return unsafe.Pointer(core)
}

// FreeCoreAPI frees the CoreAPI struct.
func (h *Handle) FreeCoreAPI() {
	if h.core != nil {
		C.free_core_api(h.core)
		h.core = nil
	}
}

// Start calls the plugin's Start with a CoreAPI pointer.
func (h *Handle) Start(corePtr unsafe.Pointer) error {
	var cErr *C.char
	if ret := int(C.call_start_plugin(h.api, corePtr, C.int(CABINum), &cErr)); ret != 0 {
		errMsg := ""
		if cErr != nil {
			errMsg = C.GoString(cErr)
			C.api_free_string(h.api, cErr)
		}
		return fmt.Errorf("start_plugin: %s", errMsg)
	}
	return nil
}

// Stop calls the plugin's Stop.
func (h *Handle) Stop() error {
	var cErr *C.char
	if ret := int(C.call_stop_plugin(h.api, &cErr)); ret != 0 {
		errMsg := ""
		if cErr != nil {
			errMsg = C.GoString(cErr)
			C.api_free_string(h.api, cErr)
		}
		return fmt.Errorf("stop_plugin: %s", errMsg)
	}
	return nil
}

// InvokeTool calls a tool handler in the plugin.
func (h *Handle) InvokeTool(name string, args map[string]interface{}) (map[string]interface{}, error) {
	argsJSON, _ := json.Marshal(args)
	cName := C.CString(name)
	cArgs := C.CString(string(argsJSON))
	var result, cErr *C.char
	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cArgs))

	if ret := int(C.call_invoke_tool(h.api, cName, cArgs, &result, &cErr)); ret != 0 {
		errMsg := ""
		if cErr != nil {
			errMsg = C.GoString(cErr)
			C.api_free_string(h.api, cErr)
		}
		return nil, fmt.Errorf("invoke_tool %s: %s", name, errMsg)
	}
	if result == nil {
		return nil, nil
	}
	defer C.api_free_string(h.api, result)
	var r map[string]interface{}
	if err := json.Unmarshal([]byte(C.GoString(result)), &r); err != nil {
		return nil, err
	}
	return r, nil
}

// Close unloads the plugin library.
func (h *Handle) Close() {
	if h.lib != nil {
		C.lib_close(h.lib)
		h.lib = nil
	}
}

// ---- plugin invocation helpers (stateless, use pluginMap lookup) ----

func pluginInvokeTool(pluginID int32, name, argsJSON string) (string, error) {
	v, ok := pluginMap.Load(pluginID)
	if !ok {
		return "", fmt.Errorf("plugin %d not found", pluginID)
	}
	ps := v.(*pluginState)
	if ps.api == nil {
		return "", fmt.Errorf("plugin %d: nil api", pluginID)
	}
	cName := C.CString(name)
	cArgs := C.CString(argsJSON)
	var result, cErr *C.char
	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cArgs))
	if ret := int(C.call_invoke_tool(ps.api, cName, cArgs, &result, &cErr)); ret != 0 {
		errMsg := ""
		if cErr != nil {
			errMsg = C.GoString(cErr)
			C.api_free_string(ps.api, cErr)
		}
		return "", fmt.Errorf("invoke_tool %s: %s", name, errMsg)
	}
	if result == nil {
		return "", nil
	}
	defer C.api_free_string(ps.api, result)
	return C.GoString(result), nil
}

// awaitOutputResult 在 goroutine 内执行真正的 cgo 发送调用，并等待其结果：
//   - 发送成功 → {status: sent}
//   - 发送失败 → 返回 error（模型可感知并重试），不再像旧实现那样谎报成功
//   - 超时未确认 → {status: unconfirmed}（结果未知，不谎报成功/失败）
//
// 为什么用 goroutine + channel 而不是直接同步调用：pluginInvokeOutput 是 cgo 调用，
// 不能嵌套在 cgo 栈上执行（cgo within cgo 会崩溃）。本 handler 由 executeOutputSendTool
// 从 Go 侧调起（不在 cgo 栈内），所以这里启动子 goroutine 执行 cgo 调用并等待其结果，
// 不构成嵌套。
//
// 修复 plan.md 11.1：旧实现无条件返回 {status: queued} + err=nil，模型永远收到「已发送」
// 而实际失败（如 meta 缺 user_id）只写日志，模型无法感知、不会重试。
func awaitOutputResult(pid int32, channel, argsJSON string) (interface{}, error) {
	return awaitOutputResultWith(pid, channel, argsJSON, pluginInvokeOutput, outputSendTimeout)
}

// awaitOutputResultWith 是 awaitOutputResult 的可注入版本（供单测替换 cgo 发送与超时）。
func awaitOutputResultWith(
	pid int32,
	channel, argsJSON string,
	invoke func(pluginID int32, channel, payload string) error,
	timeout time.Duration,
) (interface{}, error) {
	resCh := make(chan error, 1)
	go func() { resCh <- invoke(pid, channel, argsJSON) }()
	select {
	case err := <-resCh:
		if err != nil {
			log.Printf("[dispatch] output %s failed: %v", channel, err)
			return nil, err
		}
		log.Printf("[dispatch] output %s OK", channel)
		return map[string]interface{}{"status": "sent"}, nil
	case <-time.After(timeout):
		// 超时未确认：插件仍在后台发送，结果未知。不谎报成功，也不谎报失败。
		log.Printf("[dispatch] output %s 等待确认超时（%s），插件仍在后台发送", channel, timeout)
		return map[string]interface{}{
			"status": "unconfirmed",
			"note":   fmt.Sprintf("发送已提交但 %s 内未收到通道确认，结果未知；如需确认请查询该通道状态", timeout),
		}, nil
	}
}

func pluginInvokeOutput(pluginID int32, channel, payload string) error {
	v, ok := pluginMap.Load(pluginID)
	if !ok {
		return fmt.Errorf("plugin %d not found", pluginID)
	}
	ps := v.(*pluginState)
	if ps.api == nil {
		return fmt.Errorf("plugin %d: nil api", pluginID)
	}
	cCh := C.CString(channel)
	cPayload := C.CString(payload)
	var cErr *C.char
	defer C.free(unsafe.Pointer(cCh))
	defer C.free(unsafe.Pointer(cPayload))
	if ret := int(C.call_invoke_output(ps.api, cCh, nil, cPayload, &cErr)); ret != 0 {
		errMsg := ""
		if cErr != nil {
			errMsg = C.GoString(cErr)
			C.api_free_string(ps.api, cErr)
		}
		return fmt.Errorf("invoke_output %s: %s", channel, errMsg)
	}
	return nil
}

// applyStageResult 将插件回传的修改后上下文应用回内核 StageContext。
// 只回写插件有权改写的字段（RawMessage/LLMText/FinalText/Response/ToolResults/NoMemory）。
func applyStageResult(sc *sdk.StageContext, resultJSON string) {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(resultJSON), &m); err != nil {
		return
	}
	sc.Lock()
	defer sc.Unlock()
	if v, ok := m["raw_message"].(string); ok {
		sc.RawMessage = v
	}
	if v, ok := m["llm_text"].(string); ok {
		sc.LLMText = v
	}
	if v, ok := m["final_text"].(string); ok {
		sc.FinalText = v
	}
	if v, ok := m["user_id"].(string); ok {
		sc.UserID = v
	}
	if v, ok := m["group_id"].(string); ok {
		sc.GroupID = v
	}
	if v, ok := m["no_memory"].(bool); ok {
		sc.NoMemory = v
	}
	if v, ok := m["response"].(string); ok {
		vv := v
		sc.Response = &vv
	}
	if v, ok := m["tool_calls"].([]interface{}); ok && len(v) > 0 {
		if b, err := json.Marshal(v); err == nil {
			var tcs []sdk.ToolCall
			if json.Unmarshal(b, &tcs) == nil {
				sc.ToolCalls = tcs
			}
		}
	}
	if v, ok := m["tool_results"].([]interface{}); ok && len(v) > 0 {
		if b, err := json.Marshal(v); err == nil {
			var trs []sdk.ToolResult
			if json.Unmarshal(b, &trs) == nil {
				sc.ToolResults = trs
			}
		}
	}
}

func pluginInvokeStage(pluginID int32, stage, ctxJSON string, resultOut *string) error {
	v, ok := pluginMap.Load(pluginID)
	if !ok {
		return fmt.Errorf("plugin %d not found", pluginID)
	}
	ps := v.(*pluginState)
	if ps.api == nil {
		return fmt.Errorf("plugin %d: nil api", pluginID)
	}
	cStage := C.CString(stage)
	cCtx := C.CString(ctxJSON)
	var cErr *C.char
	var cResult *C.char
	defer C.free(unsafe.Pointer(cStage))
	defer C.free(unsafe.Pointer(cCtx))
	// 仅当调用方要求回传时传 &cResult，否则传 NULL（兼容无需写回的阶段）。
	if ret := int(C.call_invoke_stage(ps.api, cStage, cCtx, &cResult, &cErr)); ret != 0 {
		errMsg := ""
		if cErr != nil {
			errMsg = C.GoString(cErr)
			C.api_free_string(ps.api, cErr)
		}
		return fmt.Errorf("invoke_stage %s: %s", stage, errMsg)
	}
	if resultOut != nil && cResult != nil {
		*resultOut = C.GoString(cResult)
		C.api_free_string(ps.api, cResult)
	}
	return nil
}

// go_core_dispatch handles all plugin→core SDK calls.
//
//export go_core_dispatch
func go_core_dispatch(methodID C.int, ctx unsafe.Pointer, s1, s2, s3 *C.char, i1, i2 C.int, result **C.char, errorOut **C.char) C.int {
	pluginID := int32(uintptr(ctx))
	v, ok := pluginMap.Load(pluginID)
	if !ok {
		return 1
	}
	ps := v.(*pluginState)
	s := ps.sdk
	if s == nil {
		return 1
	}

	a1, a2, a3 := goStr(s1), goStr(s2), goStr(s3)
	n1, n2 := int(i1), int(i2)

	switch int(methodID) {
	case 1: // CORE_REGISTER_TOOL
		var def sdk.ToolDef
		if err := json.Unmarshal([]byte(a2), &def); err != nil {
			setErr(errorOut, err)
			return 1
		}
		def.Plugin = ps.name
		pid := pluginID
		toolName := a1
		_ = s.RegisterTool(a1, def, func(args map[string]interface{}) (interface{}, error) {
			argsJSON, _ := json.Marshal(args)
			r, err := pluginInvokeTool(pid, toolName, string(argsJSON))
			if err != nil {
				return nil, err
			}
			if r == "" {
				return nil, nil
			}
			var res map[string]interface{}
			if err := json.Unmarshal([]byte(r), &res); err != nil {
				return r, nil
			}
			return res, nil
		})
		return 0

	case 2: // CORE_REGISTER_STAGE
		pid := pluginID
		st := a1
		handler := func(sc *sdk.StageContext) error {
			sc.RLock()
			m := map[string]interface{}{
				"raw_message": sc.RawMessage, "user_id": sc.UserID,
				"group_id": sc.GroupID, "phase": string(sc.Phase),
				"llm_text": sc.LLMText, "final_text": sc.FinalText,
				"no_memory": sc.NoMemory,
			}
			if sc.Response != nil {
				m["response"] = *sc.Response
			}
			if len(sc.ToolCalls) > 0 {
				m["tool_calls"] = sc.ToolCalls
			}
			if len(sc.ToolResults) > 0 {
				m["tool_results"] = sc.ToolResults
			}
			sc.RUnlock()
			b, _ := json.Marshal(m)

			// ABI v2: 插件可回传修改后的上下文写回内核 sc（如 RawMessage/LLMText/Response/ToolResults）。
			var result string
			if err := pluginInvokeStage(pid, st, string(b), &result); err != nil {
				return err
			}
			if result != "" {
				applyStageResult(sc, result)
			}
			return nil
		}
		scope := sdk.StageScopeGlobal
		if a3 == "own_tools" {
			scope = sdk.StageScopeOwnTools
		}
		s.RegisterStage(sdk.Stage(st), handler, scope)
		return 0

	case 3: // CORE_REGISTER_OUTPUT_CH
		pid := pluginID
		chName := a1
		chDef := sdk.ChannelDef{}
		if a3 != "" {
			var def sdk.ChannelDef
			if err := json.Unmarshal([]byte(a3), &def); err == nil {
				chDef = def
			}
		}
		s.RegisterOutputChannel(chName, n1, a2, chDef, func(args map[string]interface{}) (interface{}, error) {
			// 发送在 goroutine 内进行（cgo 调用不能嵌套在 cgo 栈上，否则可能崩溃），
			// 但调用方必须拿到真实结果：本 handler 由 executeOutputSendTool 从 Go 侧
			// 调起，不在 cgo 栈内，因此这里等待 goroutine 的结果不构成 cgo 嵌套。
			// （plan.md 11.1）
			argsJSON, _ := json.Marshal(args)
			log.Printf("[dispatch] output %s/%s args=%s", ps.name, chName, string(argsJSON))
			return awaitOutputResult(pid, chName, string(argsJSON))
		})
		return 0

	case 4: // CORE_REGISTER_PLUGIN_API
		s.RegisterPluginAPI(a1)
		return 0

	case 5: // CORE_INJECT_TEXT
		s.InjectText(a1, a2, a3)
		return 0

	case 6: // CORE_INJECT_INTERRUPT_TEXT
		s.InjectInterruptText(a1, a2, a3)
		return 0

	case 7: // CORE_INJECT_TEXT_NO_MEMORY
		s.InjectTextNoMemory(a1, a2, a3)
		return 0

	case 47: // CORE_INJECT_INPUT_SYNC
		if out := s.InjectInputSync(a1, a2, "text", map[string]interface{}{"content": a3}); out != nil {
			reply, _ := out.Payload["content"].(string)
			setResult(result, reply)
		}
		return 0

	case 8: // CORE_SET_AUTO_RESTART
		s.SetAutoRestart(n1 != 0)
		return 0

	case 9: // CORE_MEMORY_RECALL
		if mem := s.Memory(); mem != nil {
			entities, relations, err := mem.Recall([]string{a1}, n1)
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(map[string]interface{}{"entities": entities, "relations": relations})
			setResult(result, string(b))
		}
		return 0

	case 10: // CORE_MEMORY_COMMIT
		if mem := s.Memory(); mem != nil {
			var triples []sdk.Triple
			if err := json.Unmarshal([]byte(a1), &triples); err != nil {
				setErr(errorOut, err)
				return 1
			}
			if err := mem.Commit(triples); err != nil {
				setErr(errorOut, err)
				return 1
			}
		}
		return 0

	case 11: // CORE_MEMORY_INTROSPECT
		if mem := s.Memory(); mem != nil {
			r, err := mem.Introspect()
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(r)
			setResult(result, string(b))
		}
		return 0

	case 12: // CORE_MEMORY_MERGE
		if mem := s.Memory(); mem != nil {
			if _, err := mem.MergeEntities(a1, a2); err != nil {
				setErr(errorOut, err)
				return 1
			}
		}
		return 0

	case 13: // CORE_MEMORY_PURGE
		if mem := s.Memory(); mem != nil {
			var criteria map[string]string
			if err := json.Unmarshal([]byte(a1), &criteria); err != nil {
				setErr(errorOut, err)
				return 1
			}
			mode := "soft"
			if n1 != 0 {
				mode = "hard"
			}
			if _, err := mem.Purge(criteria, mode); err != nil {
				setErr(errorOut, err)
				return 1
			}
		}
		return 0

	case 14: // CORE_DOC_QUERY
		if dm := s.DocMemory(); dm != nil {
			b, _ := json.Marshal(dm.Query(a1, n1))
			setResult(result, string(b))
		}
		return 0

	case 15: // CORE_KNOWLEDGE_SEARCH
		if kn := s.Knowledge(); kn != nil {
			results, err := kn.Search(a1, n1)
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(results)
			setResult(result, string(b))
		}
		return 0

	case 16: // CORE_SETTINGS_GET
		if sett := s.Settings(); sett != nil {
			v, err := sett.Get(a1)
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(v)
			setResult(result, string(b))
		}
		return 0

	case 17: // CORE_SETTINGS_SET
		if sett := s.Settings(); sett != nil {
			var v interface{}
			json.Unmarshal([]byte(a2), &v)
			if err := sett.Set(a1, v); err != nil {
				setErr(errorOut, err)
				return 1
			}
		}
		return 0

	case 18: // CORE_SETTINGS_REGISTER_DEF
		if sett := s.Settings(); sett != nil {
			var def sdk.ConfigDef
			if err := json.Unmarshal([]byte(a1), &def); err != nil {
				setErr(errorOut, err)
				return 1
			}
			sett.RegisterDef(def)
		}
		return 0

	case 19: // CORE_LLM_LIST_SOURCES
		if llm := s.LLM(); llm != nil {
			b, _ := json.Marshal(llm.ListSources())
			setResult(result, string(b))
		}
		return 0

	case 20: // CORE_LLM_SET_SOURCE
		if llm := s.LLM(); llm != nil {
			if err := llm.SetSource(a1); err != nil {
				setErr(errorOut, err)
				return 1
			}
		}
		return 0

	case 21: // CORE_SOCIAL_GET_PERSON
		if social := s.Social(); social != nil {
			p, err := social.GetPerson(a1)
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(p)
			setResult(result, string(b))
		}
		return 0

	case 22: // CORE_SOCIAL_GET_NETWORK
		if social := s.Social(); social != nil {
			profiles, err := social.GetNetwork(a1, n1)
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(profiles)
			setResult(result, string(b))
		}
		return 0

	case 23: // CORE_SUBSCRIBE
		_ = n2
		// Events API not wired for external plugins (SetEventSubscriber not called)
		return 0

	case 24: // CORE_UNSUBSCRIBE
		return 0

	case 25: // CORE_FREE_STRING
		if s1 != nil {
			C.free(unsafe.Pointer(s1))
		}
		return 0

	case 26: // CORE_SETTINGS_GET_CORE
		if sett := s.Settings(); sett != nil {
			v, err := sett.GetCore(a1)
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(v)
			setResult(result, string(b))
		}
		return 0

	case 27: // CORE_SETTINGS_SET_CORE
		if sett := s.Settings(); sett != nil {
			var v interface{}
			json.Unmarshal([]byte(a2), &v)
			if err := sett.SetCore(a1, v); err != nil {
				setErr(errorOut, err)
				return 1
			}
		}
		return 0

	case 28: // CORE_SETTINGS_LIST_CORE
		if sett := s.Settings(); sett != nil {
			keys, err := sett.ListCore(a1)
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(keys)
			setResult(result, string(b))
		}
		return 0

	case 29: // CORE_SETTINGS_GET_PLUGIN
		if sett := s.Settings(); sett != nil {
			v, err := sett.GetPlugin(a1, a2)
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(v)
			setResult(result, string(b))
		}
		return 0

	case 30: // CORE_SETTINGS_SET_PLUGIN
		if sett := s.Settings(); sett != nil {
			var v interface{}
			json.Unmarshal([]byte(a3), &v)
			if err := sett.SetPlugin(a1, a2, v); err != nil {
				setErr(errorOut, err)
				return 1
			}
		}
		return 0

	case 31: // CORE_SETTINGS_LIST_PLUGIN
		if sett := s.Settings(); sett != nil {
			keys, err := sett.ListPlugin(a1, a2)
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(keys)
			setResult(result, string(b))
		}
		return 0

	case 32: // CORE_DOC_INSERT
		if dm := s.DocMemory(); dm != nil {
			var doc sdk.Doc
			if err := json.Unmarshal([]byte(a1), &doc); err != nil {
				setErr(errorOut, err)
				return 1
			}
			if err := dm.Insert(&doc); err != nil {
				setErr(errorOut, err)
				return 1
			}
		}
		return 0

	case 33: // CORE_DOC_REMOVE
		if dm := s.DocMemory(); dm != nil {
			dm.Remove(a1)
		}
		return 0

	case 34: // CORE_DOC_STATS
		if dm := s.DocMemory(); dm != nil {
			b, _ := json.Marshal(dm.Stats())
			setResult(result, string(b))
		}
		return 0

	case 35: // CORE_KNOWLEDGE_ADD
		if kn := s.Knowledge(); kn != nil {
			if err := kn.Add(a1, a2); err != nil {
				setErr(errorOut, err)
				return 1
			}
		}
		return 0

	case 36: // CORE_KNOWLEDGE_LIST
		if kn := s.Knowledge(); kn != nil {
			list, err := kn.List()
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(list)
			setResult(result, string(b))
		}
		return 0

	case 37: // CORE_LLM_CURRENT_SOURCE
		if llm := s.LLM(); llm != nil {
			b, _ := json.Marshal(llm.CurrentSource())
			setResult(result, string(b))
		}
		return 0

	case 38: // CORE_SOCIAL_GET_TRAIT
		if social := s.Social(); social != nil {
			val, ok := social.GetTrait(a1, a2)
			b, _ := json.Marshal(map[string]interface{}{"value": val, "found": ok})
			setResult(result, string(b))
		}
		return 0

	case 39: // CORE_SOCIAL_GET_RELATIONS
		if social := s.Social(); social != nil {
			rels, err := social.GetRelations(a1)
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(rels)
			setResult(result, string(b))
		}
		return 0

	case 40: // CORE_SOCIAL_LIST_PERSONS
		if social := s.Social(); social != nil {
			persons, err := social.ListPersons()
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(persons)
			setResult(result, string(b))
		}
		return 0

	case 41: // CORE_TEXT_MEMORY_APPEND
		if tm := s.TextMemory(); tm != nil {
			var evt sdk.TextEvent
			if err := json.Unmarshal([]byte(a1), &evt); err != nil {
				setErr(errorOut, err)
				return 1
			}
			if err := tm.Append(evt); err != nil {
				setErr(errorOut, err)
				return 1
			}
		}
		return 0

	case 42: // CORE_SETTINGS_LIST
		if sett := s.Settings(); sett != nil {
			keys, err := sett.List(a1)
			if err != nil {
				setErr(errorOut, err)
				return 1
			}
			b, _ := json.Marshal(keys)
			setResult(result, string(b))
		}
		return 0

	case 43: // CORE_SETTINGS_DEFS
		if sett := s.Settings(); sett != nil {
			defs := sett.Defs(a1)
			b, _ := json.Marshal(defs)
			setResult(result, string(b))
		}
		return 0

	case 44: // CORE_SETTINGS_DUMP
		if sett := s.Settings(); sett != nil {
			dump := sett.Dump()
			b, _ := json.Marshal(dump)
			setResult(result, string(b))
		}
		return 0

	case 45: // CORE_SETTINGS_PLUGINS
		if sett := s.Settings(); sett != nil {
			plugins := sett.Plugins()
			b, _ := json.Marshal(plugins)
			setResult(result, string(b))
		}
		return 0

	case 51: // CORE_SETTINGS_DATA_DIR：插件专属数据目录（内核保证存在）
		if sett := s.Settings(); sett != nil {
			setResult(result, sett.DataDir())
		}
		return 0

	case 46: // CORE_REGISTER_INPUT_CH
		chDef := sdk.ChannelDef{}
		if a2 != "" {
			var def sdk.ChannelDef
			if err := json.Unmarshal([]byte(a2), &def); err == nil {
				chDef = def
			}
		}
		s.RegisterInputChannel(a1, chDef)
		return 0

	case 48: // CORE_PLUGIN_RELOAD_ONE
		if s.PluginMgr() == nil {
			setErr(errorOut, fmt.Errorf("plugin manager not available"))
			return 1
		}
		if err := s.PluginMgr().ReloadOne(a1); err != nil {
			setErr(errorOut, err)
			return 1
		}
		setResult(result, "reloaded: "+a1)
		return 0

	case 49: // CORE_PLUGIN_LIST_LOADED
		if s.PluginMgr() == nil {
			setErr(errorOut, fmt.Errorf("plugin manager not available"))
			return 1
		}
		if b, err := json.Marshal(s.PluginMgr().ListLoadedPlugins()); err == nil {
			setResult(result, string(b))
		}
		return 0

	case 50: // CORE_PLUGIN_IS_DISABLED
		if s.PluginMgr() == nil {
			setErr(errorOut, fmt.Errorf("plugin manager not available"))
			return 1
		}
		if s.PluginMgr().IsPluginDisabled(a1) {
			setResult(result, "1")
		} else {
			setResult(result, "0")
		}
		return 0
	}
	return 0
}

func goStr(s *C.char) string {
	if s == nil {
		return ""
	}
	return C.GoString(s)
}

func setErr(errOut **C.char, err error) {
	if errOut != nil && err != nil {
		*errOut = C.CString(err.Error())
	}
}

func setResult(result **C.char, v string) {
	if result != nil {
		*result = C.CString(v)
	}
}
