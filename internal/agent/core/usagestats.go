package core

import (
	"sync"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
)

// UsageTotals 是**跨调用累计**的用量账目。
//
// ── 为什么需要它 ──
//
// 单次调用的用量一直在（`StageCtx.TokenUsage`、LLM chain 事件），但没有任何地方
// 把多次调用加起来。于是「这一轮/这个会话花了多少、缓存省了多少」根本答不出来 ——
// 只能一条条翻日志自己加。这正是「无法统计缓存命中」的另一半：
// 数据流出来了，却没有落点。
//
// ── 关于命中率的口径（最容易被算错的地方）──
//
// 命中率的分母**必须只算「上游报了缓存」的调用**（`CacheReportedCalls`），
// 而不是全部调用。理由：没报缓存的提供商/版本不是「命中 0」而是「不知道」，
// 把它们算进分母会把命中率稀释成一个无意义的低值，
// 让人去优化一个本来就没开的功能。
//
// 同理 `CacheHitRate` 在没有任何调用报过缓存时返回 ok=false，
// 调用方应显示「—」而不是 0%。
type UsageTotals struct {
	// 累计 token 数。
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Total      int64 `json:"total"`
	// CacheRead 是累计命中缓存的输入 token（省下来的那部分计算）。
	CacheRead int64 `json:"cache_read"`
	// CacheMiss 是上游明确报告的未命中输入 token。
	CacheMiss int64 `json:"cache_miss"`
	// Reasoning 是累计的思考 token（计费输出里属于思考的部分）。
	Reasoning int64 `json:"reasoning"`

	// Calls 是累计的 LLM 调用次数（含用量为 0 的调用）。
	Calls int64 `json:"calls"`
	// CacheReportedCalls 是其中**上游报告了缓存字段**的调用数。
	// 命中率的分母。
	CacheReportedCalls int64 `json:"cache_reported_calls"`
}

// add 并入一次调用的用量。
func (t *UsageTotals) add(u agentAPI.TokenUsage) {
	t.Calls++
	t.Prompt += int64(u.Prompt)
	t.Completion += int64(u.Completion)
	t.Total += int64(u.Total)
	t.CacheRead += int64(u.CacheRead)
	t.CacheMiss += int64(u.CacheMiss)
	t.Reasoning += int64(u.ReasoningTokens)
	if u.CacheReported {
		t.CacheReportedCalls++
	}
	// 上游没给 total 时按分量补，避免累计出现「total 小于 prompt+completion」。
	if u.Total == 0 && (u.Prompt > 0 || u.Completion > 0) {
		t.Total += int64(u.Prompt + u.Completion)
	}
}

// CacheHitRate 返回缓存命中率与「是否可计算」。
//
// 分母是 `CacheRead + CacheMiss`（上游报告过缓存的那些调用里的输入侧），
// 而不是 `Prompt`：后者在未报告缓存的调用里也计入，会把命中率压低。
//
// 两者都为 0 时 ok=false —— 表示「没有任何调用报过缓存」，
// 而不是「命中率 0」。调用方必须区分，否则会把无数据画成 0%。
func (t UsageTotals) CacheHitRate() (float64, bool) {
	denom := t.CacheRead + t.CacheMiss
	if t.CacheReportedCalls == 0 || denom == 0 {
		return 0, false
	}
	return float64(t.CacheRead) / float64(denom), true
}

// usageLedger 是 UsageTotals 的并发安全容器。
//
// 为何要锁：LLM 调用可能来自多个 goroutine ——
// · 主 agent 与驻留子 agent 各自的任务循环；
// · 同一轮里并行的工具执行虽不直接记账，但工具内部可能再发起 LLM 调用
//
//	（子调用/摘要），与主循环并发。
//
// 计数丢失是**静默**的（少算一点没人看得出来），所以宁可用锁。
type usageLedger struct {
	mu     sync.Mutex
	totals UsageTotals
}

// record 并入一次调用。
func (l *usageLedger) record(u agentAPI.TokenUsage) {
	l.mu.Lock()
	l.totals.add(u)
	l.mu.Unlock()
}

// snapshot 取当前累计值的副本。
func (l *usageLedger) snapshot() UsageTotals {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.totals
}

// reset 清零（供测试与「重置统计」入口使用）。
func (l *usageLedger) reset() {
	l.mu.Lock()
	l.totals = UsageTotals{}
	l.mu.Unlock()
}
