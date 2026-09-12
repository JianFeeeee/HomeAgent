package core

// M1 验收测试：状态机与 TaskFrame 的**行为等价性**。
//
// 设计依据 docs/zh/input-scheduler-design.md §11（R3 / X3 的 M1 形态）：
//   M1 不引入抢占，因此 R3 退化为「经状态机跑出的结果与脚本预期一致」；
//   X3 在 M1 退化为「驱动循环必然以一次终态返回结束（不空转、不超步数）」。
//
// 抢占/挂起/恢复/优先级在 M3 起才有测试。

import (
	"context"
	"errors"
	"testing"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// scriptProvider 按脚本依次返回 CompletionResponse。
//
// ChatStream 故意返回错误：驱动 chatStreamWithFallback 走非流式回退，
// 这样脚本就是「第 N 次调用返回第 N 个响应」，不依赖流式分片语义。
type scriptProvider struct {
	script []*agentAPI.CompletionResponse
	idx    int
	reqs   []*agentAPI.CompletionRequest
	// err 非空时 Chat 直接返回它（用于错误路径测试）。
	// 配合 ProviderError(401) 可跳过 2s 瞬时重试，让测试保持快速。
	err error
}

func (s *scriptProvider) Name() string { return "script" }
func (s *scriptProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	s.reqs = append(s.reqs, req)
	if s.err != nil {
		return nil, s.err
	}
	if s.idx >= len(s.script) {
		return &agentAPI.CompletionResponse{Content: ""}, nil
	}
	r := s.script[s.idx]
	s.idx++
	return r, nil
}
func (s *scriptProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return nil, errors.New("script provider: streaming disabled")
}
func (s *scriptProvider) MaxContextTokens() int { return 8192 }

func newTaskTestAgent(t *testing.T, sp agentAPI.Provider, sh *StageHost) *Agent {
	t.Helper()
	return New(AgentConfig{
		ID:              "task-test",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		StageHost:       sh,
		IO:              agentIO.NewIOManager(),
	})
}

func tc(id, name string) agentAPI.ToolCall {
	return agentAPI.ToolCall{ID: id, Name: name, Arguments: map[string]interface{}{"q": id}}
}

// R3（M1 形态）：一次工具轮 + 一次收尾轮，结果与工具调用计数必须正确。
func TestTaskFrame_R3_ToolRoundTrip(t *testing.T) {
	sh := NewStageHost()
	var got []string
	sh.RegisterTool("t_echo", sdk.ToolDef{Name: "t_echo", Plugin: "t"}, func(args map[string]interface{}) (interface{}, error) {
		got = append(got, args["q"].(string))
		return "OUT", nil
	})

	sp := &scriptProvider{script: []*agentAPI.CompletionResponse{
		{Content: "让我调用工具", ToolCalls: []agentAPI.ToolCall{tc("c1", "t_echo")}},
		{Content: "最终答复"},
	}}
	a := newTaskTestAgent(t, sp, sh)

	stageCtx := a.stageCtxFromInput("你好", "", "")
	resp, toolsUsed, toolResults, err := a.process("你好", stageCtx)
	if err != nil {
		t.Fatalf("process 返回错误: %v", err)
	}
	if resp != "最终答复" {
		t.Fatalf("响应=%q，期望 %q", resp, "最终答复")
	}
	if len(toolsUsed) != 1 || toolsUsed[0] != "t_echo" {
		t.Fatalf("toolsUsed=%v，期望恰好一次 t_echo", toolsUsed)
	}
	if len(toolResults) != 1 || toolResults[0].Name != "t_echo" || toolResults[0].Output != "OUT" {
		t.Fatalf("toolResults=%+v，期望一条 t_echo/OUT", toolResults)
	}
	if len(got) != 1 || got[0] != "c1" {
		t.Fatalf("工具实参=%v，期望恰好执行一次且参数来自脚本", got)
	}
	if len(sp.reqs) != 2 {
		t.Fatalf("LLM 调用次数=%d，期望 2（工具轮 + 收尾轮）", len(sp.reqs))
	}

	// 第二轮请求必须携带 assistant(tool_call) + tool 结果两条消息。
	msgs := sp.reqs[1].Messages
	var hasAssistantCall, hasToolResult bool
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "c1" {
			hasAssistantCall = true
		}
		if m.Role == "tool" && m.ToolCallID == "c1" && m.Content == "OUT" {
			hasToolResult = true
		}
	}
	if !hasAssistantCall || !hasToolResult {
		t.Fatalf("第二轮请求缺少工具调用配对：assistant=%v tool=%v", hasAssistantCall, hasToolResult)
	}
}

// X3（M1 形态）：多轮脚本必须在有限步内以一次终态返回结束。
func TestTaskFrame_X3_TerminatesWithinBudget(t *testing.T) {
	sh := NewStageHost()
	sh.RegisterTool("t_noop", sdk.ToolDef{Name: "t_noop", Plugin: "t"}, func(args map[string]interface{}) (interface{}, error) {
		return "ok", nil
	})

	// 3 个工具轮 + 收尾轮：状态机会在 StepLLM/StepToolBegin/.../StepTurnEnd 间往返 4 次。
	var script []*agentAPI.CompletionResponse
	for i := 0; i < 3; i++ {
		script = append(script, &agentAPI.CompletionResponse{
			Content:   "round",
			ToolCalls: []agentAPI.ToolCall{tc("c"+string(rune('a'+i)), "t_noop")},
		})
	}
	script = append(script, &agentAPI.CompletionResponse{Content: "done"})

	sp := &scriptProvider{script: script}
	a := newTaskTestAgent(t, sp, sh)

	resp, toolsUsed, toolResults, err := a.process("跑三轮", a.stageCtxFromInput("跑三轮", "", ""))
	if err != nil {
		t.Fatalf("process 返回错误: %v", err)
	}
	if resp != "done" {
		t.Fatalf("响应=%q，期望 done", resp)
	}
	if len(toolsUsed) != 3 || len(toolResults) != 3 {
		t.Fatalf("toolsUsed=%d toolResults=%d，期望各 3", len(toolsUsed), len(toolResults))
	}
	// 步数护栏未触发（触发了会是 "step budget exhausted" 错误）。
	if len(sp.reqs) != 4 {
		t.Fatalf("LLM 调用次数=%d，期望 4", len(sp.reqs))
	}
}

// 状态机对未知 step 必须失败退出而不是空转。
func TestTaskFrame_UnknownStepFails(t *testing.T) {
	sp := &scriptProvider{}
	a := newTaskTestAgent(t, sp, NewStageHost())
	f := a.newTaskFrame("x", a.stageCtxFromInput("x", "", ""))
	f.Step = Step(999)
	if out := a.step(f); out != outcomeFailed {
		t.Fatalf("未知 step 应返回 outcomeFailed，实际 %v", out)
	}
	if f.Err == nil {
		t.Fatal("未知 step 必须带错误信息")
	}
}
