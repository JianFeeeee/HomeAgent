//go:build ignore
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

func run(name, arg string, mu *sync.Mutex, crashed *bool) {
	cmd := exec.Command("go", "run", "exp9_worker.go", arg)
	sin, _ := cmd.StdinPipe(); sout, _ := cmd.StdoutPipe()
	cmd.Stderr = nil
	cmd.Start()
	dec := json.NewDecoder(bufio.NewReader(sout))
	w := bufio.NewWriter(sin); enc := json.NewEncoder(w)
	held := false
	for {
		var q map[string]string
		if err := dec.Decode(&q); err != nil { break }
		switch q["method"] {
		case "stage.lock":   mu.Lock(); held = true; fmt.Printf("  [%s] 获得锁\n", name)
		case "stage.unlock": if held { mu.Unlock(); held = false; fmt.Printf("  [%s] 释放锁\n", name) }
		}
		enc.Encode(map[string]bool{"ok":true}); w.Flush()
	}
	err := cmd.Wait()
	// 关键：进程死了，内核侧检测到 EOF/退出 → 强制释放它持有的锁
	if held {
		mu.Unlock()
		*crashed = true
		fmt.Printf("  [%s] 进程死亡(%v)，内核强制释放其持有的锁 ← 自愈\n", name, err)
	}
}

func main() {
	fmt.Println("=== 实验 9：持锁进程崩溃后的自愈（验证无需 robust pthread_mutex）===")
	var mu sync.Mutex
	crashed := false

	fmt.Println("\n1) 插件 X 拿锁后 panic:")
	run("X", "crash", &mu, &crashed)

	fmt.Println("\n2) 插件 Y 随后申请同一把锁:")
	done := make(chan bool, 1)
	go func() { run("Y", "normal", &mu, new(bool)); done <- true }()
	select {
	case <-done:
		fmt.Println("\n✅ Y 正常获得并释放锁 —— 无死锁")
		fmt.Println("   → 内核持有锁的所有权，进程死亡由 Wait()/EOF 检测并强制释放")
		fmt.Println("   → 不需要 PTHREAD_PROCESS_SHARED|ROBUST，也不需要处理 EOWNERDEAD")
		fmt.Println("   → 整个架构可做到零 cgo")
	case <-time.After(15 * time.Second):
		fmt.Println("\n❌ 死锁：Y 拿不到锁（说明需要 robust 语义）")
	}
	_ = crashed
}
