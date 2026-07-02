package knowledge

import (
	"os"
	"testing"
)

func TestNewStore(t *testing.T) {
	dir, err := os.MkdirTemp("", "know_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if s == nil {
		t.Fatal("store should not be nil")
	}
}

func TestAddAndSearch(t *testing.T) {
	dir, err := os.MkdirTemp("", "know_add_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir)
	s.Start()
	defer s.Stop()

	if err := s.Add("coffee", "咖啡是一种饮品，含有咖啡因"); err != nil {
		t.Fatal(err)
	}

	results := s.Search("咖啡", 5)
	if len(results) == 0 {
		t.Fatal("expected results for '咖啡'")
	}
}

func TestList(t *testing.T) {
	dir, err := os.MkdirTemp("", "know_list_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir)
	s.Start()
	defer s.Stop()

	s.Add("topic1", "内容一")
	s.Add("topic2", "内容二")

	list := s.List()
	if len(list) != 2 {
		t.Errorf("expected 2 items, got %d", len(list))
	}
}

func TestRemove(t *testing.T) {
	dir, err := os.MkdirTemp("", "know_rm_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir)
	s.Start()
	defer s.Stop()

	s.Add("test", "测试内容")
	if err := s.Remove("test"); err != nil {
		t.Fatal(err)
	}

	list := s.List()
	if len(list) != 0 {
		t.Errorf("expected 0 items after remove, got %d", len(list))
	}
}

func TestRemoveNotFound(t *testing.T) {
	dir, err := os.MkdirTemp("", "know_notfound_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir)
	s.Start()
	defer s.Stop()

	if err := s.Remove("nonexistent"); err != nil {
		t.Errorf("remove nonexistent should not error, got: %v", err)
	}
}

func TestStats(t *testing.T) {
	dir, err := os.MkdirTemp("", "know_stats_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir)
	s.Start()
	defer s.Stop()

	s.Add("a", "内容A")
	s.Add("b", "内容B")

	stats := s.Stats()
	if stats["knowledge_count"].(int) != 2 {
		t.Errorf("expected knowledge_count 2, got %v", stats["knowledge_count"])
	}
	if stats["vector_count"] == nil {
		t.Error("expected vector_count in stats")
	}
}

func TestSearchNoMatch(t *testing.T) {
	dir, err := os.MkdirTemp("", "know_nomatch_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir)
	s.Start()
	defer s.Stop()

	s.Add("math", "加减乘除是基本运算")
	// "电电电电电" 中的字符 "电" 不在文档 "math 加减乘除是基本运算" 的任意 unigram 中
	results := s.Search("电电电电电", 5)
	if len(results) != 0 {
		t.Errorf("expected 0 results for non-matching query, got %d", len(results))
	}
}
