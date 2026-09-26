package knowledge

import (
	"strings"
	"testing"
)

// 分层索引端到端：多层分类 → Category 推导 → 树导出 → 分类过滤检索 →
// 索引落盘 → 重启后仍然自洽。
//
// 这是「分层索引是否真的工作」的直接证据。此前分层只是**存储布局**
// （Category 有值、树能导出），但对召回零影响 —— SearchTree/SearchCategories
// 是死代码，Search 全库平铺。现在 SearchIn 让分层参与召回。
func TestHierarchicalIndexEndToEnd(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	for _, e := range []struct{ name, body string }{
		{"tech/go/并发", "goroutine 调度 GMP 抢占 通道"},
		{"tech/go/context", "context 取消 超时 传播"},
		{"tech/rust/所有权", "borrow checker move 语义"},
		{"life/sleep", "作息 褪黑素 深睡"},
		{"cook/coffee", "手冲 烘焙 水温 粉水比"},
	} {
		if err := s.Add(e.name, e.body); err != nil {
			t.Fatal(err)
		}
	}

	// 1) 多层分类：Category 是**父路径**，不是最后一段
	for _, want := range []struct{ name, cat string }{
		{"tech/go/并发", "tech/go"},
		{"tech/go/context", "tech/go"},
		{"tech/rust/所有权", "tech/rust"},
		{"life/sleep", "life"},
		{"cook/coffee", "cook"},
	} {
		k := s.items[want.name]
		if k == nil {
			t.Errorf("条目 %s 未载入", want.name)
			continue
		}
		if k.Category != want.cat {
			t.Errorf("%s 的 Category 应为 %q，实为 %q", want.name, want.cat, k.Category)
		}
	}

	// 2) 树导出：层级结构 + 挂载点 + 条目带向量/预览
	ti := s.BuildTree()
	tech := ti.Children["tech"]
	if tech == nil || tech.Children["go"] == nil {
		t.Fatal("树中缺少 tech/go 节点")
	}
	var inGo []string
	for _, it := range tech.Children["go"].Items {
		inGo = append(inGo, it.Name)
		if len(it.Vector) == 0 {
			t.Errorf("%s 在树里没有向量", it.Name)
		}
		if it.Preview == "" {
			t.Errorf("%s 在树里没有预览", it.Name)
		}
	}
	if len(inGo) != 2 {
		t.Errorf("tech/go 下应挂 2 条，实为 %v", inGo)
	}
	// 顶层无分类条目挂在 root.Items
	if len(ti.Items) != 0 {
		t.Errorf("本用例所有条目都有分类，root.Items 应为空，实为 %d", len(ti.Items))
	}

	// 3) 分类过滤检索：前缀匹配整棵子树
	q := "调度 取消 borrow 作息 手冲"
	if r := s.SearchIn(q, "tech/go", 10); len(r) != 2 {
		t.Errorf("限定 tech/go 应命中 2 条，实为 %v", namesOf(r))
	}
	if r := s.SearchIn(q, "tech", 10); len(r) != 3 {
		t.Errorf("限定 tech 应命中 3 条，实为 %v", namesOf(r))
	}
	for _, r := range s.SearchIn(q, "tech", 10) {
		if !strings.HasPrefix(r.Name, "tech/") {
			t.Errorf("范围外条目 %s 混入", r.Name)
		}
	}
	// 不存在的分类：空结果而非报错
	if r := s.SearchIn(q, "no/such", 10); len(r) != 0 {
		t.Errorf("不存在的分类应返回空，实为 %v", namesOf(r))
	}

	// 4) 索引落盘（含分类结构）
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := readFileString(dir + "/.index.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, "tech") || !strings.Contains(data, "go") {
		t.Error("索引文件未包含分类结构")
	}

	// 5) 重启后分层仍自洽（不漂移）
	s.Stop()
	s2 := NewStore(dir)
	if err := s2.Start(); err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	if len(s2.List()) != 5 {
		t.Errorf("重启后条目数应为 5，实为 %d", len(s2.List()))
	}
	for _, want := range []struct{ name, cat string }{
		{"tech/go/并发", "tech/go"},
		{"tech/rust/所有权", "tech/rust"},
		{"life/sleep", "life"},
	} {
		k := s2.items[want.name]
		if k == nil {
			t.Errorf("重启后 %s 丢失", want.name)
			continue
		}
		if k.Category != want.cat {
			t.Errorf("重启后 %s 的 Category 漂移: %q → %q", want.name, want.cat, k.Category)
		}
	}
	if r := s2.SearchIn(q, "tech/go", 10); len(r) != 2 {
		t.Errorf("重启后分类过滤失效，实为 %v", namesOf(r))
	}
	// 树在重启后仍可用
	if s2.BuildTree().Children["tech"] == nil {
		t.Error("重启后树结构丢失")
	}
}
