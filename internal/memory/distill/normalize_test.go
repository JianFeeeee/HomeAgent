package distill

import "testing"

// ★ 泛词不能跨词边界吞掉更具体的维度名。
//
// 真库实测的反例：NormalizeDimension("连接池容量") 曾返回 "容量预警" ——
// 「容量」是 2 字符泛词，命中了「连接池**容量**」的后半截，把两个不同的
// 维度静默合并了。维度名是建边的依据，合并后边就建错。
//
// 判据：宁可留一个未归一的维度名（漂移发现机制还能报出来），
// 也不要把两个真维度错并成一个。
func TestNormalizeDimension_不跨词误伤(t *testing.T) {
	// 不该被泛词吞掉（保持原名）
	for _, in := range []string{
		"连接池容量",
		"连接池容量上限",
		"队列容量",
	} {
		if got := NormalizeDimension(in); got != in {
			t.Errorf("NormalizeDimension(%q) = %q，应保持原名（不该被泛词吞掉）", in, got)
		}
	}
}

// 该归一的仍要归一 —— 防「过度保护」把所有包含匹配都关掉。
func TestNormalizeDimension_仍要归一(t *testing.T) {
	cases := []struct{ in, want string }{
		{"停机时长(分钟)", "停机时长"}, // 带后缀，仍应归一
		{"值班手册第8版", "值班手册版本"},
		{"值班手册", "值班手册版本"},
		{"排期10月", "排期"},
		{"告警规则数", "告警规则数"},
		{"灰度比例", "灰度比例"},
		{"回滚版本", "回滚版本"},
		{"发布时间", "发布窗口"},
		{"容量预警", "容量预警"},
	}
	for _, c := range cases {
		if got := NormalizeDimension(c.in); got != c.want {
			t.Errorf("NormalizeDimension(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// ★ 跨词误伤保护的边界：**按别名长度**分档，不是一刀切。
//
// 上一版「保护不能过宽」的判据写成了「位置 >0 但该归一仍要归一」——
// 但 isCrossWordTailMatch 在位置 >0 时必然要求「前缀不在词表」，
// 而前缀在词表的又已提前 return false。所以那个组合**不可达**，
// 测试无论实现怎么写都绿（两轮假绿）。
//
// 可达的边界是长度分档：≤3 字符的泛词容易误伤（连接池|容量），
// >3 字符的别名更可能是完整维度名（值班手册版本、停机时长…）。
// 判据：把长别名也当误伤时，真实的长维度归一会失效。
func TestNormalizeDimension_长别名不判误伤(t *testing.T) {
	// 前缀是独立词 + 长别名 ⇒ 应归一（保护只拦短泛词）
	in := "系统值班手册版本"
	if got := NormalizeDimension(in); got != "值班手册版本" {
		t.Errorf("NormalizeDimension(%q) = %q，期望 值班手册版本（长别名不该判误伤）",
			in, got)
	}
	in2 := "服务停机时长"
	if got := NormalizeDimension(in2); got != "停机时长" {
		t.Errorf("NormalizeDimension(%q) = %q，期望 停机时长（长别名不该判误伤）",
			in2, got)
	}
}

// 位置 0 的精确匹配永远不是误伤（最长匹配优先仍然生效）。
func TestNormalizeDimension_最长匹配优先(t *testing.T) {
	// 「停机」与「停机时长」同 canonical，但「排期」vs「排期月」这类
	// 一对多映射下必须命中最长的那个。
	if got := NormalizeDimension("停机时长"); got != "停机时长" {
		t.Errorf("停机时长 应归一到自身，实际 %q", got)
	}
	if got := NormalizeDimension("停机"); got != "停机时长" {
		t.Errorf("停机 应归一到 停机时长，实际 %q", got)
	}
}
