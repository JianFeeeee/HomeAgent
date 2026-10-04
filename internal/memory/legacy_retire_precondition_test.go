package memory

import (
	"fmt"
	"os"
	"testing"
)

// ★★ 存量清理的前置判据：清理**之前**必须能证明块侧数据完整。
//
// 清理是不可逆动作（删表），所以判据必须回答：
//
//	① 每个 entity 都有对应的块吗？
//	② 每条 relation 都有对应的边吗？（去重口径）
//	③ 每条 sentence 都有对应的原句块吗？
//	④ 有没有边指向将被删除的节点？（悬空边）
func TestRetire_清理前置_块侧覆盖完整(t *testing.T) {
	path := os.Getenv("RETIRE_DB")
	if path == "" {
		t.Skip("需要 RETIRE_DB（生产库快照副本）")
	}
	g, err := NewGraphDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	// ① entities → legacy-entity 块
	var entCount, entBlocks int
	mustScalar(t, g, `SELECT COUNT(*) FROM entities`, &entCount)
	mustScalar(t, g, `SELECT COUNT(*) FROM memory_blocks WHERE source='legacy-entity'`, &entBlocks)
	fmt.Printf("  ① entities %d → legacy-entity 块 %d %s\n",
		entCount, entBlocks, markEq(entCount, entBlocks))

	// 每个 entity 的名字都能找到一个块（不只数数量）
	missing := missingBlocks(t, g)
	fmt.Printf("     缺块的 entity: %d 个%s\n", len(missing),
		excerpt(missing))
	if len(missing) > 0 {
		t.Errorf("★ 有 %d 个 entity 没有对应块 —— 清理会丢数据", len(missing))
	}

	// ② relations → 关系边
	var relCount, relDistinct int
	mustScalar(t, g, `SELECT COUNT(*) FROM relations`, &relCount)
	mustScalar(t, g, `SELECT COUNT(*) FROM (
		SELECT DISTINCT source_id, target_id, COALESCE(relation_type,'') FROM relations)`,
		&relDistinct)
	mustScalar(t, g, `SELECT COUNT(*) FROM memory_block_edges WHERE edge_type<>'contains'`, &relBlocks)
	fmt.Printf("  ② relations %d（去重后 %d）→ 关系边 %d %s\n",
		relCount, relDistinct, relBlocks, markEq(relDistinct, relBlocks))
	if relDistinct != relBlocks {
		t.Errorf("★ 关系边 %d ≠ 去重后关系数 %d —— 清理会丢边", relBlocks, relDistinct)
	}

	// ③ sentences → 原句块
	//
	// ★ 块 ID 是 sha256(Text[:12])，**SQL 里算不了** ⇒ 在 Go 侧比对。
	//   （SQLite 没有 sha256 函数，尝试 `SentenceBlockID(...)` 会报
	//   "no such function" —— 判据要自己算，别指望 SQL 能做哈希。）
	allBlocks := map[string]bool{}
	bs, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		allBlocks[b.ID] = true
	}
	sents := g.sentenceTexts(t)
	var sentWithBlock int
	var noBlock []string
	for _, txt := range sents {
		if allBlocks[SentenceBlockID(txt)] {
			sentWithBlock++
		} else {
			noBlock = append(noBlock, txt)
		}
	}
	fmt.Printf("  ③ sentences %d → 有原句块的 %d %s%s\n",
		len(sents), sentWithBlock, markEq(len(sents), sentWithBlock),
		excerpt(noBlock))
	if len(noBlock) > 0 {
		t.Errorf("★ %d 条 sentence 没有原句块 —— 清理会丢原句", len(noBlock))
	}

	// ④ 悬空边：指向 sentence 表的边
	var dangling int
	mustScalar(t, g, `SELECT COUNT(*) FROM memory_block_edges e
		WHERE e.source_kind='sentence'
		  AND NOT EXISTS (SELECT 1 FROM sentences s WHERE CAST(s.id AS TEXT)=e.source_id)`,
		&dangling)
	fmt.Printf("  ④ 悬空的 sentence→块 边: %d\n", dangling)
	if dangling > 0 {
		t.Errorf("★ %d 条边指向不存在的 sentence —— 清理后会成为悬空边", dangling)
	}

	// ⑤ 块边的两端都必须存在（通用悬空检查）
	var badEnds int
	mustScalar(t, g, `SELECT COUNT(*) FROM memory_block_edges e
		WHERE (e.source_kind='block' AND NOT EXISTS
			(SELECT 1 FROM memory_blocks b WHERE b.id=e.source_id))
		   OR (e.target_kind='block' AND NOT EXISTS
			(SELECT 1 FROM memory_blocks b WHERE b.id=e.target_id))`, &badEnds)
	fmt.Printf("  ⑤ 端点不存在的块边: %d\n", badEnds)
	if badEnds > 0 {
		t.Errorf("★ %d 条块边的一端不存在", badEnds)
	}
}

var relBlocks int

func mustScalar(t *testing.T, g *GraphDB, q string, dst *int) {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	if err := g.db.QueryRow(q).Scan(dst); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// sentenceTexts 读出 sentences 表的原文。
func (g *GraphDB) sentenceTexts(t *testing.T) []string {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT text FROM sentences`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// missingBlocks 找出没有对应块的 entity
func missingBlocks(t *testing.T, g *GraphDB) []string {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT e.name FROM entities e
		WHERE NOT EXISTS (SELECT 1 FROM memory_blocks b
			WHERE b.source='legacy-entity' AND b.text_content = e.name)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

func markEq(a, b int) string {
	if a == b {
		return "✓"
	}
	return "✘"
}

func excerpt(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	s := " 例: " + ss[0]
	if len(ss) > 1 {
		s += fmt.Sprintf(" …(共 %d)", len(ss))
	}
	return s
}
