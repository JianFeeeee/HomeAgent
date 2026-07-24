package memory

import (
	"testing"
)

func needJieba(t *testing.T) {
	t.Helper()
	if GetJieba() == nil {
		t.Skip("jieba dictionaries not found")
	}
}

func TestCutExact(t *testing.T) {
	needJieba(t)

	tests := []struct {
		name string
		text string
		min  int
		not  []string
	}{
		{
			name: "chinese_sentence",
			text: "今天天气怎么样",
			min:  2,
			not:  nil,
		},
		{
			name: "stop_words_removed",
			text: "和天气地",
			min:  1,
			not:  []string{"的", "和"},
		},
		{
			name: "short_words_filtered",
			text: "今天好天气",
			min:  1,
			not:  []string{"好"},
		},
		{
			name: "empty_text",
			text: "",
			min:  0,
			not:  nil,
		},
		{
			name: "qq_conversation",
			text: "今天天气怎么样 → 今天天气很好",
			min:  2,
			not:  nil,
		},
		{
			name: "all_stop_words",
			text: "的了呢吗",
			min:  0,
			not:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CutExact(tt.text)
			if len(got) < tt.min {
				t.Errorf("CutExact(%q) = %v (len=%d), want at least %d terms", tt.text, got, len(got), tt.min)
			}
			for _, forbid := range tt.not {
				for _, g := range got {
					if g == forbid {
						t.Errorf("CutExact(%q) = %v, should not contain %q", tt.text, got, forbid)
					}
				}
			}
		})
	}
}

func TestCutExactNoDuplicates(t *testing.T) {
	needJieba(t)

	got := CutExact("天气天气天气")
	if len(got) > 1 {
		t.Errorf("expected deduplicated result, got %v (len=%d)", got, len(got))
	}
}

func TestCutExactValidEntityName(t *testing.T) {
	needJieba(t)

	got := CutExact("a b c")
	for _, g := range got {
		if !validEntityName(g) {
			t.Errorf("CutExact returned invalid entity name %q", g)
		}
	}
}

func TestCutExactRemoveTimestamp(t *testing.T) {
	needJieba(t)

	got := CutExact("[15:04] 今天天气不错")
	for _, g := range got {
		if g == "15" || g == "04" || g == "15:04" {
			t.Errorf("timestamp should be removed by CleanText, got %q in %v", g, got)
		}
	}
}
