package memory

import (
	"path/filepath"
	"testing"
)

// TestHasSameSentenceSiblingFindsRealSibling 钉住「同句兄弟能被找到」。
//
// ★ 这条判据针对 2026-10-04 修的缺陷：SQL 写的是 source_kind='sentence'
//
//	而 contains 边两个端点都是 block（生产库实测 1298 条 contains 边
//	source_id 全部是 blk_src_* 原句块）⇒ 查询恒空 ⇒
//	「新值 + 旧值并列」的结构识别不出来。
//
// ★ 为什么必须单独立判据、而不是靠 arbitrate 的既有测试：
//
//	这个失效**不会让任何现存断言变红** —— 恒空只影响
//	「旧值块恰好带废弃维度名且跨句」的那批数据。
//	所以它是静默失效：测试全绿而守卫已失效。
//	判据必须直接量 hasSameSentenceSibling 本身，
//	否则修没修对都看不出来。
func TestHasSameSentenceSiblingFindsRealSibling(t *testing.T) {
	db := openArbTestDB(t)

	// 一句里拆出两个字段块（真实形态：Commit 会把一句拆成主语|维度=值）。
	// 注意 contains 方向：原句块（source）--contains--> 字段块（target）。
	writeBlockWithSource(t, db, "b_port", "端口=8243", "blk_src_s1")
	writeBlockWithSource(t, db, "b_dep", "停用旧号=8199", "blk_src_s1")

	// ③ 另一句里只有一个块 —— 它没有同句兄弟。
	writeBlockWithSource(t, db, "b_solo", "值班人=小王", "blk_src_s2")

	// 核心断言：有兄弟的那个必须报 true。
	if !hasSameSentenceSibling(db, "blk_src_s1", "b_port") {
		t.Error("blk_src_s1 下有 b_port 与 b_dep 两个块，hasSameSentenceSibling 应为 true")
	}
	// 换个 exclude 也一样（兄弟是相对而言的）。
	if !hasSameSentenceSibling(db, "blk_src_s1", "b_dep") {
		t.Error("排除 b_dep 后 b_port 仍是兄弟，应为 true")
	}
	// 独苗那句必须报 false —— 否则会把所有块都当并列，仲裁彻底失效。
	if hasSameSentenceSibling(db, "blk_src_s2", "b_solo") {
		t.Error("blk_src_s2 只有 b_solo 一个块，不该有兄弟")
	}
	// ★ 不写「exclude 为空时不该把自己算作兄弟」这条断言：
	//
	//	它描述的状态**不可达** —— 调用方恒传块自身 ID
	//	（arbitrate 里 hasSameSentenceSibling(db, b.sameOf, b.hit.Block.ID)），
	//	而 b.sameOf 非空是进入该分支的前提。所以 exclude 为空时
	//	SQL 的 target_id != '' 会把所有块都算作兄弟 —— 那是没人走的分支，
	//	为它改代码等于为不存在的场景写逻辑。
	//
	//	实测确认：exclude="" 时 COUNT(*)=1（自己被算了进去）。
}

// TestSameSentenceOfAndSiblingShareEndpointConvention 钉住两处口径一致。
//
// ★ sameSentenceOf 查「target_id = 字段块」取 source_id 作原句 id；
//
//	hasSameSentenceSibling 查「source_id = 那个 id」。
//	两处若有一处写反（或用了不同的 source_kind），
//	就出现「归属查到了、兄弟却查不到」这种半通不通的状态 ——
//	比全错更难发现，所以必须显式钉住。
func TestSameSentenceOfAndSiblingShareEndpointConvention(t *testing.T) {
	db := openArbTestDB(t)

	writeBlockWithSource(t, db, "b_a", "维度A=值1", "blk_src_x")
	writeBlockWithSource(t, db, "b_b", "维度B=值2", "blk_src_x")

	hits := []BlockHit{
		{Block: MemoryBlock{ID: "b_a", Text: "维度A=值1"}},
		{Block: MemoryBlock{ID: "b_b", Text: "维度B=值2"}},
	}

	// sameSentenceOf 必须把两个块都归到 blk_src_x。
	sentOf := sameSentenceOf(db, hits)
	for _, id := range []string{"b_a", "b_b"} {
		if sentOf[id] != "blk_src_x" {
			t.Errorf("块 %s 的原句应是 blk_src_x，实际 %q", id, sentOf[id])
		}
	}

	// ★ 关键联动：同一个 id 两处都要认。
	for _, id := range []string{"b_a", "b_b"} {
		if !hasSameSentenceSibling(db, sentOf[id], id) {
			t.Errorf("块 %s：sameSentenceOf 说它属于 blk_src_x，"+
				"hasSameSentenceSibling 却说它在 blk_src_x 下无兄弟 —— 两处口径不一致", id)
		}
	}
}

// ── 小工具 ──

func openArbTestDB(t *testing.T) *GraphDB {
	t.Helper()
	db, err := NewGraphDB(filepath.Join(t.TempDir(), "arb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// writeBlockWithSource 写一个块，并建「原句块 --contains--> 该块」的结构边。
//
// ★ 原句块本身也要存在：AddMemoryBlockEdge 会校验两端节点都存在
//
//	（graphNodeExists），只建字段块会报 "does not exist"。
func writeBlockWithSource(t *testing.T, db *GraphDB, blockID, text, sentenceBlockID string) {
	t.Helper()
	if sentenceBlockID != "" {
		// 原句块：ID 即句子块 id，文本用原句本身。
		if err := db.PutMemoryBlocks([]MemoryBlock{
			{ID: sentenceBlockID, Modality: BlockText, Text: "原句：" + text},
		}); err != nil {
			t.Fatalf("写原句块 %s: %v", sentenceBlockID, err)
		}
	}
	if err := db.PutMemoryBlocks([]MemoryBlock{
		{ID: blockID, Modality: BlockText, Text: text},
	}); err != nil {
		t.Fatalf("写块 %s: %v", blockID, err)
	}
	if sentenceBlockID != "" {
		if err := db.AddMemoryBlockEdge("block", sentenceBlockID, "block", blockID, "contains"); err != nil {
			t.Fatalf("建 contains 边 %s→%s: %v", sentenceBlockID, blockID, err)
		}
	}
}
