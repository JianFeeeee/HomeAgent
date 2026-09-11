package document

import (
	"os"
	"testing"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
)

func TestInsertAndQuery(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	doc := &Doc{
		Summary: "用户喜欢喝咖啡",
		Content: "用户提到他每天早上都会喝一杯黑咖啡",
		Tags:    []string{"咖啡", "习惯"},
		Source:  "manual",
	}
	if err := s.Insert(doc); err != nil {
		t.Fatal(err)
	}

	if doc.ID == "" {
		t.Error("doc ID should be auto-generated")
	}

	stats := s.Stats()
	if stats["doc_count"].(int) != 1 {
		t.Errorf("expected 1 doc, got %d", stats["doc_count"])
	}
}

func TestQuery(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_query_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	s.Insert(&Doc{Summary: "咖啡是一种饮品", Content: "咖啡因提神", Source: "manual"})
	s.Insert(&Doc{Summary: "茶叶也有咖啡因", Content: "茶和咖啡都提神", Source: "manual"})
	s.Insert(&Doc{Summary: "今天天气很好", Content: "适合出去散步", Source: "manual"})

	results := s.Query("咖啡", 5)
	if len(results) == 0 {
		t.Fatal("expected results for '咖啡'")
	}

	if results[0].AccessCount <= 0 {
		t.Error("access count should be updated on query")
	}
}

func TestContextToDoc(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_ctx_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	entries := []ContextEntry{
		{Timestamp: time.Now(), Source: "user", Content: "我喜欢编程", Response: "很好"},
		{Timestamp: time.Now(), Source: "user", Content: "特别是Go语言", Response: "Go很棒"},
	}

	doc, err := s.ContextToDoc("test", entries, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if doc == nil {
		t.Fatal("expected non-nil doc")
	}
	if doc.Summary == "" {
		t.Error("summary should not be empty")
	}
	if doc.Content == "" {
		t.Error("content should not be empty")
	}
}

func TestFindColdDocs(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_cold_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	hot := &Doc{Summary: "常用的信息", Content: "经常被查询", Source: "manual"}
	hot.AccessCount = 10
	hot.LastAccess = time.Now()
	s.Insert(hot)

	cold := &Doc{Summary: "很久没用的信息", Content: "几乎不被访问", Source: "manual"}
	s.Insert(cold)
	// Insert 会重置 LastAccess，手动改为过去的
	cold.LastAccess = time.Now().Add(-100 * time.Hour)
	cold.AccessCount = 1

	// 应该只找到 cold（72h 前未访问且访问 ≤ 2）
	coldDocs := s.FindColdDocs(72*time.Hour, 2)
	if len(coldDocs) != 1 {
		t.Fatalf("expected 1 cold doc, got %d", len(coldDocs))
	}
	if coldDocs[0].Summary != "很久没用的信息" {
		t.Errorf("expected cold doc, got %s", coldDocs[0].Summary)
	}
}

func TestRecentDocs(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_recent_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	s.Insert(&Doc{Summary: "第一条", Content: "a", Source: "manual"})
	time.Sleep(time.Millisecond)
	s.Insert(&Doc{Summary: "第二条", Content: "b", Source: "manual"})

	recent := s.RecentDocs(1)
	if len(recent) != 1 {
		t.Fatalf("expected 1 recent doc, got %d", len(recent))
	}
	if recent[0].Summary != "第二条" {
		t.Errorf("expected newest doc, got %s", recent[0].Summary)
	}
}

func TestReindex(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_reindex_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	s.Insert(&Doc{Summary: "测试重索引", Content: "验证索引重建", Source: "manual"})
	s.Reindex()

	results := s.Query("重索引", 5)
	if len(results) == 0 {
		t.Error("reindex should preserve searchability")
	}
}

func TestSummarizeEntries(t *testing.T) {
	entries := []ContextEntry{
		{Source: "user", Content: "今天天气如何"},
		{Source: "user", Content: "明天会下雨吗"},
	}
	summary := summarizeEntries(entries, func(s string) string { return s }, nil, nil)
	if summary == "" {
		t.Error("summary should not be empty")
	}
	if !contains(summary, "2") {
		t.Errorf("summary should mention count, got: %s", summary)
	}
}

func TestExtractKeywords(t *testing.T) {
	kws := memory.ExtractKeywords("今天天气很好")
	if len(kws) == 0 {
		t.Error("should extract keywords from Chinese text")
	}
}

func TestExtractTags(t *testing.T) {
	entries := []ContextEntry{
		{Content: "我喜欢喝咖啡和编程"},
	}
	tags := extractTags(entries, func(s string) string { return s }, nil, nil)
	if len(tags) == 0 {
		t.Error("should extract tags")
	}
}

func TestInsertEmptyDoc(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_empty_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	doc := &Doc{Summary: "", Content: "", Source: "manual"}
	if err := s.Insert(doc); err != nil {
		t.Fatal(err)
	}
	if doc.ID == "" {
		t.Error("doc ID should be generated even for empty content")
	}
}

func TestPersistence(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_persist_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// 写
	s1 := NewStore(dir, memory.TokenizeWords)
	s1.Start()
	s1.Insert(&Doc{Summary: "持久化测试", Content: "应该被保存到磁盘", Source: "manual"})
	s1.Stop()

	// 读
	s2 := NewStore(dir, memory.TokenizeWords)
	s2.Start()
	defer s2.Stop()

	stats := s2.Stats()
	if stats["doc_count"].(int) != 1 {
		t.Errorf("expected 1 doc after reload, got %d", stats["doc_count"])
	}

	results := s2.Query("持久化", 5)
	if len(results) == 0 {
		t.Error("search should work after reload")
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestFlushNoDirty(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_flush_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()

	// 不插任何文档，flush 不应报错
	s.Stop()
}

func TestRemove(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_remove_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	s.Insert(&Doc{Summary: "会被删除", Content: "a", Source: "manual"})
	s.Insert(&Doc{Summary: "会保留", Content: "b", Source: "manual"})

	// 删除前应该有 2 个
	stats := s.Stats()
	if stats["doc_count"].(int) != 2 {
		t.Fatalf("expected 2 docs before remove, got %d", stats["doc_count"])
	}

	// 遍历找到 "会被删除" 的 ID
	var rmID string
	for _, d := range s.docs {
		if d.Summary == "会被删除" {
			rmID = d.ID
			break
		}
	}
	if rmID == "" {
		t.Fatal("could not find test doc")
	}

	s.Remove(rmID)

	stats = s.Stats()
	if stats["doc_count"].(int) != 1 {
		t.Errorf("expected 1 doc after remove, got %d", stats["doc_count"])
	}

	// 搜索不应再找到
	results := s.Query("删除", 5)
	if len(results) > 0 {
		t.Error("removed doc should not appear in search results")
	}
}

func TestRemoveNonexistent(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_rm_nonexist_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	s.Insert(&Doc{Summary: "一个文档", Content: "x", Source: "manual"})

	// 删除不存在的 ID 不应 panic
	s.Remove("nonexistent_id")

	stats := s.Stats()
	if stats["doc_count"].(int) != 1 {
		t.Errorf("expected 1 doc after remove nonexistent, got %d", stats["doc_count"])
	}
}

func TestSummarizeEntriesWithToolCleanFn(t *testing.T) {
	tests := []struct {
		name        string
		toolCleanFn func(name, output string) string
		wantTopics  []string
		notTopics   []string
	}{
		{
			name:        "nil toolCleanFn uses raw output",
			toolCleanFn: nil,
			wantTopics:  []string{"手机", "电脑"},
			notTopics:   nil,
		},
		{
			name: "NoMemory returns empty skips tool output",
			toolCleanFn: func(name, output string) string {
				return ""
			},
			wantTopics: nil,
			notTopics:  []string{"手机", "电脑"},
		},
		{
			name: "Cleaner applies filter",
			toolCleanFn: func(name, output string) string {
				return "电脑 编程"
			},
			wantTopics: []string{"电脑", "编程"},
			notTopics:  nil,
		},
	}

	entry := ContextEntry{
		Content: "今天天气",
		ToolResults: []ToolResultItem{
			{Name: "test_tool", Output: "手机 电脑"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			summary := summarizeEntries([]ContextEntry{entry}, func(s string) string { return s }, tc.toolCleanFn, nil)
			for _, w := range tc.wantTopics {
				if !contains(summary, w) {
					t.Errorf("summary should contain %q, got: %s", w, summary)
				}
			}
			for _, n := range tc.notTopics {
				if contains(summary, n) {
					t.Errorf("summary should NOT contain %q, got: %s", n, summary)
				}
			}
		})
	}
}

func TestExtractTagsWithToolCleanFn(t *testing.T) {
	entries := []ContextEntry{
		{
			Content: "对话",
			ToolResults: []ToolResultItem{
				{Name: "search", Output: "编程和咖啡"},
			},
		},
	}

	// toolCleanFn 返回 "" → NoMemory，工具输出被跳过
	tagsSkip := extractTags(entries, func(s string) string { return s }, func(name, output string) string { return "" }, nil)
	for _, tag := range tagsSkip {
		if tag == "编程" || tag == "咖啡" {
			t.Errorf("NoMemory tool should not contribute keywords, got tag: %s", tag)
		}
	}

	// toolCleanFn 返回清洗文本 → 用清洗后内容提取关键词
	tagsClean := extractTags(entries, func(s string) string { return s }, func(name, output string) string { return "咖啡 编程" }, nil)
	found := false
	for _, tag := range tagsClean {
		if tag == "编程" {
			found = true
		}
	}
	if !found {
		t.Errorf("cleaner output keywords should appear in tags, got: %v", tagsClean)
	}
}

func TestContextToDocContentPreservesRawToolOutput(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_toolclean_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	entries := []ContextEntry{
		{
			Timestamp: time.Now(),
			Source:    "user",
			Content:   "查天气",
			ToolResults: []ToolResultItem{
				{Name: "weather", Output: "{\"temp\": 25}"},
			},
		},
	}

	// toolCleanFn 返回清洗文本，但 Content 必须保留原始输出
	cleaner := func(name, output string) string {
		return "天气 温度"
	}
	doc, err := s.ContextToDoc("test", entries, nil, nil, cleaner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(doc.Content, "{\"temp\": 25}") {
		t.Errorf("Content should preserve raw tool output, got: %s", doc.Content)
	}
	if doc.Summary == "" {
		t.Error("summary should not be empty")
	}
}
