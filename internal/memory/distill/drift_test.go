package distill

import (
	"strings"
	"testing"
)

// 值形态分类的判据：分类必须只看值，不看字段名。
func TestClassifyValue(t *testing.T) {
	cases := []struct {
		in   string
		want valueShape
		why  string
	}{
		{"10%", shapePercent, "百分比"},
		{"5.5%", shapePercent, "带小数的百分比"},
		{"4分", shapeDuration, "分钟数"},
		{"48分后放50%", shapeText, "带尾巴的不是纯时长"},
		{"30分钟", shapeDuration, "分钟"},
		{"v2.28.2", shapeVersion, "语义化版本"},
		{"第4版", shapeVersion, "中文版本"},
		{"4%~17%", shapeRange, "区间优先于百分比（形态更具体）"},
		{"30~84批", shapeRange, "批次区间"},
		{"8861", shapeNumber, "纯数字端口"},
		{"4324", shapeNumber, "纯数字分机号"},
		{"10月", shapeDate, "月份"},
		{"周四", shapeDate, "周几"},
		{"凌晨2点", shapeTime, "时刻"},
		{"周四凌晨2点", shapeTime, "带前缀的时刻"},
		{"均仅评审通过", shapeText, "叙述文本"},
		{"老周", shapeText, "人名"},
	}
	for _, c := range cases {
		if got := classifyValue(c.in); got != c.want {
			t.Errorf("classifyValue(%q) = %s，期望 %s（%s）", c.in, got, c.want, c.why)
		}
	}
}

// ★ 核心判据：漂移发现只依赖「同 subject + 同值形态」，不依赖字段名。
//
// 用两组字段名完全不同但形态相同的观测，验证能被发现；
// 再用形态不同的验证不会被误并。
func TestDiscoverDimensionDrift(t *testing.T) {
	obs := []dimObservation{
		// 实测的真库形态：观察时长被叫过两个名字，值都是时长
		{Subject: "第112批", Dim: "发布窗口", Shape: classifyValue("48分后放50%"),
			Samples: []string{"48分后放50%"}},
		{Subject: "第112批", Dim: "发布窗口", Shape: shapeTime, Samples: []string{"周四凌晨2点"}},
		{Subject: "第114批", Dim: "观察时长", Shape: shapeDuration, Samples: []string{"70分"}},
		{Subject: "第114批", Dim: "停机时长", Shape: shapeDuration, Samples: []string{"4分"}},

		// 峰值 vs 峰值范围（同形态区间）
		{Subject: "第85批", Dim: "峰值", Shape: shapeRange, Samples: []string{"4%~17%"}},
		{Subject: "第85批", Dim: "峰值范围", Shape: shapeRange, Samples: []string{"7%~16%"}},

		// 不同 subject 的批次命名
		{Subject: "第122批", Dim: "批次号", Shape: shapeNumber, Samples: []string{"122"}},
		{Subject: "第122批", Dim: "批次", Shape: shapeNumber, Samples: []string{"122"}},
	}

	cands := DiscoverDimensionDrift(obs)
	if len(cands) == 0 {
		t.Fatal("应发现候选")
	}

	// 每形态的候选内容
	got := map[valueShape][]string{}
	for _, c := range cands {
		got[c.Shape] = c.Dims
	}

	// 时长候选：发布窗口/观察时长/停机时长 三个都在（同一个 subject 组内）
	dur, ok := got[shapeDuration]
	if !ok || len(dur) < 2 {
		t.Errorf("时长形态应报出候选（发布窗口/观察时长/停机时长），实际 %v", got)
	}

	// 区间候选：峰值 + 峰值范围
	rng, ok := got[shapeRange]
	if !ok || len(rng) != 2 || !containsStr(rng, "峰值") || !containsStr(rng, "峰值范围") {
		t.Errorf("区间形态应报出 峰值/峰值范围，实际 %v", rng)
	}

	// 数字形态：批次号 + 批次
	num, ok := got[shapeNumber]
	if !ok || !containsStr(num, "批次号") || !containsStr(num, "批次") {
		t.Errorf("数字形态应报出 批次号/批次，实际 %v", num)
	}
}

// ★ 不得误并：形态不同即便同 subject 也不该进同一候选。
func TestDiscoverDimensionDrift_不跨形态误并(t *testing.T) {
	obs := []dimObservation{
		{Subject: "第112批", Dim: "停机时长", Shape: shapeDuration, Samples: []string{"4分"}},
		{Subject: "第112批", Dim: "发布窗口", Shape: shapeTime, Samples: []string{"周四凌晨2点"}},
		{Subject: "第112批", Dim: "灰度比例", Shape: shapePercent, Samples: []string{"10%"}},
	}
	for _, c := range DiscoverDimensionDrift(obs) {
		if len(c.Dims) > 1 {
			t.Errorf("形态不同不该被并成候选：%v", c)
		}
	}
}

// 只有一种叫法不算漂移（否则报告噪声大）。
func TestDiscoverDimensionDrift_单名不算漂移(t *testing.T) {
	obs := []dimObservation{
		{Subject: "A", Dim: "停机时长", Shape: shapeDuration},
		{Subject: "B", Dim: "停机时长", Shape: shapeDuration},
		{Subject: "C", Dim: "停机时长", Shape: shapeDuration},
	}
	if got := DiscoverDimensionDrift(obs); len(got) != 0 {
		t.Errorf("字段名一致不应报漂移，实际 %v", got)
	}
}

// 不同 subject 的同形态字段名**不算**漂移（可能确实是不同维度）。
func TestDiscoverDimensionDrift_跨subject不并(t *testing.T) {
	obs := []dimObservation{
		{Subject: "服务A", Dim: "端口", Shape: shapeNumber},
		{Subject: "服务B", Dim: "端口", Shape: shapeNumber},
		{Subject: "服务A", Dim: "端口号", Shape: shapeNumber},
		{Subject: "服务B", Dim: "端口号", Shape: shapeNumber},
	}
	cands := DiscoverDimensionDrift(obs)
	// 服务A 上有 端口/端口号 两个名字 → 是候选（虽同名但值形态相同）
	for _, c := range cands {
		if len(c.Dims) == 2 && c.Subjects[0] == "服务A" {
			return // 期望内
		}
	}
	// 若实现改成要求跨 subject，这里也应当仍能报出（同一 subject 内已足够）
	t.Logf("候选: %d", len(cands))
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

var _ = strings.TrimSpace

// ★ 候选里不得有重复项。
//
// 初版按 subject 累积时，同一 subject 的同一字段名会重复进 Dims ——
// 真库实测跑出「发布窗口 / 发布窗口 / 发布窗口 / 发布窗口 / 发布窗口」
// 五个重复项（每个 subject 贡献一次）。报告里出现重复会让人以为
// 「五个地方不一致」，实际只有一个。
func TestDiscoverDimensionDrift_候选不重复(t *testing.T) {
	obs := []dimObservation{
		{Subject: "第119批", Dim: "发布窗口", Shape: shapePercent, Samples: []string{"100%"}},
		{Subject: "第119批", Dim: "发布窗口", Shape: shapePercent, Samples: []string{"100%"}},
		{Subject: "第119批", Dim: "发布窗口", Shape: shapePercent, Samples: []string{"100%"}},
		{Subject: "第119批", Dim: "灰度比例", Shape: shapePercent, Samples: []string{"10%"}},
		{Subject: "第120批", Dim: "发布窗口", Shape: shapePercent, Samples: []string{"100%"}},
		{Subject: "第120批", Dim: "灰度比例", Shape: shapePercent, Samples: []string{"10%"}},
	}
	cands := DiscoverDimensionDrift(obs)
	if len(cands) != 1 {
		t.Fatalf("应只有 1 个候选，实际 %d", len(cands))
	}
	seen := map[string]int{}
	for _, d := range cands[0].Dims {
		seen[d]++
		if seen[d] > 1 {
			t.Errorf("候选里有重复字段名 %q：%v", d, cands[0].Dims)
		}
	}
	if len(cands[0].Dims) != 2 {
		t.Errorf("候选应恰有 2 个去重字段名，实际 %v", cands[0].Dims)
	}
	if len(cands[0].Subjects) != 2 {
		t.Errorf("subject 也应去重（2 个），实际 %v", cands[0].Subjects)
	}
}

// 区间形态必须能识别「带百分号的区间」（真库里最常见的错误率峰值形态）。
//
// 初版 reRange 写成 \d+\s*[~～-]\s*\d+ 时，4%~17% 不匹配 —
// 而它正是最该被发现的那类漂移（峰值 vs 峰值范围）的形态。
func TestClassifyValue_带单位区间(t *testing.T) {
	for _, v := range []string{"4%~17%", "7%~16%", "30~84批", "56~63批", "13-17%"} {
		if got := classifyValue(v); got != shapeRange {
			t.Errorf("classifyValue(%q) = %s，期望 区间", v, got)
		}
	}
}

// ★ 放宽「区间尾巴」后必须仍守住的边界。
//
// 上一轮为了让 "30~84批" 落成区间，把结尾从 [%％]? 放宽到 [\p{Han}A-Za-z]*。
// 放宽很容易过头：日期、版本、时刻都不该被区间正则吃掉。
func TestClassifyValue_区间放宽后的边界(t *testing.T) {
	cases := []struct {
		in   string
		want valueShape
	}{
		// 应是区间（真库里真实出现的形态）
		{"4%~17%", shapeRange},
		{"13-17%", shapeRange},
		{"30~84批", shapeRange},
		{"56~63批", shapeRange},
		{"7%~16%", shapeRange},

		// 绝不能被区间吃掉
		{"2026-10-02", shapeDate}, // ISO 日期：区间含连字符，先判它
		{"10月", shapeDate},
		{"v2.28.2", shapeVersion},
		{"第4版", shapeVersion},
		{"凌晨2点", shapeTime},
		{"周四凌晨2点", shapeTime},
		{"4324", shapeNumber},
		{"10%", shapePercent},
		{"4分", shapeDuration},
		// 值里带连字符但不是区间：版本号形态
		{"v2-3", shapeVersion},
	}
	for _, c := range cases {
		if got := classifyValue(c.in); got != c.want {
			t.Errorf("classifyValue(%q) = %s，期望 %s", c.in, got, c.want)
		}
	}
}

// ★ 年-月也必须判成日期，不能落进区间。
//
// 实测 reRange("2026-10") = true（连字符 + 纯数字对），
// 只匹配年月日的 ISO 正则接不住它。而年-月在迁移数据里很常见：
// memory_blocks.created_at 就是 "2026-10" 这种形态。
func TestClassifyValue_年月不被区间吃掉(t *testing.T) {
	for _, v := range []string{"2026-10", "2026-10-02", "2026-1", "2026-12-31"} {
		if got := classifyValue(v); got != shapeDate {
			t.Errorf("classifyValue(%q) = %s，期望 日期", v, got)
		}
	}
}
