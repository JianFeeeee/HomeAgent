package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

func newPermissionTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	instance, err := NewPluginFactory("qq", nil)
	if err != nil {
		t.Fatal(err)
	}
	return instance.(*Plugin)
}

func toolCallContext(name string, args map[string]interface{}) *sdk.StageContext {
	return &sdk.StageContext{ToolCalls: []sdk.ToolCall{{Name: name, Arguments: args}}}
}

func TestOwnerBypassesQQPermissionBoundary(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{active: true, owner: true, userID: 2198972886}
	ctx := toolCallContext("calendar_list", nil)
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response != nil {
		t.Fatalf("owner call rejected: %s", *ctx.Response)
	}
}

func TestPrivateResourceCannotBeAllowlisted(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.privateToolAllowlist = append(p.privateToolAllowlist, "calendar_*")
	p.auth = qqAuthContext{active: true, userID: 10001}
	ctx := toolCallContext("calendar_list", nil)
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response == nil || !strings.Contains(*ctx.Response, "私人资源工具") {
		t.Fatalf("expected private-resource denial, got %#v", ctx.Response)
	}
}

func TestNonOwnerQQHistoryIsScopedToCurrentGroup(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{active: true, messageID: 88, userID: 10001, groupID: 20002, isGroup: true}

	ctx := toolCallContext("qq_get_history", map[string]interface{}{"group_id": int64(20003)})
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response == nil || !strings.Contains(*ctx.Response, "当前 QQ 会话") {
		t.Fatalf("cross-group history not rejected: %#v", ctx.Response)
	}

	ctx = toolCallContext("qq_get_history", map[string]interface{}{"group_id": int64(20002)})
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response != nil {
		t.Fatalf("current-group history rejected: %s", *ctx.Response)
	}
}

func TestUnmatchedQQInputIsDowngraded(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{active: true, owner: true, userID: 2198972886}
	ctx := &sdk.StageContext{
		RawMessage: "来自未知事件(message_id=404)",
		Extra:      map[string]interface{}{"input_source": "qq"},
	}
	if err := p.onInputAuthContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.auth.active || p.auth.owner || p.auth.userID != 0 {
		t.Fatalf("unmatched input reused prior privilege: %+v", p.auth)
	}
}

func TestDuplicateQQOutputIsStopped(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.maxDuplicateSend = 1
	p.auth = qqAuthContext{active: true, owner: true, userID: 2198972886}
	args := map[string]interface{}{"payload": "same", "type": "text", "meta": `{"user_id":123}`}

	ctx := toolCallContext("output_send__qq", args)
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response != nil {
		t.Fatalf("first send rejected: %s", *ctx.Response)
	}

	ctx = toolCallContext("output_send__qq", args)
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response == nil || !strings.Contains(*ctx.Response, "循环保险") {
		t.Fatalf("duplicate send not stopped: %#v", ctx.Response)
	}
}

func TestGroupAndUserRouteAddsLeadingMention(t *testing.T) {
	var path string
	var request map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","retcode":0,"data":{"message_id":1}}`))
	}))
	defer server.Close()

	p := newPermissionTestPlugin(t)
	p.napcatURL = server.URL
	p.httpClient = server.Client()
	_, err := p.handleChannelOutput(map[string]interface{}{
		"payload": "hello",
		"type":    "text",
		"meta":    `{"group_id":20002,"user_id":10001}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/send_group_msg" {
		t.Fatalf("path=%q, want /send_group_msg", path)
	}
	segments, ok := request["message"].([]interface{})
	if !ok || len(segments) < 2 {
		t.Fatalf("message is not a segment array: %#v", request["message"])
	}
	mention, _ := segments[0].(map[string]interface{})
	data, _ := mention["data"].(map[string]interface{})
	if mention["type"] != "at" || data["qq"] != "10001" {
		t.Fatalf("leading mention=%#v", mention)
	}
}

// 回归：循环保险曾按“总数”拦截，导致参数不同且必需的调用被误杀。
// 现在只拦参数完全相同的重复调用。
func TestDistinctQQOutputsAreNotTreatedAsDuplicates(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{active: true, owner: true, userID: 2198972886}
	// maxDuplicateSend 默认 1：同一条消息重复才会被拦，不同消息必须全部放行。
	for i := 0; i < 5; i++ {
		ctx := toolCallContext("output_send__qq", map[string]interface{}{
			"payload": fmt.Sprintf("message-%d", i),
			"type":    "text",
			"meta":    `{"user_id":123}`,
		})
		if err := p.beforeToolcall(ctx); err != nil {
			t.Fatal(err)
		}
		if ctx.Response != nil {
			t.Fatalf("distinct message %d was blocked: %s", i, *ctx.Response)
		}
	}
}

func TestDistinctNecessaryToolCallsAreNotBlocked(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{active: true, owner: true, userID: 2198972886}
	// 旧实现 maxQQToolCalls=32 会在第 33 个不同参数的必需调用处误拦。
	for i := 0; i < 50; i++ {
		ctx := toolCallContext("cmd_run", map[string]interface{}{"command": fmt.Sprintf("cmd-%d", i)})
		if err := p.beforeToolcall(ctx); err != nil {
			t.Fatal(err)
		}
		if ctx.Response != nil {
			t.Fatalf("necessary tool call %d was blocked: %s", i, *ctx.Response)
		}
	}
}

func TestZeroLimitsMeanUnlimited(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.maxQQOutputCalls = 0
	p.maxDuplicateSend = 0
	p.maxQQToolCalls = 0
	p.auth = qqAuthContext{active: true, owner: true, userID: 2198972886}
	for i := 0; i < 30; i++ {
		ctx := toolCallContext("output_send__qq", map[string]interface{}{
			"payload": "same-content",
			"type":    "text",
			"meta":    `{"user_id":123}`,
		})
		if err := p.beforeToolcall(ctx); err != nil {
			t.Fatal(err)
		}
		if ctx.Response != nil {
			t.Fatalf("0 should mean unlimited, blocked at %d: %s", i, *ctx.Response)
		}
	}
}

// 降权（本轮无法精确匹配可信 OneBot 事件 ⇒ auth={active:true}、无 peer、非 owner）时，
// **输出仍必须放行**：发到哪个会话由 agent 自己给的 meta 决定，
// 不该被「当前会话身份」挡住。现场：被子的中断唤醒的一轮里，父带齐 meta 也发不出去
// （报「可信 QQ 会话身份不完整」）。
//
// 反之，**读取类**工具在降权时仍受当前会话限制 —— 那才是真的不能跨会话读。
func TestDowngradedAuthStillAllowsQQOutput(t *testing.T) {
	p := newPermissionTestPlugin(t)
	p.auth = qqAuthContext{active: true}
	p.privateToolAllowlist = []string{"output_send__qq", "qq_get_history"}
	p.groupToolAllowlists = map[int64][]string{0: {"output_send__qq", "qq_get_history"}}

	ctx := toolCallContext("output_send__qq", map[string]interface{}{
		"payload": "带齐 meta 的主动发送",
		"type":    "text",
		"meta":    `{"user_id":2198972886}`,
	})
	if err := p.beforeToolcall(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Response != nil {
		t.Fatalf("降权时输出被拒: %s", *ctx.Response)
	}

	ctx2 := toolCallContext("qq_get_history", map[string]interface{}{"group_id": 1027993713})
	if err := p.beforeToolcall(ctx2); err != nil {
		t.Fatal(err)
	}
	if ctx2.Response == nil || !strings.Contains(*ctx2.Response, "可信 QQ 会话身份不完整") {
		t.Fatalf("读取类工具在降权时应被当前会话限制挡住: %#v", ctx2.Response)
	}
}

// ---- 消息合并（debounce）----

// collectInterrupts 用注入钩子收集中断文本（避免测试依赖真实 SDK）。
func collectInterrupts(p *Plugin) *[]string {
	got := []string{}
	p.injectHook = func(s, _ string) { got = append(got, s) }
	return &got
}

func TestConsecutiveMessagesFromSameSenderAreBatched(t *testing.T) {
	p := newPermissionTestPlugin(t)
	got := collectInterrupts(p)
	p.batchWindow = 20 * time.Millisecond
	p.batchMax = time.Second

	for i := 0; i < 3; i++ {
		p.enqueueInterrupt("private", 10001, 0, int64(100+i), "小明", "单条", false, false)
	}
	time.Sleep(120 * time.Millisecond)

	if len(*got) != 1 {
		t.Fatalf("同一发送者连发 3 条应合并成 1 次中断，实际 %d 次: %#v", len(*got), *got)
	}
	if !strings.Contains((*got)[0], "3 条消息") {
		t.Fatalf("合并中断应说明一共几条，实际: %s", (*got)[0])
	}
	// 三个 message_id 都要带上，模型才能取全
	for _, id := range []string{"100", "101", "102"} {
		if !strings.Contains((*got)[0], id) {
			t.Fatalf("合并中断漏了 message_id=%s: %s", id, (*got)[0])
		}
	}
}

func TestDifferentSendersAreNotBatchedTogether(t *testing.T) {
	p := newPermissionTestPlugin(t)
	got := collectInterrupts(p)
	p.batchWindow = 20 * time.Millisecond
	p.batchMax = time.Second

	p.enqueueInterrupt("private", 10001, 0, 1, "小明", "a", false, false)
	p.enqueueInterrupt("private", 10002, 0, 2, "小红", "b", false, false)
	time.Sleep(120 * time.Millisecond)

	if len(*got) != 2 {
		t.Fatalf("不同发送者不该合并，应有 2 次中断，实际 %d: %#v", len(*got), *got)
	}
}

func TestBatchWindowZeroFallsBackToPerMessage(t *testing.T) {
	p := newPermissionTestPlugin(t)
	got := collectInterrupts(p)
	p.batchWindow = 0

	for i := 0; i < 3; i++ {
		p.enqueueInterrupt("private", 10001, 0, int64(i), "小明", "原文", false, false)
	}
	if len(*got) != 3 {
		t.Fatalf("关闭合并时应逐条投递（3 次），实际 %d: %#v", len(*got), *got)
	}
}

func TestSingleMessageKeepsOriginalText(t *testing.T) {
	p := newPermissionTestPlugin(t)
	got := collectInterrupts(p)
	p.batchWindow = 20 * time.Millisecond
	p.batchMax = time.Second

	p.enqueueInterrupt("group", 10001, 20002, 7, "小明", "单条原文", true, false)
	time.Sleep(120 * time.Millisecond)

	if len(*got) != 1 || (*got)[0] != "单条原文" {
		t.Fatalf("单条消息应沿用原文（含所有者前缀），实际 %#v", *got)
	}
}

// Bot 所有者/管理员的消息给 L2，普通人的给 L1 —— 否则所有者的话会被路人
// 的 L1 闲聊抢占/挤到队尾。
func TestOwnerMessagesGetHigherInterruptLevel(t *testing.T) {
	p := newPermissionTestPlugin(t)
	got := []string{}
	p.injectHook = func(text, level string) { got = append(got, text+"|"+level) }
	p.batchWindow = 20 * time.Millisecond
	p.batchMax = time.Second

	p.enqueueInterrupt("private", 1, 0, 1, "owner", "owner-msg", true, false)
	p.enqueueInterrupt("private", 2, 0, 2, "someone", "other-msg", false, false)
	time.Sleep(120 * time.Millisecond)

	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "owner-msg|L2") {
		t.Fatalf("所有者消息应为 L2，实际 %q", joined)
	}
	if !strings.Contains(joined, "other-msg|L1") {
		t.Fatalf("普通人消息应为 L1，实际 %q", joined)
	}
}
