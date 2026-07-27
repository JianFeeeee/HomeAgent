package nlp

import (
	"testing"
)

func TestFallbackParseDebug(t *testing.T) {
	cases := []string{"我打球", "我在杭州读书", "小明喜欢吃苹果", "天气很好", "我住在杭州"}
	p := newFallbackParser()
	for _, c := range cases {
		result, err := p.Parse(c)
		if err != nil || result == nil || len(result.Tokens) == 0 {
			t.Skip("jieba not available")
		}
		t.Logf("%q → tokens=%v pos=%v", c, result.Tokens, result.POS)
	}
}

func TestExtractFromPOS(t *testing.T) {
	p := newFallbackParser()

	tests := []struct {
		name  string
		input string
	}{
		{"pronoun_prep_ns_noun", "我在杭州读书"},
		{"pronoun_verb_noun", "我打球"},
		{"name_verb_noun", "小明喜欢吃苹果"},
		{"adj_predicate", "天气很好"},
		{"pronoun_verb_prep_ns", "我住在杭州"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.input == "" {
				result, _ := p.Parse("")
				triples := extractFromPOS(result)
				if len(triples) != 0 {
					t.Errorf("expected 0 triples for empty, got %d", len(triples))
				}
				return
			}

			result, err := p.Parse(tt.input)
			if err != nil || result == nil || len(result.Tokens) == 0 {
				t.Skip("jieba not available")
			}

			t.Logf("input=%q tokens=%v pos=%v", tt.input, result.Tokens, result.POS)
			triples := extractFromPOS(result)

			for _, tr := range triples {
				if tr.Subject == "" || tr.Relation == "" || tr.Object == "" {
					t.Errorf("triple has empty field: %+v", tr)
				}
				t.Logf("triple: Subject=%q Relation=%q Object=%q score=%.2f", tr.Subject, tr.Relation, tr.Object, tr.Score)
			}

			if len(triples) == 0 {
				t.Logf("no triples extracted (may be expected depending on jieba POS tagging)")
			}
		})
	}
}

func TestExtractorFallback(t *testing.T) {
	e := NewExtractor(nil)
	result := e.Extract("我住在杭州")
	if result == nil {
		t.Fatal("expected result")
	}
	if result.Src == "" {
		t.Skip("jieba not available")
	}
	if len(result.Triples) > 0 {
		tr := result.Triples[0]
		t.Logf("extracted: Subject=%q Relation=%q Object=%q (score=%.2f, src=%s)",
			tr.Subject, tr.Relation, tr.Object, tr.Score, tr.Src)
	}
}

func TestExtractorWithDepStub(t *testing.T) {
	dummy := &dummyParser{}
	e := NewExtractor(dummy)
	result := e.Extract("我今天去北京")
	if result == nil {
		t.Fatal("expected result")
	}
	if len(result.Triples) > 0 {
		t.Logf("result: src=%s, triples=%+v", result.Src, result.Triples)
	}
}

type dummyParser struct{}

func (d *dummyParser) Parse(text string) (*ParseResult, error) {
	return &ParseResult{}, nil
}
