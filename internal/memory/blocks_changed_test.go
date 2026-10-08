package memory

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// BlocksChangedSince 是星图轻量轮询端点（/memory/graph/pulse）的数据源。
//
// ★ 为何需要它而不是复用 GraphData：后者是全量快照（生产实测 3190 块 +
//
//	2692 边，JSON 约 3.3MB、135ms）。原 pulse 实现是「先取全量再按时间过滤」，
//	实测与全量端点同价（145ms），而它每 10s 被轮询一次 —— 「轻量端点」名不副实。
//	本方法在库侧用 SQL 过滤，响应体只含窗口内的块。
//
// 判据：窗口语义、limit 上限、空窗口三件事。
func TestBlocksChangedSince(t *testing.T) {
	g, err := NewGraphDB(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	for i := 0; i < 50; i++ {
		if _, _, err := g.Commit([]Triple{{
			Subject:      fmt.Sprintf("实体_%d", i),
			Relation:     "关联",
			Object:       fmt.Sprintf("目标_%d", i),
			SentenceText: fmt.Sprintf("句子 %d", i),
		}}, "s", i); err != nil {
			t.Fatal(err)
		}
	}

	// ① 宽窗口：应能取到（每个三元组产出多块：两端实体块 + 原句块）。
	wide, err := g.BlocksChangedSince(time.Now().Add(-time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(wide) < 100 {
		t.Fatalf("宽窗口应取到 100+ 块（50 三元组 × 多块），实际 %d", len(wide))
	}

	// ② 空窗口：future 应恒为 0（这是「无新记忆时不误报」的前提，
	//    否则星图会每次轮询都重放生长动画）。
	future, err := g.BlocksChangedSince(time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(future) != 0 {
		t.Fatalf("未来窗口应返回 0 块，实际 %d", len(future))
	}

	// ③ limit：窗口内块可能很多（如批量迁移），星图只需知道「有新东西」，
	//    必须真的封顶，否则一次迁移就能让响应体回到全量量级。
	limited, err := g.BlocksChangedSince(time.Now().Add(-time.Hour), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 7 {
		t.Fatalf("limit=7 应返回 7 块，实际 %d", len(limited))
	}
}
