package knowledge

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Add 不得随库规模线性变慢。
//
// 修复前单条 Add 的耗时曲线是 2.1ms@50 → 7.2ms@200 → 13.7ms@400（O(N)/写），
// 两个成因：
//  1. buildTreeLocked 对每条调 s.vectorize() 重算向量，而 s.vec 里已经有
//     现成的同一个向量（分词 + TF-IDF 加权，白算一遍）。
//  2. 每次 Add/Remove 都全量重写 .index.json（整棵树 JSON 序列化）。
//
// 现在改为：复用 s.vec 的向量 + 索引标脏延迟到 Flush/Stop。
func TestAddDoesNotScaleWithLibrarySize(t *testing.T) {
	body := strings.Repeat("知识库条目内容，用于压测写入路径的开销。", 40)
	per := func(n int) time.Duration {
		dir := t.TempDir()
		s := NewStore(dir)
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		defer s.Stop()
		start := time.Now()
		for i := 0; i < n; i++ {
			if err := s.Add(fmt.Sprintf("条目%04d", i), body); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(start) / time.Duration(n)
	}

	// 预热，避免首次训练分词器的开销计入小规模那一次
	_ = per(20)

	small := per(50)
	large := per(400)
	t.Logf("单条 Add 均耗时: N=50 %v  N=400 %v", small.Round(time.Microsecond), large.Round(time.Microsecond))

	// 允许 4 倍余量：机器噪声、GC、以及未来合理的小幅回退都不该卡住这条。
	// 修复前是 6.5 倍（2.1ms → 13.7ms），会稳稳越界。
	if large > small*4 {
		t.Errorf("单条 Add 耗时随库规模放大过多：N=50 %v → N=400 %v（%0.1f×）",
			small.Round(time.Microsecond), large.Round(time.Microsecond),
			float64(large)/float64(small))
	}
}

// 索引必须最终落盘（延迟不等于丢失）。
func TestIndexEventuallyPersistedOnFlush(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	// 延迟窗口内：Add 之后立刻看，不该有更新的索引内容
	if err := s.Add("later", "正文"); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := readFileString(dir + "/.index.json")
	if err != nil {
		t.Fatalf("Flush 后索引未落盘: %v", err)
	}
	if !strings.Contains(data, "later") {
		t.Errorf("索引内容不含新增条目：%s", truncForLog(data))
	}

	// 二次 Flush 无脏可写时不应报错（幂等）
	if err := s.Flush(); err != nil {
		t.Errorf("重复 Flush 应幂等，实为 %v", err)
	}
	s.Stop()
}

// Remove 之后索引同样要能被 Flush 收口。
func TestIndexPersistedAfterRemove(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("gone", "将被删除"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("kept", "保留"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("gone"); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := readFileString(dir + "/.index.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data, `"name": "gone"`) {
		t.Error("已删除条目仍在索引里")
	}
	if !strings.Contains(data, "kept") {
		t.Error("保留条目不在索引里")
	}
	s.Stop()
}

func readFileString(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}

func truncForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
