package proc

import (
	"fmt"
	"sync"
	"time"
)

// 跨进程锁：锁仲裁回归内核（§3.7 已裁定，实验 3 + 实验 9 支撑）。
//
// 为什么不用 robust pthread_mutex：
//   - PTHREAD_PROCESS_SHARED + ROBUST 属性 Go 标准库无等价物，引入它意味着
//     **为了一把锁保留 cgo**——与"C 整体退场"的目标冲突。
//   - 锁仲裁回内核后，持锁进程崩溃由 cmd.Wait()/stdio EOF 检测，内核代为释放；
//     实验 9 实测无死锁、**无需 EOWNERDEAD 处理**。
//   - 成本：一次 RPC 往返 19.4 µs（实验 3，20000 次测得）。stage handler 的加锁
//     频率很低（每次 stage 一两次，不是每字段一次），微秒级往返可忽略。
//
// 于是整个新架构可做到**完全无 cgo**。

// lockWaitTimeout 是插件申请 stage 锁的最长等待时间。
//
// 取 30s：stage handler 自身受 60s 工具超时约束（toolcall.go），锁等待
// 必须显著短于它，否则超时错误会指向错误的原因。超时返回错误而非
// 静默继续——**持锁失败下改写共享段会破坏并发正确性**。
const lockWaitTimeout = 30 * time.Second

// stageLock 是内核侧为单个 stage 执行持有的互斥体。
//
// 一次 RunStage 对应一个 stageLock 实例：同阶段并发扇出的所有插件
// （含跨进程的）在此排队，语义等价于今日内置插件共享
// *StageContext 的 sync.RWMutex——这正是"保留并发扇出原始设计"
// （§0.2 第 1 条：并发扇出是原始设计，不是缺陷）。
type stageLock struct {
	mu sync.Mutex

	// ownerMu 保护 owner/held，使 ForceRelease 能安全介入
	ownerMu sync.Mutex
	owner   string // 当前持锁的插件名，空表示未持有
	held    bool
}

func newStageLock() *stageLock { return &stageLock{} }

// Acquire 为 plugin 申请写锁，带超时。
//
// 同一插件重复 Acquire 会死锁（stage handler 不应嵌套加锁），
// 故显式拒绝并返回错误——比让插件挂死 30s 更容易排查。
func (l *stageLock) Acquire(plugin string) error {
	l.ownerMu.Lock()
	if l.held && l.owner == plugin {
		l.ownerMu.Unlock()
		return fmt.Errorf("proc: 插件 %s 重复申请 stage 锁（handler 内不应嵌套加锁）", plugin)
	}
	l.ownerMu.Unlock()

	acquired := make(chan struct{})
	go func() {
		l.mu.Lock()
		close(acquired)
	}()

	select {
	case <-acquired:
		l.ownerMu.Lock()
		l.owner = plugin
		l.held = true
		l.ownerMu.Unlock()
		return nil
	case <-time.After(lockWaitTimeout):
		// 等待超时：上面的 goroutine 可能随后拿到锁，必须让它能释放，
		// 否则锁永久泄漏。用一个补偿 goroutine 等它拿到后立刻放掉。
		go func() {
			<-acquired
			l.ownerMu.Lock()
			stillFree := !l.held
			l.ownerMu.Unlock()
			if stillFree {
				l.mu.Unlock()
			}
		}()
		return fmt.Errorf("proc: 插件 %s 申请 stage 锁超时（%s）", plugin, lockWaitTimeout)
	}
}

// Release 释放写锁。非持锁者调用返回错误（防止串扰）。
func (l *stageLock) Release(plugin string) error {
	l.ownerMu.Lock()
	if !l.held {
		l.ownerMu.Unlock()
		return fmt.Errorf("proc: 插件 %s 释放未持有的 stage 锁", plugin)
	}
	if l.owner != plugin {
		owner := l.owner
		l.ownerMu.Unlock()
		return fmt.Errorf("proc: 插件 %s 试图释放 %s 持有的 stage 锁", plugin, owner)
	}
	l.owner = ""
	l.held = false
	l.ownerMu.Unlock()
	l.mu.Unlock()
	return nil
}

// ForceRelease 在插件进程崩溃/退出时由内核代为释放其持有的锁（实验 9 的自愈机制）。
//
// 返回是否实际释放了锁。**这是"无需 robust mutex"的核心**：
// 锁的所有权在内核进程，插件死亡由 cmd.Wait()/stdio EOF 检测到，
// 内核直接解锁，不存在"持锁者死亡导致全局死锁"。
func (l *stageLock) ForceRelease(plugin string) bool {
	l.ownerMu.Lock()
	if !l.held || l.owner != plugin {
		l.ownerMu.Unlock()
		return false
	}
	l.owner = ""
	l.held = false
	l.ownerMu.Unlock()
	l.mu.Unlock()
	return true
}

// Owner 返回当前持锁插件名（诊断用）。
func (l *stageLock) Owner() string {
	l.ownerMu.Lock()
	defer l.ownerMu.Unlock()
	if !l.held {
		return ""
	}
	return l.owner
}
