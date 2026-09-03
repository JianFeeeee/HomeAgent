//go:build windows

package proc

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/windows"
)

// Windows 侧事件通知：命名 Event 对象。
//
// 与 eventfd 的语义差异：Event 是二元信号（Set/Reset），不是计数器。
// 多次 SetEvent 只对应一次唤醒，不会累积。
//
// 这不影响正确性：消费者被唤醒后按 readSeq 追 writeSeq 批量 drain，
// 一次唤醒能处理累积的全部事件（漏掉的是"唤醒次数"，不是"事件"）。
// 事件环本身允许溢出丢弃并让消费者知道丢了（dropped 计数），
// 通知面从来不是可靠投递语义。
//
// 代价：WaitForSingleObject 阻塞 OS 线程而非仅 goroutine，不如 eventfd
// 的 netpoller 路径省线程。每插件一个消费 goroutine，17 插件即 17 线程
// （实验 5 实测 17 子进程共 84 线程，仍在可接受范围）。
var evtEventSeq atomic.Uint64

// evtEventHandles 记录 fd 伪值 → Event 句柄的映射。
//
// 为何需要：跨平台签名用 int 表示通知句柄（Unix 是真 fd）。
// Windows 的 windows.Handle 是 uintptr，直接转 int 在 32 位上会截断，
// 故用递增伪 fd 做 key，句柄存表里。
var (
	evtEvents   = map[int]windows.Handle{}
	evtEventsMu sync.Mutex
	evtEventFd  atomic.Int64
)

// evtfdCreate 创建命名 Event 对象，返回伪 fd。
//
// 手动重置（manualReset=false → 自动重置）：被一个等待者唤醒后自动 Reset，
// 语义最接近 eventfd 的"取出后清零"。
func evtfdCreate() (int, error) {
	name := fmt.Sprintf("%s_%d_%d", evtEventNamePfx, os.Getpid(), evtEventSeq.Add(1))
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return -1, fmt.Errorf("proc: 事件对象名字非法 %q: %w", name, err)
	}
	h, err := windows.CreateEvent(nil, 0 /*autoReset*/, 0 /*initiallyNonSignaled*/, namePtr)
	if err != nil {
		return -1, fmt.Errorf("proc: 创建事件对象 %q: %w", name, err)
	}

	fd := int(evtEventFd.Add(1))
	evtEventsMu.Lock()
	evtEvents[fd] = h
	evtEventNames[fd] = name
	evtEventsMu.Unlock()
	return fd, nil
}

// evtEventNames 记录伪 fd → 对象名（供注入子进程环境变量）。
var evtEventNames = map[int]string{}

// EvtfdNotify 唤醒等待者（post-and-forget）。
//
// SetEvent 不阻塞，满足 §3.6 约束 B（Bus.Publish 路径上绝不等待）。
func EvtfdNotify(efd int) {
	evtEventsMu.Lock()
	h, ok := evtEvents[efd]
	evtEventsMu.Unlock()
	if !ok {
		return
	}
	// 忽略错误：句柄有效时 SetEvent 不会失败
	windows.SetEvent(h)
}

// evtfdReadFile 在 Windows 上返回 nil。
//
// 内核侧不消费事件环（只写入），消费在插件进程里由模板的
// windowsEvtWaiter 完成。这个函数只为跨平台签名存在。
func evtfdReadFile(efd int) *os.File { return nil }

// evtEventNameOf 返回某个伪 fd 对应的 Event 对象名（供注入子进程环境变量）。
func evtEventNameOf(efd int) string {
	evtEventsMu.Lock()
	defer evtEventsMu.Unlock()
	return evtEventNames[efd]
}

// evtfdClose 关闭 Event 句柄。
func evtfdClose(efd int) {
	evtEventsMu.Lock()
	h, ok := evtEvents[efd]
	delete(evtEvents, efd)
	delete(evtEventNames, efd)
	evtEventsMu.Unlock()
	if ok {
		windows.CloseHandle(h)
	}
}
