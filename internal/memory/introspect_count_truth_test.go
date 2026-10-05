package memory

import (
	"path/filepath"
	"testing"
)

// TestIntrospectEntityCountIsNotZeroAfterWrites 钉住「Introspect 不再报告空库」。
//
// ★ 缺陷来源（2026-10-05 隔离实例实测）：
//
//	一次对话写入 7 块 9 边，工具输出却是：
//	  memory_commit      → 已写入 0 个实体和 0 条关系
//	  memory_introspect  → map[entity_count:0 memory_hotspots:[map[count:15 name:order-gw]]]
//
//	⇒ entity_count 读的是**旧表 entities**（停双写后恒 0），
//	  而 hotspots 已改数块（真实度数 15）。
//	  同一份输出里自相矛盾，模型据此认为「记忆库是空的」并放弃写入
//	  （实测模型回复："the write is being rejected"）。
func TestIntrospectEntityCountIsNotZeroAfterWrites(t *testing.T) {
	g, err := NewGraphDB(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	if _, _, err := g.Commit([]Triple{
		{Subject: "张三", Relation: "负责", Object: "订单网关"},
		{Subject: "李四", Relation: "负责", Object: "支付网关"},
	}, "s1", 1); err != nil {
		t.Fatal(err)
	}

	st, err := g.Introspect()
	if err != nil {
		t.Fatal(err)
	}

	ec, ok := st["entity_count"].(int)
	if !ok {
		t.Fatalf("entity_count 类型异常: %T", st["entity_count"])
	}
	// ★ 旧表是空的（停双写），所以旧表口径必然给 0。
	if ec == 0 {
		var blocks, legacy int
		g.db.QueryRow("SELECT COUNT(*) FROM memory_blocks WHERE text_content != ''").Scan(&blocks)
		g.db.QueryRow("SELECT COUNT(*) FROM entities").Scan(&legacy)
		t.Fatalf("entity_count=0，但库里已有 %d 个块（旧表 %d 行）—— "+
			"它还在读旧表，模型会据此认为记忆库是空的", blocks, legacy)
	}
	// relation_count 已数块，同一份输出里两个字段口径必须一致。
	rc, _ := st["relation_count"].(int)
	if rc == 0 {
		t.Errorf("relation_count=0 与 entity_count=%d 矛盾 —— 同一份输出里两个字段口径不一致", ec)
	}
}

// TestIntrospectCountsBlocksNotLegacyTable 钉住「口径切块」的判据本身。
//
// ★ 为什么单独立一条：上面那条在「块数为 0」时会假绿
//
//	（新建空库时 entity_count=0 是正确的）。
//	这条显式构造「旧表空、块有内容」，把口径问题与数据量分离。
func TestIntrospectCountsBlocksNotLegacyTable(t *testing.T) {
	g, err := NewGraphDB(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	// 只写块，不碰旧表 —— 这正是停双写后的真实形态。
	if err := g.PutMemoryBlocks([]MemoryBlock{
		{ID: "b1", Modality: BlockText, Text: "节点甲"},
		{ID: "b2", Modality: BlockText, Text: "节点乙"},
		{ID: "b3", Modality: BlockText, Text: "节点丙"},
	}); err != nil {
		t.Fatal(err)
	}

	var legacy int
	if err := g.db.QueryRow("SELECT COUNT(*) FROM entities").Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Fatalf("测试前提不成立：旧表不该有行，实际 %d", legacy)
	}

	st, err := g.Introspect()
	if err != nil {
		t.Fatal(err)
	}
	if ec, _ := st["entity_count"].(int); ec != 3 {
		t.Errorf("写了 3 个块，entity_count 应为 3，实际 %d —— 仍在读旧表", ec)
	}
}

// TestMemoryEdgeCountExcludesContains 钉住边数口径。
//
// memory_commit 的文案用 newEdges 报告「写了几条关系」，
// 若把 contains 结构边算进去，数字会随原句数量翻倍 ——
// 同一个数在不同批次之间不可比，模型也会误读为「写多了」。
func TestMemoryEdgeCountExcludesContains(t *testing.T) {
	g := openArbTestDB(t)

	// 一句原句 + 一个字段块 = 1 条 contains 结构边、0 条关系边。
	writeBlockWithSource(t, g, "b_f", "维度=值", "blk_src_1")

	n, err := g.MemoryEdgeCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("只有 contains 结构边时 MemoryEdgeCount 应为 0，实际 %d", n)
	}

	// 加一条真关系边。
	if err := g.AddMemoryBlockEdge("block", "blk_src_1", "block", "b_f", "关联"); err != nil {
		t.Fatal(err)
	}
	n2, err := g.MemoryEdgeCount()
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 1 {
		t.Errorf("加了 1 条关系边后应为 1，实际 %d", n2)
	}
}

// db2Alias 只是为了让上面那行读起来明确（openArbTestDB 已返回 *GraphDB）。
func db2Alias(g *GraphDB) *GraphDB { return g }
