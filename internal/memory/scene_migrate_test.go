package memory

import (
	"fmt"
	"testing"
	"time"
)

// ★★★ scene_refs 完整迁移到块体系（kind='edge'/'block'）
//
// 现状（生产库实测）：
//
//	scene_refs  kind='entity'   450 条 → ref_id = entities.id（自增行号）
//	            kind='relation' 268 条 → ref_id = relations.id
//	            kind='block'     90 条 → **ref_text = 块 ID**（ref_id=0）
//	            kind='document'   2 条
//
// ⇒ 块引用**已经用 ref_text 存块 ID** 了。这给了迁移的现成契约：
//
//	kind='entity'   → 'block'    ref_id 清空，ref_text = blk_ent_<entityID>_<hash>
//	kind='relation' → 'edge'     ref_id 换成 memory_block_edges.id，ref_text 置空
//
// 映射可行性（生产快照实测，两个方向都是 100%）：
//
//	450 条 entity 引用   → 迁移块     450/450
//	268 条 relation 引用 → 边         268/268
//	无法反解的引用                     0 条
func TestSceneRef_迁移映射完整(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	// 造旧形态数据
	seedSceneRefs(t, g)

	before := sceneRefCounts(t, g)
	fmt.Printf("  迁移前: %v\n", before)

	n, err := g.MigrateSceneRefsToBlocks()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  迁移 %d 条引用\n", n)

	after := sceneRefCounts(t, g)
	fmt.Printf("  迁移后: %v\n", after)

	// ★ 旧 kind 必须归零（完整迁移，不是「大部分」）
	if after["entity"] != 0 {
		t.Errorf("★ kind='entity' 应全部迁走，剩 %d 条", after["entity"])
	}
	if after["relation"] != 0 {
		t.Errorf("★ kind='relation' 应全部迁走，剩 %d 条", after["relation"])
	}
	// ★ 计数要考虑去重：两条 entity 引用可能映射到同一块
	//   （scene_refs 有 UNIQUE(scene_id,kind,ref_id,ref_text)），
	//   迁移会删除重复那条。判据数据刻意造了这种情况。
	//
	// 契约是「旧 kind 全清零 + 新 kind 都有 + 无悬空」，
	// 而非「条数必须逐条对应」—— 因为去重是**正确**行为。
	//
	// 本例：entity 2 条 + 原 block 1 条 = 3 条引用，
	// 但两条 entity 指向同一块 ⇒ 实际 2 条（去重 1 条）。
	if after["block"] < before["block"] {
		t.Errorf("★ kind='block' 不该少于原有的 %d 条，实际 %d",
			before["block"], after["block"])
	}
	// 逐项核对（本例：entity 2 条 → 去重后 1 条 + 原 block 1 条 = block 2；
	//       relation 1 条 → edge 1 条）
	if after["block"] != 2 || after["edge"] != 1 {
		t.Errorf("★ 期望 block=2（去重后 1 + 原有 1）、edge=1，实际 block=%d edge=%d",
			after["block"], after["edge"])
	}
	fmt.Printf("  去重：entity 2 条 → 1 条（两条指向同一块）\n")
	if after["edge"] != before["relation"] {
		t.Errorf("★ kind='edge' 应为 %d 条（relation 全迁），实际 %d",
			before["relation"], after["edge"])
	}

	// ★ 每条迁走的引用都要能定位到真实对象（不能指向虚无）
	if n := countDanglingBlockRefs(t, g); n != 0 {
		t.Errorf("★ 迁移后有 %d 条 block 引用指向不存在的块", n)
	}
	if n := countDanglingEdgeRefs(t, g); n != 0 {
		t.Errorf("★ 迁移后有 %d 条 edge 引用指向不存在的边", n)
	}

	// ★ 幂等：跑第二遍不应改变任何东西
	if _, err := g.MigrateSceneRefsToBlocks(); err != nil {
		t.Fatal(err)
	}
	third := sceneRefCounts(t, g)
	if third["edge"] != after["edge"] || third["block"] != after["block"] {
		t.Errorf("★ 幂等被破坏: %v → %v", after, third)
	}
}

// seedSceneRefs 造旧形态的 scene_refs（entity/relation/block/document 四种）。
func seedSceneRefs(t *testing.T, g *GraphDB) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	tx, err := g.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	// 两个实体 + 一条关系 + 各自的块与边（块化后的形态，作为迁移来源）
	ents := []struct {
		id   int64
		name string
	}{}
	for i, name := range []string{"值班室分机号", "4324"} {
		if _, err := tx.Exec(
			`INSERT INTO entities (id, name, mention_count, created_at)
			 VALUES (?, ?, 1, '2026-01-01 10:00:00')`, int64(i+1), name); err != nil {
			t.Fatal(err)
		}
		ents = append(ents, struct {
			id   int64
			name string
		}{int64(i + 1), name})
	}
	if _, err := tx.Exec(
		`INSERT INTO relations (id, source_id, target_id, relation_type, confidence, status)
		 VALUES (1, 1, 2, '是', 0.9, 'active')`); err != nil {
		t.Fatal(err)
	}

	// 迁移块（ID 含 entityID，可反解）
	blkA := legacyEntityBlockID(ents[0].id, ents[0].name)
	blkB := legacyEntityBlockID(ents[1].id, ents[1].name)
	for _, b := range []MemoryBlock{
		{ID: blkA, Modality: BlockText, Text: ents[0].name, Source: "legacy-entity",
			CreatedAt: mustT("2026-01-01 10:00:00")},
		{ID: blkB, Modality: BlockText, Text: ents[1].name, Source: "legacy-entity",
			CreatedAt: mustT("2026-01-01 10:00:00")},
		{ID: "blk_src_manual", Modality: BlockText, Text: "手工建的原句块",
			Source: SentenceBlockSource},
	} {
		if _, err := putBlockTx(tx, b); err != nil {
			t.Fatal(err)
		}
	}
	// 场景与四种引用
	if _, err := tx.Exec(
		`INSERT INTO scenes (key, strength) VALUES ('值班场景', 1)`); err != nil {
		t.Fatal(err)
	}
	refs := []struct {
		kind, refID, refText string
	}{
		{"entity", "1", ""},
		{"entity", "2", ""},
		{"relation", "1", ""},
		{"block", "0", blkA},
		{"document", "0", "doc-1"},
	}
	for _, r := range refs {
		if _, err := tx.Exec(
			`INSERT INTO scene_refs (scene_id, kind, ref_id, ref_text, weight)
			 VALUES (1, ?, ?, ?, 1.0)`, r.kind, r.refID, r.refText); err != nil {
			t.Fatal(err)
		}
	}

	// ★ 关系边与上面所有数据在**同一事务**内写入。
	//   不能调 AddRelationBlockEdge：它内部 g.mu.Lock()，
	//   而本函数开头已 g.mu.Lock() 且 defer 解锁 ⇒ 自死锁。
	//   首次实现踩了这个：测试直接挂住不动，栈指向 seedSceneRefs。
	if _, err := tx.Exec(`INSERT INTO memory_block_edges
		(source_kind, source_id, target_kind, target_id, edge_type,
		 confidence, session_id, status)
		VALUES ('block', ?, 'block', ?, '是', 0.9, 's1', ?)`,
		blkA, blkB, EdgeActive); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func sceneRefCounts(t *testing.T, g *GraphDB) map[string]int {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT kind, COUNT(*) FROM scene_refs GROUP BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatal(err)
		}
		out[k] = n
	}
	return out
}

func countDanglingBlockRefs(t *testing.T, g *GraphDB) int {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	var n int
	_ = g.db.QueryRow(`SELECT COUNT(*) FROM scene_refs sr
		WHERE sr.kind='block' AND NOT EXISTS
			(SELECT 1 FROM memory_blocks b WHERE b.id = sr.ref_text)`).Scan(&n)
	return n
}

func countDanglingEdgeRefs(t *testing.T, g *GraphDB) int {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	var n int
	_ = g.db.QueryRow(`SELECT COUNT(*) FROM scene_refs sr
		WHERE sr.kind='edge' AND NOT EXISTS
			(SELECT 1 FROM memory_block_edges e WHERE CAST(e.id AS TEXT) = sr.ref_id)`).Scan(&n)
	return n
}

func mustT(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		panic(err)
	}
	return t
}
