package memory

// N2a 第二块砖：轻量内核的图记忆装配（temp 可写 + 主库只读 + 并集查询）。
//
// 设计：docs/zh/resident-subagent-design.md §5.6。
// 关键性质：
//   ① 子写入只落 temp（写不进展主库）—— main 是受限句柄，结构性拒绝；
//   ② 子查询 = temp ∪ main；
//   ③ 两个子之间 temp 互不可见；
//   ④ allowTempGraphWrite=false 时子对图记忆完全只读（没有 temp 实例）。

import (
	"path/filepath"
	"testing"
)

// newMainWith 建一个主库并写入若干三元组。
func newMainWith(t *testing.T, dir string, triples ...Triple) *GraphDB {
	t.Helper()
	main, err := NewGraphDB(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(triples) > 0 {
		if _, _, err := main.Commit(triples, "sess", 1); err != nil {
			t.Fatalf("主库写入失败: %v", err)
		}
	}
	return main
}

func names(entities []Entity) []string {
	out := make([]string, 0, len(entities))
	for _, e := range entities {
		out = append(out, e.Name)
	}
	return out
}

func hasName(entities []Entity, name string) bool {
	for _, e := range entities {
		if e.Name == name {
			return true
		}
	}
	return false
}

// 子写入只落 temp；查询是 temp ∪ main。
func TestLightMemory_WriteGoesToTempReadIsUnion(t *testing.T) {
	dir := t.TempDir()
	main := newMainWith(t, dir, Triple{Subject: "张三", Relation: "任职于", Object: "甲公司"})
	defer main.Close()

	light, err := NewLightMemory(main, filepath.Join(dir, "sub-1.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer light.Close()

	// 子写自己的发现。
	if _, _, err := light.Commit([]Triple{{Subject: "李四", Relation: "任职于", Object: "乙公司"}}, "sess", 1); err != nil {
		t.Fatalf("子写入 temp 应成功: %v", err)
	}

	// 查询能同时看到 main（张三）与 temp（李四）。
	res, err := light.Recall([]string{"张三", "李四"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if !hasName(res.Entities, "张三") {
		t.Fatalf("子应看得到主库内容，实际 %v", names(res.Entities))
	}
	if !hasName(res.Entities, "李四") {
		t.Fatalf("子应看得到自己 temp 的内容，实际 %v", names(res.Entities))
	}

	// 主库**没有**被写入（子改不了 main）。
	mainOnly, err := main.Recall([]string{"李四"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if hasName(mainOnly.Entities, "李四") {
		t.Fatalf("主的图记忆里不该出现子才知道的实体：%v", names(mainOnly.Entities))
	}
}

// 两个子之间 temp 互不可见（只有 main 共享）。
func TestLightMemory_ChildrenTempsAreIsolated(t *testing.T) {
	dir := t.TempDir()
	main := newMainWith(t, dir, Triple{Subject: "共享", Relation: "属于", Object: "主库"})
	defer main.Close()

	a, err := NewLightMemory(main, filepath.Join(dir, "sub-a.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := NewLightMemory(main, filepath.Join(dir, "sub-b.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if _, _, err := a.Commit([]Triple{{Subject: "A的秘密", Relation: "仅属于", Object: "A"}}, "s", 1); err != nil {
		t.Fatal(err)
	}

	resB, err := b.Recall([]string{"A的秘密", "共享"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if hasName(resB.Entities, "A的秘密") {
		t.Fatalf("B 不该看到 A 的 temp：%v", names(resB.Entities))
	}
	if !hasName(resB.Entities, "共享") {
		t.Fatalf("B 应看得到共享的 main：%v", names(resB.Entities))
	}
}

// allowTempGraphWrite=false：子对图记忆完全只读（没有 temp 实例，写入被拒）。
func TestLightMemory_WriteDisabledIsFullyReadOnly(t *testing.T) {
	dir := t.TempDir()
	main := newMainWith(t, dir, Triple{Subject: "张三", Relation: "任职于", Object: "甲公司"})
	defer main.Close()

	light, err := NewLightMemory(main, "", false)
	if err != nil {
		t.Fatalf("不允许写时不应要求 temp 路径: %v", err)
	}
	defer light.Close()

	if light.AllowWrite() {
		t.Fatal("AllowWrite 应为 false")
	}
	if light.Temp() != nil {
		t.Fatal("不允许写时不该有 temp 实例")
	}
	if _, _, err := light.Commit([]Triple{{Subject: "李四", Relation: "x", Object: "y"}}, "s", 1); err == nil {
		t.Fatal("不允许写时的写入必须被拒")
	}
	// 读仍然可用（只读主库）。
	res, err := light.Recall([]string{"张三"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if !hasName(res.Entities, "张三") {
		t.Fatalf("只读模式仍应读到主库：%v", names(res.Entities))
	}
}

// 并集必须去重：同一个实体同时存在于 main 与 temp 时只出现一次。
func TestLightMemory_UnionDedupesSameEntity(t *testing.T) {
	dir := t.TempDir()
	main := newMainWith(t, dir, Triple{Subject: "张三", Relation: "任职于", Object: "甲公司"})
	defer main.Close()

	light, err := NewLightMemory(main, filepath.Join(dir, "sub.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer light.Close()
	if _, _, err := light.Commit([]Triple{{Subject: "张三", Relation: "擅长", Object: "Go"}}, "s", 1); err != nil {
		t.Fatal(err)
	}

	res, err := light.Recall([]string{"张三"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range res.Entities {
		if e.Name == "张三" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("同名实体在并集中应只出现一次，实际 %d 次：%v", n, names(res.Entities))
	}
}

// 并集查询的确定性（同样的输入必得同样的顺序），便于断言与展示稳定。
func TestMergeRecall_Deterministic(t *testing.T) {
	a := &RecallResult{
		Entities: []Entity{{Name: "b", MentionCount: 1}, {Name: "a", MentionCount: 1}},
		Relations: []Relation{
			{SourceName: "z", RelationType: "r", TargetName: "y"},
			{SourceName: "a", RelationType: "r", TargetName: "b"},
		},
	}
	b := &RecallResult{
		Entities:  []Entity{{Name: "a", MentionCount: 5}},
		Relations: []Relation{{SourceName: "a", RelationType: "r", TargetName: "b"}},
	}
	got := mergeRecall(a, b)
	if len(got.Entities) != 2 || got.Entities[0].Name != "a" || got.Entities[1].Name != "b" {
		t.Fatalf("实体合并结果=%v", names(got.Entities))
	}
	if got.Entities[0].MentionCount != 5 {
		t.Fatalf("同名实体应保留 mention_count 较大者，实际 %d", got.Entities[0].MentionCount)
	}
	if len(got.Relations) != 2 {
		t.Fatalf("关系应去重后剩 2 条，实际 %d", len(got.Relations))
	}
	if got.Relations[0].SourceName != "a" {
		t.Fatalf("关系未按源名排序：%+v", got.Relations)
	}
}
