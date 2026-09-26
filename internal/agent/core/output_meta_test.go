package core

import (
	"encoding/json"
	"strings"
	"testing"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// ===== output_send 自动补收件人 =====
//
// 线上现象（2026-09-26 17:57）：用户从 QQ 私聊发来消息，agent 生成了回复、
// 也调了 output_send__qq，但**没填 meta**：
//
//	17:57:45  qq_get_message → {user_id: 2198972886, message_type: private, ...}
//	17:57:46  output_send__qq → 失败：meta 中需要 group_id 或 user_id 字段
//	17:58:14  output_send__qq_help → 查格式
//	17:58:14  output_send__qq → ok            ← 28 秒后靠重试成功
//
// 即：信息内核**本来就有**（输入事件里带着 user_id），却要模型从
// qq_get_message 的返回里手抄进 meta。抄错就失败，失败才去查 _help，
// 一次本该 5 秒的回复花了 74 秒；运气差就不重试（17:15 / 17:21 那两次
// tools=[] 压根没调发送，回复静默丢失）。
//
// 修法：meta 缺收件人时，内核从**本轮输入事件**自动补。显式传 meta 时
// 不干预（主动 DM 别人等场景行为不变）。

// ---- 判据：meta 补全逻辑 ----

// 私聊：补 user_id。
func TestOutputSendAutoFillsUserIDForPrivateChat(t *testing.T) {
	evt := &agentIO.InputEvent{
		Source:  "qq/qq",
		Payload: map[string]interface{}{"user_id": "2198972886", "message_type": "private"},
	}
	meta := autoFillOutputMeta("", "qq", evt)
	if !strings.Contains(meta, "2198972886") {
		t.Errorf("私聊应自动补 user_id，实际 meta=%q", meta)
	}
	// 必须是真的 JSON，不能是拼字符串
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		t.Fatalf("补出的 meta 不是合法 JSON: %v (meta=%q)", err, meta)
	}
	if m["user_id"] != "2198972886" {
		t.Errorf("user_id 不对: %v", m["user_id"])
	}
}

// 群聊：补 group_id（且不该同时补 user_id —— 群消息有 user_id 是发送者，
// 不是收件人）。
func TestOutputSendAutoFillsGroupIDForGroupChat(t *testing.T) {
	evt := &agentIO.InputEvent{
		Source:  "qq/qq",
		Payload: map[string]interface{}{"user_id": "111", "group_id": "999", "message_type": "group"},
	}
	meta := autoFillOutputMeta("", "qq", evt)
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		t.Fatalf("不是合法 JSON: %v (meta=%q)", err, meta)
	}
	if m["group_id"] != "999" {
		t.Errorf("群聊应补 group_id，实际 %v (meta=%q)", m["group_id"], meta)
	}
	if _, ok := m["user_id"]; ok {
		t.Errorf("群聊不该补 user_id（那是发送者不是收件人）: %q", meta)
	}
}

// 显式传了 meta 就不动它（主动 DM 等场景必须保持原行为）。
func TestOutputSendKeepsExplicitMeta(t *testing.T) {
	evt := &agentIO.InputEvent{
		Source:  "qq/qq",
		Payload: map[string]interface{}{"user_id": "2198972886"},
	}
	explicit := `{"user_id":"someone_else"}`
	if got := autoFillOutputMeta(explicit, "qq", evt); got != explicit {
		t.Errorf("显式 meta 不该被改写: got=%q want=%q", got, explicit)
	}
}

// meta 里已有 group_id（群聊）时也不该覆盖。
func TestOutputSendDoesNotOverwriteExistingRecipient(t *testing.T) {
	evt := &agentIO.InputEvent{
		Source:  "qq/qq",
		Payload: map[string]interface{}{"user_id": "111", "group_id": "999"},
	}
	explicit := `{"group_id":"777"}`
	if got := autoFillOutputMeta(explicit, "qq", evt); got != explicit {
		t.Errorf("已有收件人时不该覆盖: got=%q want=%q", got, explicit)
	}
}

// meta 是坏 JSON（模型写了错的）时不该崩，也不该当成"没传"而静默覆盖。
func TestOutputSendBadJSONMetaIsNotSilentlyReplaced(t *testing.T) {
	evt := &agentIO.InputEvent{
		Source:  "qq/qq",
		Payload: map[string]interface{}{"user_id": "2198972886"},
	}
	broken := `{"user_id": ` // 截断的 JSON
	got := autoFillOutputMeta(broken, "qq", evt)
	if got != broken {
		t.Errorf("坏 JSON 应原样保留（让下游报格式错，而不是被静默替换成别的东西）: got=%q", got)
	}
}

// 非 qq 通道（如 webui）不该被补 —— webui 回复走 ResponseCh，
// 不经过 output_send 的 meta。
func TestOutputSendNoAutoFillForWebUI(t *testing.T) {
	evt := &agentIO.InputEvent{
		Source:  "webui",
		Payload: map[string]interface{}{"user_id": "123"},
	}
	if got := autoFillOutputMeta("", "webui", evt); got != "" {
		t.Errorf("webui 通道不该补 meta: %q", got)
	}
}

// 输入事件里没有收件人信息时，保持空（让下游按原逻辑报"需要 user_id"，
// 而不是编一个错的收件人）。
func TestOutputSendNoRecipientInfoStaysEmpty(t *testing.T) {
	evt := &agentIO.InputEvent{Source: "qq/qq", Payload: map[string]interface{}{}}
	if got := autoFillOutputMeta("", "qq", evt); got != "" {
		t.Errorf("无收件人信息时应留空让下游报错，不能编: %q", got)
	}
	// evt 为 nil 也不能崩
	if got := autoFillOutputMeta("", "qq", nil); got != "" {
		t.Errorf("nil evt 应留空: %q", got)
	}
}
