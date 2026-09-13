package core

// 抢占场景下的**输出通道路由**：被打断任务恢复后，回复必须回到它自己的输出通道。
//
// 回归判据（做驻留式子 agent 前必须成立）：**内核不持有"当前通道"可变状态**。
// 曾经有 agent 级字段 a.currentOutputChannel：只在 prepare 段写入，而被打断任务
// 恢复时不重新 prepare（resumeTask 只 rebase 前缀），于是中断任务 prepare 时把它
// 覆盖成自己的通道，被恢复的任务再把回复发到**中断任务的通道**上——两任务串台。
// N0 已删除该字段：通道一律从输入事件/帧推导（outputChannelOf / f.OutputChannel）。
//
// 每任务回执（evt.ResponseCh，Target=evt.Source）不受影响，所以既有测试全绿；
// 但按通道投递（OutputEvent.OutputChannel / events.EventAgentOutput 的 channel）
// 是插件渲染给用户的路径，它会串。

import (
	"testing"
	"time"
)

func TestPreempt_ResumeKeepsOwnOutputChannel(t *testing.T) {
	sp := newPreemptProvider("intr-done", "low-done")
	a := newPreemptAgent(t, sp)

	// 排队任务，来源与输出通道都是 qq。
	lowEvt, lowCh := textEvent("qq", "低优先级任务")
	lowTask := newInputTask(lowEvt)
	if !a.sched.enqueue(lowTask) {
		t.Fatal("入队失败")
	}
	if lowEvt.OutputChannel != "qq" {
		t.Fatalf("前置条件不成立：OutputChannel=%q", lowEvt.OutputChannel)
	}

	lt, _, kind := a.sched.nextRef()
	if kind != nextReady {
		t.Fatalf("应取到排队任务，kind=%v", kind)
	}
	done := make(chan struct{})
	go func() { a.executeNewTask(lt); close(done) }()

	select {
	case <-sp.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider 未被调用")
	}

	// cli 中断抢占（L3）：任务被挂起。
	intrEvt, intrCh := textEvent("cli", "紧急打断")
	intrEvt.Payload["interrupt"] = true
	if !a.sched.requestPreempt(intrEvt, LevelInteractive) {
		t.Fatal("L3 中断应能抢占排队任务")
	}
	a.cancelCurrentLLM()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("被打断任务未挂起")
	}

	// 中断任务先运行（它会 prepare，通道是 cli）。
	it, _, k := a.sched.nextRef()
	if k != nextImmediate {
		t.Fatalf("应取到立即运行的中断，kind=%v", k)
	}
	a.executeNewTask(it)
	if len(intrCh) != 1 {
		t.Fatalf("中断任务应回执一次，实际 %d", len(intrCh))
	}
	// 注意：这里**刻意**不再有任何"内核当前通道"可断言 —— 该字段已删除，
	// 通道只跟着输入事件与帧走。下面断言的就是这个性质本身。

	// 恢复被抢占任务：它不重新 prepare，只能靠帧里记着自己的通道。
	rt, rf, k2 := a.sched.nextRef()
	if k2 != nextSuspended {
		t.Fatalf("应恢复被抢占任务，kind=%v", k2)
	}
	a.resumeTask(rt, rf)

	select {
	case out := <-lowCh:
		if out.OutputChannel != "qq" {
			t.Fatalf("被打断任务恢复后的输出通道=%q，期望 qq —— 被中断任务的通道覆盖了 agent 级字段（两任务串台）",
				out.OutputChannel)
		}
		if out.Target != "qq" {
			t.Fatalf("回执 Target=%q，期望 qq", out.Target)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("被恢复任务未回执")
	}
}
