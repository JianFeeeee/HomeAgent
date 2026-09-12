package core

// 输入调度器（M2：骨架）。
//
// 设计依据 docs/zh/input-scheduler-design.md。
//
// M2 只建立结构，不引入抢占：
//   - 显式的 readyQueue 与 Task 抽象（取代 eventLoop 里隐式的 channel 排队）；
//   - 统一的排序键 (-Level, EnqueuedAt, ID)（设计文档 §4.1）；
//   - 原子快照 DumpScheduler() 与计数（可观测性）；
//   - **每任务 panic 隔离**：panic 只使该任务失败，调度器本身存活（不变量 I6）。
//
// M2 全部任务都是 LevelBackground（默认级），因此排序结果等价于 FIFO——
// 与改造前的 channel 语义逐条一致。抢占、suspendPool、pendingInterrupts、
// 任务级回执在 M3–M6 加入。
//
// 并发模型（不变量 I2）：readyQueue/running/stats 只由 schedulerLoop 写，
// 外部只读——读取一律经 DumpScheduler() 加锁取快照。

import (
	"log"
	"runtime/debug"
	"sync"
	"time"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// Level 是任务优先级，由内核预定义四级（设计文档 §3.1）。
//
// 取值域刻意只有四档：不引入任意整数，避免"9 级比 4 级大但没人知道怎么排"。
type Level int

const (
	// LevelBackground 后台维护：心跳蒸馏/归档/合并/复审、子任务、consolidation。
	LevelBackground Level = 1
	// LevelMessage 异步消息：QQ/微信等入站消息、插件通知。
	LevelMessage Level = 2
	// LevelInteractive 人机交互：用户在 CLI/WebUI 的直接对话。
	LevelInteractive Level = 3
	// LevelCritical 紧急打断：显式打断、系统告警、安全类中断。
	LevelCritical Level = 4
)

// DefaultLevel 是未显式声明时的优先级。
//
// 取最低级是刻意的：**显式才是特权**，新插件不会默认拿到抢占权。
const DefaultLevel = LevelBackground

func (l Level) String() string {
	switch l {
	case LevelBackground:
		return "L1-background"
	case LevelMessage:
		return "L2-message"
	case LevelInteractive:
		return "L3-interactive"
	case LevelCritical:
		return "L4-critical"
	default:
		return "L?-unknown"
	}
}

// TaskKind 区分任务来源。
type TaskKind int

const (
	// TaskKindInput 来自 io.InputChan（外部/插件注入的输入）。
	TaskKindInput TaskKind = iota
	// TaskKindSelf 来自 selfInputCh（内核自循环：记忆整理、子任务通知）。
	TaskKindSelf
)

// Task 是调度器的最小单位。
//
// M2 只承载"一份待处理的输入"；M3 起把 TaskFrame（现场）挂上来，
// 使其成为可挂起/可恢复的执行单元。
type Task struct {
	ID         uint64
	Level      Level
	Kind       TaskKind
	EnqueuedAt time.Time

	Event *agentIO.InputEvent // Kind == TaskKindInput
	Self  selfInputMsg        // Kind == TaskKindSelf
}

// SchedulerStats 是调度器的累计计数（可观测性，设计文档 §11 O2）。
type SchedulerStats struct {
	Enqueued uint64
	Executed uint64
	// Rejected 是因队列满而未入队的次数。M2 由泵入侧节流，正常为 0；
	// 出现非 0 说明消费端长期慢于生产端。
	Rejected uint64
}

// SchedulerSnapshot 是调度器的原子快照。
type SchedulerSnapshot struct {
	Running *Task
	Queue   []*Task
	Stats   SchedulerStats
}

type scheduler struct {
	mu       sync.Mutex
	queue    []*Task
	running  *Task
	seq      uint64
	stats    SchedulerStats
	maxQueue int
}

func newScheduler(maxQueue int) *scheduler {
	if maxQueue <= 0 {
		maxQueue = 256
	}
	return &scheduler{maxQueue: maxQueue}
}

// hasRoom 报告就绪队列是否还能接收任务。泵入侧据此节流：
// 队列满则停止从 channel 取，让背压落回 channel 本身。
func (s *scheduler) hasRoom() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue) < s.maxQueue
}

// enqueue 入队；队列满返回 false（调用方负责计数）。
func (s *scheduler) enqueue(t *Task) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) >= s.maxQueue {
		s.stats.Rejected++
		return false
	}
	s.seq++
	t.ID = s.seq
	if t.EnqueuedAt.IsZero() {
		t.EnqueuedAt = time.Now()
	}
	s.stats.Enqueued++
	s.queue = append(s.queue, t)
	return true
}

// next 取出下一个要执行的任务；队列空返回 nil。
func (s *scheduler) next() *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return nil
	}
	i := pickTaskIndex(s.queue)
	t := s.queue[i]
	s.queue = append(s.queue[:i], s.queue[i+1:]...)
	s.running = t
	return t
}

// done 标记任务执行结束。
func (s *scheduler) done(t *Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == t {
		s.running = nil
	}
	s.stats.Executed++
}

// pickTaskIndex 返回下一个要执行的任务下标（设计文档 §4.1 的选择函数）。
//
// 排序键：优先级降序 → 入队时刻升序 → ID 升序。
// 纯函数：便于对抢占/优先级矩阵做确定性单测。
func pickTaskIndex(q []*Task) int {
	best := 0
	for i := 1; i < len(q); i++ {
		if taskBefore(q[i], q[best]) {
			best = i
		}
	}
	return best
}

// taskBefore 报告 x 是否应先于 y 执行。
func taskBefore(x, y *Task) bool {
	if x.Level != y.Level {
		return x.Level > y.Level
	}
	if !x.EnqueuedAt.Equal(y.EnqueuedAt) {
		return x.EnqueuedAt.Before(y.EnqueuedAt)
	}
	return x.ID < y.ID
}

func newInputTask(evt *agentIO.InputEvent) *Task {
	return &Task{Kind: TaskKindInput, Level: DefaultLevel, Event: evt, EnqueuedAt: time.Now()}
}

func newSelfTask(msg selfInputMsg) *Task {
	return &Task{Kind: TaskKindSelf, Level: DefaultLevel, Self: msg, EnqueuedAt: time.Now()}
}

// DumpScheduler 返回调度器的原子快照（供状态页/测试断言）。
func (a *Agent) DumpScheduler() SchedulerSnapshot {
	if a.sched == nil {
		return SchedulerSnapshot{}
	}
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	snap := SchedulerSnapshot{Running: a.sched.running, Stats: a.sched.stats}
	snap.Queue = append(snap.Queue, a.sched.queue...)
	return snap
}

// schedulerLoop 是唯一的任务执行者（取代原 eventLoop 的输入处理）。
func (a *Agent) schedulerLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] schedulerLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.schedulerLoop()
		}
	}()

	for {
		a.pumpInbox()

		t := a.sched.next()
		if t == nil {
			// 就绪队列空：阻塞等新输入或退出。
			select {
			case evt := <-a.io.InputChan():
				a.sched.enqueue(newInputTask(evt))
			case msg := <-a.selfInputCh:
				a.sched.enqueue(newSelfTask(msg))
			case <-a.ctx.Done():
				return
			}
			continue
		}
		a.executeTask(t)
	}
}

// pumpInbox 把 channel 里**已就绪**的输入搬进就绪队列（非阻塞）。
//
// 为什么不直接边收边执行：先把已到达的输入收进队列，选择函数才有意义——
// M3 起抢占必然要看"队列里还压着什么"，而 channel 不是可枚举的结构。
//
// 队列满即停止泵入（背压落回 channel，语义与设计文档 §4.4 一致）。
func (a *Agent) pumpInbox() {
	for a.sched.hasRoom() {
		select {
		case evt := <-a.io.InputChan():
			a.sched.enqueue(newInputTask(evt))
		case msg := <-a.selfInputCh:
			a.sched.enqueue(newSelfTask(msg))
		case <-a.ctx.Done():
			return
		default:
			return
		}
	}
}

// executeTask 执行一个任务，并做**任务级 panic 隔离**（不变量 I6）。
//
// 与改造前的差异（有意）：原 eventLoop 在 panic 后重启整个循环，
// 现在一个任务的 panic 只丢弃该任务，调度器与其它任务不受影响。
func (a *Agent) executeTask(t *Task) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[agent] task#%d (%s) panic recovered: %v\n%s",
					t.ID, t.Level, r, debug.Stack())
			}
		}()
		switch t.Kind {
		case TaskKindInput:
			a.handleInput(t.Event)
		case TaskKindSelf:
			a.handleSelfInput(t.Self)
		}
	}()
	a.sched.done(t)
}
