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

// 覆盖同名条目必须把旧向量摘掉，而不是再插一份。
//
// 这条是从一次真实的知识库更新里发现的：在线上实例更新一个已有条目后，
// knowledge_count=32 但 vector_count=33 ——多出来的那一条是上一版的副本。
// 成因是 vector.Store.Insert 为追加语义（s.docs = append + index.Add），不按 id 去重。
// 危害不在于多占一份内存：检索可能命中**已被替换掉的旧内容**。
func TestAddOverwriteReplacesVector(t *testing.T) {
	dir, err := os.MkdirTemp("", "know_overwrite_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir)
	s.Start()
	defer s.Stop()

	if err := s.Add("recent", "第一版内容：旧的多模态描述式索引"); err != nil {
		t.Fatal(err)
	}
	if got := s.Stats()["vector_count"].(int); got != 1 {
		t.Fatalf("首次写入后 vector_count 应为 1，实为 %d", got)
	}

	if err := s.Add("recent", "第二版内容：媒体已成为图记忆的一等节点"); err != nil {
		t.Fatal(err)
	}

	if n := s.Stats()["knowledge_count"].(int); n != 1 {
		t.Fatalf("同名覆盖后 knowledge_count 应为 1，实为 %d", n)
	}
	if n := s.Stats()["vector_count"].(int); n != 1 {
		t.Fatalf("同名覆盖后 vector_count 应为 1（多了就是旧版没被摘掉），实为 %d", n)
	}

	// 目录里也只应有一份内容，且是新的那份
	b, err := os.ReadFile(dir + "/recent/content.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "第二版内容：媒体已成为图记忆的一等节点" {
		t.Fatalf("content.md 未被新内容覆盖，实为 %q", string(b))
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

// TestRemoveNotFound 原断言"删除不存在的条目不应报错"，该契约已作废：
// webui 的 DELETE 处理器把 error 映射成 404，说明调用方本来就期望 ErrNotFound；
// 宽松版本只会让工具层对一次什么都没删的操作回报"已删除"。
// 新契约见 hardening_test.go 的 TestRemoveNotFound。

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
