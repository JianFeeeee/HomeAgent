package memory

import (
	"fmt"
	"testing"
)

// ★★★ 边作为独立单位的契约判据
//
// 现状（graph.go:216 的 schema）：
//
//	UNIQUE(source_kind, source_id, target_kind, target_id, edge_type)
//
// ⇒ 同一对节点之间只能有**一条**同类型边 ⇒ 边被降级成「两点的一个类型标签」，
// 而不是一个可独立存在、可承载属性、可并存多条的对象。
//
// 而「块作节点、边作独立单位、织成实体网」要求：
//
//	① 同一对节点间可并存多条同类边（不同 session / confidence / turn）
//	② 边自己承载属性（confidence / session_id / status …）
//	③ 边的身份是独立的，不是 (源,目标,类型) 三元组的派生
func TestEdge_同类边可并存(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	a := MemoryBlock{ID: "blk_a", Modality: BlockText, Text: "甲"}
	b := MemoryBlock{ID: "blk_b", Modality: BlockText, Text: "乙"}
	if err := g.PutMemoryBlocks([]MemoryBlock{a, b}); err != nil {
		t.Fatal(err)
	}

	// 同一对节点、三条同类型边，但来源会话不同
	type edgeSpec struct {
		sessionID string
		turnID    int
		conf      float64
	}
	specs := []edgeSpec{
		{"sess-1", 1, 0.9},
		{"sess-2", 2, 0.5},
		{"sess-3", 3, 0.7},
	}
	for _, sp := range specs {
		if err := g.AddRelationBlockEdge("blk_a", "blk_b", "属于", RelationEdgeData{
			SessionID: sp.sessionID, TurnID: sp.turnID, Confidence: sp.conf,
		}); err != nil {
			t.Fatalf("写入关系边失败: %v", err)
		}
	}

	edges, err := g.MemoryBlockEdges()
	if err != nil {
		t.Fatal(err)
	}
	var relEdges []MemoryBlockEdge
	for _, e := range edges {
		if e.Type == "属于" {
			relEdges = append(relEdges, e)
		}
	}
	fmt.Printf("  同类边写入 %d 条，实际存下 %d 条\n", len(specs), len(relEdges))

	// ★ 契约：三条都要在
	if len(relEdges) != len(specs) {
		t.Errorf("★ 同类边应可并存 %d 条，实际 %d 条 —— UNIQUE 约束把边降级成了标签",
			len(specs), len(relEdges))
	}
}

// ★ 边自己承载属性。
func TestEdge_承载关系属性(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	for _, b := range []MemoryBlock{
		{ID: "blk_a", Modality: BlockText, Text: "甲"},
		{ID: "blk_b", Modality: BlockText, Text: "乙"},
	} {
		if err := g.PutMemoryBlocks([]MemoryBlock{b}); err != nil {
			t.Fatal(err)
		}
	}

	if err := g.AddRelationBlockEdge("blk_a", "blk_b", "属于", RelationEdgeData{
		SessionID: "sess-x", TurnID: 42, Confidence: 0.87, Status: EdgeActive,
	}); err != nil {
		t.Fatal(err)
	}

	edges, err := g.MemoryBlockEdges()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range edges {
		if e.Type != "属于" {
			continue
		}
		found = true
		fmt.Printf("  边 #%d: session=%s turn=%d conf=%.2f status=%s\n",
			e.ID, e.SessionID, e.TurnID, e.Confidence, e.Status)
		if e.SessionID != "sess-x" {
			t.Errorf("★ 边未承载 session_id，实际 %q", e.SessionID)
		}
		if e.TurnID != 42 {
			t.Errorf("★ 边未承载 turn_id，实际 %d", e.TurnID)
		}
		if e.Confidence < 0.86 || e.Confidence > 0.88 {
			t.Errorf("★ 边未承载 confidence，实际 %.2f", e.Confidence)
		}
		if e.Status != EdgeActive {
			t.Errorf("★ 边未承载 status，实际 %q", e.Status)
		}
	}
	if !found {
		t.Fatal("没找到刚写入的关系边")
	}
}
