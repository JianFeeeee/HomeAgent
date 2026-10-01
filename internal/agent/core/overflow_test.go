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

	handled, pruned := a.maybeHandleContextOverflow(nil, "退款导出")
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

	handled, pruned := a.maybeHandleContextOverflow(nil, "查询")
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
	handled, pruned := a.maybeHandleContextOverflow(nil, "查询")
	if handled || pruned != 0 {
		t.Fatalf("轻量内核不应按相关度裁剪：handled=%v pruned=%d", handled, pruned)
	}
}

// ── T2：裁剪后积累量确实下降 ──

func TestOverflow_T2_裁剪后累积下降(t *testing.T) {
	a := newOverflowAgent(1000, 2, 20, 400)
	before := a.accumulatedTokens()

	a.maybeHandleContextOverflow(nil, "退款导出")
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

	a.maybeHandleContextOverflow(nil, "查询")

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
	if handled, _ := a.maybeHandleContextOverflow(nil, "q"); handled {
		t.Fatal("nil agent 不应处理")
	}
	empty := &Agent{}
	if handled, _ := empty.maybeHandleContextOverflow(nil, "q"); handled {
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
// ── T5（变异项）：帧内恢复预算耗尽 ⇒ 终止而非死循环 ──
//
// 为什么要单独测：这个防护一度是**死代码**（TaskFrame.OverflowRecover 声明了
// 但从没被读取）。没接线的防护等于没有防护 —— 变异测试正是抓这种。
func TestOverflow_T5_恢复预算耗尽则终止(t *testing.T) {
	a := newOverflowAgent(1000, 2, 20, 400)
	f := &TaskFrame{}

	// 第一次超页：正常裁剪
	handled, pruned := a.maybeHandleContextOverflow(f, "退款导出")
	if !handled || pruned <= 0 {
		t.Fatalf("首次应正常裁剪：handled=%v pruned=%d", handled, pruned)
	}
	if f.OverflowRecover != 1 {
		t.Fatalf("首次应记一次恢复，��际 %d", f.OverflowRecover)
	}

	// 把 topK 放大到裁不动（模拟「topK 与窗口脱钩」这个真实根因）
	a.maxContextSize = 99999

	// 预算内的第二次：仍会尝试
	handled, _ = a.maybeHandleContextOverflow(f, "退款导出")
	if !handled {
		t.Fatal("预算内仍应处理")
	}
	if f.OverflowRecover != 2 {
		t.Fatalf("预算内应累计到 2，实际 %d", f.OverflowRecover)
	}

	// 第三次：超预算 ⇒ 终止，且写明原因
	handled, _ = a.maybeHandleContextOverflow(f, "退款导出")
	if !handled {
		t.Fatal("超预算也应算「已处理」（终止），以免调用方再重跑")
	}
	if f.Terminal != terminalError {
		t.Fatalf("超预算应置终止态，实际 terminal=%v", f.Terminal)
	}
	if f.Err == nil || !strings.Contains(f.Err.Error(), "max_context_size") {
		t.Fatalf("终止原因应指向根因（max_context_size 与窗口不匹配）：%v", f.Err)
	}
}

// 变异自证：把预算调到极大 ⇒ 上面那条必须变红（证明它真的在数）。
func TestOverflow_T5_变异_预算极大则不终止(t *testing.T) {
	backup := overflowRecoverBudget
	overflowRecoverBudget = 1 << 30
	defer func() { overflowRecoverBudget = backup }()

	a := newOverflowAgent(1000, 2, 20, 400)
	a.maxContextSize = 99999 // 裁不动
	f := &TaskFrame{}
	for i := 0; i < 5; i++ {
		a.maybeHandleContextOverflow(f, "退款导出")
	}
	if f.Terminal == terminalError {
		t.Fatalf("预算=2^30 时不应终止 ⇒ 预算判定可能没生效")
	}
}

// ── T9：topK 随窗口换算（不再与窗口脱钩）──
//
// 这是 v4 跑分归因的根因二：max_context_size 是条数、与窗口无关，
// 所以「只调 context_window 到 1M 就更好」不成立。
func TestOverflow_T9_topK随窗口变化(t *testing.T) {
	// 同样 20 条事件、同样的 max_context_size，只改窗口 ⇒ topK 必须跟着变
	build := func(window int) *Agent {
		return newOverflowAgent(window, 30, 20, 400) // maxContextSize=30（默认）
	}

	small := build(10_000)      // 小窗口
	large := build(1_000_000)   // 大窗口（用户关心的 1M 场景）

	topKSmall := small.contextTopK()
	topKLarge := large.contextTopK()

	if topKLarge <= topKSmall {
		t.Fatalf("窗口变大后 topK 应变大：%d → %d（说明仍与窗口脱钩）", topKSmall, topKLarge)
	}
	// 小窗口下预算只够留很少事件；大窗口下应更宽松
	if topKLarge < 10 {
		t.Fatalf("1M 窗口下 topK=%d 过小，说明预算反推失效", topKLarge)
	}
}

// max_context_size 仍是硬上限（改了配置必须生效，否则运维会困惑）。
func TestOverflow_T9_运维配置仍是上限(t *testing.T) {
	a := newOverflowAgent(1_000_000, 5, 40, 200) // 窗口很大但硬上限 5
	if got := a.contextTopK(); got > 4 {
		t.Fatalf("topK=%d 超过 max_context_size-1=4，运维配置失效", got)
	}
}

// 无积累样本时不得除零/返回 0。
func TestOverflow_T9_空上下文topK至少为1(t *testing.T) {
	a := newOverflowAgent(50_000, 30, 0, 200)
	if got := a.contextTopK(); got < 1 {
		t.Fatalf("空上下文时 topK=%d，至少应为 1", got)
	}
}

// 变异自证：把 contextTopK 退回「固定用 maxContextSize」，
// 上面那条「窗口变大 topK 变大」必须变红（证明该测试真能抓到脱钩）。
func TestOverflow_T9_变异_固定topK则脱钩(t *testing.T) {
	build := func(window int) *Agent { return newOverflowAgent(window, 30, 20, 400) }

	// 旧行为：topK 只看 max_context_size，与窗口无关 ⇒ 两个窗口得到同一个值。
	oldTopK := func(a *Agent) int { return a.maxContextSize - 1 }
	if oldTopK(build(10_000)) != oldTopK(build(1_000_000)) {
		t.Fatalf("前置不成立：旧行为本应与窗口无关")
	}

	// 新实现必须与旧行为不同，否则「T9_topK随窗口变化」那条主测试是假绿。
	newSmall := build(10_000).contextTopK()
	newLarge := build(1_000_000).contextTopK()
	if newSmall == oldTopK(build(10_000)) && newLarge == oldTopK(build(1_000_000)) {
		t.Fatalf("新实现与旧实现不可区分 ⇒ topK 实际仍与窗口脱钩，主测试是假绿")
	}
	if newLarge <= newSmall {
		t.Fatalf("新实现下窗口变大 topK 未变大：%d → %d", newSmall, newLarge)
	}
}

// ── provider 未就绪时不得崩 ──
//
// 回归：contextTopK → computeTokenBudget → ComputeTokenBudgetTuned 里曾直接调
// provider.MaxContextTokens()，provider 为 nil 时 SIGSEGV（实测由
// TestMemoryPass_PruneAndRecallTogether 撞出来）。它同样会发生在
// 「配置早于 provider 就绪」的启动序列上，不只是测试场景。
func TestOverflow_零值Agent不崩(t *testing.T) {
	// 完全零值 Agent（非 nil）：无 provider、无 context、无配置
	empty := &Agent{}
	if got := empty.contextTopK(); got < 1 {
		t.Fatalf("零值 Agent 的 topK=%d，至少应为 1", got)
	}
	// 硬上限为 0 时也不能崩
	empty.maxContextSize = 0
	if got := empty.contextTopK(); got < 1 {
		t.Fatalf("maxContextSize=0 时 topK=%d，至少应为 1", got)
	}
}

// ComputeTokenBudgetTuned 直接吃 nil provider（不经 Agent）也要安全。
func TestComputeTokenBudgetTuned_nilProvider(t *testing.T) {
	b := ComputeTokenBudgetTuned(nil, "", ContextTuning{})
	if b.MaxContext <= 0 {
		t.Fatalf("nil provider 时应回退到默认窗口，实际 %d", b.MaxContext)
	}
	if b.ContextTokens < 0 {
		t.Fatalf("ContextTokens 不应为负：%d", b.ContextTokens)
	}
}

// ── topK 下限：不能被算成 1（会让裁剪完全无效）──
//
// 实测缺陷：v4 的工具大回执型事件平均 48000 token，而 50k 窗口的
// ContextTokens 预算只有 40000 ⇒ 预算反推的 topK = 0 ⇒ 钳到 1。
// 而 Prune 里 keepCount = topK - protected = 1 - 10 < 0 ⇒ keep 为空
// ⇒ **全部事件被归档**：裁完一轮上下文照样超页，超页处理空转。
//
// 正确下限是 protectedCount+1：宁可暂时超页，等预算或事件尺寸回到正常区间。
func TestOverflow_大事件时topK不塌到1(t *testing.T) {
	// 窗口 50000 ⇒ ContextTokens 预算约 40000
	a := newOverflowAgent(50_000, 30, 30, 0)
	// 造「单条就超预算」的事件：Response 48000 token
	for i := 0; i < 30; i++ {
		a.context.Append(ContextEvent{
			Input:    strings.Repeat("A", 160_000), // 约 40000 token
			Response: strings.Repeat("B", 192_000), // 约 48000 token
		})
	}

	topK := a.contextTopK()
	protected := a.protectedContextCount()
	if topK <= protected {
		t.Fatalf("topK=%d <= protectedCount=%d ⇒ 裁剪会把全部事件归档（keep 为空），"+
			"超页处理空转", topK, protected)
	}
	if topK < protected+1 {
		t.Fatalf("topK=%d 必须至少是 protectedCount+1=%d", topK, protected+1)
	}
}

// 真实形状验证：topK 下限生效时，Prune 确实还能裁掉东西。
func TestOverflow_大事件时裁剪仍有效(t *testing.T) {
	a := newOverflowAgent(50_000, 30, 30, 0)
	for i := 0; i < 30; i++ {
		a.context.Append(ContextEvent{
			Input:    strings.Repeat("A", 8_000),
			Response: strings.Repeat("B", 40_000),
		})
	}
	before := a.context.Len()
	pruned := a.handleContextOverflow("退款导出", 10.0)
	after := a.context.Len()
	if pruned <= 0 && after >= before {
		t.Fatalf("裁剪无效：%d → %d 条，pruned=%d", before, after, pruned)
	}
	// 裁剪后仍应留下 protected 条以上，不能清空
	if after < a.protectedContextCount() {
		t.Fatalf("裁剪后只剩 %d 条，少于 protectedCount=%d（记忆被清空）",
			after, a.protectedContextCount())
	}
}
