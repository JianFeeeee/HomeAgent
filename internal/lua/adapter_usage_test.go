package lua

import (
	"encoding/json"
	"testing"
)

// 适配器必须把**上游给的用量**透传出来。
//
// ── 为什么需要这组判据 ──
//
// 用户的描述是准确的：「Lua 适配器透传上游提供的用量，内核是估算」。
// 设计意图确实如此。但实现没做到 —— 流式路径上适配器**一个字节的用量都不透传**：
//
//	anthropic.lua  transform_stream_chunk
//	  message_start  → return ""        ← input_tokens 与 cache_read_input_tokens 就在这帧
//	  message_delta  → 返回不带 usage    ← output_tokens 就在这帧
//	openai.lua     transform_stream_chunk
//	  整段从不读 chunk.usage
//
// 后果：生产流式请求的 `StreamChunk.Usage` 恒为 nil ⇒ 缓存命中率与
// token 消耗**根本无从统计**，内核只剩估算一条路。
//
// ── 为什么以前没被发现 ──
//
// 没有任何判据断言过「适配器输出里有 usage」。既有测试全部只查 content /
// tool_calls / stream_index —— 用量字段从来没进过任何 fixture。
// 这与 stream_index 那次是同一形态：内核侧写好了、判据也加了，
// 唯独**透传那一步从未落地**，而它静默（缺字段不报错，只是永远取零值）。
//
// ── 契约 ──
//
// 任何**上游 payload 里带 usage 对象**的帧，适配器输出必须带出该 usage。
// 键名对齐 Go 侧 agentAPI.TokenUsage 的 json tag
// （prompt / completion / total / cache_read / cache_miss / cache_reported /
//  reasoning_tokens），因为该输出会被 json.Unmarshal 进 StreamChunk。

// usageView 是断言用的宽松视图：只关心有没有、值对不对。
type usageView struct {
	Usage *struct {
		Prompt        int  `json:"prompt"`
		Completion    int  `json:"completion"`
		Total         int  `json:"total"`
		CacheRead     int  `json:"cache_read"`
		CacheMiss     int  `json:"cache_miss"`
		CacheReported bool `json:"cache_reported"`
		Reasoning     int  `json:"reasoning_tokens"`
	} `json:"usage"`
}

func decodeUsage(t *testing.T, out string) usageView {
	t.Helper()
	if out == "" {
		return usageView{}
	}
	var v usageView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("适配器输出不是合法 JSON: %s（原文 %q）", err, out)
	}
	return v
}

// TestAnthropicAdapterPassesInputCacheUsage 覆盖 Anthropic 的 message_start。
//
// 这是**最贵的一帧**：prompt caching 的收益全在这里 ——
// input_tokens 是总输入，cache_read_input_tokens 是其中命中缓存的部分。
// 少了它，Anthropic 用户永远看不到自己省了多少。
func TestAnthropicAdapterPassesInputCacheUsage(t *testing.T) {
	vm := NewVM(t.TempDir())
	loadBundled(t, vm, "anthropic")

	start := `{"type":"message_start","message":{"id":"msg_1","usage":{` +
		`"input_tokens":1000,"output_tokens":1,` +
		`"cache_creation_input_tokens":128,` +
		`"cache_read_input_tokens":768}}}`

	out, err := vm.CallTransformStreamChunk("anthropic", start)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	v := decodeUsage(t, out)
	if v.Usage == nil {
		t.Fatalf("message_start 的输出没有 usage —— 这一帧携带 input_tokens 与 "+
			"cache_read_input_tokens，丢掉就再也拿不回来了。输出: %q", out)
	}
	if v.Usage.Prompt != 1896 {
		// ★ 为何是 1896 而不是 1000 —— 这是**跨厂商口径差异**，很容易搞错：
		//
		//   OpenAI : prompt_tokens **包含**缓存命中部分
		//   Anthropic: input_tokens **不包含**；
		//              真实总输入 = input + cache_read + cache_creation
		//
		// 本系统的 TokenUsage.Prompt 统一按 OpenAI 口径（否则同一个字段
		// 在不同后端下含义不同，统计就无法横比）。所以这里是
		// 1000(input) + 768(cache_read) + 128(cache_creation) = 1896。
		//
		// 若直接搬 input_tokens，会**少算缓存那部分** —— 而它通常是最
		// 大的一块，于是“输入很短”的错觉会让缓存收益看起来不存在。
		t.Errorf("prompt = %d，期望 1896（input 1000 + cache_read 768 + "+
			"cache_creation 128，按 OpenAI 口径含缓存）", v.Usage.Prompt)
	}
	if v.Usage.CacheRead != 768 {
		t.Errorf("cache_read = %d，期望 768（cache_read_input_tokens）", v.Usage.CacheRead)
	}
	if v.Usage.CacheMiss != 128 {
		// 缓存**写入**算未命中侧的开销（要花钱但不算 hit）。
		// 命中率分母若把它算成 hit，命中率会被写缓存抬高。
		t.Errorf("cache_miss = %d，期望 128（cache_creation_input_tokens 归入未命中侧）",
			v.Usage.CacheMiss)
	}
	if !v.Usage.CacheReported {
		t.Error("cache_reported = false，期望 true —— 上游明确报了缓存字段，" +
			"必须与「没报缓存」区分开，否则 0 命中会被显示成「无数据」")
	}
}

// TestAnthropicAdapterPassesOutputUsage 覆盖 Anthropic 的 message_delta。
// output_tokens 只在这一帧，而它返回的是非空 unified ⇒ Go 侧会**采用适配器结果**、
// 不再走回退解析 ⇒ 适配器不透传就彻底没有。
func TestAnthropicAdapterPassesOutputUsage(t *testing.T) {
	vm := NewVM(t.TempDir())
	loadBundled(t, vm, "anthropic")

	delta := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},` +
		`"usage":{"output_tokens":250}}`

	out, err := vm.CallTransformStreamChunk("anthropic", delta)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if out == "" {
		t.Fatal("message_delta 输出为空 —— 但该帧同时携带 stop_reason 与 usage，" +
			"不能整帧丢弃")
	}
	v := decodeUsage(t, out)
	if v.Usage == nil {
		t.Fatalf("message_delta 的输出没有 usage（output_tokens=250 丢失）。输出: %q", out)
	}
	if v.Usage.Completion != 250 {
		t.Errorf("completion = %d，期望 250（output_tokens）", v.Usage.Completion)
	}
}

// TestOpenAIAdapterPassesUsageOnContentFrame 覆盖「内容帧同时带 usage」。
//
// 为什么单列一条：OpenAI 的**纯 usage 心跳帧**（choices 为空）适配器会返回 ""，
// 此时 Go 侧回退到标准解析，能接住用量 —— 所以那条路是通的（但属隐式依赖）。
// 而**内容帧带 usage** 时适配器返回非空 unified，Go 侧就采用适配器结果了，
// 用量只能由适配器透传。
func TestOpenAIAdapterPassesUsageOnContentFrame(t *testing.T) {
	vm := NewVM(t.TempDir())
	loadBundled(t, vm, "openai")

	frame := `{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":900,"completion_tokens":40,"total_tokens":940,` +
		`"prompt_tokens_details":{"cached_tokens":640}}}`

	out, err := vm.CallTransformStreamChunk("openai", frame)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	v := decodeUsage(t, out)
	if v.Usage == nil {
		t.Fatalf("内容帧携带 usage，但适配器输出里没有。输出: %q", out)
	}
	if v.Usage.Prompt != 900 {
		t.Errorf("prompt = %d，期望 900", v.Usage.Prompt)
	}
	if v.Usage.CacheRead != 640 {
		t.Errorf("cache_read = %d，期望 640（prompt_tokens_details.cached_tokens）",
			v.Usage.CacheRead)
	}
}

// TestOpenAIAdapterPassesUsageOnlyHeartbeat 覆盖纯 usage 心跳帧。
//
// 上游开 include_usage 时，最后一帧是 `{"choices":[],"usage":{...}}`。
// 适配器可以返回 ""（让 Go 回退解析）**或**自己透传，但**不能两者都丢** ——
// 本判据接受返回 ""（回退路径已由 api 包判据覆盖），只禁止
// 「返回了非空内容却不带 usage」这种把数据吞掉的情形。
func TestOpenAIAdapterPassesUsageOnlyHeartbeat(t *testing.T) {
	vm := NewVM(t.TempDir())
	loadBundled(t, vm, "openai")

	frame := `{"choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":0,` +
		`"total_tokens":1200,"prompt_tokens_details":{"cached_tokens":1024}}}`

	out, err := vm.CallTransformStreamChunk("openai", frame)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if out == "" {
		// 允许：Go 侧会回退到标准解析（api 包的
		// parseOpenAICompatibleStreamChunkFull 已覆盖该形态）。
		return
	}
	v := decodeUsage(t, out)
	if v.Usage == nil {
		t.Fatalf("适配器返回了非空内容却不带 usage ⇒ 用量被吞掉。输出: %q", out)
	}
	if v.Usage.CacheRead != 1024 {
		t.Errorf("cache_read = %d，期望 1024", v.Usage.CacheRead)
	}
}
