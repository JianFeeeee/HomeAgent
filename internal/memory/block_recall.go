package memory

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
)

// 块节点的向量召回。
//
// 为什么需要它：entities 是旧形态且正在退场，而 memory_blocks 才是带
// vector+fingerprint 的节点载体（46f833c 引入）。此前唯一有向量的只有
// memory_blocks，但**没有任何召回路径读它** —— BlocksForNode 只能按
// 端点反查，必须先知道 nodeID。于是「问一个具体问题」时只能退到
// entities 的 jieba+LIKE，实测跨维度定位 0/5。
//
// 本文件补的就是这条缺失的路径：query 向量 → 块向量 → 按余弦排序。
//
// 与 vector.Store 的分工：Store 是内存倒排索引（L0/L1 用），块向量存在
// SQLite（graph.db），生命周期与图一致、可跨进程重启、可被备份脚本覆盖。
//
// 全表扫描的代价（**已量化，不是估算**）：生产库迁移后约 1277 块 × 2048 维
// = 21 MB 读入 + 2.6M 次乘加，每次 memory_recall 一次。当前可接受；
// 块数上到万级时需要换方案（候选：把向量搬进 vector.Store 的内存倒排，
// 或按 modality/scene 预筛减少扫描量）。在此之前不做优化 —— 没有真实
// 规模数据时优化是猜，本会话已经因此浪费过几轮。

// BlockHit 是一条块召回的候选及相似度。
type BlockHit struct {
	Block MemoryBlock `json:"block"`
	Score float64     `json:"score"`
}

// BlockRecallQuery 描述一次块召回请求。
type BlockRecallQuery struct {
	Vector []float64
	// Fingerprint 是发起方所用向量空间的指纹。与库中块不一致的块会被
	// **跳过**（不是报错）：库里可能同时存在两个空间的块（迁移期），
	// 跳过才是正确的——拿新空间的查询向量去比旧空间的块向量，余弦值
	// 没有任何意义，而且不会报错，只是"结果看起来还行但全是错的"。
	Fingerprint string
	TopK        int
	// MinScore 低于此分数的候选被丢弃（0 表示不设下限）。
	MinScore float64
}

// RecallBlocks 按向量相似度召回块节点。
//
// 返回的 hit 已按 Score 降序。库里没有任何块的向量时返回空切片而非错误
// ——「还没回填」是正常状态，不是故障。
func (g *GraphDB) RecallBlocks(q BlockRecallQuery) ([]BlockHit, error) {
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("recall blocks: empty query vector")
	}
	topK := q.TopK
	if topK <= 0 {
		topK = 10
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	rows, err := g.db.Query(`SELECT id, modality, text_content, payload_digest,
		mime, size, width, height, vector, fingerprint, source, tool, scene,
		created_at, updated_at
		FROM memory_blocks
		WHERE vector IS NOT NULL AND vector != '' AND vector != 'null'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	dim := len(q.Vector)
	var hits []BlockHit
	var skippedDim, skippedFP, scanned int
	for rows.Next() {
		var b MemoryBlock
		var vectorJSON string
		if err := rows.Scan(&b.ID, &b.Modality, &b.Text, &b.PayloadDigest,
			&b.MIME, &b.Size, &b.Width, &b.Height, &vectorJSON,
			&b.Fingerprint, &b.Source, &b.Tool, &b.Scene,
			&b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		scanned++
		vec, err := decodeVector(vectorJSON)
		if err != nil {
			// 单条坏数据不该让整次召回失败 —— 但要记日志，否则会静默缺失。
			log.Printf("[graph] recall blocks: skip %s: %v", b.ID, err)
			continue
		}
		// 维度不一致：跳过并计数。**不截断、不填充**——那会造出无意义的分数。
		if len(vec) != dim {
			skippedDim++
			continue
		}
		// 指纹不一致：跳过并计数。查询方声明的空间与块所属空间不同。
		if q.Fingerprint != "" && b.Fingerprint != "" && b.Fingerprint != q.Fingerprint {
			skippedFP++
			continue
		}
		score := cosine(vec, q.Vector)
		if q.MinScore > 0 && score < q.MinScore {
			continue
		}
		b.Vector = vec
		hits = append(hits, BlockHit{Block: b, Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if skippedDim > 0 {
		log.Printf("[graph] recall blocks: 跳过 %d 个维度不匹配的块（查询 %d 维，共扫 %d）",
			skippedDim, dim, scanned)
	}
	if skippedFP > 0 {
		log.Printf("[graph] recall blocks: 跳过 %d 个指纹不匹配的块（查询空间 %s，共扫 %d）",
			skippedFP, shortFP(q.Fingerprint), scanned)
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// BlockVectorStats 报告块向量的回填状态，用于运维判断「要不要跑回填」。
type BlockVectorStats struct {
	Total        int      `json:"total"`
	WithVector   int      `json:"with_vector"`
	MissingFP    int      `json:"missing_fingerprint"`
	Fingerprints []string `json:"fingerprints"`
	Dimensions   []int    `json:"dimensions"`
}

func (g *GraphDB) BlockVectorStats() (BlockVectorStats, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var st BlockVectorStats
	fpSet := map[string]bool{}
	dimSet := map[int]bool{}
	rows, err := g.db.Query(`SELECT vector, fingerprint FROM memory_blocks`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var vectorJSON, fp string
		if err := rows.Scan(&vectorJSON, &fp); err != nil {
			return st, err
		}
		st.Total++
		if vectorJSON == "" || vectorJSON == "null" {
			continue
		}
		vec, err := decodeVector(vectorJSON)
		if err != nil {
			continue
		}
		st.WithVector++
		if fp == "" {
			st.MissingFP++
		} else {
			fpSet[fp] = true
		}
		dimSet[len(vec)] = true
	}
	for fp := range fpSet {
		st.Fingerprints = append(st.Fingerprints, fp)
	}
	for d := range dimSet {
		st.Dimensions = append(st.Dimensions, d)
	}
	sort.Strings(st.Fingerprints)
	sort.Ints(st.Dimensions)
	return st, rows.Err()
}

// ── 向量工具 ──

func decodeVector(s string) ([]float64, error) {
	var vec []float64
	if err := json.Unmarshal([]byte(s), &vec); err != nil {
		return nil, fmt.Errorf("decode vector: %w", err)
	}
	return vec, nil
}

func cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	s := dot / (math.Sqrt(na) * math.Sqrt(nb))
	// NaN 会让排序结果不确定（NaN 的比较全 false），显式归零。
	if math.IsNaN(s) {
		return 0
	}
	return s
}

func shortFP(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}
