package memory

import (
	"path/filepath"
	"testing"
)

// 钩子必须真的在 commit / recall / delete / merge 上触发，且 ID 真实。
func TestAccessHookFires(t *testing.T) {
	dir := t.TempDir()
	g, err := NewGraphDB(filepath.Join(dir, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	var got []AccessEvent
	g.SetAccessHook(func(ev AccessEvent) { got = append(got, ev) })

	// commit：应上报 written + created
	if _, _, err := g.Commit([]Triple{{
		Subject: "张三", Relation: "喜欢", Object: "咖啡",
		SentenceText: "张三喜欢咖啡",
	}}, "s1", 1); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[len(got)-1].Op != "commit" {
		t.Fatalf("commit 未触发钩子: %+v", got)
	}
	last := got[len(got)-1]
	t.Logf("commit: blocks=%d created=%d", len(last.Blocks), len(last.Created))
	if len(last.Blocks) == 0 {
		t.Fatal("commit 应上报写入的块 ID")
	}
	if len(last.Created) == 0 {
		t.Fatal("首次写入应有新建块（驱动生长动画）")
	}

	// 重复提交同一三元组：不应再报 created（否则动画重复播）
	got = nil
	if _, _, err := g.Commit([]Triple{{
		Subject: "张三", Relation: "喜欢", Object: "咖啡",
		SentenceText: "张三喜欢咖啡",
	}}, "s1", 1); err != nil {
		t.Fatal(err)
	}
	if len(got) > 0 && len(got[len(got)-1].Created) != 0 {
		t.Fatalf("重复写入不应报新建，实际 =%v", got[len(got)-1].Created)
	}
	t.Log("重复 commit 未误报新建 ✓")

	// recall：应上报读到的块
	got = nil
	if _, err := g.Recall([]string{"咖啡"}, nil, 1, ""); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range got {
		if ev.Op == "recall" && len(ev.Blocks) > 0 {
			found = true
			t.Logf("recall: blocks=%v", ev.Blocks)
		}
	}
	if !found {
		t.Fatalf("recall 未上报块 ID: %+v", got)
	}

	// delete：应上报 removed
	got = nil
	if _, err := g.DeleteEntity("张三"); err != nil {
		t.Fatal(err)
	}
	found = false
	for _, ev := range got {
		if ev.Op == "delete" && len(ev.Removed) > 0 {
			found = true
			t.Logf("delete: removed=%v", ev.Removed)
		}
	}
	if !found {
		t.Fatalf("delete 未上报被删块: %+v", got)
	}
}
