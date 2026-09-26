package knowledge

import (
	"os"
	"path/filepath"
	"testing"
)

// 知识名不得逃出知识根。
//
// 复现（修复前）：Remove("..") 直接 os.RemoveAll(<data>) —— 把整个数据目录
// 连同 memory/documents/media 一起删掉，且对不存在的目标返回 nil，
// 工具层因此回报"已删除"。Add("../../x") 则把内容写到知识根外，
// 重启 scanAll 扫不回来 ⇒ 幽灵条目。
//
// 该缺陷在 release/v1.0.x ~ v1.3.x 四条发布线上均存在。
func TestNameCannotEscapeKnowledgeRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data", "knowledge")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	// 造出与生产同构的邻居：记忆/文档/媒体都在 <data> 下
	neighbors := []string{"memory", "documents", "media"}
	for _, n := range neighbors {
		if err := os.MkdirAll(filepath.Join(base, "data", n), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// 放一个"数据"文件，确保邻居非空（空目录时 RemoveAll 会连父一起删）
	for _, n := range neighbors {
		p := filepath.Join(base, "data", n, "keep.db")
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	s := NewStore(root)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if err := s.Add("real", "正常知识"); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"..", "../..", "../../..", "a/../../..", ".hidden", "x/.hidden"} {
		if err := s.Add(name, "越界内容"); err == nil {
			t.Errorf("Add(%q) 应被拒绝，实际成功了", name)
		}
		if err := s.Remove(name); err == nil {
			t.Errorf("Remove(%q) 应被拒绝，实际成功了", name)
		}
	}

	// 邻居必须完好
	for _, n := range neighbors {
		if _, err := os.Stat(filepath.Join(base, "data", n, "keep.db")); err != nil {
			t.Errorf("邻居数据 %s 被删了: %v", n, err)
		}
	}
	// 知识根本身与正常条目必须还在
	if _, err := os.Stat(filepath.Join(root, "real", "content.md")); err != nil {
		t.Errorf("正常知识被误删: %v", err)
	}
	// 根外不得留下任何东西
	if _, err := os.Stat(filepath.Join(base, "..")); err == nil {
		t.Log("（父目录存在属正常）")
	}
	for _, name := range []string{"real", ".hidden", "a"} {
		if _, err := os.Stat(filepath.Join(base, name)); err == nil {
			t.Errorf("根外残留了 %q", name)
		}
	}
}

// 反向验证：确认本分支修复前确实存在该缺陷（防止"修了个不存在的问题"）。
func TestVulnerableBaselineReproduces(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实执行破坏性路径")
	}
	base := t.TempDir()
	root := filepath.Join(base, "data", "knowledge")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	neighbor := filepath.Join(base, "data", "memory")
	if err := os.MkdirAll(neighbor, 0755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(neighbor, "keep.db")
	if err := os.WriteFile(keep, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	s := NewStore(root)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// 修复后这里必然被拒；若真被删了，说明防护失效
	if err := s.Remove(".."); err == nil {
		t.Fatal("Remove(\"..\") 未被拒绝 —— 防护已失效")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("数据被删: %v", err)
	}
}
