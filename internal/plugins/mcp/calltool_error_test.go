package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// MCP 工具调用的**失败必须可辨**：isError=true 不得被当成成功。
//
// ## 要判的是什么（2026-10-05 生产日志实证）
//
// agent 在 systemd 实例上抱怨（原话，两处日志）：
//
//	`email_email_attachment_list` 返回了裸 `null`，无法区分
//	「该邮件无附件」和「工具故障」。做两个只读对照探针来消歧。
//
//	Tool 13 returned a bare `null` — ambiguous. Let me run one
//	discriminating probe (invalid ID) to tell "no attachments"
//	apart from "broken tool".
//
// 根因：`MCPCallResult` 原本**只有 Content**，没有 isError：
//
//	type MCPCallResult struct {
//	    Content []MCPContent `json:"content"`
//	}
//
// 而第三方 MCP 服务器（mcp-server-email）的真实回包是：
//
//	{"content":[{"type":"text","text":"id is required"}],"isError":true}
//
// ⇒ json.Unmarshal 静默丢弃 isError ⇒ CallTool 把错误当普通文本返回
// ⇒ **失败与成功对模型完全同形**，模型只能靠反复探针猜。
//
// ★ 与本项目反复记的「报假成功比报错危险」同型。
//
// ## 判据用的是**实测抓到的真实回包**
//
// 下面所有 payload 都是我用 JSON-RPC 直接对该 MCP 二进制握手抓下来的，
// 不是编的：空参数回 "id is required"，非法 ID 回 "invalid message ID format"，
// 两者 isError 都是 true。

// callResultOf 模拟 CallTool 的解析与判定（与生产同一逻辑）。
func callResultOf(t *testing.T, raw string) (string, error) {
	t.Helper()
	var resp rpcResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal rpcResponse: %v", err)
	}
	return classifyCallResult("email", "email_attachment_list", &resp)
}

// TestCallTool_IsErrorBecomesError 钉住 isError=true 必须以 error 上抛。
func TestCallTool_IsErrorBecomesError(t *testing.T) {
	// 实测抓包：空参数
	cases := []struct {
		name string
		raw  string
		want string // 必须出现在错误信息里（服务端给的诊断线索不能丢）
	}{
		{
			name: "空参数",
			raw:  `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"id is required"}],"isError":true}}`,
			want: "id is required",
		},
		{
			name: "非法 ID",
			raw:  `{"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"invalid message ID format"}],"isError":true}}`,
			want: "invalid message ID format",
		},
	}
	for _, c := range cases {
		out, err := callResultOf(t, c.raw)
		if err == nil {
			t.Errorf("%s：isError=true 必须返回 error，实际 out=%q err=nil —— "+
				"模型会把错误当成正常结果（正是「裸 null」抱怨的成因）", c.name, out)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误信息必须保留服务端文本 %q（那是唯一诊断线索），实际 %v",
				c.name, c.want, err)
		}
	}
}

// TestCallTool_SuccessUnaffected 钉住正常成功路径不被误判。
func TestCallTool_SuccessUnaffected(t *testing.T) {
	raw := `{"jsonrpc":"2.0","id":5,"result":{"content":[{"type":"text","text":"3 attachments"}],"isError":false}}`
	out, err := callResultOf(t, raw)
	if err != nil {
		t.Fatalf("isError=false 不应报错：%v", err)
	}
	if out != "3 attachments" {
		t.Errorf("文本应原样返回，实际 %q", out)
	}
	// 服务端省掉 isError 字段（规范里它是可选的）时也应按成功处理。
	raw2 := `{"jsonrpc":"2.0","id":6,"result":{"content":[{"type":"text","text":"ok"}]}}`
	if out2, err2 := callResultOf(t, raw2); err2 != nil || out2 != "ok" {
		t.Errorf("缺省 isError 应视为成功，实际 out=%q err=%v", out2, err2)
	}
}

// TestCallTool_EmptyResultIsError 钉住「成功但没内容」也说清楚。
//
// ★ 旧实现在 resp.Result == nil 时 `return "", nil` —— 调用方拿到空串，
//
//	在工具结果里与「失败」同样难以区分。而空 content 也是同一个坑。
func TestCallTool_EmptyResultIsError(t *testing.T) {
	// result 缺失
	if _, err := callResultOf(t, `{"jsonrpc":"2.0","id":7}`); err == nil {
		t.Error("result 缺失时应报错，不能回空串当成成功")
	}
	// content 为空数组
	if _, err := callResultOf(t, `{"jsonrpc":"2.0","id":8,"result":{"content":[]}}`); err == nil {
		t.Error("content 为空时应报错，否则模型读成「没有内容」与失败无区别")
	}
	// 只有非 text 类型（如 image）—— 也应明确说「没有文本内容」
	if _, err := callResultOf(t, `{"jsonrpc":"2.0","id":9,"result":{"content":[{"type":"image"}]}}`); err == nil {
		t.Error("无 text 片段时应报错")
	}
}

// TestCallTool_JSONRPCLevelError 钉住 JSON-RPC 层 error 仍走原路。
func TestCallTool_JSONRPCLevelError(t *testing.T) {
	if _, err := callResultOf(t,
		`{"jsonrpc":"2.0","id":10,"error":{"code":-32601,"message":"method not found"}}`); err == nil {
		t.Error("JSON-RPC error 应报错")
	}
}
