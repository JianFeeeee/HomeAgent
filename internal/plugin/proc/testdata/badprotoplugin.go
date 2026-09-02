//go:build ignore

// badprotoplugin 上报错误的协议版本，验证内核显式拒绝而非半兼容运行。
package main

import (
	"bufio"
	"encoding/json"
	"os"
)

type request struct {
	ID     uint64          `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID     uint64      `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

func main() {
	in := bufio.NewScanner(bufio.NewReader(os.Stdin))
	out := bufio.NewWriter(os.Stdout)
	send := func(v interface{}) {
		b, _ := json.Marshal(v)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}

	for in.Scan() {
		var req request
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			continue
		}
		if req.Method == "handshake" {
			send(response{ID: req.ID, Result: map[string]interface{}{
				"protocol":    999, // 故意不匹配
				"sdk_version": "ancient",
				"plugin_name": "badproto",
				"pid":         os.Getpid(),
			}})
			continue
		}
		if req.ID != 0 {
			send(response{ID: req.ID})
		}
	}
}
