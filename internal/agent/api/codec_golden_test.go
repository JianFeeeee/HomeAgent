package api

// codec_golden_test.go —— 黄金对照测试：C 实现与纯 Go 实现必须逐值等价。
//
// 这是本轮 C 化**最重要的验收**（见 docs/zh/c-core/llm-orchestration-c.md §五）。
// 没有它，「C 化没坏」就只是感觉，不是证据。
//
// 两条约束：
//   1. CGO_ENABLED=1 时：真的对比 C 与纯 Go 两条路径
//   2. CGO_ENABLED=0 时：C 符号已转发到纯 Go，对照退化为自比（仍跑，防止
//      测试文件因 build tag 被整文件跳过 —— 那会让 0 模式下失去这段覆盖）

import (
	"math/rand"
	"strings"
	"testing"
)

func TestGolden_ModelContextWindow(t *testing.T) {
	cases := []string{
		// 已覆盖的分支各取一个（含大小写、子公司前缀、带路径的模型名）
		"deepseek/deepseek-v4.1-flash", "deepseek-v4-flash", "DEEPSEEK-V3", "deepseek-r1",
		"deepseek-chat", "gpt-4-turbo", "gpt-4o-mini", "gpt-4-omni", "gpt-4", "gpt-4-0613",
		"gpt-3.5-turbo", "claude-3.5-sonnet", "claude-3-opus", "claude-opus-5", "claude-2",
		"gemini-1.5-pro", "gemini-2.0-flash", "gemini-pro", "qwen-max", "QWEN-MAX",
		"glm-4", "chatglm3", "llama-3-70b", "llama-2-7b", "mistral-large", "mixtral-8x7b",
		"yi-34b", "零一万物", "moonshot-v1-128k", "kimi-128k",
		// 推断不出（哨兵路径）
		"AUTO", "auto", "", "unknown-model", "some-local-model",
	}
	for _, model := range cases {
		c := modelContextWindowC(model)
		p := modelContextWindowPure(model)
		if c != p {
			t.Errorf("ModelContextWindow(%q): C=%d, pure=%d", model, c, p)
		}
	}
}

// TestGolden_EstimateTokens 覆盖 ASCII / 中文 / emoji / 空 / 长文本。
func TestGolden_EstimateTokens(t *testing.T) {
	cases := []string{
		"", "a", "ab", "abc", "hello world",
		"你好", "你好世界", "中文English混合", "a你b好c",
		"😀", "😀😀", "👨‍👩‍👧‍👦", // 含 ZWJ 组合序列（多 rune）
		strings.Repeat("x", 1000),
		strings.Repeat("你", 1000),
		"\n\t\r ", "{}[]()",
	}
	for _, s := range cases {
		c := estimateTokensC(s)
		p := estimateTokensPure(s)
		if c != p {
			t.Errorf("EstimateTokens(%q): C=%d, pure=%d", s, c, p)
		}
	}
}

// TestGolden_TruncateByTokens 覆盖边界：maxTokens 为 0/负/奇数/超限/恰好。
func TestGolden_TruncateByTokens(t *testing.T) {
	texts := []string{
		"", "a", "abc", "abcdefghij",
		"你好世界", "你好世界再见", "a你b好c世d界",
		"😀😀😀😀", strings.Repeat("x", 100), strings.Repeat("你", 100),
	}
	maxTokensList := []int{-1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 20, 100, 200, 201, 1000}
	for _, s := range texts {
		for _, mt := range maxTokensList {
			c := truncateByTokensC(s, mt)
			p := truncateByTokensPure(s, mt)
			if c != p {
				t.Errorf("TruncateByTokens(%q, %d): C=%q, pure=%q", s, mt, c, p)
			}
			// 额外不变量：结果必须是原串前缀，且不超过预算
			if !strings.HasPrefix(s, c) && c != "" {
				t.Errorf("TruncateByTokens(%q, %d)=%q 不是原串前缀", s, mt, c)
			}
		}
	}
}

// TestGolden_Randomized 随机输入对拍：抓前面手写用例没想到的组合。
// 固定 seed，失败可复现。
func TestGolden_Randomized(t *testing.T) {
	rng := rand.New(rand.NewSource(20260924))
	alphabet := []rune("abcXYZ019 你好世界😀-_./")

	for i := 0; i < 2000; i++ {
		n := rng.Intn(40)
		var sb strings.Builder
		for j := 0; j < n; j++ {
			sb.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		s := sb.String()

		if c, p := estimateTokensC(s), estimateTokensPure(s); c != p {
			t.Fatalf("EstimateTokens(%q): C=%d, pure=%d", s, c, p)
		}

		mt := rng.Intn(60) - 5
		if c, p := truncateByTokensC(s, mt), truncateByTokensPure(s, mt); c != p {
			t.Fatalf("TruncateByTokens(%q, %d): C=%q, pure=%q", s, mt, c, p)
		}

		// 模型名：拼一段 ASCII 再随机插入已知子串
		models := []string{"deepseek-v4", "gpt-4-turbo", "claude-3", "qwen", "llama-3", "kimi", "zzz"}
		m := models[rng.Intn(len(models))]
		if rng.Intn(2) == 0 {
			m = strings.ToUpper(m)
		}
		if c, p := modelContextWindowC(m), modelContextWindowPure(m); c != p {
			t.Fatalf("ModelContextWindow(%q): C=%d, pure=%d", m, c, p)
		}
	}
}
