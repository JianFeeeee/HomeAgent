//go:build linux

package proc

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// evtfdCreate 创建 Linux eventfd（EFD_NONBLOCK | EFD_CLOEXEC）。
//
// 语义：64 位无符号计数器，多次 Write(8) 只累加，Read 一次取出合并值。
// 计数合并满足 §3.6 的设计：1000 个 token 事件只唤醒几次。
// 走 Go netpoller（实验 1 已验证 200 等待者仅 +1 OS 线程）。
func evtfdCreate() (int, error) {
	return unix.Eventfd(0, unix.EFD_CLOEXEC)
}

// evtfdNotify 写 1 到 eventfd 通知子进程有新事件（post-and-forget）。
//
// EFD_NONBLOCK 保证不阻塞（§3.6 约束 B：Bus.Publish 路径上绝不等待）。
// 计数语义使多事件写入合并成一次唤醒。
func EvtfdNotify(efd int) {
	var buf [8]byte
	buf[0] = 1
	// 忽略错误：EFD_NONBLOCK 下只有内存不足才会失败，此时进程已在崩溃边缘
	syscall.Write(efd, buf[:])
}

// evtfdReadFile 把 eventfd 包装成 *os.File 供 netpoller 消费。
func evtfdReadFile(efd int) *os.File {
	return os.NewFile(uintptr(efd), "evtring-notify")
}

// evtfdClose 关闭通知句柄。Unix 侧由 *os.File.Close 负责，此处为跨平台签名占位。
func evtfdClose(efd int) {}
