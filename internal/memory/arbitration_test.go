package memory

import (
	"strconv"
	"testing"
	"time"
)

// 仲裁判据的用例全部取自真库 /var/tmp/ha-c 的实际块文本。
//
// ★ 两组数据方向相反，这是本判据的核心：
//   - 值覆盖组：后一条取代前一条 ⇒ 取最新才对
//   - 指标收窄组：每条都在补充，最新那条说「未再上报」⇒ 取最新会答错
// 任何「一律取最新」的规则都会在第二组上失败。

func hit(text string, when string, score float64) BlockHit {
	t, _ := time.Parse("2006-01-02 15:04:05", when)
	return BlockHit{
		Block: MemoryBlock{ID: text, Text: text, CreatedAt: t},
		Score: score,
	}
}

// 真库形态（迁移来的整句，无 '|' 无 '='）→ 不可仲裁，必须保留。
func TestArbitrate_整句块不仲裁(t *testing.T) {
	hits := []BlockHit{
		hit("值班室分机号 4379，值班人 阿李", "2026-10-01 13:28:28", 0.81),
		hit("下周起值班室分机号改为 4324，旧号 4379 停用", "2026-10-01 13:44:26", 0.73),
	}
	res := arbitrate(nil, hits)
	if len(res.Kept) != 2 {
		t.Errorf("整句块解析不出三元组，应全部保留，实际保留 %d：%+v",
			len(res.Kept), res.Kept)
	}
	if len(res.Superseded) != 0 {
		t.Errorf("不该有被取代的块，实际 %d", len(res.Superseded))
	}
}

// ★ 值覆盖的两种形态，方向相反，都要判对。
//
// 形态A 同一时刻的并列字段（同一条原句拆出来的）：
//
//	值班室分机号|值班分机号=4324    13:44
//	值班室分机号|停用旧号=4379      13:44   ← 与上条同句，不是"取代"
//
// 形态B 跨时刻的真正替代：
//
//	值班室分机号|值班分机号=4379    13:28   ← 旧值
//	值班室分机号|值班分机号=4324    13:44   ← 新值，但文本未提 4379
//
// 形态B 里新值**不该**取代旧值 —— 因为 4324 那条的文本没有提到 4379，
// 单看图无法断定「4379 作废了」。这正是「显式提及」判据与「时间更晚」
// 判据的区别：后者会误伤。
func TestArbitrate_并列字段不互斥(t *testing.T) {
	// 形态A：同句并列。两条同时间、不同维度名 → 不是时序替代。
	hitsA := []BlockHit{
		hit("值班室分机号|值班分机号=4324", "2026-10-01 13:44:26", 0.73),
		hit("值班室分机号|停用旧号=4379", "2026-10-01 13:44:26", 0.70),
	}
	resA := arbitrate(nil, hitsA)
	if len(resA.Kept) != 2 {
		t.Errorf("形态A：同句并列的两条都该保留，实际 %d：%v",
			len(resA.Kept), textsOf(resA.Kept))
	}

	// 形态B：跨时刻但文本未提及旧值 → 不判取代。
	hitsB := []BlockHit{
		hit("值班室分机号|值班分机号=4379", "2026-10-01 13:28:28", 0.81),
		hit("值班室分机号|值班分机号=4324", "2026-10-01 13:44:26", 0.73),
	}
	resB := arbitrate(nil, hitsB)
	if len(resB.Superseded) != 0 {
		t.Errorf("形态B：文本未提及旧值就不该判取代（只按时间判会误伤），实际 %d：%v",
			len(resB.Superseded), textsOf(resB.Superseded))
	}
	if len(resB.Kept) != 2 {
		t.Errorf("形态B：两条都该保留（是否作废需人工/上层判断），实际 %d",
			len(resB.Kept))
	}
}

// ★ 真正的取代：新一条同时给出新值并提及旧值。
func TestArbitrate_显式提及才算取代(t *testing.T) {
	hits := []BlockHit{
		hit("值班室分机号|值班分机号=4379", "2026-10-01 13:28:28", 0.81),
		// 这条文本里含 "4379" ⇒ 旧值被显式取代
		hit("值班室分机号|旧分机号=4379", "2026-10-01 13:44:26", 0.70),
		hit("值班室分机号|值班分机号=4324", "2026-10-01 13:44:26", 0.73),
	}
	res := arbitrate(nil, hits)

	if len(res.Superseded) != 1 {
		t.Fatalf("应恰有 1 条被取代，实际 %d：%v", len(res.Superseded), textsOf(res.Superseded))
	}
	if res.Superseded[0].Block.Text != "值班室分机号|值班分机号=4379" {
		t.Errorf("被取代的应是旧值那条，实际 %q", res.Superseded[0].Block.Text)
	}
	if res.Status != "superseded" {
		t.Errorf("状态应为 superseded，实际 %q", res.Status)
	}
}

// ★ 指标收窄：三条都要保留，最新那条不能把旧值挤掉。
func TestArbitrate_指标收窄不合并(t *testing.T) {
	hits := []BlockHit{
		hit("第85批|峰值=4%~17%（30~71批整体）", "2026-10-01 13:34:22", 0.72),
		hit("第85批|峰值=4%~17%（30~84批整体）", "2026-10-01 13:35:57", 0.71),
		hit("第85批|峰值=4%~17%（85~95批未再上报）", "2026-10-01 13:40:06", 0.70),
	}
	res := arbitrate(nil, hits)

	if len(res.Kept) != 3 {
		t.Errorf("指标收窄的三条都该保留（每条带不同批次范围），实际 %d：%v",
			len(res.Kept), textsOf(res.Kept))
	}
	if len(res.Superseded) != 0 {
		t.Errorf("指标收窄不该判取代，实际 %d：%v",
			len(res.Superseded), textsOf(res.Superseded))
	}
	// 时间升序：最早的在前
	if res.Kept[0].Block.Text != "第85批|峰值=4%~17%（30~71批整体）" {
		t.Errorf("Kept 应按时间升序（最早的在前），实际首条 %q", res.Kept[0].Block.Text)
	}
}

// ★ 「不再上报」这类状态陈述不该因时间最新而压掉真实值。
func TestReportsValue_状态陈述不算取值(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"第85批起不再上报错误率峰值", false}, // 真库实见
		{"85~95批未再上报", false},     // 真库实见
		{"值班室分机号 4324，值班 老周", true},
		{"峰值 4%~17%（30~84批整体）", true},
		{"下周起值班室分机号改为 4324，旧号 4379 停用", true},
		{"该问题已修复", false},
		{"服务暂无异常", false},
	}
	for _, c := range cases {
		if got := reportsValue(c.text); got != c.want {
			t.Errorf("reportsValue(%q) = %v，期望 %v", c.text, got, c.want)
		}
	}
}

// 无时间的块不被仲裁吃掉（迁移兜底会把块时间设成 NOW，但旧库可能有零值）。
func TestArbitrate_无时间块保留(t *testing.T) {
	hits := []BlockHit{
		{Block: MemoryBlock{ID: "a", Text: "服务A|端口=8861"}}, // 零时间
		hit("服务A|端口=9999", "2026-10-01 13:00:00", 0.9),
	}
	res := arbitrate(nil, hits)
	if len(res.Kept) != 2 {
		t.Errorf("无时间块应保留（不该被判为更早而吃掉），实际 %d：%v",
			len(res.Kept), textsOf(res.Kept))
	}
}

// 时间相同时按分数降序（迁移后同批写入的块时间完全一致）。
func TestArbitrate_同时间按分数(t *testing.T) {
	hits := []BlockHit{
		hit("服务A|端口=8861", "2026-10-01 13:00:00", 0.70),
		hit("服务A|值班人=阿李", "2026-10-01 13:00:00", 0.90),
		hit("服务A|排期=10月", "2026-10-01 13:00:00", 0.80),
	}
	res := arbitrate(nil, hits)
	if len(res.Kept) != 3 {
		t.Fatalf("三条都该保留，实际 %d", len(res.Kept))
	}
	if res.Kept[0].Block.Text != "服务A|值班人=阿李" {
		t.Errorf("同时间应按分数降序（0.90 在前），实际首条 %q (score %.2f)",
			res.Kept[0].Block.Text, res.Kept[0].Score)
	}
}

// 块文本解析的两种形态。
func TestParseFact_两种形态(t *testing.T) {
	cases := []struct {
		text          string
		ok            bool
		sub, dim, val string
	}{
		{"值班室分机号|值班分机号=4324", true, "值班室分机号", "值班分机号", "4324"},
		{"服务A|端口=8861", true, "服务A", "端口", "8861"},
		{"停机时长=4分", true, "", "停机时长", "4分"},
		{"值班室分机号 4379，值班人 阿李", false, "", "", ""},
		{"下周起值班室分机号改为 4324", false, "", "", ""},
		{"=无维度", false, "", "", ""},
	}
	for _, c := range cases {
		sub, dim, val, ok := parseFact(c.text)
		if ok != c.ok {
			t.Errorf("parseFact(%q) ok = %v，期望 %v", c.text, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if sub != c.sub || dim != c.dim || val != c.val {
			t.Errorf("parseFact(%q) = (%q,%q,%q)，期望 (%q,%q,%q)",
				c.text, sub, dim, val, c.sub, c.dim, c.val)
		}
	}
}

func textsOf(hits []BlockHit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Block.Text)
	}
	return out
}

// ★ 同句归属（blocks 的 contains 边）必须真的参与仲裁。
//
// 上一版的测试全部传 db=nil，于是「同句排除」那条分支永远走不到：
// 变异去掉它仍然全绿 —— 假绿。判据得从真库形态出发：
// 用真 GraphDB 建句子+块（sentence --contains--> block），再仲裁。
func TestArbitrate_同句归属参与仲裁(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	sentence := "值班室分机号 4324，值班 老周（下周起；旧号 4379 停用）"
	sid, err := g.EnsureSentence(sentence)
	if err != nil {
		t.Fatal(err)
	}
	// 同一条原句拆出的两个字段块
	b1 := MemoryBlock{ID: "blk_a", Modality: BlockText, Text: "值班室分机号|值班分机号=4324",
		CreatedAt: mustTime("2026-10-01 13:44:26")}
	b2 := MemoryBlock{ID: "blk_b", Modality: BlockText, Text: "值班室分机号|停用旧号=4379",
		CreatedAt: mustTime("2026-10-01 13:44:26")}
	if err := g.PutMemoryBlocks([]MemoryBlock{b1, b2}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"blk_a", "blk_b"} {
		if err := g.AddMemoryBlockEdge("sentence", itoa64(sid), "block", id, "contains"); err != nil {
			t.Fatal(err)
		}
	}
	// 另一条原句拆出的旧值块（独立事实）
	oldSent, err := g.EnsureSentence("值班室分机号 4379，值班人 阿李")
	if err != nil {
		t.Fatal(err)
	}
	b3 := MemoryBlock{ID: "blk_c", Modality: BlockText, Text: "值班室分机号|值班分机号=4379",
		CreatedAt: mustTime("2026-10-01 13:28:28")}
	if err := g.PutMemoryBlocks([]MemoryBlock{b3}); err != nil {
		t.Fatal(err)
	}
	if err := g.AddMemoryBlockEdge("sentence", itoa64(oldSent), "block", "blk_c", "contains"); err != nil {
		t.Fatal(err)
	}

	hits := []BlockHit{
		{Block: b1, Score: 0.73},
		{Block: b2, Score: 0.70},
		{Block: b3, Score: 0.81},
	}
	res := arbitrate(g, hits)

	// blk_a（4324）与 blk_b（4379停用）同句 ⇒ 并列，都保留
	// blk_c（13:28 的旧值）是否被取代取决于 blk_a 的文本是否提及 4379：
	// 「值班室分机号|值班分机号=4324」不含 4379 ⇒ 不取代
	if len(res.Kept) != 3 {
		t.Errorf("三条都该保留（blk_a/blk_b 同句并列，blk_c 未被显式取代），实际 %d：%v",
			len(res.Kept), textsOf(res.Kept))
	}

	// ★ 关键：把 blk_a 的文本换成提及旧值的版本，此时 blk_c 应被取代。
	hits2 := []BlockHit{
		{Block: MemoryBlock{ID: "blk_a", Modality: BlockText,
			Text:      "值班室分机号|值班分机号=4324（旧号 4379 停用）",
			CreatedAt: mustTime("2026-10-01 13:44:26")}, Score: 0.73},
		{Block: b2, Score: 0.70},
		{Block: b3, Score: 0.81},
	}
	res2 := arbitrate(g, hits2)
	if len(res2.Superseded) != 1 || res2.Superseded[0].Block.ID != "blk_c" {
		t.Errorf("提及旧值时 blk_c 应被取代，实际 %d：%v",
			len(res2.Superseded), textsOf(res2.Superseded))
	}
}

func mustTime(s string) time.Time {
	t, _ := time.Parse("2006-01-02 15:04:05", s)
	return t
}

func itoa64(n int64) string {
	return strconv.FormatInt(n, 10)
}

// 「旧值作废」语义识别。判据来自真库模型的实际用词。
func TestIsDeprecatedDim(t *testing.T) {
	cases := []struct {
		dim  string
		want bool
	}{
		{"停用旧号", true}, // 真库实见
		{"旧分机号", true}, // 真库实见
		{"值班分机号", false},
		{"值班人", false},
		{"端口", false},
		{"峰值", false},
		{"原版本", true},
		{"历史值", true},
		{"", false},
	}
	for _, c := range cases {
		if got := isDeprecatedDim(c.dim); got != c.want {
			t.Errorf("isDeprecatedDim(%q) = %v，期望 %v", c.dim, got, c.want)
		}
	}
}

// ★ 同句排除的直接判据：同一条原句拆出的、**同维度**的两条，
// 即使后者文本里含前者的值，也**不是**时序替代。
//
// 为什么需要这条独立用例：上一版的「同句归属参与仲裁」只测了
// 不同维度的组合，而那组数据即使删掉同句排除也照样通过（同维度排除与
// 外溢约束各自挡住了）—— 变异自证时判成了假绿。这条用同维度组合
// 把同句排除单独钉住。
//
// 真实形态：模型偶尔会把同一原句里的同一属性说两遍
// （「值班室分机号 4324，值班 老周（旧号 4379 停用）」
//
//	→ 值班分机号=4324、值班分机号=4379）
//
// 这是同一次陈述里的并列，不是「4379 作废」。
func TestArbitrate_同句同维度不互斥(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	sid, err := g.EnsureSentence("值班室分机号 4324，值班 老周（旧号 4379 停用）")
	if err != nil {
		t.Fatal(err)
	}
	b1 := MemoryBlock{ID: "s1", Modality: BlockText,
		Text:      "值班室分机号|值班分机号=4324",
		CreatedAt: mustTime("2026-10-01 13:44:26")}
	// 同句、同维度，且文本含 b1 的值 4324
	// ★ 时间必须不同：同时间时 arbitrate 的「只看更晚的」判断本来就会
	// 挡掉这对，同句排除根本执行不到 —— 那样这条用例会因为**别的原因**
	// 而通过，变异自证就判成假绿（第一版正是如此，已踩）。
	b2 := MemoryBlock{ID: "s2", Modality: BlockText,
		Text:      "值班室分机号|值班分机号=4379（本周起 4324 生效）",
		CreatedAt: mustTime("2026-10-01 13:44:27")}
	if err := g.PutMemoryBlocks([]MemoryBlock{b1, b2}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s1", "s2"} {
		if err := g.AddMemoryBlockEdge("sentence", itoa64(sid), "block", id, "contains"); err != nil {
			t.Fatal(err)
		}
	}

	res := arbitrate(g, []BlockHit{{Block: b1, Score: 0.7}, {Block: b2, Score: 0.6}})
	if len(res.Kept) != 2 {
		t.Errorf("同句同维度的两条都该保留（是并列陈述，不是新旧替代），实际 %d：%v",
			len(res.Kept), textsOf(res.Kept))
	}
}

// ★ 接入层判据：仲裁必须发生在 **topK 截断之前**。
//
// 实测的故障形态：查询「值班室分机号是多少」，旧号 4379 以 0.8127 排
// top1，新号 4324 那条**进不了 top8**。正确记录在召回阶段就被挤掉了，
// 此后任何仲裁都无从挽回。
//
// 这条测试构造「旧值分数更高、新值分数更低但时间更新」的场景：
// 若仲裁在截断后做，topK=1 时只剩旧值，断言新值在结果里就会失败。
func TestRecallBlocks_仲裁在截断前(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	// 旧值：更早、分数更高（模拟"长旧句相似度高"）
	oldSent, err := g.EnsureSentence("值班室分机号 4379，值班人 阿李")
	if err != nil {
		t.Fatal(err)
	}
	oldB := MemoryBlock{ID: "a_old", Modality: BlockText,
		Text:   "值班室分机号|值班分机号=4379",
		Vector: []float64{1, 0, 0}, Fingerprint: "fp-test",
		CreatedAt: mustTime("2026-10-01 13:28:28")}
	if err := g.PutMemoryBlocks([]MemoryBlock{oldB}); err != nil {
		t.Fatal(err)
	}
	_ = g.AddMemoryBlockEdge("sentence", itoa64(oldSent), "block", "a_old", "contains")

	// 新值：更晚、分数更低，但文本提及旧值 → 应取代它
	newSent, err := g.EnsureSentence("值班室分机号 4324，值班 老周")
	if err != nil {
		t.Fatal(err)
	}
	newB := MemoryBlock{ID: "a_new", Modality: BlockText,
		Text:   "值班室分机号|值班分机号=4324（旧号 4379 停用）",
		Vector: []float64{0.92, 0.39, 0}, Fingerprint: "fp-test",
		CreatedAt: mustTime("2026-10-01 13:44:26")}
	if err := g.PutMemoryBlocks([]MemoryBlock{newB}); err != nil {
		t.Fatal(err)
	}
	_ = g.AddMemoryBlockEdge("sentence", itoa64(newSent), "block", "a_new", "contains")

	// 查询向量更贴近旧值（0.98 vs 新值 0.95）
	q := BlockRecallQuery{
		Vector: []float64{0.98, 0.199, 0}, Fingerprint: "fp-test", TopK: 1,
	}
	hits, arb, err := g.RecallBlocksWithArbitration(q)
	if err != nil {
		t.Fatal(err)
	}
	if arb.Status != "superseded" {
		t.Errorf("仲裁状态应为 superseded（4324 取代 4379），实际 %q", arb.Status)
	}
	if len(arb.Superseded) != 1 || arb.Superseded[0].Block.ID != "a_old" {
		t.Fatalf("被取代的应是 a_old，实际 %d：%v",
			len(arb.Superseded), textsOf(arb.Superseded))
	}
	if len(hits) == 0 || hits[0].Block.ID != "a_new" {
		t.Errorf("topK=1 时应返回新值 a_new（仲裁在截断前），实际 %v", textsOf(hits))
	}

	// 对照：纯召回（不仲裁）按向量分返回 —— 旧值分数更高，所以排前。
	// 这正是仲裁要纠正的形态。
	raw, err := g.RecallBlocks(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || raw[0].Block.ID != "a_old" {
		t.Errorf("纯召回应按向量分返回（旧值在前），实际 %v", textsOf(raw))
	}
}
