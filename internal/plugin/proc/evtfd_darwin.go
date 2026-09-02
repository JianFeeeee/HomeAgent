//go:build darwin

package proc

import (
	"os"
)

// evtfdCreate 用 pipe 模拟 Linux eventfd 的通知语义（macOS 无 eventfd）。
//
// 限制：不具 eventfd 的计数合并（多次写会触发多次读），
// 但事件环本身允许溢出丢弃，consumer 在 drainEvents 里按 readSeq 批量读取，
// 故多次唤醒只多几次无效循环（readSeq == writeSeq 时立即返回），不造成正确性问题。
//
// 走 Go netpoller（os.File.Read 阻塞时只 park goroutine，实验 1 已验证）。
func evtfdCreate() (int, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return -1, err
	}
	return int(r.Fd()), nil
}

// evtfdNotify 写 1 字节通知子进程有新事件（post-and-forget）。
func EvtfdNotify(efd int) {
	var buf [1]byte
	syscall.Write(efd, buf[:])
}

// evtfdReadFile 把事件通知读端包装成 *os.File 供 netpoller 消费。
func evtfdReadFile(efd int) *os.File {
	return os.NewFile(uintptr(efd), "evtring-notify")
}
