package memory

// N2d 数据面：**回收**＝父读子的 temp → 选记录 → 合入 main。
//
// 设计 §9：回收是"取消语义"（收割后取消该驻留子），且**由父决定纳入哪些**。
// 这里只做数据面（读得到、合得进、幂等），控制面（谁来决定、何时取消）在 N3/N6。

import (
	"path/filepath"
	"testing"
)

func TestExportTriples_OnlyActiveWithNames(t *testing.T) {
	dir := t.TempDir()
	g, err := NewGraphDB(filepath.Join(dir, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	if _, _, err := g.Commit([]Triple{
		{Subject: "张三", Relation: "任职于", Object: "甲公司"},
		{Subject: "李四", Relation: "合作", Object: "王五"},
	}, "sess", 1); err != nil {
		t.Fatal(err)
	}

	got, err := g.ExportTriples(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("导出 %d 条，期望 2：%+v", len(got), got)
	}
	// 实体名必须带出来（合入侧要靠名字 upsert，而不是内部 id）。
	seen := map[string]bool{}
	for _, tr := range got {
		if tr.Subject == "" || tr.Object == "" || tr.Relation == "" {
			t.Fatalf("导出的三元组字段不全：%+v", tr)
		}
		seen[tr.Subject+"→"+tr.Object] = true
	}
	if !seen["张三→甲公司"] || !seen["李四→王五"] {
		t.Fatalf("导出内容不对：%+v", got)
	}

	// limit 生效。
	one, err := g.ExportTriples(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 {
		t.Fatalf("limit=1 应导出 1 条，实际 %d", len(one))
	}
}

// 回收主流程：子写 temp → 父导出 → 选中的合入 main；主库拿到、temp 不变。
func TestReclaim_HarvestChildTempIntoMain(t *testing.T) {
	dir := t.TempDir()
	main, err := NewGraphDB(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()

	child, err := NewLightMemory(main, filepath.Join(dir, "sub.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()

	// 子在 temp 里积累了两条发现。
	if _, _, err := child.Commit([]Triple{
		{Subject: "子发现A", Relation: "指向", Object: "结论1"},
		{Subject: "子发现B", Relation: "指向", Object: "结论2"},
	}, "sess", 1); err != nil {
		t.Fatal(err)
	}

	// 父：导出 → **选一条**（"哪些纳入记忆"由父决定）→ 写进 main。
	all, err := child.Temp().ExportTriples(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("导出 %d 条，期望 2", len(all))
	}
	selected := []Triple{all[0]}
	if _, _, err := main.Commit(selected, "reclaim", 0); err != nil {
		t.Fatalf("合入 main 失败: %v", err)
	}

	// 主库只拿到选中的那条。
	mainRes, err := main.Recall([]string{"子发现A", "子发现B"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if !hasName(mainRes.Entities, "子发现A") {
		t.Fatalf("选中的记录应进主库：%v", names(mainRes.Entities))
	}
	if hasName(mainRes.Entities, "子发现B") {
		t.Fatalf("未选中的记录不该进主库：%v", names(mainRes.Entities))
	}

	// 重复收割是幂等的（Commit 按实体名 upsert + 关系唯一约束）。
	before := len(mainRes.Entities)
	if _, _, err := main.Commit(selected, "reclaim", 0); err != nil {
		t.Fatal(err)
	}
	after, err := main.Recall([]string{"子发现A"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Entities) != before {
		t.Fatalf("重复收割不应产生重复实体：before=%d after=%d", before, len(after.Entities))
	}
}
