package core

import (
	"unicode/utf8"

	"gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
)

// TokenBudget 上下文 token 预算分配结果
type TokenBudget struct {
	MaxContext    int // 模型窗口上限
	TargetUsage   int // 目标使用量（受 maxTargetTokens 与 utilizationRate 共同约束）
	FixedTokens   int // 固定部分（system prompt base + tools + rules）
	MemoryTokens  int // memory context 可用预算
	ContextTokens int // 上下文事件可用预算
	Reserved      int // 预留（response 空间）
}

// EstimateTokens 粗略估算 token 数
// 中文 ~1.5 token/字，英文 ~0.3 token/字符
// 保守估计取 max(1, runeCount * 2)，对混合文本足够安全
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	runeCount := utf8.RuneCountInString(text)
	if runeCount == 0 {
		return 0
	}
	t := runeCount * 2
	if t < 1 {
		return 1
	}
	return t
}

// maxTargetTokens 是**有效工作区间**的上限（不是模型窗口）。
//
// 为什么窗口 1M 却不能按 800K 干活：标称窗口 ≠ 有效窗口。接近满窗口时注意力
// 明显涣散、成本与延迟也随 prompt 线性上升。该源（llmsproxy）实测 990,034 token
// 仍能返回，但 600K 才是它的最优工作区间 —— 超过这个量级，回答质量与延迟都不划算。
//
// 因此把「窗口上限」（判断请求会不会被上游拒）与「工作区间」（分配记忆/历史预算）
// 分开：前者由 provider.MaxContextTokens() 给，后者封顶在这里。
const maxTargetTokens = 600000

// ComputeTokenBudget 计算各部分的 token 预算
// utilizationRate 为目标窗口利用率（0.0-1.0），预留 1-utilizationRate 给 response
// 固定部分优先保障，剩余预算 1:2 分配给 memory context 和 context events
func ComputeTokenBudget(provider api.Provider, systemPromptBase string) TokenBudget {
	maxCtx := provider.MaxContextTokens()
	if maxCtx <= 0 {
		maxCtx = 32768
	}

	utilizationRate := 0.8
	targetUsage := int(float64(maxCtx) * utilizationRate)
	// 窗口很大（如 1M）时不要把 80% 当成工作面：按 maxTargetTokens 封顶。
	if targetUsage > maxTargetTokens {
		targetUsage = maxTargetTokens
	}
	reserved := maxCtx - targetUsage
	if reserved < 0 {
		reserved = 0
	}

	fixedTokens := EstimateTokens(systemPromptBase)

	available := targetUsage - fixedTokens
	if available < 0 {
		available = 0
	}

	// memory context 占 1/3，context events 占 2/3
	memTokens := available / 3
	ctxTokens := available - memTokens

	return TokenBudget{
		MaxContext:    maxCtx,
		TargetUsage:   targetUsage,
		FixedTokens:   fixedTokens,
		MemoryTokens:  memTokens,
		ContextTokens: ctxTokens,
		Reserved:      reserved,
	}
}

// TruncateByTokens 截断字符串至不超过 maxTokens 估计值
func TruncateByTokens(s string, maxTokens int) string {
	if maxTokens <= 0 || s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes)*2 <= maxTokens {
		return s
	}
	// 从开头保留 maxTokens/2 个字符（每个字符约 2 token）
	keep := maxTokens / 2
	if keep >= len(runes) {
		return s
	}
	return string(runes[:keep])
}
