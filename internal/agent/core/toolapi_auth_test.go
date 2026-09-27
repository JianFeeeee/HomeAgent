package core

import (
	"strings"
	"testing"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	"gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// toolAPIOf 造出与插件侧**完全同一个** ToolAPI 实现（PluginSDK.Tool()
// 内部就是 sdk.NewTool(stageHost, iom)）。
// 刻意不另写一份判据实现——两处会漂移，而漂移本身就是漏洞。
func toolAPIOf(t *testing.T, a *Agent) sdk.ToolAPI {
	// 注入当前 agent 的授权判据：与 bootstrap 装配时的做法一致。
	// 不注入则 CanUse 对设备放行（那是"尚未接线"的状态，见 sdk 包注释）。
	sdk.SetDeviceAuthQuery(func(deviceID string) bool {
		return a.IsOutputAllowed("device/" + deviceID)
	})
	t.Cleanup(func() { sdk.SetDeviceAuthQuery(nil) })
	// 方案 B：把本 agent 的内置工具面注入（真实路径里由 bootstrap/新建 agent 时做）
	sdk.SetBuiltinProvider(builtinProvider{a: a})
	t.Cleanup(func() { sdk.SetBuiltinProvider(nil) })
	return sdk.NewTool(a.stageHost, a.io)
}

// 阶段 D4：设备授权闸下沉到 ToolAPI 路径。
//
// 问题：设备类工具的授权闸只存在于 `executeToolCallInner`
// （toolcall.go:151-152），即**「agent 收到模型 tool_call」这条路径**。
// 而 `ToolAPI.ExecuteTool` 是另一条独立的执行入口，**不经那道闸**。
//
// 实测范围（不止 seq）：`cli` 插件的 /terminal 直接经 ToolAPI 调
// agentcli 的终端工具（cli/plugin.go:1038 的注释自陈"SDK 的 ToolAPI
// 已允许跨插件调用工具"），这条路同样不过闸。
// ⇒ 凡是走 ToolAPI 的调用都能绕过 AllowedOutputs，不只是序列。

// ① 收窄授权时，ToolAPI 路径必须**同样**被拦。
//
// 这是本阶段的核心断言：同一份 allowedOutputs，两条路径判定必须一致。
func TestToolAPIPathRespectsDeviceGrant(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"device/ok-1"}
	registerFakeDevice(t, a, "devicectl", nil)

	tc := agentAPI.ToolCall{
		ID: "c1", Name: "device_ctl_cmdrun",
		Arguments: map[string]interface{}{"device_id": "other-2", "command": "rm -rf /"},
	}

	// ① 内核路径（现状已有）
	gotInner := a.executeToolCall(tc, "cli")
	if !strings.Contains(gotInner, "未授权") {
		t.Fatalf("内核路径应拒绝未授权设备，实际: %s", gotInner)
	}

	// ② ToolAPI 路径（此前无此判定 ⇒ 缺口）
	if toolAPIOf(t, a).CanUse(tc.Name, tc.Arguments) {
		t.Error("ToolAPI 路径对未授权设备返回了 true —— 授权可被绕过（缺口未堵）")
	}
}

// ② 反向：已授权的设备必须放行，否则正常能力被误杀。
func TestToolAPIPathAllowsGrantedDevice(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"device/ok-1"}
	registerFakeDevice(t, a, "devicectl", nil)

	tc := agentAPI.ToolCall{
		ID: "c1", Name: "device_ctl_cmdrun",
		Arguments: map[string]interface{}{"device_id": "ok-1", "command": "ls"},
	}
	if !toolAPIOf(t, a).CanUse(tc.Name, tc.Arguments) {
		t.Error("已授权设备被误拒 —— 授权闸过严会把正常能力杀掉")
	}
}

// ③ 非设备工具不受该闸影响（否则会把所有工具都锁死）。
func TestToolAPIPathIgnoresNonDeviceTools(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"device/ok-1"} // 收窄到只给一台设备

	for _, name := range []string{"cmd_run", "knowledge_search", "memory_recall", "output_list_channels"} {
		if !toolAPIOf(t, a).CanUse(name, map[string]interface{}{}) {
			t.Errorf("非设备工具 %q 被设备授权闸拦了 —— 闸的作用域过宽", name)
		}
	}
}

// ④ 枚举类工具（无 device_id）不受闸——与内核现有测试
// TestDeviceToolAuth_EnumerationNotGated 保持同一语义。
func TestToolAPIPathAllowsEnumerationTools(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"device/ok-1"}
	registerFakeDevice(t, a, "devicectl", nil)

	if !toolAPIOf(t, a).CanUse("devicedetect", map[string]interface{}{}) {
		t.Error("枚举类工具不应被设备授权闸拦")
	}
}

// ⑤ 未配置白名单（根 agent 默认）= 完整授权。
func TestToolAPIPathFullGrantByDefault(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	registerFakeDevice(t, a, "devicectl", nil)
	if !toolAPIOf(t, a).CanUse("device_ctl_cmdrun", map[string]interface{}{"device_id": "any-1"}) {
		t.Error("未配置白名单时应为完整授权")
	}
}

// ⑥ 两条路径的判定必须**一致** —— 这是本设计的核心不变式。
func TestCanUseAgreesWithInnerPath(t *testing.T) {
	cases := []struct {
		deviceID string
		allowed  []string
	}{
		{"ok-1", []string{"device/ok-1"}},
		{"other-2", []string{"device/ok-1"}},
		{"ok-1", nil}, // 完整授权
		{"other-2", nil},
	}
	for _, c := range cases {
		a := newPreemptAgent(t, newPreemptProvider())
		a.allowedOutputs = c.allowed
		registerFakeDevice(t, a, "devicectl", nil)

		tc := agentAPI.ToolCall{
			ID: "c1", Name: "device_ctl_cmdrun",
			Arguments: map[string]interface{}{"device_id": c.deviceID, "command": "ls"},
		}
		innerOK := !strings.Contains(a.executeToolCall(tc, "cli"), "未授权")
		apiOK := toolAPIOf(t, a).CanUse(tc.Name, tc.Arguments)
		if innerOK != apiOK {
			t.Errorf("device=%s allowed=%v：内核路径=%v 而 ToolAPI 路径=%v —— 两条路径判定不一致",
				c.deviceID, c.allowed, innerOK, apiOK)
		}
	}
}

// ⑦ 设备工具但 device_id 缺失：内核现状是 **fail-open**。
// 本判据把现状钉住，避免无意中改变既有行为（内核有测试
// TestDeviceToolAuth_* 依赖它）；若将来要改成 fail-closed，
// 必须同时改内核与此处，并更新两边判据。
func TestCanUseMatchesInnerFailOpenOnMissingDeviceID(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"device/ok-1"}
	registerFakeDevice(t, a, "devicectl", nil)

	tc := agentAPI.ToolCall{
		ID: "c1", Name: "device_ctl_cmdrun",
		Arguments: map[string]interface{}{"command": "ls"}, // 无 device_id
	}
	innerOK := !strings.Contains(a.executeToolCall(tc, "cli"), "未授权")
	apiOK := toolAPIOf(t, a).CanUse(tc.Name, tc.Arguments)
	if innerOK != apiOK {
		t.Errorf("缺 device_id 时两条路径不一致：内核=%v ToolAPI=%v", innerOK, apiOK)
	}
	if innerOK {
		t.Log("现状：缺 device_id 时放行（fail-open）。已钉住，若要改须两边同时改。")
	}
}
