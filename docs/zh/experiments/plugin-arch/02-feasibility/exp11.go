//go:build ignore
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"time"
)

type Req struct{ ID int `json:"id"`; Method string `json:"method"`; Args json.RawMessage `json:"args"` }
type Res struct{ ID int `json:"id"`; Result string `json:"result"` }

func main() {
	fmt.Println("=== 实验 11：工具调用 RPC 端到端延迟（实测 payload 中位 93B）===")
	cmd := exec.Command("./plug11")
	sin, _ := cmd.StdinPipe(); sout, _ := cmd.StdoutPipe()
	cmd.Start()
	enc := json.NewEncoder(bufio.NewWriter(sin))
	w := bufio.NewWriter(sin); enc = json.NewEncoder(w)
	dec := json.NewDecoder(bufio.NewReader(sout))

	args := json.RawMessage(`{"city":"hangzhou","days":3,"unit":"celsius","detail":true}`)
	const N = 10000
	lat := make([]time.Duration, 0, N)
	for i := 0; i < N; i++ {
		t0 := time.Now()
		enc.Encode(Req{ID: i, Method: "weather_query", Args: args}); w.Flush()
		var r Res
		if err := dec.Decode(&r); err != nil { break }
		lat = append(lat, time.Since(t0))
	}
	sin.Close(); cmd.Wait()
	sort.Slice(lat, func(a,b int) bool { return lat[a] < lat[b] })
	p := func(q float64) time.Duration { return lat[int(float64(len(lat))*q)] }
	fmt.Printf("样本 %d 次\n", len(lat))
	fmt.Printf("  p50 = %v\n  p90 = %v\n  p99 = %v\n  max = %v\n", p(0.5), p(0.9), p(0.99), lat[len(lat)-1])
	fmt.Printf("\n对照 LLM 单轮往返 2-8 秒 → RPC 占比 ≈ %.5f%%\n",
		float64(p(0.5))/float64(3*time.Second)*100)
}
