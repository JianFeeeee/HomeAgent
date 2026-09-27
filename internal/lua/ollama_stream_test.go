package lua

import (
	"encoding/json"
	"testing"
)

// TestOllamaAdapterEmitsFlatToolCallsWithStreamIndex 用 **Ollama 协议**
// 的形态喂分片，验证输出是扁平结构且键名是 stream_index。
//
// ollama 的 tool_calls 是**整条一次发完**（不分片），所以 stream_index 取
// 数组下标。它此前发的是嵌套 ["function"]={...} + index 键名 —— 两种都是
// **静默**失效：Go 侧 agentAPI.ToolCall 没有 function 字段（取零值），
// 键名 index 也不会映射到 StreamIndex（取零值）。
func TestOllamaAdapterEmitsFlatToolCallsWithStreamIndex(t *testing.T) {
	vm := NewVM(t.TempDir())
	loadBundled(t, vm, "ollama")

	chunk := `{"message":{"content":"","tool_calls":[` +
		`{"id":"c0","function":{"name":"Read","arguments":"{\"p\":1}"}},` +
		`{"id":"c1","function":{"name":"Write","arguments":"{\"p\":2}"}}]},` +
		`"done":false}`
	out, err := vm.CallTransformStreamChunk("ollama", chunk)
	if err != nil {
		t.Fatalf("transform_stream_chunk: %v", err)
	}
	var u struct {
		ToolCalls []map[string]interface{} `json:"tool_calls"`
	}
	if json.Unmarshal([]byte(out), &u) != nil {
		t.Fatalf("输出非法: %s", out)
	}
	if len(u.ToolCalls) != 2 {
		t.Fatalf("应有 2 个 tool_call，实际 %d: %s", len(u.ToolCalls), out)
	}
	for i, tc := range u.ToolCalls {
		if _, nested := tc["function"]; nested {
			t.Errorf("[%d] 仍是**嵌套**形态 %v —— Go 侧没有 function 字段，"+
				"name/raw_arguments 取零值（静默）", i, tc)
		}
		if _, has := tc["name"]; !has {
			t.Errorf("[%d] 缺顶层 name: %v", i, tc)
		}
		if _, has := tc["raw_arguments"]; !has {
			t.Errorf("[%d] 缺顶层 raw_arguments: %v", i, tc)
		}
		si, ok := tc["stream_index"]
		if !ok {
			t.Errorf("[%d] 缺 stream_index: %v —— 键名写成 index 的话内核取零值，"+
				"多分片并桶、参数混拼（静默）", i, tc)
		} else if int(si.(float64)) != i {
			t.Errorf("[%d] stream_index = %v，应为 %d", i, si, i)
		}
	}
}
