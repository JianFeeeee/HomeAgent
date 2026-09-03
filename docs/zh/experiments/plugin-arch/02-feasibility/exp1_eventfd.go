//go:build ignore
package main

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

func threads() int { e, _ := os.ReadDir("/proc/self/task"); return len(e) }

func main() {
	fmt.Println("=== 实验 1：eventfd 是否走 Go netpoller（只 park goroutine 不占 OS 线程）===")
	base := threads()
	fmt.Printf("基线线程数: %d (GOMAXPROCS=%d)\n\n", base, runtime.GOMAXPROCS(0))

	const N = 200 // 模拟 200 个订阅者等待
	var wg sync.WaitGroup
	var woke int64
	files := make([]*os.File, N)

	for i := 0; i < N; i++ {
		efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
		if err != nil { fmt.Println("eventfd 失败:", err); return }
		f := os.NewFile(uintptr(efd), fmt.Sprintf("evt%d", i))
		files[i] = f
		wg.Add(1)
		go func(f *os.File) {
			defer wg.Done()
			buf := make([]byte, 8)
			// 阻塞读：若走 netpoller 只 park goroutine
			if _, err := f.Read(buf); err == nil {
				atomic.AddInt64(&woke, 1)
			}
		}(f)
	}

	time.Sleep(500 * time.Millisecond) // 让所有 goroutine 进入等待
	waiting := threads()
	fmt.Printf("%d 个 goroutine 阻塞在 eventfd.Read 后:\n", N)
	fmt.Printf("  线程数 = %d (增长 %d)\n", waiting, waiting-base)
	if waiting-base < 20 {
		fmt.Println("  ✅ 走 netpoller：线程未随等待者数量增长")
	} else {
		fmt.Printf("  ❌ 退化为阻塞 syscall：每个等待者占一个 OS 线程\n")
	}

	// 全部唤醒
	one := []byte{1,0,0,0,0,0,0,0}
	for _, f := range files { f.Write(one) }
	wg.Wait()
	fmt.Printf("\n唤醒数 = %d/%d  唤醒后线程数 = %d\n", woke, N, threads())
}
