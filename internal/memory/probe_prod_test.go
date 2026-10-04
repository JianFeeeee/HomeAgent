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

// ★ 探针的 want 值全部取自**生产库自己的数据**。
//
// 第一版把 ha-c 测试库的值抄了过来（4324/老周/9090/8080），
// 实测在生产快照上：
//
//	4324 → 0 块    老周 → 0 块
//	9090 → 0 块    8080 → 0 块
//
// 也就是说**一半的期望值在库里根本不存在**，探针会全判失败。
// 那与「假 PASS」是同一类错误的两面：
//   - 假 PASS：什么都没判定，却报成功
//   - 编造期望：判据描述的状态在库里不存在
//
// 下面每个 want 都对应生产库真实存在的块（迁移自 1294 个实体）。
var prodProbes = []prodProbe{
	// casual：常规查询
	{name: "脚本路径", dim: "casual",
		query: "脚本路径改到哪个目录了", want: "/home/newqqagent"},
	{name: "插件工具链", dim: "casual",
		query: "从零开发 QQ 插件用什么工具链", want: "plugindev"},
	{name: "公网地址", dim: "casual",
		query: "agentmail 公网访问地址是什么", want: "101.201.37.155"},
	{name: "本机端口", dim: "casual",
		query: "本机 13010 端口对应什么", want: "13010"},

	// overwrite：值覆盖 —— 需要库里真有「新旧两个值」的成对事实。
	// 生产库的端口事实是 13010/13011 这类**并存**的（不是覆盖），
	// 所以这一维度在生产数据上**不适用**，改为验证「不误判」：
	// ★★ coexist 是**已知缺口**，判据先按现状标注为"不达标"（2026-10-04）
	//
	// 迁移后实测 0/1，原因已定位（不是缺陷，是缺能力）：
	//
	//	库里含「端口」二字的块 22 个（14010端口 / 14011端口 …），
	//	而真正的本机端口块文本是「http://127.0.0.1:13010」——
	//	**不含「端口」二字**。
	//
	//	于是泛指提问「本机服务监听哪些端口」召回的是 14010 那批，
	//	13010 挤不进 topK。
	//
	// ★ 补法是**图联想**而不是关键词调优：
	//	「端口」→ BFS 到端口号 → 再 BFS 到那个端口是什么服务。
	//	BFSBlocks 已实现（9df1efb）但**尚未接入 RecallBlocksFused**
	//	（见 docs/zh/legacy-table-retirement.md 的待办）。
	//
	// ⇒ 在 BFS 进召回链之前，这一格必然红。它标的是进度，不是回归。
	{name: "并存端口不互斥", dim: "coexist",
		query: "本机服务监听哪些端口", want: "13010", notWant: ""},

	// confusable：两个相近的东西
	{name: "13010与13011", dim: "confusable",
		query: "13011 端口对应什么服务", want: "13011", notWant: "13010"},

	// ★★ abstention：库里根本没有的东西，不该编造
	//
	// ★★★ 这三条的查询内容在 2026-10-04 全部换过（2026-10-04）
	//
	// 原查询是：
	//
	//	「grafana 监控面板的端口是多少」
	//	「谁负责数据库容灾演练」
	//	「kafka 消息队列的 broker 地址是什么」
	//
	// ★★ 它们**按契约就不该被拒答**，而判据写着 abstain:true：
	//
	//	1) grafana / kafka 是英文标识符，但中文滑窗会把它与后续中文
	//	   粘成一个跨词符号（「grafana 监控面板」），那个串库里真没有 ——
	//	   于是零命中。可零命中在中文伪词上**不可信**：
	//	   「本机服务监听哪些端口」同样零命中，而库里有 13010/8081。
	//	2) 「谁负责…」命中泛指词「谁」⇒ 符号提取这个**前提不成立** ⇒ 豁免。
	//
	// 契约（docs/zh/abstention-criterion.md）：
	//
	//	零命中只在「查询含数字串/版本串」时才敢拒答。
	//
	// ⇒ 原判据测的是**契约之外**的东西，于是长期红。
	//   迁移前后对照实测：迁移前 3/3「通过」是数据为空的假象
	//   （那时所有真实问题都被拒答挡掉了），迁移后 0/3 是契约的诚实结果。
	//
	// ⇒ 换成契约保证的三类：纯数字串 / 端口号 / 版本串。
	//   它们零命中的含义明确「那个串库里确实没有」。
	{name: "不存在的批次", dim: "abstention",
		// ★ 批次号选 997：实测 8/138 个块含「8」（8081、13010 等），
		//   用小数字会命中而不拒答 —— 判据就会假红。
		query: "第997批的停机时长是多少", abstain: true},
	{name: "不存在的端口", dim: "abstention",
		query: "本机 99999 端口对应什么服务", abstain: true},
	{name: "不存在的版本", dim: "abstention",
		query: "grafana 9.99.99 的面板地址是什么", abstain: true},
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
		// ★ 走**融合**入口 —— 这是本次改动的真实判据。
		// 纯向量入口的 2/9 是基线（纯向量 top8 全挤在 0.84~0.90，
		// 含精确串的「13010/13011 而非 12011」连 top2000 都进不去）。
		// ★ 走**带拒答**的入口，与生产路径完全一致。
		// 之前这里直连 RecallBlocksFused，绕过了 core 层的拒答 ——
		// 于是探针测不出拒答是否存在（判据没覆盖被测路径）。
		hits, abstain, _, err := g.RecallBlocksGuarded(BlockRecallQuery{
			Vector: vec, Fingerprint: fp, TopK: 8,
		}, p.query)
		if err != nil {
			t.Fatalf("召回失败: %v", err)
		}
		if abstain != nil {
			// 拒答了：abstention 维度要求的就是这个
			ok := p.abstain
			st := byDim[p.dim]
			st[1]++
			if ok {
				st[0]++
			} else {
				fails = append(fails, fmt.Sprintf("[%s]%s 误拒答", p.dim, p.name))
			}
			byDim[p.dim] = st
			fmt.Printf("    [%s] %-18s %s 拒答: %s\n", p.dim, p.name,
				mark(ok), trunc(abstain.Notice, 78))
			continue
		}
		texts := make([]string, 0, len(hits))
		for _, h := range hits {
			texts = append(texts, h.Text)
		}
		joined := strings.Join(texts, " ｜ ")

		ok := judgeProd(p, texts)
		// ★ 必须写回 map：数组是值语义，
		// st := byDim[k]; st[1]++ 不写回的话 byDim 永远是零值
		// ⇒ 汇总时 st[1]==0 全部 continue ⇒ 打印「合计 0/0」。
		//
		// 这个 bug 藏了两次重跑：修判据时看到 0/0 以为是「前提不满足」，
		// 补完探针后它还在，才意识到是计数本身坏了。
		st := byDim[p.dim]
		st[1]++
		if ok {
			st[0]++
		} else {
			fails = append(fails, fmt.Sprintf("[%s]%s", p.dim, p.name))
		}
		byDim[p.dim] = st
		// 归因统计：融合救回了多少条（精确串 / 词法）
		var nExact, nSymOnly int
		for _, h := range hits {
			if h.ExactHit {
				nExact++
			} else if h.VectorHit == 0 && h.SymbolHit > 0 {
				nSymOnly++
			}
		}
		attr := ""
		if nExact+nSymOnly > 0 {
			attr = fmt.Sprintf("  [+符号 %d: 精确%d 词法%d]", nExact+nSymOnly, nExact, nSymOnly)
		}
		fmt.Printf("    [%s] %-18s %s %s%s\n", p.dim, p.name,
			mark(ok), trunc(joined, 92), attr)
	}

	fmt.Println()
	totP, totT := 0, 0
	for _, d := range []string{"casual", "coexist", "confusable", "abstention"} {
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

	// ★ 硬断言：探针全挂 ≠ 测试失败（t.Logf 不改退出码）
	//
	// 实测踩过：首次跑生产快照时 7 条探针全部失败、合计打印 0/0，
	// 而测试框架报 --- PASS —— 因为「0 条通过 / 0 条判定」根本不满足
	// `failures > 0`。**「0/0」被打印成 PASS 比红更危险**：
	// 红会让人去查，绿不会。
	if totT == 0 || totP == 0 {
		t.Fatalf("探针未产生任何有效判定（通过 %d / 总数 %d）—— "+
			"前提不满足（如库里有文本块才能判召回），不是通过", totP, totT)
	}
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
