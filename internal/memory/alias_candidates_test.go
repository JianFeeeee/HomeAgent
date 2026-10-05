package memory

import (
	"sort"
	"strings"
	"testing"
)

// ★ 别名候选发现的判据（不依赖具体 provider，可离线跑）。
//
// 背景：文本全等判据能抓 5 组真重复，但判不出「同一事实的两种说法」：
//
//	连接池|等待队列告警阈值=300~500
//	连接池|等待队列长度告警阈值=300~500     ← 维度名多一个「长度」
//
// 这类是漂移发现（58b26e0）报出的候选，量大且要人工逐个判断。
// 相似度粗筛的作用是**给候选排序**，让人先看最像的。
//
// ── 候选的三类，必须分开 ────────────────────────────────
//
// ① 值全等 + 维度不同  → **别名**（同一事实两种说法）← 要找的就是这个
// ② 值不同 + 维度相同  → **覆盖**（新旧值）← 归仲裁管，不是别名
// ③ 值不同 + 维度不同  → 无关
//
// ★ ②③ 混进来会让候选报告失真：实测真库里 7 对「同属性不同值」
// （第132批|版本=v2.33.1 vs =周二）如果混进别名候选，
// 人就得逐个排除，白看。
func AliasCandidateKind(a, b string) string {
	sa, da, va, oka := parseFact(a)
	sb, db, vb, okb := parseFact(b)
	if !oka || !okb {
		return "unparsable"
	}
	switch {
	case sa != sb:
		return "different-subject"
	case da == db && va == vb:
		return "exact-duplicate"
	case da == db && va != vb:
		return "value-overwrite" // 覆盖，不是别名
	case da != db && va == vb:
		return "dimension-alias" // ★ 这才是别名候选
	default:
		return "unrelated"
	}
}

// findAliasCandidates 在同主语的块里找出「值相同、维度不同」的候选。
//
// 为什么按主语分组：别名必然发生在同一主体的不同维度上
// （值班室分机号的「值班分机号」与「旧分机号」）。
// 跨主语不构成别名 —— 连接池的端口与服务的端口不是一回事。
//
// 为什么要求值全等：这是「别名」的确定性信号。
// 维度名不同 + 值相同 ⇒ 同一件事的两种说法。
// 若维度名与值都不同，那就是两件事，不该报。
func findAliasCandidates(blocks []MemoryBlock) []AliasCandidate {
	type keyed struct {
		b   MemoryBlock
		sub string
		dim string
		val string
	}
	var parsed []keyed
	for _, b := range blocks {
		s, d, v, ok := parseFact(b.Text)
		if !ok || s == "" || d == "" || v == "" {
			continue
		}
		parsed = append(parsed, keyed{b, s, d, v})
	}

	// 同主语内两两比较
	bySub := map[string][]keyed{}
	for _, p := range parsed {
		bySub[p.sub] = append(bySub[p.sub], p)
	}

	var out []AliasCandidate
	for sub, members := range bySub {
		for i := 0; i < len(members); i++ {
			for j := i + 1; j < len(members); j++ {
				if members[i].dim == members[j].dim {
					continue // 同维度 → 不是别名
				}
				if members[i].val != members[j].val {
					continue // 值不同 → 不是别名
				}
				// ★ 值全等 + 维度不同 ⇒ 别名候选
				score := dimSimilarity(members[i].dim, members[j].dim)
				out = append(out, AliasCandidate{
					Subject: sub,
					DimA:    members[i].dim, DimB: members[j].dim,
					Value: members[i].val, Score: score,
				})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// AliasCandidate 是一个别名候选。
type AliasCandidate struct {
	Subject string
	DimA    string
	DimB    string
	Value   string
	Score   float64
}

func (c AliasCandidate) String() string {
	return c.Subject + "|" + c.DimA + " == " + c.DimB + "  (值=" + c.Value + ")"
}

// dimSimilarity 是**纯词法**的维度名相似度。
//
// 为什么要它而不是向量：维度名是很短的字符串（2~8 字），
// 向量在这种短文本上几乎没有区分度（实测 chineseclip 全均 0.878）。
// 而别名候选的判别恰恰是「差几个字」—— 词法在这里比语义更合适。
//
// 这是「相似度用途分层」的一个实例：
//
//	块级语义检索   → 向量（长文本，有意义）
//	维度名判别名   → 词法（短文本，向量无效）
func dimSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	// 一方是另一方的前缀/后缀 → 强信号（「批次号」vs「批次」）
	if strings.HasPrefix(a, b) || strings.HasSuffix(a, b) ||
		strings.HasPrefix(b, a) || strings.HasSuffix(b, a) {
		return 0.95
	}
	// ★ 「等待队列告警阈值」vs「等待队列长度告警阈值」不是前缀关系
	// （中间插了「长度」），实测初版按 bigram 只给 0.60，被判成低分候选。
	//
	//   短的一方几乎完全落在长的一方里（最长公共子序列占比很高）
	//   ⇒ 强烈提示「同一维度的两种写法」，应给高分。
	//
	// 判据用 LCS 覆盖率而不是 bigram：bigram 对「插入两个字」很敏感
	// （两个二元组被破坏），而 LCS 只看保留了什么。
	if lcsCoverage(a, b) >= 0.7 {
		return 0.9
	}
	// 否则用二元组 Jaccard
	sa, sb := map[string]bool{}, map[string]bool{}
	for i := 0; i+1 < len(ra); i++ {
		sa[string(ra[i:i+2])] = true
	}
	for i := 0; i+1 < len(rb); i++ {
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

// TestFindAliasCandidates_三类必须分开
//
// ★ 判据：候选里不能混进「覆盖」与「无关」，
// 否则人得逐个排除（实测真库 7 对覆盖对混进去就废了）。
// lcsCoverage 返回「较短串被较长串覆盖」的比例（最长公共子序列长度 / 短串长度）。
func lcsCoverage(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	// 让 a 是较短的
	if len(ra) > len(rb) {
		ra, rb = rb, ra
	}
	// dp[j] = ra[:i] 与 rb[:j] 的 LCS 长度
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			if ra[i-1] == rb[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] >= cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	lcs := prev[len(rb)]
	return float64(lcs) / float64(len(ra))
}

func TestFindAliasCandidates_三类必须分开(t *testing.T) {
	blocks := []MemoryBlock{
		// ① 别名：值全等、维度不同
		{ID: "1", Text: "连接池|等待队列告警阈值=300~500"},
		{ID: "2", Text: "连接池|等待队列长度告警阈值=300~500"},
		// ② 覆盖：维度同、值不同
		{ID: "3", Text: "连接池|连接池容量=32/64"},
		{ID: "4", Text: "连接池|连接池容量=128/256"},
		// ③ 跨主语：不是别名
		{ID: "5", Text: "admin服务|端口=8861"},
		{ID: "6", Text: "billing服务|端口=8861"},
		// ④ 文本全等的重复：不是别名，是精确重复
		{ID: "7", Text: "第112批|批次号=112"},
		{ID: "8", Text: "第112批|批次号=112"},
	}
	cands := findAliasCandidates(blocks)
	if len(cands) != 1 {
		t.Fatalf("应恰有 1 个别名候选（①②③④ 各不该混入），实际 %d：%+v", len(cands), cands)
	}
	c := cands[0]
	if c.Subject != "连接池" || c.Value != "300~500" {
		t.Errorf("候选应是 连接池 / 300~500，实际 %s / %s", c.Subject, c.Value)
	}
	if !strings.Contains(c.DimA, "告警阈值") || !strings.Contains(c.DimB, "告警阈值") {
		t.Errorf("候选的两个维度都该是告警阈值类，实际 %s / %s", c.DimA, c.DimB)
	}
	// 前缀关系 → 高分
	if c.Score < 0.9 {
		t.Errorf("前缀关系的维度名相似度应很高，实际 %.2f", c.Score)
	}
}

// 三类判定的直接判据。
func TestAliasCandidateKind_分类(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"连接池|等待队列告警阈值=300~500", "连接池|等待队列长度告警阈值=300~500", "dimension-alias"},
		{"连接池|连接池容量=32/64", "连接池|连接池容量=128/256", "value-overwrite"},
		{"第112批|批次号=112", "第112批|批次号=112", "exact-duplicate"},
		{"admin服务|端口=8861", "billing服务|端口=8861", "different-subject"},
		{"服务A|甲=1", "服务A|乙=2", "unrelated"},
		{"不是三元组的整句", "也是", "unparsable"},
	}
	for _, c := range cases {
		if got := AliasCandidateKind(c.a, c.b); got != c.want {
			t.Errorf("AliasCandidateKind(%q, %q) = %q，期望 %q", c.a, c.b, got, c.want)
		}
	}
}

// 真库上的候选发现。
func TestFindAliasCandidates_真库(t *testing.T) {
	g := probeDB(t, "/var/tmp/ha-c/memory/graph.db")
	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Skip("无真库")
	}
	var distill []MemoryBlock
	for _, b := range blocks {
		if b.Source != "distill" {
			continue
		}
		distill = append(distill, b)
	}
	cands := findAliasCandidates(distill)
	t.Logf("distill 块 %d 个 → 别名候选 %d 个", len(distill), len(cands))
	for i, c := range cands {
		if i >= 10 {
			t.Logf("  ... 共 %d 个", len(cands))
			break
		}
		t.Logf("  [%.2f] %s", c.Score, c)
	}
}

// ★ 词法判据的失效边界：它靠什么、会在哪失效。
//
// dimSimilarity 的三种信号：
//  1. 前缀/后缀包含 → 0.95（「批次号」vs「批次」）
//  2. LCS 覆盖 ≥0.7 → 0.90（「等待队列告警阈值」vs「等待队列长度告警阈值」）
//  3. bigram Jaccard → 0~1（兜底）
//
// 失效边界必须写清楚，否则会被当成"通用相似度"用：
//
// ① **同义词换词**：灰度比例 vs 观察比例
//
//	bigram≈0，LCS=0 ⇒ 词法给 0 分，但它们确实是别名。
//	这类要靠漂移发现的**值形态**判据（58b26e0），不是词法。
//
// ② **完全不同的词**：端口 vs 端点
//
//	词法给低分 —— 但这正是对的（它们未必是同义，需要人判）。
//
// ⇒ 词法判据的定位：**高精度的"显然像"**，漏掉同义词换词。
//
//	漏的方向是安全的（不会误并），而向量若给高分则可能误并。
func TestDimSimilarity_边界(t *testing.T) {
	cases := []struct {
		a, b    string
		min     float64
		comment string
	}{
		{"批次号", "批次", 0.9, "前缀包含 ⇒ 高分"},
		{"等待队列告警阈值", "等待队列长度告警阈值", 0.85, "插入两字，LCS 覆盖高"},
		{"值班分机号", "旧分机号", 0.5, "共享「分机号」但语义不同（一个是旧值）"},
		{"灰度比例", "观察比例", 0.0, "同义换词：词法给 0 —— 已知漏检，靠漂移发现的值形态判据"},
	}
	for _, c := range cases {
		got := dimSimilarity(c.a, c.b)
		if got < c.min {
			t.Errorf("dimSimilarity(%q,%q) = %.2f，期望 ≥%.2f（%s）",
				c.a, c.b, got, c.min, c.comment)
		}
		t.Logf("  %.2f  %-16q vs %-20q  %s", got, c.a, c.b, c.comment)
	}
}

// ★ 漏检方向的代价：词法漏掉的别名，人工还能看见（因为值全等）。
func TestFindAliasCandidates_漏检仍可发现(t *testing.T) {
	// 「灰度比例」与「观察比例」—— 词法给 0，但只要**值全等**
	// 且同主语，就仍会被 findAliasCandidates 收进来（分数低）。
	blocks := []MemoryBlock{
		{ID: "1", Text: "第113批|灰度比例=10%"},
		{ID: "2", Text: "第113批|观察比例=10%"},
	}
	cands := findAliasCandidates(blocks)
	if len(cands) != 1 {
		t.Fatalf("值全等就该被收进候选（哪怕分数低），实际 %d 个", len(cands))
	}
	t.Logf("✓ 收进来了，分数 %.2f（低分提示「不像，可能同义换词」）", cands[0].Score)
	if cands[0].Score > 0.5 {
		t.Errorf("同义换词应给低分（提示人细看），实际 %.2f", cands[0].Score)
	}
}
