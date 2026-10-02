package memory

import (
	"math"
	"os"
	"testing"
)

func newBlockTestGraph(t *testing.T) *GraphDB {
	t.Helper()
	g := newTestGraph(t)
	t.Cleanup(func() {
		g.Close()
		os.Remove(g.dbPath)
	})
	return g
}

func vec(vals ...float64) []float64 { return vals }

// ★ 基本召回：按余弦排序，TopK 截断。
func TestRecallBlocks_按余弦排序(t *testing.T) {
	g := newBlockTestGraph(t)
	mustPut(t, g, []MemoryBlock{
		{ID: "b_far", Modality: BlockText, Text: "无关内容",
			Vector: vec(0, 1, 0), Fingerprint: "fp1"},
		{ID: "b_near", Modality: BlockText, Text: "最相关",
			Vector: vec(1, 0, 0), Fingerprint: "fp1"},
		{ID: "b_mid", Modality: BlockText, Text: "中等相关",
			Vector: vec(0.7, 0.7, 0), Fingerprint: "fp1"},
	})

	hits, err := g.RecallBlocks(BlockRecallQuery{
		Vector: vec(1, 0, 0), Fingerprint: "fp1", TopK: 3,
	})
	if err != nil {
		t.Fatalf("RecallBlocks: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("期望 3 条，实际 %d", len(hits))
	}
	want := []string{"b_near", "b_mid", "b_far"}
	for i, id := range want {
		if hits[i].Block.ID != id {
			t.Errorf("第 %d 位应为 %s，实际 %s（score=%.4f）",
				i, id, hits[i].Block.ID, hits[i].Score)
		}
	}
	if math.Abs(hits[0].Score-1.0) > 1e-9 {
		t.Errorf("正交向量自身相似度应为 1，实际 %.9f", hits[0].Score)
	}
}

// ★ TopK 必须真的截断（否则召回会灌爆上下文——这是本会话实测过的症状）。
func TestRecallBlocks_TopK截断(t *testing.T) {
	g := newBlockTestGraph(t)
	var blocks []MemoryBlock
	for i := 0; i < 20; i++ {
		blocks = append(blocks, MemoryBlock{
			ID: "b" + itoa(i), Modality: BlockText, Text: "t",
			Vector: vec(float64(i+1), 1, 0), Fingerprint: "fp1",
		})
	}
	mustPut(t, g, blocks)

	hits, err := g.RecallBlocks(BlockRecallQuery{Vector: vec(1, 0, 0), TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 5 {
		t.Fatalf("TopK=5 应返回 5 条，实际 %d", len(hits))
	}
}

// ★★ 指纹防护：换向量空间后，旧空间的块必须被**跳过**。
//
// 这是本文件最重要的判据。拿新空间的查询向量去比旧空间的块向量，
// 余弦值没有任何意义，而且**不报错**——只是"结果看起来还行但全是错的"。
func TestRecallBlocks_指纹不匹配则跳过(t *testing.T) {
	g := newBlockTestGraph(t)
	mustPut(t, g, []MemoryBlock{
		{ID: "old", Modality: BlockText, Text: "旧空间",
			Vector: vec(1, 0, 0), Fingerprint: "old-space"},
		{ID: "new", Modality: BlockText, Text: "新空间",
			Vector: vec(1, 0, 0), Fingerprint: "new-space"},
	})

	hits, err := g.RecallBlocks(BlockRecallQuery{
		Vector: vec(1, 0, 0), Fingerprint: "new-space", TopK: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Block.ID != "new" {
		t.Fatalf("只应召回新空间的块，实际 %+v", hitIDs(hits))
	}
	// 旧空间的块若被召回，就是静默污染
	for _, h := range hits {
		if h.Block.Fingerprint != "new-space" {
			t.Errorf("召回了指纹不匹配的块: %s (%s)", h.Block.ID, h.Block.Fingerprint)
		}
	}
}

// ★★ 维度防护：维度不同必须跳过，**不截断不填充**。
func TestRecallBlocks_维度不匹配则跳过(t *testing.T) {
	g := newBlockTestGraph(t)
	mustPut(t, g, []MemoryBlock{
		{ID: "d512", Modality: BlockText, Text: "旧 512 维",
			Vector: vec(1, 0, 0), Fingerprint: "fp"},
		{ID: "d2048", Modality: BlockText, Text: "新 2048 维",
			Vector: append(vec(1, 0, 0), make([]float64, 2045)...), Fingerprint: "fp"},
	})

	hits, err := g.RecallBlocks(BlockRecallQuery{
		Vector: append(vec(1, 0, 0), make([]float64, 2045)...), Fingerprint: "fp", TopK: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Block.ID != "d2048" {
		t.Fatalf("只应召回同维度的块，实际 %+v", hitIDs(hits))
	}
}

// 没有向量的块不参与向量召回（但它们仍可被 BlocksForNode 按边查到）。
func TestRecallBlocks_无向量的块不参与(t *testing.T) {
	g := newBlockTestGraph(t)
	mustPut(t, g, []MemoryBlock{
		{ID: "no_vec", Modality: BlockText, Text: "没向量"},
		{ID: "has_vec", Modality: BlockText, Text: "有向量",
			Vector: vec(1, 0), Fingerprint: "fp"},
	})

	hits, err := g.RecallBlocks(BlockRecallQuery{Vector: vec(1, 0), TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Block.ID != "has_vec" {
		t.Fatalf("无向量的块不该参与召回，实际 %+v", hitIDs(hits))
	}
}

// 空查询向量必须报错（而不是静默返回全零分的垃圾结果）。
func TestRecallBlocks_空向量报错(t *testing.T) {
	g := newBlockTestGraph(t)
	if _, err := g.RecallBlocks(BlockRecallQuery{}); err == nil {
		t.Fatal("空查询向量应报错")
	}
}

// MinScore 过滤。
func TestRecallBlocks_MinScore过滤(t *testing.T) {
	g := newBlockTestGraph(t)
	mustPut(t, g, []MemoryBlock{
		{ID: "hi", Modality: BlockText, Text: "高", Vector: vec(1, 0), Fingerprint: "fp"},
		{ID: "lo", Modality: BlockText, Text: "低", Vector: vec(0, 1), Fingerprint: "fp"},
	})
	hits, err := g.RecallBlocks(BlockRecallQuery{
		Vector: vec(1, 0), TopK: 10, MinScore: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Block.ID != "hi" {
		t.Fatalf("MinScore 应滤掉低分候选，实际 %+v", hitIDs(hits))
	}
}

// 库里没有任何块时返回空切片而非错误（"还没回填"是正常状态）。
func TestRecallBlocks_空库不报错(t *testing.T) {
	g := newBlockTestGraph(t)
	hits, err := g.RecallBlocks(BlockRecallQuery{Vector: vec(1, 0), TopK: 5})
	if err != nil {
		t.Fatalf("空库不该报错: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("空库应返回 0 条，实际 %d", len(hits))
	}
}

// BlockVectorStats 报告回填状态（运维据此判断要不要跑回填）。
func TestBlockVectorStats(t *testing.T) {
	g := newBlockTestGraph(t)
	mustPut(t, g, []MemoryBlock{
		{ID: "a", Modality: BlockText, Text: "有向量有指纹",
			Vector: vec(1, 0), Fingerprint: "fpX"},
		{ID: "b", Modality: BlockText, Text: "有向量无指纹",
			Vector: vec(1, 0)},
		{ID: "c", Modality: BlockText, Text: "无向量"},
	})

	st, err := g.BlockVectorStats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 3 || st.WithVector != 2 || st.MissingFP != 1 {
		t.Fatalf("统计错误: %+v", st)
	}
	if len(st.Fingerprints) != 1 || st.Fingerprints[0] != "fpX" {
		t.Errorf("指纹集错误: %v", st.Fingerprints)
	}
	if len(st.Dimensions) != 1 || st.Dimensions[0] != 2 {
		t.Errorf("维度集错误: %v", st.Dimensions)
	}
}

// ── 辅助 ──

func mustPut(t *testing.T, g *GraphDB, blocks []MemoryBlock) {
	t.Helper()
	if err := g.PutMemoryBlocks(blocks); err != nil {
		t.Fatalf("PutMemoryBlocks: %v", err)
	}
}

func hitIDs(hits []BlockHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Block.ID
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
