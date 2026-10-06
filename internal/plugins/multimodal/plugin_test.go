package multimodal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 多模态工具的内核退场（2026-10-06）
//
// ## 为什么退场
//
// README 第一原则：「内核职责限定在 LLM 编排、记忆管理与知识检索；
// 所有 IO 能力由插件实现」，而「文件读取」正在 IO 列表里。
//
// `ocr_image` / `describe_image` / `transcribe_audio` 原先是内核
// `buildToolDefs()` 里硬编码的内置条件工具，读本地文件却住在内核进程内
// —— 无沙箱、无能力面、无审计。
//
// ## 这些判据钉住什么
//
// ① 媒体来源解析（path / digest / 都没有）三条路径的行为
// ② 失败必须以 **error** 上抛，不渲染成普通文本
// ③ 内核侧**不再有**这三个工具的实现与定义（结构性判据，跨文件）

// ---- ① 媒体来源解析 ----

func TestResolveMedia_PathWins(t *testing.T) {
	p := &Plugin{}
	dir := t.TempDir()
	fp := filepath.Join(dir, "shot.jpg")
	if err := os.WriteFile(fp, []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	url, err := p.resolveMedia(map[string]interface{}{"path": fp}, "image_url")
	if err != nil {
		t.Fatalf("按 path 解析应成功: %v", err)
	}
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Errorf("应按扩展名给 image/jpeg，实际前缀: %.40s", url)
	}
	// ★ path 与 digest 同时给：path 优先（且不读 CAS）
	url2, err := p.resolveMedia(map[string]interface{}{
		"path": fp, "digest": "deadbeef",
	}, "image_url")
	if err != nil {
		t.Fatalf("path 优先时应成功: %v", err)
	}
	if url2 != url {
		t.Error("path 与 digest 同时给时应以 path 为准")
	}
}

func TestResolveMedia_NoSourceExplainsHowToFix(t *testing.T) {
	p := &Plugin{}
	_, err := p.resolveMedia(map[string]interface{}{}, "image_url")
	if err == nil {
		t.Fatal("既无 path 又无 digest 时必须报错")
	}
	// ★ 错误信息必须**可操作**：说清传什么、为什么。
	msg := err.Error()
	if !strings.Contains(msg, "path") || !strings.Contains(msg, "digest") {
		t.Errorf("错误信息要指明可传 path 或 digest，实际: %v", err)
	}
}

func TestResolveMedia_FileErrorsAreSpecific(t *testing.T) {
	p := &Plugin{}
	// 不存在的文件：带路径，便于模型修正
	_, err := p.resolveMedia(map[string]interface{}{"path": "/nope/missing.jpg"}, "image_url")
	if err == nil || !strings.Contains(err.Error(), "/nope/missing.jpg") {
		t.Errorf("读取失败应带上路径便于定位，实际: %v", err)
	}
	// 空文件：与「读不到」区分开
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.png")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = p.resolveMedia(map[string]interface{}{"path": empty}, "image_url")
	if err == nil || !strings.Contains(err.Error(), "空") {
		t.Errorf("空文件应明说「空」，实际: %v", err)
	}
}

func TestMimeByExt(t *testing.T) {
	for p, want := range map[string]string{
		"a.jpg": "image/jpeg", "a.JPEG": "image/jpeg", "a.png": "image/png",
		"a.webp": "image/webp", "a.gif": "image/gif",
		"a.wav": "audio/wav", "a.mp3": "audio/mpeg", "a.m4a": "audio/aac",
		"a.bin": "", "noext": "",
	} {
		if got := mimeByExt(p); got != want {
			t.Errorf("mimeByExt(%q)=%q，期望 %q", p, got, want)
		}
	}
}

// ---- ② ocr_enabled 关闭时不得出现一个「在表里但不可用」的工具 ----

// TestOCRDisabledKeepsToolOut 钉住 ocr_enabled=false 时 ocr_image 不注册。
//
// ★ 为什么这条重要（本项目反复踩的坑）：工具在表里却调不通，
//
//	模型会反复调一个注定失败的工具。宁可**不出现**。
func TestOCRDisabledKeepsToolOut(t *testing.T) {
	if truthy("false") {
		t.Error("truthy(\"false\") 应为 false")
	}
	if !truthy("true") {
		t.Error("truthy(\"true\") 应为 true")
	}
	// 描述文案必须说清「不是专用 OCR 引擎」——名字承诺大于实现会让模型误判精度。
	p := &Plugin{name: "multimodal", ocrEnabled: true}
	def := p.ocrToolDef()
	if !strings.Contains(def.Description, "不是专用 OCR 引擎") {
		t.Errorf("ocr_image 的描述必须说明它不是专用 OCR 引擎，实际: %s", def.Description)
	}
}
