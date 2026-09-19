package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
)

// newRootWithoutSchedulerLoop 造一个**不启动后台循环**的根 agent。
//
// 为什么测试必须用它：newRootWith 会 a.Start()，于是真实的 schedulerLoop
// 与测试**并发**跑，它会瞬间把测试排进队列的任务执行掉并清空 running
// ⇒ "主 agent 正忙"这个前提会被后台循环消掉，转投判定随机失效
// （实测：同一测试两次运行结果不同，一个过一个不过）。
// 本特性测的是**判定 + 搬运**这两步的语义，不需要真的把任务跑起来。
func newRootWithoutSchedulerLoop(t *testing.T) (*Agent, *memory.GraphDB) {
	t.Helper()
	dir := t.TempDir()
	main, err := memory.NewGraphDB(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	a := New(AgentConfig{
		ID:              "parent",
		Provider:        &countingProvider{},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
		Memory:          main,
		DataDir:         dir,
	})
	t.Cleanup(func() { a.Stop(); main.Close() })
	return a, main
}

// makeQueuedInput 造一条排队输入任务（Event 非空，Class=TaskQueued）。
func makeQueuedInput(id int) *Task {
	return newInputTask(&agentIO.InputEvent{
		RequestID:     "req",
		Source:        "qq",
		Type:          "text",
		OutputChannel: "qq",
		Payload:       map[string]interface{}{"content": "hello"},
	})
}

// 转投只应该动**纯排队输入**：中断任务带级别语义、self 任务是内核内部记账，
// 搬走它们会分别破坏中断阶梯与记忆整理。
func TestTakeQueuedInputsOnlyTakesQueuedInputs(t *testing.T) {
	s := newScheduler(64)
	// 混合：1 条排队输入 + 1 条中断 + 1 条 self + 3 条排队输入
	s.enqueue(makeQueuedInput(1))
	s.enqueue(newInterruptTask(&agentIO.InputEvent{Source: "qq", OutputChannel: "qq"}, LevelMessage))
	s.enqueue(newSelfTask(selfInputMsg{text: "distill", channel: "cli"}))
	s.enqueue(makeQueuedInput(2))
	s.enqueue(makeQueuedInput(3))
	s.enqueue(makeQueuedInput(4))

	if len(s.queue) != 6 {
		t.Fatalf("就绪队列应有 6 条（4 排队输入 + 1 中断 + 1 self），实际 %d", len(s.queue))
	}
	got := s.takeQueuedInputs(3)
	if len(got) != 3 {
		t.Fatalf("应取走 3 条排队输入，实际 %d", len(got))
	}
	for _, c := range got {
		if c.Event == nil {
			t.Fatal("取出的候选不得为空事件")
		}
	}
	// self 与中断必须还在
	var hasSelf, hasInterrupt bool
	for _, tt := range s.queue {
		if tt.Kind == TaskKindSelf {
			hasSelf = true
		}
		if tt.Class == TaskInterrupt {
			hasInterrupt = true
		}
	}
	if !hasSelf {
		t.Error("self 任务被误取（会破坏记忆整理）")
	}
	if !hasInterrupt {
		t.Error("中断任务被误取（会破坏中断阶梯）")
	}
}

// 不够量时**一条都不取**：拉起一个 agent 的成本不该为一条任务付。
// 这条保证「要么不动、要么成批移动」。
func TestTakeQueuedInputsIsAllOrNothing(t *testing.T) {
	s := newScheduler(64)
	s.enqueue(makeQueuedInput(1))
	s.enqueue(makeQueuedInput(2))

	if got := s.takeQueuedInputs(3); got != nil {
		t.Fatalf("不足 3 条时不应取走任何任务，实际取走 %d", len(got))
	}
	if len(s.queue) != 2 {
		t.Errorf("队列不应被改动，实际剩 %d", len(s.queue))
	}
}

// ★ 安全不变量：转投失败必须把任务**放回队列**。
// 吞掉一条输入比多处理一条更糟——用户会看到"消息发出去了却没人理"。
func TestRequeueFrontKeepsAllTasks(t *testing.T) {
	s := newScheduler(64)
	s.enqueue(makeQueuedInput(1))
	s.enqueue(makeQueuedInput(2))
	s.enqueue(makeQueuedInput(3))

	taken := s.takeQueuedInputs(3)
	if len(taken) != 3 {
		t.Fatalf("应取走 3 条，实际 %d", len(taken))
	}
	if len(s.queue) != 0 {
		t.Fatalf("取走后队列应空，实际 %d", len(s.queue))
	}

	s.requeueFront(taken)
	if len(s.queue) != 3 {
		t.Fatalf("★ 放回后必须一条不少：期望 3，实际 %d", len(s.queue))
	}
	// 放回的是**前端**：它们比队列里原有的一切都早
	s.enqueue(makeQueuedInput(4))
	if s.queue[len(s.queue)-1].Event.Payload["content"] != "hello" {
		t.Error("放回的任务应在队列前端")
	}
}

// 转投说明必须自己说清是系统做的：用户看到队列里出现一条没人发过的消息时，
// 唯一能解释这件事的就是这句话本身。
func TestOffloadNoticeExplainsItself(t *testing.T) {
	msg := offloadNotice(3, "offload-123")
	for _, want := range []string{"系统", "3 条", "offload-123", "转投"} {
		if !strings.Contains(msg, want) {
			t.Errorf("说明缺少 %q：%s", want, msg)
		}
	}
}

// 默认必须是**关闭**：自动拉起是内核替父做决策（设计 §7 的例外），
// 不能默默改变系统行为。
func TestOffloadDisabledByDefault(t *testing.T) {
	opts := DefaultOffloadOptions()
	if opts.Enabled {
		t.Error("默认必须关闭")
	}
	s := newScheduler(64)
	s.enqueue(makeQueuedInput(1))
	s.enqueue(makeQueuedInput(2))
	s.enqueue(makeQueuedInput(3))
	a := &Agent{sched: s}
	if n := a.offloadPendingTasks(opts); n != 0 {
		t.Errorf("关闭时不得转投，实际转了 %d", n)
	}
	if len(s.queue) != 3 {
		t.Errorf("关闭时队列不得被改动，实际 %d", len(s.queue))
	}
}

// 不忙（无运行任务）时不转投：没有"长任务占住"这个前提，排队就是正常的。
func TestOffloadSkippedWhenIdle(t *testing.T) {
	opts := DefaultOffloadOptions()
	opts.Enabled = true
	opts.BusyAfter = time.Nanosecond
	opts.MinPending = 1

	s := newScheduler(64)
	s.enqueue(makeQueuedInput(1))
	a := &Agent{sched: s}
	if n := a.offloadPendingTasks(opts); n != 0 {
		t.Errorf("空闲时不应转投，实际 %d", n)
	}
}

// ★ 端到端：主 agent 忙时，积压任务应真的被搬到驻留子，且队列里留下说明。
// 这是本特性的核心行为 —— 只测"判定函数返回 0/非 0"不够，
// 必须证明任务**换了 agent 且原队列留下了可读的交代**。
func TestOffloadMovesTasksToResidentEndToEnd(t *testing.T) {
	root, main := newRootWithoutSchedulerLoop(t)
	defer main.Close()

	opts := DefaultOffloadOptions()
	opts.Enabled = true
	opts.BusyAfter = time.Nanosecond // 立即算"忙"
	opts.MinPending = 2
	opts.MaxResidents = 1

	// 伪造"正在跑一条长任务"：转投判定要求 running 非空。
	root.sched.enqueue(makeQueuedInput(1))
	root.sched.nextRef() // 把它变成 running

	// 再排 2 条积压
	root.sched.enqueue(makeQueuedInput(2))
	root.sched.enqueue(makeQueuedInput(3))

	moved := root.offloadPendingTasks(opts)
	if moved != 2 {
		t.Fatalf("应转投 2 条，实际 %d", moved)
	}

	// ① 确实拉起了一个驻留子，且标记为"为转投而建"
	list := root.Residents()
	if len(list) != 1 {
		t.Fatalf("应拉起 1 个驻留子，实际 %d", len(list))
	}
	resident := list[0]
	if !strings.HasPrefix(resident.ID, "offload-") {
		t.Errorf("驻留子应为转投专用命名，实际 %s", resident.ID)
	}
	// ② 它不配任何插件 inputch（用户要求），但持有全部输出通道（nil=全授权）
	if len(resident.InputChs) != 0 {
		t.Errorf("转投驻留子不应配 inputch，实际 %v", resident.InputChs)
	}
	if len(resident.AllowedOutputs) != 0 {
		t.Errorf("转投驻留子应持有全部输出通道（空=全授权），实际 %v", resident.AllowedOutputs)
	}
	t.Logf("驻留子 %s: inputch=%v outputs=%v", resident.ID, resident.InputChs, resident.AllowedOutputs)

	// ③ 原队列里留下说明（且说明是内核发的）
	if len(root.sched.queue) != 1 {
		t.Fatalf("原队列应只剩 1 条说明，实际 %d", len(root.sched.queue))
	}
	notice := root.sched.queue[0]
	if notice.Event == nil || notice.Event.Source != "kernel" {
		t.Fatalf("留下的应是内核说明，实际 %+v", notice.Event)
	}
	content, _ := notice.Event.Payload["content"].(string)
	for _, want := range []string{"2 条", resident.ID, "转投"} {
		if !strings.Contains(content, want) {
			t.Errorf("说明缺少 %q：%s", want, content)
		}
	}
	t.Logf("队列说明: %s", content)
}

// 达到上限后不得无界增殖：每个 tick 都拉一个新子会把机器拖垮。
func TestOffloadRespectsResidentCap(t *testing.T) {
	root, main := newRootWithoutSchedulerLoop(t)
	defer main.Close()

	opts := DefaultOffloadOptions()
	opts.Enabled = true
	opts.BusyAfter = time.Nanosecond
	opts.MinPending = 1
	opts.MaxResidents = 1

	root.sched.enqueue(makeQueuedInput(1))
	root.sched.nextRef()

	// 第一轮：拉起 1 个
	root.sched.enqueue(makeQueuedInput(2))
	if n := root.offloadPendingTasks(opts); n != 1 {
		t.Fatalf("第一轮应转 1 条，实际 %d", n)
	}
	// 第二轮：已达上限，但**会复用**刚建的那个子，所以仍能转投
	root.sched.enqueue(makeQueuedInput(3))
	if n := root.offloadPendingTasks(opts); n != 1 {
		t.Fatalf("第二轮应复用已有驻留子，实际转 %d", n)
	}
	if got := len(root.Residents()); got != 1 {
		t.Fatalf("★ 不得越过上限增殖：期望 1 个驻留子，实际 %d", got)
	}
}

// ★ 回归：检查必须发生在**独立 goroutine** 里。
//
// schedulerLoop 是同步执行任务的（executeNewTask 阻塞到任务结束），
// 所以"正忙"期间它根本不会回到循环顶部 —— 把检查放在那里的实现
// 永远不会触发（我第一版就是这么写的，测出来才发现）。
// 本测试钉死：offloadLoop 确实起了自己的 goroutine 并能被唤醒干活。
func TestOffloadLoopRunsWhileBusy(t *testing.T) {
	root, main := newRootWithoutSchedulerLoop(t)
	defer main.Close()

	root.offload = OffloadOptions{
		Enabled: true, BusyAfter: 10 * time.Millisecond,
		MinPending: 1, MaxResidents: 1,
	}
	// 伪造"正忙"：直接占住 running（不启动真实调度循环，避免它把任务跑掉）
	root.sched.enqueue(makeQueuedInput(1))
	root.sched.nextRef()
	root.sched.enqueue(makeQueuedInput(2))

	go root.offloadLoop() // 独立 goroutine，正是被测的点

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(root.Residents()) > 0 {
			return // 成功：忙时后台循环把积压转走了
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("offloadLoop 在忙时没有转投：检查没有跑在独立 goroutine 里？")
}

// ★★ 回归：积压可能**全在 io 输入 channel 里**，不在 sched.queue。
//
// 这是本特性最容易写错、而且我在线上真踩了的一步：schedulerLoop 是同步执行
// 任务的，所以「正忙」期间它根本回不到 pumpInbox —— 后到的输入全堆在
// io.inputCh（容量 256）里，sched.queue 恒为 0。
//
// 只数 s.queue 的实现在线上**永不触发**（实测：主 agent 跑着 6×45s 的任务、
// 我连发 4 条消息，队列始终显示 0、residents 始终 0）。
// 仓库里 armStop 早记过同一个坑（"只数 s.queue 会得到 0"），这里钉死不重犯。
func TestOffloadSeesInputsStuckInChannel(t *testing.T) {
	root, main := newRootWithoutSchedulerLoop(t)
	defer main.Close()

	opts := DefaultOffloadOptions()
	opts.Enabled = true
	opts.BusyAfter = time.Nanosecond
	opts.MinPending = 3
	opts.MaxResidents = 1

	// 伪造"正忙"
	root.sched.enqueue(makeQueuedInput(1))
	root.sched.nextRef()

	// 关键：把 3 条消息注入 **io 输入 channel**，不碰 sched.queue。
	// 这精确复现"调度器忙于执行任务、pumpInbox 没被调用"的现场状态。
	for i := 0; i < 3; i++ {
		root.io.InjectInputTo("webui", "webui", "text",
			map[string]interface{}{"content": "stuck"})
	}
	if len(root.sched.queue) != 0 {
		t.Fatalf("前置条件：此时 sched.queue 应为 0（输入还没被搬运），实际 %d", len(root.sched.queue))
	}
	if root.io.PendingInputs() != 3 {
		t.Fatalf("前置条件：输入应堆在 channel 里，实际 %d", root.io.PendingInputs())
	}

	// 转投必须能看到它们（先搬进队列再取）
	moved := root.offloadPendingTasks(opts)
	if moved != 3 {
		t.Fatalf("★ 堆在 channel 里的积压必须被看见并转投：期望 3，实际 %d", moved)
	}
	if len(root.Residents()) != 1 {
		t.Fatalf("应拉起 1 个驻留子，实际 %d", len(root.Residents()))
	}
}

// ★★ 回归：转投必须保留 ResponseCh，否则同步调用方永久挂起。
//
// 这是我在线上真踩的第二个 bug：第一版用 InjectInputTo 重建事件 ⇒ ResponseCh
// 被丢掉 ⇒ 日志显示子**正常处理完了**（各 ~3s），但 webui 的 HTTP 请求一直挂着
// 不返回，最终 504。仓库反复警告同一件事（Agent.Stop 的注释："带 ResponseCh 的
// 同步注入方（cli / clawhubadapter 均无超时）会永久挂起"）。
//
// 正确做法是走既有的跨 agent 投递原语 DeliverRouted：它推**原事件**。
//
// 断言方式是**最强的那个**：真的等同步回执回来。
// （不用读子的 InputChan 来断言：SpawnResident 会启动子自己的调度循环，
//
//	它会与测试抢同一个 channel —— 那样写出来的测试是 flaky 的，实测过一次挂死。）
func TestForwardKeepsResponseCh(t *testing.T) {
	root, main := newRootWithoutSchedulerLoop(t)
	defer main.Close()

	opts := DefaultOffloadOptions()
	opts.Enabled = true
	opts.BusyAfter = time.Nanosecond
	opts.MinPending = 1
	opts.MaxResidents = 1

	root.sched.enqueue(makeQueuedInput(1))
	root.sched.nextRef()

	// 一条**带同步回执通道**的输入（模拟 cli/webui 这类调用方）
	respCh := make(chan *agentIO.OutputEvent, 1)
	evt := &agentIO.InputEvent{
		RequestID: "sync-1", Source: "webui", Type: "text",
		OutputChannel: "webui",
		Payload:       map[string]interface{}{"content": "sync request"},
		ResponseCh:    respCh,
	}
	root.sched.enqueue(newInputTask(evt))

	if n := root.offloadPendingTasks(opts); n != 1 {
		t.Fatalf("应转投 1 条，实际 %d", n)
	}

	// 转投的是**同一个事件对象**（所以 payload 上的标注能在这里被看到），
	// 而不是重建的副本 —— 副本会丢掉 ResponseCh。
	if evt.Payload["offloaded_from"] != "parent" {
		t.Errorf("应标注转投来源，实际 %v", evt.Payload["offloaded_from"])
	}
	if evt.ResponseCh == nil {
		t.Fatal("★ 原事件的 ResponseCh 被清掉了")
	}

	// ★ 决定性断言：同步调用方真的收到回执。
	select {
	case out := <-respCh:
		if out == nil {
			t.Fatal("收到空回执")
		}
		if out.RequestID != "sync-1" {
			t.Errorf("回执应带原 RequestID，实际 %q", out.RequestID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("★ 同步调用方没收到回执：转投丢了 ResponseCh（线上表现为 HTTP 挂起 504）")
	}
}
