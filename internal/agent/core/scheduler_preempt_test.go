package core

// M3b 验收测试：四级优先级 + 严格大于抢占 + 现场保存/恢复 + suspendPool。
//
// 设计依据 docs/zh/input-scheduler-design.md §11（P1–P4、R1、R5、D1T、Q3）。
//
// 测试手法：用一个「第一次调用阻塞到 ctx 取消、之后按脚本返回」的 provider，
// 让测试可以确定性地把运行任务停在 S_LLM 上，再注入中断观察让位与恢复。

import (
	"context"
	"sync"
	"testing"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// preemptProvider 第 1 次 Chat 阻塞直到 ctx 取消；第 2 次起返回脚本。
type preemptProvider struct {
	mu        sync.Mutex
	calls     int
	entered   chan struct{}
	enteredOn sync.Once
	responses []string
}

func newPreemptProvider(responses ...string) *preemptProvider {
	return &preemptProvider{entered: make(chan struct{}), responses: responses}
}

func (p *preemptProvider) Name() string { return "preempt" }

func (p *preemptProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()

	if n == 1 {
		p.enteredOn.Do(func() { close(p.entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	i := n - 2
	if i < len(p.responses) {
		return &agentAPI.CompletionResponse{Content: p.responses[i]}, nil
	}
	return &agentAPI.CompletionResponse{Content: "done"}, nil
}

func (p *preemptProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return nil, context.Canceled
}
func (p *preemptProvider) MaxContextTokens() int { return 8192 }

func (p *preemptProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newPreemptAgent(t *testing.T, sp agentAPI.Provider) *Agent {
	t.Helper()
	return New(AgentConfig{
		ID:              "preempt",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
}

func enqueueTask(t *testing.T, a *Agent, level Level, source, content string) (*Task, *agentIO.InputEvent) {
	t.Helper()
	evt, _ := textEvent(source, content)
	task := &Task{Kind: TaskKindInput, Level: level, Event: evt, EnqueuedAt: time.Now()}
	if !a.sched.enqueue(task) {
		t.Fatal("入队失败")
	}
	return task, evt
}

// P1 + R1 + R5 + D1=A：高优先级抢占 → 挂起在 S_LLM → 中断任务带只读前缀 →
// 恢复后从 S_LLM 重发，且原任务的 msgs 未被改动。
func TestPreempt_HigherPreemptsAndResumes(t *testing.T) {
	sp := newPreemptProvider("intr-done", "low-done")
	a := newPreemptAgent(t, sp)

	lowTask, _ := enqueueTask(t, a, LevelBackground, "qq", "低优先级任务")
	lt, _, kind := a.sched.nextRef()
	if kind != nextReady || lt != lowTask {
		t.Fatalf("应取到低优先级任务，kind=%v", kind)
	}

	done := make(chan struct{})
	go func() { a.executeNewTask(lt); close(done) }()

	select {
	case <-sp.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider 第 1 次调用未发生")
	}

	// 注入 L4 中断（cli）
	intrEvt, _ := textEvent("cli", "紧急打断")
	intrEvt.Payload["interrupt"] = true
	if !a.sched.requestPreempt(intrEvt, LevelCritical) {
		t.Fatal("L4 应请求抢占并返回 true（应取消 LLM）")
	}
	a.cancelCurrentLLM()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("低优先级任务未在取消后挂起")
	}

	snap := a.DumpScheduler()
	if snap.Running != nil {
		t.Fatal("挂起后不应还有 running")
	}
	if len(snap.SuspendPool) != 1 {
		t.Fatalf("suspendPool=%d，期望 1", len(snap.SuspendPool))
	}
	if len(snap.PendingInterrupts) != 1 {
		t.Fatalf("pendingInterrupts=%d，期望 1", len(snap.PendingInterrupts))
	}
	sf := snap.SuspendPool[0].Frame
	if sf.Terminal != terminalSuspended {
		t.Fatalf("挂起任务终态=%v，期望 terminalSuspended", sf.Terminal)
	}
	if sf.Step != StepLLM {
		t.Fatalf("应在 StepLLM 安全点挂起，实际 step=%v", sf.Step)
	}
	msgsBefore := len(sf.Msgs)

	// R5：第一次 LLM 调用被丢弃，未产生新消息。
	if sp.callCount() != 1 {
		t.Fatalf("挂起前 LLM 调用=%d，期望 1（不完整请求被丢弃）", sp.callCount())
	}

	// Q3：三集合统一比较 → 下一轮取中断（L4 > L1）。
	it, _, k := a.sched.nextRef()
	if k != nextPending || it.Level != LevelCritical {
		t.Fatalf("应取到 pending 中断，kind=%v level=%v", k, it.Level)
	}
	// D1=A：抢占式中断任务继承被打断任务的只读前缀。
	if len(it.SeedMsgs) == 0 {
		t.Fatal("抢占式中断任务必须继承只读前缀（D1=A）")
	}
	a.executeNewTask(it)
	if len(a.DumpScheduler().PendingInterrupts) != 0 {
		t.Fatal("中断任务执行后 pendingInterrupts 应清空")
	}

	// R1：恢复被抢占任务；msgs 与被抢占前逐字节一致（长度不变），并从 S_LLM 重发。
	rt, rf, k2 := a.sched.nextRef()
	if k2 != nextSuspended || rt != lowTask {
		t.Fatalf("应恢复被抢占任务，kind=%v", k2)
	}
	if rf.Step != StepLLM {
		t.Fatalf("恢复游标=%v，期望 StepLLM", rf.Step)
	}
	if len(rf.Msgs) != msgsBefore {
		t.Fatalf("恢复后 msgs 长度=%d，期望 %d（不得被中断污染）", len(rf.Msgs), msgsBefore)
	}
	a.resumeTask(rt, rf)

	if sp.callCount() != 3 {
		t.Fatalf("LLM 总调用=%d，期望 3（丢弃 1 + 中断 1 + 恢复 1）", sp.callCount())
	}
	snap = a.DumpScheduler()
	if len(snap.SuspendPool) != 0 || snap.Running != nil {
		t.Fatalf("全部结束后应无挂起与运行任务：%+v", snap)
	}
	if snap.Stats.Executed != 2 {
		t.Fatalf("Executed=%d，期望 2（中断任务 + 被抢占任务）", snap.Stats.Executed)
	}
}

// P2/P3：同级与更低级都不得抢占，请求进 pendingInterrupts。
func TestPreempt_LowerOrEqualDoesNotPreempt(t *testing.T) {
	sp := newPreemptProvider("low-done", "intr-done")
	a := newPreemptAgent(t, sp)

	lowTask, _ := enqueueTask(t, a, LevelInteractive, "cli", "运行中的 L3")
	if _, _, kind := a.sched.nextRef(); kind != nextReady {
		t.Fatal("应取到运行任务")
	}

	done := make(chan struct{})
	go func() { a.executeNewTask(lowTask); close(done) }()
	select {
	case <-sp.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider 未被调用")
	}

	// 同级 L3
	e1, _ := textEvent("webui", "同级打断")
	if a.sched.requestPreempt(e1, LevelInteractive) {
		t.Fatal("同级不得抢占")
	}
	// 更低级 L1
	e2, _ := textEvent("system", "低优先级打断")
	if a.sched.requestPreempt(e2, LevelBackground) {
		t.Fatal("更低级不得抢占")
	}
	if a.sched.preemptGrantedFor(LevelInteractive) {
		t.Fatal("未 arm 让位信号，preemptGrantedFor 应为 false")
	}

	// 运行任务没有被取消，仍在等它自己的 ctx；用取消让它收尾，便于清理。
	a.cancelCurrentLLM()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("运行任务未结束")
	}
	if len(a.DumpScheduler().PendingInterrupts) != 2 {
		t.Fatalf("两条未抢占中断都应保留在 pendingInterrupts，实际 %d",
			len(a.DumpScheduler().PendingInterrupts))
	}
}

// D1T：suspendPool 满时不再下潜（canSuspend=false），让位信号也不会 arm。
func TestPreempt_DepthCapBlocksSuspension(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	if a.sched.maxSuspendDepth != 4 {
		t.Fatalf("默认深度上限=%d，期望 4", a.sched.maxSuspendDepth)
	}
	frame := func() *TaskFrame { return a.newTaskFrame("x", a.stageCtxFromInput("x", "", "")) }
	for i := 0; i < a.sched.maxSuspendDepth; i++ {
		a.sched.suspend(&Task{ID: uint64(i + 1), Level: LevelBackground}, frame())
	}
	if a.sched.canSuspend() {
		t.Fatal("深度已达上限，canSuspend 应为 false")
	}
	// 超限兜底：仍保留帧（不丢副作用记录），但计数 Rejected。
	before := a.DumpScheduler().Stats.Rejected
	a.sched.suspend(&Task{ID: 99, Level: LevelBackground}, frame())
	if a.DumpScheduler().Stats.Rejected != before+1 {
		t.Fatal("超限挂起必须计数 Rejected")
	}
	if len(a.DumpScheduler().SuspendPool) != a.sched.maxSuspendDepth+1 {
		t.Fatal("兜底路径必须保留帧而不是丢弃")
	}
}

// 中断不丢：空闲时请求抢占 → 不 arm 信号，但请求进 pendingInterrupts 并被选出。
func TestPreempt_IdleInterruptIsQueuedNotLost(t *testing.T) {
	a := newPreemptAgent(t, &scriptProvider{script: []*agentAPI.CompletionResponse{{Content: "已处理中断"}}})

	evt, respCh := textEvent("qq", "空闲时的中断")
	if a.sched.requestPreempt(evt, LevelMessage) {
		t.Fatal("空闲时不应请求取消 LLM（没有运行任务）")
	}
	if len(a.DumpScheduler().PendingInterrupts) != 1 {
		t.Fatal("空闲时的中断必须进 pendingInterrupts")
	}
	task, _, kind := a.sched.nextRef()
	if kind != nextPending || task.Level != LevelMessage {
		t.Fatalf("应取到待处理中断，kind=%v", kind)
	}
	a.executeNewTask(task)
	if len(respCh) != 1 {
		t.Fatal("中断任务应完成并回执")
	}
}

// 带媒体/中断标记的输入走新任务路径时不得污染下一个任务的尾部消息。
func TestPreempt_SeedPathDoesNotLeakInterruptFlag(t *testing.T) {
	sp := &scriptProvider{script: []*agentAPI.CompletionResponse{{Content: "ok"}}}
	a := newPreemptAgent(t, sp)

	f := a.newTaskFrame("打断文本", a.stageCtxFromInput("打断文本", "", ""))
	f.SeedMsgs = []agentAPI.Message{{Role: "system", Content: "S"}, {Role: "user", Content: "U"}}
	a.interruptInput = true
	if out := a.stepPrepare(f); out != outcomeContinue {
		t.Fatalf("seed 路径应继续，实际 %v", out)
	}
	if a.interruptInput {
		t.Fatal("seed 路径必须消费 interruptInput，否则下一个任务尾部会被误改")
	}
	last := f.Msgs[len(f.Msgs)-1]
	if last.Role != "user" || last.Content != "打断文本" {
		t.Fatalf("seed 路径尾部应为中断输入本身，实际 %+v", last)
	}
	if f.Step != StepLLM {
		t.Fatalf("seed 路径应直接进入 StepLLM，实际 %v", f.Step)
	}
}
