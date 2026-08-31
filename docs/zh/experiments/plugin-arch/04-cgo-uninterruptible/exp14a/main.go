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
	"os/exec"
	"runtime"
	"time"
	"unsafe"
)

func threads() int { e,_ := os.ReadDir("/proc/self/task"); return len(e) }

func main() {
	fmt.Println("=== A. cgo 模型：插件死循环，超时后能回收吗？ ===")
	p := C.CString("./hang.so"); h := C.dlopen(p, C.RTLD_NOW); C.free(unsafe.Pointer(p))
	n := C.CString("hang_forever"); f := C.dlsym(h, n); C.free(unsafe.Pointer(n))

	base := threads()
	fmt.Printf("  基线: goroutines=%d threads=%d\n", runtime.NumGoroutine(), base)

	for i := 1; i <= 3; i++ {
		done := make(chan string, 1)
		go func() { C.call(f); done <- "ok" }()   // 模拟 executeToolCallInner
		select {
		case <-done:
		case <-time.After(600 * time.Millisecond):  // 缩短的"60s 超时"
		}
		time.Sleep(200 * time.Millisecond)
		fmt.Printf("  第 %d 次超时后: goroutines=%d threads=%d (+%d)\n",
			i, runtime.NumGoroutine(), threads(), threads()-base)
	}
	fmt.Println("  ❌ 每次超时永久泄漏 1 goroutine + 1 OS 线程（cgo 调用不可中断）")

	fmt.Println("\n=== B. 子进程模型：同样死循环，可强杀 ===")
	base2 := threads()
	for i := 1; i <= 3; i++ {
		cmd := exec.Command("sleep", "3600")
		cmd.Start()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
			cmd.Process.Kill()   // ← 可强制终止
			<-done
		}
		fmt.Printf("  第 %d 次超时+Kill 后: goroutines=%d threads=%d (+%d)\n",
			i, runtime.NumGoroutine(), threads(), threads()-base2)
	}
	fmt.Println("  ✅ 零泄漏：进程被杀，OS 回收全部资源")
}
