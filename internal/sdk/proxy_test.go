package sdk

import "testing"

// 内置插件的运行期声明必须真的被登记、可枚举、可清除——
// remotedevice（内置、无 plugin.json）就靠这条通道。
func TestBuiltinProxyDeclRegistry(t *testing.T) {
	const p = "test_builtin_proxy"
	ClearBuiltinProxyDecls(p)
	defer ClearBuiltinProxyDecls(p)

	before := BuiltinProxyVersion()
	DeclareBuiltinProxy(p, ProxyDecl{Name: "gw", Host: "devices", Target: "127.0.0.1:9890", WebSocket: true, Auth: ProxyAuthNone})
	if BuiltinProxyVersion() == before {
		t.Error("登记后版本号应递增（反代层靠它判断缓存失效）")
	}

	got := BuiltinProxyDecls()
	list := got[p]
	if len(list) != 1 {
		t.Fatalf("登记了 %d 条，期望 1: %+v", len(list), got)
	}
	if !list[0].WebSocket || list[0].Auth != ProxyAuthNone || list[0].Host != "devices" {
		t.Errorf("声明内容不对: %+v", list[0])
	}

	// 重复登记同名（如自动重启后再次 Start）应为刷新而非累积
	DeclareBuiltinProxy(p, ProxyDecl{Name: "gw", Host: "devices", Target: "127.0.0.1:9890", WebSocket: true, Auth: ProxyAuthNone})
	if l := BuiltinProxyDecls()[p]; len(l) != 1 {
		t.Errorf("重复登记应为刷新，实际累积成 %d 条", len(l))
	}

	// 空 target 必须被拒（不声明的默认就是不被反代，空声明更不该登记）
	DeclareBuiltinProxy(p, ProxyDecl{Name: "bad", Target: "  "})
	if l := BuiltinProxyDecls()[p]; len(l) != 1 {
		t.Errorf("空 target 不应被登记，实际 %d 条", len(l))
	}

	ClearBuiltinProxyDecls(p)
	if _, ok := BuiltinProxyDecls()[p]; ok {
		t.Error("清除后不应还有该插件的声明")
	}
}

// DeclareProxy 必须把声明转发给内核注入的收集回调。
func TestDeclareProxyForwards(t *testing.T) {
	var got []ProxyDecl
	ps := New("demo", SDKConfig{})
	ps.SetProxyDeclarer(func(d ProxyDecl) { got = append(got, d) })

	ps.DeclareProxy(ProxyDecl{Name: "ui", Host: "demo", Target: "127.0.0.1:12100", WebSocket: false, Auth: ProxyAuthNone})
	if len(got) != 1 || got[0].Host != "demo" || got[0].Auth != ProxyAuthNone {
		t.Fatalf("声明未转发到收集回调: %+v", got)
	}
}
