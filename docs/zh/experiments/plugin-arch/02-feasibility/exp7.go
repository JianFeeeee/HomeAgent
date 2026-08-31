//go:build ignore
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

func spawnAndAsk(bin string) string {
	cmd := exec.Command(bin)
	sin, _ := cmd.StdinPipe()
	sout, _ := cmd.StdoutPipe()
	cmd.Start()
	enc := json.NewEncoder(sin)
	dec := json.NewDecoder(bufio.NewReader(sout))
	enc.Encode(map[string]string{"method": "version"})
	var r map[string]interface{}
	dec.Decode(&r)
	sin.Close()
	cmd.Process.Kill()
	cmd.Wait()
	if v, ok := r["version"].(string); ok { return v }
	return "?"
}

func build(ver, out string) {
	src := fmt.Sprintf(`package main
import ("bufio";"encoding/json";"os")
func main(){
  dec:=json.NewDecoder(bufio.NewReader(os.Stdin))
  w:=bufio.NewWriter(os.Stdout); enc:=json.NewEncoder(w)
  for { var m map[string]interface{}
    if err:=dec.Decode(&m); err!=nil {return}
    enc.Encode(map[string]string{"version":%q}); w.Flush() }
}`, ver)
	os.MkdirAll("v", 0755)
	os.WriteFile("v/main.go", []byte(src), 0644)
	os.WriteFile("v/go.mod", []byte("module v\ngo 1.21\n"), 0644)
	c := exec.Command("go", "build", "-o", "../"+out, ".")
	c.Dir = "v"
	if b, err := c.CombinedOutput(); err != nil { fmt.Println("build err:", string(b)) }
}

func main() {
	fmt.Println("=== 实验 7：子进程模型下的热重载（迁移的原始目标）===")
	build("v1.0.0", "hotbin")
	fmt.Printf("1) 首次启动插件 → version = %s\n", spawnAndAsk("./hotbin"))

	fmt.Println("2) 替换二进制为 v2.0.0（同路径，无需版本化 hash 目录）")
	build("v2.0.0", "hotbin")
	time.Sleep(200 * time.Millisecond)

	v := spawnAndAsk("./hotbin")
	fmt.Printf("3) 重启插件进程 → version = %s\n", v)
	if v == "v2.0.0" {
		fmt.Println("\n✅ 同路径替换即生效：无 NODELETE、无版本化路径、无线程泄漏")
		fmt.Println("   对照 .so 模型：同路径 dlopen 复用旧映像，永远拿不到 v2")
	}
}
