package remotedevice

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// screensee 落盘化（2026-10-06）
//
// ## 为什么要落盘
//
// 原行为是把 96KB 截图的 base64 直接送去视觉模型：
//   · 约 13 万 token 的内联载荷
//   · 自动挑源不可控（生产实证：AUTO 路由到不支持视觉的源，模型说「没收到图」）
//   · agent 不知道图存不存在，无法决定该 describe 还是该 ocr
//
// 现在：截图落盘 → 回 {file,mime,size,sha256,width,height} →
//       agent 显式调 describe_image(path=…) / ocr_image(path=…)
//
// 这些判据钉住「真的落盘了」与「元信息可信」——后者是 agent 决定
// 下一步的唯一依据，写错了它就会去读一个不存在的文件。

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0x40, A: 0xff})
		}
	}
	var buf strings.Builder
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return []byte(buf.String())
}

// ---- 落盘：文件存在且字节逐字节一致 ----

func TestSaveInlineMedia_DataURLWritesExactBytes(t *testing.T) {
	reg := NewRegistry()
	reg.SetMediaDir(t.TempDir())

	raw := jpegBytes(t, 64, 48)
	meta, err := reg.SaveInlineMedia("shot", "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(raw), "")
	if err != nil {
		t.Fatalf("落盘应成功: %v", err)
	}
	fp, _ := meta["file"].(string)
	if fp == "" {
		t.Fatalf("meta 必须含 file，实际: %v", meta)
	}
	got, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("file 应可读: %v", err)
	}
	// ★ 逐字节比对：只查「文件存在」会放过内容被截断/转码的情况。
	if string(got) != string(raw) {
		t.Errorf("落盘字节与原图不一致: 写入 %d 字节, 原图 %d 字节", len(got), len(raw))
	}
	if meta["size"] != len(raw) {
		t.Errorf("meta.size=%v，实际 %d", meta["size"], len(raw))
	}
}

func TestSaveInlineMedia_BareBase64AlsoWorks(t *testing.T) {
	reg := NewRegistry()
	reg.SetMediaDir(t.TempDir())
	raw := jpegBytes(t, 32, 32)
	// 设备端有时只回裸 base64（无 data: 前缀）
	meta, err := reg.SaveInlineMedia("bare", base64.StdEncoding.EncodeToString(raw), "image/jpeg")
	if err != nil {
		t.Fatalf("裸 base64 应可落盘: %v", err)
	}
	got, _ := os.ReadFile(meta["file"].(string))
	if string(got) != string(raw) {
		t.Error("裸 base64 落盘字节不一致")
	}
	if meta["mime"] != "image/jpeg" {
		t.Errorf("mimeHint 未生效: %v", meta["mime"])
	}
}

// ---- 元信息可信：agent 全靠它决定下一步 ----

func TestSaveInlineMedia_MetaIsVerifiable(t *testing.T) {
	reg := NewRegistry()
	reg.SetMediaDir(t.TempDir())
	raw := jpegBytes(t, 128, 96)
	meta, err := reg.SaveInlineMedia("meta", base64.StdEncoding.EncodeToString(raw), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	// sha256 必须与文件内容对得上（不是随机的/占位的）
	got, _ := os.ReadFile(meta["file"].(string))
	sum := sha256Hex(got)
	if meta["sha256"] != sum {
		t.Errorf("sha256 与落盘内容不符: meta=%v 实际=%v", meta["sha256"], sum)
	}
	// 尺寸必须是真解出来的，不是 0 或缺省
	if meta["width"] != 128 || meta["height"] != 96 {
		t.Errorf("尺寸应解出 128x96，实际 %vx%v", meta["width"], meta["height"])
	}
}

// TestSaveInlineMedia_ContentAddressedName 钉住文件名含内容摘要。
//
// ★ 为什么要摘要后缀：截屏/录像会被反复抓取。用 reqID 命名会把同一张图
//
//	存成多份；用固定名则不同内容互相覆盖。摘要两个问题都避免。
func TestSaveInlineMedia_ContentAddressedName(t *testing.T) {
	reg := NewRegistry()
	reg.SetMediaDir(t.TempDir())
	raw := jpegBytes(t, 16, 16)

	m1, err := reg.SaveInlineMedia("req-1", base64.StdEncoding.EncodeToString(raw), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	// 同内容不同 name ⇒ 摘要部分相同（内容寻址的语义）
	m2, err := reg.SaveInlineMedia("req-2", base64.StdEncoding.EncodeToString(raw), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(m1["file"].(string)) == filepath.Base(m2["file"].(string)) {
		t.Error("不同 name 的同内容落盘，文件名不应完全相同（name 前缀不同）")
	}
	if !strings.HasPrefix(filepath.Base(m1["file"].(string)), "req-1_") {
		t.Errorf("文件名应保留 name 前缀，实际 %s", filepath.Base(m1["file"].(string)))
	}
	if !strings.HasSuffix(m1["file"].(string), ".jpg") {
		t.Errorf("应按 mime 推出 .jpg 后缀，实际 %s", m1["file"])
	}
}

// ---- 失败必须报错，不得静默降级 ----

func TestSaveInlineMedia_FailuresAreExplicit(t *testing.T) {
	reg := NewRegistry()

	// 未配置落盘目录
	if _, err := reg.SaveInlineMedia("x", "data:image/jpeg;base64,AAAA", ""); err == nil {
		t.Error("未配置 mediaDir 时必须报错（静默降级会让 agent 拿不到路径也无从排查）")
	}

	reg.SetMediaDir(t.TempDir())
	for name, payload := range map[string]string{
		"空载荷":      "",
		"只有空白":     "   ",
		"非法base64": "data:image/jpeg;base64,!!!not-base64!!!",
		"缺逗号":      "data:image/jpeg;base64",
		"解出空内容":    "data:image/jpeg;base64,",
	} {
		if _, err := reg.SaveInlineMedia(name, payload, ""); err == nil {
			t.Errorf("%s 应报错，实际返回成功", name)
		}
	}
}

// ---- 尺寸解析：不认识的格式省略字段，而不是填 0 ----

func TestImageDimensions(t *testing.T) {
	// JPEG
	if w, h, ok := imageDimensions(jpegBytes(t, 200, 150)); !ok || w != 200 || h != 150 {
		t.Errorf("JPEG 尺寸解析错: %dx%d ok=%v", w, h, ok)
	}
	// PNG（存一个真 PNG 头）
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 13,
		'I', 'H', 'D', 'R', 0, 0, 1, 44, // 300
		0, 0, 1, 0x90, // 400
		8, 6, 0, 0, 0}
	if w, h, ok := imageDimensions(png); !ok || w != 300 || h != 400 {
		t.Errorf("PNG 尺寸解析错: %dx%d ok=%v", w, h, ok)
	}
	// 认不出的格式 ⇒ ok=false，**不得**返回 0,0,true
	if _, _, ok := imageDimensions([]byte("not an image at all")); ok {
		t.Error("非图片应返回 ok=false")
	}
	if _, _, ok := imageDimensions(nil); ok {
		t.Error("空输入应返回 ok=false")
	}
}
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
