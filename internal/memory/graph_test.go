package memory

import (
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
		{Subject: "甲", Relation: "认识", Object: "乙"},
		{Subject: "乙", Relation: "认识", Object: "丙"},
	}, "session3", 0)

	result, err := g.Recall(nil, []string{"甲"}, 2, "")
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
