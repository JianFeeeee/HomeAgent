//go:build unix

package proc

import (
	"errors"

	"golang.org/x/sys/unix"
)

// pollEvtfd 等待 fd 可读，最多 timeoutMs 毫秒。超时返回 (false, nil)。
//
// 为什么不用 os.File.SetReadDeadline：eventfd/pipe 经 os.NewFile 包装后
// **不会**注册进 Go netpoller（os.NewFile 对非 open 得到的 fd 一律按非
// pollable 处理），Read 退化成阻塞 syscall，SetReadDeadline 返回错误且
// 不生效。实测表现是 Run 永久卡在 syscall.Read，Stop 无法打断。
//
// 用 poll(2) 显式加超时，消费循环才能周期性回到 stop 检查。
func pollEvtfd(fd int, timeoutMs int) (bool, error) {
	if fd < 0 {
		return false, errors.New("evtfd: 非法 fd")
	}
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, timeoutMs)
		if err == unix.EINTR {
			continue // 被信号打断：重试，超时预算不变
		}
		if err != nil {
			return false, err
		}
		return n > 0, nil
	}
}
