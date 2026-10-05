package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// 这组判据守的是**无头消费方**（waiter -chat、基准测试 runner、脚本）
// 的唯一信息出口：chat 的 response 帧。
//
// 背景（与本轮修的 usage 缺陷同族）：agent 在每次 LLM 调用后记账，
// emitResponse 也把 usage 放进了同步回执；但 cli 的 handleChat 此前
// 只从回执里取 content 就往帧里写 —— usage 被丢掉。
// 后果是**有头 UI 看得到成本、无头调用方看不到**，而跑分恰恰全靠无头。
//
// 症状形态与链事件那次一模一样：写了、发了、没有任何报错，只是没人收到。

// TestChatResponseFrameCarriesUsage 是核心判据：
// 回执里有 usage 时，帧里必须原样带上（且类型不被压扁）。
func TestChatResponseFrameCarriesUsage(t *testing.T) {
	usage := map[string]interface{}{
		"prompt_tokens":     1000,
		"completion_tokens": 200,
		"total_tokens":      1200,
		"cache_read_tokens": 768,
		"cache_miss_tokens": 232,
	}
	frame := chatResponseFrame(map[string]interface{}{
		"content": "好的",
		"usage":   usage,
	})

	if frame["type"] != "response" {
		t.Errorf("type=%v，期望 response", frame["type"])
	}
	if frame["content"] != "好的" {
		t.Errorf("content=%v，期望「好的」", frame["content"])
	}
	got, ok := frame["usage"]
	if !ok || got == nil {
		t.Fatalf("帧里没有 usage —— 无头调用方看不到成本；frame=%v", frame)
	}
	// 必须是 map[string]interface{}：消费方（runner）按 JSON 读取，
	// 若这里被压成别的类型，序列化后会变形或丢失。
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("usage 类型=%T，期望 map[string]interface{}", got)
	}
	if m["total_tokens"] != 1200 {
		t.Errorf("total_tokens=%v，期望 1200", m["total_tokens"])
	}
	if m["cache_read_tokens"] != 768 {
		t.Errorf("cache_read_tokens=%v，期望 768", m["cache_read_tokens"])
	}
}

// TestChatResponseFrameOmitsUsageWhenAbsent 守住「无数据 ≠ 0」：
// 回执里没有 usage 时，帧里也不该出现这个键。
//
// 若这里补一个零值 usage，消费方就无法区分「没开缓存」与「命中率为 0」，
// 会把「不知道」画成 0% —— 与本项目 CacheHitRate 的 ok 口径一致。
func TestChatResponseFrameOmitsUsageWhenAbsent(t *testing.T) {
	frame := chatResponseFrame(map[string]interface{}{"content": "你好"})
	if u, ok := frame["usage"]; ok {
		t.Errorf("回执没有 usage 时不该凭空造：%v", u)
	}
	// 序列化后也必须真的没有这个键。
	b, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "usage") {
		t.Errorf("JSON 里不该出现 usage：%s", b)
	}
	// nil 值同样要当成「没有」（Go 里 map 取值存在但为 nil 是常见形态）。
	frame2 := chatResponseFrame(map[string]interface{}{"content": "x", "usage": nil})
	if u, ok := frame2["usage"]; ok {
		t.Errorf("usage 为 nil 时不该带上：%v", u)
	}
}

// TestChatResponseFrameCarriesReasoning 保住既有能力：
// reasoning_content 也要透传（此前 handleChat 也没带它，
// 无头调用方因此看不到思考过程）。
func TestChatResponseFrameCarriesReasoning(t *testing.T) {
	frame := chatResponseFrame(map[string]interface{}{
		"content":           "答案",
		"reasoning_content": "先想一下",
	})
	if frame["reasoning_content"] != "先想一下" {
		t.Errorf("reasoning_content=%v，期望透传", frame["reasoning_content"])
	}
	// 空思考不该出现（避免给每个普通回复都挂一个空字段）。
	frame2 := chatResponseFrame(map[string]interface{}{"content": "x"})
	if _, ok := frame2["reasoning_content"]; ok {
		t.Error("空 reasoning_content 不该出现在帧里")
	}
}
