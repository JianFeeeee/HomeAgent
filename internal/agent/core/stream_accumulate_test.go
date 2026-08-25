package core

import (
	"context"
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

	resp, err := accumulateStream(context.Background(), ch, nil)
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
	resp, err := accumulateStream(context.Background(), ch, nil)
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
