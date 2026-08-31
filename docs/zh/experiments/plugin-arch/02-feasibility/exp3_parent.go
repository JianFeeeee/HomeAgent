//go:build ignore
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

type req struct{ ID int `json:"id"`; Method string `json:"method"` }
type resp struct{ ID int `json:"id"`; OK bool `json:"ok"` }

func main() {
	fmt.Println("=== 实验 3：锁仲裁 RPC 往返成本（stdio JSON-RPC）===")
	cmd := exec.Command("go", "run", "exp3_child.go")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	cmd.Start()

	var mu sync.Mutex // 内核侧真实的锁仲裁
	dec := json.NewDecoder(bufio.NewReader(stdout))
	w := bufio.NewWriter(stdin)
	enc := json.NewEncoder(w)
	for {
		var q req
		if err := dec.Decode(&q); err != nil { break }
		mu.Lock()   // 真实加锁
		mu.Unlock() // 立即释放（模拟仲裁开销）
		enc.Encode(resp{ID: q.ID, OK: true})
		w.Flush()
	}
	cmd.Wait()
}
