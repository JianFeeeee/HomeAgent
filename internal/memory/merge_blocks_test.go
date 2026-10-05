package memory

import (
	"fmt"
	"testing"
)

// ═══════════════════════════════════════════════════════════════
//  MergeBlocks —— 块合并判据
//
//  ★ 与旧 MergeEntities 的语义对齐，但按块的特性调整：
//
//  旧（entities/relations，85 行）
//      1. source 的关系重定向到 target
//      2. mention_count 相加
//      3. 删自引用关系
//      4. 删 source 实体
//
//  新（blocks/edges）
//      1. source 块的**关系边**重定向到 target 块
//      2. 删合并产生的**自环边**（source→X 与 target→X 重定向后同端）
//      3. **删 source 块本身**（不留 @merged_ 残留）
//      4. 结构边（contains）也要处理 —— 否则原句块会指向已删的块
//      5. 场景引用同步
//
//  ★★ 与旧实现的一个本质差异
//  块 ID 是**内容派生**的（blk_ent_<hash(name)>），所以
//  「张先生」和「张三」是两个不同的块。合并不是改端点，
//  而是**让源块消失并把它的边改指向目标块**。
//  这意味着边的端点值会变 —— 而旧实现在这一点上反而更简单
//  （改 relations.source_id 即可，实体 ID 不变）。
// ═══════════════════════════════════════════════════════════════

// TestMergeBlocks_基础合并：边重定向 + 源块消失
func TestMergeBlocks_基础合并(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	if _, _, err := g.Commit([]Triple{
		{Subject: "张先生", Relation: "身份", Object: "值班长", Confidence: 1.0},
		{Subject: "张三", Relation: "负责", Object: "billing服务", Confidence: 1.0},
	}, "main", 0); err != nil {
		t.Fatal(err)
	}

	src, err := g.BlockByText("张先生")
	if err != nil || src == nil {
		t.Fatalf("块源不存在: %v", err)
	}
	dst, err := g.BlockByText("张三")
	if err != nil || dst == nil {
		t.Fatalf("块目标不存在: %v", err)
	}

	n, err := g.MergeBlocks("张先生", "张三")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  合并重定向 %d 条边\n", n)
	if n != 1 {
		t.Errorf("★ 应重定向 1 条边，实际 %d", n)
	}

	// ★ 源块必须彻底消失
	after, err := g.BlockByText("张先生")
	if err != nil {
		t.Fatal(err)
	}
	if after != nil {
		t.Errorf("★ 源块应被删除，仍存在: %+v", after)
	}

	// ★ 目标块继承源块的关系
	res, err := g.RecallSorted([]string{"张三"}, nil, 1, "", "", SortRelevance)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range res.Relations {
		names[r.TargetName] = true
	}
	fmt.Printf("  合并后「张三」的关系目标: %v\n", names)
	if !names["值班长"] {
		t.Errorf("★ 「张三」应继承源块的关系「值班长」，实际 %v", names)
	}
	if !names["billing服务"] {
		t.Errorf("★ 「张三」自己的关系被误删，实际 %v", names)
	}
}

// TestMergeBlocks_自环清理：两条边重定向后同端，必须去重
func TestMergeBlocks_自环清理(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	// 同一人对同一对象说了两件事 —— 合并后都会指向 target→X
	if _, _, err := g.Commit([]Triple{
		{Subject: "李四", Relation: "喜欢", Object: "咖啡", Confidence: 1.0},
		{Subject: "李四", Relation: "讨厌", Object: "咖啡", Confidence: 1.0},
		{Subject: "王五", Relation: "评价", Object: "咖啡", Confidence: 1.0},
	}, "main", 0); err != nil {
		t.Fatal(err)
	}

	n, err := g.MergeBlocks("李四", "王五")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  合并「李四」→「王五」重定向 %d 条边\n", n)
	// ★ 王五→咖啡 与 李四→咖啡 重定向后同端 ⇒ 自环，必须被删
	//   保留它们会让王五「喜欢咖啡」「讨厌咖啡」自相矛盾。
	edges, _, err := g.NeighbourEdgesOfBlock(mustBlockID(t, g, "王五"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  「王五」合并后有 %d 条边\n", len(edges))
	for _, e := range edges {
		if e.Peer.Text == "咖啡" && e.SourceID == e.TargetID {
			t.Error("★ 自环边未被清理")
		}
	}
}

// TestMergeBlocks_结构边处理：contains 不能悬空
func TestMergeBlocks_结构边不被误删(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	if _, _, err := g.Commit([]Triple{
		{Subject: "赵六", Relation: "状态", Object: "在线", Confidence: 1.0,
			SentenceText: "赵六现在在线"},
		// ★ 合并需要**两端都存在**（目标不存在时报错，见「不存在的块」判据）
		{Subject: "钱七", Relation: "状态", Object: "离线", Confidence: 1.0},
	}, "main", 0); err != nil {
		t.Fatal(err)
	}

	// 先确认原句块与 contains 结构边存在
	srcBlkID := mustBlockID(t, g, "赵六")
	edges, _, err := g.NeighbourEdgesOfBlock(srcBlkID)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  合并前「赵六」边数 %d\n", len(edges))

	if _, err := g.MergeBlocks("赵六", "钱七"); err != nil {
		t.Fatal(err)
	}

	// ★ 源块消失了 ⇒ 指向它的 contains 边（source=原句块,target=源块）
	//   必须一并消失，否则块边表出现悬空端点。
	// ★ 关键：源块被删后，任何指向它的边都不能悬空。
	//   悬空端点会让 graphNodeExists 校验失败，后续引用它的写入全被拒。
	g.mu.RLock()
	var dangling int
	if err := g.db.QueryRow(`
		SELECT COUNT(*) FROM memory_block_edges
		WHERE (source_kind='block' AND source_id = ?)
		   OR (target_kind='block' AND target_id = ?)`,
		srcBlkID, srcBlkID).Scan(&dangling); err != nil {
		g.mu.RUnlock()
		t.Fatalf("查悬空边: %v", err)
	}
	g.mu.RUnlock()
	if dangling != 0 {
		t.Errorf("★ 源块删除后仍有 %d 条边指向它（端点悬空）", dangling)
	}

	// ★ 原句块仍可解析（内容派生的，合并不该删原句）
	if blk, err := g.BlockByText("赵六现在在线"); err != nil || blk == nil {
		t.Errorf("★ 原句块不该被合并删除: %v", err)
	}
}

// TestMergeBlocks_重复合并要报错：源块不存在不等于成功
func TestMergeBlocks_幂等(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	if _, _, err := g.Commit([]Triple{
		{Subject: "甲一", Relation: "属性", Object: "值一", Confidence: 1.0},
		{Subject: "乙一", Relation: "属性", Object: "值二", Confidence: 1.0},
	}, "main", 0); err != nil {
		t.Fatal(err)
	}

	if _, err := g.MergeBlocks("甲一", "乙一"); err != nil {
		t.Fatal(err)
	}
	// 再合并一次：源块已被删除 ⇒ 必须报错。
	//
	// ★ 不做成幂等（0, nil）：源块不存在可能是「已合并」也可能是
	//   「名字写错了」，两者返回同一个结果会让调用方无法区分 ——
	//   而后者是 bug，却表现为成功。
	if n, err := g.MergeBlocks("甲一", "乙一"); err == nil {
		t.Errorf("★ 重复合并应报错（源块已不存在），实际返回 n=%d", n)
	}
}

// TestMergeBlocks_不存在的块要明确报错
func TestMergeBlocks_不存在的块(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	if _, _, err := g.Commit([]Triple{
		{Subject: "真实主体", Relation: "属性", Object: "某值", Confidence: 1.0},
	}, "main", 0); err != nil {
		t.Fatal(err)
	}

	if _, err := g.MergeBlocks("不存在的人", "真实主体"); err == nil {
		t.Error("★ 合并不存在的源块应报错（而不是静默成功）")
	}
	if _, err := g.MergeBlocks("真实主体", "也不存在的人"); err == nil {
		t.Error("★ 合并到不存在的目标块应报错")
	}
}

func mustBlockID(t *testing.T, g *GraphDB, text string) string {
	t.Helper()
	b, err := g.BlockByText(text)
	if err != nil {
		t.Fatalf("BlockByText(%q): %v", text, err)
	}
	if b == nil {
		t.Fatalf("块 %q 不存在", text)
	}
	return b.ID
}
