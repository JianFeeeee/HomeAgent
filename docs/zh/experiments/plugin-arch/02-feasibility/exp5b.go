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
	if err != nil { return -1 }
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "Pss:") { f := strings.Fields(l); n,_ := strconv.Atoi(f[1]); return n }
	}
	return -1
}
func rssKB(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil { return -1 }
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") { f := strings.Fields(l); n,_ := strconv.Atoi(f[1]); return n }
	}
	return -1
}
func threads(pid int) int { e,_ := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid)); return len(e) }

func main() {
	fmt.Println("=== 实验 5b：17 个 Go 子进程常驻开销（保持 stdin 管道存活）===")
	var cmds []*exec.Cmd
	var pipes []interface{ Close() error }
	for i := 0; i < 17; i++ {
		c := exec.Command("./plugbin")
		w, _ := c.StdinPipe()   // 保持打开 → 不 EOF
		pipes = append(pipes, w)
		c.Stdout = nil
		if err := c.Start(); err != nil { fmt.Println(err); return }
		cmds = append(cmds, c)
	}
	time.Sleep(2 * time.Second)

	tp, tr, tt, alive := 0, 0, 0, 0
	for _, c := range cmds {
		pid := c.Process.Pid
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil { continue }
		alive++
		if v := pssKB(pid); v > 0 { tp += v }
		if v := rssKB(pid); v > 0 { tr += v }
		tt += threads(pid)
	}
	fmt.Printf("存活进程 %d/17\n", alive)
	fmt.Printf("合计: PSS=%.1f MB  RSS=%.1f MB  线程=%d\n",
		float64(tp)/1024, float64(tr)/1024, tt)
	if alive > 0 {
		fmt.Printf("均摊: PSS=%.2f MB  RSS=%.2f MB  线程=%.1f\n",
			float64(tp)/1024/float64(alive), float64(tr)/1024/float64(alive), float64(tt)/float64(alive))
	}
	out, _ := exec.Command("pgrep", "-x", "homed").Output()
	if p := strings.TrimSpace(string(out)); p != "" {
		pid, _ := strconv.Atoi(strings.Fields(p)[0])
		fmt.Printf("\n对照 homed（单进程 + 17 个 .so）: RSS=%.1f MB 线程=%d\n",
			float64(rssKB(pid))/1024, threads(pid))
	}
	for _, c := range cmds { c.Process.Kill(); c.Wait() }
}
