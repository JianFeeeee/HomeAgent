package memory

import (
	"path/filepath"
	"testing"
)

// TestDeleteEntityRemovesLiveBlocks 钉住「删除要作用在活图谱上」。
//
// ★ 这条判据来自 2026-10-04 的实证缺陷：
//
//	旧实现在生产库副本上执行后 ——
//	  memory_blocks      2764 → 2764  (Δ0)
//	  memory_block_edges 2379 → 2379  (Δ0)
//	  legacy entities    1294 → 1293  (只删了旧表 1 行)
//	而工具回「已彻底删除实体…及其所有关联关系」。
//
//	危害不是「删不干净」，是**谎报**：模型据此认为内容已消失，
//	而 40+ 条关联边还在，召回继续命中。
//
// ★ 判据设计要点：只断言「块和边真的少了」会漏掉一半缺陷
//
//	（比如只删块不删边、或只删边不删块都能通过一半断言），
//	所以这里**同时**断言块没了、边没了、没有悬空边。
func TestDeleteEntityRemovesLiveBlocks(t *testing.T) {
	g, err := NewGraphDB(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	// 建三块：被删的 + 两个幸存的邻居。
	putBlocks(t, g,
		MemoryBlock{ID: "b_del", Text: "待删实体"},
		MemoryBlock{ID: "b_a", Text: "邻居甲"},
		MemoryBlock{ID: "b_b", Text: "邻居乙"})
	// 被删块 ↔ 两个邻居各一条关系边（双向都建，验证两条都被清）。
	//
	// ★ 注意 Commit 会**顺带创建**主语/宾语块（与 PutMemoryBlocks 建的块
	//   可能同名而成为不同 ID），所以块数不是 3。判据用相对值，
	//   并在末尾单独断言「文本还在」而不是数绝对个数。
	if _, _, err := g.Commit([]Triple{
		{Subject: "待删实体", Relation: "关联", Object: "邻居甲"},
		{Subject: "邻居乙", Relation: "关联", Object: "待删实体"},
	}, "s1", 1); err != nil {
		t.Fatal(err)
	}

	before := countRows(t, g, "memory_blocks")
	// ★ 同名块数不写死：Commit 会为三元组端点**再建一个同名块**
	//   （PutMemoryBlocks 建的与 Commit 建的 ID 不同、文本相同）。
	//   真实库里也确实会同名共存，所以判据只要求「≥1 且删除后清零」。
	dupBefore := countBlocksByText(t, g, "待删实体")
	if dupBefore < 1 {
		t.Fatalf("「待删实体」应有块，实际 %d", dupBefore)
	}

	res, err := g.DeleteEntity("待删实体")
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	// ① 块真的少了（同名的都要清，所以减的是 dupBefore）。
	if got := countRows(t, g, "memory_blocks"); got != before-dupBefore {
		t.Errorf("删除后块数应从 %d 降到 %d（同名 %d 块全清），实际 %d —— 删除没作用在活图谱上",
			before, before-dupBefore, dupBefore, got)
	}
	// ② 关联边真的少了（两条都该走）。
	if res.Edges != 2 {
		t.Errorf("应删掉 2 条关联边，实际 %d", res.Edges)
	}
	// ③ 结果要如实报告，不能报 0 块却返回成功。
	if res.Blocks != dupBefore {
		t.Errorf("DeleteResult.Blocks 应为 %d，实际 %d", dupBefore, res.Blocks)
	}
	// ④ 被删块不能残留在库里。
	if n := countBlocksByText(t, g, "待删实体"); n != 0 {
		t.Errorf("「待删实体」仍残留 %d 个块", n)
	}
	// ⑤ 邻居必须幸存 —— 删除不能误伤。
	for _, keep := range []string{"邻居甲", "邻居乙"} {
		if n := countBlocksByText(t, g, keep); n == 0 {
			t.Errorf("邻居 %q 全部消失 —— 删除误伤了无关块", keep)
		}
	}
	// ⑥ ★ 不能留悬空边：指向已删块的边必须一并清掉。
	//
	//    这条最容易被漏：只删块不删边的话，blocks 里查不到，
	//    但 edges 里还挂着 —— 而召回/仲裁照样会命中那个不存在的 ID。
	var dangling int
	g.db.QueryRow(`SELECT COUNT(*) FROM memory_block_edges e
		WHERE (e.source_kind='block' AND NOT EXISTS
		         (SELECT 1 FROM memory_blocks b WHERE b.id = e.source_id))
		   OR (e.target_kind='block' AND NOT EXISTS
		         (SELECT 1 FROM memory_blocks b WHERE b.id = e.target_id))
		`).Scan(&dangling)
	if dangling != 0 {
		t.Errorf("删除后留了 %d 条悬空边 —— 召回会命中已删块的 ID", dangling)
	}
}

// TestDeleteEntityByExactTextNotSubstring 钉住「精确匹配，不是子串」。
//
// ★ 真实库形态就是子串相撞的（今天的长对话库里同时存在）：
//
//	"admin"（端点块）        ← 要删这个
//	"admin 8861、billing 8499、oauth 8271"（原句块）
//
// 用 LIKE 会把后者一起删掉 —— 那不是「删除这一个实体」，是数据丢失。
func TestDeleteEntityByExactTextNotSubstring(t *testing.T) {
	g, err := NewGraphDB(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	putBlocks(t, g,
		MemoryBlock{ID: "b_admin", Text: "admin"},
		MemoryBlock{ID: "b_sent", Text: "admin 8861、billing 8499、oauth 8271"})

	if _, err := g.DeleteEntity("admin"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	if n := countBlocksByText(t, g, "admin"); n != 0 {
		t.Errorf("「admin」应被删，仍残留 %d 块", n)
	}
	if n := countBlocksByText(t, g, "admin 8861、billing 8499、oauth 8271"); n != 1 {
		t.Errorf("含「admin」的原句块不该被连带删除，实际剩 %d 块 —— 用了子串匹配", n)
	}
}

// TestDeleteEntityMissingBlockIsError 钉住「找不到要报错，不能静默成功」。
//
// ★ 静默成功比报错坏：模型收到「已删除」会认为内容已消失，
//
//	于是重新写入或不再提及 —— 而库里那块从未被动过。
func TestDeleteEntityMissingBlockIsError(t *testing.T) {
	g, err := NewGraphDB(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	putBlocks(t, g, MemoryBlock{ID: "b_keep", Text: "存在的块"})

	res, err := g.DeleteEntity("不存在的块")
	if err == nil {
		t.Fatalf("删除不存在的块必须报错；却返回成功（Blocks=%d Edges=%d）", res.Blocks, res.Edges)
	}
	// 报错不得牵连无关块。
	if n := countBlocksByText(t, g, "存在的块"); n != 1 {
		t.Errorf("失败路径不该动任何块，实际 %q 剩 %d 块", "存在的块", n)
	}
}

// ── 小工具 ──

// putBlocks 写入测试块（补齐 PutMemoryBlocks 要求的 ID/模态）。
func putBlocks(t *testing.T, g *GraphDB, blocks ...MemoryBlock) {
	t.Helper()
	for i := range blocks {
		blocks[i].Modality = BlockText
		if err := g.PutMemoryBlocks([]MemoryBlock{blocks[i]}); err != nil {
			t.Fatalf("写块 %q: %v", blocks[i].ID, err)
		}
	}
}

func countRows(t *testing.T, g *GraphDB, table string) int {
	t.Helper()
	var n int
	if err := g.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func countBlocksByText(t *testing.T, g *GraphDB, text string) int {
	t.Helper()
	var n int
	if err := g.db.QueryRow(
		"SELECT COUNT(*) FROM memory_blocks WHERE text_content = ?", text).Scan(&n); err != nil {
		t.Fatalf("count blocks %q: %v", text, err)
	}
	return n
}

// TestDeleteEntity_LegacyTableHasReferencingRelation 钉住**旧表被引用时也能删**。
//
// ★ 这条判据是 2026-10-05 复核时补的，来源是一次真实的生产库副本实测：
//
//	DeleteEntity("CodeGraph安装任务") → FOREIGN KEY constraint failed
//
// 根因是旧表清理写成了「跨表误用 id」：
//
//	DELETE FROM relations WHERE id IN (SELECT id FROM entities WHERE name = ?)
//
// 子查询给的是 **entities.id**，外层匹配的是 **relations.id** —— 同名不同表。
// 于是被引用的 relation 根本没删，紧接着 DELETE FROM entities 撞上
// relations 的外键（source_id/target_id → entities.id）⇒ **整个事务回滚**。
//
// ★★ 为什么原有的 3 条判据全绿（两次教训叠在一起）：
//
//	① 它们只用 Commit/putBlocks 建**块**体系，旧表恒空 ⇒ 路径不可达。
//	② 我第一版补的判据也只用了 SeedLegacyEntity/SeedLegacyRelation，
//	   而那条 helper 返回的第二个值是 **target_id**、不是 relations.id；
//	   在一个新库上两个 AUTOINCREMENT 序列**恰好对齐**（都是 1、都是 2…），
//	   于是「拿 entity id 当 relation id」碰巧命中正确的那一行 ⇒ 变异测不出来。
//
//	⇒ 必须**显式把两个 id 序列错开**，让 entity.id ≠ relations.id，
//	   这才是生产的真实形态（生产 entities 1294 行 / relations 980 行）。
//	   「测不到」不等于「没问题」——这是本仓反复记的那条纪律。
func TestDeleteEntity_LegacyTableHasReferencingRelation(t *testing.T) {
	g := newTestGraph(t)
	defer g.Close()

	// 块侧：一个同名块（DeleteEntity 以块为权威源）。
	putBlocks(t, g, MemoryBlock{ID: "b_del", Text: "待删实体"})

	// ★ 旧表侧：先灌一批无关关系，把 relations 的 AUTOINCREMENT 推远，
	//   再造目标实体与引用它的关系 —— 保证 entity.id ≠ relations.id。
	for i := 0; i < 20; i++ {
		s := "pre" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if _, err := g.SeedLegacyEntity(s+"-A", "Concept"); err != nil {
			t.Fatalf("SeedLegacyEntity(%s): %v", s, err)
		}
		if _, err := g.SeedLegacyEntity(s+"-B", "Concept"); err != nil {
			t.Fatalf("SeedLegacyEntity(%s): %v", s, err)
		}
		if _, _, err := g.SeedLegacyRelation(s+"-A", s+"-B", "无关", 1.0, "", 0); err != nil {
			t.Fatalf("SeedLegacyRelation(%s): %v", s, err)
		}
	}
	eid, err := g.SeedLegacyEntity("待删实体", "Concept")
	if err != nil {
		t.Fatalf("SeedLegacyEntity: %v", err)
	}
	if _, _, err := g.SeedLegacyRelation("待删实体", "prea-A", "关联", 1.0, "", 0); err != nil {
		t.Fatalf("SeedLegacyRelation: %v", err)
	}

	// ★ 前提自检：两个 id 必须**不相等**，否则本判据是假绿。
	var relID int64
	if err := g.db.QueryRow(
		`SELECT id FROM relations WHERE source_id = ?`, eid).Scan(&relID); err != nil {
		t.Fatal(err)
	}
	if relID == eid {
		t.Fatalf("前提不成立：entity id 与 relation id 相同（%d）——"+
			"「跨表误用 id」的变异会侥幸通过，本判据失去意义", eid)
	}

	// ★ 原实现在这里报 FOREIGN KEY constraint failed。
	if _, err := g.DeleteEntity("待删实体"); err != nil {
		t.Fatalf("旧表有引用关系时也必须能删（旧表清理跨表误用了 id）：%v", err)
	}

	// 旧表同步清干净。
	var ents, rels int
	g.db.QueryRow(`SELECT count(*) FROM entities WHERE name = ?`, "待删实体").Scan(&ents)
	g.db.QueryRow(`SELECT count(*) FROM relations
	  WHERE source_id = ? OR target_id = ?`, eid, eid).Scan(&rels)
	if ents != 0 {
		t.Errorf("旧表 entities 残留 %d 行", ents)
	}
	if rels != 0 {
		t.Errorf("旧表 relations 残留 %d 行（会持续阻塞后续删除）", rels)
	}
}
