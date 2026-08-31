//go:build ignore
package main

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

type ring struct {
	writeSeq atomic.Uint64
	cap      uint64
	slots    []uint64
}

func main() {
	fmt.Println("=== 实验 4：事件环 post-and-forget vs 同步 Publish（慢消费者场景）===")
	const tokens = 5000

	// --- A. 现状：同步 Publish，消费者慢 ---
	slowHandler := func() { time.Sleep(20 * time.Microsecond) }
	t0 := time.Now()
	for i := 0; i < tokens; i++ { slowHandler() }
	syncDur := time.Since(t0)
	fmt.Printf("A 同步 Publish (慢消费者 20µs): %d token 耗时 %v  → 均摊 %.1f µs/token\n",
		tokens, syncDur, float64(syncDur.Microseconds())/tokens)

	// --- B. 新方案：写环 + eventfd post，不等消费者 ---
	r := &ring{cap: 1024, slots: make([]uint64, 1024)}
	efd, _ := unix.Eventfd(0, unix.EFD_NONBLOCK)
	f := os.NewFile(uintptr(efd), "e")

	var dropped atomic.Uint64
	// 慢消费者 goroutine
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 8)
		var readSeq uint64
		for {
			if _, err := f.Read(buf); err != nil { return }
			w := r.writeSeq.Load()
			if w-readSeq > r.cap {
				dropped.Add(w - readSeq - r.cap)
				readSeq = w - r.cap
			}
			for readSeq < w { readSeq++ }
			time.Sleep(20 * time.Microsecond) // 慢
			select { case <-done: return; default: }
		}
	}()

	t0 = time.Now()
	one := []byte{1,0,0,0,0,0,0,0}
	for i := 0; i < tokens; i++ {
		s := r.writeSeq.Add(1)
		r.slots[s%r.cap] = s      // 写数据
		f.Write(one)              // post，不等
	}
	asyncDur := time.Since(t0)
	close(done)
	fmt.Printf("B 环+eventfd post:            %d token 耗时 %v  → 均摊 %.2f µs/token\n",
		tokens, asyncDur, float64(asyncDur.Microseconds())/tokens)
	fmt.Printf("\n加速比 %.1fx   丢弃事件 %d（消费者跟不上，已计数）\n",
		float64(syncDur)/float64(asyncDur), dropped.Load())
	if asyncDur < syncDur/5 {
		fmt.Println("✅ post-and-forget 使流式发布与消费者速度解耦")
	}
}
