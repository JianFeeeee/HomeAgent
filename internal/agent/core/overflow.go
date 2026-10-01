package core

import (
	"errors"
	"fmt"
	"log"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
)

// 上下文超页（context overflow）处理。
//
// 背景（2026-10-01 记忆召回跑分 v4 实测）：HA 的 prompt 是锯齿波，峰值达
// 窗口的 7 倍，每轮都在「远超窗口 → 紧急修剪」。根因之一是超页的三条触发
// 路径没有统一出口：
//
//	① 上游 context_full 错误 —— 完全没接，整轮 outcomeFailed（用户在报错里看到它）
//	② 本地积累超限       —— 只在轮首 checkContextFull，且根 agent no-op、一次性
//	③ 子 agent contextfull —— 已在用 L4 上报，但只推信号不携带动作
//
// 本文件把三条路径收敛到 L4 唯一入口（raiseKernelInterrupt），并把「裁剪」
// 做成同步动作 —— 详见 docs/zh/context-overflow-l4-design.md。

// overflowRatio 是触发超页的本地预判水位：积累上下文超过窗口的这个比例就裁剪。
//
// 为什么是 1.25 而不是 1.0：请求体本身被 buildMessages 按 targetUsage=0.8×窗口
// 裁过（见 buildMessages），所以「请求装不下」在本地永不会发生；能观测到的
// 超限信号是**积累上下文**（a.context 里全部事件）。1.25 留了一点余量，避免
// 在水位线附近反复触发。
const overflowRatio = 1.25

// overflowRecoverBudget 是「同一 TaskFrame 内超页恢复次数」的上限。
//
// 为什么需要上限：裁剪的目标是让积累量回到窗口以内，一次通常就够；但若
// topK 与窗口脱钩（max_context_size 是**条数**不是 token，见 pruneByQuery），
// 一次裁剪可能仍不达标。没有上限就会变成「裁剪 → 复原 → 又超页 → 再裁剪」的
// 循环，每次都付一轮任务调度开销。
//
// 超限时**不做静默重试**：保存现场并明确终止，把问题暴露给用户与状态面。
const overflowRecoverBudget = 2

// accumulatedTokens 估算**未裁剪**的积累上下文 token 数。
//
// ⚠️ 判据必须用积累量，不能用 f.Msgs：后者已被 buildMessages 按
// targetUsage = 0.8×窗口 裁过，结构上封顶 80%，对任何 >0.8 的阈值都**永远
// 不成立**（这个坑已由 resident.go:checkContextFull 的注释记录过一次，
// 这里抽成函数是为了让两处共用同一口径，避免再次漂移）。
func (a *Agent) accumulatedTokens() int {
	if a == nil || a.context == nil {
		return 0
	}
	acc := 0
	for _, e := range a.context.Recent(0) {
		acc += EstimateTokens(e.Input) + EstimateTokens(e.Response)
	}
	return acc
}

// contextWindowTokens 取本 agent 的窗口上限（token）。
func (a *Agent) contextWindowTokens() int {
	if a != nil && a.provider != nil {
		if max := a.provider.MaxContextTokens(); max > 0 {
			return max
		}
	}
	return defaultMaxContextTokens
}

// overflowRatioNow 返回「积累量 / 窗口」。窗口拿不到时返回 0（表示不判定）。
func (a *Agent) overflowRatioNow() float64 {
	max := a.contextWindowTokens()
	if max <= 0 {
		return 0
	}
	return float64(a.accumulatedTokens()) / float64(max)
}

// maybeHandleContextOverflow 是本地预判的入口：**每次发 LLM 请求之前**调用，
// 覆盖轮内 tool 回环（现状只在轮首检查，轮内撑爆时才发现，浪费一整轮工具调用）。
//
// 返回 true 表示已处理（本帧应当重跑），调用方需要重建 f.Msgs —— 因为裁剪
// 之后挂起帧里那份超限请求不能复用（见设计文档约束三）。
//
// 幂等：未超页时零开销（一次 token 估算 + 一次比较），可安全地在热路径调用。
func (a *Agent) maybeHandleContextOverflow(query string) (handled bool, pruned int) {
	if a == nil || a.context == nil || a.isLightKernel() {
		// 轻量内核（驻留子）不做按相关度裁剪，见 pruneByQuery 的同款理由。
		return false, 0
	}
	ratio := a.overflowRatioNow()
	if ratio < overflowRatio {
		return false, 0
	}
	return true, a.handleContextOverflow(query, ratio)
}

// handleContextOverflow 执行「裁剪 + L4 打断」。
//
// 关键取舍：裁剪在**这里同步做完**，L4 只负责打断与告知。理由：
//   - Prune 全路径无 IO、无 panic（打分→排序→重排→可选写 docStore），
//     把它放进 L4 任务里做只会多付一次完整任务调度开销；
//   - 这样「L4 里 Prune 出错 → 再触发 L4」在结构上就不可能发生。
//
// 出错处理（用户口径：保存退出，不重试）：裁剪 0 条说明「本来装得下」
// 却报超页 —— 这是判定逻辑的 bug，不是可恢复状态。此时保存现场并明确终止，
// 绝不再次触发中断。
func (a *Agent) handleContextOverflow(query string, ratio float64) int {
	before := a.context.Len()
	beforeTokens := a.accumulatedTokens()
	// pruned 是**写进 docStore 的条数**（Prune 内部计数），不是「少了几条」。
	// 判据必须看上下文是否真的变小了 —— 因为 docStore == nil 时（轻量内核、
	// 文档记忆未初始化）pruned 恒为 0，但裁剪其实照常发生了。用 pruned 判会
	// 把正常裁剪误判成 bug 而终止。
	pruned := a.pruneByQuery(query)
	after := a.context.Len()
	afterTokens := a.accumulatedTokens()

	if after >= before || afterTokens >= beforeTokens {
		// 超页了却裁不动 ⇒ 判定逻辑有问题（或 topK ≥ 实际条数）。
		// 按用户口径：保存现场 + 明确终止，**不重试不触发中断**。
		// 为什么不再试一次：L4 遇 L4 不能抢占（canPreempt 用严格大于），
		// 第二个 L4 只会排队等，永远等不到能执行的时机。
		log.Printf("[agent] context overflow aborted: ratio=%.2f no reduction "+
			"(%d→%d events, %d→%d tokens) — 判定逻辑可能有 bug，保存现场并终止本轮，不重试",
			ratio, before, after, beforeTokens, afterTokens)
		a.markOverflowAborted(ratio, before)
		return 0
	}
	pruned = before - after
	log.Printf("[agent] context overflow: ratio=%.2f pruned=%d (%d→%d events), raising L4",
		ratio, pruned, before, after)
	a.raiseContextOverflow(pruned)
	return pruned
}

// markOverflowAborted 记录一次「超页但裁不出东西」的终止，供状态面观测。
func (a *Agent) markOverflowAborted(ratio float64, events int) {
	a.overflowStat.Lock()
	a.overflowStat.Aborted++
	a.overflowStat.LastRatio = ratio
	a.overflowStat.LastEvents = events
	a.overflowStat.Unlock()
}

// raiseContextOverflow 通过 **L4 唯一入口**上报上下文超页。
//
// 与 panic / selfip 并列，是 L4 的第三个来源。
//
// 消息内容不是通知而是**必要的认知输入**：L4 任务按调度器设计拿不到被打断
// 者的上下文（suspend 的 D1=B 注释明确「不把被打断任务的任何内容交给它」），
// 所以「已裁剪了什么、查不到不等于不存在」必须显式写进去。
//
// 这条提示直接对应 v4 实测的一类失败：HA 对查不到的事实给出
// 「库里根本没有 X，任何数字都是编的」——它不知道有内容被移走，
// 于是把「没检索到」推断成「不存在」。
func (a *Agent) raiseContextOverflow(pruned int) {
	a.overflowStat.Lock()
	a.overflowStat.Triggered++
	a.overflowStat.LastPruned = pruned
	a.overflowStat.Unlock()

	a.raiseKernelInterrupt("kernel/overflow", "kernel", overflowNotice(pruned))
}

// overflowNotice 构造超页中断消息。
//
// 抽成纯函数是为了可测：这条消息的措辞不是装饰，它是 agent 唯一的认知输入
//（L4 任务按调度器设计拿不到被打断者的上下文）。「查不到不等于不存在」这句
// 直接对应 v4 实测的一类真实失败 —— HA 对检索不到的事实回答
// 「库里根本没有 X，任何数字都是编的」，因为它不知道有内容被移走了。
func overflowNotice(pruned int) string {
	return fmt.Sprintf("[内核] 上下文超页：已裁剪 %d 条低相关事件到文档记忆。"+
		"被裁内容仍可检索，但需显式查询；查不到不等于不存在。", pruned)
}

// handleUpstreamContextFull 处理上游返回的 ErrContextFull：与本地预判走**同一条**
// 路径（裁剪 → L4），避免两条路径行为不一致。
//
// 必须在 provider fallback **之前**调用：fallback 会换 provider 重发同一个
// 超限请求，换谁都一样超。
//
// 返回 true 表示已裁剪，调用方应重试本轮；false 表示裁不出东西，应终止。
func (a *Agent) handleUpstreamContextFull(llmErr error) bool {
	var pe *agentAPI.ProviderError
	if !errors.As(llmErr, &pe) || pe.Kind != agentAPI.ErrContextFull {
		return false
	}
	pruned := a.handleContextOverflow("", 0)
	a.overflowStat.Lock()
	a.overflowStat.Upstream++
	a.overflowStat.Unlock()
	return pruned > 0
}

// overflowStat 字段定义在 Agent 上（见 agent.go），此处只做访问辅助。