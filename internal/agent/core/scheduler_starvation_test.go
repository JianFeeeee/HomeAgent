package core

// M5 验收测试：饥饿防护（抢占计数提升有效级 + 抢占冷却）。
//
// 设计依据 docs/zh/input-scheduler-design.md §9、§11.5（G1/G2）。
//
// 为什么需要：固定四级 + 「严格大于才抢占」下，一条 L4 流可以反复打断同一个
// L1 任务，使它永不完结。提升被抢占者的**有效**优先级，让它在竞争排队时
// 逐步追上；封顶 L4，因此它永远抢不过真正的紧急输入（紧急输入本身不被抢占）。

import (
	"testing"
	"time"
)

func TestStarvation_EffectiveLevelPromotion(t *testing.T) {
	base := &Task{Level: LevelBackground}
	if got := effectiveLevel(base); got != LevelBackground {
		t.Fatalf("未抢占时有效级=%v，期望 L1", got)
	}

	base.PreemptCount = 1
	if got := effectiveLevel(base); got != LevelMessage {
		t.Fatalf("被抢占 1 次后有效级=%v，期望 L2", got)
	}
	base.PreemptCount = 2
	if got := effectiveLevel(base); got != LevelInteractive {
		t.Fatalf("被抢占 2 次后有效级=%v，期望 L3", got)
	}
	base.PreemptCount = 99
	if got := effectiveLevel(base); got != LevelInteractive {
		t.Fatalf("提升应封顶在 +2 档，实际 %v", got)
	}

	// 封顶 L4：L3 任务被多次抢占也不会超过紧急级。
	high := &Task{Level: LevelInteractive, PreemptCount: 99}
	if got := effectiveLevel(high); got != LevelCritical {
		t.Fatalf("L3 提升后应封顶为 L4，实际 %v", got)
	}
}

func TestStarvation_CooldownBlocksImmediateRepreempt(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	low := &Task{ID: 1, Level: LevelBackground, EnqueuedAt: time.Now()}
	a.sched.enqueue(low)
	a.sched.nextRef() // running = low

	e1, _ := textEvent("qq", "第一次打断")
	if !a.sched.requestPreempt(e1, LevelMessage) {
		t.Fatal("L2 应能抢占 L1（首次）")
	}
	a.sched.suspend(low, a.newTaskFrame("x", a.stageCtxFromInput("x", "", "")))
	if low.PreemptCount != 1 {
		t.Fatalf("PreemptCount=%d，期望 1", low.PreemptCount)
	}
	if low.LastPreemptAt.IsZero() {
		t.Fatal("挂起必须记录 LastPreemptAt（冷却起点）")
	}

	// 冷却期内：即使 L4 也不得再抢占。
	a.sched.mu.Lock()
	a.sched.running = low
	a.sched.mu.Unlock()

	e2, _ := textEvent("cli", "冷却期内的紧急打断")
	if a.sched.requestPreempt(e2, LevelCritical) {
		t.Fatal("抢占冷却期内不得再抢占")
	}
	if a.sched.preemptGrantedFor() {
		t.Fatal("冷却期内不得 arm 让位信号")
	}
}

func TestStarvation_PromotionBlocksSameLevelPreempt(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	low := &Task{ID: 1, Level: LevelBackground, EnqueuedAt: time.Now()}
	a.sched.enqueue(low)
	a.sched.nextRef()
	// 模拟「已被抢占过一次」：有效级 = L2。
	low.PreemptCount = 1
	low.LastPreemptAt = time.Now().Add(-time.Hour) // 冷却已过

	e1, _ := textEvent("qq", "同级打断")
	if a.sched.requestPreempt(e1, LevelMessage) {
		t.Fatal("有效级 L2 时，L2 中断不得抢占（严格大于才抢占）")
	}

	e2, _ := textEvent("cli", "更高级打断")
	if !a.sched.requestPreempt(e2, LevelInteractive) {
		t.Fatal("L3 应能抢占有效级 L2")
	}
	if !a.sched.preemptGrantedFor() {
		t.Fatal("L3 > 有效 L2，应已 arm")
	}
}

// 选择函数必须用有效级：被抢占过的任务在排队时应当优先于同级/更低的任务。
func TestStarvation_SelectionUsesEffectiveLevel(t *testing.T) {
	base := time.Now()
	promoted := &Task{ID: 1, Level: LevelBackground, PreemptCount: 2, EnqueuedAt: base} // 有效 L3
	normal := &Task{ID: 2, Level: LevelMessage, EnqueuedAt: base.Add(time.Second)}      // L2

	if !taskBefore(promoted, normal) {
		t.Fatal("被抢占 2 次的 L1（有效 L3）应先于 L2 执行")
	}
	if taskBefore(normal, promoted) {
		t.Fatal("选择函数不得只看基础级")
	}

	// 提升不改变调度器自身的排序稳定性：同为有效级时按入队时刻。
	a := &Task{ID: 3, Level: LevelBackground, PreemptCount: 1, EnqueuedAt: base.Add(2 * time.Second)} // 有效 L2
	b := &Task{ID: 4, Level: LevelMessage, EnqueuedAt: base.Add(time.Second)}                          // L2，更早
	if !taskBefore(b, a) {
		t.Fatal("同有效级时应先到先服务")
	}
}
