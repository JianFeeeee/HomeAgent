package api

import "testing"

// 回归（2026-09-19）：模型名推断不出窗口时，此前会静默退回 32768。
// 生产实际配的是 core.llm.model="AUTO"，于是整个预算按 32768 算，
// 而该源真实窗口是 1M（实测 990,034 token 的 prompt 通过）—— 小 30 倍。
//
// 这里钉死两点：① deepseek-v4 系能推断出真实窗口；② AUTO 仍走兜底
// （兜底值本身不猜大：猜大会让请求直接撞上游 400）。
func TestModelContextWindow(t *testing.T) {
	cases := map[string]int{
		"deepseek/deepseek-v4.1-flash": 1048576,
		"deepseek-v4-flash":            1048576,
		"deepseek-chat":                65536,
		"claude-opus-5":                100000,
		"gpt-4-turbo":                  128000,
		"llama-3-70b":                  8192,
		"AUTO":                         32768, // 推断不出 → 兜底，靠 context_window 覆盖
	}
	for model, want := range cases {
		if got := ModelContextWindow(model); got != want {
			t.Errorf("ModelContextWindow(%q) = %d, want %d", model, got, want)
		}
	}
}

// 显式声明的 context_window 必须覆盖模型名推断 —— 这是部署方绕开
// “AUTO 推断不出窗口”的唯一手段，不能反过来被推断值盖掉。
func TestExplicitContextWindowWinsOverInference(t *testing.T) {
	p := &LuaAdaptedProvider{cfg: BaseConfig{Model: "AUTO", ContextWindow: 1048576}}
	if got := p.MaxContextTokens(); got != 1048576 {
		t.Errorf("显式 context_window 未生效：got %d, want 1048576", got)
	}
	p2 := &LuaAdaptedProvider{cfg: BaseConfig{Model: "AUTO"}}
	if got := p2.MaxContextTokens(); got != 32768 {
		t.Errorf("未声明时应走推断兜底：got %d, want 32768", got)
	}
}
