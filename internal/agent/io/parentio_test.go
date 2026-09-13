package io

import "testing"

// 上级回退：驻留子的轻量内核有自己的 IOManager，但输出通道（io 里的 Device）
// 是插件登记在**父**的 io 上的。子若看不见它们，`output_send__<通道>` 会被判
// "通道不存在或不可用"、`output_list_channels` 为空 —— 现场联调实录
// 「父侧通道装载完整、子侧 childIO 空壳」。
func TestIOManagerParentFallback(t *testing.T) {
	parent := NewIOManager()
	if err := parent.RegisterDevice(&mockDevice{name: "qq", devType: DeviceOutput, caps: CapText}); err != nil {
		t.Fatal(err)
	}

	child := NewIOManager()
	// 未挂上级时行为与以前完全一致（不能悄悄多出通道）
	if got := child.GetChannelCapabilities("qq"); got != 0 {
		t.Fatalf("无上级时不应看见父的通道，得到 %v", got)
	}
	if n := len(child.ListChannels()); n != 0 {
		t.Fatalf("无上级时通道数应为 0，得到 %d", n)
	}

	child.SetParentIO(parent)
	if got := child.GetChannelCapabilities("qq"); got != CapText {
		t.Fatalf("挂上级后应看见父通道能力 CapText，得到 %v", got)
	}
	if dev := child.GetDevice("qq"); dev == nil || dev.Name() != "qq" {
		t.Fatalf("GetDevice 未回退到父: %v", dev)
	}
	if n := len(child.ListChannels()); n != 1 {
		t.Fatalf("ListChannels 未回退到父，得到 %d 条", n)
	}

	// **实时**回退而非快照：父后来登记的通道，子立刻可见。
	// （设备随资源生灭 —— 远程设备上线/掉线以分钟计，快照一分钟就过期）
	if err := parent.RegisterDevice(&mockDevice{name: "newdev", devType: DeviceOutput, caps: CapImage}); err != nil {
		t.Fatal(err)
	}
	if got := child.GetChannelCapabilities("newdev"); got != CapImage {
		t.Fatalf("子应实时看见父新登记的通道，得到 %v", got)
	}

	// 父掉线注销后，子也立刻看不见（不是复制出来的旧表）
	parent.UnregisterDevice("newdev")
	if got := child.GetChannelCapabilities("newdev"); got != 0 {
		t.Fatalf("父注销后子不应再看见，得到 %v", got)
	}
}

// 自己的登记优先：子可以覆盖/屏蔽同名通道，父的登记不会重复列出。
func TestIOManagerOwnDeviceWins(t *testing.T) {
	parent := NewIOManager()
	if err := parent.RegisterDevice(&mockDevice{name: "ch", devType: DeviceOutput, caps: CapText}); err != nil {
		t.Fatal(err)
	}
	child := NewIOManager()
	child.SetParentIO(parent)
	if err := child.RegisterDevice(&mockDevice{name: "ch", devType: DeviceOutput, caps: CapImage}); err != nil {
		t.Fatal(err)
	}

	if got := child.GetChannelCapabilities("ch"); got != CapImage {
		t.Fatalf("同名时自己的登记应优先，得到 %v", got)
	}
	list := child.ListChannels()
	if len(list) != 1 {
		t.Fatalf("同名通道不应重复列出，得到 %d 条", len(list))
	}
	if list[0].OutputCaps != CapImage {
		t.Fatalf("列出的应是子自己的那条，得到 %v", list[0].OutputCaps)
	}
}

// 设备工具（io.ExecuteTool）同样回退：子的设备工具都在父的 io 上。
func TestIOManagerExecuteToolFallsBackToParent(t *testing.T) {
	parent := NewIOManager()
	called := 0
	if err := parent.RegisterDevice(&mockDevice{
		name: "dev", devType: DeviceIO,
		tools: []ToolDef{{Name: "dev_do", Description: "干点什么"}},
		executeFn: func(tool string, args map[string]interface{}) (interface{}, error) {
			called++
			return "parent-done", nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	child := NewIOManager()
	if _, err := child.ExecuteTool("dev_do", nil); err == nil {
		t.Fatal("无上级时不该能执行父的设备工具")
	}
	child.SetParentIO(parent)
	got, err := child.ExecuteTool("dev_do", map[string]interface{}{"x": 1})
	if err != nil {
		t.Fatalf("应回退到父执行: %v", err)
	}
	if got != "parent-done" || called != 1 {
		t.Fatalf("执行结果=%v called=%d", got, called)
	}
}
