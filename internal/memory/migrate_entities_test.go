package memory

import (
	"strings"
	"testing"
	"time"
)

func newMigrateGraph(t *testing.T) *GraphDB {
	t.Helper()
	g := newTestGraph(t)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

func seedLegacy(t *testing.T, g *GraphDB, triples []Triple) {
	t.Helper()
	if _, _, err := g.Commit(triples, "s", 1); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

const fixedEmbed = "fp-migrate"

// ★ 迁移形态：实体 → 句子+块，关系 → 块边。旧表**不删**（由调用方决定）。
func TestMigrateLegacyTextEntities_形态(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{
		{Subject: "值班室分机号", Relation: "是", Object: "4324"},
	})

	res, err := g.MigrateLegacyTextEntities(func(string) ([]float64, string) {
		return []float64{1, 0, 0}, fixedEmbed
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// ★ 2 个实体 → 2 个迁移块 + 2 个原句块（方案 A 后原句也是块）
	//
	// res.Sentences 不再递增：统计口径改为「迁移块数」，
	// 因为 sentences 表已不再被写入。
	if res.Blocks != 2 {
		t.Errorf("2 个实体应得 2 个迁移块，实际 %+v", res)
	}
	if res.Sentences != 0 {
		t.Errorf("迁移不应再创建 sentence 记录（方案 A 退场），实际 %d", res.Sentences)
	}
	// 1 条关系 → 1 条块边
	if res.Edges != 1 {
		t.Errorf("1 条关系应得 1 条边，实际 %d", res.Edges)
	}

	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	// 2 迁移块 + 2 原句块
	if len(blocks) != 4 {
		t.Fatalf("应有 4 块（2 迁移 + 2 原句），实际 %d", len(blocks))
	}
	for _, b := range blocks {
		// ★ 原句块刻意不带向量（溯源锚点，长整句会稀释召回）
		if b.Source == SentenceBlockSource {
			if len(b.Vector) != 0 {
				t.Errorf("原句块 %s 不该带向量，实际 %d 维", b.ID, len(b.Vector))
			}
			continue
		}
		if len(b.Vector) != 3 || b.Fingerprint != fixedEmbed {
			t.Errorf("%s 应带 3 维向量与 %s，实际 %d 维 %q",
				b.ID, fixedEmbed, len(b.Vector), b.Fingerprint)
		}
		if b.Source != "legacy-entity" {
			t.Errorf("%s 应标记来源 legacy-entity，实际 %q", b.ID, b.Source)
		}
	}

	// 每块都应有一条 sentence--contains--> 边
	edges, err := g.MemoryBlockEdges()
	if err != nil {
		t.Fatal(err)
	}
	containsCount := 0
	for _, e := range edges {
		// ★ 起点是**原句块**（block），不再是 sentence 表行 —— 方案 A
		if e.Type == "contains" && e.SourceKind == "block" && e.TargetKind == "block" {
			containsCount++
		}
	}
	if containsCount != 2 {
		t.Errorf("应有 2 条 contains 边，实际 %d（总边数 %d）", containsCount, len(edges))
	}

	// ★ 旧表还在（迁移不删数据，可回滚）
	r, err := g.Recall([]string{"值班室分机号"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Entities) == 0 {
		t.Error("迁移不应删除 entities（由调用方在验证通过后决定清理）")
	}
}

// ★ 幂等：重复迁移不产生重复块。
func TestMigrateLegacyTextEntities_幂等(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{
		{Subject: "主体甲", Relation: "是", Object: "客体乙"},
	})
	embed := func(string) ([]float64, string) { return []float64{1, 0}, fixedEmbed }

	for i := 0; i < 3; i++ {
		if _, err := g.MigrateLegacyTextEntities(embed); err != nil {
			t.Fatalf("第 %d 次迁移: %v", i, err)
		}
	}
	blocks, _ := g.MemoryBlocks()
	// ★ 2 迁移块 + 2 原句块；重复跑不增
	if len(blocks) != 4 {
		t.Fatalf("迁移 3 次后仍应只有 4 块（2 迁移 + 2 原句），实际 %d", len(blocks))
	}
	edges, _ := g.MemoryBlockEdges()
	contains := 0
	for _, e := range edges {
		if e.Type == "contains" {
			contains++
		}
	}
	if contains != 2 {
		t.Fatalf("contains 边应仍是 2 条，实际 %d", contains)
	}
}

// ★ 无 embed 时仍迁移，但块不带向量——不编造零向量。
func TestMigrateLegacyTextEntities_无embed不编造(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{{Subject: "主体甲", Relation: "是", Object: "客体乙"}})

	res, err := g.MigrateLegacyTextEntities(nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocks != 2 {
		t.Fatalf("无 embed 也应迁移，实际 %d 块", res.Blocks)
	}
	if res.SkippedNoVec != 2 {
		t.Errorf("应统计 2 个无向量块，实际 %d", res.SkippedNoVec)
	}
	blocks, _ := g.MemoryBlocks()
	for _, b := range blocks {
		if len(b.Vector) != 0 {
			t.Errorf("%s 不该有向量，实际 %d 维", b.ID, len(b.Vector))
		}
	}
}

// embed 返回 nil 向量时算「算不出」，迁移继续但不计入成功。
func TestMigrateLegacyTextEntities_embed失败仍迁移(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{{Subject: "主体甲", Relation: "是", Object: "客体乙"}})
	res, err := g.MigrateLegacyTextEntities(func(string) ([]float64, string) {
		return nil, "" // 模型加载失败
	})
	if err != nil {
		t.Fatalf("embed 失败不该让迁移失败: %v", err)
	}
	if res.Blocks != 2 {
		t.Fatalf("应仍迁移出 2 块，实际 %d", res.Blocks)
	}
	if res.SkippedNoVec != 2 {
		t.Errorf("应统计 2 个无向量，实际 %d", res.SkippedNoVec)
	}
}

// ★ 孤儿关系（端点实体为空名）被跳过并计数，不编造边。
func TestMigrateLegacyTextEntities_孤儿关系跳过(t *testing.T) {
	g := newMigrateGraph(t)
	// 直接构造一个指向空名实体的关系是不可能的（upsert 拒绝空名），
	// 所以这里测的是「关系两端都存在」时边数正确，作为对照基线。
	seedLegacy(t, g, []Triple{
		{Subject: "主体甲", Relation: "是", Object: "客体乙"},
		{Subject: "主体甲", Relation: "属于", Object: "客体丙"},
	})
	res, err := g.MigrateLegacyTextEntities(nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Edges != 2 {
		t.Errorf("2 条关系应得 2 条边，实际 %d", res.Edges)
	}
	if res.SkippedOrphan != 0 {
		t.Errorf("端点齐全时不该有跳过，实际 %d", res.SkippedOrphan)
	}
}

// 空关系类型用占位名而不是被拒绝（关系的**存在**本身是信息）。
func TestMigrateLegacyTextEntities_空关系类型用占位(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{{Subject: "主体甲", Relation: "   ", Object: "客体乙"}})
	res, err := g.MigrateLegacyTextEntities(nil)
	if err != nil {
		t.Fatalf("空关系类型不该让迁移失败: %v", err)
	}
	if res.Edges != 1 {
		t.Fatalf("空关系类型也应建成边，实际 %d", res.Edges)
	}
	edges, _ := g.MemoryBlockEdges()
	found := false
	for _, e := range edges {
		if e.Type == "related_to" {
			found = true
		}
	}
	if !found {
		t.Errorf("应有占位关系类型的边，实际 %+v", edges)
	}
}

// ★ 事务回滚：迁移中途失败时，**库内容保持迁移前状态**。
//
// 失败注入方式：给两个实体起相同的块 ID 是不行的（ID 由 entity id 派生，
// 天然唯一）。改用「让 putBlockTx 失败」——预先占位一个**非块节点类型**
// 的行占住表结构，或直接删掉 memory_blocks 表。
//
// 删表是最直接的：阶段三第一次 putBlockTx 就会报 no such table，
// 此时前面的 ensureSentenceTx 已经在事务里写过句子 —— 若无回滚，
// 那些句子会留在库里。
func TestMigrateLegacyTextEntities_中途失败整体回滚(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{
		{Subject: "主体甲", Relation: "是", Object: "客体乙"},
	})
	beforeBlocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	beforeSentences := countSentences(t, g)

	// 在阶段三开始前破坏块表：此时阶段一（读）已完成、阶段二（embed）正在跑
	calls := 0
	_, err = g.MigrateLegacyTextEntities(func(string) ([]float64, string) {
		calls++
		if calls == 1 {
			g.mu.Lock()
			_, dbErr := g.db.Exec(`DROP TABLE memory_blocks`)
			g.mu.Unlock()
			if dbErr != nil {
				t.Fatalf("构造失败: %v", dbErr)
			}
			t.Cleanup(func() {
				g.mu.Lock()
				_, _ = g.db.Exec(ddlMemoryBlocks)
				g.mu.Unlock()
			})
		}
		return []float64{1, 0}, "fp-x"
	})
	if err == nil {
		t.Fatal("块表被删后迁移应失败")
	}
	t.Logf("迁移如预期失败: %v", err)

	// 关键断言：块与句子都没留下（回滚生效）
	afterSentences := countSentences(t, g)
	if afterSentences != beforeSentences {
		t.Errorf("失败后句子数应不变（回滚），实际 %d → %d",
			beforeSentences, afterSentences)
	}
	_ = beforeBlocks

	// 恢复表后确认库里确实没有残留的块
	g.mu.Lock()
	_, dbErr := g.db.Exec(ddlMemoryBlocks)
	g.mu.Unlock()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	afterBlocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(afterBlocks) != 0 {
		t.Errorf("失败后不应留下块（回滚），实际 %d 块: %+v", len(afterBlocks), afterBlocks)
	}
}

func countSentences(t *testing.T, g *GraphDB) int {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	var n int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM sentences`).Scan(&n); err != nil {
		t.Fatalf("count sentences: %v", err)
	}
	return n
}

// 块 id 含实体 id 前缀（可读 + 稳定）。
func TestLegacyEntityBlockID_稳定可读(t *testing.T) {
	a := legacyEntityBlockID(42, "值班室分机号")
	b := legacyEntityBlockID(42, "值班室分机号")
	c := legacyEntityBlockID(43, "值班室分机号")
	if a != b {
		t.Error("同实体应得同块 ID")
	}
	if a == c {
		t.Error("不同实体应得不同块 ID")
	}
	if !strings.Contains(a, "blk_ent_42") {
		t.Errorf("块 ID 应含实体 id 便于排查，实际 %q", a)
	}
}

// ★ 值覆盖维度依赖时序：迁移必须保住实体的先后关系。
//
// 背景：实测（真实 chineseclip + 真库）同一属性先后给两个值时，
// 「值班室分机号 4324」与「值班室分机号 4379」短句向量相似度是
// 0.9284 vs 0.9298 —— 数值上区分不了。若迁移把块的时���全写成 NOW()，
// overwrite 检索就彻底没救了。
//
// ★ 断言覆盖**全部**块：初版只查了含关键字的那一个，结果把
// 「CreatedAt 不传」的变异判成通过 —— 假绿。实际漏掉的是没被 UPDATE
// 到的那些实体（它们本就该落 NOW()，但保护要能区分两种情况）。
func TestMigrateLegacyTextEntities_保留时序(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{
		{Subject: "值班室分机号", Relation: "是", Object: "四三七九"},
	})
	// 给**两个**实体都写同一个历史时刻
	g.mu.Lock()
	_, err := g.db.Exec(`UPDATE entities SET created_at = '2026-01-01 10:00:00',
		updated_at = '2026-01-01 10:00:00'`)
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := g.MigrateLegacyTextEntities(nil); err != nil {
		t.Fatal(err)
	}

	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) == 0 {
		t.Fatal("无块")
	}
	want := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	for _, b := range blocks {
		if b.CreatedAt.IsZero() {
			t.Errorf("%s created_at 为零值", b.ID)
			continue
		}
		if !b.CreatedAt.Equal(want) {
			t.Errorf("%s 应继承实体时间 %v，实际 %v（时序被抹平/未传递）",
				b.ID, want, b.CreatedAt)
		}
	}
}

// 多个实体的时间戳必须**不同**（全都落成同一时刻等于抹平时序）。
func TestMigrateLegacyTextEntities_时序不被抹平(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{
		{Subject: "值班室分机号", Relation: "是", Object: "四三七九"},
		{Subject: "新分机号码", Relation: "是", Object: "四三二四"},
	})
	// 主语（较早）与后写入的实体（较晚）
	g.mu.Lock()
	_, err := g.db.Exec(`UPDATE entities SET
		created_at = CASE WHEN name IN ('值班室分机号', '四三七九')
		                  THEN '2026-01-01 10:00:00' ELSE '2026-01-01 11:00:00' END,
		updated_at = CASE WHEN name IN ('值班室分机号', '四三七九')
		                  THEN '2026-01-01 10:00:00' ELSE '2026-01-01 11:00:00' END`)
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := g.MigrateLegacyTextEntities(nil); err != nil {
		t.Fatal(err)
	}
	blocks, _ := g.MemoryBlocks()
	// 主语+宾语各成一块：「值班室分机号」「新分机号码」「四三七九」「四三二四」
	// ★ 4 个实体 → 4 迁移块 + 4 原句块 = 8
	//   （原为 4，方案 A 让原句也成了块）
	if len(blocks) != 8 {
		// 2 迁移块 + 2 原句块
		t.Fatalf("应得 8 块（4 迁移 + 4 原句），实际 %d", len(blocks))
	}
	// 判据：4 个块必须分属两个不同时刻（主语+首宾语 10:00，
	// 次宾语对 11:00）。若全落同一时刻，时序就被抹平了。
	// ★ 注意：不能断言「排序后首两块不等」—— 10:00 那组本来就有 2 块，
	// 同刻是正常的。判据是「时刻的分布」而不是「相邻两块是否相等」。
	seen := map[string]int{}
	for _, b := range blocks {
		seen[b.CreatedAt.UTC().Format("2006-01-02 15:04")]++
	}
	if len(seen) != 2 {
		t.Errorf("块应分属 2 个不同时刻，实际 %d 个：%v", len(seen), seen)
	}
	if seen["2026-01-01 10:00"] != 4 {
		// ★ 每组 2 实体 × 2 块（迁移块 + 原句块）= 4 块
		t.Errorf("10:00 组应有 4 块（2 迁移 + 2 原句），实际 %d", seen["2026-01-01 10:00"])
	}
	if seen["2026-01-01 11:00"] != 4 {
		// ★ 同上
		t.Errorf("11:00 组应有 4 块（2 迁移 + 2 原句），实际 %d", seen["2026-01-01 11:00"])
	}
}

// 旧库时间格式不认得时：宁可零值兜底，也不编造错的时刻。
func TestParseLegacyTime(t *testing.T) {
	cases := []struct {
		in   string
		want string // 空 = 期望零值
	}{
		{"2026-01-01 10:00:00", "2026-01-01 10:00:00"},
		{"2026-01-01T10:00:00Z", "2026-01-01 10:00:00"},
		{"2026-01-01 10:00:00.123456", "2026-01-01 10:00:00.123456"},
		{"2026-01-01", "2026-01-01 00:00:00"},
		{"", ""},
		{"   ", ""},
		{"去年某天", ""},
	}
	for _, c := range cases {
		got := parseLegacyTime(c.in)
		if c.want == "" {
			if !got.IsZero() {
				t.Errorf("parseLegacyTime(%q) 应为零值，实际 %v", c.in, got)
			}
			continue
		}
		want, err := time.Parse("2006-01-02 15:04:05", c.want)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(want) {
			t.Errorf("parseLegacyTime(%q) = %v，期望 %v", c.in, got, want)
		}
	}
}

// ★★ 报告数必须等于实际写入数。
//
// 生产快照实测（1294 实体 / 980 relations）：
//
//	迁移报告   边 980
//	边表实际   959        ← 差 21
//
// 根因：addBlockEdgeTx 用 `INSERT OR IGNORE` 且**不检查 RowsAffected**，
// 而 `res.Edges++` 照加。relations 表里有 20 组 (src,tgt,type) 完全重复
// （各 2 次），980 → 去重 959。
//
// ★ 与 941e6b9 同类：那是 relations 口径（active vs 全表），
//
//	这是边去重口径。两次都是「报告数 ≠ 实际写入数」，
//	而用户会把报告数当承诺。
func TestMigrate_报告数等于实际写入数(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{
		{Subject: "值班室分机号", Relation: "是", Object: "4324"},
	})
	// ★ 生产库里有 20 组 (src,tgt,type) 完全重复的关系行。
	//   seedLegacy 用 Upsert（第二次覆盖第一次）⇒ 造不出重复，
	//   所以直接插库 —— 这也是之前判据「通过」却没测到问题的原因。
	if _, err := g.db.Exec(`INSERT INTO relations
		(source_id, target_id, relation_type, confidence, status)
		SELECT source_id, target_id, relation_type, confidence, status
		FROM relations LIMIT 1`); err != nil {
		t.Fatalf("插入重复关系: %v", err)
	}

	embed := func(string) ([]float64, string) { return []float64{1, 0}, fixedEmbed }
	res, err := g.MigrateLegacyTextEntities(embed)
	if err != nil {
		t.Fatal(err)
	}

	edges, err := g.MemoryBlockEdges()
	if err != nil {
		t.Fatal(err)
	}
	var relEdges int
	for _, e := range edges {
		if e.Type != "contains" {
			relEdges++
		}
	}

	if res.Edges != relEdges {
		t.Errorf("★ 报告边数 %d ≠ 实际写入 %d —— 报告会骗人", res.Edges, relEdges)
	}
}
