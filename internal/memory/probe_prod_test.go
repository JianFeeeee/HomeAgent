// 生产规模召回验证 —— 在**快照**上跑，不碰生产库。
//
// ★ 为什么必须用快照
// ----------------------------
// 生产库 /home/newqqagent/memory/graph.db 有 1294 entities / 980 relations /
// 98 blocks，而且它**在跑**。本验证需要补向量（98 个块里大部分没有），
// 而补向量会：
//  1. 与线上 1294 个实体抢写锁
//  2. 改写向量空间，污染线上召回
//
// 所以流程固定为：sqlite3 .backup 取一致性快照 → 在副本上补向量 →
// 在副本上跑探针 → 只把**结论**带回生产决策。
//
// ★ 判据按维度分开，因为失效原因完全不同
// ------------------------------------
//
//	casual     常规查询，看基础召回
//	overwrite  值覆盖，看仲裁（本次改动的目标维度）
//	confusable 易混，看同向量空间下的区分度
//	abstention 库中没有的东西，**不该编造** —— 最危险的一类，
//	           单列出来因为它的失败比误召回更严重
package memory

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

type prodProbe struct {
	name    string
	dim     string
	query   string
	want    string
	notWant string
	abstain bool // 期望「答不出」：不该召回任何块
}

var prodProbes = []prodProbe{
	// casual：常规查询
	{name: "插件目录", dim: "casual", query: "homeagent 插件装在哪个目录", want: "plugins"},
	{name: "记忆库路径", dim: "casual", query: "graph.db 存在哪里", want: "graph.db"},

	// overwrite：值覆盖（本次改动的目标维度）
	{name: "值班分机-覆盖", dim: "overwrite", query: "现在的值班室分机号是多少",
		want: "4324", notWant: "4379"},
	{name: "值班人-覆盖", dim: "overwrite", query: "现在谁值班", want: "老周"},

	// confusable：两个相近的东西，别答错对象
	{name: "计费端口不混admin", dim: "confusable",
		query: "billing 服务的监听端口", want: "9090", notWant: "8080"},

	// ★ abstention：库里根本没有，不该编造
	{name: "不存在的面板", dim: "abstention", query: "grafana 监控面板的端口是多少",
		abstain: true},
	{name: "不存在的负责人", dim: "abstention", query: "谁负责数据库容灾演练",
		abstain: true},
}

// TestProbe_生产规模召回 在生产库快照上跑全维度探针。
//
// 需要 PROD_SNAPSHOT 指向 .backup 出来的副本，且 chineseclip 可用。
func TestProbe_生产规模召回(t *testing.T) {
	db := os.Getenv("PROD_SNAPSHOT")
	if db == "" {
		t.Skip("需要 PROD_SNAPSHOT=<备份文件>")
	}
	g, err := NewGraphDB(db)
	if err != nil {
		t.Fatalf("打开快照失败: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	ad, fp := probeEmbedder(t, "/var/tmp/ha-c/models/chinese-clip-vit-b16-onnx")

	// 先报规模，让结论有分母
	var nBlocks, nVec int
	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		nBlocks++
		if len(b.Vector) > 0 {
			nVec++
		}
	}
	fmt.Printf("    快照规模: %d 块（带向量 %d, %.0f%%）\n",
		nBlocks, nVec, ratio(nVec, nBlocks))

	if nVec == 0 {
		t.Fatal("快照里没有任何向量 —— 先跑 homed-graph-migrate 补向量再验证召回")
	}

	byDim := map[string][2]int{}
	var fails []string
	for _, p := range prodProbes {
		vec, err := ad.VectorizeDense(p.query)
		if err != nil {
			t.Fatalf("向量化失败 %q: %v", p.query, err)
		}
		hits, _, err := g.RecallBlocksWithArbitration(BlockRecallQuery{
			Vector: vec, Fingerprint: fp, TopK: 8, MinScore: 0.25,
		})
		if err != nil {
			t.Fatalf("召回失败: %v", err)
		}
		texts := make([]string, 0, len(hits))
		for _, h := range hits {
			texts = append(texts, h.Block.Text)
		}
		joined := strings.Join(texts, " ｜ ")

		ok := judgeProd(p, texts)
		st := byDim[p.dim]
		st[1]++
		if ok {
			st[0]++
		} else {
			fails = append(fails, fmt.Sprintf("[%s]%s", p.dim, p.name))
		}
		fmt.Printf("    [%s] %-18s %s %s\n", p.dim, p.name,
			mark(ok), trunc(joined, 110))
	}

	fmt.Println()
	totP, totT := 0, 0
	for _, d := range []string{"casual", "overwrite", "confusable", "abstention"} {
		st := byDim[d]
		if st[1] == 0 {
			continue
		}
		totP += st[0]
		totT += st[1]
		verdict := "OK"
		if st[0] < st[1] {
			// ★ abstention 失败比误召回更严重：那是编造
			if d == "abstention" {
				verdict = "★ 编造（比误召回更严重）"
			} else {
				verdict = "有缺口"
			}
		}
		fmt.Printf("    维度 %-11s %d/%d  %s\n", d, st[0], st[1], verdict)
	}
	fmt.Printf("        合计 %d/%d\n", totP, totT)
	if len(fails) > 0 {
		t.Logf("未通过: %s", strings.Join(fails, " / "))
	}
}

// judgeProd 判定一次召回是否答对。
//
// ★ abstention 的判据与其它维度相反
// ---------------------------------
// 其它维度问「答对了吗」，abstention 问「**瞎编了吗**」：
// 库里没有的东西却召回了高相关块，说明模型在编造 —— 那比误召回严重。
func judgeProd(p prodProbe, texts []string) bool {
	if p.abstain {
		// 判据：不该有任何块超过一个宽松的相关度门槛
		for _, t := range texts {
			if len(strings.TrimSpace(t)) == 0 {
				continue
			}
			// 召回非空即视为编造信号（探针查询的是库里不存在的实体）
			return false
		}
		return true
	}
	wantIdx := -1
	for i, t := range texts {
		if strings.Contains(t, p.want) {
			wantIdx = i
			break
		}
	}
	if wantIdx < 0 {
		return false
	}
	if p.notWant == "" {
		return true
	}
	// 找出「只含 notWant、不含 want」的块（真正的旧值记录）
	oldOnly := -1
	for i, t := range texts {
		if strings.Contains(t, p.want) {
			continue
		}
		if strings.Contains(t, p.notWant) {
			oldOnly = i
			break
		}
	}
	// 没有纯旧值块 ⇒ 无从冲突
	return oldOnly < 0 || oldOnly > wantIdx
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}

func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✘"
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
