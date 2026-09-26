package knowledge

import (
	"encoding/json"
	"strings"
	"testing"
)

func seedTreeStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(t.TempDir())
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	for _, e := range []struct{ name, body string }{
		{"tech/go/并发", "goroutine 调度 GMP 抢占"},
		{"tech/go/context", "context 取消 超时"},
		{"tech/rust/所有权", "borrow checker move 语义"},
		{"life/sleep", "作息 褪黑素"},
		{"cook/coffee", "手冲 烘焙"},
		{"顶层条目", "没有分类的条目"},
	} {
		if err := s.Add(e.name, e.body); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// 树视图必须呈现正确的层级、计数与挂载点。
func TestTreeViewShape(t *testing.T) {
	s := seedTreeStore(t)
	root := s.Tree(TreeOptions{IncludeItems: true})
	if root == nil {
		t.Fatal("Tree 返回 nil")
	}
	if root.Name != "root" || root.Path != "" {
		t.Errorf("根节点应为 root/空 path，实为 %q/%q", root.Name, root.Path)
	}
	// 顶层无分类条目挂在 root.Items
	if root.ItemCount != 1 || len(root.Items) != 1 || root.Items[0].Name != "顶层条目" {
		t.Errorf("root 应挂 1 条无分类条目，实为 %d 条 %v", root.ItemCount, itemNames(root.Items))
	}
	// 子分类按名排序
	var kids []string
	for _, c := range root.Children {
		kids = append(kids, c.Name)
	}
	if strings.Join(kids, ",") != "cook,life,tech" {
		t.Errorf("子分类应按名排序为 cook,life,tech，实为 %v", kids)
	}
	// tech 有两个子分类
	tech := findChild(root, "tech")
	if tech == nil {
		t.Fatal("缺少 tech 节点")
	}
	if tech.TotalCount != 3 {
		t.Errorf("tech 子树应有 3 条（go 2 + rust 1），实为 %d", tech.TotalCount)
	}
	// tech/go 是 tech 的**子节点**（层级体现在树上，不是平铺）
	goNode := findChild(tech, "go")
	if goNode == nil {
		t.Fatal("缺少 tech/go 节点")
	}
	if goNode.ItemCount != 2 || goNode.Path != "tech/go" {
		t.Errorf("tech/go 应有 2 条且 path 正确，实为 %d / %q", goNode.ItemCount, goNode.Path)
	}
	// 条目视图字段完整
	for _, it := range goNode.Items {
		if it.Preview == "" || it.Size == 0 {
			t.Errorf("%s 缺 preview/size: %+v", it.Name, it)
		}
	}
}

// 树视图**不得**带向量（那是内部导出物的内容，见 tree.go 头部说明）。
func TestTreeViewCarriesNoVectors(t *testing.T) {
	s := seedTreeStore(t)
	data := mustJSON(t, s.Tree(TreeOptions{IncludeItems: true}))
	for _, forbidden := range []string{`"vector"`, `"Vector"`} {
		if strings.Contains(data, forbidden) {
			t.Errorf("树视图不应含 %s（会把稀疏特征表泄漏给外部调用方）", forbidden)
		}
	}
}

// MaxDepth 剪枝：只给计数不下钻，且 TotalCount 仍准确。
func TestTreeMaxDepthPrunesButKeepsCount(t *testing.T) {
	s := seedTreeStore(t)
	full := s.Tree(TreeOptions{})
	shallow := s.Tree(TreeOptions{MaxDepth: 1})

	if full.TotalCount != shallow.TotalCount {
		t.Errorf("剪枝不应改变 TotalCount：%d vs %d", full.TotalCount, shallow.TotalCount)
	}
	if full.TotalCount != 6 {
		t.Errorf("总条目应为 6，实为 %d", full.TotalCount)
	}
	// depth=1 ⇒ 根的直接子分类有节点，但它们的 Children 为空
	for _, c := range shallow.Children {
		if len(c.Children) != 0 {
			t.Errorf("depth=1 时 %s 下不应再展开子节点，实为 %d 个", c.Name, len(c.Children))
		}
		if c.TotalCount == 0 {
			t.Errorf("depth=1 时 %s 的 TotalCount 仍应是真实值", c.Name)
		}
	}
	tech := findChild(shallow, "tech")
	if tech != nil && tech.TotalCount != 3 {
		t.Errorf("剪枝后 tech 的 TotalCount 应仍为 3，实为 %d", tech.TotalCount)
	}
}

// Subtree 只返回那棵子树。
func TestTreeSubtree(t *testing.T) {
	s := seedTreeStore(t)
	view := s.Subtree("tech/go", TreeOptions{IncludeItems: true})
	if view == nil {
		t.Fatal("Subtree 返回 nil")
	}
	// Name 是**本级段名**，Path 是完整路径（前端拼层级用 Name，定位用 Path）
	if view.Path != "tech/go" {
		t.Errorf("子树根 Path 应为 tech/go，实为 %q", view.Path)
	}
	if view.Name != "go" {
		t.Errorf("子树根 Name 应为本级段名 go，实为 %q", view.Name)
	}
	if view.ItemCount != 2 {
		t.Errorf("tech/go 应有 2 条，实为 %d", view.ItemCount)
	}
	// 空分类 → 整棵树
	if r := s.Subtree("", TreeOptions{}); r == nil || r.Name != "root" {
		t.Errorf("空分类应返回整棵树，实为 %+v", r)
	}
	// 不存在的分类 → nil（调用方据此给 404）
	if s.Subtree("no/such", TreeOptions{}) != nil {
		t.Error("不存在的分类应返回 nil")
	}
	// 前后斜杠应被归一化
	if s.Subtree("/tech/go/", TreeOptions{}) == nil {
		t.Error("前后斜杠应被归一化后仍能命中")
	}
}

// IncludeItems=false 时只给计数，不给条目。
func TestTreeWithoutItems(t *testing.T) {
	s := seedTreeStore(t)
	view := s.Tree(TreeOptions{IncludeItems: false})
	if view.ItemCount != 1 {
		t.Errorf("ItemCount 应仍为 1，实为 %d", view.ItemCount)
	}
	if len(view.Items) != 0 {
		t.Errorf("IncludeItems=false 不应返回条目，实为 %v", itemNames(view.Items))
	}
}

// Categories 应含中间层（tech/go/并发 ⇒ tech、tech/go 都在列表里）。
func TestTreeCategoriesIncludesIntermediate(t *testing.T) {
	s := seedTreeStore(t)
	cats := s.Categories()
	want := map[string]bool{"tech": true, "tech/go": true, "tech/rust": true, "life": true, "cook": true}
	for _, c := range cats {
		delete(want, c)
	}
	if len(want) != 0 {
		t.Errorf("缺少分类 %v，实际列表 %v", want, cats)
	}
	// 已排序
	if !isSorted(cats) {
		t.Errorf("Categories 应有序，实为 %v", cats)
	}
}

// CategoryCounts 按数量倒序，且中间层计入其后代。
func TestTreeCategoryCounts(t *testing.T) {
	s := seedTreeStore(t)
	counts := s.CategoryCounts()
	byName := map[string]int{}
	for _, c := range counts {
		byName[c.Category] = c.Count
	}
	if byName["tech"] != 3 {
		t.Errorf("tech 应聚合 3 条，实为 %d", byName["tech"])
	}
	if byName["tech/go"] != 2 {
		t.Errorf("tech/go 应为 2 条，实为 %d", byName["tech/go"])
	}
	// 倒序：首项应是条目最多的分类
	if len(counts) == 0 || counts[0].Category != "tech" {
		t.Errorf("首项应为条目最多的 tech，实为 %+v", counts)
	}
}

// 预览按 rune 截断，不能把多字节字符切坏。
func TestTreePreviewRuneSafe(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if err := s.Add("zh", strings.Repeat("知识库条目内容", 100)); err != nil {
		t.Fatal(err)
	}
	view := s.Tree(TreeOptions{IncludeItems: true, PreviewLimit: 10})
	it := view.Items[0]
	if strings.ContainsRune(it.Preview, 0xFFFD) {
		t.Errorf("预览含替换字符，说明按字节切了多字节字符: %q", it.Preview)
	}
	if runeLen(it.Preview) > 11 { // 10 字 + 省略号
		t.Errorf("预览长度应约 11 rune，实为 %d: %q", runeLen(it.Preview), it.Preview)
	}
}

// findChild 按**本级段名**在子节点里查找（Name 语义见 tree.go 的说明）。
func findChild(v *TreeView, name string) *TreeView {
	for i := range v.Children {
		if v.Children[i].Name == name {
			return &v.Children[i]
		}
	}
	return nil
}

func itemNames(items []TreeItemView) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Name
	}
	return out
}

func isSorted(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}

func runeLen(s string) int { return len([]rune(s)) }

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
