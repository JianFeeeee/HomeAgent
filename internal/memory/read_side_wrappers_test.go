package memory

import (
	"fmt"
	"testing"
)

// ═══════════════════════════════════════════════════════════════
//  读方切换用的薄包装 —— 独立判据
//
//  ★ 这三个函数是 indexer / social / distill / Purge 迁到块体系的前提。
//   它们最容易出的错是「看着对」：能返回数据，但语义偏了一点 ——
//   比如去重口径、LIKE 转义、方向判定，而调用方未必能察觉。
//
//  所以判据不只测「有返回」，还测**边界**：
//   空输入、单字符、%、_、方向、重复文本。
// ═══════════════════════════════════════════════════════════════

func seedReadSideBlocks(t *testing.T) *GraphDB {
	t.Helper()
	g := newTestGraph(t)
	blocks := []MemoryBlock{
		{ID: "blk_person_a", Modality: BlockText, Text: "老周"},
		{ID: "blk_person_b", Modality: BlockText, Text: "小李"},
		{ID: "blk_svc", Modality: BlockText, Text: "billing服务"},
		{ID: "blk_port", Modality: BlockText, Text: "4324"},
		{ID: "blk_pct", Modality: BlockText, Text: "CPU占用50%"},
		{ID: "blk_under", Modality: BlockText, Text: "a_b"},
		{ID: "blk_dup", Modality: BlockText, Text: "老周"},
	}
	for i := range blocks {
		if err := g.PutMemoryBlocks([]MemoryBlock{blocks[i]}); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []struct {
		from, to, typ string
	}{
		{"blk_person_a", "blk_svc", "维护"},
		{"blk_svc", "blk_port", "监听"},
		{"blk_person_b", "blk_svc", "使用"},
		{"blk_port", "blk_dup", "同值"},
	} {
		if err := g.AddRelationBlockEdge(e.from, e.to, e.typ,
			RelationEdgeData{SessionID: "s1", Confidence: 0.8, Status: EdgeActive}); err != nil {
			t.Fatal(err)
		}
	}
	// ★ 一条 deleted 边：不该被 NeighbourEdgesOfBlock 返回
	if err := g.AddRelationBlockEdge("blk_person_a", "blk_port", "废弃",
		RelationEdgeData{SessionID: "s1", Status: EdgeDeleted}); err != nil {
		t.Fatal(err)
	}
	return g
}

// BlockByText：精确匹配 + 同文本多块取最早
func TestWrap_BlockByText精确匹配(t *testing.T) {
	g := seedReadSideBlocks(t)
	defer func() { _ = g.Close() }()

	b, err := g.BlockByText("老周")
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		t.Fatal("★ 应命中「老周」")
	}
	// 同文本两块（blk_person_a / blk_dup）⇒ 取最早的
	fmt.Printf("  「老周」命中 %s\n", b.ID)
	if b.ID != "blk_person_a" {
		t.Errorf("★ 同文本多块应取最早，实际 %s", b.ID)
	}

	// 精确匹配不该做前缀/子串
	if b2, err := g.BlockByText("老"); err != nil || b2 != nil {
		t.Errorf("★ 精确匹配不该命中「老」（子串），实际 %+v", b2)
	}
	// 空输入
	if b3, err := g.BlockByText("  "); err != nil || b3 != nil {
		t.Errorf("★ 空输入应返回 nil，实际 %+v", b3)
	}
}

// BlockTextsLike：% 与 _ 必须被转义，否则误命中
func TestWrap_BlockTextsLike转义元字符(t *testing.T) {
	g := seedReadSideBlocks(t)
	defer func() { _ = g.Close() }()

	// 「50%」若不转义，% 会当通配符 ⇒ 命中所有含 5 的块
	got, err := g.BlockTextsLike("50%")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  LIKE '50%%' 命中 %d 个\n", len(got))
	if len(got) != 1 || got[0].Text != "CPU占用50%" {
		t.Errorf("★ 「50%%」应只命中字面量那一条，实际 %d 个", len(got))
	}

	// 「a_b」若不转义，_ 会匹配任意单字符
	got2, err := g.BlockTextsLike("a_b")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  LIKE 'a_b' 命中 %d 个\n", len(got2))
	if len(got2) != 1 || got2[0].Text != "a_b" {
		t.Errorf("★ 「a_b」应只命中字面量那一条，实际 %d 个：%v", len(got2), texts2(got2))
	}

	// 普通子串仍应工作
	got3, err := g.BlockTextsLike("服务")
	if err != nil {
		t.Fatal(err)
	}
	if len(got3) != 1 || got3[0].Text != "billing服务" {
		t.Errorf("★ 普通子串应命中 1 条，实际 %d", len(got3))
	}
}

// AllBlockTexts：去重 + 排除空文本 + 分批边界
func TestWrap_AllBlockTexts去重与分批(t *testing.T) {
	g := seedReadSideBlocks(t)
	defer func() { _ = g.Close() }()
	// 一个空文本块（媒体块常见）
	if err := g.PutMemoryBlocks([]MemoryBlock{
		{ID: "blk_media", Modality: BlockImage, Text: ""},
	}); err != nil {
		t.Fatal(err)
	}

	full, err := g.AllBlockTexts(0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  全量块文本 %d 个（块数 8，其中「老周」重复）\n", len(full))
	// 8 块 - 1 空文本 - 1 重复 = 6
	if len(full) != 6 {
		t.Errorf("★ 应为 6 个（去重 + 排除空），实际 %d：%v", len(full), full)
	}
	for _, s := range full {
		if s == "" {
			t.Error("★ 不该返回空文本块")
		}
	}

	// ★ 分批大小必须不影响结果（批大小 1 / 2 / 3 都要与默认一致）
	for _, batch := range []int{1, 2, 3, 100} {
		got, err := g.AllBlockTexts(batch)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(full) {
			t.Errorf("★ 批大小 %d 结果不同：%d vs %d —— 分页逻辑有 bug", batch, len(got), len(full))
		}
	}
}

// NeighbourEdgesOfBlock：双向 + 方向标记 + 排除 deleted
func TestWrap_NeighbourEdges双向与方向(t *testing.T) {
	g := seedReadSideBlocks(t)
	defer func() { _ = g.Close() }()

	// blk_svc 有出边(→port)与入边(←person_a, ←person_b)
	edges, peers, err := g.NeighbourEdgesOfBlock("blk_svc")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  blk_svc 邻居边 %d 条，对端块 %d 个\n", len(edges), len(peers))
	if len(edges) != 3 {
		t.Errorf("★ blk_svc 应有 3 条边（1 出 2 入），实际 %d", len(edges))
	}

	var out, in int
	for _, e := range edges {
		if e.IsOutgoing {
			out++
			if e.Peer.Text != "4324" {
				t.Errorf("★ 出边对端应是 4324，实际 %q", e.Peer.Text)
			}
		} else {
			in++
			if e.Peer.Text != "老周" && e.Peer.Text != "小李" {
				t.Errorf("★ 入边对端应是 老周/小李，实际 %q", e.Peer.Text)
			}
		}
		if e.Status == EdgeDeleted {
			t.Error("★ deleted 边不该返回")
		}
	}
	if out != 1 || in != 2 {
		t.Errorf("★ 方向统计不对：出 %d 入 %d（应 1/2）", out, in)
	}

	// 「老周」是 blk_person_a 与 blk_dup 两个块的文本
	// —— BlockByText 取 blk_person_a，Neighbor 查它自己的边
	e2, _, err := g.NeighbourEdgesOfBlock("blk_person_a")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  blk_person_a 邻居边 %d 条（应为 1，deleted 已排除）\n", len(e2))
	if len(e2) != 1 {
		t.Errorf("★ blk_person_a 应只有 1 条 active 边，实际 %d", len(e2))
	}

	// 空与不存在
	if e3, _, err := g.NeighbourEdgesOfBlock("blk_nope"); err != nil || len(e3) != 0 {
		t.Errorf("★ 不存在的块应返回空，实际 %d", len(e3))
	}
	if e4, _, err := g.NeighbourEdgesOfBlock(""); err != nil || len(e4) != 0 {
		t.Errorf("★ 空块 ID 应返回空，实际 %d", len(e4))
	}
}

func texts2(bs []MemoryBlock) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Text)
	}
	return out
}
