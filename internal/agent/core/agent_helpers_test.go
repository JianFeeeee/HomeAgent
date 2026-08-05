package core

import (
	"testing"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
)

func needJieba() bool {
	return memory.GetJieba() != nil
}

func TestDocToTriplesEmpty(t *testing.T) {
	doc := &document.Doc{
		Summary: "empty doc",
		Content: "",
		Source:  "test",
	}
	triples := docToTriples(doc, nil)
	if len(triples) < 2 {
		t.Fatalf("expected at least 2 triples (主题+来源), got %d", len(triples))
	}

	if triples[0].Subject != "文档" || triples[0].Relation != "主题" || triples[0].Object != "empty doc" {
		t.Errorf("first triple mismatch: %+v", triples[0])
	}

	last := triples[len(triples)-1]
	if last.Subject != "文档" || last.Relation != "来源" || last.Object != "test" {
		t.Errorf("last triple mismatch: %+v", last)
	}
}

func TestDocToTriplesConversation(t *testing.T) {
	doc := &document.Doc{
		Summary: "测试对话 (qq) 涉及: 天气",
		Content: "[15:04] qq: 今天天气怎么样\n[15:05] agent: 今天天气很好",
		Source:  "qq",
	}
	triples := docToTriples(doc, nil)

	if len(triples) < 2 {
		t.Errorf("expected at least 2 triples (主题+来源), got %d", len(triples))
	}

	for i, tr := range triples {
		if tr.Subject == "" || tr.Relation == "" || tr.Object == "" {
			t.Errorf("triple[%d] has empty field: %+v", i, tr)
		}
		if tr.Confidence <= 0 {
			t.Errorf("triple[%d] has non-positive confidence: %+v", i, tr)
		}
	}
}

func TestDocToTriplesMultiLine(t *testing.T) {
	doc := &document.Doc{
		Summary: "多轮对话",
		Content: "[10:00] user: 你好\n[10:01] agent: 你好，有什么可以帮助你的\n[10:02] user: 今天天气如何\n[10:03] agent: 今天天气很好",
		Source:  "qq",
	}
	triples := docToTriples(doc, nil)
	if len(triples) < 2 {
		t.Fatalf("expected at least 2 triples, got %d", len(triples))
	}

	if triples[0].Subject != "文档" || triples[0].Relation != "主题" {
		t.Errorf("first triple should be 主题, got %+v", triples[0])
	}
	last := triples[len(triples)-1]
	if last.Subject != "文档" || last.Relation != "来源" {
		t.Errorf("last triple should be 来源, got %+v", last)
	}
}

func TestDocToTriplesEmptyContent(t *testing.T) {
	doc := &document.Doc{
		Summary: "空内容",
		Content: "",
		Source:  "test",
	}
	triples := docToTriples(doc, nil)
	if len(triples) != 2 {
		t.Fatalf("expected exactly 2 triples (主题+来源) for empty content, got %d", len(triples))
	}
}

func TestTruncateStr(t *testing.T) {
	tests := []struct {
		input string
		max   int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello..."},
		{"你好世界", 2, "你好..."},
		{"", 5, ""},
		{"abc", 3, "abc"},
	}
	for _, tt := range tests {
		got := truncateStr(tt.input, tt.max)
		if got != tt.want {
			t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
		}
	}
}

func TestGetString(t *testing.T) {
	m := map[string]interface{}{
		"name": "张三",
		"age":  30,
	}

	if got := getString(m, "name"); got != "张三" {
		t.Errorf("expected '张三', got %q", got)
	}
	if got := getString(m, "age"); got != "" {
		t.Errorf("expected empty for int, got %q", got)
	}
	if got := getString(m, "nonexistent"); got != "" {
		t.Errorf("expected empty for missing key, got %q", got)
	}
	if got := getString(nil, "key"); got != "" {
		t.Errorf("expected empty for nil map, got %q", got)
	}
}

func TestGetFloat(t *testing.T) {
	m := map[string]interface{}{
		"count": 42.5,
		"score": 100,
		"name":  "test",
	}

	if got := getFloat(m, "count"); got != 42.5 {
		t.Errorf("expected 42.5, got %f", got)
	}
	if got := getFloat(m, "score"); got != 100.0 {
		t.Errorf("expected 100.0, got %f", got)
	}
	if got := getFloat(m, "name"); got != 0 {
		t.Errorf("expected 0 for string, got %f", got)
	}
	if got := getFloat(m, "nonexistent"); got != 0 {
		t.Errorf("expected 0 for missing key, got %f", got)
	}
	if got := getFloat(nil, "key"); got != 0 {
		t.Errorf("expected 0 for nil map, got %f", got)
	}
}

func TestGetFloatInt(t *testing.T) {
	m := map[string]interface{}{
		"top_k": float64(5),
	}
	if got := getFloat(m, "top_k"); got != 5.0 {
		t.Errorf("expected 5.0, got %f", got)
	}
}
