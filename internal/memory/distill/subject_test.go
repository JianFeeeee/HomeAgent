package distill

import (
	"strings"
	"testing"
)

// ★ 主语推导的判据（全部来自 goldcases_test.go 的真库记录）。
//
// 这层**不调 LLM** —— 主语推导是纯规则，判据可以精确到「推导出了什么」，
// 而不是「最后有没有字段」。LLM 那层的模糊性留给 gold_test.go。
func TestDeriveSubjects(t *testing.T) {
	cases := []struct {
		record string
		want   []string
		why    string
	}{
		{"值班室分机号 4324，值班 老周（下周起；旧号 4379 停用）",
			[]string{"值班室分机号"}, "属性型：句首是受控别名"},
		{"值班室分机号 4379，值班人 阿李",
			[]string{"值班室分机号"}, "属性型旧值，同一主语不同值"},
		{"下周起值班室分机号改为 4324，旧号 4379 停用，值班轮换到 老周",
			[]string{"值班室分机号"}, "改述形态仍应归到同一主语"},
		{"admin服务端口8861·billing服务端口8499·oauth服务端口8271",
			[]string{"admin服务", "billing服务", "oauth服务"},
			"★ 一句多主语：三个服务各成一主语，主体词边界不含属性词"},
		{"第183批 告警规则9条·值班手册第4版·容量预警70%·排期10月",
			[]string{"第183批"}, "叙述型：显式批次号优先"},
		{"第129~143批均仅评审通过",
			[]string{"第129~143批"}, "范围批次号"},
		{"均仅评审通过，无具体字段", nil,
			"纯叙述句推不出主语 —— 宁可少记也不写悬空属性"},
	}

	for _, c := range cases {
		got := DeriveSubjects(c.record)
		var names []string
		for _, s := range got {
			names = append(names, s.Name)
		}
		if !equalStrs(names, c.want) {
			t.Errorf("主语推导错（%s）\n  记录: %q\n  期望: %v\n  实得: %v",
				c.why, c.record, c.want, names)
		}
	}
}

// 别名归一：同一属性不得裂成两个主语。
func TestDeriveSubjects_别名归一(t *testing.T) {
	for _, rec := range []string{
		"值班分机号 4324",
		"分机号 4324",
		"值班室电话 4324",
	} {
		got := DeriveSubjects(rec)
		if len(got) != 1 {
			t.Errorf("%q 应推出 1 个主语，实际 %d", rec, len(got))
			continue
		}
		if got[0].Name != "值班室分机号" {
			t.Errorf("%q 主语应归一到「值班室分机号」，实际 %q", rec, got[0].Name)
		}
	}
}

// indexOfRunes 的契约：未找到必须是 -1。
//
// ★ 这条是被真实 bug 逼出来的：初版对「needle 比 haystack 长」返回 0，
// 于是词表里的中文词（"节点"/"队列"/"网关"）在 ASCII 串 "admin服务端口8861"
// 上全部报「命中于位置 0」，主语被截成 "ad"/"bi"/"oa"。
func TestIndexOfRunes_未找到返回负一(t *testing.T) {
	hay := []rune("admin服务端口8861")
	for _, needle := range []string{"节点", "队列", "网关", "订单", "不存在的词"} {
		if got := indexOfRunes(hay, []rune(needle)); got >= 0 {
			t.Errorf("indexOfRunes(%q, %q) = %d，应为 -1（未找到）", string(hay), needle, got)
		}
	}
	if got := indexOfRunes(hay, []rune("服务")); got != 5 {
		t.Errorf("indexOfRunes 应命中位置 5，实际 %d", got)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ = strings.TrimSpace
