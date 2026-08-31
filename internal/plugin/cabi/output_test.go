package cabi

import (
	"errors"
	"strings"
	"testing"
	"time"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// output_send 不再假成功（plan.md 11.1）：sent / error / unconfirmed 三态。

func TestAwaitOutputResult_Success(t *testing.T) {
	res, err := awaitOutputResultWith(0, "qq", `{"x":1}`, func(pid int32, ch, args string) error {
		return nil
	}, outputSendTimeout)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["status"] != "sent" {
		t.Fatalf("expected status=sent, got %v", m["status"])
	}
}

func TestAwaitOutputResult_Failure(t *testing.T) {
	_, err := awaitOutputResultWith(0, "qq", `{}`, func(pid int32, ch, args string) error {
		return errors.New("meta 中需要 group_id 或 user_id 字段")
	}, outputSendTimeout)
	if err == nil {
		t.Fatal("expected error on failed send, got nil (旧实现会谎报成功)")
	}
	if !strings.Contains(err.Error(), "需要 group_id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAwaitOutputResult_Timeout(t *testing.T) {
	res, err := awaitOutputResultWith(0, "qq", `{}`, func(pid int32, ch, args string) error {
		time.Sleep(2 * time.Second) // 模拟插件发送迟迟不确认
		return nil
	}, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("unconfirmed 不应返回 error，got %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["status"] != "unconfirmed" {
		t.Fatalf("expected status=unconfirmed, got %v", m["status"])
	}
}

// applyStageResult 必须能表达「插件清空了 tool_calls/tool_results」——
// ABI v2 diff 回传（plan.md 11.3）下插件拒绝全部工具调用时会显式回传 []。
func TestApplyStageResult_ClearedSlicesAreApplied(t *testing.T) {
	sc := &sdk.StageContext{
		ToolCalls:   []sdk.ToolCall{{ID: "t1", Name: "cmd_run"}},
		ToolResults: []sdk.ToolResult{{CallID: "t1", Name: "cmd_run", Result: "x"}},
	}
	applyStageResult(sc, `{"tool_calls":[],"tool_results":[]}`)
	if len(sc.ToolCalls) != 0 {
		t.Fatalf("tool_calls 应被清空，实际 %v", sc.ToolCalls)
	}
	if len(sc.ToolResults) != 0 {
		t.Fatalf("tool_results 应被清空，实际 %v", sc.ToolResults)
	}
}

// diff 回传只带变更字段：未出现的键不得被改动（避免旧快照覆盖）。
func TestApplyStageResult_OnlyPresentKeysApplied(t *testing.T) {
	sc := &sdk.StageContext{
		RawMessage:  "原始输入",
		LLMText:     "原始LLM",
		FinalText:   "原始最终",
		ToolResults: []sdk.ToolResult{{CallID: "c1", Result: "已清洗"}},
	}
	// 只回传 final_text 的变更
	applyStageResult(sc, `{"final_text":"新最终"}`)

	if sc.FinalText != "新最终" {
		t.Fatalf("final_text 应被应用，实际 %q", sc.FinalText)
	}
	if sc.RawMessage != "原始输入" {
		t.Errorf("raw_message 未回传却被改动: %q", sc.RawMessage)
	}
	if sc.LLMText != "原始LLM" {
		t.Errorf("llm_text 未回传却被改动: %q", sc.LLMText)
	}
	if len(sc.ToolResults) != 1 || sc.ToolResults[0].Result != "已清洗" {
		t.Errorf("tool_results 未回传却被改动: %v", sc.ToolResults)
	}
}
