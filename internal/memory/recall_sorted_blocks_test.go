package memory

import (
	"fmt"
	"testing"
)

// ═══════════════════════════════════════════════════════════════
//  RecallSorted 第二分支改块/边 —— 迁移前判据
//
//  ★ 这批判据在**改代码之前**写，测的是「旧表还在时的行为」，
//   改完后它们必须**一字不改**地继续通过。
//
//   理由：这次要重写 302 行、entityIDs 贯穿 14 处。
//   如果先改代码再写判据，判据会不自觉地跟着新实现写 ——
//   那就变成「证明新代码符合新代码」。
// ═══════════════════════════════════════════════════════════════

// TestRecallSorted_关键词命中并带出关系：核心语义
func TestRecallSorted_关键词命中并带出关系(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	if _, _, err := g.Commit([]Triple{
		{Subject: "张三", Relation: "性格", Object: "内向", Confidence: 0.9},
		{Subject: "张三", Relation: "工作", Object: "值班", Confidence: 0.8},
		{Subject: "李四", Relation: "性格", Object: "外向", Confidence: 0.9},
	}, "main", 0); err != nil {
		t.Fatal(err)
	}

	res, err := g.RecallSorted([]string{"张三"}, nil, 1, "", "", SortRelevance)
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, e := range res.Entities {
		names[e.Name] = true
	}
	if !names["张三"] {
		t.Errorf("★ 应命中「张三」，得到 %v", names)
	}
	if names["李四"] {
		t.Errorf("★ depth=1 不该把「李四」拉进来（他不是张三的邻居），得到 %v", names)
	}

	if len(res.Relations) == 0 {
		t.Fatal("★ 应带出「张三」的关系")
	}
	fmt.Printf("  关键词「张三」→ 实体 %d，关系 %d\n", len(res.Entities), len(res.Relations))
	for _, r := range res.Relations {
		if r.SourceName != "张三" && r.TargetName != "张三" {
			t.Errorf("★ 关系两端应含「张三」，实际 %s -%s-> %s",
				r.SourceName, r.RelationType, r.TargetName)
		}
	}
}

// TestRecallSorted_深度扩展：depth=2 应拉进邻居的邻居
func TestRecallSorted_深度扩展(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	if _, _, err := g.Commit([]Triple{
		// ★ 用双字名：validEntityName 拒单字（len(rune) < 2），
		//   那是 3bafe1c 定下的口径，不是 bug。
		{Subject: "甲组", Relation: "指向", Object: "乙组", Confidence: 1.0},
		{Subject: "乙组", Relation: "指向", Object: "丙组", Confidence: 1.0},
		{Subject: "丙组", Relation: "指向", Object: "丁组", Confidence: 1.0},
	}, "main", 0); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		depth int
		want  bool
		why   string
	}{
		{1, false, "depth=1 到乙为止，不该有丙"},
		{2, true, "depth=2 应扩展到丙"},
	} {
		res, err := g.RecallSorted([]string{"甲组"}, nil, tc.depth, "", "", SortRelevance)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, e := range res.Entities {
			names[e.Name] = true
		}
		if got := names["丙组"]; got != tc.want {
			t.Errorf("★ depth=%d 丙组=%v，%s", tc.depth, got, tc.why)
		}
	}
}

// TestRecallSorted_种子实体：seedEntities 必须按名精确命中
func TestRecallSorted_种子实体(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	if _, _, err := g.Commit([]Triple{
		{Subject: "唯一主体", Relation: "是", Object: "某值", Confidence: 1.0},
	}, "main", 0); err != nil {
		t.Fatal(err)
	}

	res, err := g.RecallSorted(nil, []string{"唯一主体"}, 1, "", "", SortRelevance)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range res.Entities {
		names[e.Name] = true
	}
	if !names["唯一主体"] {
		t.Errorf("★ seedEntities 应命中「唯一主体」，得到 %v", names)
	}
	if len(res.Relations) == 0 {
		t.Error("★ seedEntities 命中后应带出关系")
	}

	// 不存在的种子不该 panic，且返回空
	res2, err := g.RecallSorted(nil, []string{"不存在的人"}, 1, "", "", SortRelevance)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Entities) != 0 {
		t.Errorf("★ 不存在的种子应返回空，得到 %d 个实体", len(res2.Entities))
	}
}

// TestRecallSorted_会话过滤：sessionFilter 必须真的过滤
func TestRecallSorted_会话过滤(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	// ★ 会话来自 Commit 的 sessionID 参数，不在 Triple 上
	//   （Triple.SentenceText 那行的注释还写着「写入 sentences 表」，
	//    那是旧表时代的残留，实际写的是原句块 —— 另一处待清理的误导）。
	if _, _, err := g.Commit([]Triple{
		{Subject: "会话甲", Relation: "属性", Object: "值甲", Confidence: 1.0},
	}, "s-A", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.Commit([]Triple{
		{Subject: "会话甲", Relation: "属性", Object: "值乙", Confidence: 1.0},
	}, "s-B", 0); err != nil {
		t.Fatal(err)
	}

	res, err := g.RecallSorted([]string{"会话甲"}, nil, 1, "s-A", "", SortRelevance)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  sessionFilter=s-A → 关系 %d 条\n", len(res.Relations))
	for _, r := range res.Relations {
		if r.SessionID != "s-A" {
			t.Errorf("★ 会话过滤失效：拿到 session %q 的关系", r.SessionID)
		}
	}
	if len(res.Relations) == 0 {
		t.Error("★ s-A 会话里明明有「属性=值甲」")
	}
}

// TestRecallSorted_全量分支：Indexer.Sync 的数据源
func TestRecallSorted_全量分支(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	if _, _, err := g.Commit([]Triple{
		{Subject: "全量甲", Relation: "是", Object: "全量乙", Confidence: 1.0},
		{Subject: "全量丙", Relation: "是", Object: "全量丁", Confidence: 1.0},
	}, "main", 0); err != nil {
		t.Fatal(err)
	}

	res, err := g.RecallSorted(nil, nil, 1, "", "", SortRelevance)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range res.Entities {
		names[e.Name] = true
	}
	fmt.Printf("  全量分支 → 实体 %d，关系 %d\n", len(res.Entities), len(res.Relations))
	for _, want := range []string{"全量甲", "全量乙", "全量丙", "全量丁"} {
		if !names[want] {
			t.Errorf("★ 全量分支应含 %q", want)
		}
	}
	if len(res.Relations) == 0 {
		t.Error("★ 全量分支应带出关系")
	}
}

// TestRecallSorted_软删不可见：status 语义
func TestRecallSorted_软删不可见(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	if _, _, err := g.Commit([]Triple{
		{Subject: "软删主体", Relation: "属性", Object: "旧值", Confidence: 1.0},
	}, "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Purge(map[string]string{"subject_contains": "软删主体"}, "soft"); err != nil {
		t.Fatal(err)
	}

	res, err := g.RecallSorted([]string{"软删主体"}, nil, 1, "", "", SortRelevance)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  软删后召回关系 %d 条\n", len(res.Relations))
	for _, r := range res.Relations {
		if r.TargetName == "旧值" {
			t.Error("★ 软删的边不该被召回")
		}
	}
}
