//go:build !linux

package proc

import "os/exec"

// applyProcAttr 在非 Linux 平台是空实现。
//
// macOS 没有 Pdeathsig（kqueue 的 NOTE_EXIT 要求父进程存活才能监听，
// 恰好在父进程被 SIGKILL 时失效）；Windows 的 Job Object 可做到类似效果，
// 但需要额外的句柄管理，且 Windows 侧尚未真机验证（§12.5），不在此引入。
//
// 后果：这两个平台上 homed 被强杀时子进程会成为孤儿。
// 正常关停路径（Supervisor.StopAll）不受影响。
func applyProcAttr(cmd *exec.Cmd) {}
