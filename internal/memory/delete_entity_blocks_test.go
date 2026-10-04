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
