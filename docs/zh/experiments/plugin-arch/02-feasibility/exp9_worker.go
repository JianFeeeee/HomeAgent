//go:build ignore
package main

import ("bufio";"encoding/json";"os")
func main() {
	dec := json.NewDecoder(bufio.NewReader(os.Stdin))
	w := bufio.NewWriter(os.Stdout); enc := json.NewEncoder(w)
	rpc := func(m string) { enc.Encode(map[string]string{"method":m}); w.Flush(); var r map[string]interface{}; dec.Decode(&r) }
	rpc("stage.lock")
	if os.Args[1] == "crash" { panic("持锁时崩溃") }  // 拿着锁死掉
	rpc("stage.unlock")
}
