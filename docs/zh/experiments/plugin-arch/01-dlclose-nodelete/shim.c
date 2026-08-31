#include <dlfcn.h>
#include <stdio.h>
void* shim_open(const char* p){
    void* h = dlopen(p, RTLD_NOW|RTLD_LOCAL);
    if(!h) printf("    [shim] open FAIL: %s\n", dlerror());
    return h;
}
int shim_close(void* h){ return dlclose(h); }
void* shim_sym(void* h, const char* n){ return dlsym(h, n); }
