// 向量 + 符号融合召回。
//
// ★ 为什么必须融合而不是 fallback
// -------------------------------
// 现状（toolcall.go:236）是「块向量在前，符号路兜底」：块一旦有命中
// 就直接 return，符号路的 RecallSorted **根本没被调用**。
// 而实测（生产快照 1391 块，chineseclip 512 维）证明向量侧会失手：
//
//	查询「本机 13010 端口对应什么」
//	  向量 top8  → 「13000端口」「本地网关8081」…（都不含 13010）
//	  词法命中   → 「13010/13011 而非 12011」「http://127.0.0.1:13010」
//
// 设计注释里写的「端口号、分机号这类纯数字串向量天然弱」是对的，
// 但**没有真正生效** —— 架构让符号路没有补位机会。
//
// ★ 三层根因里的第三层
// ----------------------
//  1. 各向异性       → 已修（RebuildCentroid 补上生产调用者）
//  2. 精确串被稀释   → 向量模型固有限制，本文件解决它
//  3. 符号路无补位   → **本文件**
//
// 融合不是「两路结果拼在一起」，而是**给每个候选算一个统一分数**：
// 只有一路命中的候选仍可能被另一路拉上来，而不是排在后面。
package memory

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// FusedHit 是融合后的候选。
type FusedHit struct {
	BlockID   string
	Text      string
	Score     float64 // 统一分数
	VectorHit float64 // 向量路分数（0 = 未命中）
	SymbolHit float64 // 符号路分数（0 = 未命中）
	// ExactHit 表示「查询里的精确数字串/版本串命中了本块」。
	//
	// ★ 这是**布尔信号**不是强度信号：它的价值在于「存在与否」。
	//   混进加权和会被低权重稀释（实测：sym=1.0×0.3=0.30
	//   输给 vec=0.62 + sym=0.33 → 0.534），所以单独一路置顶。
	ExactHit bool
	Why      []string // 可读的归因，给调试与模型解释用
}

// FusionWeights 是两路的权重。
//
// ★ 为什么默认向量主导（0.7/0.3）而不是对半
// ------------------------------------------
// 符号路在**长尾中文实体名**上非常强（「本批第181批的值班手册是第几版」
// 这种问题 jieba+LIKE 就能定位），但它对**同义改写**毫无办法
// （「脚本路径改到哪了」vs「/home/newqqagent」零词法重叠）。
// 向量恰好相反。
//
// 取 0.7/0.3 而不是 0.5/0.5：符号路的误报率更高（LIKE 命中泛词
// 就会带出一堆无关块），而它的强项在 2 层能靠「符号命中即置顶」
// 的规则体现，不必靠权重放大。
type FusionWeights struct {
	Vector float64 // 默认 0.7
	Symbol float64 // 默认 0.3
}

func defaultWeights() FusionWeights { return FusionWeights{Vector: 0.7, Symbol: 0.3} }

// 融合排序的符号提取规则。
//
// ★ 不能直接 jieba：memory 里记过「jieba + LIKE 把『服务』『端口』
// 这类泛词当过滤词，造成大量噪音」。所以符号路只认**高信息量**的词：
//   - 数字串（端口、版本号、日期、批次号）
//   - 英文标识符（plugindev、grafana、kafka）
//   - 中文长词（≥2 字，且不是泛词表里的）
var (
	symbolNumericRe = regexp.MustCompile(
		`\d+(?:[./]\d+)*(?:\.\d+)?%?|[vV]\d+(?:\.\d+)+`)
	symbolLatinRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_-]{2,}`)
	symbolCJKRe   = regexp.MustCompile(`[一-鿿]{2,}`)
)

// symbolStopWords 是符号路的泛词黑名单 —— 命中它们不作为信号。
var symbolStopWords = map[string]bool{
	"服务": true, "端口": true, "地址": true, "配置": true, "问题": true,
	"记忆": true, "时间": true, "地方": true, "东西": true, "什么": true,
	"怎么": true, "哪个": true, "是否": true, "可以": true, "需要": true,
	"文件": true, "目录": true, "路径": true, "用户": true, "系统": true,
}

// QuerySymbols 提取查询里的高信息量符号。
func QuerySymbols(query string) []string {
	out := make([]string, 0, 8)
	seen := map[string]bool{}
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, m := range symbolNumericRe.FindAllString(query, -1) {
		add(m)
	}
	for _, m := range symbolLatinRe.FindAllString(query, -1) {
		add(strings.ToLower(m))
	}
	for _, m := range symbolCJKRe.FindAllString(query, -1) {
		if symbolStopWords[m] {
			continue
		}
		add(m)
	}
	return out
}

// SymbolScore 用查询符号在块文本里的出现情况打分（0~1）。
//
// ★ 精确串命中权重远高于中文词
// --------------------------------
// 正是因为「13010」这种精确串是唯一能救回向量失手的信号，
// 它的命中必须给满分，而不是和其它词按个数平均。
func SymbolScore(symbols []string, text string) (score float64, matched []string, exact bool) {
	if len(symbols) == 0 || text == "" {
		return 0, nil, false
	}
	lower := strings.ToLower(text)
	exactHit, wordHit := 0, 0
	for _, s := range symbols {
		if strings.Contains(lower, strings.ToLower(s)) {
			if isNumericSymbol(s) {
				exactHit++
			} else {
				wordHit++
			}
			matched = append(matched, s)
		}
	}
	if exactHit > 0 {
		// 有精确数字串命中 ⇒ 满分 + exact 标记。
		// ★ 1.0 在调用方被当作**布尔信号**用（直接置顶），
		//   不参与加权和 —— 否则会被 Symbol 权重稀释。
		return 1.0, matched, true
	}
	// 中文/英文词按覆盖比例给分，不叠加（避免「命中多就满分」）。
	if wordHit > 0 {
		r := float64(wordHit) / float64(len(symbols))
		if r > score {
			score = r
		}
	}
	return score, matched, false
}

func isNumericSymbol(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	// v2.31.5 这类版本串：首字母 + 数字
	if (s[0] == 'v' || s[0] == 'V') && len(s) > 1 {
		for _, r := range s[1:] {
			if unicode.IsDigit(r) {
				return true
			}
		}
	}
	return false
}

// fuseCandidates 把向量候选与符号候选合并成一个统一排序。
//
// ★ 与仲裁的顺序：融合在**前**，仲裁在**后**
// ---------------------------------------------
// 仲裁判断「谁取代了谁」依赖向量的相关性来决定「给模型看哪些」，
// 若先仲裁后融合，仲裁剔除的块可能正是符号路能救回来的那条。
//
// 注意 candidates 必须来自**未截断**的全量候选集 ——
// 向量路已经把正确块挤掉的话，融合也无从救回（与仲裁同一条理由）。
func fuseCandidates(vectorHits []BlockHit, allBlocks []MemoryBlock,
	query string, w FusionWeights) []FusedHit {

	symbols := QuerySymbols(query)
	if len(symbols) == 0 {
		// 无符号可提 ⇒ 退化成纯向量序（但仍标注 why，便于调试）
		out := make([]FusedHit, 0, len(vectorHits))
		for _, h := range vectorHits {
			out = append(out, FusedHit{
				BlockID: h.Block.ID, Text: h.Block.Text,
				Score: h.Score * w.Vector, VectorHit: h.Score,
				Why: []string{"仅向量命中（查询未提取到符号）"},
			})
		}
		return out
	}

	// 符号路在全量块里找命中（不是只在向量候选里找 ——
	// 那样「向量没召回但符号命中」的块根本没有机会上场）
	// ★ 必须用**索引**而不是 &out[i] 指针。
	//
	// 实测 bug：符号路阶段会 append（向量未召回但符号命中的块），
	// append 触发扩容后切片换了底层数组，之前存的 &out[i] 全部指向
	// 旧数组 —— 于是给已有候选写 SymbolHit 时写进了废弃副本，
	// 分数永远算不出来。判据直接抓到了这个：目标块符号分 0.333
	// 而非满分。
	//
	// 同理第一遍循环里那个 `byID[h.Block.ID] = &f`（f 是循环内临时变量）
	// 也是无效指针，一并去掉。
	idxOf := make(map[string]int, len(vectorHits))
	out := make([]FusedHit, 0, len(vectorHits)+8)
	for _, h := range vectorHits {
		idxOf[h.Block.ID] = len(out)
		out = append(out, FusedHit{
			BlockID: h.Block.ID, Text: h.Block.Text,
			VectorHit: h.Score,
		})
	}

	seenSymbol := map[string]bool{}
	for _, b := range allBlocks {
		if seenSymbol[b.ID] {
			continue
		}
		sc, matched, exact := SymbolScore(symbols, b.Text)
		if sc <= 0 {
			continue
		}
		seenSymbol[b.ID] = true
		if i, ok := idxOf[b.ID]; ok {
			out[i].SymbolHit = sc
			out[i].ExactHit = exact
			out[i].Why = append(out[i].Why,
				fmt.Sprintf("符号命中 %v", matched))
		} else {
			// ★ 向量没召回但符号命中 —— 这就是融合要救的那一类
			out = append(out, FusedHit{
				BlockID: b.ID, Text: b.Text,
				SymbolHit: sc, ExactHit: exact,
				Why: []string{
					fmt.Sprintf("仅符号命中（向量未召回）：%v", matched),
				},
			})
		}
	}

	for i := range out {
		f := &out[i]
		// ★ 精确串命中直接置顶，不参与加权。
		//
		// 实测 bug：只符号命中且符号满分（精确数字串）的块
		//   score = 1.0 × w.Symbol(0.3) = 0.300
		// 竟然输给「符号分只有 0.333」的向量候选：
		//   score = 0.620×0.7 + 0.333×0.3 = 0.534
		//
		// 根因：**满分被低权重稀释了**。而精确串命中正是符号路
		// 不可替代的能力（向量对「13010」这种串天然弱），
		// 它的价值在于「存在与否」，不在于「有多少」。
		//
		// 所以 SymbolScore 给的 1.0 是布尔信号，这里必须按布尔用。
		if f.ExactHit {
			// 向量分只做平手时的微调（不改变「已置顶」这个事实）
			f.Score = 1.0 + f.VectorHit*0.01
			continue
		}
		if f.SymbolHit > 0 && f.VectorHit == 0 {
			f.Score = f.SymbolHit * w.Symbol
			continue
		}
		f.Score = f.VectorHit*w.Vector + f.SymbolHit*w.Symbol
	}

	// 稳定排序：分数降序；同分保持向量原序（不引入额外的不确定性）
	sortFused(out)
	return out
}

func sortFused(out []FusedHit) {
	// 插入排序 —— 候选规模是百级，无需 sort.Slice 的反射开销，
	// 且完全稳定（不依赖 sort.SliceStable 的额外分配）。
	for i := 1; i < len(out); i++ {
		x := out[i]
		j := i - 1
		for j >= 0 && out[j].Score < x.Score {
			out[j+1] = out[j]
			j--
		}
		out[j+1] = x
	}
}

// RecallBlocksFused 是**融合召回的对外入口**。
//
// 与 RecallBlocksWithArbitration 的区别：
//   - RecallBlocksWithArbitration：向量召回 + 时序仲裁（排序仍是余弦序）
//   - RecallBlocksFused：向量召回 + 符号路融合 + 时序仲裁
//
// 返回的 FusedHit 带 Why（归因），便于模型理解「为什么这条被召回」。
func (g *GraphDB) RecallBlocksFused(q BlockRecallQuery, query string) (
	[]FusedHit, ArbitrationResult, error) {

	q2 := q
	q2.TopK = largeTopK
	vecHits, err := g.RecallBlocks(q2)
	if err != nil {
		return nil, ArbitrationResult{}, err
	}
	// 全量块（供符号路扫描）—— 只取有文本的（图像块无符号可匹配）
	all, err := g.MemoryBlocks()
	if err != nil {
		return nil, ArbitrationResult{}, err
	}
	textBlocks := make([]MemoryBlock, 0, len(all))
	for _, b := range all {
		if b.Text != "" {
			textBlocks = append(textBlocks, b)
		}
	}

	fused := fuseCandidates(vecHits, textBlocks, query, defaultWeights())

	// 融合 → 仲裁：仲裁在融合之后，且要喂进 FusedHit 的 id
	vecOrder := make([]BlockHit, 0, len(fused))
	byID := make(map[string]MemoryBlock, len(fused))
	for _, f := range fused {
		byID[f.BlockID] = MemoryBlock{ID: f.BlockID, Text: f.Text}
		vecOrder = append(vecOrder, BlockHit{
			Block: byID[f.BlockID], Score: f.Score,
		})
	}
	arb := arbitrate(g, vecOrder)
	superseded := map[string]bool{}
	for _, h := range arb.Superseded {
		superseded[h.Block.ID] = true
	}

	kept := make([]FusedHit, 0, len(fused))
	for _, f := range fused {
		if superseded[f.BlockID] {
			continue
		}
		if q.MinScore > 0 && f.Score < q.MinScore {
			continue
		}
		kept = append(kept, f)
	}
	if k := effectiveTopK(q.TopK); len(kept) > k {
		kept = kept[:k]
	}
	return kept, arb, nil
}
