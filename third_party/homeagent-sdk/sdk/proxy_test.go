package sdk

import "testing"

func TestProxyAuthDefaultsToHomeAgent(t *testing.T) {
	// 空串必须归一化为「HomeAgent 统一保护」——这是安全默认。
	// 若哪天有人把默认改成 none，这条会立刻红。
	if got := EffectiveProxyAuth(""); got != ProxyAuthHomeAgent {
		t.Fatalf("空 auth 应归一化为 %q，实际 %q", ProxyAuthHomeAgent, got)
	}
	if got := EffectiveProxyAuth(ProxyAuthNone); got != ProxyAuthNone {
		t.Fatalf("显式 none 应保持 none，实际 %q", got)
	}
	for _, ok := range []string{"", ProxyAuthHomeAgent, ProxyAuthNone} {
		if !ValidProxyAuth(ok) {
			t.Errorf("%q 应合法", ok)
		}
	}
	for _, bad := range []string{"nope", "HOMEAGENT", "None", "true"} {
		if ValidProxyAuth(bad) {
			t.Errorf("%q 应非法", bad)
		}
	}
}

func TestValidProxyHostLabel(t *testing.T) {
	legit := []string{"huawei", "a", "a-b", "abc123", "0", "x" + string(make([]byte, 0)) + "yz"}
	for _, s := range legit {
		if !ValidProxyHostLabel(s) {
			t.Errorf("%q 应为合法 label", s)
		}
	}
	bad := []string{
		"", "-a", "a-", "-", "a_b", "a.b", "A", "aB", "a b",
		"a/b", "a:b", string(make([]byte, 64)), // 超长 63
	}
	for _, s := range bad {
		if ValidProxyHostLabel(s) {
			t.Errorf("%q 应为非法 label", s)
		}
	}
	// 边界：恰好 63 合法，64 非法
	l63 := ""
	for i := 0; i < 63; i++ {
		l63 += "a"
	}
	if !ValidProxyHostLabel(l63) {
		t.Error("63 字符应为合法 label")
	}
	if ValidProxyHostLabel(l63 + "a") {
		t.Error("64 字符应为非法 label")
	}
}

func TestNormalizeProxyHost(t *testing.T) {
	cases := map[string]string{
		"huawei_smarthome": "huawei-smarthome", // 下划线不是合法 DNS label
		"webui":            "webui",
		"UPPER_Case":       "upper-case",
		"a__b":             "a--b",
		"__x__":            "x",
		"---":              "plugin", // 全非法 → 保守回退
		"":                 "plugin",
		"a.b.c":            "abc",
	}
	for in, want := range cases {
		if got := NormalizeProxyHost(in); got != want {
			t.Errorf("NormalizeProxyHost(%q) = %q，期望 %q", in, got, want)
		}
	}
	// 归一化结果必须自身合法（产物自洽）
	for _, in := range []string{"huawei_smarthome", "UPPER_Case", "__x__", "a.b.c", "非常长的名字非常长的名字非常长的名字非常长的名字非常长的名字非常长的名字非常长的名字"} {
		if got := NormalizeProxyHost(in); !ValidProxyHostLabel(got) {
			t.Errorf("NormalizeProxyHost(%q) = %q 不合法", in, got)
		}
	}
}

func TestValidateProxyDecl(t *testing.T) {
	valid := []ProxyDecl{
		{Target: "127.0.0.1:12100"},
		{Target: "http://127.0.0.1:12100"},
		{Target: "127.0.0.1:12100", Host: "huawei"},
		{Target: "127.0.0.1:12100", Auth: ProxyAuthNone},
		{Target: "127.0.0.1:12100", Auth: ProxyAuthHomeAgent, WebSocket: true},
		{Target: "127.0.0.1:3000/base", Host: "x"},
		{Target: "https://example.com", Host: "ext"}, // 远程上游也允许（由 auth 决定安全性）
	}
	for _, d := range valid {
		if msg := ValidateProxyDecl(d); msg != "" {
			t.Errorf("%+v 应合法，却报: %s", d, msg)
		}
	}

	bad := []ProxyDecl{
		{},                                  // 无 target
		{Target: "   "},                     // 空白 target
		{Target: "127.0.0.1:12100", Auth: "yes"}, // auth 非法
		{Target: "127.0.0.1:12100", Host: "a_b"}, // host 非法
		{Target: "127.0.0.1:12100", Host: "-x"},
		{Target: "127.0.0.1:12100", Host: "X"},
		{Target: "://12100"},         // 无主机
		{Target: "http:///path"},     // 无主机
		{Target: "127.0.0.1:notaport"}, // 端口非数字
	}
	for _, d := range bad {
		if msg := ValidateProxyDecl(d); msg == "" {
			t.Errorf("%+v 应被拒绝，却通过了", d)
		}
	}
}
