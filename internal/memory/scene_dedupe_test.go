package memory

// 场景去重（R3）的判据。
//
// 缺口事实：图整理心跳（mergeLoop → detectEntityMerge）唯一的遍历入口是
// Recall(nil,nil,1,"")，而该全量路径只查 entities(graph.go:609) 与
// relations(graph.go:631) —— scenes 不在其中。于是同一个场面的双胞胎键
// （auto:chan:qq+part:morning ↔ auto:chan:qq_part:morning）从建库起
// 无人发现：强度一路涨到 270、6 个 features、0 条记忆，而孪生的那个
// 持有 201 条记忆却有 0 features。两套特征体系各活各的。
//
// 判据参照物在生产代码之外：期望值是「归一化后同名的场景必须合成一个」
// 与「合并后记忆/特征/强度都不丢」这两条不变量，不引用被测实现。

import (
	"os"
	"strings"
	"testing"
)

// insertSceneRow 直写一行 scenes（绕开建键路径），用来复现历史双胞胎。
func insertSceneRow(t *testing.T, g *GraphDB, key, origin string, strength int) int64 {
	t.Helper()
	res, err := g.db.Exec(
		`INSERT INTO scenes (key, strength, origin) VALUES (?, ?, ?)`,
		key, strength, origin)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func addFeature(t *testing.T, g *GraphDB, sceneID int64, feature string, weight float64) {
	t.Helper()
	if _, err := g.db.Exec(
		`INSERT INTO scene_features (scene_id, feature, weight) VALUES (?, ?, ?)`,
		sceneID, feature, weight); err != nil {
		t.Fatal(err)
	}
}

func addRef(t *testing.T, g *GraphDB, sceneID int64, kind string, refID int64, weight float64) {
	t.Helper()
	if _, err := g.db.Exec(
		`INSERT INTO scene_refs (scene_id, kind, ref_id, weight) VALUES (?, ?, ?, ?)`,
		sceneID, kind, refID, weight); err != nil {
		t.Fatal(err)
	}
}

func sceneRowCount(t *testing.T, g *GraphDB) int {
	t.Helper()
	var n int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM scenes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func sceneRefCount(t *testing.T, g *GraphDB) int {
	t.Helper()
	var n int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM scene_refs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// R3 核心：归一化后同名的双胞胎必须合成一个。
func TestDedupeScenes_MergesNormalizedTwins(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	// 复现现网形态：一个带 +（有 features、无 refs），一个带 _（有 refs、无 features）
	plus := insertSceneRow(t, g, "auto:chan:qq+part:morning", "emergent", 270)
	under := insertSceneRow(t, g, "auto:chan:qq_part:morning", "emergent", 1)
	addFeature(t, g, plus, "chan:qq", 1.0)
	addFeature(t, g, plus, "part:morning", 0.2)
	addRef(t, g, under, "relation", 42, 1.0)
	addRef(t, g, under, "entity", 7, 1.0)

	merged, err := g.DedupeScenes()
	if err != nil {
		t.Fatalf("DedupeScenes 出错: %v", err)
	}
	if merged != 1 {
		t.Errorf("应合并 1 组，实际 %d", merged)
	}
	if n := sceneRowCount(t, g); n != 1 {
		t.Fatalf("合并后 scenes 应剩 1 行，实际 %d:\n%v", n, sceneKeys(t, g))
	}
}

// 合并绝不能丢记忆：refs 要全部转到存活的那一行。
func TestDedupeScenes_PreservesRefs(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	plus := insertSceneRow(t, g, "auto:chan:mc:event+topic:mc", "emergent", 50)
	under := insertSceneRow(t, g, "auto:chan:mc:event_topic:mc", "emergent", 1)
	addFeature(t, g, plus, "chan:mc:event", 1.0)
	addRef(t, g, under, "relation", 1, 1.0)
	addRef(t, g, under, "relation", 2, 1.0)
	addRef(t, g, under, "entity", 3, 1.0)

	if _, err := g.DedupeScenes(); err != nil {
		t.Fatal(err)
	}

	// 一条都不能少
	if got := sceneRefCount(t, g); got != 3 {
		t.Fatalf("合并后 scene_refs 应有 3 条，实际 %d —— 合并丢了记忆", got)
	}
	// 且全部挂在存活的那一行上
	var sceneID int64
	if err := g.db.QueryRow(`SELECT id FROM scenes LIMIT 1`).Scan(&sceneID); err != nil {
		t.Fatal(err)
	}
	var onSurvivor int
	if err := g.db.QueryRow(
		`SELECT COUNT(*) FROM scene_refs WHERE scene_id = ?`, sceneID).Scan(&onSurvivor); err != nil {
		t.Fatal(err)
	}
	if onSurvivor != 3 {
		t.Errorf("存活场景上只挂了 %d/3 条 refs", onSurvivor)
	}
}

// 特征取并集：哪一侧有就保留，权重取大。
func TestDedupeScenes_UnionsFeatures(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	plus := insertSceneRow(t, g, "auto:chan:qq+topic:排班", "emergent", 3)
	under := insertSceneRow(t, g, "auto:chan:qq_topic:排班", "emergent", 2)
	addFeature(t, g, plus, "chan:qq", 1.0)
	addFeature(t, g, plus, "topic:排班", 0.4)
	addFeature(t, g, under, "chan:qq", 1.0)
	addFeature(t, g, under, "peer:boss", 1.0) // 只在孪生那侧有

	if _, err := g.DedupeScenes(); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM scene_features`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("特征并集应为 3（chan:qq / topic:排班 / peer:boss），实际 %d", n)
	}
}

// strength 要相加：两个场景各被遇到过 N 次，合起来就该是 2N。
func TestDedupeScenes_SumsStrength(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	insertSceneRow(t, g, "auto:chan:qq+part:morning", "emergent", 270)
	insertSceneRow(t, g, "auto:chan:qq_part:morning", "emergent", 1)

	if _, err := g.DedupeScenes(); err != nil {
		t.Fatal(err)
	}

	var strength int
	if err := g.db.QueryRow(`SELECT strength FROM scenes LIMIT 1`).Scan(&strength); err != nil {
		t.Fatal(err)
	}
	if strength != 271 {
		t.Errorf("strength 应为 270+1=271，实际 %d", strength)
	}
}

// 幂等：跑两次，第二次必须是 0 合并、0 行变化。
func TestDedupeScenes_IsIdempotent(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	insertSceneRow(t, g, "auto:chan:qq+part:morning", "emergent", 270)
	insertSceneRow(t, g, "auto:chan:qq_part:morning", "emergent", 1)
	addRef(t, g, 2, "relation", 1, 1.0)

	if n, err := g.DedupeScenes(); err != nil || n != 1 {
		t.Fatalf("首次应合并 1 组，实际 n=%d err=%v", n, err)
	}
	rows, refs, strength := sceneRowCount(t, g), sceneRefCount(t, g), 271

	n2, err := g.DedupeScenes()
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("第二次不应再合并，实际 %d", n2)
	}
	if got := sceneRowCount(t, g); got != rows {
		t.Errorf("第二次改变了行数: %d → %d", rows, got)
	}
	if got := sceneRefCount(t, g); got != refs {
		t.Errorf("第二次改变了 refs: %d → %d", refs, got)
	}
	var s int
	if err := g.db.QueryRow(`SELECT strength FROM scenes LIMIT 1`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s != strength {
		t.Errorf("第二次把 strength 改成了 %d（应保持 %d）", s, strength)
	}
}

// 不同场面不能被合到一起：只有归一化后**完全同名**才算重复。
// 这是本判据的另一半——去重不能变成"把相似的一律合并"。
func TestDedupeScenes_KeepsDistinctScenes(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	insertSceneRow(t, g, "auto:chan:qq", "emergent", 5)
	insertSceneRow(t, g, "auto:chan:webui", "emergent", 6)
	insertSceneRow(t, g, "auto:chan:qq_part:morning", "emergent", 270)
	addFeature(t, g, 1, "chan:qq", 1.0)
	addFeature(t, g, 2, "chan:webui", 1.0)

	n, err := g.DedupeScenes()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("三个不同场面不该被合并，实际合并了 %d 组", n)
	}
	if got := sceneRowCount(t, g); got != 3 {
		t.Errorf("应有 3 个场景，实际 %d: %v", got, sceneKeys(t, g))
	}
}

// 声明场景与涌现场景归一化后同名时也要合——现网 chan:context_archived
// 就是这么来的（origin='emergent' 却长得像声明键）。
func TestDedupeScenes_MergesAcrossOrigins(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	insertSceneRow(t, g, "chan:qq", "declared", 281)
	insertSceneRow(t, g, "chan:QQ", "declared", 3) // 仅大小写不同

	n, err := g.DedupeScenes()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("大小写不同的同名场景应合并，实际 %d 组", n)
	}
	if got := sceneRowCount(t, g); got != 1 {
		t.Errorf("应剩 1 行，实际 %d: %v", got, sceneKeys(t, g))
	}
}

// 归一化口径必须与写/读侧一致：这里独立复算一遍期望名，
// 避免判据与被测实现共用同一个 NormalizeSceneKey 而一起错。
func TestDedupeScenes_UsesSameNormalizationAsWriteSide(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	// 写侧（effectiveScenes）会把 "auto:chan:qq+part:morning" 归一成什么？
	wantKey := NormalizeSceneKey("auto:chan:qq+part:morning")
	if strings.Contains(wantKey, "+") {
		t.Fatalf("前提不成立：NormalizeSceneKey 未处理 '+'，got %q", wantKey)
	}
	insertSceneRow(t, g, "auto:chan:qq+part:morning", "emergent", 1)
	insertSceneRow(t, g, wantKey, "emergent", 1)

	if _, err := g.DedupeScenes(); err != nil {
		t.Fatal(err)
	}
	if got := sceneRowCount(t, g); got != 1 {
		t.Errorf("建键侧的未归一化键应与写侧归一化后的键合并，实际剩 %d 行", got)
	}
}
