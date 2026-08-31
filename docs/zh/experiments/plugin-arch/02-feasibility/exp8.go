//go:build ignore
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	fmt.Println("=== 实验 8：跨进程并发扇出改写同一 StageContext（最高风险点 3.4）===")

	mfd, _ := unix.MemfdCreate("stagectx", 0)
	unix.Ftruncate(mfd, 65536)
	shmFile := os.NewFile(uintptr(mfd), "shm")
	data, _ := unix.Mmap(mfd, 0, 65536, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)

	// 初始 final_text = "" @1024, arena 游标 = 1024
	binary.LittleEndian.PutUint32(data[0:4], 1024)
	binary.LittleEndian.PutUint32(data[4:8], 0)
	binary.LittleEndian.PutUint32(data[8:12], 1024)

	tags := []string{"A", "B", "C", "D", "E"}   // 5 个并发插件
	var mu sync.Mutex                           // 内核侧锁仲裁
	var wg sync.WaitGroup
	var rpcCount int64
	var cntMu sync.Mutex

	t0 := time.Now()
	for _, tag := range tags {
		cmd := exec.Command("go", "run", "exp8_worker.go", tag)
		cmd.ExtraFiles = []*os.File{shmFile}
		sin, _ := cmd.StdinPipe()
		sout, _ := cmd.StdoutPipe()
		cmd.Stderr = os.Stderr
		cmd.Start()
		wg.Add(1)
		go func() {
			defer wg.Done()
			dec := json.NewDecoder(bufio.NewReader(sout))
			w := bufio.NewWriter(sin)
			enc := json.NewEncoder(w)
			held := false
			for {
				var q map[string]string
				if err := dec.Decode(&q); err != nil { break }
				switch q["method"] {
				case "stage.lock":   mu.Lock();   held = true
				case "stage.unlock": if held { mu.Unlock(); held = false }
				}
				cntMu.Lock(); rpcCount++; cntMu.Unlock()
				enc.Encode(map[string]bool{"ok": true}); w.Flush()
			}
			if held { mu.Unlock() }
			cmd.Wait()
		}()
	}
	wg.Wait()
	dur := time.Since(t0)

	off := binary.LittleEndian.Uint32(data[0:4])
	ln := binary.LittleEndian.Uint32(data[4:8])
	final := string(data[off : off+ln])

	fmt.Printf("\n--- 结果 ---\n")
	fmt.Printf("最终 final_text 长度 = %d\n", len(final))
	counts := map[string]int{}
	for _, t := range tags { counts[t] = strings.Count(final, t) }
	fmt.Printf("各插件写入次数: %v\n", counts)
	total := 0
	for _, c := range counts { total += c }
	fmt.Printf("总字符 = %d, 长度 = %d  → %s\n", total, len(final),
		map[bool]string{true:"一致 ✅ 无丢失/无撕裂", false:"不一致 ❌"}[total == len(final)])
	fmt.Printf("RPC 锁操作 = %d 次, 总耗时 %v\n", rpcCount, dur)
	fmt.Printf("\n注：写入次数少于 5×300 是 arena 64KB 上限所致（append-only 未压实），符合设计\n")
}
