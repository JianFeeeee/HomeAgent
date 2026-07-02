package core

import (
	"testing"
	"time"
)

func TestContextAppendAndLen(t *testing.T) {
	ctx := NewRelevanceContext()
	if ctx.Len() != 0 {
		t.Errorf("new context should be empty, got %d", ctx.Len())
	}

	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "hello"})
	if ctx.Len() != 1 {
		t.Errorf("expected len 1, got %d", ctx.Len())
	}
}

func TestContextRecent(t *testing.T) {
	ctx := NewRelevanceContext()
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "a"})
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "b"})
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "c"})

	recent := ctx.Recent(2)
	if len(recent) != 2 {
		t.Errorf("expected 2 recent, got %d", len(recent))
	}
	if recent[0].Input != "b" || recent[1].Input != "c" {
		t.Errorf("expected [b, c], got %v", recent)
	}
}

func TestContextFormat(t *testing.T) {
	ctx := NewRelevanceContext()
	f := ctx.Format()
	if f != "" {
		t.Errorf("empty context should format to empty string, got %q", f)
	}

	now := time.Now()
	ctx.Append(ContextEvent{Timestamp: now, Source: "user", Input: "hello"})
	f = ctx.Format()
	if f == "" {
		t.Fatal("non-empty context should produce non-empty format")
	}
	if !contains(f, "hello") {
		t.Errorf("format should contain input 'hello', got: %s", f)
	}
	if !contains(f, "user") {
		t.Errorf("format should contain source 'user'")
	}
}

func TestContextPruneKeepsTopK(t *testing.T) {
	ctx := NewRelevanceContext()
	for i := 0; i < 10; i++ {
		ctx.Append(ContextEvent{
			Timestamp: time.Now(),
			Source:    "user",
			Input:     "今天天气很好",
			Response:  "是的天气不错",
		})
	}
	// 加一条不同主题的
	ctx.Append(ContextEvent{
		Timestamp: time.Now(),
		Source:    "user",
		Input:     "帮我算一下微积分题目",
		Response:  "好的我来算",
	})

	archived := ctx.Prune("微积分", 5, nil) // nil docStore → 不归档，只裁剪
	_ = archived

	if ctx.Len() > 5 {
		t.Errorf("after prune to 5, len should be ≤5, got %d", ctx.Len())
	}
}

func TestContextPruneWithDocStore(t *testing.T) {
	ctx := NewRelevanceContext()
	for i := 0; i < 15; i++ {
		ctx.Append(ContextEvent{
			Timestamp: time.Now(),
			Source:    "user",
			Input:     "今天天气很好",
			Response:  "是的",
		})
	}

	archived := ctx.Prune("天气", 10, nil)
	if archived != 0 {
		t.Errorf("with nil docStore, archived should be 0, got %d", archived)
	}
}

func TestContextAppendAfterPrune(t *testing.T) {
	ctx := NewRelevanceContext()
	for i := 0; i < 10; i++ {
		ctx.Append(ContextEvent{
			Timestamp: time.Now(),
			Source:    "user",
			Input:     "hello world",
		})
	}

	ctx.Prune("hello", 3, nil)
	if ctx.Len() > 3 {
		t.Errorf("expected ≤3 after prune, got %d", ctx.Len())
	}

	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "new message"})
	if ctx.Len() != 4 {
		t.Errorf("after append, expected 4, got %d", ctx.Len())
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
