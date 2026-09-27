package api

import (
	"encoding/json"
	"testing"
)

// `has empty arguments` 诊断日志在部署后的生产日志里出现了 14 次，
// 全部是误报。原始响应（从日志里扒出来的）参数**完好**：
//
//	"tool_calls":[{"function":{"arguments":"{}","name":"clawhubadapter_list"},...}]
//
// 根因：`parseToolArguments("{}")` 走 string 分支 → `json.Unmarshal("{}", &m)`
// 成功且 `m != nil`（**非 nil 的空 map**）⇒ 返回空 map。而诊断条件是
// `len(args)==0 && RawArguments==""`，于是命中。
//
// 被点名的全是**零参数工具**（seq_list / *_list / seq_help，它们的
// `properties` 本来就是 `{}`）。
//
// ## 为什么要紧
//
// 不是"日志吵"，是它**占用了本该报真问题的位置**：这条诊断存在的意义
// 是抓「上游/适配器真的把参数丢了」，真发生时会被这堆噪音淹没。
// 诊断日志一旦失去信噪比就等于没有。
func TestEmptyArgumentsDiagnosticIgnoresExplicitEmptyObject(t *testing.T) {
	cases := []struct {
		name    string
		rawArgs string
		wantLog bool
	}{
		// 上游明确给了空对象 ⇒ 参数没丢，是零参数工具的正常形态
		{"显式空对象 {}", "{}", false},
		{"显式空对象带空格 { }", " { } ", false},
		// 真正丢了参数：连 "{}" 都没有
		{"完全缺失", "", true},
		{"只有空白", "   ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := argsLookDropped(c.rawArgs)
			if got != c.wantLog {
				t.Errorf("argsLookDropped(%q) = %v，期望 %v", c.rawArgs, got, c.wantLog)
			}
		})
	}
}

// 上游给的是**非空**参数时，当然不能报。
func TestArgsLookDroppedIgnoresNonEmpty(t *testing.T) {
	for _, raw := range []string{`{"a":1}`, `{"device_id":"x"}`, `{"path":"/tmp"}`} {
		if argsLookDropped(raw) {
			t.Errorf("argsLookDropped(%q) = true，非空参数不应被判为丢失", raw)
		}
	}
}

// 端到端：从真实的 tool_call 形态走到判定，确认零参数工具不报、
// 真丢失要报。这是防止"单测过了但接缝不对"。
func TestArgsLookDroppedEndToEnd(t *testing.T) {
	// 日志里出现过的真实 body
	const zeroParamBody = `{"choices":[{"message":{"tool_calls":[
		{"function":{"arguments":"{}","name":"seq_list"},"id":"a","type":"function"}]}}]}`
	const droppedBody = `{"choices":[{"message":{"tool_calls":[
		{"function":{"name":"seq_list"},"id":"a","type":"function"}]}}]}`

	// 用 openAIToolCall —— normalizeOpenAIToolCalls 的真实入参类型。
	// （先前误用 apiToolCall，那是**非流式**路径的结构，接缝不对。）
	normalize := func(body string) []ToolCall {
		var resp struct {
			Choices []struct {
				Message struct {
					ToolCalls []openAIToolCall `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		return normalizeOpenAIToolCalls(resp.Choices[0].Message.ToolCalls)
	}

	for _, tc := range normalize(zeroParamBody) {
		if argsLookDropped(tc.RawArguments) {
			t.Errorf("零参数工具 %q 被误报为参数丢失", tc.Name)
		}
	}
	for _, tc := range normalize(droppedBody) {
		if !argsLookDropped(tc.RawArguments) {
			t.Errorf("真丢失参数的 %q 未被报出 —— 这条诊断会失效", tc.Name)
		}
	}
}
