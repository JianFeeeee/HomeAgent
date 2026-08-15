//go:build linux || darwin

// HomeAgent C ABI loader — C implementation (compiled alongside Go code via cgo)

#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>

// HOMEAGENT_ABI_VERSION 与 internal/meta/meta.go CABINum 同步（major*100+minor，v0.9.x→900）。
// C ABI 通过 version/version_min 协商，旧插件不受影响。
#define HOMEAGENT_ABI_VERSION 900

// PluginAPI — provided by the plugin
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

// CoreAPI — provided by the core
typedef struct {
    int version; int version_min;
    int (*dispatch)(int, void*, char*, char*, char*, int, int, char**, char**);
    void* ctx;
} core_api_t;

// Forward declare Go dispatch function
extern int go_core_dispatch(int, void*, char*, char*, char*, int, int, char**, char**);

// Bridge function called by CoreAPI.dispatch
static int dispatch_bridge(int id, void* ctx, char* s1, char* s2, char* s3, int i1, int i2, char** r, char** e) {
    return go_core_dispatch(id, ctx, s1, s2, s3, i1, i2, r, e);
}

// Create a CoreAPI struct
core_api_t* make_core_api(void) {
    core_api_t* api = (core_api_t*)malloc(sizeof(core_api_t));
    if (!api) return NULL;
    api->version = HOMEAGENT_ABI_VERSION;
    api->version_min = HOMEAGENT_ABI_VERSION;
    api->dispatch = dispatch_bridge;
    api->ctx = NULL;
    return api;
}

void free_core_api(core_api_t* api) { free(api); }

// dlopen helpers
typedef void* lib_handle;

lib_handle lib_open(const char* path) {
    return dlopen(path, RTLD_NOW | RTLD_LOCAL);
}

plugin_api_t* lib_get_api(lib_handle h) {
    plugin_api_t* (*fn)(void);
    *(void**)(&fn) = dlsym(h, "plugin_init");
    if (!fn) return NULL;
    return fn();
}

void lib_close(lib_handle h) { dlclose(h); }
char* lib_err(void) { return dlerror(); }

void api_free_string(plugin_api_t* api, char* ptr) {
    if (api && api->free_string) api->free_string(ptr);
}

int call_init_plugin(plugin_api_t* api, char* name, char* config, char** err) { return api->init_plugin(name, config, err); }
int call_start_plugin(plugin_api_t* api, void* core, int ver, char** err) { return api->start_plugin(core, ver, err); }
int call_stop_plugin(plugin_api_t* api, char** err) { return api->stop_plugin(err); }
int call_invoke_tool(plugin_api_t* api, char* n, char* a, char** r, char** e) { return api->invoke_tool(n, a, r, e); }
int call_invoke_stage(plugin_api_t* api, char* s, char* c, char** r, char** e) { return api->invoke_stage(s, c, r, e); }
int call_invoke_output(plugin_api_t* api, char* c, char* m, char* p, char** e) { return api->invoke_output(c, m, p, e); }
