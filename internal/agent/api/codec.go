package api

// codec.go —— 编解码层的**统一出口**（无论 CGO 开关如何，调用方只认这里）。
//
// 分层：
//   codec_pure.go  —— 纯 Go 实现，永远参与编译（回退 + 黄金对照基准）
//   codec_cgo.go   —— CGO_ENABLED=1：真正调 C 库
//   codec_nocgo.go —— CGO_ENABLED=0：把 C 符号转发到纯 Go
//   codec.go       —— 本文件：对外的稳定 API，含兜底与日志
//
// 这样调用方（provider.go / core）不需要写任何 build tag 分支。

import (
	"log"
	"strings"
)

// ModelContextWindow 返回模型的最大上下文窗口（token 数）。
//
// 推断不出时（如 model="AUTO"）记一行日志并回退 defaultInferredContextWindow：
// 窗口被低估必须可见，部署方用 per-source
// core.llm.sources.<name>.context_window 显式声明真实值即可覆盖。
//
// 标称窗口 ≠ 有效窗口：接近满时注意力涣散，调用方应取 70-80% 为目标利用率。
func ModelContextWindow(model string) int {
	if w := modelContextWindowC(model); w != contextWindowUnknown {
		return w
	}
	log.Printf("[provider] 模型 %q 无法推断上下文窗口，回退 %d；"+
		"若真实窗口更大，请设置 core.llm.sources.<name>.context_window",
		strings.ToLower(model), defaultInferredContextWindow)
	return defaultInferredContextWindow
}

// EstimateTokens 粗略估算 token 数。
//
// 注意：这是**高频热路径**（上下文裁剪对每个事件都调）。走 C 的跨语言开销
// 对短文本未必划算 —— 是否该留在 C 侧由 codec_bench_test.go 的实测数据决定，
// 不要凭直觉断言（见 docs/zh/c-core/llm-orchestration-c.md §七 未决问题 3）。
func EstimateTokens(text string) int { return estimateTokensC(text) }

// TruncateByTokens 截断字符串至不超过 maxTokens 估计值。
func TruncateByTokens(s string, maxTokens int) string { return truncateByTokensC(s, maxTokens) }
