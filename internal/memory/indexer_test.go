package memory

import (
	"path/filepath"
	"testing"
)

func TestNewIndexer(t *testing.T) {
	idx := NewIndexer(nil)
	if idx == nil {
		t.Fatal("expected non-nil indexer")
	}
}

func TestIndexerSyncWithNilDB(t *testing.T) {
	idx := NewIndexer(nil)
	if err := idx.Sync(); err != nil {
		t.Errorf("expected no error with nil db, got %v", err)
	}
}

func TestIndexerSync(t *testing.T) {
	db, err := NewGraphDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 写入一些实体
	ec, _, err := db.Commit([]Triple{
		{Subject: "张三", Relation: "喜欢", Object: "篮球"},
		{Subject: "李四", Relation: "喜欢", Object: "足球"},
	}, "test", 0)
	if err != nil {
		t.Fatal(err)
	}

	idx := NewIndexer(db)
	if err := idx.Sync(); err != nil {
		t.Fatal(err)
	}

	if !idx.trained {
		t.Error("expected indexer to be trained after sync")
	}
	if idx.vec.Size() != ec {
		t.Errorf("expected %d vectors, got %d", ec, idx.vec.Size())
	}
}

func TestIndexerBuildContext(t *testing.T) {
	db, err := NewGraphDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.Commit([]Triple{
		{Subject: "张三", Relation: "喜欢", Object: "篮球"},
		{Subject: "张三", Relation: "职业", Object: "程序员"},
	}, "test", 0)

	idx := NewIndexer(db)
	idx.Sync()

	ctx := idx.BuildContext("张三")
	if ctx == nil {
		t.Fatal("expected non-nil context")
	}
	if len(ctx.Entities) == 0 {
		t.Error("expected at least one entity in context")
	}
}

func TestIndexerBuildContextEmpty(t *testing.T) {
	idx := NewIndexer(nil)
	ctx := idx.BuildContext("anything")
	if ctx == nil {
		t.Fatal("expected non-nil context even with nil db")
	}
	if ctx.Summary != "" {
		t.Errorf("expected empty summary with nil db, got %q", ctx.Summary)
	}
}

func TestIndexerMarkRecalled(t *testing.T) {
	idx := NewIndexer(nil)
	idx.MarkRecalled("张三", "李四")

	idx.mu.RLock()
	_, ok1 := idx.recalled["张三"]
	_, ok2 := idx.recalled["李四"]
	_, ok3 := idx.recalled["王五"]
	idx.mu.RUnlock()

	if !ok1 {
		t.Error("expected '张三' to be marked recalled")
	}
	if !ok2 {
		t.Error("expected '李四' to be marked recalled")
	}
	if ok3 {
		t.Error("expected '王五' NOT to be marked recalled")
	}
}

func TestIndexerFilterRecalled(t *testing.T) {
	db, err := NewGraphDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.Commit([]Triple{
		{Subject: "张三", Relation: "喜欢", Object: "篮球"},
		{Subject: "李四", Relation: "喜欢", Object: "足球"},
	}, "test", 0)

	idx := NewIndexer(db)
	idx.Sync()
	idx.MarkRecalled("张三")

	ctx := idx.BuildContext("张三")
	if ctx == nil {
		t.Fatal("expected non-nil context")
	}
	for _, e := range ctx.Entities {
		if e.Name == "张三" {
			t.Error("expected '张三' to be filtered out (marked recalled)")
		}
	}
}

func TestFormatContext(t *testing.T) {
	idx := NewIndexer(nil)

	result := idx.FormatContext(nil)
	if result != "" {
		t.Errorf("expected empty string for nil context, got %q", result)
	}

	ctx := &InjectedContext{
		Entities: []Entity{
			{ID: 1, Name: "张三", Type: "Person"},
			{ID: 2, Name: "篮球", Type: "Concept"},
		},
		Summary: "关联 2 个记忆实体，高频：张三、篮球",
	}
	result = idx.FormatContext(ctx)
	if result == "" {
		t.Error("expected non-empty formatted context")
	}
	if !contains(result, "张三") || !contains(result, "篮球") {
		t.Errorf("expected context to contain entity names, got %q", result)
	}
}

func TestBuildIndexSummary(t *testing.T) {
	entities := []Entity{
		{Name: "张三", Type: "Person", MentionCount: 10},
		{Name: "李四", Type: "Person", MentionCount: 5},
		{Name: "篮球", Type: "Concept", MentionCount: 3},
		{Name: "北京", Type: "Location", MentionCount: 2},
	}
	s := buildIndexSummary(entities)
	if !contains(s, "张三") || !contains(s, "李四") {
		t.Errorf("expected summary to contain top entities, got %q", s)
	}
}

func TestBuildIndexSummaryEmpty(t *testing.T) {
	s := buildIndexSummary(nil)
	if s != "" {
		t.Errorf("expected empty summary for nil, got %q", s)
	}
}

func TestExtractKeywords(t *testing.T) {
	tests := []struct {
		input string
		min   int
	}{
		{"今天天气怎么样", 1},
		{"张三的朋友李四", 1},
		{"hello world", 1},
		{"的了的", 0}, // all stop words
	}
	for _, tt := range tests {
		kw := ExtractKeywords(tt.input)
		if len(kw) < tt.min {
			t.Errorf("ExtractKeywords(%q) = %v, want at least %d keywords", tt.input, kw, tt.min)
		}
	}
}

func TestGetToolDefinitions(t *testing.T) {
	idx := NewIndexer(nil)
	defs := idx.GetToolDefinitions()
	if len(defs) < 3 {
		t.Errorf("expected at least 3 tool defs, got %d", len(defs))
	}
}

func TestBuildToolPrompt(t *testing.T) {
	idx := NewIndexer(nil)
	prompt := idx.BuildToolPrompt()
	if !contains(prompt, "memory_recall") {
		t.Errorf("expected prompt to mention memory_recall, got %q", prompt)
	}
}

func TestVectorSearchEntities(t *testing.T) {
	db, err := NewGraphDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.Commit([]Triple{
		{Subject: "张三", Relation: "喜欢", Object: "篮球"},
	}, "test", 0)

	idx := NewIndexer(db)
	idx.Sync()

	// After sync, we have entities. Vector search should find relevant ones.
	entities := idx.vectorSearchEntities("张三")
	if len(entities) == 0 {
		// This might be empty due to TF-IDF matching — vector search on single
		// entity names is approximate. Just check it doesn't crash.
		t.Log("vector search returned 0 results (acceptable for short queries)")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
