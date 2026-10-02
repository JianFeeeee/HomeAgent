package memory

import (
	"math"
	"testing"
)

// ★ 中心化的判据核心：把「所有向量都朝同一方向偏」这个共同分量去掉，
// 同值对的相似度要**保持**，异值对要**拉远**。
//
// 依据实测（真库 260 个 distill 块，chineseclip 512 维）：
//
//	均值向量长度 0.9374 → 去均值后平均两两余弦从 0.8727 降到 -0.0012
//	分离度 0.0095 → 0.0689（提升 7 倍）
func TestCentroid_分离度提升(t *testing.T) {
	// 构造：向量 = 强共同方向 + 弱个体差异
	const dim = 8
	mkVec := func(f1, f2 float64) []float64 {
		// 共同分量恒为 1，个体差异放在末两维
		v := make([]float64, dim)
		for i := 0; i < dim-2; i++ {
			v[i] = 1.0
		}
		v[dim-2] = f1
		v[dim-1] = f2
		return v
	}
	// 同值对：个体差异相同
	sameA := mkVec(0, 0)
	sameB := mkVec(0, 0)
	// 异值对：个体差异不同
	diffA := mkVec(0, 0)
	diffB := mkVec(0.05, 0.05)

	norm := func(v []float64) []float64 {
		l2Normalize(v)
		return v
	}
	c, st := BuildCentroid([][]float64{
		norm(mkVec(0, 0)), norm(mkVec(0.05, 0.05)), norm(mkVec(0.1, 0.1)),
	})
	if !st.Anomalous {
		t.Fatalf("构造的数据应被判为各向常（均值范数 %.3f），实际 %+v", st.MeanLength, st)
	}
	t.Logf("均值范数 %.4f  平均两两余弦 %.4f  anomalous=%v",
		st.MeanLength, st.AvgPairCosine, st.Anomalous)

	rawSame := cosineOf(sameA, sameB)
	rawDiff := cosineOf(diffA, diffB)
	cSame := cosineOf(c.CenterVector(sameA), c.CenterVector(sameB))
	cDiff := cosineOf(c.CenterVector(diffA), c.CenterVector(diffB))

	t.Logf("  同值对: 原始 %.4f → 去均值 %.4f", rawSame, cSame)
	t.Logf("  异值对: 原始 %.4f → 去均值 %.4f", rawDiff, cDiff)
	rawSep := rawSame - rawDiff
	cSep := cSame - cDiff
	t.Logf("  分离度: 原始 %.4f → 去均值 %.4f", rawSep, cSep)

	if cSep <= rawSep {
		t.Errorf("去均值后分离度应提升，实际 %.4f → %.4f", rawSep, cSep)
	}
	// 同值对必须保持高相似（中心化不该破坏真重复）
	if cSame < 0.99 {
		t.Errorf("同值对中心化后应仍几乎完全相同，实际 %.4f", cSame)
	}
}

// ★ 关键约束：中心化后必须重新归一化。
//
// 只减不归一的话，所有余弦会被「向量变短了」这个纯粹的尺度效应污染。
func TestCentroid_必须重新归一化(t *testing.T) {
	const dim = 4
	v := []float64{1, 1, 1, 1} // 范数 2
	c := NewCentroid(dim)
	c.Add([]float64{1, 0, 0, 0})
	c.Add([]float64{0, 1, 0, 0})

	onlySub := c.Subtract(append([]float64{}, v...))
	onlySubNorm := math.Sqrt(dotOf(onlySub, onlySub))
	if onlySubNorm >= 2.0 {
		t.Logf("减均值后范数 %.4f（原 2.0）—— 尺度确实变了", onlySubNorm)
	}
	// CenterVector 会重新归一化
	centered := c.CenterVector(v)
	centeredNorm := math.Sqrt(dotOf(centered, centered))
	if math.Abs(centeredNorm-1.0) > 1e-9 {
		t.Errorf("CenterVector 后范数应为 1，实际 %.6f", centeredNorm)
	}
	// 同向的两个向量中心化后仍应余弦为 1（若已归一化）
	w := []float64{1, 0.9, 0.8, 0.7}
	a := c.CenterVector(v)
	b := c.CenterVector(w)
	if got := cosineOf(a, b); got < 0.99 {
		t.Errorf("同向向量中心化+归一化后余弦应≈1，实际 %.4f", got)
	}
}

// 各向同性的库不该做中心化（否则放大噪声）。
func TestCentroid_各向同性时不报异常(t *testing.T) {
	// 四个正交方向 → 均值接近 0
	vs := [][]float64{
		{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}, {0, 0, 0, 1},
	}
	_, st := BuildCentroid(vs)
	if st.Anomalous {
		t.Errorf("正交基应判为各向同性（均值范数 %.4f），实际 anomalous", st.MeanLength)
	}
	if st.MeanLength > meanThreshold {
		t.Errorf("正交基的均值范数应接近 0，实际 %.4f", st.MeanLength)
	}
}

// 维度不匹配必须被拒绝（混维度会把均值算坏）。
func TestCentroid_维度不匹配(t *testing.T) {
	c := NewCentroid(3)
	if c.Add([]float64{1, 2}) { // 维度 2 ≠ 3
		t.Error("维度不匹配的向量不该被接受")
	}
	if c.Add(nil) {
		t.Error("nil 向量不该被接受")
	}
	if c.Count != 0 {
		t.Errorf("Count 应仍为 0，实际 %d", c.Count)
	}
	// Subtract 遇到维度不匹配应原样返回
	out := c.Subtract([]float64{1, 2, 3})
	if len(out) != 3 || out[0] != 1 {
		t.Errorf("空中心时 Subtract 应原样返回，实际 %v", out)
	}
}

// 空中心的行为。
func TestCentroid_空中心(t *testing.T) {
	c := NewCentroid(4)
	if c.Mean() != nil {
		t.Error("空中心的 Mean 应为 nil")
	}
	st := c.Stats()
	if st.MeanLength != 0 || st.Anomalous {
		t.Errorf("空中心的 Stats 应为零值，实际 %+v", st)
	}
	// Subtract 应原样返回（不 panic）
	v := []float64{1, 2, 3, 4}
	out := c.Subtract(v)
	if len(out) != 4 || out[3] != 4 {
		t.Errorf("空中心 Subtract 应原样返回，实际 %v", out)
	}
}

// 缓存的版本失效：块数或维度变化后要返回 nil（否则用旧均值算新查询）。
func TestCentroidCache_版本失效(t *testing.T) {
	cc := NewCentroidCache()
	c1, _ := BuildCentroid([][]float64{{1, 0}, {0, 1}})
	st1 := c1.Stats()
	cc.Put(c1, st1)

	if got, _ := cc.Get(2, 2); got == nil {
		t.Error("相同 (dim,count) 应命中缓存")
	}
	if got, _ := cc.Get(2, 3); got != nil {
		t.Error("count 变化应失效（块数变了，均值含义也变了）")
	}
	if got, _ := cc.Get(3, 2); got != nil {
		t.Error("dim 变化应失效")
	}
	// 放 nil 不该清空已有缓存
	cc.Put(nil, CentroidStats{})
	if got, _ := cc.Get(2, 2); got == nil {
		t.Error("Put(nil) 不该清空已有缓存")
	}
}

func dotOf(a, b []float64) float64 {
	var s float64
	for i := range a {
		if i < len(b) {
			s += a[i] * b[i]
		}
	}
	return s
}

func cosineOf(a, b []float64) float64 {
	na, nb := dotOf(a, a), dotOf(b, b)
	if na == 0 || nb == 0 {
		return 0
	}
	return dotOf(a, b) / (math.Sqrt(na) * math.Sqrt(nb))
}

// ★ 真库上的中心化判据（本轮的核心实验）。
//
// 合成数据只能验证机制正确，**收益量级必须在真库上量**。
// 实测基线（这轮跑出来的）：
//
//	均值范数 0.9374 / 平均两两余弦 0.8727
//	分离度 0.0095 → 去均值后 0.0689（7.3 倍）
//
// 判据：真库上中心化必须显著提升分离度，且不破坏同值对。
func TestCentroid_真库分离度(t *testing.T) {
	g := probeDB(t, "/var/tmp/ha-c/memory/graph.db")
	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Skip("无真库")
	}
	var vecs [][]float64
	type idx struct {
		sub, dim, val string
	}
	var ids []idx
	for _, b := range blocks {
		if b.Source != "distill" || len(b.Vector) == 0 {
			continue
		}
		s, d, v, ok := parseFact(b.Text)
		if !ok || s == "" {
			continue
		}
		vecs = append(vecs, b.Vector)
		ids = append(ids, idx{s, d, v})
	}
	if len(vecs) < 10 {
		t.Skip("块太少")
	}
	t.Logf("真库 %d 个块，维度 %d", len(vecs), len(vecs[0]))

	c, st := BuildCentroid(vecs)
	t.Logf("均值范数 %.4f  平均两两余弦 %.4f  anomalous=%v",
		st.MeanLength, st.AvgPairCosine, st.Anomalous)
	if !st.Anomalous {
		t.Fatalf("真库应判为各向异性严重，实际均值范数 %.4f", st.MeanLength)
	}

	// 同属性对：分离度（原始 vs 中心化后）
	var rawSame, rawDiff, cenSame, cenDiff []float64
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[i].sub != ids[j].sub || ids[i].dim != ids[j].dim {
				continue
			}
			r := cosineOf(vecs[i], vecs[j])
			cc := cosineOf(c.CenterVector(vecs[i]), c.CenterVector(vecs[j]))
			if ids[i].val == ids[j].val {
				rawSame = append(rawSame, r)
				cenSame = append(cenSame, cc)
			} else {
				rawDiff = append(rawDiff, r)
				cenDiff = append(cenDiff, cc)
			}
		}
	}
	if len(rawSame) == 0 || len(rawDiff) == 0 {
		t.Skip("缺少同值对或异值对")
	}
	rawSep := minOf(rawSame) - maxOf(rawDiff)
	cenSep := minOf(cenSame) - maxOf(cenDiff)
	t.Logf("同值对 %d 个（最低 %.4f → %.4f）", len(rawSame), minOf(rawSame), minOf(cenSame))
	t.Logf("异值对 %d 个（最高 %.4f → %.4f）", len(rawDiff), maxOf(rawDiff), maxOf(cenDiff))
	t.Logf("★ 分离度 %.4f → %.4f（%.1f 倍）",
		rawSep, cenSep, cenSep/rawSep)

	if cenSep <= rawSep {
		t.Errorf("真库上中心化应提升分离度，实际 %.4f → %.4f", rawSep, cenSep)
	}
	// 同值对不能被破坏
	if minOf(cenSame) < 0.99 {
		t.Errorf("同值对中心化后应仍几乎相同，实际 %.4f", minOf(cenSame))
	}
}

func minOf(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs {
		if x < m {
			m = x
		}
	}
	return m
}
func maxOf(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}
