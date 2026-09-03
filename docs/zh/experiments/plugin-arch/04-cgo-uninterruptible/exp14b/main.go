//go:build ignore

package main

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>
typedef void (*fn)(void);
static void call(void* f){ ((fn)f)(); }
*/
import "C"
import (
	"fmt"
	"os"
	"runtime"
	"time"
	"unsafe"
)

func threads() int { e,_ := os.ReadDir("/proc/self/task"); return len(e) }

func main() {
	p := C.CString("./hang.so"); h := C.dlopen(p, C.RTLD_NOW); C.free(unsafe.Pointer(p))
	n := C.CString("hang_forever"); f := C.dlsym(h, n); C.free(unsafe.Pointer(n))
	base := threads()
	fmt.Printf("基线 threads=%d goroutines=%d\n\n", base, runtime.NumGoroutine())
	for i := 1; i <= 20; i++ {
		done := make(chan string, 1)
		go func() { C.call(f); done <- "ok" }()
		select {
		case <-done:
		case <-time.After(120 * time.Millisecond):
		}
		if i%5 == 0 {
			fmt.Printf("  %2d 次卡死调用后: goroutines=%2d threads=%2d (+%d)\n",
				i, runtime.NumGoroutine(), threads(), threads()-base)
		}
	}
	fmt.Printf("\n结论: 20 次超时 → 泄漏 %d goroutine, %d OS 线程\n",
		runtime.NumGoroutine()-1, threads()-base)
	fmt.Println("每个卡在 cgo 里的 goroutine 独占一个 M（OS 线程），无法被抢占或回收")
}
