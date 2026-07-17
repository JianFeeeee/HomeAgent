package cabi

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>

// PluginAPI struct (mirrors plugin ABI)
typedef struct {
    int version;
    int version_min;
    int (*init_plugin)(char*, char*, char**);
    int (*start_plugin)(void*, int, char**);
    int (*stop_plugin)(char**);
    int (*invoke_tool)(char*, char*, char**, char**);
    int (*invoke_stage)(char*, char*, char**);
    int (*invoke_output)(char*, char*, char*, char**);
    void (*free_string)(char*);
    int (*get_tool_defs)(char**);
    int (*get_stages)(char**);
    int (*get_channels)(char**);
} plugin_api_t;

// CoreAPI struct (implemented by core, passed to plugin)
typedef struct {
    int version;
    int version_min;
    int (*register_tool)(char*, char*, char**);
    int (*register_stage)(char*, int, char**);
    int (*register_output_channel)(char*, int, char*, int, char**);
    int (*register_plugin_api)(char*, char**);
    int (*inject_text)(char*, char*, char*, char**);
    int (*inject_interrupt_text)(char*, char*, char*, char**);
    int (*inject_text_no_memory)(char*, char*, char*, char**);
    int (*set_auto_restart)(int, char**);
    int (*memory_recall)(char*, int, char**, char**);
    int (*memory_commit)(char*, char**);
    int (*memory_introspect)(char**, char**);
    int (*memory_merge)(char*, char*, char**);
    int (*memory_purge)(char*, int, char**);
    int (*doc_query)(char*, int, char**, char**);
    int (*knowledge_search)(char*, int, char**, char**);
    int (*settings_get)(char*, char**, char**);
    int (*settings_set)(char*, char*, char**);
    int (*settings_register_def)(char*, char**);
    int (*llm_list_sources)(char**, char**);
    int (*llm_set_source)(char*, char**);
    int (*social_get_person)(char*, char**, char**);
    int (*social_get_network)(char*, int, char**, char**);
    int (*subscribe)(char*, int, char**);
    int (*unsubscribe)(char*, int, char**);
    void (*free_string)(char*);
} core_api_t;

// libHandle wraps a dlopen handle
typedef void* libHandle;

libHandle lib_open(const char* path) {
    return dlopen(path, RTLD_NOW | RTLD_LOCAL);
}

plugin_api_t* lib_get_api(libHandle h) {
    plugin_api_t* (*fn)(void);
    *(void**)(&fn) = dlsym(h, "plugin_init");
    if (!fn) return NULL;
    return fn();
}

void lib_close(libHandle h) {
    dlclose(h);
}

char* lib_get_error(void) {
    return dlerror();
}

void api_free_string(plugin_api_t* api, char* ptr) {
    if (api && api->free_string) api->free_string(ptr);
}

int call_init_plugin(plugin_api_t* api, char* name, char* config, char** err) { return api->init_plugin(name, config, err); }
int call_start_plugin(plugin_api_t* api, void* core, int ver, char** err) { return api->start_plugin(core, ver, err); }
int call_stop_plugin(plugin_api_t* api, char** err) { return api->stop_plugin(err); }
int call_get_tool_defs(plugin_api_t* api, char** r) { return api->get_tool_defs(r); }
int call_get_stages(plugin_api_t* api, char** r) { return api->get_stages(r); }
int call_get_channels(plugin_api_t* api, char** r) { return api->get_channels(r); }
int call_invoke_tool(plugin_api_t* api, char* n, char* a, char** r, char** e) { return api->invoke_tool(n, a, r, e); }
int call_invoke_stage(plugin_api_t* api, char* s, char* c, char** e) { return api->invoke_stage(s, c, e); }
int call_invoke_output(plugin_api_t* api, char* c, char* m, char* p, char** e) { return api->invoke_output(c, m, p, e); }
*/
import "C"
import (
	"encoding/json"
	"fmt"
	"unsafe"
)

// Handle represents a loaded C ABI plugin.
type Handle struct {
	soPath string
	lib    C.libHandle
	api    *C.plugin_api_t
}

// Load opens a .so plugin and initializes it via the C ABI.
func Load(soPath, name string, config map[string]interface{}) (*Handle, error) {
	cPath := C.CString(soPath)
	defer C.free(unsafe.Pointer(cPath))

	lib := C.lib_open(cPath)
	if lib == nil {
		errStr := C.GoString(C.lib_get_error())
		return nil, fmt.Errorf("dlopen %s: %s", soPath, errStr)
	}

	api := C.lib_get_api(lib)
	if api == nil {
		C.lib_close(lib)
		errStr := C.GoString(C.lib_get_error())
		return nil, fmt.Errorf("dlsym plugin_init in %s: %s", soPath, errStr)
	}

	if int(api.version) < ABIVersionMin {
		C.lib_close(lib)
		return nil, fmt.Errorf("plugin %s ABI version %d < minimum %d", name, int(api.version), ABIVersionMin)
	}

	handle := &Handle{soPath: soPath, lib: lib, api: api}

	// Initialize plugin
	configJSON, _ := json.Marshal(config)
	cName := C.CString(name)
	cConfig := C.CString(string(configJSON))
	var initErr *C.char
	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cConfig))

	if ret := C.int(C.call_init_plugin(api, cName, cConfig, &initErr)); ret != 0 {
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

// Start calls the plugin's Start with a CoreAPI pointer.
func (h *Handle) Start(corePtr unsafe.Pointer) error {
	if ret := C.int(C.call_start_plugin(h.api, corePtr, C.int(ABIVersion), nil)); ret != 0 {
		return fmt.Errorf("start_plugin failed")
	}
	return nil
}

// Stop calls the plugin's Stop.
func (h *Handle) Stop() error {
	if ret := C.int(C.call_stop_plugin(h.api, nil)); ret != 0 {
		return fmt.Errorf("stop_plugin failed")
	}
	return nil
}

// GetToolDefs returns the tool definitions registered by the plugin during Start.
func (h *Handle) GetToolDefs() ([]json.RawMessage, error) {
	var result *C.char
	if ret := C.int(C.call_get_tool_defs(h.api, &result)); ret != 0 || result == nil {
		return nil, nil
	}
	defer C.api_free_string(h.api, result)
	var defs []json.RawMessage
	if err := json.Unmarshal([]byte(C.GoString(result)), &defs); err != nil {
		return nil, err
	}
	return defs, nil
}

// GetStages returns stage names registered by the plugin.
func (h *Handle) GetStages() ([]string, error) {
	var result *C.char
	if ret := C.int(C.call_get_stages(h.api, &result)); ret != 0 || result == nil {
		return nil, nil
	}
	defer C.api_free_string(h.api, result)
	var stages []string
	if err := json.Unmarshal([]byte(C.GoString(result)), &stages); err != nil {
		return nil, err
	}
	return stages, nil
}

// GetChannels returns output channel registrations.
func (h *Handle) GetChannels() ([]channelInfo, error) {
	var result *C.char
	if ret := C.int(C.call_get_channels(h.api, &result)); ret != 0 || result == nil {
		return nil, nil
	}
	defer C.api_free_string(h.api, result)
	var channels []channelInfo
	if err := json.Unmarshal([]byte(C.GoString(result)), &channels); err != nil {
		return nil, err
	}
	return channels, nil
}

type channelInfo struct {
	Name string `json:"name"`
	Caps int    `json:"caps"`
	Desc string `json:"desc"`
}

// InvokeTool calls a tool handler in the plugin.
func (h *Handle) InvokeTool(name string, args map[string]interface{}) (map[string]interface{}, error) {
	argsJSON, _ := json.Marshal(args)
	cName := C.CString(name)
	cArgs := C.CString(string(argsJSON))
	var result, cErr *C.char
	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cArgs))

	if ret := C.int(C.call_invoke_tool(h.api, cName, cArgs, &result, &cErr)); ret != 0 {
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

// InvokeStage calls a stage handler in the plugin.
func (h *Handle) InvokeStage(stage, ctxJSON string) error {
	cStage := C.CString(stage)
	cCtx := C.CString(ctxJSON)
	defer C.free(unsafe.Pointer(cStage))
	defer C.free(unsafe.Pointer(cCtx))
	if ret := C.int(C.call_invoke_stage(h.api, cStage, cCtx, nil)); ret != 0 {
		return fmt.Errorf("invoke_stage %s failed", stage)
	}
	return nil
}

// Close unloads the plugin library.
func (h *Handle) Close() {
	if h.lib != nil {
		C.lib_close(h.lib)
		h.lib = nil
	}
}
