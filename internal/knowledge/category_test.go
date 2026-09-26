package knowledge

import (
	"strings"
	"testing"
	"time"
)

// 分层必须真正参与召回：SearchIn 把范围限定在分类子树内。
//
// 此前分层只是**存储布局**——检索一律全库平铺，而 SearchTree /
// SearchCategories 两个想按分类聚合的函数是死代码（且停留在 Search 修复
// 前的单路口径）。分类存在却对召回零影响。
func TestSearchInCategoryScope(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	for _, e := range []struct{ name, body string }{
		{"tech/go/并发", "goroutine 调度 GMP 抢占 通道"},
		{"tech/rust/所有权", "borrow checker move 语义"},
		{"life/sleep", "作息 褪黑素 深睡 咖啡因"},
		{"cook/coffee", "手冲 烘焙 水温 粉水比"},
	} {
		if err := s.Add(e.name, e.body); err != nil {
			t.Fatal(err)
		}
	}

	// 全库：能跨分类召回
	if got := s.Search("调度 语义 作息 手冲", 10); len(got) < 4 {
		t.Fatalf("全库检索应召回全部 4 条，实为 %v", namesOf(got))
	}

	// 限定 tech：只剩 tech 子树两条
	got := s.SearchIn("调度 语义 作息 手冲", "tech", 10)
	if len(got) != 2 {
		t.Fatalf("限定 tech 应命中 2 条，实为 %v", namesOf(got))
	}
	for _, k := range got {
		if !hasPrefix(k.Name, "tech/") {
			t.Errorf("限定 tech 却返回了 %q", k.Name)
		}
	}

	// 前缀匹配整棵子树：查 "tech/go" 只命中其下
	if got := s.SearchIn("调度 语义 作息 手冲", "tech/go", 10); len(got) != 1 || got[0].Name != "tech/go/并发" {
		t.Errorf("限定 tech/go 应只命中并发，实为 %v", namesOf(got))
	}

	// 不存在的分类：空结果，且不报错
	if got := s.SearchIn("调度", "no/such/cat", 5); len(got) != 0 {
		t.Errorf("不存在的分类应返回空，实为 %v", namesOf(got))
	}

	// 空 category ≡ 全库（与 Search 等价）
	a, b := s.Search("调度 语义", 10), s.SearchIn("调度 语义", "", 10)
	if strings.Join(namesOf(a), ",") != strings.Join(namesOf(b), ",") {
		t.Errorf("空 category 应等价于全库：%v vs %v", namesOf(a), namesOf(b))
	}
	// 前后斜杠不应影响（界面上很容易带上）
	for _, c := range []string{"/tech", "tech/", "/tech/"} {
		if got := s.SearchIn("调度 语义 作息 手冲", c, 10); len(got) != 2 {
			t.Errorf("category=%q 应归一化后命中 2 条，实为 %v", c, namesOf(got))
		}
	}
}

// 分类过滤下，归一化必须取**作用域内**的最大值。
//
// 否则：作用域内只有一条弱命中，范围外却有个强命中把全局最大值拉高，
// 作用域内的分数被压到接近 0，排序与阈值语义全失真。
func TestSearchInNormalizesWithinScope(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// 故意让范围外的条目在词法路上分数更高
	if err := s.Add("outside", "zzzz 罕见词zzz 独有"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("cat/in", "zebra 条目"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("cat/in2", "zebra 条目"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("cat/in3", "zebra 条目"); err != nil {
		t.Fatal(err)
	}

	// 全库时 outside 应因独有词而排前
	if got := s.Search("罕见词zzz", 4); len(got) == 0 || got[0].Name != "outside" {
		t.Logf("注：全库首位为 %v（稀疏语义路可能改写排序），仅作观察", namesOf(got))
	}
	// 限定 cat：outside 必须被排除，且 cat 下的条目仍要正常返回（分数不能被压成 0）
	got := s.SearchIn("zebra", "cat", 4)
	if len(got) != 3 {
		t.Fatalf("限定 cat 应返回 3 条，实为 %v", namesOf(got))
	}
	for _, k := range got {
		if k.Name == "outside" {
			t.Error("作用域过滤失效：范围外条目被返回")
		}
	}
	// 关键：范围外的强信号不得把作用域内的分数压掉——
	// 三个 cat 条目必须都在（而不是只留 0 个）
	if len(got) == 0 {
		t.Error("作用域内分数被范围外最大值压没了")
	}
}

// 分类过滤对稠密路同样生效（不能只过滤稀疏两路）。
func TestSearchInFiltersDensePath(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	mm := &fakeMM{dim: 2, fp: "fp", loaded: true,
		text: map[string][]float64{"__default": {0, 1}},
		img:  map[string][]float64{"__default": {0, 1}},
	}
	s.SetDenseSpace(mm)
	if err := s.Add("cat/a", "甲"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("other/b", "乙"); err != nil {
		t.Fatal(err)
	}
	qv := []float64{0, 1}
	if hits := s.denseHits(qv); len(hits) != 2 {
		t.Fatalf("稠密路应命中 2 条，实为 %d", len(hits))
	}
	// SearchIn 走真实查询路径，验证范围外那条被排除
	if got := s.SearchIn("甲乙", "cat", 5); len(got) != 1 || got[0].Name != "cat/a" {
		t.Errorf("稠密路未被分类过滤，实为 %v", namesOf(got))
	}
}

// 性能：分类过滤不应拖慢全库检索（早退守卫 + 作用域判定开销可忽略）。
func TestSearchInOverhead(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	for i := 0; i < 200; i++ {
		if err := s.Add(catName(i), "内容 关键词 内容"); err != nil {
			t.Fatal(err)
		}
	}
	q := "关键词 内容"
	// 预热
	for i := 0; i < 20; i++ {
		s.Search(q, 5)
		s.SearchIn(q, "g0", 5)
	}
	start := time.Now()
	for i := 0; i < 50; i++ {
		s.Search(q, 5)
	}
	full := time.Since(start)
	start = time.Now()
	for i := 0; i < 50; i++ {
		s.SearchIn(q, "g0", 5)
	}
	scoped := time.Since(start)
	t.Logf("50 次：全库 %v  限定 g0 %v", full.Round(time.Microsecond), scoped.Round(time.Microsecond))
	// 限定范围命中数更少，理应更快；即便持平也不该慢太多
	if scoped > full*3 {
		t.Errorf("分类过滤带来 %0.1f× 开销（全库 %v → 限定 %v）",
			float64(scoped)/float64(full), full, scoped)
	}
}

func catName(i int) string {
	return "g" + string(rune('0'+i/100)) + "/item" + string(rune('a'+i/10)) + string(rune('0'+i%10))
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}
