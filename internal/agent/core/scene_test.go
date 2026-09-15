package core

import (
	"testing"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// TestSceneKeysFor 钉住当前场景的推导优先级：
// 注入点显式声明 > 通道 > 工具；并列命中且去重。
func TestSceneKeysFor(t *testing.T) {
	// 通道 + 工具：两个都能独立成立的触发条件，都要带上
	got := sceneKeysFor(&agentIO.InputEvent{Source: "qq"}, "qq_get_message")
	want := []string{"chan:qq", "tool:qq_get_message"}
	if len(got) != len(want) {
		t.Fatalf("sceneKeysFor = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sceneKeysFor[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// 注入点显式声明排最前；归一化生效；重复声明去重
	evt := &agentIO.InputEvent{
		Source:  "QQ",
		Payload: map[string]interface{}{"scene": " chan:qq/peer:group_1 "},
	}
	got = sceneKeysFor(evt, "")
	if len(got) != 2 || got[0] != "chan:qq/peer:group_1" || got[1] != "chan:qq" {
		t.Errorf("显式声明应排最前且通道场景归一: %v", got)
	}

	// 数组形式声明
	evt = &agentIO.InputEvent{
		Source:  "webui",
		Payload: map[string]interface{}{"scene": []interface{}{"chan:qq", "task:reminder"}},
	}
	got = sceneKeysFor(evt, "")
	if len(got) != 3 || got[0] != "chan:qq" || got[1] != "task:reminder" || got[2] != "chan:webui" {
		t.Errorf("数组声明未生效: %v", got)
	}

	// nil 事件不 panic
	if got := sceneKeysFor(nil, ""); len(got) != 0 {
		t.Errorf("nil 事件应无场景: %v", got)
	}
	// 未声明的 payload 键不影响
	evt = &agentIO.InputEvent{Source: "cli", Payload: map[string]interface{}{"recall_policy": "none"}}
	if got := sceneKeysFor(evt, ""); len(got) != 1 || got[0] != "chan:cli" {
		t.Errorf("无 scene 声明时应只有通道场景: %v", got)
	}
}
