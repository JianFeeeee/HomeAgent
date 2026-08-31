//go:build ignore
package main

import (
	"bufio"
	"encoding/json"
	"os"
)

func main() {
	dec := json.NewDecoder(bufio.NewReader(os.Stdin))
	out := bufio.NewWriter(os.Stdout)
	enc := json.NewEncoder(out)
	for {
		var m map[string]interface{}
		if err := dec.Decode(&m); err != nil { return }
		if m["method"] == "boom" {
			panic("插件故意崩溃")   // 真 panic
		}
		enc.Encode(map[string]interface{}{"ok": true})
		out.Flush()
	}
}
