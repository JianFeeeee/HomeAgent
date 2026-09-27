package lua

import (
	"encoding/json"
	"testing"
)

// TestDeepSeekAdapterHandlesStreamToolCalls 钉住「deepseek 源的流式模式也能工具调用」。
//
// ★ 为什么单独给 deepseek 写判据：
//
// 它的 transform_stream_chunk 只透 content/done，**完全不处理 tool_calls**
// （那部分代码只存在于 transform_response，即非流式路径）。于是：
//
//	· 非流式请求 ⇒ 工具调用正常
//	· 流式请求   ⇒ 工具调用**全部丢失**，模型只收到纯文本
//
// 而内核的 tool call 循环默认走流式（provider.go 的 stream 分支）。所以
// 配了 deepseek 源的用户，模型调不动任何工具，且**没有任何报错** ——
// 只是"工具好像不听话"。
//
// 这类缺陷极难察觉：功能判据（core 包的批内测试）直接构造 Go 结构体，
// 完全绕过适配器；而非流式的端到端路径又是好的。
func TestDeepSeekAdapterHandlesStreamToolCalls(t *testing.T) {
	vm := NewVM(t.TempDir())
	loadBundled(t, vm, "deepseek")

	// 一个含 2 个 tool_call 的 chunk（首片 + 续传片各一）
	frags := []string{
		`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[` +
			`{"index":0,"id":"t0","type":"function","function":{"name":"cmd_run","arguments":"{}"}}]}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[` +
			`{"index":1,"id":"t1","type":"function","function":{"name":"cmd_run","arguments":"{}"}}]}}]}`,
	}
	for i, f := range frags {
		out, err := vm.CallTransformStreamChunk("deepseek", f)
		if err != nil {
			t.Fatalf("第 %d 片: %v", i, err)
		}
		var u struct {
			ToolCalls []struct {
				StreamIndex  int    `json:"stream_index"`
				ID           string `json:"id"`
				Name         string `json:"name"`
				RawArguments string `json:"raw_arguments"`
			} `json:"tool_calls"`
		}
		if json.Unmarshal([]byte(out), &u) != nil {
			t.Fatalf("第 %d 片输出非法: %s", i, out)
		}
		if len(u.ToolCalls) == 0 {
			t.Errorf("第 %d 片：deepseek 适配器的流式路径**丢掉了 tool_call**\n"+
				"  输出：%s\n"+
				"  ⇒ deepseek 源在流式模式下无法调用任何工具，且无任何报错。\n"+
				"    它的 tool_calls 处理只存在于 transform_response（非流式路径）。",
				i, out)
			continue
		}
		if u.ToolCalls[0].StreamIndex != i {
			t.Errorf("第 %d 片的 stream_index = %d，应为 %d", i, u.ToolCalls[0].StreamIndex, i)
		}
		if u.ToolCalls[0].Name != "cmd_run" {
			t.Errorf("第 %d 片 name = %q，应为 cmd_run", i, u.ToolCalls[0].Name)
		}
	}
}
