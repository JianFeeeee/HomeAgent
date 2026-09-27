package io

import (
	"errors"
	"testing"
)

// 回归：驻留子的 IOManager 向父兜底时，父的**执行失败**不得被吞成「工具不存在」。
//
// 原实现（channel.go:655 附近）：
//
//	if ret, err := parent.ExecuteTool(name, args); err == nil { return ret, nil }
//	// 父的 err 被丢弃 ⇒ 落到 return "tool X not found"
//
// 后果放大：设备离线、插件崩溃这类「本该 retry 的失败」被上报为「工具没了」，
// 于是 on_error 整组跳过 —— 与「插件真的没加载」无法区分。
func TestExecuteTool_DoesNotSwallowParentFailureAsNotFound(t *testing.T) {
	parent := NewIOManager()
	// 父持有一个会在执行时失败的设备：工具存在，但 Execute 报错。
	failing := &failingDevice{name: "dev", toolName: "boom_tool", err: errors.New("device offline")}
	if err := parent.RegisterDevice(failing); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	child := NewIOManager()
	child.SetParentIO(parent)

	// 工具不在 child 上 → 向父兜底；父执行失败必须**如实上抛**。
	_, err := child.ExecuteTool("boom_tool", map[string]interface{}{})
	if err == nil {
		t.Fatal("期望父的失败被上抛，实际 err=nil（被吞了）")
	}
	if IsToolNotFound(err) {
		t.Fatalf("父的执行失败被误报为『工具不存在』: %v", err)
	}
	if err.Error() != "device offline" {
		t.Errorf("应如实上抛父的错误文案，实际: %v", err)
	}
}

// 真正的「不存在」仍须保持可判别（子与父都没有）。
func TestExecuteTool_NotFoundStillTypeable(t *testing.T) {
	parent := NewIOManager()
	child := NewIOManager()
	child.SetParentIO(parent)

	_, err := child.ExecuteTool("no_such_tool", map[string]interface{}{})
	if !IsToolNotFound(err) {
		t.Fatalf("两级都没有时应为 ErrToolNotFound，实际: %v", err)
	}
}

// failingDevice 是一个 Execute 恒定报错的测试设备。
type failingDevice struct {
	name     string
	toolName string
	err      error
}

func (d *failingDevice) Name() string        { return d.name }
func (d *failingDevice) Type() DeviceType    { return DeviceOutput }
func (d *failingDevice) Description() string { return "failing test device" }
func (d *failingDevice) Tools() []ToolDef    { return []ToolDef{{Name: d.toolName}} }
func (d *failingDevice) Execute(string, map[string]interface{}) (interface{}, error) {
	return nil, d.err
}
func (d *failingDevice) Start() error                         { return nil }
func (d *failingDevice) Stop() error                          { return nil }
func (d *failingDevice) OutputCapabilities() OutputCapability { return CapText }
func (d *failingDevice) ChannelDef() ChannelDef               { return ChannelDef{} }
