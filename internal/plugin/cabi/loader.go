package cabi

/*
#cgo LDFLAGS: -ldl
#include <stdlib.h>

#define HOMEAGENT_ABI_VERSION 1

// PluginAPI — provided by the plugin via plugin_init()
typedef struct {
    int version; int version_min;
    int (*init_plugin)(char*, char*, char**);
    int (*start_plugin)(void*, int, char**);
    int (*stop_plugin)(char**);
    int (*invoke_tool)(char*, char*, char**, char**);
    int (*invoke_stage)(char*, char*, char**);
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
extern int call_invoke_stage(plugin_api_t*, char*, char*, char**);
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
	"sync"
	"sync/atomic"
	"unsafe"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

var (
	pluginMap sync.Map // int32 pluginID → *pluginState
	nextID    int32
)

type pluginState struct {
	id    int32
	name  string
	sdk   *sdk.PluginSDK // set after CreateCoreAPI
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

	id := atomic.AddInt32(&nextID, 1)
	ps := &pluginState{id: id, name: name}
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
		if initErr != nil { errMsg = C.GoString(initErr); C.api_free_string(api, initErr) }
		handle.Close()
		return nil, fmt.Errorf("init_plugin %s: %s", name, errMsg)
	}

	return handle, nil
}

// CreateCoreAPI creates a CoreAPI struct for this plugin.
// The CoreAPI dispatches all SDK calls back to Go, routing to the plugin's PluginSDK.
func (h *Handle) CreateCoreAPI(s *sdk.PluginSDK) unsafe.Pointer {
	core := C.make_core_api()
	if core == nil { return nil }
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
	if ret := int(C.call_start_plugin(h.api, corePtr, C.int(1), nil)); ret != 0 {
		return fmt.Errorf("start_plugin failed")
	}
	return nil
}

// Stop calls the plugin's Stop.
func (h *Handle) Stop() error {
	if ret := int(C.call_stop_plugin(h.api, nil)); ret != 0 {
		return fmt.Errorf("stop_plugin failed")
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
		if cErr != nil { errMsg = C.GoString(cErr); C.api_free_string(h.api, cErr) }
		return nil, fmt.Errorf("invoke_tool %s: %s", name, errMsg)
	}
	if result == nil { return nil, nil }
	defer C.api_free_string(h.api, result)
	var r map[string]interface{}
	if err := json.Unmarshal([]byte(C.GoString(result)), &r); err != nil { return nil, err }
	return r, nil
}

// Close unloads the plugin library.
func (h *Handle) Close() {
	if h.lib != nil {
		C.lib_close(h.lib)
		h.lib = nil
	}
}

// go_core_dispatch handles all plugin→core SDK calls.
// CoreAPI.dispatch → dispatch_bridge (C) → go_core_dispatch (Go) → PluginSDK
//
//export go_core_dispatch
func go_core_dispatch(methodID C.int, ctx unsafe.Pointer, s1, s2, s3 *C.char, i1, i2 C.int, result **C.char, errorOut **C.char) C.int {
	pluginID := int32(uintptr(ctx))
	v, ok := pluginMap.Load(pluginID)
	if !ok { return 1 }
	ps := v.(*pluginState)
	s := ps.sdk

	a1, a2, a3 := goStr(s1), goStr(s2), goStr(s3)
	n1, n2 := int(i1), int(i2)

			_ = n2
	switch int(methodID) {
	case 1: // CORE_REGISTER_TOOL
		var def sdk.ToolDef
		if err := json.Unmarshal([]byte(a2), &def); err != nil { setErr(errorOut, err); return 1 }
		def.Plugin = ps.name
		_ = s.RegisterTool(a1, def, func(args map[string]interface{}) (interface{}, error) {
			return nil, nil
		})
		return 0

	case 2: // CORE_REGISTER_STAGE
		s.RegisterStage(sdk.Stage(a1), func(sc *sdk.StageContext) error { return nil })
		return 0

	case 3: // CORE_REGISTER_OUTPUT_CH
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

	case 8: // CORE_SET_AUTO_RESTART
		s.SetAutoRestart(n1 != 0)
		return 0

	case 9: // CORE_MEMORY_RECALL
		if mem := s.Memory(); mem != nil {
			entities, relations, err := mem.Recall([]string{a1}, n1)
			if err != nil { setErr(errorOut, err); return 1 }
			b, _ := json.Marshal(map[string]interface{}{"entities": entities, "relations": relations})
			*result = C.CString(string(b))
		}
		return 0

	case 10: // CORE_MEMORY_COMMIT
		if mem := s.Memory(); mem != nil {
			var triples []sdk.Triple
			if err := json.Unmarshal([]byte(a1), &triples); err != nil { setErr(errorOut, err); return 1 }
			if err := mem.Commit(triples); err != nil { setErr(errorOut, err); return 1 }
		}
		return 0

	case 11: // CORE_MEMORY_INTROSPECT
		if mem := s.Memory(); mem != nil {
			r, err := mem.Introspect()
			if err != nil { setErr(errorOut, err); return 1 }
			b, _ := json.Marshal(r)
			*result = C.CString(string(b))
		}
		return 0

	case 12: // CORE_MEMORY_MERGE
		if mem := s.Memory(); mem != nil {
			if _, err := mem.MergeEntities(a1, a2); err != nil { setErr(errorOut, err); return 1 }
		}
		return 0

	case 13: // CORE_MEMORY_PURGE
		if mem := s.Memory(); mem != nil {
			var criteria map[string]string
			if err := json.Unmarshal([]byte(a1), &criteria); err != nil { setErr(errorOut, err); return 1 }
			mode := "soft"
			if n1 != 0 { mode = "hard" }
			if _, err := mem.Purge(criteria, mode); err != nil { setErr(errorOut, err); return 1 }
		}
		return 0

	case 14: // CORE_DOC_QUERY
		if dm := s.DocMemory(); dm != nil {
			docs := dm.Query(a1, n1)
			b, _ := json.Marshal(docs)
			*result = C.CString(string(b))
		}
		return 0

	case 15: // CORE_KNOWLEDGE_SEARCH
		if kn := s.Knowledge(); kn != nil {
			results, err := kn.Search(a1, n1)
			if err != nil { setErr(errorOut, err); return 1 }
			b, _ := json.Marshal(results)
			*result = C.CString(string(b))
		}
		return 0

	case 16: // CORE_SETTINGS_GET
		if sett := s.Settings(); sett != nil {
			v, err := sett.Get(a1)
			if err != nil { setErr(errorOut, err); return 1 }
			b, _ := json.Marshal(v)
			*result = C.CString(string(b))
		}
		return 0

	case 17: // CORE_SETTINGS_SET
		if sett := s.Settings(); sett != nil {
			var v interface{}
			json.Unmarshal([]byte(a2), &v)
			if err := sett.Set(a1, v); err != nil { setErr(errorOut, err); return 1 }
		}
		return 0

	case 18: // CORE_SETTINGS_REGISTER_DEF
		if sett := s.Settings(); sett != nil {
			var def sdk.ConfigDef
			if err := json.Unmarshal([]byte(a1), &def); err != nil { setErr(errorOut, err); return 1 }
			sett.RegisterDef(def)
		}
		return 0

	case 19: // CORE_LLM_LIST_SOURCES
		if llm := s.LLM(); llm != nil {
			b, _ := json.Marshal(llm.ListSources())
			*result = C.CString(string(b))
		}
		return 0

	case 20: // CORE_LLM_SET_SOURCE
		if llm := s.LLM(); llm != nil {
			if err := llm.SetSource(a1); err != nil { setErr(errorOut, err); return 1 }
		}
		return 0

	case 21: // CORE_SOCIAL_GET_PERSON
		if social := s.Social(); social != nil {
			p, err := social.GetPerson(a1)
			if err != nil { setErr(errorOut, err); return 1 }
			b, _ := json.Marshal(p)
			*result = C.CString(string(b))
		}
		return 0

	case 22: // CORE_SOCIAL_GET_NETWORK
		if social := s.Social(); social != nil {
			profiles, err := social.GetNetwork(a1, n1)
			if err != nil { setErr(errorOut, err); return 1 }
			b, _ := json.Marshal(profiles)
			*result = C.CString(string(b))
		}
		return 0

	case 23: // CORE_SUBSCRIBE
		return 0

	case 24: // CORE_UNSUBSCRIBE
		return 0

	case 25: // CORE_FREE_STRING
		if s1 != nil { C.free(unsafe.Pointer(s1)) }
		return 0
	}
	return 0
}

func goStr(s *C.char) string {
	if s == nil { return "" }
	return C.GoString(s)
}

func setErr(errOut **C.char, err error) {
	if errOut != nil && err != nil {
		*errOut = C.CString(err.Error())
	}
}
