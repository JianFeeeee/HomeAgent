package memory

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Introspect 曾是内核状态页最大的单点开销（2026-10-08 修）。
//
// 生产实测（3364 块 / 2829 边）：**507ms**。
// 根因：hotspots 的度数写成**相关标量子查询**——对每个块执行一次
//
//	SELECT COUNT(*) FROM memory_block_edges
//	 WHERE source_id = b.id OR target_id = b.id
//
// EXPLAIN 是 SCAN b → CORRELATED SCALAR SUBQUERY → SCAN e，
// 即 3364 × 2829 ≈ **950 万次行扫描**，且 ORDER BY 前必须算完全部块的度数。
//
// 修法：边表按端点 UNION ALL + GROUP BY 聚合一次，再 LEFT JOIN 回块表。
// 实测同数据 **507ms → 11ms**，且新旧输出逐字节一致。
//
// 为何用源码断言兜住「形状」而不只靠耗时：小库上两种写法都快，
// 只有大库才暴露；而形状（有没有相关子查询）是**静态可判定**的，
// 回归时立刻红，不必等库长大才发现。
func TestIntrospect_HotspotsQueryIsNotCorrelated(t *testing.T) {
	src := readSource(t, "graph.go")

	// 定位 hotspots 那条查询：用其返回值行的**最后**一次出现（前面的注释里
	// 也提到 memory_hotspots，用 Index 会截到注释处，取到的片段里根本没有查询）。
	i := strings.LastIndex(src, `"memory_hotspots": hotspots`)
	if i < 0 {
		t.Fatal("找不到 memory_hotspots 返回值")
	}
	seg := src[:i]
	q := strings.LastIndex(seg, "g.db.Query(")
	if q < 0 {
		t.Fatal("找不到 hotspots 查询")
	}
	// 从查询起点到返回值：这段里就应当只有 hotspots 那一条 SELECT。
	query := seg[q:]
	if len(query) > 4000 {
		t.Fatalf("提取的查询片段异常长(%d)，定位可能错了", len(query))
	}

	// ① 度数必须来自**预聚合**（GROUP BY），不是逐行子查询。
	if !strings.Contains(query, "GROUP BY id") {
		t.Error("hotspots 度数必须用一次聚合（GROUP BY）算出，不能是相关子查询：\n" +
			"  相关子查询 = 每块一次边表扫描 = 3364 × 2829 ≈ 950 万次行访问\n" +
			"  实测 Introspect 507ms → 11ms 就是这一处的差别")
	}
	// ② 明确禁止那个形态：括号里直接嵌 COUNT(*) 子查询。
	if strings.Contains(query, "COALESCE((SELECT COUNT(*) FROM memory_block_edges e") {
		t.Error("hotspots 又退回相关子查询了（COALESCE((SELECT COUNT(*) ... WHERE (e.source_id = b.id OR ...)）")
	}
	// ③ 聚合形态必须双边都算（source 与 target 都是端点）。
	if !strings.Contains(query, "SELECT source_id AS id") ||
		!strings.Contains(query, "SELECT target_id AS id") {
		t.Error("度数聚合必须同时含 source_id 与 target_id 两个端点，缺一个就会漏度")
	}
}

// TestIntrospect_LargeGraphIsFast 用真实规模的库钉住耗时上限。
//
// 源码断言只能守形状；这条守「形状对了但仍慢」的情况（比如聚合子查询里
// 又嵌了别的全表扫）。规模按生产量级造（2000 块 / 1500 边），
// 上限留足余量但足以抓住相关子查询那档（2000×1500 ≈ 300 万次访问）。
func TestIntrospect_LargeGraphIsFast(t *testing.T) {
	g := newPerfGraph(t, 2000, 1500)

	start := time.Now()
	if _, err := g.Introspect(); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	// 相关子查询版在这个规模约 250-500ms；聚合版 < 20ms。
	// 60ms 的门槛既能抓住退化，又不会因 CI 机器慢而误报。
	if elapsed > 60*time.Millisecond {
		t.Errorf("Introspect 在 2000 块/1500 边上耗时 %v，超过 60ms 门槛。\n"+
			"  相关子查询版本会落到几百毫秒；若本条变红，检查 hotspots 是否退回逐块计数。",
			elapsed)
	}
}

// newPerfGraph 造一个指定规模的图：nBlocks 个块、nEdges 条边（互相引用）。
func newPerfGraph(t *testing.T, nBlocks, nEdges int) *GraphDB {
	t.Helper()
	g, err := NewGraphDB(filepath.Join(t.TempDir(), "perf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })

	ids := make([]string, 0, nBlocks)
	for i := 0; i < nBlocks; i++ {
		id := "blk_" + strconv.Itoa(i)
		ids = append(ids, id)
		if err := g.PutMemoryBlocks([]MemoryBlock{{
			ID: id, Modality: BlockText, Text: "实体 " + strconv.Itoa(i),
			Source: "triple", SemanticType: "Concept",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < nEdges; i++ {
		a := ids[i%len(ids)]
		b := ids[(i*7+3)%len(ids)]
		if a == b {
			continue
		}
		if err := g.AddMemoryBlockEdge("block", a, "block", b, "关联"); err != nil {
			t.Fatal(err)
		}
	}
	return g
}
