package api

// codec_pure.go —— 编解码层的**纯 Go 实现**，永远参与编译。
//
// 它有两个身份：
//   1. CGO_ENABLED=0 时的生产实现（Windows 包走这里，见
//      deploy/packaging/package-windows.sh:69）
//   2. CGO_ENABLED=1 时**黄金对照的基准**（codec_golden_test.go 用同一组输入
//      对比它与 C 实现，逐值必须相等）
//
// 因此本文件**不带 build tag**——两条路径都要能见到它。

import "strings"

// defaultInferredContextWindow 是模型名无法推断窗口时的兜底。
//
// 32768 是个保守值，但它属于**静默降级**：模型名写 AUTO（网关自己选上游）时
// 匹配不到任何分支，内核就会拿着一份比真实小得多的窗口去算全部预算
// （实测：deepseek-v4.1-flash 能吞 990,034 token，而预算按 32768 算）。
// 兜底值本身不猜大：猜大会让请求直接撞上游 400。
const defaultInferredContextWindow = 32768

// contextWindowUnknown 是「模型名推断不出窗口」的哨兵值。
//
// 与 C 侧 HA_CODEC_CONTEXT_WINDOW_UNKNOWN 取值必须一致。
// 用哨兵而非直接返回兜底值：调用方要能区分「真推断出了」与
// 「推断不出、只能兜底」——后者必须记日志，让窗口被低估这件事可见。
const contextWindowUnknown = -1

// modelContextWindowPure 由模型名推断最大上下文窗口；推断不出返回哨兵。
// 标称窗口 ≠ 有效窗口：接近满时注意力涣散，调用方应取 70-80% 为目标利用率。
func modelContextWindowPure(model string) int {
	model = strings.ToLower(model)
	switch {
	case strings.Contains(model, "deepseek-v4") || strings.Contains(model, "deepseek-v3"):
		return 1048576
	case strings.Contains(model, "deepseek-r1") || strings.Contains(model, "deepseek-chat"):
		return 65536
	case strings.Contains(model, "gpt-4") && (strings.Contains(model, "turbo") || strings.Contains(model, "mini") || strings.Contains(model, "omni")):
		return 128000
	case strings.Contains(model, "gpt-4"):
		return 8192
	case strings.Contains(model, "gpt-3.5"):
		return 16384
	case strings.Contains(model, "claude-3.5") || strings.Contains(model, "claude-3"):
		return 200000
	case strings.Contains(model, "claude"):
		return 100000
	case strings.Contains(model, "gemini-1.5") || strings.Contains(model, "gemini-2"):
		return 1048576
	case strings.Contains(model, "gemini"):
		return 32768
	case strings.Contains(model, "qwen"):
		return 131072
	case strings.Contains(model, "glm") || strings.Contains(model, "chatglm"):
		return 131072
	case strings.Contains(model, "llama-3"):
		return 8192
	case strings.Contains(model, "llama-2"):
		return 4096
	case strings.Contains(model, "mistral") || strings.Contains(model, "mixtral"):
		return 32768
	case strings.Contains(model, "yi-") || strings.Contains(model, "零一"):
		return 200000
	case strings.Contains(model, "moonshot") || strings.Contains(model, "kimi"):
		return 131072
	default:
		return contextWindowUnknown
	}
}

// estimateTokensPure 粗略估算 token 数。
// 中文 ~1.5 token/字，英文 ~0.3 token/字符，保守估计取 max(1, runeCount * 2)。
func estimateTokensPure(text string) int {
	if text == "" {
		return 0
	}
	runeCount := len([]rune(text))
	if runeCount == 0 {
		return 0
	}
	t := runeCount * 2
	if t < 1 {
		return 1
	}
	return t
}

// truncateByTokensPure 截断字符串至不超过 maxTokens 估计值。
func truncateByTokensPure(s string, maxTokens int) string {
	if maxTokens <= 0 || s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes)*2 <= maxTokens {
		return s
	}
	keep := maxTokens / 2
	if keep >= len(runes) {
		return s
	}
	return string(runes[:keep])
}
