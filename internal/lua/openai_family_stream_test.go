package lua

import (
	"encoding/json"
	"testing"
)

// openAICompatibleStreamAdapters 是「OpenAI 兼容流式协议」那一族适配器。
//
// 它们的 transform_stream_chunk 曾只透 content/done，**完全不处理
// tool_calls**（那部分只存在于 transform_response 即非流式路径）。后果：
// 流式模式下工具调用全部丢失，模型调不动任何工具，且没有任何报错。
//
// 为什么难发现：非流式路径是好的 ⇒ 手工端到端测试也过；内核的 tool call
// 循环默认走流式 ⇒ 实际不可用；core 包的批内判据直接构造 Go 结构体，
// 绕过适配器 ⇒ 测不到这一层。
//
// ★ 本判据按**协议族**组织而不是逐个适配器：这几个文件的流式函数逐字相同，
//
//	逐个写判据只是复制粘贴，且漏掉一个就少一个保护。
var openAICompatibleStreamAdapters = []string{"openai", "deepseek", "github", "groq", "mistral"}

func TestOpenAICompatibleFamilyHandlesStreamToolCalls(t *testing.T) {
	// 两个 tool_call，各一片：首片带 name、续传片只带 arguments
	frags := []string{
		`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[` +
			`{"index":0,"id":"t0","type":"function","function":{"name":"cmd_run","arguments":""}}]}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[` +
			`{"index":1,"function":{"arguments":"{\"command\":\"x\"}"}}]}}]}`,
	}
	for _, name := range openAICompatibleStreamAdapters {
		t.Run(name, func(t *testing.T) {
			vm := NewVM(t.TempDir())
			loadBundled(t, vm, name)
			for i, f := range frags {
				out, err := vm.CallTransformStreamChunk(name, f)
				if err != nil {
					t.Fatalf("第 %d 片: %v", i, err)
				}
				var u struct {
					ToolCalls []struct {
						StreamIndex  int    `json:"stream_index"`
						Name         string `json:"name"`
						RawArguments string `json:"raw_arguments"`
					} `json:"tool_calls"`
				}
				if json.Unmarshal([]byte(out), &u) != nil {
					t.Fatalf("第 %d 片输出非法: %s", i, out)
				}
				if len(u.ToolCalls) == 0 {
					t.Errorf("第 %d 片：%s 的流式路径**丢掉了 tool_call**\n"+
						"  输出：%s\n"+
						"  ⇒ 该源在流式模式下无法调用任何工具，且无任何报错。",
						i, name, out)
					continue
				}
				if u.ToolCalls[0].StreamIndex != i {
					t.Errorf("第 %d 片 stream_index = %d，应为 %d —— 缺它会让内核"+
						"把多个分片并到同一个桶，参数混拼成非法 JSON", i, u.ToolCalls[0].StreamIndex, i)
				}
			}
		})
	}
}
