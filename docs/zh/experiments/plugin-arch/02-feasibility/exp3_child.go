//go:build ignore
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type req struct{ ID int `json:"id"`; Method string `json:"method"` }
type resp struct{ ID int `json:"id"`; OK bool `json:"ok"` }

func main() {
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	enc, dec := json.NewEncoder(out), json.NewDecoder(in)

	const N = 20000
	t0 := time.Now()
	for i := 0; i < N; i++ {
		enc.Encode(req{ID: i, Method: "stage.lock"})
		out.Flush()
		var r resp
		if err := dec.Decode(&r); err != nil { fmt.Fprintln(os.Stderr, "dec:", err); return }
	}
	d := time.Since(t0)
	fmt.Fprintf(os.Stderr, "CHILD: %d 次 lock RPC 往返 用时 %v, 均摊 %.2f µs/次\n",
		N, d, float64(d.Microseconds())/float64(N))
}
