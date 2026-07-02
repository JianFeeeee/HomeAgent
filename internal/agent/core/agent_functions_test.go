package core

import (
	"testing"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
)

func TestIsSimilarName(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"张三", "张三四", false},
		{"", "", false},
		{"a", "b", false},
		{"张三", "李四", false},
		{"张三", "张三", false},
	}
	for _, tt := range tests {
		got := isSimilarName(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("isSimilarName(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
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
