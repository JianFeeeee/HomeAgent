package media

import (
	"math"
	"testing"
)

func TestQueryMedia_BasicSimilarity(t *testing.T) {
	s := newTestStore(t, 0)
	defer s.Close()

	// 入库三张带向量的媒体：两张图、一段音频
	d1, _ := s.Put([]byte("img1"), Item{MIME: "image/png", Description: "紫蓝红三色带"})
	d2, _ := s.Put([]byte("img2"), Item{MIME: "image/jpeg", Description: "蓝紫红渐变"})
	d3, _ := s.Put([]byte("aud1"), Item{MIME: "audio/wav", Description: "一段语音"})

	// 模拟视觉嵌入：img1 和 img2 向量接近，aud1 远离
	vec1 := []float64{0.9, 0.1, 0.0, 0.0}
	vec2 := []float64{0.8, 0.2, 0.0, 0.0} // 与 vec1 相似
	vec3 := []float64{0.0, 0.0, 0.9, 0.1} // 与前两个完全不同

	s.SetVec(d1, vec1, "test-clip")
	s.SetVec(d2, vec2, "test-clip")
	s.SetVec(d3, vec3, "test-clip")

	// 用 vec1 作为查询：vec2 最相似，vec3 与 vec1 正交（相似度 0，被阈值过滤）
	results, err := s.QueryMedia(vec1, "test-clip", 10)
	if err != nil {
		t.Fatal(err)
	}
	// vec3 与 vec1 正交（余弦相似度 0），被 0.05 阈值正确剔除 → 只召回 2 个
	if len(results) != 2 {
		t.Fatalf("expected 2 results (正交的 aud1 被阈值过滤), got %d", len(results))
	}
	// 第一个应该是 img2（0.9 vs d1 的 1.0？不，这里算清楚）
	// vec1·vec2 与 vec1·vec1 比较：
	//   sim(vec1,vec1) = 1.0（img1 与自身），sim(vec1,vec2) = 0.9*0.8+0.1*0.2 = 0.74
	// 所以 img1（自相似 1.0）排第一，img2 排第二
	if results[0].Digest != d1 {
		t.Errorf("expected d1 (自相似 1.0) as first, got %s", results[0].Digest)
	}
	if results[1].Digest != d2 {
		t.Errorf("expected d2 as second, got %s", results[1].Digest)
	}

	// 验证分数：img1 与自身是 1.0
	selfScore := cosineSimilaritySlice(vec1, vec1)
	if math.Abs(selfScore-1.0) > 1e-10 {
		t.Errorf("self-similarity should be 1.0, got %f", selfScore)
	}

	// img1 与 aud1 的相似度应该很低
	crossScore := cosineSimilaritySlice(vec1, vec3)
	if crossScore > 0.1 {
		t.Errorf("cross-modality similarity should be low, got %f", crossScore)
	}
}

func TestQueryMedia_EmptyVecSkipped(t *testing.T) {
	s := newTestStore(t, 0)
	defer s.Close()

	d1, _ := s.Put([]byte("img1"), Item{MIME: "image/png"})
	_, _ = s.Put([]byte("img2"), Item{MIME: "image/png"})

	// d1 有向量，d2 没有
	s.SetVec(d1, []float64{0.5, 0.5}, "test")
	// d2 留空

	results, err := s.QueryMedia([]float64{0.5, 0.5}, "test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (d2 has no vec), got %d", len(results))
	}
	if results[0].Digest != d1 {
		t.Errorf("expected d1, got %s", results[0].Digest)
	}
}

func TestQueryMedia_DimensionMismatchSkipped(t *testing.T) {
	s := newTestStore(t, 0)
	defer s.Close()

	d1, _ := s.Put([]byte("img1"), Item{MIME: "image/png"})
	s.SetVec(d1, []float64{0.5, 0.5}, "model-A") // 2 维

	// 查询用 3 维向量：维度不匹配，应该返回空
	results, err := s.QueryMedia([]float64{0.3, 0.3, 0.3}, "model-A", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results (dim mismatch), got %d", len(results))
	}
}

func TestQueryMedia_EmptyQueryReturnsNil(t *testing.T) {
	s := newTestStore(t, 0)
	defer s.Close()

	results, err := s.QueryMedia(nil, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if results != nil {
		t.Fatalf("expected nil, got %d results", len(results))
	}
}

func TestSetVec_PersistsCorrectly(t *testing.T) {
	s := newTestStore(t, 0)
	defer s.Close()

	d, _ := s.Put([]byte("hello"), Item{MIME: "image/png"})
	vec := []float64{0.1, 0.2, 0.3, 0.4}
	s.SetVec(d, vec, "clip-vit-b32")

	it, err := s.Stat(d)
	if err != nil {
		t.Fatal(err)
	}
	if it.VecModel != "clip-vit-b32" {
		t.Errorf("VecModel = %q, want clip-vit-b32", it.VecModel)
	}
	if len(it.Vec) != 4 {
		t.Fatalf("Vec len = %d, want 4", len(it.Vec))
	}
	for i, v := range vec {
		if math.Abs(it.Vec[i]-v) > 1e-10 {
			t.Errorf("Vec[%d] = %f, want %f", i, it.Vec[i], v)
		}
	}
}
