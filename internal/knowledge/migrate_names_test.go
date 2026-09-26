package knowledge

import (
	"os"
	"path/filepath"
	"testing"
)

// makeLegacy 造出"修复前 Add 留下的脏目录布局"。
func makeLegacy(t *testing.T, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		p := filepath.Join(root, filepath.FromSlash(d), "content.md")
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("遗留正文 "+d), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// PlanMigration 只读，不得改动任何文件。
func TestPlanMigrationIsReadOnly(t *testing.T) {
	root := makeLegacy(t, "Tech/Upper", "a/b with space", "good", "tech/_go_/note")
	before := snapshotTree(t, root)

	items, err := PlanMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("应发现 4 条，实为 %+v", items)
	}
	if after := snapshotTree(t, root); after != before {
		t.Errorf("PlanMigration 改动了盘上状态：\n前 %s\n后 %s", before, after)
	}
	// 清单内容正确
	byOld := map[string]MigrationItem{}
	for _, it := range items {
		byOld[it.OldName] = it
	}
	for old, want := range map[string]string{
		"Tech/Upper":     "tech/upper",
		"a/b with space": "a/b_with_space",
		// 段内无空格/大写 ⇒ 已是规范名，NewName 为空（无需改动）
		"tech/_go_/note": "",
		"good":           "",
	} {
		it, ok := byOld[old]
		if !ok {
			t.Fatalf("清单缺 %q", old)
		}
		if it.NewName != want {
			t.Errorf("%q 的 NewName 应为 %q，实为 %q", old, want, it.NewName)
		}
	}
}

// 迁移后：Store 能载入全部条目，且能按规范名删除。
func TestApplyMigrationThenUsable(t *testing.T) {
	root := makeLegacy(t, "Tech/Upper", "a/b with space", "good")
	items, err := PlanMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	applied, failed := ApplyMigration(root, items, 0)
	if applied != 2 || failed != 0 {
		t.Fatalf("应成功 2 失败 0，实为 %d/%d", applied, failed)
	}

	s := NewStore(root)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if got := len(s.List()); got != 3 {
		t.Fatalf("迁移后应载入 3 条，实为 %d 条：%v", got, s.List())
	}
	for _, id := range s.List() {
		if err := s.Remove(id); err != nil {
			t.Errorf("迁移后条目 %q 删不掉: %v", id, err)
		}
	}
}

// 冲突必须整批拒绝，且盘上零改动（半迁移比不迁移更难收拾）。
func TestApplyMigrationRefusesOnConflict(t *testing.T) {
	root := makeLegacy(t, "A/b", "a/B", "keep/me")
	before := snapshotTree(t, root)

	items, err := PlanMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	applied, failed := ApplyMigration(root, items, 0)
	if applied != 0 {
		t.Errorf("有冲突时不应改名任何一条，实为成功 %d", applied)
	}
	if failed == 0 {
		t.Error("冲突应被报告为失败")
	}
	if after := snapshotTree(t, root); after != before {
		t.Errorf("冲突路径下盘上被改动：\n前 %s\n后 %s", before, after)
	}
}

// 目标名已存在于盘上（与既有条目撞名）也算冲突。
func TestApplyMigrationRefusesWhenTargetExists(t *testing.T) {
	root := makeLegacy(t, "Tech/Upper", "tech/upper")
	before := snapshotTree(t, root)

	items, err := PlanMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	applied, _ := ApplyMigration(root, items, 0)
	if applied != 0 {
		t.Errorf("目标已存在时应拒绝，实为成功 %d", applied)
	}
	if after := snapshotTree(t, root); after != before {
		t.Error("盘上被改动")
	}
}

// limit 必须真的限住条数。
func TestApplyMigrationRespectsLimit(t *testing.T) {
	root := makeLegacy(t, "A/one", "B/two", "C/three", "D/four")
	items, err := PlanMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	applied, _ := ApplyMigration(root, items, 2)
	if applied != 2 {
		t.Errorf("limit=2 应只改 2 条，实为 %d", applied)
	}
	// 剩下两条仍可被再次迁移（幂等续跑）
	items2, _ := PlanMigration(root)
	rest := 0
	for _, it := range items2 {
		if it.NewName != "" {
			rest++
		}
	}
	if rest != 2 {
		t.Errorf("剩余待迁移应为 2 条，实为 %d", rest)
	}
	applied2, _ := ApplyMigration(root, items2, 0)
	if applied2 != 2 {
		t.Errorf("续跑应再改 2 条，实为 %d", applied2)
	}
}

// 迁移不得越出知识根（回归：sanitize 不过滤 .. 时的老问题）。
func TestApplyMigrationStaysInRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data", "knowledge")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	// 人为构造一条越界计划
	items := []MigrationItem{{OldName: "x", NewName: "../escaped"}}
	applied, failed := ApplyMigration(root, items, 0)
	if applied != 0 || failed != 1 {
		t.Errorf("越界目标必须被拒（applied=%d failed=%d）", applied, failed)
	}
	if _, err := os.Stat(filepath.Join(base, "data", "escaped")); err == nil {
		t.Error("发生了根外写入")
	}
}

func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var sb []byte
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		sb = append(sb, rel...)
		if !info.IsDir() {
			sb = append(sb, ' ')
		}
		sb = append(sb, '\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(sb)
}
