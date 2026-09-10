//go:build linux

package proc

import (
	"fmt"
	"os"
	"strings"
)

// countOurShmMappings 在 Linux 上审计本 Host 的共享段被多少进程映射。
//
// 原理：memfd 是匿名对象，文件系统无名，但在每个映射它的进程的
// /proc/<pid>/maps 里会有一行形如：
//
//	7f.. 7f.. rw-s 00000000 00:00 7312 .../memfd:hastagectx (deleted)
//
// 我们遍历 Supervisor.List() 给出的全部子进程 PID，统计它们的 maps 中
// 出现 "memfd:hastagectx" 的行数。
// homed 自身的映射不算（它本来就该有一份）。
//
// 局限：/proc/<pid>/maps 需要同 UID/同命名空间读。读不到的 PID 直接跳过，
// 返回 ok=true 但少计——审计是告警不是判据，宁缺毋滥。
func (h *Host) countOurShmMappings() (int, bool) {
	if h.sup == nil {
		return 0, false
	}
	procs := h.sup.List()
	if len(procs) == 0 {
		return 0, true
	}
	// memfd 的"文件名"在 shmalloc_linux.go 里是 "hastagectx"。
	const memfdTag = "memfd:hastagectx"
	total := 0
	for _, p := range procs {
		if !p.Alive {
			continue
		}
		if n, err := countMemfdLinesForPID(p.PID, memfdTag); err == nil {
			total += n
		}
	}
	return total, true
}

// countMemfdLinesForPID 读 /proc/<pid>/maps，统计含 tag 的行数。
//
// 一个进程对同一 memfd 多次 mmap 会产生多行；我们计数"行数"而非
// "是否出现"，这样能发现一个插件进程泄漏了多余映射的情况。
func countMemfdLinesForPID(pid int, tag string) (int, error) {
	data, err := os.ReadFile(procMapsPath(pid))
	if err != nil {
		return 0, fmt.Errorf("读 /proc/%d/maps: %w", pid, err)
	}
	return strings.Count(string(data), tag), nil
}

// procMapsPath 组装 /proc/<pid>/maps 路径。pid 是内核自己 spawn 的进程号，
// 无用户输入注入面。
func procMapsPath(pid int) string {
	return fmt.Sprintf("/proc/%d/maps", pid)
}
