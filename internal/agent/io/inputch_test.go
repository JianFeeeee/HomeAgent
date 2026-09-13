package io

// inputch 登记表与划分（单工具多视图背后的数据面）。
//
// 设计依据 docs/zh/resident-subagent-design.md §4：
// inputch 是**最基本的输入路由单位**，由插件注册，一个插件可注册多个。

import "testing"

func TestChannelRegistry_RegisterKeepsAttribution(t *testing.T) {
	r := NewChannelRegistry()

	// 一个插件注册多个 inputch（最基本的输入路由单位）。
	if err := r.Register(InputChannel{Name: "qq", Plugin: "qq", Def: ChannelDef{NoMemory: true}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(InputChannel{Name: "qq/device-2", Plugin: "qq"}); err != nil {
		t.Fatal(err)
	}
	if n := r.Count(); n != 2 {
		t.Fatalf("注册数=%d，期望 2（同一插件的多个 inputch 彼此独立）", n)
	}

	ch, ok := r.Lookup("qq/device-2")
	if !ok || ch.Plugin != "qq" {
		t.Fatalf("归属插件未记录：%+v ok=%v", ch, ok)
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Fatal("未注册的 inputch 不应查得到")
	}
	if err := r.Register(InputChannel{Name: ""}); err != ErrInputChannelNameEmpty {
		t.Fatalf("空名应报错，实际 %v", err)
	}
}

// 插件重载（重复登记）**不得抹掉划分**：Owner/Capacity/Output 必须保留。
func TestChannelRegistry_ReRegisterKeepsAllocation(t *testing.T) {
	r := NewChannelRegistry()
	_ = r.Register(InputChannel{Name: "qq", Plugin: "qq"})
	if err := r.Assign("qq", "agent-1", 128); err != nil {
		t.Fatal(err)
	}

	// 插件重载：只带 Plugin/Def，不带 Owner/Capacity。
	if err := r.Register(InputChannel{Name: "qq", Plugin: "qq", Def: ChannelDef{NoMemory: true}}); err != nil {
		t.Fatal(err)
	}
	ch, _ := r.Lookup("qq")
	if ch.Owner != "agent-1" || ch.Capacity != 128 {
		t.Fatalf("重载后划分被抹掉：owner=%q capacity=%d", ch.Owner, ch.Capacity)
	}
	if !ch.Def.NoMemory {
		t.Fatal("重载应更新 Def")
	}
}

func TestChannelRegistry_AssignAndViews(t *testing.T) {
	r := NewChannelRegistry()
	_ = r.Register(InputChannel{Name: "qq", Plugin: "qq"})
	_ = r.Register(InputChannel{Name: "cli", Plugin: "cli"})
	_ = r.Register(InputChannel{Name: "sub/in", Plugin: "sub"})

	if err := r.Assign("qq", "root", 0); err != nil {
		t.Fatal(err)
	}
	if err := r.Assign("sub/in", "child-1", 32); err != nil {
		t.Fatal(err)
	}
	if err := r.Assign("nope", "x", 0); err != ErrInputChannelUnknown {
		t.Fatalf("划分未注册的 inputch 应报错，实际 %v", err)
	}

	if got := len(r.ListByOwner("root")); got != 1 {
		t.Fatalf("root 的 inputch 数=%d，期望 1", got)
	}
	child := r.ListByOwner("child-1")
	if len(child) != 1 || child[0].Name != "sub/in" || child[0].Capacity != 32 {
		t.Fatalf("child-1 的划分=%+v", child)
	}
	// 未分配的：cli。
	un := r.ListByOwner("")
	if len(un) != 1 || un[0].Name != "cli" {
		t.Fatalf("未分配的 inputch=%+v，期望只有 cli", un)
	}
	// List 按名排序（视图输出稳定）。
	all := r.List()
	if len(all) != 3 || all[0].Name != "cli" || all[2].Name != "sub/in" {
		t.Fatalf("List 未按名排序：%+v", all)
	}
}

// 共享登记表：根 agent 与驻留子共用同一份，划分才有意义。
func TestChannelRegistry_SharedBetweenManagers(t *testing.T) {
	shared := NewChannelRegistry()
	root := NewIOManager()
	child := NewIOManager()
	root.SetChannelRegistry(shared)
	child.SetChannelRegistry(shared)

	if err := root.RegisterInputChannelFrom("sub", "sub/in", ChannelDef{}); err != nil {
		t.Fatal(err)
	}
	if err := root.AssignInputChannel("sub/in", "child-1", 8); err != nil {
		t.Fatal(err)
	}

	// 子在**自己**的 io 上就能看到这份划分。
	ch, ok := child.LookupInputChannel("sub/in")
	if !ok {
		t.Fatal("共享登记表后，子应看得到根注册的 inputch")
	}
	if ch.Owner != "child-1" || ch.Capacity != 8 {
		t.Fatalf("子看到的划分=%+v", ch)
	}

	// 注销也要跨 manager 生效。
	root.UnregisterInputChannel("sub/in")
	if _, ok := child.LookupInputChannel("sub/in"); ok {
		t.Fatal("注销应跨 manager 生效")
	}
}

// 记忆策略查询保持向后兼容（原 GetInputChannelDef 的语义）。
func TestChannelRegistry_DefLookupCompat(t *testing.T) {
	m := NewIOManager()
	m.RegisterInputChannel("qq", ChannelDef{NoMemory: true, ContextPolicy: "prune"})
	def, ok := m.GetInputChannelDef("qq")
	if !ok || !def.NoMemory || def.ContextPolicy != "prune" {
		t.Fatalf("策略查询=%+v ok=%v", def, ok)
	}
	if _, ok := m.GetInputChannelDef("nope"); ok {
		t.Fatal("未注册的 inputch 不应有策略")
	}
}

// TestOutputTarget_UnbindAndUnregisterCleanup 守住 outputTargets 的清理：
//   - UnbindOutputTarget 幂等解绑；
//   - Unregister(inputch) 顺手清掉指向它的目标登记（显式绑定 + 同名回退两种）。
//
// 背景：outputTargets 只增不减是漏项（审查发现）——通道随资源生灭时会留下
// 指向已不存在 agent/inputch 的路由。
func TestOutputTarget_UnbindAndUnregisterCleanup(t *testing.T) {
	r := NewChannelRegistry()

	if err := r.Register(InputChannel{Name: "sub/in"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(InputChannel{Name: "same"}); err != nil {
		t.Fatal(err)
	}
	if err := r.BindOutputTarget("to-child", "child-1", "sub/in"); err != nil {
		t.Fatal(err)
	}
	// InputCh=="" 表示"与输出通道同名"的回退。
	if err := r.BindOutputTarget("same", "child-2", ""); err != nil {
		t.Fatal(err)
	}

	if _, ok := r.ResolveOutputTarget("to-child"); !ok {
		t.Fatal("绑定后应能解析")
	}
	r.UnbindOutputTarget("to-child")
	if _, ok := r.ResolveOutputTarget("to-child"); ok {
		t.Fatal("解绑后不应再解析")
	}
	r.UnbindOutputTarget("to-child") // 幂等：重复解绑不 panic

	// 注销 inputch：显式绑定到它的、以及同名回退的，都要一起清。
	r.Unregister("sub/in")
	if _, ok := r.ResolveOutputTarget("to-child"); ok {
		t.Fatal("显式绑定到已注销 inputch 的目标应被清理")
	}
	r.Unregister("same")
	if _, ok := r.ResolveOutputTarget("same"); ok {
		t.Fatal("同名回退目标应随该 inputch 注销被清理")
	}
}
