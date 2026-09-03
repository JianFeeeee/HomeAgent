//go:build ignore

package main

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>
typedef char* (*verfn)(void);
static char* call_ver(void* f){ return ((verfn)f)(); }
*/
import "C"
import (
	"fmt"
	"os"
	"strings"
	"unsafe"
)

func threads() int {
	e, _ := os.ReadDir("/proc/self/task")
	return len(e)
}
func rss() int {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			var k int
			fmt.Sscanf(l, "VmRSS: %d kB", &k)
			return k
		}
	}
	return 0
}
func main() {
	base, baseT := rss(), threads()
	fmt.Printf("基线: RSS=%dKB threads=%d\n\n", base, baseT)
	src, _ := os.ReadFile("glv1.so")
	os.MkdirAll("stress", 0755)
	var hs []unsafe.Pointer
	for i := 1; i <= 30; i++ {
		p := fmt.Sprintf("stress/%010d-qq.so", 1700000000+i)
		os.WriteFile(p, src, 0755)
		cp := C.CString("./" + p)
		h := C.dlopen(cp, C.RTLD_NOW|C.RTLD_LOCAL)
		C.free(unsafe.Pointer(cp))
		if h == nil { fmt.Printf("第 %d 次失败\n", i); break }
		hs = append(hs, h)
		C.dlclose(h) // 模拟每次都尝试卸载（no-op）
		if i%10 == 0 {
			fmt.Printf("第 %2d 次重载: RSS=%dKB (+%dKB) threads=%d (+%d)\n",
				i, rss(), rss()-base, threads(), threads()-baseT)
		}
	}
	fmt.Printf("\n30 次重载后: RSS 增长 %dKB, 线程增长 %d\n", rss()-base, threads()-baseT)
	fmt.Printf("每次重载均摊: RSS +%.1fKB, 线程 +%.2f\n",
		float64(rss()-base)/30, float64(threads()-baseT)/30)
}
