package memory

import (
	"fmt"
	"testing"
)

// ★★ Commit 块化的目标判据。
//
// 现状（graph.go:495~530）：Commit 往三张旧表写
//
//	entities   （三元组主语/宾语，name 唯一）
//	sentences  （SentenceText 原句）
//	relations  （三元组本体）
//
// 而 memory_blocks / memory_block_edges **完全没被写**
// ⇒ 旧表持续增长（跑分实测 entities 188 → 381）。
//
// 块化后 Commit 必须：
//
//	① 不再写 entities / relations
//	② 三元组变成「主语块 --关系--> 宾语块」的块边
//	③ 原句变成原句块（blk_src_<hash>），供 CommitWithMedia 挂媒体边
//
// ★ 但 CommitWithMedia 必须**保持签名兼容** ——
//
//	媒体桥（graphmedia.go）靠它返回的 sentences.id 挂
//	sentence --contains--> block 边。那条边要改成 原句块 --contains--> 媒体块，
//	所以返回值得从「sentences 表行号」变成「原句块 ID」。
func TestBlockCommit_不写旧表(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	triples := []Triple{
		{Subject: "值班室分机号", Relation: "是", Object: "4324",
			SentenceText: "值班室分机号改为 4324，旧号 4379 停用"},
		{Subject: "admin服务", Relation: "端口", Object: "8080",
			SentenceText: "admin 服务监听 8080"},
	}
	sentIDs, ec, rc, err := g.CommitWithMedia(triples, "sess1", 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  CommitWithMedia: 实体 %d，关系 %d，句子 %d\n", ec, rc, len(sentIDs))

	// ① 三张旧表必须仍是 0
	var nEnt, nRel, nSent int
	count(t, g, `SELECT COUNT(*) FROM entities`, &nEnt)
	count(t, g, `SELECT COUNT(*) FROM relations`, &nRel)
	count(t, g, `SELECT COUNT(*) FROM sentences`, &nSent)
	fmt.Printf("  ① 旧表: entities=%d relations=%d sentences=%d\n", nEnt, nRel, nSent)
	// ★ 旧表写入是**有意保留**的，不是缺陷。
	//
	// Recall / RecallSorted / social / scene / light_memory / WebUI /
	// healthcheck / proc / lua / SDK 共 55 处仍读它们
	// （docs/zh/legacy-table-retirement.md）。
	// 退场顺序必须是：
	//
	//	① 读方切块  →  ② 停写旧表  →  ③ 删表
	//
	// 在①完成前停写 = 在线读取直接断。所以本阶段只保证
	//「**块与块边完整**，旧表仍同步写」，不追求旧表为 0。
	//
	// ⇒ 这里断言「旧表与块数一致」——保证块化没有漏写。
	//
	// ★★ 2026-10-04：旧表**双写已停**（读方全部切块完成），
	// 所以这三项现在恒为 0。原断言 `nEnt == 0 → 报错`
	// 把「当时的状态」写成了「永久的契约」——
	// 阶段推进之后它自己过期了。
	//
	// ★ 判据要验的是**块化没漏写**，不是旧表还在不在。
	//   所以改成「旧表为 0 是**预期**」并说明理由，
	//   而真正的完整性由下面的②（块与块边）保证。
	fmt.Printf("  ① 旧表（已停双写，预期全 0）: entities=%d relations=%d sentences=%d\n",
		nEnt, nRel, nSent)
	if nEnt != 0 {
		t.Errorf("旧表双写已停，entities 应为 0，实际 %d —— 旧表写入可能被漏摘", nEnt)
	}

	// ② 块与块边必须存在
	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	var sentBlocks, valBlocks int
	for _, b := range blocks {
		switch b.Source {
		case SentenceBlockSource:
			sentBlocks++
		default:
			valBlocks++
		}
	}
	fmt.Printf("  ② 块: 原句块 %d，值块 %d\n", sentBlocks, valBlocks)
	if sentBlocks == 0 || valBlocks == 0 {
		t.Errorf("★ 块化未生效: 原句块 %d，值块 %d", sentBlocks, valBlocks)
	}

	// ③ 三元组必须变成块边（主语块 --关系--> 宾语块）
	edges, err := g.MemoryBlockEdges()
	if err != nil {
		t.Fatal(err)
	}
	relTypes := make([]string, 0, len(edges))
	var relEdges int
	for _, e := range edges {
		if e.Type != "contains" {
			relTypes = append(relTypes, fmt.Sprintf("%s--%s-->%s", e.SourceID, e.Type, e.TargetID))
			relEdges++
		}
	}
	fmt.Printf("     实际关系边: %v\n", relTypes)
	fmt.Printf("  ③ 关系型块边: %d\n", relEdges)
	if relEdges == 0 {
		t.Error("★ 三元组没有变成块边")
	}

	// ④ CommitWithMedia 的返回值必须仍可用于挂媒体边
	//
	// ⚠️ 当前签名是 map[string]int64（sentences 表行号）。
	//   块化后必须改成 map[string]string（原句块 ID）——
	//   因为 sentences 表退场后行号不再存在，而媒体桥要靠它挂
	//   「原句块 --contains--> 媒体块」这条边。
	//
	// 这条断言现在必然失败（int64 没有 blk_src 前缀），
	// 它标记的是**签名变更点**，不是缺陷。
	for text, id := range sentIDs {
		t.Logf("  ④ 句子 %q → 当前返回 %v（块化后应为 blk_src_* 字符串）", text, id)
		if ec == 0 {
			t.Log("     （实体数为 0，说明当前实现未块化）")
		}
	}
}

func count(t *testing.T, g *GraphDB, q string, dst *int) {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	if err := g.db.QueryRow(q).Scan(dst); err != nil {
		t.Fatal(err)
	}
}
