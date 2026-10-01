package core

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// ── 测试替身 ──

// fakeOverflowProvider 只提供 MaxContextTokens（超页判据需要它），
// 其余 Provider 方法不参与本组测试。
type fakeOverflowProvider struct {
	agentAPI.Provider
	window int
}

func (p *fakeOverflowProvider) MaxContextTokens() int { return p.window }

// newOverflowAgent 造一个够用���超页测试 Agent。
func newOverflowAgent(window, maxContextSize int, events int, chunkChars int) *Agent {
	a := &Agent{
		io:             agentIO.NewIOManager(),
		provider:       &fakeOverflowProvider{window: window},
		maxContextSize: maxContextSize,
		context:        NewRelevanceContext("", nil),
	}
	// 固定向量，避免依赖稠密模型；也不进 docStore（nil ⇒ Prune 只重排不写盘）。
	for i := 0; i < events; i++ {
		a.context.Append(ContextEvent{
			Input:    fmt.Sprintf("第%d批输入", i) + strings.Repeat("x", chunkChars),
			Response: fmt.Sprintf("第%d批回执", i) + strings.Repeat("y", chunkChars),
		})
	}
	return a
}

// ── T1：积累量超限才触发裁剪 ──

func TestOverflow_T1_积累超125触发(t *testing.T) {
	// 窗口 1000 token；maxContextSize=2（条数），制造远超容量的积累。
	a := newOverflowAgent(1000, 2, 20, 400)
	before := a.context.Len()

	ratio := a.overflowRatioNow()
	if ratio < overflowRatio {
		t.Fatalf("构造后应已超页，ratio=%.2f 阈值=%.2f", ratio, overflowRatio)
	}

	handled, pruned := a.maybeHandleContextOverflow("退款导出")
	if !handled {
		t.Fatal("超限时 maybeHandleContextOverflow 应返回 handled=true")
	}
	if pruned <= 0 {
		t.Fatal("应裁剪出至少一条")
	}
	if after := a.context.Len(); after >= before {
		t.Fatalf("裁剪后条数未减少：%d → %d", before, after)
	}
}

// 未超页时必须零动作（这是热路径，每轮都调）。
func TestOverflow_T1_未超页不动手(t *testing.T) {
	// 窗口 10_000_000 token：任何合理积累都远低于 1.25×
	a := newOverflowAgent(10_000_000, 2, 5, 100)
	before := a.context.Len()

	handled, pruned := a.maybeHandleContextOverflow("查询")
	if handled || pruned != 0 {
		t.Fatalf("未超页不应处理：handled=%v pruned=%d", handled, pruned)
	}
	if after := a.context.Len(); after != before {
		t.Fatalf("未超页时上下文不应变化：%d → %d", before, after)
	}
}

// 轻量内核（驻留子）走的是另一套策略，不做按相关度裁剪。
func TestOverflow_T1_轻量内核跳过(t *testing.T) {
	a := newOverflowAgent(1000, 2, 20, 400)
	a.parentID = "parent-1" // 触发 isLightKernel
	handled, pruned := a.maybeHandleContextOverflow("查询")
	if handled || pruned != 0 {
		t.Fatalf("轻量内核不应按相关度裁剪：handled=%v pruned=%d", handled, pruned)
	}
}

// ── T2：裁剪后积累量确实下降 ──

func TestOverflow_T2_裁剪后累积下降(t *testing.T) {
	a := newOverflowAgent(1000, 2, 20, 400)
	before := a.accumulatedTokens()

	a.maybeHandleContextOverflow("退款导出")
	after := a.accumulatedTokens()

	if after >= before {
		t.Fatalf("裁剪后积累 token 未下降：%d → %d", before, after)
	}
}

// ── T4（变异项）：裁不出东西 ⇒ 终止且不触发 L4 ──
//
// 构造「装得下却超页」：maxContextSize 给得足够大，Prune 会返回 0。
// 期望：记账 Aborted、不调 raiseKernelInterrupt（用 stat.Triggered 验证）。
func TestOverflow_T4_裁不动则终止不中断(t *testing.T) {
	// 窗口极小 ⇒ ratio 超限；但 maxContextSize=9999 ⇒ len(events) <= topK
	// ⇒ Prune 直接返回 0（context.go:349），上下文一点没变 ⇒ 必须终止
	a := newOverflowAgent(1000, 9999, 5, 400)
	if a.overflowRatioNow() < overflowRatio {
		t.Fatal("前置条件不成立：构造的会话应超页")
	}

	a.maybeHandleContextOverflow("查询")

	a.overflowStat.Lock()
	aborted, triggered := a.overflowStat.Aborted, a.overflowStat.Triggered
	a.overflowStat.Unlock()

	if aborted != 1 {
		t.Fatalf("应记一次 Aborted，实际 %d", aborted)
	}
	if triggered != 0 {
		t.Fatalf("裁不出东西时**不应**触发 L4，实际 Triggered=%d", triggered)
	}
}

// ── T6：中断消息必须带「查不到不等于不存在」──

func TestOverflow_T6_中断消息含认知提示(t *testing.T) {
	got := overflowNotice(17)
	if got == "" {
		t.Fatal("超页通知不应为空")
	}
	if !strings.Contains(got, "查不到不等于不存在") {
		t.Fatalf("中断消息缺少认知提示，agent 会把「没检索到」误判成「不存在」：%q", got)
	}
	if !strings.Contains(got, "文档记忆") {
		t.Fatalf("中断消息应说明被裁内容去了哪里：%q", got)
	}
	// 来源标识在 raiseKernelInterrupt 的参数里（"kernel/overflow"），
	// 不在消息正文 —— 那部分留给可读性。这里只保证提示词自洽。
	if strings.Contains(got, "查不到等于存在") {
		t.Fatalf("提示词语义写反了：%q", got)
	}
}

// ── 上游 ErrContextFull 走同一条裁剪路径 ──

func TestOverflow_上游ErrContextFull走同路径(t *testing.T) {
	a := newOverflowAgent(1000, 2, 20, 400)

	// 累积不足以下次超页也没关系：上游已经明说装不下了
	if !a.handleUpstreamContextFull(&agentAPI.ProviderError{
		StatusCode: 400,
		Kind:       agentAPI.ErrContextFull,
		Message:    "context_length_exceeded",
	}) {
		t.Fatal("上游 ErrContextFull 应被接住并返回 true（可恢复）")
	}
	a.overflowStat.Lock()
	upstream, triggered := a.overflowStat.Upstream, a.overflowStat.Triggered
	a.overflowStat.Unlock()
	if upstream != 1 {
		t.Fatalf("应记一次上游超页，实际 %d", upstream)
	}
	if triggered != 1 {
		t.Fatalf("上游超页也应经 L4 上报一次，实际 %d", triggered)
	}
}

// 非上下文类错误不得被误接。
func TestOverflow_上游其他错误不误接(t *testing.T) {
	a := newOverflowAgent(1000, 2, 20, 400)
	for _, kind := range []agentAPI.ErrKind{agentAPI.ErrCredential, agentAPI.ErrTransient, agentAPI.ErrUnknown} {
		if a.handleUpstreamContextFull(&agentAPI.ProviderError{
			StatusCode: 500, Kind: kind, Message: "boom",
		}) {
			t.Fatalf("kind=%v 不应被当作上下文超限接住", kind)
		}
	}
}

// nil context / nil provider 不得 panic。
func TestOverflow_零值不panic(t *testing.T) {
	var a *Agent
	if handled, _ := a.maybeHandleContextOverflow("q"); handled {
		t.Fatal("nil agent 不应处理")
	}
	empty := &Agent{}
	if handled, _ := empty.maybeHandleContextOverflow("q"); handled {
		t.Fatal("零值 agent 不应处理")
	}
	_ = empty.overflowRatioNow()
	_ = empty.accumulatedTokens()
}

// ── T8（回归）：并发调用安全 ──
// 超页判据在每轮 LLM 请求前调用，是热路径；AccumulatedTokens 会遍历事件，
// 必须与 Append 并发安全，否则数据竞争。
func TestOverflow_并发安全(t *testing.T) {
	a := newOverflowAgent(1000, 2, 50, 200)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				a.context.Append(ContextEvent{
					Input:    fmt.Sprintf("并发%d-%d", i, j),
					Response: strings.Repeat("z", 300),
				})
				_ = a.overflowRatioNow()
			}
		}(i)
	}
	wg.Wait()
}