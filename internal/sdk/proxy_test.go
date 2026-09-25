package sdk

import "testing"

// 内置插件的运行期声明必须真的被登记、可枚举、可清除——
// remotedevice（内置、无 plugin.json）就靠这条通道。
func TestBuiltinProxyDefRegistry(t *testing.T) {
	const p = "test_builtin_proxy"
	ClearBuiltinProxyDefs(p)
	defer ClearBuiltinProxyDefs(p)

	before := BuiltinProxyVersion()
	RegisterBuiltinProxy(p, "gw", ProxyDef{Name: "gw", Host: "devices", Target: "127.0.0.1:9890", WebSocket: true, Auth: ProxyAuthNone})
	if BuiltinProxyVersion() == before {
		t.Error("登记后版本号应递增（反代层靠它判断缓存失效）")
	}

	got := BuiltinProxyDefs()
	list := got[p]
	if len(list) != 1 {
		t.Fatalf("登记了 %d 条，期望 1: %+v", len(list), got)
	}
	if !list[0].WebSocket || list[0].Auth != ProxyAuthNone || list[0].Host != "devices" {
		t.Errorf("声明内容不对: %+v", list[0])
	}

	// 重复登记同名（如自动重启后再次 Start）应为刷新而非累积
	RegisterBuiltinProxy(p, "gw", ProxyDef{Name: "gw", Host: "devices", Target: "127.0.0.1:9890", WebSocket: true, Auth: ProxyAuthNone})
	if l := BuiltinProxyDefs()[p]; len(l) != 1 {
		t.Errorf("重复登记应为刷新，实际累积成 %d 条", len(l))
	}

	// 空 target 必须被拒（不声明的默认就是不被反代，空声明更不该登记）
	RegisterBuiltinProxy(p, "bad", ProxyDef{Name: "bad", Target: "  "})
	if l := BuiltinProxyDefs()[p]; len(l) != 1 {
		t.Errorf("空 target 不应被登记，实际 %d 条", len(l))
	}

	ClearBuiltinProxyDefs(p)
	if _, ok := BuiltinProxyDefs()[p]; ok {
		t.Error("清除后不应还有该插件的声明")
	}
}

// RegisterProxy 必须把声明转发给内核注入的注册回调，且把 name 落进 def
// （与 RegisterTool 的风格一致：name 同时来自参数与 def.Name）。
func TestRegisterProxyForwards(t *testing.T) {
	var gotName string
	var got []ProxyDef
	ps := New("demo", SDKConfig{})
	ps.SetProxyRegistrar(func(name string, d ProxyDef) {
		gotName = name
		got = append(got, d)
	})

	ps.RegisterProxy("ui", ProxyDef{Name: "ui", Host: "demo", Target: "127.0.0.1:12100", Auth: ProxyAuthNone})
	if len(got) != 1 || got[0].Host != "demo" || got[0].Auth != ProxyAuthNone {
		t.Fatalf("声明未转发到注册回调: %+v", got)
	}
	if gotName != "ui" {
		t.Errorf("name 参数未透传: %q", gotName)
	}
}
