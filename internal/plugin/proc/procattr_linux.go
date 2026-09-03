//go:build linux

package proc

import (
	"os/exec"
	"syscall"
)

// applyProcAttr 让子进程在父进程（homed）死亡时收到 SIGKILL。
//
// 这是**最后一道兜底**，不是主路径：正常关停走 Supervisor.StopAll。
// 它兜的是内核自身异常终止的场景——homed 被 SIGKILL、段错误、OOM——
// 此时没有任何 Go 代码有机会运行，Supervisor 也来不及 StopAll，
// 子进程会被 init 收养成孤儿：
//   - 继续持有已被 unmap 的共享段映射，下次访问即 SIGBUS；
//   - 与新启动的 homed 抢同一份外部资源（qq 的 WS 会话、browser 的
//     chromium profile 锁），表现为"重启后插件时好时坏"。
//
// Pdeathsig 由内核在父进程退出时投递，不依赖任何用户态代码，
// 因此在 homed 被 SIGKILL 的情况下依然生效。
//
// 仅 Linux 有此机制。macOS/Windows 无等价物，回退为空实现（procattr_other.go）：
// 那两个平台上孤儿风险依旧存在，靠 StopAll 覆盖正常关停路径。
func applyProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
