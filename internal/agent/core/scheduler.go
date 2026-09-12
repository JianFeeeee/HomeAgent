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

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
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

func (k TaskKind) String() string {
	switch k {
	case TaskKindInput:
		return "input"
	case TaskKindSelf:
		return "self"
	default:
		return "unknown"
	}
}

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

	// SeedMsgs 是抢占式中断任务的只读前缀（D1=A）：由被打断的任务在挂起时
	// 附上，使中断任务看得见「进行到哪一步」，但其产出不合并回原任务。
	SeedMsgs []agentAPI.Message

	// PreemptCount 是本任务被抢占的次数，用于饥饿防护：
	// effectiveLevel = min(L4, Level + min(PreemptCount, 2))。
	PreemptCount int
	// LastPreemptAt 是上次被抢占的时刻，用于抢占冷却。
	LastPreemptAt time.Time
}

// preemptPromotionCap 是抢占计数能带来的最大提升档数。
const preemptPromotionCap = 2

// preemptCooldown 是“刚被抢占过”的冷却期：期内不再被抢占，
// 避免高优先级流把同一任务反复打断到永不完结。
const preemptCooldown = 2 * time.Second

// effectiveLevel 返回任务的**有效**优先级（设计文档 §9 饥饿防护）。
//
// 被抢占越多的任务越“值钱”，从而逐步追上抢占它的流；封顶 L4，
// 因此它永远不会反过来抢占真正的紧急输入。
func effectiveLevel(t *Task) Level {
	p := t.PreemptCount
	if p > preemptPromotionCap {
		p = preemptPromotionCap
	}
	l := t.Level + Level(p)
	if l > LevelCritical {
		l = LevelCritical
	}
	return l
}

// SchedulerStats 是调度器的累计计数（可观测性，设计文档 §11 O2）。
type SchedulerStats struct {
	Enqueued uint64
	Executed uint64
	// Rejected 是因队列满（或深度超限）而未被接纳的次数。
	Rejected uint64
	// Suspended / Resumed 是挂起与恢复的次数。
	Suspended uint64
	Resumed   uint64
}

// SchedulerSnapshot 是调度器的原子快照。
type SchedulerSnapshot struct {
	Running           *Task
	Queue             []*Task
	PendingInterrupts []*Task
	SuspendPool       []*suspendedTask
	Stats             SchedulerStats
	MaxSuspendDepth   int
}

// schedulerStatus 把快照转成对外的状态 DTO（不暴露帧内容）。
func (a *Agent) schedulerStatus() sdk.SchedulerStatus {
	if a.sched == nil {
		return sdk.SchedulerStatus{}
	}
	snap := a.DumpScheduler()
	out := sdk.SchedulerStatus{
		ReadyQueueDepth:   len(snap.Queue),
		PendingInterrupts: len(snap.PendingInterrupts),
		SuspendPool:       len(snap.SuspendPool),
		MaxSuspendDepth:   snap.MaxSuspendDepth,
		Enqueued:          snap.Stats.Enqueued,
		Executed:          snap.Stats.Executed,
		Rejected:          snap.Stats.Rejected,
		Suspended:         snap.Stats.Suspended,
		Resumed:           snap.Stats.Resumed,
		Preempted:         snap.Stats.Suspended,
	}
	if snap.Running != nil {
		out.Running = &sdk.SchedulerTask{
			ID: snap.Running.ID, Level: int(snap.Running.Level), Kind: snap.Running.Kind.String(),
		}
	}
	return out
}

type scheduler struct {
	mu       sync.Mutex
	queue    []*Task
	running  *Task
	seq      uint64
	stats    SchedulerStats
	maxQueue int

	// pendingInterrupts：因优先级不足（或运行任务在临界区）而未立即抢占的中断请求。
	// 与 readyQueue 分离：取出时以中断语义启动（设计文档 D3）。
	pendingInterrupts []*Task
	// suspendPool：被抢占后保存了现场、等待恢复的任务（**不是栈**，按优先级取）。
	suspendPool []*suspendedTask
	// preemptArmed/preemptLevel：运行任务的“让位信号”。
	// interruptLoop 只写这两个字段与 pendingInterrupts；帧永远只由调度器读写。
	preemptArmed bool
	preemptLevel Level
	// maxSuspendDepth：suspendPool 深度上限（设计文档 §6.3，默认 4）。
	maxSuspendDepth int
}

// suspendedTask 是一个被抢占任务的现场。
type suspendedTask struct {
	Task  *Task
	Frame *TaskFrame
}

// nextSelection 标识 nextRef 从哪个集合取出任务。
type nextSelection int

const (
	nextNone nextSelection = iota
	nextReady
	nextPending
	nextSuspended
)

func newScheduler(maxQueue int) *scheduler {
	if maxQueue <= 0 {
		maxQueue = 256
	}
	return &scheduler{maxQueue: maxQueue, maxSuspendDepth: 4}
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
//
// 保留该签名供已有测试使用；调度器自用 nextRef（需要区分是否携带现场）。
func (s *scheduler) next() *Task {
	t, _, _ := s.nextRef()
	return t
}

// nextRef 从三个集合中按统一排序键取出下一个任务。
//
// 设计文档 §4.1：高有效级先；同级先到先服务。挂起任务保留其**原始**入队时刻，
// 因此同级时天然倾向“先把旧任务做完”，抑制饥饿。
func (s *scheduler) nextRef() (*Task, *TaskFrame, nextSelection) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var bestTask *Task
	var bestFrame *TaskFrame
	bestKind := nextNone
	consider := func(t *Task, k nextSelection, fr *TaskFrame) {
		if bestTask == nil || taskBefore(t, bestTask) {
			bestTask, bestKind, bestFrame = t, k, fr
		}
	}
	for _, t := range s.queue {
		consider(t, nextReady, nil)
	}
	for _, t := range s.pendingInterrupts {
		consider(t, nextPending, nil)
	}
	for _, st := range s.suspendPool {
		consider(st.Task, nextSuspended, st.Frame)
	}
	if bestTask == nil {
		return nil, nil, nextNone
	}

	switch bestKind {
	case nextReady:
		s.queue = removeTask(s.queue, bestTask)
	case nextPending:
		s.pendingInterrupts = removeTask(s.pendingInterrupts, bestTask)
	case nextSuspended:
		for i, st := range s.suspendPool {
			if st.Task == bestTask {
				s.suspendPool = append(s.suspendPool[:i], s.suspendPool[i+1:]...)
				break
			}
		}
	}
	s.running = bestTask
	return bestTask, bestFrame, bestKind
}

func removeTask(list []*Task, target *Task) []*Task {
	for i, t := range list {
		if t == target {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// enqueueInterrupt 把一个未立即抢占的中断请求放进 pendingInterrupts。
//
// 有界：满了丢**最老**的一条并计数（中断是提示性输入，宁可丢旧保新）。
func (s *scheduler) enqueueInterrupt(t *Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	t.ID = s.seq
	if t.EnqueuedAt.IsZero() {
		t.EnqueuedAt = time.Now()
	}
	if len(s.pendingInterrupts) >= s.maxQueue {
		s.pendingInterrupts = s.pendingInterrupts[1:]
		s.stats.Rejected++
	}
	s.pendingInterrupts = append(s.pendingInterrupts, t)
}

// requestPreempt 登记一次中断请求。
//
// 返回 true 表示“应该尝试取消运行任务正在进行的可取消步骤（LLM 流式）”。
//
// 无论能否抢占，中断请求都进 pendingInterrupts——这样即使运行任务在抢占生效前
// 就正常结束，中断也不会丢（它会被 nextRef 按优先级选出）。
//
// 判据用**有效**优先级（饥饿防护），并受抢占冷却约束。
func (s *scheduler) requestPreempt(evt *agentIO.InputEvent, level Level) bool {
	s.mu.Lock()
	running := s.running
	canPreempt := false
	if running != nil && level > effectiveLevel(running) {
		if running.LastPreemptAt.IsZero() || time.Since(running.LastPreemptAt) >= preemptCooldown {
			canPreempt = true
			s.preemptArmed = true
			s.preemptLevel = level
		}
	}
	s.mu.Unlock()

	s.enqueueInterrupt(newInterruptTask(evt, level))
	return canPreempt
}

// preemptGrantedFor 报告运行任务是否应在当前安全点让位。
func (s *scheduler) preemptGrantedFor() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.preemptArmed || s.running == nil {
		return false
	}
	return s.preemptLevel > effectiveLevel(s.running)
}

func (s *scheduler) clearPreempt() {
	s.mu.Lock()
	s.preemptArmed = false
	s.preemptLevel = 0
	s.mu.Unlock()
}

// suspend 保存现场。
//
// 深度上限（设计文档 §6.3）：安全点上的 canSuspend 已提前拦下超限情况，
// 此处仅在竞态下兜底计数——绝不丢弃帧（帧丢了会丢副作用记录）。
func (s *scheduler) suspend(t *Task, f *TaskFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.suspendPool) >= s.maxSuspendDepth {
		s.stats.Rejected++
	}
	s.suspendPool = append(s.suspendPool, &suspendedTask{Task: t, Frame: f})
	s.stats.Suspended++
	// 饥饿防护：抢占计数 +1（提升有效级）并记录冷却起点。
	t.PreemptCount++
	t.LastPreemptAt = time.Now()
	if s.running == t {
		s.running = nil
	}

	// D1=A：把被抢占任务的只读前缀交给造成本次抢占的中断任务。
	// 选最高优先级的待处理中断；若它已有前缀（嵌套抢占）则不覆盖。
	if s.preemptArmed {
		var victim *Task
		for _, it := range s.pendingInterrupts {
			if it.Level < s.preemptLevel {
				continue
			}
			if victim == nil || taskBefore(victim, it) {
				victim = it
			}
		}
		if victim != nil && len(victim.SeedMsgs) == 0 {
			victim.SeedMsgs = append([]agentAPI.Message(nil), f.Msgs...)
		}
	}

	s.preemptArmed = false
	s.preemptLevel = 0
}

// canSuspend 报告还有下潜余量（安全点用它决定是否真的让位）。
func (s *scheduler) canSuspend() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.suspendPool) < s.maxSuspendDepth
}

// done 标记任务执行结束。
func (s *scheduler) done(t *Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == t {
		s.running = nil
	}
	// 任务正常结束：让位信号不再有意义（中断已在 pendingInterrupts 里）。
	s.preemptArmed = false
	s.preemptLevel = 0
	s.stats.Executed++
}

// currentLevel 返回当前正在执行任务的级别；无 running 时为默认级。
//
// 用于在 prepare 段把级别写进帧（抢占比较的基准）。
func (s *scheduler) currentLevel() Level {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running != nil {
		return s.running.Level
	}
	return DefaultLevel
}

// taskLevel 是内核的优先级策略：四级的来源（设计文档 §3.2）。
//
// 优先走注入的查找函数（配置表）；未命中则用通道名兜底：
// cli/webui/http 为人机交互（L3），system/_consolidation_ 为后台（L1），
// 其余一律默认级（L1）。显式才是特权：没有策略就不给抢占权。
func (a *Agent) taskLevel(source, channel string) Level {
	if a.priorityLookup != nil {
		if l := a.priorityLookup(source, channel); l >= LevelBackground && l <= LevelCritical {
			return l
		}
	}
	switch channel {
	case "cli", "webui", "http":
		return LevelInteractive
	case channelConsolidation, "system":
		return LevelBackground
	}
	switch source {
	case "cli", "webui":
		return LevelInteractive
	case "system":
		return LevelBackground
	}
	return DefaultLevel
}

// inCriticalSection 报告运行任务是否处于不可抢占区。
//
// M3b 只处理「整个任务不可抢占」的情形（记忆整理）。工具执行、ONNX、
// CAS 落盘属于**单步**临界区——它们由「只在 step 之间检查让位」天然保护，
// 不需要在这里列（M4 会把清单显式化）。
func (a *Agent) inCriticalSection() bool {
	return a.currentOutputChannel == channelConsolidation
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

// taskBefore 报告 x 是否应先于 y 执行（按**有效**优先级）。
func taskBefore(x, y *Task) bool {
	lx, ly := effectiveLevel(x), effectiveLevel(y)
	if lx != ly {
		return lx > ly
	}
	if !x.EnqueuedAt.Equal(y.EnqueuedAt) {
		return x.EnqueuedAt.Before(y.EnqueuedAt)
	}
	return x.ID < y.ID
}

func newInputTask(evt *agentIO.InputEvent) *Task {
	return &Task{Kind: TaskKindInput, Level: DefaultLevel, Event: evt, EnqueuedAt: time.Now()}
}

// newInterruptTask 把一个中断请求包装成任务。
func newInterruptTask(evt *agentIO.InputEvent, level Level) *Task {
	return &Task{Kind: TaskKindInput, Level: level, Event: evt, EnqueuedAt: time.Now()}
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
	snap.PendingInterrupts = append(snap.PendingInterrupts, a.sched.pendingInterrupts...)
	snap.SuspendPool = append(snap.SuspendPool, a.sched.suspendPool...)
	snap.MaxSuspendDepth = a.sched.maxSuspendDepth
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

		t, f, kind := a.sched.nextRef()
		if kind == nextNone {
			// 无待办：阻塞等新输入或退出。
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

		if kind == nextSuspended {
			a.resumeTask(t, f)
			continue
		}
		a.executeNewTask(t)
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

// executeTask 执行一个任务（测试与旧调用方的入口）；见 executeNewTask。
func (a *Agent) executeTask(t *Task) {
	a.executeNewTask(t)
}

// executeNewTask 执行一个**新建**任务，并做任务级 panic 隔离（不变量 I6）。
//
// 与改造前的差异（有意）：原 eventLoop 在 panic 后重启整个循环，
// 现在一个任务的 panic 只丢弃该任务，调度器与其它任务不受影响。
func (a *Agent) executeNewTask(t *Task) {
	var f *TaskFrame
	var out stepOutcome = outcomeDone

	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[agent] task#%d (%s) panic recovered: %v\n%s",
					t.ID, t.Level, r, debug.Stack())
			}
		}()
		switch t.Kind {
		case TaskKindInput:
			f, out = a.runInputTask(t.Event, t.SeedMsgs)
		case TaskKindSelf:
			f, out = a.runInputTask(selfEvent(t.Self), nil)
		}
	}()

	if out == outcomeSuspended && f != nil {
		a.sched.suspend(t, f)
		a.publishEvent(events.EventScheduler, map[string]interface{}{
			"action": "suspend", "task": t.ID, "level": int(t.Level),
		})
		return
	}
	a.sched.done(t)
}

// resumeTask 从保存的现场继续一个被抢占的任务。
//
// 关键：不重建帧、不重跑 prepare 段——否则会重复提交上下文与事件。
func (a *Agent) resumeTask(t *Task, f *TaskFrame) {
	a.sched.mu.Lock()
	a.sched.stats.Resumed++
	a.sched.mu.Unlock()
	a.publishEvent(events.EventScheduler, map[string]interface{}{
		"action": "resume", "task": t.ID, "level": int(t.Level),
	})
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] resume task#%d panic recovered: %v\n%s",
				t.ID, r, debug.Stack())
			a.sched.done(t)
		}
	}()

	out := a.runTaskSteps(f)
	if out == outcomeSuspended {
		a.sched.suspend(t, f)
		return
	}
	a.finishInputTask(f, out)
	a.sched.done(t)
}
