package core

// M4 验收测试：临界区语义显式化 + 抢占延迟到安全点 + 批次不再被中断放弃。
//
// 设计依据 docs/zh/input-scheduler-design.md §4.3（临界区）、§11.1（P5/P6）。
//
// 关键结构事实：让位检查**只在 step 之间**进行，因此任何正在执行的 step
// （工具 RPC、ONNX、CAS 落盘）天然不可抢占——中断只能等它返回。

import (
	"sync"
	"testing"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// P5/P6：工具执行期间到达的高优先级中断不得立即抢占；必须等工具返回后的安全点。
func TestPreempt_DeferredDuringToolExec(t *testing.T) {
	sh := NewStageHost()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sh.RegisterTool("t_slow", sdk.ToolDef{Name: "t_slow", Plugin: "t"}, func(args map[string]interface{}) (interface{}, error) {
		once.Do(func() { close(entered) })
		<-release
		return "slow-done", nil
	})

	sp := &scriptProvider{script: []*agentAPI.CompletionResponse{
		{Content: "", ToolCalls: []agentAPI.ToolCall{tc("c1", "t_slow")}}, // 低优先级任务调用慢工具
		{Content: "low-done"},  // 恢复后收尾
		{Content: "intr-done"}, // 中断任务
	}}
	a := New(AgentConfig{
		ID:              "crit",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       sh,
	})

	if _, _ = enqueueTask(t, a, LevelBackground, "qq", "低优先级任务"); true {
	}
	lt, _, _ := a.sched.nextRef()
	done := make(chan struct{})
	go func() { a.executeNewTask(lt); close(done) }()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("慢工具未被调用")
	}

	// 工具执行中注入 L4 中断。
	intrEvt, _ := textEvent("cli", "紧急打断")
	intrEvt.Payload["interrupt"] = true
	if !a.sched.requestPreempt(intrEvt, LevelCritical) {
		t.Fatal("L4 应 arm 让位信号")
	}
	// 关键断言：信号已 arm，但任务仍在工具里 —— 绝不能挂起。
	if !a.sched.preemptGrantedFor() {
		t.Fatal("让位信号应已 arm")
	}
	if a.DumpScheduler().Running == nil {
		t.Fatal("工具执行中不得挂起（StepToolExec 是临界区）")
	}
	if len(a.DumpScheduler().SuspendPool) != 0 {
		t.Fatal("工具执行中 suspendPool 应为空")
	}

	// 放行工具 → 工具返回后的安全点才挂起。
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("工具返回后未挂起")
	}

	snap := a.DumpScheduler()
	if len(snap.SuspendPool) != 1 {
		t.Fatalf("工具返回后 suspendPool=%d，期望 1", len(snap.SuspendPool))
	}
	if snap.SuspendPool[0].Frame.Step != StepToolAfter {
		t.Fatalf("应在工具执行后的安全点挂起（StepToolAfter），实际 %v", snap.SuspendPool[0].Frame.Step)
	}
	if len(snap.PendingInterrupts) != 1 {
		t.Fatalf("中断请求不得丢失，pendingInterrupts=%d", len(snap.PendingInterrupts))
	}
}

// _consolidation_ 整任务视为不可抢占（它直接改图库）。
func TestCriticalSection_ConsolidationMarked(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	a.currentOutputChannel = "cli"
	if a.inCriticalSection() {
		t.Fatal("普通通道不应被判为临界区")
	}
	a.currentOutputChannel = channelConsolidation
	if !a.inCriticalSection() {
		t.Fatal("记忆整理必须是不可抢占临界区")
	}
}

// 新语义：没有抢占时，同批的多个工具必须全部执行——不再有「中断放弃剩余批」。
func TestBatch_NotAbandonedWithoutPreemption(t *testing.T) {
	sh := NewStageHost()
	var mu sync.Mutex
	var ran []string
	reg := func(name string) {
		sh.RegisterTool(name, sdk.ToolDef{Name: name, Plugin: "t"}, func(args map[string]interface{}) (interface{}, error) {
			mu.Lock()
			ran = append(ran, name)
			mu.Unlock()
			return name + "-out", nil
		})
	}
	reg("t_a")
	reg("t_b")

	sp := &scriptProvider{script: []*agentAPI.CompletionResponse{
		{Content: "", ToolCalls: []agentAPI.ToolCall{tc("c1", "t_a"), tc("c2", "t_b")}},
		{Content: "全部完成"},
	}}
	a := New(AgentConfig{
		ID:              "batch",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       sh,
	})

	if _, _ = enqueueTask(t, a, LevelBackground, "cli", "跑两个工具"); true {
	}
	tt, _, _ := a.sched.nextRef()
	a.executeNewTask(tt)

	if len(ran) != 2 || ran[0] != "t_a" || ran[1] != "t_b" {
		t.Fatalf("同批工具应全部按序执行，实际 %v", ran)
	}
	snap := a.DumpScheduler()
	if len(snap.SuspendPool) != 0 || len(snap.PendingInterrupts) != 0 {
		t.Fatalf("无抢占时不应有挂起或待处理中断：%+v", snap)
	}
	if snap.Stats.Executed != 1 {
		t.Fatalf("Executed=%d，期望 1", snap.Stats.Executed)
	}
}
