//go:build ignore
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

func main() {
	fmt.Println("=== 实验 6：子进程崩溃隔离 + 退出码/EOF 作为 recordCrash 信号 ===")
	cmd := exec.Command("./crashbin")
	sin, _ := cmd.StdinPipe()
	sout, _ := cmd.StdoutPipe()
	cmd.Stderr = nil // 丢弃 panic 栈
	cmd.Start()
	fmt.Printf("插件进程 pid=%d 已启动\n", cmd.Process.Pid)

	enc := json.NewEncoder(sin)
	dec := json.NewDecoder(bufio.NewReader(sout))

	// 正常调用
	enc.Encode(map[string]string{"method": "ping"})
	var r map[string]interface{}
	if err := dec.Decode(&r); err == nil { fmt.Println("正常调用 → ", r) }

	// 触发崩溃
	fmt.Println("\n发送 boom（插件内 panic）...")
	t0 := time.Now()
	enc.Encode(map[string]string{"method": "boom"})
	err := dec.Decode(&r)

	detected := "未检测到"
	if errors.Is(err, io.EOF) || err == io.ErrUnexpectedEOF { detected = "EOF" } else if err != nil { detected = fmt.Sprintf("%v", err) }
	fmt.Printf("调用侧感知: %s (耗时 %v)\n", detected, time.Since(t0))

	werr := cmd.Wait()
	var ec int = -1
	if ee, ok := werr.(*exec.ExitError); ok { ec = ee.ExitCode() }
	fmt.Printf("进程退出码 = %d  （panic → 2，可直接喂 recordCrash）\n", ec)

	fmt.Printf("\n宿主进程仍存活: pid=%d ✅ 崩溃已隔离\n", os.Getpid())
	fmt.Println("→ 对照：当前 .so 模型下，bridge 兜不住的 panic 会带崩整个 homed")
}
