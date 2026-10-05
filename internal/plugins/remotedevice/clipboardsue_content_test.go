package remotedevice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clipboardsue 的 content 参数：从**服务端可见的路径**读入剪切板。
//
// ## 为什么要它（2026-10-05）
//
// 长文本/代码块塞不进 `clipboardsue <文字>` 的命令行；让模型先生成一大段
// 文本再原样传给设备，既烧 token 又容易在转述中失真。
// 与 GUI 侧 screensue / speakeruse 的 `@<路径>` 是同一类问题的同一类解法。
//
// ## 这些判据钉住的行为
//
//  ① text 与 content 都空 ⇒ 明确报错（不能静默写空剪切板）
//  ② content 指向不存在的文件 ⇒ 报错且**带上路径**（否则模型无从修正）
//  ③ content 超上限 ⇒ 报错（防止把会话撑爆）
//  ④ 设备不可用时优先报设备错，而不是先读文件
//     ——顺序很重要：不然一个离线设备会伪装成「文件不存在」
//
// 注：本文件只验**参数校验与错误路径**。真正读文件并下发需要一台在线设备，
// 那由 binary_test.go 的 WS 级联调覆盖。

// newOfflineDevice 造一个 device，其 clipboardsue 必然在 clipboardCheck 阶段失败。
func newOfflineDevice(t *testing.T) *devicectlDevice {
	t.Helper()
	return &devicectlDevice{reg: NewRegistry()}
}

func TestClipboardsue_EmptyTextAndContentRejected(t *testing.T) {
	d := newOfflineDevice(t)
	_, err := d.clipboardsue(map[string]interface{}{"device_id": "x"})
	if err == nil {
		t.Fatal("text 与 content 都为空时应当报错（不能静默写空剪切板）")
	}
	if !strings.Contains(err.Error(), "text") && !strings.Contains(err.Error(), "content") {
		t.Errorf("错误信息应指明缺哪个参数，实际: %v", err)
	}
	// 只有空白也视为空
	if _, err2 := d.clipboardsue(map[string]interface{}{
		"device_id": "x", "text": "   ", "content": "  ",
	}); err2 == nil {
		t.Error("空白 text/content 也应报错")
	}
}

func TestClipboardsue_DeviceCheckedBeforeReadingFile(t *testing.T) {
	// 设备不存在 ⇒ 必须先报「设备不存在」，
	// 而不是先读文件报「文件不存在」——否则离线设备会伪装成文件问题。
	d := newOfflineDevice(t)
	_, err := d.clipboardsue(map[string]interface{}{
		"device_id": "no-such-device",
		"content":   filepath.Join(t.TempDir(), "nope.txt"),
	})
	if err == nil {
		t.Fatal("设备不存在时应当报错")
	}
	if !strings.Contains(err.Error(), "no-such-device") {
		t.Errorf("应先报设备问题（设备 %s 不存在），实际: %v", "no-such-device", err)
	}
}

func TestClipboardsueMaxBytesIsTwoMB(t *testing.T) {
	// 上限与 GUI 侧 CAPABILITY_SOURCE_MAX_BYTES 对齐（2MB）。
	// 改这个常量时这条判据会红——提醒同步改 GUI。
	if clipboardsueMaxBytes != 2*1024*1024 {
		t.Errorf("clipboardsue 上限应为 2MB（与 GUI 侧一致），实际 %d", clipboardsueMaxBytes)
	}
}

func TestClipboardsue_ContentFileReadable(t *testing.T) {
	// 造一个真文件，确认 os.ReadFile 能读到（判据本身的自检：
	// 若将来换成别的读法或路径处理变了，这里会先炸）。
	dir := t.TempDir()
	p := filepath.Join(dir, "note.txt")
	want := "行1\n行2\n"
	if err := os.WriteFile(p, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(b) != want {
		t.Errorf("读回内容不符: %q", string(b))
	}
}

// TestClipboardsue_ContentOversizeRejected 钉住**上限真的被用上**。
//
// ★ 这条判据是补来的：我先前只写了「常量等于 2MB」那条，
//
//	把 `if len(b) > clipboardsueMaxBytes` 改成 `if false` 它照样绿——
//	**测的是常量、不是闸**。凡是要验「闸真的关着」，
//	必须造一个越过阈值的数据去撞它，而不是断言阈值的数值。
func TestClipboardsue_ContentOversizeRejected(t *testing.T) {
	// 造一个在线且已授权的设备，让 clipboardCheck 过；
	// 随后超限文件必须在**读文件后的校验**处被拦下。
	dir := t.TempDir()
	p := filepath.Join(dir, "big.txt")
	big := make([]byte, clipboardsueMaxBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(p, big, 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	// 注册一个在线设备并授 capability：clipboardsue。
	reg.devices["d1"] = &DeviceMeta{
		DeviceID: "d1", Name: "d", Kind: "computer", Online: true,
		Caps: []string{"clipboardsue"},
	}
	d := &devicectlDevice{reg: reg}

	_, err := d.clipboardsue(map[string]interface{}{
		"device_id": "d1",
		"content":   p,
	})
	if err == nil {
		t.Fatal("超 2MB 的 content 应当被拒绝（把上限改成 if false 时这条会红）")
	}
	if !strings.Contains(err.Error(), "过大") && !strings.Contains(err.Error(), "上限") {
		t.Errorf("错误信息应说明超限，实际: %v", err)
	}
}
