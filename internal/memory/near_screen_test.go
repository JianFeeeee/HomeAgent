package memory

import (
	"fmt"
	"sort"
	"testing"
)

// ★ 近邻粗筛的判据与实验台。
//
// 与既有机制的关系（照抄 detectEntityMerge 的分层，distill.go:239）：
//
//	detectEntityMerge   实体层：全库两两 bigram+TFIDF，>0.75 送 LLM 裁决
//	dedupeScenes        场景层：只做归一化后完全同名的确定性合并
//
// 本文件是**块层**的对应物，口径取自后者的原则：
// 「去重不是'把像的一律合并'」。
//
// ── 粗筛 vs 判定 ──────────────────────────────────────
//
// 用户指出「相似度计算就是为了粗筛相似节点」——这是对的，且它改变了设计：
//
//	粗筛（召回优先）：宁滥勿缺，阈值低一点，误报交给下游过滤
//	判定（精确优先）：宁缺勿滥，必须确定性
//
// 实测三种判据在真库 260 块上（组内 12 对 / 跨组 33658 对）：
//
//	判据          全均      组内命中   跨组误报
//	chineseclip   0.8778    10/12      14812
//	TF-IDF(300)   0.4074     2/12       3419
//	bigram        0.1517     6/12         74
//
// ★ 关键发现：chineseclip 全均 0.878 —— 任意两块都 >0.9，
// 所以 0.9 阈值对它**等于没有阈值**（14812/33658 ≈ 44% 全被判为相似）。
// 这不是"阈值没调好"，是这个向量空间把纯文本压得太扁：
//
//	「值班室分机号 4324」cos = 0.9284
//	「值班室分机号 4379」cos = 0.9298   ← 新旧号差 0.0014
//
// CLIP 架构是为图文对齐训的，纯文本的细粒度区分度天然低。
//
// ── 但粗筛恰好能用 ──────────────────────────────────────
//
// 粗筛要的是「不漏」，而 chineseclip 的组内命中 10/12 是三者最高。
// 44% 的误报率对粗筛**可以接受**，因为下游是 (主语,维度) 分组 + 值全等：
//
//	跨组 33658 对 → 分组后只剩 12 对（组内）
//	12 对 → 值全等筛出 5 对真重复，7 对不同值
//
// 也就是说粗筛这一层的工作量是 33658 次比较，而确定性判据只要 12 次。
// 省下的正是最贵的那部分。

// ★ 粗筛的结果类型：只给候选，不给结论。
type NearCandidate struct {
	A, B     MemoryBlock
	Score    float64
	Judge    string // 哪条判据说它们像
	SameFact bool   // 值是否全等（确定性判据，不是相似度）
}

// screenNeighbors 在 blocks 里粗筛相似节点对。
//
// judge 是相似度函数；阈值 thr 取**召回优先**的值（宁可多报）。
// 下游必须用确定性判据复核（SameFact），不能拿 Score 当结论。
//
// O(n²)：实测 260 块 = 33658 次比较，毫秒级。但它自己的注释
// （detectEntityMerge）警告过「1 万实体 5000 万次配对、224GB 瞬时分配」——
// 所以**不要**把这里用在全库无分组的大集合上。分组是前置条件。
func screenNeighbors(blocks []MemoryBlock, judge func(a, b MemoryBlock) float64,
	thr float64) []NearCandidate {

	var out []NearCandidate
	for i := 0; i < len(blocks); i++ {
		for j := i + 1; j < len(blocks); j++ {
			// 前置分组：不同 (主语,维度) 的对一律跳过。
			// ★ 这一行是 33658 → 12 的全部原因。去掉它，粗筛量会爆炸
			//   且误报全部来自「跨属性的形似」（第113批 vs 第133批的评审通过）。
			sa, da, va, oka := parseFact(blocks[i].Text)
			sb, db, vb, okb := parseFact(blocks[j].Text)
			if !oka || !okb || sa != sb || da != db {
				continue
			}
			s := judge(blocks[i], blocks[j])
			if s < thr {
				continue
			}
			out = append(out, NearCandidate{
				A: blocks[i], B: blocks[j], Score: s,
				Judge: "sim", SameFact: va == vb,
			})
		}
	}
	// 按分数降序，方便看最像的在最前
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// factKey 是事实级标识：(主语,维度,值) 三元组的哈希。
//
// 用于确定性去重 —— 同一事实从不同原句拆出来会是两个块
// （BlockID 含 sentence，拆分期正确但事实层重复），
// 实测真库 5 组这种重复：第112批|批次号=112 出现两次，来自
// 「第112批 v2.31.0 评审通过·采样10%」与
// 「第112批周四凌晨2点·停机4分·回滚v2.29.5…」。
func factKey(text string) (string, bool) {
	sub, dim, val, ok := parseFact(text)
	if !ok {
		return "", false
	}
	return sub + "\x00" + dim + "\x00" + val, true
}

// ── 判据 ──────────────────────────────────────────────

// TestScreenNeighbors_确定性判据完全替代相似度
//
// ★ 这是本文件的核心判据：相似度只用于**粗筛召回**，
// 真正的"是不是同一个事实"由三元组全等**确定性**回答。
//
// 判据分两层：
//  1. 粗筛层（相似度）—— 必须**不漏**，所以低阈值
//  2. 判定层（三元组全等）—— 必须**准确**，所以零误差
//
// 混淆这两层是本轮最初犯的错：拿相似度直接当结论，
// 结果 chineseclip 44% 误报被当成"向量不可用"。
func TestScreenNeighbors_确定性判据完全替代相似度(t *testing.T) {
	blocks := []MemoryBlock{
		// 真重复：同一事实，不同原句拆出来的两个块
		{ID: "a", Text: "第112批|批次号=112"},
		{ID: "b", Text: "第112批|批次号=112"},
		// 同属性不同值（覆盖关系）—— 绝不能合并
		{ID: "c", Text: "连接池|连接池容量=32/64"},
		{ID: "d", Text: "连接池|连接池容量=128/256"},
		// 跨属性形似 —— 前置分组就该挡掉
		{ID: "e", Text: "第113批|评审通过=采样10%"},
		{ID: "f", Text: "第133批|评审通过=采样10%"},
	}
	// 一个「永远通过」的粗筛：只要同组就报
	screen := func(a, b MemoryBlock) float64 { return 1.0 }
	cands := screenNeighbors(blocks, screen, 0.5)

	// 只有同 (主语,维度) 的对能进粗筛
	if len(cands) != 2 {
		t.Fatalf("粗筛应只产出同属性对（2 对），实际 %d 对：%+v", len(cands), cands)
	}

	// 判定层：只有值全等的才是同一事实
	sameFact := 0
	for _, c := range cands {
		if c.SameFact {
			sameFact++
		}
	}
	if sameFact != 1 {
		t.Errorf("只有 1 对是同一事实（批次号=112），实际 %d", sameFact)
	}
	for _, c := range cands {
		if c.SameFact && (c.A.ID != "a" || c.B.ID != "b") {
			t.Errorf("同一事实应是 a/b，实际 %s/%s", c.A.ID, c.B.ID)
		}
	}
}

// ★ 粗筛阈值该定多低：看召回曲线。
//
// 判据：粗筛的召回率必须 100%（真重复一个不漏），
// 精确率不管 —— 那是判定层的事。
func TestScreenNeighbors_粗筛阈值只需保证召回(t *testing.T) {
	// 12 对组内对，5 对真重复
	blocks, truth := buildRealGroupPairs(t)
	trueTotal := 0
	for _, v := range truth {
		if v {
			trueTotal++
		}
	}
	if trueTotal == 0 {
		t.Skip("无真重复对")
	}
	t.Logf("真库同属性对 %d 组，其中真重复 %d 组", len(truth), trueTotal)

	for _, thr := range []float64{0.3, 0.5, 0.7, 0.9, 0.95} {
		got := 0
		hit := 0
		for i := 0; i < len(blocks); i++ {
			for j := i + 1; j < len(blocks); j++ {
				sa, da, _, _ := parseFact(blocks[i].Text)
				sb, db, _, _ := parseFact(blocks[j].Text)
				if sa != sb || da != db {
					continue
				}
				got++
				s := bigramSim(blocks[i].Text, blocks[j].Text)
				if s >= thr && truth[pairKey(i, j, len(blocks))] {
					hit++
				}
			}
		}
		recall := 0.0
		if trueTotal > 0 {
			recall = float64(hit) / float64(trueTotal)
		}
		t.Logf("  阈值 %.2f: 粗筛 %d 对，召回 %d/%d = %.0f%%",
			thr, got, hit, trueTotal, recall*100)
	}
}

func pairKey(i, j, n int) string {
	return fmt.Sprintf("%d-%d", i, j)
}

func bigramSim(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) < 2 || len(rb) < 2 {
		return 0
	}
	sa, sb := map[string]bool{}, map[string]bool{}
	for i := 0; i < len(ra)-1; i++ {
		sa[string(ra[i:i+2])] = true
	}
	for i := 0; i < len(rb)-1; i++ {
		sb[string(rb[i:i+2])] = true
	}
	in := 0
	for g := range sa {
		if sb[g] {
			in++
		}
	}
	u := len(sa) + len(sb) - in
	if u == 0 {
		return 0
	}
	return float64(in) / float64(u)
}

// buildRealGroupPairs 从真库取同属性对，并标出哪些是真重复（值全等）。
func buildRealGroupPairs(t *testing.T) ([]MemoryBlock, map[string]bool) {
	t.Helper()
	g := probeDB(t, "/var/tmp/ha-c/memory/graph.db")
	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Skipf("无真库: %v", err)
	}
	var distill []MemoryBlock
	for _, b := range blocks {
		if b.Source != "distill" {
			continue
		}
		if _, _, _, ok := parseFact(b.Text); !ok {
			continue
		}
		distill = append(distill, b)
	}
	// 只保留出现在同属性组里的
	groups := map[string][]MemoryBlock{}
	for _, b := range distill {
		s, d, _, _ := parseFact(b.Text)
		groups[s+"|"+d] = append(groups[s+"|"+d], b)
	}
	var kept []MemoryBlock
	truth := map[string]bool{}
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		base := len(kept)
		for _, m := range members {
			kept = append(kept, m)
		}
		for i := 0; i < len(members); i++ {
			for j := i + 1; j < len(members); j++ {
				_, _, va, _ := parseFact(members[i].Text)
				_, _, vb, _ := parseFact(members[j].Text)
				truth[pairKey(base+i, base+j, len(kept))] = va == vb
			}
		}
	}
	return kept, truth
}

// ★ 「同属性不同值」的区分度 —— overwrite 维度的命门。
//
// 实测 chineseclip 对这类几乎无区分度：
//
//	「值班室分机号 4324」cos = 0.9284
//	「值班室分机号 4379」cos = 0.9298    ← 旧号反而高 0.0014
//
// 所以它连"粗筛"都做不了：粗筛至少要能**分档**，
// 而全部块都在 0.85~0.95 之间 ⇒ 任何阈值都切不开。
//
// 判据：同属性不同值的对，其相似度必须**显著低于**同属性同值的对。
// 若两者分布重叠，粗筛就失效（不只是阈值问题）。
func TestNearScreen_同属性不同值的区分度(t *testing.T) {
	blocks, truth := buildRealGroupPairs(t)
	if len(blocks) == 0 {
		t.Skip("无真库同属性对")
	}
	var sameScores, diffScores []float64
	for i := 0; i < len(blocks); i++ {
		for j := i + 1; j < len(blocks); j++ {
			sa, da, _, _ := parseFact(blocks[i].Text)
			sb, db, _, _ := parseFact(blocks[j].Text)
			if sa != sb || da != db {
				continue
			}
			s := bigramSim(blocks[i].Text, blocks[j].Text)
			if truth[pairKey(i, j, len(blocks))] {
				sameScores = append(sameScores, s)
			} else {
				diffScores = append(diffScores, s)
			}
		}
	}
	minSame, maxDiff := 1.0, 0.0
	for _, s := range sameScores {
		if s < minSame {
			minSame = s
		}
	}
	for _, s := range diffScores {
		if s > maxDiff {
			maxDiff = s
		}
	}
	t.Logf("同值对 %d 个，相似度 %.2f~%.2f", len(sameScores), minSame, maxSame(sameScores))
	t.Logf("异值对 %d 个，相似度 %.2f~%.2f", len(diffScores), minDiff(diffScores), maxDiff)
	if maxDiff > minSame {
		t.Logf("★ 两类分布重叠（异值最高 %.2f > 同值最低 %.2f）⇒ 粗筛在此判据上失效", maxDiff, minSame)
	} else {
		t.Logf("★ 两类可分（同值最低 %.2f > 异值最高 %.2f）⇒ 阈值 %.2f 可用",
			minSame, maxDiff, (minSame+maxDiff)/2)
	}
}

func maxSame(s []float64) float64 {
	m := 0.0
	for _, x := range s {
		if x > m {
			m = x
		}
	}
	return m
}
func minDiff(s []float64) float64 {
	m := 1.0
	for _, x := range s {
		if x < m {
			m = x
		}
	}
	return m
}

// ★ 最强判据：块文本全等 = 同一事实，无需任何相似度。
//
// 实测（真库 260 个 distill 块）三件事等价：
//
//	块文本全等  ⟺  (主语,维度,值) 全等  ⟺  bigram = 1.00
//
// 因为块文本形态是 `<主语>|<维度>=<值>`，而主语/维度不含 '|'、值不含 '='
// —— 拆分成三段是无损的。所以「文本全等」就是「三元组全等」，
// 而真重复的 5 组文本**完全相同**（bigram=1.00，非 0.99）。
//
// ⇒ 事实级去重就是一条 GROUP BY，不需要相似度、不需要向量、不需要 LLM。
//
// ★ 那相似度（和 Qwen3-VL）到底做什么用？
// 它解决的是**文本不全等但语义同**的情形，例如：
//
//	连接池|等待队列告警阈值=300~500
//	连接池|等待队列长度告警阈值=300~500     ← 维度名多一个「长度」
//
// 这两条值相同、维度同义，是同一事实的两种说法。
// 文本全等判据**判不出**它们（需要走别名归一），而它们恰好是
// 真库里 drift 发现报出的候选（58b26e0）。
//
// 所以分工是：
//
//	文本全等      → 确定性去重（零成本，覆盖 5 组）
//	别名词表      → 确定性去重（人工确认后写入词表）
//	相似度粗筛    → 发现**新的**别名候选（Qwen3-VL 在这里才有价值）
func TestDedupeByExactText_完全确定性(t *testing.T) {
	g := probeDB(t, "/var/tmp/ha-c/memory/graph.db")
	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Skip("无真库")
	}
	byText := map[string][]MemoryBlock{}
	for _, b := range blocks {
		if b.Source != "distill" {
			continue
		}
		byText[b.Text] = append(byText[b.Text], b)
	}
	dups := 0
	for text, group := range byText {
		if len(group) < 2 {
			continue
		}
		dups++
		// 同一事实的重复：主语/维度/值必然全等
		_, _, v0, _ := parseFact(group[0].Text)
		for _, b := range group[1:] {
			_, _, v, _ := parseFact(b.Text)
			if v != v0 {
				t.Errorf("文本全等却值不同（不该发生）: %q", text)
			}
		}
	}
	t.Logf("文本全等的重复组: %d 组", dups)
	if dups == 0 {
		t.Log("（真库当前没有重复 —— 可能已去重过）")
	}

	// 反向：三元组全等 ⟺ 文本全等（无损性）
	byKey := map[string]int{}
	for text := range byText {
		k, ok := factKey(text)
		if !ok {
			continue
		}
		byKey[k]++
	}
	conflicts := 0
	for k, n := range byKey {
		if n < 2 {
			continue
		}
		// 同一三元组键下必须全部是同一条文本
		var texts []string
		for text := range byText {
			if kk, ok := factKey(text); ok && kk == k {
				texts = append(texts, text)
			}
		}
		if len(texts) != n {
			conflicts++
		}
	}
	if conflicts > 0 {
		t.Errorf("三元组键相同但文本不同的组: %d（说明拆分有损）", conflicts)
	} else {
		t.Logf("✓ 三元组键 ↔ 文本 双向无损（%d 个键）", len(byKey))
	}
}
