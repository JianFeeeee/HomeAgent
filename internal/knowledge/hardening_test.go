package knowledge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本轮加固的回归测试。每条都对应一次实测复现，注释里留了复现现象，
// 免得后来者以为这些分支是过度防御而删掉。

// 知识名必须与盘上目录逐字一致，且重启前后不变。
//
// 复现（修复前）：Add("tech/ Go /Note") 得到 id="tech/_go_/note"，
// 而建目录时逐段 sanitize 得到 "tech/_go/note" —— 内存键与盘上目录从
// 第一次落盘起就不一致。重启后 scanDir 读回 "tech/_go/note"，
// knowledge_list / knowledge_delete 的 key 全部对不上。
func TestNameStableAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}

	inputs := []string{"tech/ Go /Note", "Tech/Go/并发", "coffee"}
	for _, in := range inputs {
		if err := s.Add(in, "正文 "+in); err != nil {
			t.Fatalf("Add(%q): %v", in, err)
		}
	}
	before := s.List()
	s.Stop()

	s2 := NewStore(dir)
	if err := s2.Start(); err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	after := s2.List()

	if strings.Join(before, "\x00") != strings.Join(after, "\x00") {
		t.Fatalf("重启前后知识名不一致：\n  前 %v\n  后 %v", before, after)
	}

	// 内存键必须能在盘上找到同名目录
	for _, id := range after {
		p := filepath.Join(dir, filepath.FromSlash(id), "content.md")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("知识 %q 的盘上路径不存在: %v", id, err)
		}
	}
}

// 知识名不得逃出知识根。
//
// 复现（修复前）：Add("../../escaped") 无报错写到根外，重启 scanAll 扫不到
// => 幽灵条目；Remove("..") 直接 RemoveAll 掉整个数据目录（实测返回 nil）。
func TestNameRejectsEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data", "knowledge")
	s := NewStore(root)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	bad := []string{"", "  ", "..", "../..", "../../escaped", "a/../../..", ".hidden", "a//b", "a/./b"}
	for _, name := range bad {
		if err := s.Add(name, "不该被写入"); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Add(%q) 应返回 ErrInvalidName，实为 %v", name, err)
		}
	}
	if n := len(s.List()); n != 0 {
		t.Fatalf("非法名不应产生条目，实有 %d 条: %v", n, s.List())
	}

	// 根外不得出现任何东西
	if _, err := os.Stat(filepath.Join(base, "data", "escaped")); err == nil {
		t.Error("发生了根外写入")
	}

	// Remove 同样不得越界，且知识根必须还在
	if err := s.Remove("../.."); !errors.Is(err, ErrInvalidName) {
		t.Errorf("Remove(\"../..\") 应返回 ErrInvalidName，实为 %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("知识根被删掉了: %v", err)
	}
}

// 分类条目必须能用全名删掉，且文件真的消失。
//
// 复现（修复前）：List() 给 "tech/go/并发"，Remove 却拼出根下 "tech/go/并发"
// 之外的路径，os.RemoveAll 删空目录返 nil => 工具回报"已删除"，
// content.md 与索引条目都还在。
func TestRemoveCategorizedByFullName(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.Add("tech/go/并发", "goroutine 调度 GMP 抢占"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("coffee", "手冲 烘焙 水温"); err != nil {
		t.Fatal(err)
	}

	if err := s.Remove("tech/go/并发"); err != nil {
		t.Fatalf("Remove 全名失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tech", "go", "并发", "content.md")); err == nil {
		t.Error("报成功但 content.md 仍在盘上")
	}
	if got := s.List(); len(got) != 1 || got[0] != "coffee" {
		t.Errorf("删除后 List 应为 [coffee]，实为 %v", got)
	}
	// 空掉的分类目录应被清掉，不留一片空壳
	if _, err := os.Stat(filepath.Join(dir, "tech")); err == nil {
		t.Error("分类空目录 tech/ 未清理")
	}
	// 知识根绝不能被一起删掉
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("知识根被删掉了: %v", err)
	}
}

// 叶名寻址：唯一命中时接受（界面历史数据兼容），多条同名时拒绝而不是猜。
func TestRemoveByLeafName(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.Add("tech/go/并发", "A"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("并发"); err != nil {
		t.Fatalf("唯一叶名应可删除，实为 %v", err)
	}
	if n := len(s.List()); n != 0 {
		t.Fatalf("叶名删除后应为空，实为 %v", s.List())
	}

	// 两条同叶名 => 拒绝，且都不能被误删
	if err := s.Add("a/dup", "A"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("b/dup", "B"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("dup"); !errors.Is(err, ErrNotFound) {
		t.Errorf("歧义叶名应拒绝，实为 %v", err)
	}
	if n := len(s.List()); n != 2 {
		t.Errorf("歧义叶名不得误删，实剩 %v", s.List())
	}
}

// 删除不存在的条目必须报错（webui 的 DELETE 把 error 映射成 404）。
func TestRemoveNotFound(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.Remove("nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除不存在条目应返回 ErrNotFound，实为 %v", err)
	}
	// 幂等：再删一次仍是同一个错，不 panic 不误删
	if err := s.Remove("nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除应仍是 ErrNotFound，实为 %v", err)
	}
}

// 遗留盘上布局（大写、空格）必须仍可寻址与删除。
//
// 这是一次真实回归：scanDir 按盘上目录**原样**建键，所以修复前 Add 留下的
// "Tech/Upper" 在 items 里的键就是 "Tech/Upper"，而 normalizeName 会
// 小写化+替换空格。若 resolve 只查规范名，这些老条目就会"删不掉、除不尽"
// ——List() 看得到、Remove() 报不存在。
func TestLegacyLayoutIsResolvable(t *testing.T) {
	dir := t.TempDir()
	legacy := []string{"Tech/Upper", "a/b with space", "tech/_go_/note"}
	for _, d := range legacy {
		p := filepath.Join(dir, filepath.FromSlash(d), "content.md")
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("遗留正文 "+d), 0644); err != nil {
			t.Fatal(err)
		}
	}

	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	names := s.List()
	if len(names) != len(legacy) {
		t.Fatalf("应载入 %d 条遗留条目，实为 %v", len(legacy), names)
	}
	for _, id := range names {
		if err := s.Remove(id); err != nil {
			t.Errorf("遗留条目 %q 删不掉: %v", id, err)
		}
	}
	if rest := s.List(); len(rest) != 0 {
		t.Errorf("遗留条目未清空，实剩 %v", rest)
	}
}

// 空目录清理绝不能往上走到知识根：root 被删 = 整个知识库连同索引一起没了。
// 单段名（直接挂在 root 下）是最容易越界的一种——filepath.Dir 恰好等于 root，
// 若前缀判断写松一格就会命中。
func TestRemoveNeverDeletesRoot(t *testing.T) {
	for _, name := range []string{"single", "a/b", "a/b/c/d"} {
		dir := t.TempDir()
		s := NewStore(dir)
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		if err := s.Add(name, "正文"); err != nil {
			t.Fatalf("Add(%q): %v", name, err)
		}
		if err := s.Remove(name); err != nil {
			t.Fatalf("Remove(%q): %v", name, err)
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("name=%q 删除后知识根没了: err=%v", name, err)
		}
		// 哨兵：删除后**必须还能写入知识根**。不能用 .index.json 作哨兵——
		// 索引写入已改为标脏延迟到 Flush/Stop，删除后它本就不该立刻存在
		// （那样反而会把「延迟」这个行为给测漏）。
		probe := filepath.Join(dir, "sentinel", "content.md")
		if err := os.MkdirAll(filepath.Dir(probe), 0755); err != nil {
			t.Fatalf("name=%q 删除后知识根不可写（已被破坏）: %v", name, err)
		}
		if err := os.WriteFile(probe, []byte("x"), 0644); err != nil {
			t.Fatalf("name=%q 删除后无法在根内建目录: %v", name, err)
		}
		s.Stop()
	}
}

// 分类是 Add 从名字里声明的，不是检索时现推的；重启后必须一致。
func TestCategorySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("tech/go/并发", "正文"); err != nil {
		t.Fatal(err)
	}
	k := s.items["tech/go/并发"]
	if k == nil {
		t.Fatalf("未按规范名建条目，实有 %v", s.List())
	}
	if k.Category != "tech/go" {
		t.Errorf("Add 后 Category 应为 tech/go，实为 %q", k.Category)
	}
	s.Stop()

	s2 := NewStore(dir)
	if err := s2.Start(); err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	k2 := s2.items["tech/go/并发"]
	if k2 == nil {
		t.Fatalf("重启后条目名漂移，实有 %v", s2.List())
	}
	if k2.Category != k.Category {
		t.Errorf("重启后 Category 变了：%q -> %q", k.Category, k2.Category)
	}
	// 树里的挂载点应与 Category 一致
	tr := s2.BuildTree()
	node := tr
	for _, part := range strings.Split(k.Category, "/") {
		node = node.Children[part]
		if node == nil {
			t.Fatalf("树里缺少分类节点 %q", part)
		}
	}
	if len(node.Items) != 1 || node.Items[0].Name != "tech/go/并发" {
		t.Errorf("分类节点下应有该条目，实为 %+v", node.Items)
	}
}
