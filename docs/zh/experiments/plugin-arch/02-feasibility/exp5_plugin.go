//go:build ignore
package main

import (
	"bufio"
	"encoding/json"
	"os"
)

// 模拟一个最小插件：stdio JSON-RPC loop + 一个 goroutine
func main() {
	go func() { select {} }()
	in := bufio.NewReader(os.Stdin)
	dec := json.NewDecoder(in)
	out := bufio.NewWriter(os.Stdout)
	enc := json.NewEncoder(out)
	for {
		var m map[string]interface{}
		if err := dec.Decode(&m); err != nil { return }
		enc.Encode(map[string]interface{}{"ok": true})
		out.Flush()
	}
}
