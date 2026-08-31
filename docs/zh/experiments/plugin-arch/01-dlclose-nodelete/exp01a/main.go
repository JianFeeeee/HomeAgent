//go:build ignore

package main

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>
typedef const char* (*verfn)(void);
static const char* call_ver(void* f){ return ((verfn)f)(); }
*/
import "C"
import (
	"fmt"
	"os"
	"unsafe"
)

func main() {
	// Go 用 dlopen 加载纯 C shim（shim 本身常驻，无所谓）
	sp := C.CString("./shim.so")
	shim := C.dlopen(sp, C.RTLD_NOW|C.RTLD_LOCAL)
	C.free(unsafe.Pointer(sp))
	if shim == nil {
		fmt.Println("shim 加载失败:", C.GoString(C.dlerror()))
		os.Exit(1)
	}
	openName := C.CString("shim_open")
	closeName := C.CString("shim_close")
	symName := C.CString("shim_sym")
	shimOpen := C.dlsym(shim, openName)
	shimClose := C.dlsym(shim, closeName)
	shimSym := C.dlsym(shim, symName)
	C.free(unsafe.Pointer(openName))
	C.free(unsafe.Pointer(closeName))
	C.free(unsafe.Pointer(symName))
	fmt.Printf("shim 就绪: open=%p close=%p sym=%p\n\n", shimOpen, shimClose, shimSym)

	// 直接用 dlopen/dlsym 调 shim 的三个函数（避免再写一层 C 包装）
	load := func(path string) unsafe.Pointer {
		cp := C.CString(path)
		defer C.free(unsafe.Pointer(cp))
		return C.dlopen(cp, C.RTLD_NOW|C.RTLD_LOCAL)
	}
	ver := func(h unsafe.Pointer) string {
		n := C.CString("probe_version")
		defer C.free(unsafe.Pointer(n))
		f := C.dlsym(h, n)
		if f == nil { return "<no sym>" }
		return C.GoString(C.call_ver(f))
	}

	fmt.Println("--- 场景: Go(带 NODELETE runtime) 加载/卸载纯 C 的第三层 so ---")
	h1 := load("./probe.so")
	fmt.Printf("1) dlopen probe.so       handle=%p  version=%s\n", h1, ver(h1))

	rc := C.dlclose(h1)
	fmt.Printf("2) dlclose               rc=%d\n", int(rc))

	// 换内容（V1 -> V2），同路径
	in, _ := os.ReadFile("probe_v2.so")
	os.WriteFile("probe.so", in, 0755)
	fmt.Println("3) 磁盘 probe.so 内容替换为 V2（同路径）")

	h2 := load("./probe.so")
	fmt.Printf("4) 再 dlopen 同路径       handle=%p  version=%s\n", h2, ver(h2))
	if h1 == h2 {
		fmt.Println("   => 句柄相同：未卸载，仍是旧代码")
	} else {
		fmt.Println("   => 句柄不同：真正卸载并重新装载了新代码 ✅")
	}
}
