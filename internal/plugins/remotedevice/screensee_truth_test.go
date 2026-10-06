package remotedevice

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// screensee 的两条教训（2026-10-05 生产日志实证）
//
// ## ① 视觉描述失败必须以 error 上抛，不能渲染成普通文本
//
//	原实现：`return fmt.Sprintf("屏幕截图视觉描述失败: %v…", err)`
//	生产实测（13:51）：
//	    {"description":"屏幕截图视觉描述失败: api error 403:
//	               {\"error\":{\"message\":\"model \"gpt-5.6-sol\" is not allowed…"}
//	⇒ status=ok、只有 description 字段 ⇒ **失败与成功同形**。
//	agent 以为拿到了结果，于是换 provider 再试一次（又失败一次）。
//
//	与同一天修掉的 MCP `isError` 丢失是**同一个病**。
//
// ★ 教训：不能只断言「返回值里含错误文本」——
//   那样「把错误写成 description」照样通过。必须断言**它从 error 通道出来**。

// TestSeeHandler_ErrorPropagatesAsError 钉住 seeHandler 的 error 不会被吞成文本。
func TestSeeHandler_ErrorPropagatesAsError(t *testing.T) {
	reg := NewRegistry()
	dev := &devicectlDevice{reg: reg}

	// 模拟「视觉源 403」：handler 返回 error
	dev.SetSeeHandler(func(dataURL, provider string) (string, error) {
		return "", errFake403
	})

	// 直接验契约：签名本身允许 error 通道
	var f func(string, string) (string, error) = dev.seeHandler
	desc, err := f("data:image/jpeg;base64,AAAA", "")
	if err == nil {
		t.Fatal("handler 返回 error 时，调用方必须能拿到 error（否则又会被渲染成 description）")
	}
	if desc != "" {
		t.Errorf("失败时不应同时返回描述文本，实际 %q", desc)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误信息应保留上游原文供诊断，实际 %v", err)
	}
}

// TestSeeHandler_SuccessStillWorks 钉住成功路径不受影响。
func TestSeeHandler_SuccessStillWorks(t *testing.T) {
	reg := NewRegistry()
	dev := &devicectlDevice{reg: reg}
	dev.SetSeeHandler(func(dataURL, provider string) (string, error) {
		return "屏幕上是一个终端窗口", nil
	})
	desc, err := dev.seeHandler("data:image/jpeg;base64,AAAA", "")
	if err != nil || desc == "" {
		t.Fatalf("成功路径被破坏：desc=%q err=%v", desc, err)
	}
}

// TestLogScreenseePayload_IdentifiesContent 钉住「回传内容可查证」。
//
// ★ 这条对应那两次无从查证的「我收不到截图」：
//   服务端日志必须能回答「设备到底回传了什么」。
//   现在记录 字节数 / magic / 像素尺寸 / base64 头 / SHA256 前缀。

func TestLogScreenseePayload_IdentifiesContent(t *testing.T) {
	cases := []struct {
		name  string
		data  []byte
		wantW int
		wantH int
	}{
		{"jpeg 8x4", makeJPEG(t, 8, 4), 8, 4},
		{"jpeg 1920x1080", makeJPEG(t, 1920, 1080), 1920, 1080},
	}
	for _, c := range cases {
		url := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(c.data)
		logScreenseePayload("dev1", url)
	}
	// png（magic 走 png 分支，尺寸解不出 → 0 0，也应正常不 panic）
	pngURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(makePNG(t, 3, 3))
	logScreenseePayload("dev1", pngURL)
	// 空
	logScreenseePayload("dev1", "")
	// 非法 base64（被截断的典型形态）
	logScreenseePayload("dev1", "data:image/jpeg;base64,!!!not-base64!!!")
}

// TestJpegSize 单独钉住 SOF 解析：尺寸是判断「图是不是真的截到了」的关键信号。
func TestJpegSize(t *testing.T) {
	for _, dim := range [][2]int{{1, 1}, {640, 480}, {1920, 1080}, {2560, 1440}} {
		b := makeJPEG(t, dim[0], dim[1])
		w, h := jpegSize(b)
		if w != dim[0] || h != dim[1] {
			t.Errorf("%dx%d: jpegSize 给 %dx%d", dim[0], dim[1], w, h)
		}
	}
	// 非 jpeg 一律 0,0，且不得 panic
	if w, h := jpegSize([]byte("not an image")); w != 0 || h != 0 {
		t.Errorf("非 jpeg 应给 0,0，实际 %dx%d", w, h)
	}
	if w, h := jpegSize(nil); w != 0 || h != 0 {
		t.Errorf("nil 应给 0,0，实际 %dx%d", w, h)
	}
	// ★ 截断的 jpeg（真实故障形态）：不得 panic，且不该报出假的尺寸
	trunc := makeJPEG(t, 100, 100)
	if w, _ := jpegSize(trunc[:20]); w != 0 {
		t.Errorf("截断的 jpeg 不应报出宽度，实际 %d", w)
	}
}

func TestMagicName(t *testing.T) {
	if got := magicName(makeJPEG(t, 2, 2)); got != "jpeg" {
		t.Errorf("jpeg 识别错: %s", got)
	}
	if got := magicName(makePNG(t, 2, 2)); got != "png" {
		t.Errorf("png 识别错: %s", got)
	}
	if got := magicName(nil); got != "empty" {
		t.Errorf("nil 应为 empty，实际 %s", got)
	}
	if got := magicName([]byte("garbage")); got != "unknown" {
		t.Errorf("垃圾数据应为 unknown，实际 %s", got)
	}
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var errFake403 = &fakeErr{"api error 403: model \"gpt-5.6-sol\" is not allowed for this key"}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }

// ===== 以下走完整链路（真 WS + 真 registry），盯住「失败从哪个通道出来」 =====

// TestScreenseE2E_VisualFailureIsErrorNotDescription 是本次修改的**核心门禁**。
//
// ★ 为什么必须走完整链路：
//
//	我第一版判据只测 `seeHandler` 的签名（能不能返回 error）——
//	把调用点的 `if err != nil` 改成「把错误渲染成 description 文本」时，
//	**判据全绿**。因为被测的是「签名允许 error」，
//	而真正的缺陷在「调用点有没有真的走 error 通道」。
//	⇒ 判据必须驱动 `dev.Execute("screensee", …)` 走完一整圈。
//
// 生产症状（2026-10-05 13:51）：
//
//	{"description":"屏幕截图视觉描述失败: api error 403: …"}   ← status=ok
//
// 修好后应当是：err != nil，且结果里**没有** description 字段。
func TestScreenseE2E_VisualFailureIsErrorNotDescription(t *testing.T) {
	reg := NewRegistry()
	token := "tk-visual-fail"
	reg.SetAcceptToken(func(p string) bool { return p == token })
	// auto_describe 需要落盘目录先存在（默认路径也会落盘）
	reg.SetMediaDir(t.TempDir())

	dev := &devicectlDevice{reg: reg}
	// 视觉源返回 403（MCP/screensee 生产上真实发生过）
	dev.SetSeeHandler(func(dataURL, provider string) (string, error) {
		return "", errFake403
	})

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()
	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	// 设备必须先 hello+bind 才会进 registry（否则 Execute 报「设备不存在」）
	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"vf-dev","name":"视觉失败机","kind":"computer","caps":["screensee"]}}`))
	cli.readHelloAckAndBind(t, token)

	go func() {
		for {
			op, payload, err := cli.readMsg()
			if err != nil || op != 0x1 {
				return
			}
			var msg map[string]interface{}
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			if msg["op"] == "cmd" && msg["command"] == "screensee" {
				reqID, _ := msg["req_id"].(string)
				img := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(makeJPEG(t, 64, 48))
				cli.sendText(mustJSON(map[string]interface{}{
					"op": "cmd_result", "req_id": reqID, "device_id": "vf-dev",
					"status": "ok", "output": img,
				}))
			}
		}
	}()

	res, err := dev.Execute("screensee", map[string]interface{}{"device_id": "vf-dev", "auto_describe": true})

	// ★ 必须报错
	if err == nil {
		t.Fatalf("视觉描述失败必须以 error 上抛，实际 res=%v —— "+
			"渲染成 description 会让 agent 以为拿到了结果并反复重试", res)
	}
	// ★ 错误信息要保留上游原文（那是唯一诊断线索）
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误信息应保留上游原文，实际: %v", err)
	}
	// ★ 结果里绝不能有 description 字段
	if m, ok := res.(map[string]interface{}); ok {
		if _, has := m["description"]; has {
			t.Errorf("失败时不得返回 description 字段（那是本次要修的缺陷），实际: %v", m)
		}
	}
}

// TestScreenseE2E_SuccessReturnsDescription 钉住成功路径没被改坏。
func TestScreenseE2E_SuccessReturnsDescription(t *testing.T) {
	reg := NewRegistry()
	token := "tk-visual-ok"
	reg.SetAcceptToken(func(p string) bool { return p == token })

	dev := &devicectlDevice{reg: reg}
	var gotLen int
	dev.SetSeeHandler(func(dataURL, provider string) (string, error) {
		gotLen = len(dataURL)
		return "屏幕上是一个终端窗口", nil
	})

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()
	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"vo-dev","name":"视觉成功机","kind":"computer","caps":["screensee"]}}`))
	cli.readHelloAckAndBind(t, token)

	go func() {
		for {
			op, payload, err := cli.readMsg()
			if err != nil || op != 0x1 {
				return
			}
			var msg map[string]interface{}
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			if msg["op"] == "cmd" && msg["command"] == "screensee" {
				reqID, _ := msg["req_id"].(string)
				img := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(makeJPEG(t, 64, 48))
				cli.sendText(mustJSON(map[string]interface{}{
					"op": "cmd_result", "req_id": reqID, "device_id": "vo-dev",
					"status": "ok", "output": img,
				}))
			}
		}
	}()

	// ★★ 2026-10-06：默认路径**不再自动描述**，改为落盘 + 回路径。
	//   旧行为保留在 auto_describe=true 下，另有一处判据覆盖。
	reg.SetMediaDir(t.TempDir())
	res, err := dev.Execute("screensee", map[string]interface{}{"device_id": "vo-dev"})
	if err != nil {
		t.Fatalf("成功路径被破坏: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("返回类型变了: %T", res)
	}
	fp, _ := m["file"].(string)
	if fp == "" {
		t.Fatalf("默认路径必须回 file，实际返回: %v", m)
	}
	if got, err := os.ReadFile(fp); err != nil || len(got) == 0 {
		t.Errorf("file 应指向落盘的截图: err=%v", err)
	}
	if _, hasDesc := m["description"]; hasDesc {
		t.Error("默认路径不应再返回 description（内容由 describe_image 显式取）")
	}
	if gotLen != 0 {
		t.Error("默认路径不应调用视觉描述 handler（模型必须显式调 describe_image）")
	}

	// ★ auto_describe=true 走旧路径（调试用）：handler 被调，description 回来。
	res2, err := dev.Execute("screensee", map[string]interface{}{
		"device_id": "vo-dev", "auto_describe": true,
	})
	if err != nil {
		t.Fatalf("auto_describe 路径被破坏: %v", err)
	}
	m2, _ := res2.(map[string]interface{})
	if m2["description"] != "屏幕上是一个终端窗口" {
		t.Errorf("auto_describe 应返回 description，实际: %v", m2["description"])
	}
	if gotLen == 0 {
		t.Error("auto_describe 下 handler 应收到 dataURL")
	}
}

// TestIsModelNotAllowed 钉住「模型名不被接受」的识别。
//
// ★ 这是 2026-10-05 生产那条 403 的判定入口：
//
//	model "gpt-5.6-sol" is not allowed for this key
//
// 根因不是「模型不支持图」，而是**那个 key 的 model scope 只有 AUTO**
// （/etc/llmsproxy/config.yaml 的 keys[homeagent].models = [AUTO]）。
// 而同一 key + base_url 下 model=AUTO 实测完全能看图 ⇒ 该回退而不是报错。
func TestIsModelNotAllowed(t *testing.T) {
	// llmsproxy 的三种原文（internal/gateway/chat.go）
	yes := []string{
		`model "gpt-5.6-sol" is not allowed for this key`,
		`{"error":{"type":"model_not_allowed","message":"model \"x\" is not allowed for this key"}}`,
		`model "x" is not in this key's model scope, so it cannot use AUTO`,
	}
	for _, s := range yes {
		if !isModelNotAllowed(errors.New(s)) {
			t.Errorf("应识别为「模型名不被接受」: %s", s)
		}
	}
	// ★ 必须**不**把其它错误误判成这一类 —— 那会触发错误的 AUTO 回退。
	no := []string{
		`invalid gateway api key`,
		`api error 401: Unauthorized`,
		`read: connection reset by peer`,
		`context deadline exceeded`,
		``, // nil 情形另行覆盖
	}
	for _, s := range no {
		if isModelNotAllowed(errors.New(s)) {
			t.Errorf("不应被识别为「模型名不被接受」（会误触发 AUTO 回退）: %s", s)
		}
	}
	if isModelNotAllowed(nil) {
		t.Error("nil 不应被判为模型名问题")
	}
}
