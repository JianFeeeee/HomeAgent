package core

import (
	"context"
	"strings"
	"testing"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
)

// 验证流式 tool call 分片累积：模拟 llmsproxy/big-pickle 的分片序列
func TestAccumulateStreamToolCalls(t *testing.T) {
	ch := make(chan agentAPI.StreamChunk, 10)
	go func() {
		// 分片1: name + id + arguments 开头
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{
			{ID: "call_1", Name: "cmd_run", RawArguments: "{\""},
		}}
		// 分片2-3: 只有 arguments 分片
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{
			{RawArguments: "command\""},
		}}
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{
			{RawArguments: ":\"date\"}"},
		}}
		ch <- agentAPI.StreamChunk{Done: true, FinishReason: "tool_calls"}
		close(ch)
	}()

	resp, err := accumulateStream(context.Background(), ch, nil, "cli", 4096)
	if err != nil {
		t.Fatalf("accumulateStream: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "cmd_run" || tc.ID != "call_1" {
		t.Fatalf("bad name/id: %s/%s", tc.ID, tc.Name)
	}
	cmd, _ := tc.Arguments["command"].(string)
	if cmd != "date" {
		t.Fatalf("arguments not merged, got: %v", tc.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish reason: %q", resp.FinishReason)
	}
}

// 验证 content/reasoning 增量累积
func TestAccumulateStreamContent(t *testing.T) {
	ch := make(chan agentAPI.StreamChunk, 5)
	go func() {
		ch <- agentAPI.StreamChunk{ReasoningContent: "think "}
		ch <- agentAPI.StreamChunk{Content: "你"}
		ch <- agentAPI.StreamChunk{Content: "好"}
		ch <- agentAPI.StreamChunk{Done: true, FinishReason: "stop"}
		close(ch)
	}()
	resp, err := accumulateStream(context.Background(), ch, nil, "cli", 4096)
	if err != nil {
		t.Fatalf("accumulateStream: %v", err)
	}
	if resp.Content != "你好" {
		t.Fatalf("content: %q", resp.Content)
	}
	if resp.ReasoningContent != "think " {
		t.Fatalf("reasoning: %q", resp.ReasoningContent)
	}
}

// 回归（2026-09-19 实测事故）：长参数工具调用被 max_tokens 从中间截断时，
// 上游发 finish_reason="length"、参数 JSON 残缺。旧实现把残缺 JSON 静默降级成
// 空 map，工具只报 "path is required"，模型看不出真因、原样重试四次。
//
// 本测试钉死：截断必须变成带指引的 __truncated_error，而不是空参数。
func TestAccumulateStreamTruncatedArgsSurfaced(t *testing.T) {
	ch := make(chan agentAPI.StreamChunk, 10)
	go func() {
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{
			{ID: "call_1", Name: "files_write", RawArguments: `{"path":"/tmp/a.py","content":"# -*- coding`},
		}}
		// 参数写到一半被切断，随后到达 length 终止块
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{
			{RawArguments: `: utf-8 -*-\nimport openpyxl\nfor i in range(80):\n    w`},
		}}
		ch <- agentAPI.StreamChunk{Done: true, FinishReason: "length"}
		close(ch)
	}()

	resp, err := accumulateStream(context.Background(), ch, nil, "cli", 4096)
	if err != nil {
		t.Fatalf("accumulateStream: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(resp.ToolCalls))
	}
	msg, ok := resp.ToolCalls[0].Arguments["__truncated_error"].(string)
	if !ok || msg == "" {
		t.Fatalf("截断的参数必须带 __truncated_error，实际 Arguments=%v", resp.ToolCalls[0].Arguments)
	}
	// 指引必须可执行：说出真因（截断/max_tokens）并给出拆小方案
	for _, want := range []string{"截断", "max_tokens=4096", "拆成多次调用"} {
		if !strings.Contains(msg, want) {
			t.Errorf("指引缺少 %q：%s", want, msg)
		}
	}
	// 截断时绝不能把残缺 JSON 解析出的空 map 当参数交出去
	if _, hasPath := resp.ToolCalls[0].Arguments["path"]; hasPath {
		t.Error("截断参数不应残留任何可用字段（否则会以残缺参数执行）")
	}
}

// 非截断的残缺 JSON 保持旧行为（静默降级成空 map，由工具自己的必填校验报错）：
// 这样不会把「厂商不回 finish_reason」的流也误判成截断。
func TestAccumulateStreamInvalidArgsNotFlaggedAsTruncated(t *testing.T) {
	ch := make(chan agentAPI.StreamChunk, 10)
	go func() {
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{
			{ID: "call_1", Name: "files_write", RawArguments: `{"path":`},
		}}
		ch <- agentAPI.StreamChunk{Done: true, FinishReason: "tool_calls"}
		close(ch)
	}()

	resp, err := accumulateStream(context.Background(), ch, nil, "cli", 4096)
	if err != nil {
		t.Fatalf("accumulateStream: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(resp.ToolCalls))
	}
	if _, ok := resp.ToolCalls[0].Arguments["__truncated_error"]; ok {
		t.Error("finish_reason=tool_calls 时不应标记为截断")
	}
}
