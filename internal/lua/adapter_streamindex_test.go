package lua

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// ★ 必须真正加载并执行**内嵌的 openai.lua**。
//
// 为什么不复用 core 里的 stream_index_test.go：那个判据直接构造 Go 结构体
// agentAPI.StreamChunk{...}，**不经过 Lua 适配器** —— 于是"适配器有没有把
// 上游 index 透传出来"这件事它永远测不到。
//
// 历史教训：提交 ddef195（2026-08-26，"流式并行 tool_call 按 JSON index 分桶"）
// 的说明里写着「openai.lua 输出 stream_index 字段」，Go 侧也加了
// StreamIndex int `json:"stream_index,omitempty"` 并注明"lua 适配器以
// stream_index 键透传" —— 但那次提交**根本没有改 openai.lua**（6 个文件里
// 没有它）。内核侧逻辑写好了、判据也加上了，唯独透传那一步从未落地。
//
// 生产为什么没暴露：单工具调用时上游 index 恒为 0，缺省也是 0，分桶恰好正确。
// 只有一轮**多个** tool_call（index=1,2,3…）时才会全部并到槽 0。
type streamIndex struct {
	StreamIndex  int    `json:"stream_index"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	RawArguments string `json:"raw_arguments"`
}

type unifiedChunk struct {
	ToolCalls []streamIndex `json:"tool_calls"`
	Content   string        `json:"content"`
	Done      bool          `json:"done"`
}

// openAIMultiToolChunk 造一个「一轮 3 个 tool_call」的首分片，形态取自真实
// OpenAI 流式协议：每个元素带 index/id/name/arguments。
const openAIMultiToolChunk = `{"id":"c","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[` +
	`{"index":0,"id":"c0","type":"function","function":{"name":"cmd_run","arguments":"{\"command\":\"a\"}"}},` +
	`{"index":1,"id":"c1","type":"function","function":{"name":"cmd_run","arguments":"{\"command\":\"b\"}"}},` +
	`{"index":2,"id":"c2","type":"function","function":{"name":"cmd_run","arguments":"{\"command\":\"c\"}"}}` +
	`]}}]}`

// TestOpenAIAdapterPassesThroughStreamIndex 是本判据的核心。
//
// 断言：三个 tool_call 的 stream_index 必须是 0/1/2。
//
// 现状（缺透传）下三者全为 0 —— 内核 accumulateStream 会把三个分片并到
// 同一个桶，argsRaw 互相混拼，最终每个工具都报"参数不是合法 JSON"，
// 而工具一次都没真跑过。
func TestOpenAIAdapterPassesThroughStreamIndex(t *testing.T) {
	vm := NewVM(t.TempDir())
	loadBundled(t, vm, "openai")

	out, err := vm.CallTransformStreamChunk("openai", openAIMultiToolChunk)
	if err != nil {
		t.Fatalf("transform_stream_chunk: %v", err)
	}
	var u unifiedChunk
	if err := json.Unmarshal([]byte(out), &u); err != nil {
		t.Fatalf("适配器输出不是合法 unified JSON: %v\n输出：%s", err, out)
	}
	if len(u.ToolCalls) != 3 {
		t.Fatalf("应透传 3 个 tool_call，实际 %d 个：%s", len(u.ToolCalls), out)
	}
	for i, tc := range u.ToolCalls {
		if tc.StreamIndex != i {
			t.Errorf("第 %d 个 tool_call 的 stream_index = %d，应为 %d\n"+
				"★ 缺失 index 透传会让内核把多个分片并到同一个桶（process.go:347 "+
				"`idx := tc.StreamIndex`），参数混拼成非法 JSON。\n完整输出：%s",
				i, tc.StreamIndex, i, out)
		}
	}
}

// TestOpenAIAdapterKeepsContinuationFragment 续传分片（只带 arguments、
// 不带 name）必须被保留 —— 它的 StreamIndex 是分槽的**唯一**依据。
//
// 这一条比上一条更关键：内核对「idx==0 且 name/args 都空」才跳过，
// 而 index=1/2/3 的续传分片正是靠 stream_index 归位。
func TestOpenAIAdapterKeepsContinuationFragment(t *testing.T) {
	vm := NewVM(t.TempDir())
	loadBundled(t, vm, "openai")

	// 续传分片：只有 index + arguments
	const frag = `{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[` +
		`{"index":2,"function":{"arguments":"{\"command\":\"c\"}"}}]}}]}`
	out, err := vm.CallTransformStreamChunk("openai", frag)
	if err != nil {
		t.Fatalf("transform_stream_chunk: %v", err)
	}
	var u unifiedChunk
	if err := json.Unmarshal([]byte(out), &u); err != nil {
		t.Fatalf("输出非法: %v\n%s", err, out)
	}
	if len(u.ToolCalls) != 1 {
		t.Fatalf("续传分片应被保留，实际 %d 个：%s", len(u.ToolCalls), out)
	}
	if got := u.ToolCalls[0].StreamIndex; got != 2 {
		t.Errorf("续传分片的 stream_index = %d，应为 2 —— 它是内核分槽的唯一依据：%s", got, out)
	}
	if u.ToolCalls[0].RawArguments == "" {
		t.Errorf("续传分片的 arguments 丢了：%s", out)
	}
}

// TestAllBundledAdaptersStreamToolCallStatus 给**全部**内嵌适配器做一次体检，
// 并把结果**分类记进测试输出**。
//
// ★ 为什么全绿不等于全支持：
//
//	早期版本对"未产出 tool_calls / 返回空 / 报错"的适配器一律 t.Skip，
//	于是一个**完全不支持流式工具调用**的适配器也会让整体显示为绿 ——
//	而"绿"在这里被误读成"都支持"。压测发现这一点时才回头查。
//
//	现在改成：分三类明确记录（supported / nested-passthrough / unsupported），
//	任何"声称支持但 index 不对"的都判失败。
func TestAllBundledAdaptersStreamToolCallStatus(t *testing.T) {
	type row struct {
		adapters []string
		note     string
	}
	var supported, nested, unsupported []string

	for _, name := range bundledAdapterNames(t) {
		vm := NewVM(t.TempDir())
		loadBundled(t, vm, name)
		out, err := vm.CallTransformStreamChunk(name, openAIMultiToolChunk)
		switch {
		case err != nil || out == "":
			// 协议不同（Anthropic 用 content_block_*、gemini 用 candidates 等）
			unsupported = append(unsupported, name)
			continue
		}
		var u unifiedChunk
		if err := json.Unmarshal([]byte(out), &u); err != nil {
			t.Fatalf("%s 输出非法: %v\n%s", name, err, out)
		}
		if len(u.ToolCalls) == 0 {
			unsupported = append(unsupported, name)
			continue
		}
		// 嵌套透传的形态：字段在 function 里，Go 侧解析不到 → 归为「需另行修」
		if u.ToolCalls[0].RawArguments == "" {
			var probe map[string]interface{}
			_ = json.Unmarshal([]byte(out), &probe)
			tcs, _ := probe["tool_calls"].([]interface{})
			if len(tcs) > 0 {
				if m, ok := tcs[0].(map[string]interface{}); ok {
					if _, hasFn := m["function"]; hasFn {
						nested = append(nested, name)
						continue
					}
				}
			}
		}
		bad := false
		for i, tc := range u.ToolCalls {
			if tc.StreamIndex != i {
				t.Errorf("%s 第 %d 个 tool_call 的 stream_index = %d，应为 %d（缺透传）",
					name, i, tc.StreamIndex, i)
				bad = true
			}
		}
		if !bad {
			supported = append(supported, name)
		}
	}

	t.Logf("流式工具调用支持情况（共 %d 个适配器）", len(supported)+len(nested)+len(unsupported))
	t.Logf("  ✓ 扁平+index 透传正确 : %v", supported)
	t.Logf("  ⚠ 透传嵌套形态需另修   : %v", nested)
	t.Logf("  ✗ 未产出 tool_calls   : %v", unsupported)
}

func loadBundled(t *testing.T, vm *VM, name string) {
	t.Helper()
	b, err := bundledAdapters.ReadFile("adapters/" + name + ".lua")
	if err != nil {
		t.Fatalf("读内嵌适配器 %s 失败: %v", name, err)
	}
	if err := vm.LoadAdapterSource(name, string(b)); err != nil {
		t.Fatalf("加载 %s 失败: %v", name, err)
	}
}

func bundledAdapterNames(t *testing.T) []string {
	t.Helper()
	ents, err := bundledAdapters.ReadDir("adapters")
	if err != nil {
		t.Fatalf("读 adapters 目录失败: %v", err)
	}
	var out []string
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".lua" {
			out = append(out, e.Name()[:len(e.Name())-4])
		}
	}
	return out
}
