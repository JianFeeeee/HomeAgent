//go:build ignore

package main

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>
typedef void* (*openfn)(const char*);
typedef int (*closefn)(void*);
static void* c_open(void* f, const char* p){ return ((openfn)f)(p); }
static int   c_close(void* f, void* h){ return ((closefn)f)(h); }
*/
import "C"
import (
	"fmt"
	"os"
	"strings"
	"unsafe"
)

func cnt(s string) int {
	b, _ := os.ReadFile("/proc/self/maps")
	n := 0
	for _, l := range strings.Split(string(b), "\n") { if strings.Contains(l, s) { n++ } }
	return n
}

func main() {
	sp := C.CString("./shim.so")
	shim := C.dlopen(sp, C.RTLD_NOW|C.RTLD_LOCAL)
	C.free(unsafe.Pointer(sp))
	no := C.CString("shim_open"); nc := C.CString("shim_close")
	fo := C.dlsym(shim, no); fc := C.dlsym(shim, nc)
	C.free(unsafe.Pointer(no)); C.free(unsafe.Pointer(nc))

	// 经【纯 C shim】去 dlopen/dlclose Go c-shared 插件
	qp := C.CString("/home/newqqagent/plugins/qq/plugin.so")
	h := C.c_open(fo, qp)
	C.free(unsafe.Pointer(qp))
	fmt.Printf("经 C shim dlopen Go 插件  handle=%p 映射段=%d\n", h, cnt("qq/plugin.so"))
	rc := C.c_close(fc, h)
	fmt.Printf("经 C shim dlclose        rc=%d 映射段=%d\n", int(rc), cnt("qq/plugin.so"))
	if cnt("qq/plugin.so") > 0 {
		fmt.Println("\n❌ 仍未卸载 —— NODELETE 属于目标 .so 本身，与谁调 dlopen 无关")
	} else {
		fmt.Println("\n✅ 卸载成功")
	}
}
