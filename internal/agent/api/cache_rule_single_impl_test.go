package api

import (
	"encoding/json"
	"testing"
)

// 这组判据守的是「缓存规则只有一个实现」这条**结构约束**。
//
// ── 背景：用户问「你是不是搓了两套解决同一个问题的逻辑？」──
//
// 是的。修「无法统计缓存命中」时（commit 73a6359）同一份规则被写了两遍：
//
//	Lua 侧  usage_to_unified    × 10 个适配器文件各一份
//	Go  侧  chunkAssemble       × 1
//
// 两边都判「上游报了缓存」、都从 cached_tokens 取命中数，而**都没算未命中数**。
// 后果在 2026-09-30 跑分时暴露：llmsproxy+AUTO 的 7 个任务报出
// **命中率恒 100%**（因为分母只剩 read），真实值约 53%。
//
// 修法是（用户选定方案 A）：规则的**实现**收成一处 ——
// `TokenUsage.DeriveCacheMiss()` —— 两条路各自调用**同一个函数**；
// Lua 只负责搬上游给的字段，不再自己算。
//
// 本文件把这些约束变成可执行的判据，否则下次有人图方便在任一侧写回去，
// 没有任何东西拦得住。

// TestDeriveCacheMissOnAdapterShapedUsage 是**生产路径**的判据。
//
// 生产是 `adapter=openai`，Lua 的输出会被 json.Unmarshal 进 StreamChunk。
// OpenAI v2 只给命中侧（prompt_tokens_details.cached_tokens），
// 所以适配器输出里 cache_miss 缺席 —— 这正是「恒 100%」的输入形态。
// 这里直接构造该形态，断言 DeriveCacheMiss 补出未命中数。
func TestDeriveCacheMissOnAdapterShapedUsage(t *testing.T) {
	// 这是 openai.lua 对 usage={"prompt_tokens":1000,"completion_tokens":50,
	// "total_tokens":1050,"prompt_tokens_details":{"cached_tokens":768}}
	// 的实际输出形态（字段名对齐 agentAPI.TokenUsage 的 json tag）。
	adapterOut := `{"content":"hi","usage":{"prompt":1000,"completion":50,` +
		`"total":1050,"cache_read":768,"cache_reported":true}}`

	var ck StreamChunk
	if err := json.Unmarshal([]byte(adapterOut), &ck); err != nil {
		t.Fatalf("unmarshal 适配器输出失败: %v", err)
	}
	if ck.Usage == nil {
		t.Fatal("适配器输出里的 usage 没解析出来")
	}
	// 未补之前：miss=0 ⇒ 命中率分母只剩 read ⇒ 必然 100%（假绿）
	if ck.Usage.CacheMiss != 0 {
		t.Fatalf("前提不成立：适配器本不该给 cache_miss，实际 %d", ck.Usage.CacheMiss)
	}
	ck.Usage.DeriveCacheMiss()
	if got := ck.Usage.CacheMiss; got != 232 {
		t.Errorf("CacheMiss=%d，期望 232（= prompt 1000 - read 768）；"+
			"留 0 会让命中率恒等于 100%%", got)
	}
}

// TestParseStreamAndNonStreamAgreeOnUsage 是**防漂移**判据。
//
// 同一份 usage payload 分别走流式解析与非流式解析，两边的 TokenUsage
// 必须逐字段相同。它们此前各写了一份字段清单与缓存判断 ——
// 那正是「两套逻辑」的所在，也正是漂移会发生的地方。
func TestParseStreamAndNonStreamAgreeOnUsage(t *testing.T) {
	usage := `"usage":{"prompt_tokens":1000,"completion_tokens":50,` +
		`"total_tokens":1050,"prompt_tokens_details":{"cached_tokens":768}}`

	streamBody := `{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}],` + usage + `}`
	ck, ok := parseOpenAICompatibleStreamChunkFull(streamBody)
	if !ok || ck.Usage == nil {
		t.Fatal("流式解析失败或 Usage 为 nil")
	}

	nonStreamBody := `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],` + usage + `}`
	resp, err := parseOpenAICompatibleResponse([]byte(nonStreamBody))
	if err != nil {
		t.Fatalf("非流式解析失败: %v", err)
	}

	// 逐字段对比：任何一侧漏字段/漏补齐，这里就红。
	if ck.Usage.Prompt != resp.TokenUsage.Prompt {
		t.Errorf("prompt 不一致：流式 %d vs 非流式 %d",
			ck.Usage.Prompt, resp.TokenUsage.Prompt)
	}
	if ck.Usage.Completion != resp.TokenUsage.Completion {
		t.Errorf("completion 不一致：流式 %d vs 非流式 %d",
			ck.Usage.Completion, resp.TokenUsage.Completion)
	}
	if ck.Usage.Total != resp.TokenUsage.Total {
		t.Errorf("total 不一致：流式 %d vs 非流式 %d",
			ck.Usage.Total, resp.TokenUsage.Total)
	}
	if ck.Usage.CacheRead != resp.TokenUsage.CacheRead {
		t.Errorf("cache_read 不一致：流式 %d vs 非流式 %d",
			ck.Usage.CacheRead, resp.TokenUsage.CacheRead)
	}
	if ck.Usage.CacheMiss != resp.TokenUsage.CacheMiss {
		t.Errorf("cache_miss 不一致：流式 %d vs 非流式 %d（两边必须同一份规则）",
			ck.Usage.CacheMiss, resp.TokenUsage.CacheMiss)
	}
	if ck.Usage.CacheReported != resp.TokenUsage.CacheReported {
		t.Errorf("cache_reported 不一致：流式 %v vs 非流式 %v",
			ck.Usage.CacheReported, resp.TokenUsage.CacheReported)
	}
	// 顺带钉住具体值，避免"两边都错成一样"也算过。
	if resp.TokenUsage.CacheMiss != 232 {
		t.Errorf("CacheMiss=%d，期望 232", resp.TokenUsage.CacheMiss)
	}
}

// TestDeriveCacheMissRespectsExplicitUpstreamMiss 守住「上游明说的优先」。
//
// DeepSeek 遗留字段会**同时**给命中与未命中（prompt_cache_hit_tokens /
// prompt_cache_miss_tokens）。那种情况下我们一个数都不该改 ——
// 推导值只是上游没给时的兜底，不能覆盖上游的权威值。
func TestDeriveCacheMissRespectsExplicitUpstreamMiss(t *testing.T) {
	body := `{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":2048,"completion_tokens":10,"total_tokens":2058,` +
		`"prompt_cache_hit_tokens":1920,"prompt_cache_miss_tokens":128}}`
	ck, ok := parseOpenAICompatibleStreamChunkFull(body)
	if !ok || ck.Usage == nil {
		t.Fatal("解析失败")
	}
	if got := ck.Usage.CacheMiss; got != 128 {
		t.Errorf("CacheMiss=%d，期望 128（上游明说的值）；"+
			"按 prompt-read 推出来也是 128 但那是巧合，不能说推的覆盖明说的", got)
	}

	// 直接构造一个"上游给的 miss 与推导不同"的输入，确认推导让位。
	u := TokenUsage{Prompt: 1000, CacheRead: 400, CacheMiss: 111, CacheReported: true}
	u.DeriveCacheMiss()
	if u.CacheMiss != 111 {
		t.Errorf("CacheMiss=%d，期望保持上游的 111（推导不得覆盖）", u.CacheMiss)
	}
}

// TestDeriveCacheMissNoOpWhenCacheNotReported 守住「无数据 ≠ 0」。
//
// 上游没报缓存时必须原样不动，让消费方显示「—」。
// 若这里也补出 miss，就会把「不知道」变成 0% 命中率 ——
// 让人去优化一个本来没开的功能。
func TestDeriveCacheMissNoOpWhenCacheNotReported(t *testing.T) {
	u := TokenUsage{Prompt: 1000, Completion: 50, Total: 1050}
	u.DeriveCacheMiss()
	if u.CacheMiss != 0 {
		t.Errorf("未报缓存时不该补 miss，实际 %d", u.CacheMiss)
	}
	if u.CacheReported {
		t.Error("不该把 CacheReported 置真")
	}
}

// TestDeriveCacheMissClampsDirtyUpstream 守住脏数据不产生负值。
//
// 上游偶尔给「命中数 > 输入总数」的脏数据。若不夹住，miss 会变负数，
// 命中率分母跟着缩小甚至变负 —— 比 100% 假绿更离谱。
func TestDeriveCacheMissClampsDirtyUpstream(t *testing.T) {
	u := TokenUsage{Prompt: 100, CacheRead: 400, CacheReported: true}
	u.DeriveCacheMiss()
	if u.CacheMiss != 0 {
		t.Errorf("CacheMiss=%d，期望夹到 0（命中数 400 > 输入 100 是脏数据）", u.CacheMiss)
	}
}
