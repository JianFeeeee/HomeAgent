package core

// 「停止」语义的验收测试（用户明确的设计）：
//
//	停止 = ①立即结束当前 LLM 推理（不重试、不恢复）；②对停止那一刻**已排队**的
//	x 条消息，后续依次在 pre-action 阶段短路，而不是把它们当新输入再跑一遍。
//
// 为什么单独一组用例：此前的实现把「停止」当成一条普通中断——空内容还会被
// interceptLoop 直接丢掉。实测症状是接口回 200、内核零日志、生成继续跑到自然结束。
// 这组用例把三条不变量钉住：停止必须终结当前任务、必须按快照配额短路排队消息、
// 停止之后新到的输入不受影响。

import (
	"context"
	"sync"
	"testing"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// stopProvider 是真会走流式路径的 provider：首个 ChatStream 阻塞到 ctx 取消，
// 之后（若真的重跑了）计数会在 callCount 上暴露出来。
//
// 与 preemptProvider 的区别：后者 ChatStream 恒回错误，会让
// chatStreamWithFallback 走非流式回退——那条路径恰好是“停止后重跑”的现场，
// 但也让测试看不到流式取消本身的行为。两个 double 各测一段。
type stopProvider struct {
	mu        sync.Mutex
	calls     int
	streams   int
	entered   chan struct{}
	enteredOn sync.Once
}

func newStopProvider() *stopProvider {
	return &stopProvider{entered: make(chan struct{})}
}

func (p *stopProvider) Name() string          { return "stop" }
func (p *stopProvider) MaxContextTokens() int { return 8192 }

func (p *stopProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &agentAPI.CompletionResponse{Content: "非流式回退内容"}, nil
}

func (p *stopProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	p.mu.Lock()
	p.calls++
	p.streams++
	p.mu.Unlock()
	p.enteredOn.Do(func() { close(p.entered) })
	ch := make(chan agentAPI.StreamChunk)
	go func() {
		defer close(ch)
		<-ctx.Done()
	}()
	return ch, nil
}

func (p *stopProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// 停止指令必须终结当前任务，而不是取消后重跑本步。
func TestStop_TerminatesCurrentTaskWithoutRetry(t *testing.T) {
	sp := newStopProvider()
	a := newPreemptAgent(t, sp)

	evt, _ := textEvent("webui", "写一篇很长的文章")
	task := newInputTask(evt)
	if !a.sched.enqueue(task) {
		t.Fatal("入队失败")
	}
	nt, _, _ := a.sched.nextRef()

	done := make(chan struct{})
	var frame *TaskFrame
	go func() {
		frame, _ = a.runInputTask(nt.Event)
		close(done)
	}()

	select {
	case <-sp.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider 未被调用")
	}

	// 与停止按钮完全一致的调用形状：空消息 + stop 标记。
	stopEvt := &agentIO.InputEvent{
		Source:        "webui",
		Type:          "text",
		OutputChannel: "webui",
		Payload: map[string]interface{}{
			"content": "", "stop": true, "interrupt": true, "priority": "L4",
		},
	}
	a.io.InjectInterrupt(stopEvt.Source, stopEvt.OutputChannel, stopEvt.Payload)
	// interceptLoop 才是 armStop 的调用点；这里模拟它已消费该指令（只 arm，
	// 不 takeStop——takeStop 必须由 stepLLM 去消费，那正是被测行为）。
	a.sched.armStop()
	a.cancelCurrentLLM()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("停止后任务未结束（仍在重跑或挂起）")
	}

	if frame == nil {
		t.Fatal("未拿到帧")
	}
	// 不该出现「取消 → 重跑 → 再问一次模型」：provider 只该被调用一次。
	// 这正是停止按钮失效的核心——实测旧行为：取消后还会（经非流式回退或
	// stepLLM 重跑）再发一次完整请求。
	if n := sp.callCount(); n != 1 {
		t.Fatalf("停止后 LLM 调用次数=%d，期望 1（不得重跑/回退）", n)
	}
	if frame.Response == "" {
		t.Fatal("停止应给用户一条收尾说明")
	}
}

// 停止时的排队深度是快照：恰好短路 x 条，之后新到的输入不受影响。
func TestStop_CancelBudgetIsSnapshot(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	// 停止那一刻队列里有 2 条。
	a.sched.enqueue(newInputTask(mustTextEvent("qq", "一")))
	a.sched.enqueue(newInputTask(mustTextEvent("qq", "二")))
	n := a.sched.armStop()
	if n != 2 {
		t.Fatalf("停止时排队深度=%d，期望 2", n)
	}

	// 停止后才到的新输入：不受配额影响。
	a.sched.enqueue(newInputTask(mustTextEvent("qq", "三")))

	canceled := 0
	kept := []string{}
	for _, name := range []string{"一", "二", "三"} {
		if a.sched.consumeCancel() {
			canceled++
			continue
		}
		kept = append(kept, name)
	}
	if canceled != 2 {
		t.Fatalf("被短路条数=%d，期望 2（快照配额）", canceled)
	}
	if len(kept) != 1 || kept[0] != "三" {
		t.Fatalf("停止后新到的输入应正常执行，实际 kept=%v", kept)
	}

	// 配额用尽后不再短路。
	if a.sched.consumeCancel() {
		t.Fatal("配额应已耗尽")
	}
}

// 多次按停止取较大值，而不是累加（两个客户端同时按下不该翻倍）。
func TestStop_ArmTwiceTakesMaxNotSum(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.sched.enqueue(newInputTask(mustTextEvent("qq", "一")))
	a.sched.enqueue(newInputTask(mustTextEvent("qq", "二")))
	if n := a.sched.armStop(); n != 2 {
		t.Fatalf("首次 armStop=%d，期望 2", n)
	}
	if n := a.sched.armStop(); n != 2 {
		t.Fatalf("重复 armStop=%d，期望仍为 2（取 max 不累加）", n)
	}
	if !a.sched.consumeCancel() || !a.sched.consumeCancel() {
		t.Fatal("两次配额都应可消费")
	}
	if a.sched.consumeCancel() {
		t.Fatal("第三次不应再有配额")
	}
}

func mustTextEvent(source, text string) *agentIO.InputEvent {
	evt, _ := textEvent(source, text)
	return evt
}
