package api

import "testing"

// 缓存命中与推理 token 的端到端透传。
//
// 为什么需要这组判据：这三个字段**早就被解析过又被丢掉** ——
// `chunkUsage` 里声明了 `prompt_cache_hit_tokens` /
// `prompt_cache_miss_tokens` / `prompt_tokens_details.cached_tokens`，
// 但 `chunkAssemble` 只把 prompt/completion/total 拷进 `TokenUsage`，
// 于是「解析了却不返回」。这是本项目反复出现的缺陷形态：
// 算了却不返回 = 没算，而它**不会让任何东西报错**，只是答案永远缺席。
//
// 参考实现：llmsproxy 的 `internal/types/types.go` 与
// `internal/gateway/chat.go:recordChatUsage` —— 那边已经把
// 「OpenAI v2 的 prompt_tokens_details」与「DeepSeek 遗留的
// prompt_cache_hit_tokens」两条来源归一化好了，且用 `CacheReported`
// 区分「上游报了缓存但 0 命中」与「上游没报缓存」。
// 这个区分很重要：前者应显示 0%，后者应显示「—」，混起来会撒谎。

// TestCacheUsage_OpenAIV2Details 覆盖 OpenAI v2 形态：
// usage.prompt_tokens_details.cached_tokens。
func TestCacheUsage_OpenAIV2Details(t *testing.T) {
	frame := `{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,` +
		`"prompt_tokens_details":{"cached_tokens":768}}}`

	ck, ok := parseOpenAICompatibleStreamChunkFullGo(frame)
	if !ok {
		t.Fatal("解析失败，期望成功")
	}
	if ck.Usage == nil {
		t.Fatal("Usage 为 nil，期望有用量")
	}
	if got := ck.Usage.Prompt; got != 1000 {
		t.Errorf("Prompt = %d，期望 1000", got)
	}
	if got := ck.Usage.CacheRead; got != 768 {
		t.Errorf("CacheRead = %d，期望 768（prompt_tokens_details.cached_tokens）", got)
	}
	if !ck.Usage.CacheReported {
		t.Error("CacheReported = false，期望 true（上游报了缓存细节）")
	}
}

// TestCacheUsage_DeepSeekLegacy 覆盖 DeepSeek 遗留的独立字段。
// 上游可能只给这一种，不给 prompt_tokens_details。
func TestCacheUsage_DeepSeekLegacy(t *testing.T) {
	frame := `{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":2048,"completion_tokens":10,"total_tokens":2058,` +
		`"prompt_cache_hit_tokens":1920,"prompt_cache_miss_tokens":128}}`

	ck, ok := parseOpenAICompatibleStreamChunkFullGo(frame)
	if !ok || ck.Usage == nil {
		t.Fatal("解析失败或 Usage 为 nil")
	}
	if got := ck.Usage.CacheRead; got != 1920 {
		t.Errorf("CacheRead = %d，期望 1920（prompt_cache_hit_tokens）", got)
	}
	if got := ck.Usage.CacheMiss; got != 128 {
		t.Errorf("CacheMiss = %d，期望 128（prompt_cache_miss_tokens）", got)
	}
	if !ck.Usage.CacheReported {
		t.Error("CacheReported = false，期望 true")
	}
}

// TestCacheUsage_ReportedZeroDistinctFromAbsent 是这组判据里最重要的一条：
// **「上游报了缓存但 0 命中」必须与「上游根本没报缓存」区分开**。
//
// 混为一谈的后果是撒谎：把「无数据」显示成 0% 命中率，
// 会让人误以为缓存完全失效而去优化一个本来就没开的功能。
func TestCacheUsage_ReportedZeroDistinctFromAbsent(t *testing.T) {
	// ① 上游明确报了 cached_tokens = 0
	reportedZero := `{"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":500,"completion_tokens":1,"total_tokens":501,` +
		`"prompt_tokens_details":{"cached_tokens":0}}}`
	ck, _ := parseOpenAICompatibleStreamChunkFullGo(reportedZero)
	if ck.Usage == nil {
		t.Fatal("① Usage 为 nil")
	}
	if ck.Usage.CacheRead != 0 {
		t.Errorf("① CacheRead = %d，期望 0", ck.Usage.CacheRead)
	}
	if !ck.Usage.CacheReported {
		t.Error("① CacheReported = false —— 上游报了缓存细节（值为 0），必须为 true")
	}

	// ② 上游完全没提缓存
	absent := `{"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":500,"completion_tokens":1,"total_tokens":501}}`
	ck2, _ := parseOpenAICompatibleStreamChunkFullGo(absent)
	if ck2.Usage == nil {
		t.Fatal("② Usage 为 nil")
	}
	if ck2.Usage.CacheReported {
		t.Error("② CacheReported = true —— 上游没提缓存，应为 false")
	}
}

// TestCacheUsage_ReasoningTokens 覆盖 OpenAI v2 的
// completion_tokens_details.reasoning_tokens。
//
// 为什么需要：它回答「计费的输出里有多少是思考而非答案」。
// 缺了它，成本归因会把思考 token 算进「回答长度」，
// 于是长思考被误读成啰嗦。
func TestCacheUsage_ReasoningTokens(t *testing.T) {
	frame := `{"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":900,"total_tokens":1000,` +
		`"completion_tokens_details":{"reasoning_tokens":850}}}`

	ck, _ := parseOpenAICompatibleStreamChunkFullGo(frame)
	if ck.Usage == nil {
		t.Fatal("Usage 为 nil")
	}
	if got := ck.Usage.ReasoningTokens; got != 850 {
		t.Errorf("ReasoningTokens = %d，期望 850", got)
	}
}

// TestCacheUsage_NoUsageBlockStaysNil 守住既有语义：
// 整帧没有 usage 时 `Usage` 必须是 nil，不能因为新增字段就变成
// 一个「全零但非 nil」的结构 —— 那会让「无用量」被当成「用量为 0」。
func TestCacheUsage_NoUsageBlockStaysNil(t *testing.T) {
	frame := `{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`
	ck, ok := parseOpenAICompatibleStreamChunkFullGo(frame)
	if !ok {
		t.Fatal("解析失败，期望成功（有内容块）")
	}
	if ck.Usage != nil {
		t.Errorf("Usage = %+v，期望 nil（该帧没有 usage）", *ck.Usage)
	}
}
