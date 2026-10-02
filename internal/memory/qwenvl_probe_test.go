package memory

import (
	"testing"
)

// ★ Qwen3-VL vs chineseclip：Qwen3-VL 该用在哪、不该用在哪。
//
// ═══════════════════════════════════════════════════════════
// 本文件是**决策实验**，不是回归测试。跳过条件是模型目录不存在。
// ═══════════════════════════════════════════════════════════
//
// ── 待回答的问题 ──────────────────────────────────────────
//
// 已知（实测 chineseclip）：
//
//	全部配对平均相似度 0.8778 —— 任意两块都 >0.9
//	「值班室分机号 4324」cos=0.9284  vs  「…4379」cos=0.9298
//	                                     ← 新旧号差 0.0014
//
// 所以 chineseclip 有两个已证实的短板：
//  (1) 块级检索时，同属性不同值几乎无区分度 → overwrite 维度答错
//  (2) 当不了近邻粗筛（0.9 阈值等于没有阈值，44% 配对误报）
//
//  Qwen3-VL-Embedding-2B 是 2048 维、last_token 池化，为检索训练。
// 它在长文本语义上应该显著更强。三个问题：
//
//	① 块级：同属性不同值的区分度是否改善？（overwrite 的命门）
//	② 粗筛：阈值是否变得有效？（不再 44% 误报）
//	③ 短文本：维度名上能否打过词法？（别名发现的用途分层）
//
// ── 期望与判据 ────────────────────────────────────────────
//
// ① 若 Qwen3-VL 能把 4324/4379 拉开到明显可分 → 切换实例的向量 provider
// ② 若 0.9 阈值误报大幅下降 → 可用于粗筛
// ③ 若短文本上打不过词法（见 alias_candidates_test.go 的边界）→
//    明确"短文本用词法"，不用向量
//
// ★ 三个问题分别对应不同的用途，结论可能不一致 ——
//   **不能用一个指标否定整个 provider**。

const qwenModelDir = "/root/qwen3vl-emb-onnx"

// ① 同属性不同值的区分度（overwrite 维度的命门）。
//
// 判据：separation = 同值对的最低分 − 异值对的最高分。
// separation > 0 ⇒ 两类可分，粗筛/阈值有效。
func TestQwenVL_同属性不同值区分度(t *testing.T) {
	q := openQwenVL(t)
	defer q.Close()

	cases := []struct {
		a, b    string
		sameVal bool
		why     string
	}{
		// 异值对：必须能分开，否则 overwrite 答不对
		{"值班室分机号 4324，值班 老周", "值班室分机号 4379，值班人 阿李", false,
			"★ 新旧分机号 —— overwrite 维度的核心用例"},
		{"连接池容量 32/64", "连接池容量 128/256", false, "扩容前后"},
		{"第113批 告警规则 9条", "第113批 告警规则 15条", false, "同批次不同规则数"},
		// 同值对：应该更相似
		{"值班室分机号 4324", "分机号 4324", true, "同一值两种写法"},
		{"告警规则 9条", "规则数 9条", true, "同一值不同措辞"},
		// 完全无关：必须低分
		{"值班室分机号 4324", "峰值 4%~17%", false, "毫不相干"},
	}

	var sameMin, diffMax float64 = 1.0, 0.0
	for _, c := range cases {
		va := q.vec(c.a)
		vb := q.vec(c.b)
		s := cosineVec(va, vb)
		flag := ""
		if c.sameVal {
			if s < sameMin {
				sameMin = s
			}
			flag = " [同值对]"
		} else if s > diffMax {
			diffMax = s
		}
		t.Logf("  cos=%.4f%s  %s", s, flag, c.why)
		t.Logf("        A=%s", c.a)
		t.Logf("        B=%s", c.b)
	}
	t.Logf("\n  同值对最低 %.4f   异值对最高 %.4f", sameMin, diffMax)
	if diffMax < sameMin {
		t.Logf("  ★ 两类可分（separation=%.4f）⇒ 阈值可用", sameMin-diffMax)
	} else {
		t.Logf("  ★ 两类重叠（separation=%.4f）⇒ 同属性不同值仍不可分",
			sameMin-diffMax)
	}
}

// ② 与 chineseclip 的正面对照：同一批数据，两个 provider。
//
// ★ 判据是**误报率**，不是平均相似度。粗筛要的是阈值有效。
func TestQwenVL_粗筛有效性对照(t *testing.T) {
	q := openQwenVL(t)
	defer q.Close()

	g := probeDB(t, "/var/tmp/ha-c/memory/graph.db")
	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Skip("无真库")
	}
	var distill []MemoryBlock
	for _, b := range blocks {
		if b.Source != "distill" || len(b.Vector) == 0 {
			continue
		}
		if _, _, _, ok := parseFact(b.Text); !ok {
			continue
		}
		distill = append(distill, b)
	}
	t.Logf("块 %d 个", len(distill))

	// 全部配对的相似度分布 —— 看「阈值是否有效」
	var qwAll, ccAll []float64
	qwAll = make([]float64, 0, len(distill)*len(distill)/2)
	ccAll = make([]float64, 0, len(distill)*len(distill)/2)
	cache := make(map[string][]float64, len(distill))
	for _, b := range distill {
		cache[b.ID] = q.vec(b.Text)
	}
	for i := 0; i < len(distill); i++ {
		for j := i + 1; j < len(distill); j++ {
			qwAll = append(qwAll, cosineVec(cache[distill[i].ID], cache[distill[j].ID]))
			ccAll = append(ccAll, cosineVec(distill[i].Vector, distill[j].Vector))
		}
	}
	t.Logf("  provider      平均      P50      P90      >0.9占比")
	report(t, "Qwen3-VL", qwAll)
	report(t, "chineseclip", ccAll)
}

// ③ 短文本（维度名）上能否打过词法。
//
// 判据：别名候选排序质量。词法是 dimSimilarity（见 alias_candidates_test.go）。
func TestQwenVL_短文本能否打过词法(t *testing.T) {
	q := openQwenVL(t)
	defer q.Close()

	pairs := []struct {
		a, b    string
		isAlias bool
		why     string
	}{
		{"批次号", "批次", true, "前缀包含，真别名"},
		{"等待队列告警阈值", "等待队列长度告警阈值", true, "插入两字，真别名"},
		{"版本", "版本号", true, "后缀包含，真别名"},
		{"灰度比例", "观察比例", true, "同义换词，词法漏检 ← 这里向量该赢"},
		{"值班分机号", "旧分机号", false, "共享「分机号」但语义不同（覆盖）"},
		{"端口", "端点", false, "未必同义"},
		{"停机时长", "发布窗口", false, "完全不同的维度"},
	}
	var qwTrue, qwFalse, lexTrue, lexFalse float64
	for _, p := range pairs {
		qs := cosineVec(q.vec(p.a), q.vec(p.b))
		ls := dimSimilarity(p.a, p.b)
		if p.isAlias {
			qwTrue += qs
			lexTrue += ls
		} else {
			qwFalse += qs
			lexFalse += ls
		}
		t.Logf("  别名=%-5v  Qwen=%.4f  词法=%.4f   %s", p.isAlias, qs, ls, p.why)
	}
	nT, nF := 4.0, 3.0
	t.Logf("\n  别名对平均: Qwen=%.4f  词法=%.4f", qwTrue/nT, lexTrue/nT)
	t.Logf("  非别名对平均: Qwen=%.4f  词法=%.4f", qwFalse/nF, lexFalse/nF)
	// 判别力 = 别名均分 − 非别名均分（越大越能排序）
	t.Logf("  判别力: Qwen=%.4f  词法=%.4f", qwTrue/nT-qwFalse/nF, lexTrue/nT-lexFalse/nF)
}

func report(t *testing.T, name string, xs []float64) {
	if len(xs) == 0 {
		return
	}
	sum := 0.0
	over := 0
	for _, x := range xs {
		sum += x
		if x > 0.9 {
			over++
		}
	}
	sorted := make([]float64, len(xs))
	copy(sorted, xs)
	// 简单插入排序（数据量小）
	for i := 1; i < len(sorted); i++ {
		v := sorted[i]
		j := i - 1
		for j >= 0 && sorted[j] > v {
			sorted[j+1] = sorted[j]
			j--
		}
		sorted[j+1] = v
	}
	t.Logf("  %-12s %.4f   %.4f   %.4f   %.1f%%",
		name, sum/float64(len(xs)),
		sorted[len(sorted)/2], sorted[len(sorted)*9/10],
		float64(over)/float64(len(xs))*100)
}
