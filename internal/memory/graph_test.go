package memory

import (
	"database/sql"
	"path/filepath"
	"os"
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

	// target 的 mention_count 应合并
	// 验证 target 还存在（seedEntities 精确查找）
	result2, err := g.Recall([]string{"张三"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range result2.Entities {
		if e.Name == "张三" {
			found = true
			if e.MentionCount < 2 {
				t.Errorf("expected 张三 mention_count >= 2 after merge, got %d", e.MentionCount)
			}
			break
		}
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
