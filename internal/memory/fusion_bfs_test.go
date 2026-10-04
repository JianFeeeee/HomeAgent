package memory

import (
	"fmt"
	"testing"
)

// ═══════════════════════════════════════════════════════════════
//  BFS 接入召回链 —— 判据
//
//  ★ 真实缺口（生产探针 coexist 维度 0/1）
//
//  查询「本机服务监听哪些端口」召不回 13010。
//  原因：库里含「端口」二字的块是 14010端口 / 14011端口 那批，
//  而真正的本机端口块文本是「http://127.0.0.1:13010」——
//  **不含「端口」二字**。
//
//  ⇒ 纯向量/符号召回召不回它。
//  ⇒ 而图上是连着的：billing服务 --监听--> 13010
//     只要从「监听」或端口号出发 BFS，就能找回服务名。
//
//  ★ 所以这一格测的不是「关键词召回能不能更强」，
//  而是「召回**是否即联想**」—— 孤立命中之外，
//  还要沿边找回上下文。
// ═══════════════════════════════════════════════════════════════

func seedCoexistGraph(t *testing.T) *GraphDB {
	t.Helper()
	g := newTestGraph(t)
	// 织一张真实形态的网：服务 --监听--> 端口号
	blocks := []MemoryBlock{
		{ID: "b_svc", Modality: BlockText, Text: "billing服务", Vector: []float64{1, 0}, Fingerprint: "fp1"},
		{ID: "b_port", Modality: BlockText, Text: "13010", Vector: []float64{0.9, 0.1}, Fingerprint: "fp1"},
		{ID: "b_noise1", Modality: BlockText, Text: "14010端口", Vector: []float64{0, 1}, Fingerprint: "fp1"},
		{ID: "b_noise2", Modality: BlockText, Text: "14011端口", Vector: []float64{0, 1}, Fingerprint: "fp1"},
		{ID: "b_noise3", Modality: BlockText, Text: "CPU总线与MAR/MDR/ALU连接问题", Vector: []float64{0, 1}, Fingerprint: "fp1"},
	}
	for i := range blocks {
		if err := g.PutMemoryBlocks([]MemoryBlock{blocks[i]}); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []struct{ from, to, typ string }{
		{"b_svc", "b_port", "监听"},
		{"b_noise1", "b_port", "监听"},
		{"b_noise3", "b_svc", "涉及"},
	} {
		if err := g.AddRelationBlockEdge(e.from, e.to, e.typ,
			RelationEdgeData{SessionID: "s1", Confidence: 0.9, Status: EdgeActive}); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// TestBFS召回_从服务找回端口：图上连着的就该召回
func TestBFS召回_从服务找回端口(t *testing.T) {
	g := seedCoexistGraph(t)
	defer func() { _ = g.Close() }()

	// 查询直接命中「billing服务」——一个**孤立的语义节点**，
	// 但图上它连着 13010。
	// 带上查询向量：RecallBlocks 要求非空向量（符号路也依赖融合入口）
	q := BlockRecallQuery{Vector: []float64{1, 0}, TopK: 5, MinScore: -1}
	hits, _, err := g.RecallBlocksFusedBFS(q, "billing服务", 2)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  命中 %d 个: ", len(hits))
	for _, h := range hits {
		fmt.Printf("%q ", h.Text)
	}
	fmt.Println()

	if !containsText(hits, "13010") {
		t.Errorf("★ 召回应沿边找回端口号 13010（召回即联想），实际 %v", textsOfFused(hits))
	}
}

// TestBFS召回_顺序稳定：map 遍历会让输出顺序随机
func TestBFS召回_顺序稳定(t *testing.T) {
	g := seedCoexistGraph(t)
	defer func() { _ = g.Close() }()

	// 带上查询向量：RecallBlocks 要求非空向量（符号路也依赖融合入口）
	q := BlockRecallQuery{Vector: []float64{1, 0}, TopK: 5, MinScore: -1}
	var first []string
	for run := 0; run < 5; run++ {
		hits, _, err := g.RecallBlocksFusedBFS(q, "billing服务", 2)
		if err != nil {
			t.Fatal(err)
		}
		got := textsOfFused(hits)
		if run == 0 {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("第 %d 次长度不同: %v vs %v", run, got, first)
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("★ 第 %d 次顺序不同: %v vs %v（map 遍历导致输出不稳定）", run, got, first)
			}
		}
	}
	fmt.Printf("  5 次顺序一致: %v\n", first)
}

// TestBFS召回_预算有界：不能因为加 BFS 就无限膨胀
func TestBFS召回_预算有界(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	// 织一个 200 节点的链：加 BFS 若无预算，depth=3 会拉进一大片
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("n%03d", i)
		if err := g.PutMemoryBlocks([]MemoryBlock{
			{ID: id, Modality: BlockText, Text: fmt.Sprintf("节点%03d", i),
				Vector: []float64{1, 0}, Fingerprint: "fp1"},
		}); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			if err := g.AddRelationBlockEdge(fmt.Sprintf("n%03d", i-1), id, "next",
				RelationEdgeData{SessionID: "s", Status: EdgeActive}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// 带上查询向量：RecallBlocks 要求非空向量（符号路也依赖融合入口）
	q := BlockRecallQuery{Vector: []float64{1, 0}, TopK: 5, MinScore: -1}
	hits, _, err := g.RecallBlocksFusedBFS(q, "节点000", 3)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  200 节点链，depth=3 → 召回 %d 个\n", len(hits))
	if len(hits) > 60 {
		t.Errorf("★ BFS 应受预算约束（200 节点链 depth=3 召回 %d 个，过多）", len(hits))
	}
}

func containsText(hits []FusedHit, s string) bool {
	for _, h := range hits {
		if h.Text == s {
			return true
		}
	}
	return false
}

func textsOfFused(hits []FusedHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Text)
	}
	return out
}
