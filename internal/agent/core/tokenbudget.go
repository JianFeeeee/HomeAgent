package core

import (
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

// EstimateTokens / TruncateByTokens 转发到编解码层统一出口。
//
// 本包内调用点很多（context.go / process.go / resident.go / tooldefs.go…），
// 而实现只有一份（api 包，C 化后可选走 C）。保留这两个同名转发，
// 是为了不把调用点全部改写成 api.EstimateTokens —— 那场改动对行为零收益，
// 却把「本包依赖 api」这件事铺得到处都是。

// EstimateTokens 粗略估算 token 数（转发到 api.EstimateTokens）。
func EstimateTokens(text string) int { return api.EstimateTokens(text) }

// TruncateByTokens 截断字符串至不超过 maxTokens 估计值（转发到 api.TruncateByTokens）。
func TruncateByTokens(s string, maxTokens int) string { return api.TruncateByTokens(s, maxTokens) }

// ComputeTokenBudget 计算各部分的 token 预算。
//
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

// TruncateByTokens 已移至 api 包（编解码层统一出口，见 codec.go）。
// 上方已有同名转发，此处不再重复定义。
