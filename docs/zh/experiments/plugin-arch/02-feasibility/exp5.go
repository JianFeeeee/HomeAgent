//go:build ignore
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func pssKB(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil { return 0 }
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "Pss:") {
			f := strings.Fields(l)
			n, _ := strconv.Atoi(f[1]); return n
		}
	}
	return 0
}
func threads(pid int) int {
	e, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid)); return len(e)
}

func main() {
	fmt.Println("=== 实验 5：17 个 Go 子进程插件的真实常驻开销（PSS 计入共享页去重）===")
	var cmds []*exec.Cmd
	for i := 0; i < 17; i++ {
		c := exec.Command("./plugbin")
		c.Stdin, _ = os.Open(os.DevNull)
		if err := c.Start(); err != nil { fmt.Println("start:", err); return }
		cmds = append(cmds, c)
	}
	time.Sleep(1500 * time.Millisecond)

	totalPss, totalThreads := 0, 0
	for _, c := range cmds {
		totalPss += pssKB(c.Process.Pid)
		totalThreads += threads(c.Process.Pid)
	}
	fmt.Printf("17 进程合计: PSS = %.1f MB, 线程 = %d\n", float64(totalPss)/1024, totalThreads)
	fmt.Printf("单进程均摊: PSS = %.2f MB, 线程 = %.1f\n",
		float64(totalPss)/1024/17, float64(totalThreads)/17)
	fmt.Printf("\n对照 homed 当前（单进程装 17 个 .so）:\n")
	// 找 homed
	out, _ := exec.Command("pgrep", "-x", "homed").Output()
	if p := strings.TrimSpace(string(out)); p != "" {
		pid, _ := strconv.Atoi(strings.Fields(p)[0])
		fmt.Printf("  homed PSS = %.1f MB, 线程 = %d\n", float64(pssKB(pid))/1024, threads(pid))
	}
	for _, c := range cmds { c.Process.Kill(); c.Wait() }
}
