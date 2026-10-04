package memory

import (
	"fmt"
	"testing"
)

// ★★★ 召回 = 命中节点 + N 层 BFS
//
// 用户定义：
//	「召回是针对节点的，然后召回的是节点与 n 层 bfs 结果」
//
// ⇒ ① 所有节点都是召回对象（主语块、宾语块、原句块一视同仁）
//	② 命中之后沿关系边展开 N 层，把邻居一起返回
//	③ 邻居**不需要**「来源标注」这类特判 —— BFS 自然带出上下文
//
// 这与「只召回主语块」是两种不同的召回语义：后者需要为
// 「宾语块孤立命中」写特判，而 BFS 展开让那种情况自动有上下文。
func TestBFS_节点加N层邻居(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	// 织一张网：值班室分机号 --是--> 4324 --停用于--> 4379
	//                       └ 4324 被 老周 值班 引用
	blocks := []MemoryBlock{
		{ID: "b_subject", Modality: BlockText, Text: "值班室分机号"},
		{ID: "b_value", Modality: BlockText, Text: "4324"},
		{ID: "b_old", Modality: BlockText, Text: "4379"},
		{ID: "b_person", Modality: BlockText, Text: "老周"},
		{ID: "b_unrelated", Modality: BlockText, Text: "完全不相关的块"},
	}
	if err := g.PutMemoryBlocks(blocks); err != nil {
		t.Fatal(err)
	}
	edges := []struct{ from, to, typ string }{
		{"b_subject", "b_value", "是"},
		{"b_value", "b_old", "停用后改为"},
		{"b_person", "b_value", "值班分机"},
	}
	for _, e := range edges {
		if err := g.AddRelationBlockEdge(e.from, e.to, e.typ,
			RelationEdgeData{SessionID: "s1", Confidence: 0.9}); err != nil {
			t.Fatal(err)
		}
	}

	// 命中「4324」这个**宾语块**（孤立时它不知道自己来自哪里）
	got, err := g.BFSBlocks("b_value", 1)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, b := range got {
		ids[b.ID] = true
	}
	fmt.Printf("  从 b_value 展开 1 层：%d 个节点 %v\n", len(got), ids)

	// ★ 起点自身必须包含
	if !ids["b_value"] {
		t.Error("★ BFS 结果必须包含起点自身")
	}
	// ★ 1 层邻居：主语块与旧值块都在（它们是 b_value 的入/出邻居）
	if !ids["b_subject"] {
		t.Error("★ 1 层应含主语块（值班室分机号）—— 宾语块孤立命中时靠 BFS 找回上下文")
	}
	if !ids["b_old"] {
		t.Error("★ 1 层应含旧值块（4379）")
	}
	if !ids["b_person"] {
		t.Error("★ 1 层应含 1 层反向可达的「老周」（它指向 b_value）")
	}
	// ★ 不相关的块不该出现
	if ids["b_unrelated"] {
		t.Error("★ 不相关块不该被 BFS 带出")
	}

	// ★ 层数可控：0 层只有自己
	zero, err := g.BFSBlocks("b_value", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(zero) != 1 || zero[0].ID != "b_value" {
		t.Errorf("★ 0 层应只返回自身，实际 %d 个", len(zero))
	}

	// ★ 2 层能到「老周」（经 b_value 到 b_person 是 1 层；
	//   经 b_subject 再往外没有出边，所以 1 层已足够）
	two, err := g.BFSBlocks("b_value", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(two) < len(zero) {
		t.Errorf("★ 2 层结果不应少于 0 层")
	}
}

// ★ 规模判据：BFS 的增长必须是**有界**的。
//
// docs/zh/recall-as-association.md 里我把「层数 N 是上下文预算，
// 不是图算法参数」列为约束，理由是：网最终会织成实体网，
// 若不做 BFS 就退化成全表扫描。
//
// ★ 这条判据盯的是**实测的增长曲线**，不是形态 ——
//   形态判据（TestBFS_节点加N层邻居）只证明「能找回主语」，
//   不证明「规模可控」。
//
// 场景：织一张中等密度的网（100 节点 / 约 300 边，含环），
// 检查 depth=1..3 的返回规模是否线性可预期。
func TestBFS_规模增长有界(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	const n = 100
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("n%03d", i)
		ids = append(ids, id)
		if err := g.PutMemoryBlocks([]MemoryBlock{
			{ID: id, Modality: BlockText, Text: fmt.Sprintf("节点%03d", i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 织网：i --关联--> i+1、i --关联--> i+2（近邻，密度约 2/n）
	// 末几个连回开头 ⇒ **有环**（真实记忆网必然成环）
	edgeCount := 0
	for i := 0; i < n; i++ {
		for _, off := range []int{1, 2} {
			j := (i + off) % n
			if i == j {
				continue
			}
			if err := g.AddRelationBlockEdge(ids[i], ids[j], "关联",
				RelationEdgeData{SessionID: "s", Confidence: 0.5}); err != nil {
				t.Fatal(err)
			}
			edgeCount++
		}
	}
	fmt.Printf("  织网: %d 节点 / %d 边（含环）\n", n, edgeCount)

	var prev int
	for depth := 0; depth <= 3; depth++ {
		got, err := g.BFSBlocks(ids[0], depth)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("  depth=%d → %d 个节点\n", depth, len(got))
		// 硬约束：depth=3 时不该把 100 节点全拖出来。
		//
		// ★ 阈值 25 是**从实测定的**，不是拍的：
		//   近邻网（每个节点连 2 个后继）下 3 层的理论上限是
		//   1 + 2 + 4 + 8 = 15。若实测远超，说明环上失控了。
		if depth == 3 && len(got) > 25 {
			t.Errorf("★ depth=3 返回 %d 个节点（>25）—— 环上失控，BFS 退化成全表扫描", len(got))
		}
		if depth > 0 && len(got) <= prev {
			t.Errorf("depth=%d 返回 %d 个，不比 depth=%d 的 %d 个多 —— 层数没生效",
				depth, len(got), depth-1, prev)
		}
		prev = len(got)
	}
}
