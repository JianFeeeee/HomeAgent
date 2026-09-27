package lua

import (
	"encoding/json"
	"testing"
)

// TestAnthropicAdapterEmitsFlatToolCallsWithStreamIndex 用 **Anthropic 协议**
// 的形态喂分片，验证输出是**扁平**结构且键名是 stream_index。
//
// ★ 与 OpenAI 族的判据分开，原因有二：
//
//  1. 协议不同：Anthropic 用 content_block_start / content_block_delta +
//     input_json_delta，不是 OpenAI 的 delta.tool_calls。共用一个 fixture
//     会因"不适用该 chunk"而跳过 —— 看着绿，实则没测。
//  2. 它此前是**两处都错**：发嵌套 `["function"]={...}`，且键名用 `index`
//     而非 `stream_index`。两种都是**静默**失效（Go 侧按 json tag 取值，取不到
//     就是零值，没有报错）：
//     · 嵌套 ⇒ name / raw_arguments 取零值 ⇒ flush 时判 "无 name" 丢弃
//     · 键名 index ⇒ StreamIndex 取零值 ⇒ 多分片并桶 ⇒ argsRaw 混拼
func TestAnthropicAdapterEmitsFlatToolCallsWithStreamIndex(t *testing.T) {
	vm := NewVM(t.TempDir())
	loadBundled(t, vm, "anthropic")

	// 首片：content_block_start，声明工具名
	start := `{"type":"content_block_start","index":2,` +
		`"content_block":{"type":"tool_use","id":"toolu_01","name":"Read"}}`
	// 续传片：content_block_delta + input_json_delta
	delta := `{"type":"content_block_delta","index":2,` +
		`"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}`

	for i, f := range []string{start, delta} {
		out, err := vm.CallTransformStreamChunk("anthropic", f)
		if err != nil {
			t.Fatalf("第 %d 片: %v", i, err)
		}
		var u struct {
			ToolCalls []map[string]interface{} `json:"tool_calls"`
		}
		if json.Unmarshal([]byte(out), &u) != nil {
			t.Fatalf("第 %d 片输出非法: %s", i, out)
		}
		if len(u.ToolCalls) == 0 {
			t.Fatalf("第 %d 片没有 tool_calls: %s", i, out)
		}
		tc := u.ToolCalls[0]
		// 必须是扁平：name / raw_arguments 在顶层，不能藏在 ["function"] 里
		if _, nested := tc["function"]; nested {
			t.Errorf("第 %d 片仍是**嵌套**形态 %v —— Go 侧 agentAPI.ToolCall 没有 "+
				"function 字段，name/raw_arguments 会取零值（静默）", i, tc)
		}
		if _, has := tc["name"]; !has {
			t.Errorf("第 %d 片缺顶层 name 键: %v", i, tc)
		}
		if _, has := tc["raw_arguments"]; !has {
			t.Errorf("第 %d 片缺顶层 raw_arguments 键: %v", i, tc)
		}
		// 键名必须是 stream_index，不是 index
		if si, ok := tc["stream_index"]; !ok {
			t.Errorf("第 %d 片缺 stream_index 键: %v —— 键名写成 index 的话内核取零值，"+
				"多个分片并到同一个桶、参数混拼（静默）", i, tc)
		} else if int(si.(float64)) != 2 {
			t.Errorf("第 %d 片 stream_index = %v，应为 2", i, si)
		}
	}
}
