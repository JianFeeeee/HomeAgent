package core

import (
	"testing"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
)

func TestEntitySimilarity(t *testing.T) {
	tests := []struct {
		a, b string
		want float64
	}{
		{"", "", 0},         // empty → 0
		{"a", "b", 0},       // single char → 0
		{"张三", "张三", 1.0}, // identical → 1.0
		{"张三", "李四", 0},  // no common bigrams
		{"iPhone", "iPhone 15", 0.625}, // partial overlap
	}
	for _, tt := range tests {
		got := entitySimilarity(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("entitySimilarity(%q, %q) = %.3f, want %.3f", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestDocToTriples(t *testing.T) {
	doc := &document.Doc{
		Summary:  "用户喜欢编程",
		Content:  "用户提到喜欢Go和Python",
		Tags:     []string{"编程", "Go"},
		Entities: []string{"Go", "Python"},
		Source:   "context",
	}

	triples := docToTriples(doc)
	if len(triples) == 0 {
		t.Fatal("expected non-empty triples")
	}

	foundSummary := false
	foundEntity := false
	foundTag := false
	foundSource := false

	for _, tr := range triples {
		if tr.Subject == "文档" && tr.Relation == "包含内容" {
			foundSummary = true
		}
		if tr.Subject == "文档" && tr.Relation == "提及实体" {
			foundEntity = true
		}
		if tr.Subject == "文档" && tr.Relation == "标签" {
			foundTag = true
		}
		if tr.Subject == "文档" && tr.Relation == "来源" {
			foundSource = true
		}
	}

	if !foundSummary {
		t.Error("missing '包含内容' triple")
	}
	if !foundEntity {
		t.Error("missing '提及实体' triple")
	}
	if !foundTag {
		t.Error("missing '标签' triple")
	}
	if !foundSource {
		t.Error("missing '来源' triple")
	}
}

func TestDocToTriplesNil(t *testing.T) {
	triples := docToTriples(nil)
	if len(triples) != 0 {
		t.Errorf("expected empty for nil doc, got %d", len(triples))
	}
}

func TestDocToTriplesNoSource(t *testing.T) {
	doc := &document.Doc{
		Summary: "无来源文档",
		Content: "content",
	}
	triples := docToTriples(doc)
	for _, tr := range triples {
		if tr.Relation == "来源" {
			t.Error("should not have source triple when Source is empty")
		}
	}
}

func TestDocToTriplesTypes(t *testing.T) {
	doc := &document.Doc{
		Summary:  "测试三元组类型",
		Content:  "用于验证 SubjectType 和 ObjectType",
		Entities: []string{"Go"},
	}

	triples := docToTriples(doc)
	for _, tr := range triples {
		if tr.Subject != "文档" {
			t.Errorf("expected subject '文档', got %q", tr.Subject)
		}
	}
}
