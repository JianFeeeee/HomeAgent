//go:build darwin

package proc

import (
	"os"
	"sync"
	"syscall"
)

// macOS 侧事件通知：用 pipe 模拟 eventfd（macOS 无 eventfd_create）。
//
// 与 eventfd 的语义差异：pipe 不具计数合并，多次写会触发多次读。
// 这不影响正确性——消费者在 drainEvents 里按 readSeq 追 writeSeq 批量读，
// 多次唤醒只多几次空循环（readSeq == writeSeq 时立即返回）。
//
// 走 Go netpoller：os.File.Read 阻塞时只 park goroutine，不占 OS 线程
// （实验 1 已验证 200 个等待者仅增 1 个 OS 线程）。
//
// ❗ 必须同时持有读端与写端：写端若被 GC 回收，读端会收到 EOF 而非阻塞，
// 消费循环变成忙转。故用 pipePair 表存住两端。
type pipePair struct {
	r *os.File
	w *os.File
}

var (
	evtPipes   = map[int]*pipePair{}
	evtPipesMu sync.Mutex
)

// evtfdCreate 建 pipe，返回读端 fd。
func evtfdCreate() (int, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return -1, err
	}
	fd := int(r.Fd())
	evtPipesMu.Lock()
	evtPipes[fd] = &pipePair{r: r, w: w}
	evtPipesMu.Unlock()
	return fd, nil
}

// EvtfdNotify 写 1 字节通知子进程有新事件（post-and-forget）。
//
// 直接写裸 fd 而非 pipePair.w：本函数在 Bus.Publish 路径上被高频调用，
// 查表加锁不值得。写端 fd 由 pipePair 持有引用故不会被 GC 回收。
func EvtfdNotify(efd int) {
	evtPipesMu.Lock()
	p, ok := evtPipes[efd]
	evtPipesMu.Unlock()
	if !ok {
		return
	}
	var buf [1]byte
	// 忽略错误：管道满说明消费者落后，事件环本身允许溢出丢弃
	syscall.Write(int(p.w.Fd()), buf[:])
}

// evtfdReadFile 返回通知读端（供 netpoller 消费）。
func evtfdReadFile(efd int) *os.File {
	evtPipesMu.Lock()
	defer evtPipesMu.Unlock()
	if p, ok := evtPipes[efd]; ok {
		return p.r
	}
	return nil
}

// evtfdClose 关闭 pipe 两端。
func evtfdClose(efd int) {
	evtPipesMu.Lock()
	p, ok := evtPipes[efd]
	delete(evtPipes, efd)
	evtPipesMu.Unlock()
	if ok {
		p.r.Close()
		p.w.Close()
	}
}
