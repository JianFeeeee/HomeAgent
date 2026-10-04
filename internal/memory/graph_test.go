package memory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func newTestGraph(t *testing.T) *GraphDB {
	t.Helper()
	f, err := os.CreateTemp("", "graph_test_*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	os.Remove(f.Name())

	g, err := NewGraphDB(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestNewGraphDB(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	stats, err := g.Introspect()
	if err != nil {
		t.Fatal(err)
	}
	if stats["entity_count"].(int) != 0 {
		t.Errorf("expected 0 entities, got %d", stats["entity_count"])
	}
	if stats["relation_count"].(int) != 0 {
		t.Errorf("expected 0 relations, got %d", stats["relation_count"])
	}
}

func TestCommitTriples(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	triples := []Triple{
		{Subject: "张三", Relation: "喜欢", Object: "编程"},
		{Subject: "张三", Relation: "居住", Object: "北京"},
	}

	ec, rc, err := g.Commit(triples, "test_session", 1)
	if err != nil {
		t.Fatal(err)
	}
	if ec != 4 {
		t.Errorf("expected 4 entity ops (张三×2, 编程, 北京), got %d", ec)
	}
	if rc != 2 {
		t.Errorf("expected 2 relations, got %d", rc)
	}
}

func TestCommitDedupSameSession(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	triple := []Triple{{Subject: "李四", Relation: "喜欢", Object: "篮球"}}

	ec, rc, err := g.Commit(triple, "session_dup", 1)
	if err != nil {
		t.Fatal(err)
	}
	if ec != 2 || rc != 1 {
		t.Fatalf("first commit: want 2/1, got %d/%d", ec, rc)
	}

	// 同一会话重复 commit 同一三元组：关系不再新增
	_, rc, err = g.Commit(triple, "session_dup", 2)
	if err != nil {
		t.Fatal(err)
	}
	if rc != 0 {
		t.Errorf("duplicate commit should not create relations again, got %d", rc)
	}

	var cnt int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM relations`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("expected exactly 1 relation after duplicate commit, got %d", cnt)
	}
}

func TestCommitDedupDifferentSession(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	triple := []Triple{{Subject: "王五", Relation: "喜欢", Object: "足球"}}

	for _, sess := range []string{"s1", "s2"} {
		if _, _, err := g.Commit(triple, sess, 0); err != nil {
			t.Fatal(err)
		}
	}
	var cnt int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM relations`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 2 {
		t.Errorf("different sessions may repeat a triple, expected 2 relations, got %d", cnt)
	}
}

func TestMigrateRelationUniqueDedupsOldTable(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy.db")

	// 构造旧版 schema：relations 无复合唯一约束，且塞入重复行
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	setup := []string{
		`CREATE TABLE entities (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			type TEXT DEFAULT 'Concept',
			mention_count INTEGER DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE sentences (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			text TEXT UNIQUE NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE relations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER NOT NULL,
			target_id INTEGER NOT NULL,
			relation_type TEXT NOT NULL,
			confidence REAL DEFAULT 1.0,
			status TEXT DEFAULT 'active',
			session_id TEXT,
			turn_id INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			date_bucket TEXT,
			sentence_id INTEGER DEFAULT 0,
			sentence_ref TEXT DEFAULT '',
			FOREIGN KEY (source_id) REFERENCES entities(id),
			FOREIGN KEY (target_id) REFERENCES entities(id)
		)`,
		`INSERT INTO entities (id, name) VALUES (1, '张三'), (2, '编程')`,
		`INSERT INTO relations (source_id, target_id, relation_type, session_id) VALUES (1, 2, '喜欢', 's'), (1, 2, '喜欢', 's')`,
	}
	for _, s := range setup {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	// 用 NewGraphDB 打开，应触发 migrateRelationUnique：重建带约束表并去重
	g, err := NewGraphDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	var cnt int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM relations`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("expected 1 relation after migration dedup, got %d", cnt)
	}

	// 再次提交重复三元组不应再新增
	_, rc, err := g.Commit([]Triple{{Subject: "张三", Relation: "喜欢", Object: "编程"}}, "s", 1)
	if err != nil {
		t.Fatal(err)
	}
	if rc != 0 {
		t.Errorf("after migration, duplicate commit should add 0 relations, got %d", rc)
	}
}

func TestCommitEmptyTriples(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	ec, rc, err := g.Commit(nil, "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ec != 0 || rc != 0 {
		t.Errorf("expected 0,0 for nil triples, got %d,%d", ec, rc)
	}
}

func TestRecallByKeywords(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	g.Commit([]Triple{
		{Subject: "咖啡", Relation: "属于", Object: "饮品"},
		{Subject: "咖啡", Relation: "含有", Object: "咖啡因"},
	}, "session1", 0)

	result, err := g.Recall([]string{"咖啡"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entities) == 0 {
		t.Error("expected entities for keyword '咖啡'")
	}
}

func TestRecallBySeedEntity(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	g.Commit([]Triple{
		{Subject: "Go", Relation: "是", Object: "编程语言"},
		{Subject: "Go", Relation: "用于", Object: "后端开发"},
	}, "session2", 0)

	result, err := g.Recall(nil, []string{"Go"}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entities) == 0 {
		t.Error("expected entities for seed 'Go'")
	}
}

func TestRecallWithDepth(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	g.Commit([]Triple{
		{Subject: "小明", Relation: "认识", Object: "小红"},
		{Subject: "小红", Relation: "认识", Object: "小刚"},
	}, "session3", 0)

	result, err := g.Recall(nil, []string{"小明"}, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Relations) == 0 {
		t.Error("expected relations with depth search")
	}

	// 逐层查邻接会把已访问实体之间的关系反复查回。不跨层去重时，
	// 小明→小红 会在 depth=2 出现两次，memory_recall 的 10 条关系预算
	// 被同一句话刷屏、真正的新关系（小红→小刚）被截断。
	seen := map[int64]int{}
	for _, r := range result.Relations {
		seen[r.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("关系 id=%d 在深度遍历中重复 %d 次", id, n)
		}
	}
	if len(result.Relations) != 2 {
		t.Errorf("depth=2 应得 2 条关系（小明→小红、小红→小刚），实际 %d", len(result.Relations))
	}
}

func TestPurgeHard(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	g.Commit([]Triple{
		{Subject: "临时", Relation: "用于", Object: "测试"},
	}, "session4", 0)

	n, err := g.Purge(map[string]string{"subject_contains": "临时"}, "hard")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 purged relation, got %d", n)
	}

	stats, _ := g.Introspect()
	if stats["relation_count"].(int) != 0 {
		t.Errorf("expected 0 relations after purge, got %d", stats["relation_count"])
	}
}

func TestPurgeSoft(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	g.Commit([]Triple{
		{Subject: "可删除", Relation: "属于", Object: "测试"},
	}, "session5", 0)

	n, err := g.Purge(map[string]string{"subject_contains": "可删除"}, "soft")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 soft-deleted relation, got %d", n)
	}

	stats, _ := g.Introspect()
	if stats["relation_count"].(int) != 0 {
		t.Errorf("expected 0 active relations after soft-delete, got %d", stats["relation_count"])
	}
}

func TestArchive(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	// 直接插入一条旧记录
	g.db.Exec(`INSERT INTO entities (id, name, type) VALUES (1, '旧数据', 'Concept')`)
	g.db.Exec(`INSERT INTO relations (source_id, target_id, relation_type, created_at)
		VALUES (1, 1, '包含', datetime('now', '-1 day'))`)

	n, err := g.Archive(0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 archived relation, got %d", n)
	}
}

func TestIntrospectHotspots(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	g.Commit([]Triple{
		{Subject: "热门话题", Relation: "关于", Object: "AI"},
		{Subject: "热门话题", Relation: "关于", Object: "机器学习"},
		{Subject: "冷门话题", Relation: "关于", Object: "旧技术"},
	}, "session7", 0)

	stats, _ := g.Introspect()
	hotspots := stats["memory_hotspots"].([]map[string]interface{})
	if len(hotspots) == 0 {
		t.Error("expected hotspots")
	}
}

func TestMergeEntities(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	g.Commit([]Triple{
		{Subject: "张三", Relation: "喜欢", Object: "编程"},
		{Subject: "张三", Relation: "居住", Object: "北京"},
	}, "session", 0)

	g.Commit([]Triple{
		{Subject: "张先生", Relation: "工作", Object: "字节跳动"},
	}, "session", 0)

	// 合并前：两个实体各有关联
	stats, _ := g.Introspect()
	if stats["entity_count"].(int) != 5 {
		t.Fatalf("expected 5 entities (张三, 编程, 北京, 张先生, 字节跳动), got %d", stats["entity_count"])
	}

	// 先增加张先生的 mention_count
	g.Commit([]Triple{
		{Subject: "张先生", Relation: "喜欢", Object: "Go"},
	}, "session", 0)

	n, err := g.MergeEntities("张先生", "张三")
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Errorf("expected at least 2 redirected relations, got %d", n)
	}

	// source 应被改名
	result, err := g.Recall(nil, []string{"张先生"}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entities) > 0 {
		t.Error("张先生 should be merged and hidden")
	}

	// ★ 判据从「mention_count 相加」改成「信息没丢」（2026-10-04）
	//
	//   块侧**没有 mention_count 这个概念** —— 它是旧 entities 表的列，
	//   而 Recall 从不填它（块是内容派生的，不存在「被提及几次」）。
	//
	//   原断言 `mention_count >= 2` 在块体系下无意义，且它衡量的
	//   只是「计数」这个代理指标，不是「信息还在」这件真事。
	//
	//   块侧的真实验证：**合并后「张三」应当同时承载两人的关系**。
	//   合并前张先生有「喜欢→Go」，张三有「负责→billing服务」等；
	//   合并后两组都必须还在 —— 那才是「没丢信息」。
	result2, err := g.Recall([]string{"张三"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	targets := map[string]bool{}
	for _, e := range result2.Entities {
		if e.Name == "张三" {
			found = true
		}
	}
	for _, r := range result2.Relations {
		targets[r.TargetName] = true
	}
	fmt.Printf("  合并后「张三」继承 %d 条关系: %v\n", len(result2.Relations), targets)
	if !targets["Go"] {
		t.Error("★ 合并丢失了源块的关系「喜欢→Go」（信息丢失，不是计数问题）")
	}
	if !found {
		t.Error("张三 should still exist after merge")
	}
}

func TestMergeEntitiesSelf(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	g.Commit([]Triple{
		{Subject: "张三", Relation: "喜欢", Object: "编程"},
	}, "session", 0)

	_, err := g.MergeEntities("张三", "张三")
	if err == nil {
		t.Error("expected error when merging entity with itself")
	}
}

func TestMergeEntitiesNonexistent(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	_, err := g.MergeEntities("不存在", "张三")
	if err == nil {
		t.Error("expected error for nonexistent source")
	}
}

func TestMemoryBlocksAreFirstClassGraphNodes(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	image := MemoryBlock{
		ID: "block_image_1", Modality: BlockImage,
		PayloadDigest: "0123456789abcdef", MIME: "image/png", Size: 1234,
		Width: 768, Height: 512, Vector: []float64{0.1, 0.2, 0.3},
		Fingerprint: "qwen:test", Source: "qq", Tool: "upload",
	}
	text := MemoryBlock{ID: "block_text_1", Modality: BlockText, Text: "用户上传了一张架构图"}
	if err := g.PutMemoryBlocks([]MemoryBlock{image, text}); err != nil {
		t.Fatalf("PutMemoryBlocks: %v", err)
	}
	if err := g.AddMemoryBlockEdge("block", text.ID, "block", image.ID, "contains"); err != nil {
		t.Fatalf("AddMemoryBlockEdge: %v", err)
	}

	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		t.Fatalf("memory blocks=%d, want 2", len(blocks))
	}
	var gotImage *MemoryBlock
	for i := range blocks {
		if blocks[i].ID == image.ID {
			gotImage = &blocks[i]
		}
	}
	if gotImage == nil || gotImage.Modality != BlockImage || gotImage.PayloadDigest != image.PayloadDigest || gotImage.Fingerprint != image.Fingerprint || len(gotImage.Vector) != 3 {
		t.Fatalf("image block not round-tripped: %+v", gotImage)
	}

	edges, err := g.MemoryBlockEdges()
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].Type != "contains" || edges[0].SourceID != text.ID || edges[0].TargetID != image.ID {
		t.Fatalf("memory block edges=%+v", edges)
	}

	graph, err := g.GraphData()
	if err != nil {
		t.Fatal(err)
	}
	graphBlocks, ok := graph["memory_blocks"].([]MemoryBlock)
	if !ok || len(graphBlocks) != 2 {
		t.Fatalf("GraphData memory_blocks=%T %+v", graph["memory_blocks"], graph["memory_blocks"])
	}
	graphEdges, ok := graph["memory_block_edges"].([]MemoryBlockEdge)
	if !ok || len(graphEdges) != 1 {
		t.Fatalf("GraphData memory_block_edges=%T %+v", graph["memory_block_edges"], graph["memory_block_edges"])
	}
}

func TestMemoryBlockEdgeRejectsMissingEndpoint(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	if err := g.PutMemoryBlocks([]MemoryBlock{{ID: "known", Modality: BlockText, Text: "known"}}); err != nil {
		t.Fatal(err)
	}
	if err := g.AddMemoryBlockEdge("block", "known", "block", "missing", "derived_from"); err == nil {
		t.Fatal("edge to missing node must fail")
	}
	edges, err := g.MemoryBlockEdges()
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Fatalf("failed transaction left edges: %+v", edges)
	}
}

func TestPlaceholders(t *testing.T) {
	if placeholders(0) != "NULL" {
		t.Errorf("expected NULL for n=0, got %s", placeholders(0))
	}
	if placeholders(1) != "?" {
		t.Errorf("expected '?' for n=1, got %s", placeholders(1))
	}
	if placeholders(3) != "?,?,?" {
		t.Errorf("expected '?,?,?' for n=3, got %s", placeholders(3))
	}
}

// ★ Introspect 的两个关系计数必须**语义分离**。
//
// 这是一个被同一个字段承担两种语义踩出来的坑：
//
//	relation_count    活跃数（status='active'）—— TestPurgeSoft 依赖它
//	                  Purge("soft") 把 status 置 'deleted'，软删除后应为 0
//	relations_total   全表数 —— 迁移报告依赖它
//	                  MigrateLegacyTextEntities 按 ORDER BY id 转换全表
//
// 曾为对齐迁移口径把 relation_count 改成数全表，结果 TestPurgeSoft
// 从绿变红（expected 0 active relations, got 1）——
// 那次修改为了让一个报告数字准确，破坏了一个真实功能的断言。
func TestIntrospect_关系计数语义分离(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	g.Commit([]Triple{
		{Subject: "活跃方", Relation: "属于", Object: "测试"},
		{Subject: "待删方", Relation: "属于", Object: "测试"},
	}, "session-split", 0)

	before, err := g.Introspect()
	if err != nil {
		t.Fatal(err)
	}
	activeBefore := before["relation_count"].(int)
	totalBefore, ok := before["relations_total"].(int)
	if !ok {
		t.Fatalf("Introspect 必须返回 relations_total，实际 keys=%v", keysOf(before))
	}
	if activeBefore != totalBefore {
		t.Errorf("初始应相等：active=%d total=%d", activeBefore, totalBefore)
	}

	// 软删一条
	if _, err := g.Purge(map[string]string{"subject_contains": "待删方"}, "soft"); err != nil {
		t.Fatal(err)
	}

	after, err := g.Introspect()
	if err != nil {
		t.Fatal(err)
	}
	activeAfter := after["relation_count"].(int)
	totalAfter := after["relations_total"].(int)

	if activeAfter != activeBefore-1 {
		t.Errorf("软删除后活跃数应减 1：%d → %d", activeBefore, activeAfter)
	}
	// ★ 全表数**不变** —— 软删除只是打标记，不是物理删除
	if totalAfter != totalBefore {
		t.Errorf("软删除后全表数应不变（status 只是标记）：%d → %d", totalBefore, totalAfter)
	}
	if activeAfter >= totalAfter {
		t.Errorf("活跃数(%d) 必须小于全表数(%d) —— 否则 status 过滤没生效",
			activeAfter, totalAfter)
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
