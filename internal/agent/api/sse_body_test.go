package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// 锁定契约：网关（llmsproxy auto 链等）在非流式请求下返回 SSE 流 body 时，
// 必须拼接为完整响应而不是报 "invalid character 'd'" 丢掉已生成的回复。
// 事故样本取自 2026-08-25 生产日志：上游恢复后吐出完整 chunk 流被非流式解析器丢弃。
func TestParseOpenAICompatibleSSEBody(t *testing.T) {
	body := "data: {\"id\":\"chatcmpl-572\",\"object\":\"chat.completion.chunk\",\"created\":1787630289,\"model\":\"x-preview-f-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"},\"finish_reason\":null}]}\n" +
		"\n" +
		"data: {\"id\":\"chatcmpl-572\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"，世界\"},\"finish_reason\":null}]}\n" +
		"\n" +
		"data: {\"id\":\"chatcmpl-572\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}]}\n" +
		"data: {\"id\":\"chatcmpl-572\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"total_tokens\":107}}\n" +
		"data: [DONE]\n"

	resp, ok := parseOpenAICompatibleSSEBody([]byte(body))
	if !ok {
		t.Fatal("expected SSE body to be recognized")
	}
	if resp.Content != "你好，世界" {
		t.Errorf("content = %q, want %q", resp.Content, "你好，世界")
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", resp.FinishReason)
	}
	if resp.TokenUsage.Total != 107 || resp.TokenUsage.Prompt != 100 || resp.TokenUsage.Completion != 7 {
		t.Errorf("usage = %+v, want prompt=100 completion=7 total=107", resp.TokenUsage)
	}
}

func TestParseOpenAICompatibleSSEBodyRejectsPlainJSON(t *testing.T) {
	plain := `{"choices":[{"message":{"content":"hi"}}]}`
	if _, ok := parseOpenAICompatibleSSEBody([]byte(plain)); ok {
		t.Fatal("plain JSON body must not be treated as SSE")
	}
}

func TestParseOpenAICompatibleSSEBodyToolCallShards(t *testing.T) {
	// 用 json.Marshal 构建测试数据，避免 Go 字面量转义错误
	chunk1 := map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index": 0,
			"delta": map[string]interface{}{
				"tool_calls": []map[string]interface{}{{
					"index": 0,
					"id":    "call_1",
					"type":  "function",
					"function": map[string]interface{}{
						"name":      "exec",
						"arguments": `{"command":`,
					},
				}},
			},
		}},
	}
	chunk2 := map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index": 0,
			"delta": map[string]interface{}{
				"tool_calls": []map[string]interface{}{{
					"index": 0,
					"function": map[string]interface{}{
						"arguments": `"date"}`,
					},
				}},
			},
		}},
	}
	chunk3 := map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         map[string]interface{}{},
			"finish_reason": "tool_calls",
		}},
	}

	var sb strings.Builder
	for _, c := range []map[string]interface{}{chunk1, chunk2, chunk3} {
		b, _ := json.Marshal(c)
		sb.WriteString("data: ")
		sb.Write(b)
		sb.WriteString("\n")
	}
	sb.WriteString("data: [DONE]\n")

	t.Logf("SSE body:\n%s", sb.String())

	resp, ok := parseOpenAICompatibleSSEBody([]byte(sb.String()))
	if !ok {
		t.Fatal("expected SSE body to be recognized")
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "exec" || tc.ID != "call_1" {
		t.Errorf("tool call name/id = %q/%q, want exec/call_1", tc.Name, tc.ID)
	}
	args := tc.RawArguments
	if args != `{"command":"date"}` {
		t.Errorf("raw args = %q", args)
	}
	if tc.Arguments["command"] != "date" {
		t.Errorf("parsed args = %v, want command=date", tc.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", resp.FinishReason)
	}
}

func TestParseOpenAICompatibleSSEBodyEmptyStream(t *testing.T) {
	body := "data: \ndata: \n"
	if resp, ok := parseOpenAICompatibleSSEBody([]byte(body)); ok && strings.TrimSpace(resp.Content) != "" {
		t.Fatalf("empty stream should not parse into non-empty response")
	}
}
