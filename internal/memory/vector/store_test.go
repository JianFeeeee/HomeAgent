package vector

import (
	"math"
	"testing"
)

func TestExtractNGrams(t *testing.T) {
	tests := []struct {
		text  string
		maxN  int
		check []string // 应包含
	}{
		{"hello", 2, []string{"h", "e", "l", "o", "he", "el", "ll", "lo"}},
		{"中文测试", 2, []string{"中", "文", "测", "试", "中文", "文测", "测试"}},
		{"a b", 1, []string{"a", "b"}},
	}
	for _, tt := range tests {
		got := extractNGrams(tt.text, tt.maxN)
		for _, want := range tt.check {
			found := false
			for _, g := range got {
				if g == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("extractNGrams(%q, %d) missing %q; got %v", tt.text, tt.maxN, want, got)
			}
		}
	}
}

func TestExtractNGramsNoDups(t *testing.T) {
	got := extractNGrams("aaaa", 2)
	seen := make(map[string]bool)
	for _, g := range got {
		if seen[g] {
			t.Errorf("duplicate ngram: %q", g)
		}
		seen[g] = true
	}
}

func TestCosineSimilarity(t *testing.T) {
	a := Vector{"a": 1, "b": 2}
	b := Vector{"a": 2, "b": 4}
	sim := CosineSimilarity(a, b)
	if math.Abs(sim-1.0) > 1e-6 {
		t.Errorf("identical direction vectors should have cos=1, got %f", sim)
	}

	c := Vector{"a": 1, "b": 0}
	d := Vector{"a": 0, "b": 1}
	sim = CosineSimilarity(c, d)
	if math.Abs(sim) > 1e-6 {
		t.Errorf("orthogonal vectors should have cos=0, got %f", sim)
	}

	sim = CosineSimilarity(Vector{}, Vector{"a": 1})
	if sim != 0 {
		t.Errorf("zero vector should return 0, got %f", sim)
	}
}

func TestTFIDFVectorizer(t *testing.T) {
	v := NewTFIDFVectorizer(NGramTokenizer(2))
	docs := []string{"今天天气很好", "今天心情不错", "明天要下雨"}
	v.Train(docs)

	vec := v.Vectorize("今天")
	if len(vec) == 0 {
		t.Fatal("vector should not be empty")
	}
	if _, ok := vec["今天"]; !ok {
		t.Errorf("expected feature '今天' in vector")
	}

	// 两个文档都有"今天"，idf 应该较低
	idfToday := vec["今天"]
	vecSun := v.Vectorize("下雨")
	idfRain := vecSun["下雨"]
	if idfRain <= idfToday {
		t.Errorf("expected rare '下雨' to have higher idf than common '今天', got today=%f rain=%f", idfToday, idfRain)
	}
}

func TestTFIDFVectorizerEmpty(t *testing.T) {
	v := NewTFIDFVectorizer(NGramTokenizer(2))
	v.Train(nil)
	vec := v.Vectorize("test")
	if len(vec) == 0 {
		t.Error("should produce features even without training")
	}

	// 未训练时所有 idf=1, 仅有 tf 归一化
	for _, w := range vec {
		if w < 0 {
			t.Errorf("weight should be non-negative, got %f", w)
		}
	}
}

func TestInvertedIndex(t *testing.T) {
	idx := NewInvertedIndex()

	idx.Add("doc1", Vector{"a": 1, "b": 2})
	idx.Add("doc2", Vector{"b": 1, "c": 3})

	results := idx.Search(Vector{"a": 1}, 10)
	if len(results) != 1 || results[0] != "doc1" {
		t.Errorf("search 'a' should return doc1 only, got %v", results)
	}

	results = idx.Search(Vector{"b": 1}, 10)
	if len(results) != 2 {
		t.Errorf("search 'b' should return 2 docs, got %v", results)
	}

	idx.Remove("doc1")
	results = idx.Search(Vector{"a": 1}, 10)
	if len(results) != 0 {
		t.Errorf("after remove, search 'a' should return empty, got %v", results)
	}
}

func TestStoreInsertAndSearch(t *testing.T) {
	s := NewStore()
	v := NewTFIDFVectorizer(NGramTokenizer(2))
	v.Train([]string{"hello world", "goodbye world"})

	s.Insert("1", "hello world", v.Vectorize("hello world"), nil)
	s.Insert("2", "goodbye world", v.Vectorize("goodbye world"), nil)

	if s.Size() != 2 {
		t.Errorf("expected size 2, got %d", s.Size())
	}

	results := s.Search(v.Vectorize("hello"), 5)
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if results[0].ID != "1" {
		t.Errorf("expected doc1 as top result for 'hello', got %s", results[0].ID)
	}
}

func TestStoreRemove(t *testing.T) {
	s := NewStore()
	v := NewTFIDFVectorizer(NGramTokenizer(1))
	v.Train([]string{"hello world", "hello a", "foo bar", "baz qux", "test doc"})

	s.Insert("1", "a", v.Vectorize("a"), nil)
	s.Insert("2", "a", v.Vectorize("a"), nil)
	s.Remove("1")

	if s.Size() != 1 {
		t.Errorf("after remove, size should be 1, got %d", s.Size())
	}

	results := s.Search(v.Vectorize("a"), 5)
	if len(results) != 1 || results[0].ID != "2" {
		t.Errorf("only doc2 should remain, got %v", results)
	}
}

func TestStoreEmpty(t *testing.T) {
	s := NewStore()
	results := s.Search(Vector{"a": 1}, 5)
	if results != nil {
		t.Errorf("empty store should return nil, got %v", results)
	}
}

func TestStoreAll(t *testing.T) {
	s := NewStore()
	v := NewTFIDFVectorizer(NGramTokenizer(1))
	v.Train([]string{"a", "b"})

	s.Insert("1", "a", v.Vectorize("a"), map[string]string{"k": "v"})
	s.Insert("2", "b", v.Vectorize("b"), nil)

	all := s.All()
	if len(all) != 2 {
		t.Errorf("All() should return 2 docs, got %d", len(all))
	}
	if all[0].Meta["k"] != "v" {
		t.Errorf("meta should be preserved")
	}
}

func BenchmarkCosineSimilarity(b *testing.B) {
	va := Vector{}
	vb := Vector{}
	for i := 0; i < 100; i++ {
		f := string(rune('a' + i%26))
		va[f] = float64(i)
		vb[f] = float64(100 - i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CosineSimilarity(va, vb)
	}
}

func BenchmarkExtractNGrams(b *testing.B) {
	text := "今天天气很好，适合出去散步。明天可能下雨，记得带伞。"
	for i := 0; i < b.N; i++ {
		extractNGrams(text, 2)
	}
}

// 候选中选阈值必须**按向量空间标定**：词向量/多模态余弦通常在 0.3~0.9，
// 而 TF-IDF 余弦只有 0.0~0.2。用同一个阈值会把词法路的有效候选静默砍掉
// （知识库自检索 MRR 0.307→0.193 就是这么掉的，且当时看不出任何报错）。
func TestSearchScoredRespectsMinScore(t *testing.T) {
	// 构造一个低余弦候选：共享特征 "a"，但两个向量几乎正交 → cosine ≈ 0.02
	st := NewStore()
	st.Insert("doc", "", Vector{"a": 1, "b": 1}, nil) // |doc| = √2
	query := Vector{"a": 0.02, "c": 100}              // 与 doc 的点积 0.02

	hits := st.SearchScored(query, 10)
	for _, h := range hits {
		if h.Score < DefaultMinScore {
			t.Fatalf("默认阈值 %.2f 不该返回 %.5f 的候选", DefaultMinScore, h.Score)
		}
	}
	if len(hits) != 0 {
		t.Fatalf("该查询在默认阈值下应被过滤，实际返回 %d 条", len(hits))
	}

	st.SetMinScore(0)
	hits = st.SearchScored(query, 10)
	if len(hits) != 1 || hits[0].Doc.ID != "doc" {
		t.Fatalf("阈值设为 0 后应召回低余弦候选，实际 %+v", hits)
	}
}
