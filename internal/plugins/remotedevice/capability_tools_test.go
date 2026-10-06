package remotedevice

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 三个结构化能力工具（screensue / camerasue / speakeruse）—— 2026-10-06
//
// # 为什么要从 device_ctl_cmdrun 里拆出来
//
// 生产日志实证（10-06 12:46~13:04，agent 连续 9 次试参数）：
//
//	13:00:38  stream tool_call device_ctl_cmdrun argument fragments invalid JSON:
//	          "{\"device_id\": \"gui-JianF\", \"command\": \"homeagent-screensue 0
//	           <!DOCTYPE html>…<meta charset=\\\"utf-8\\\">…
//	13:00:38  args unparseable (982 bytes) — surfacing to model instead of calling
//
// 一屏 HTML 内联进 command 字符串后：① 引号要穿过两层 JSON 转义；
// ② 时长与内容共用一个字符串靠空格切分；③ 长度无上限。
// 三者叠加 ⇒ 工具调用变成非法 JSON，**连执行都没执行**。
//
// agent 的应对是反复试参数：
//
//	screensue shown on display 0 for 15s / 25s / 30s / 40s / 60s / 180s
//
// —— 它在猜格式，不是在做事。
//
// # 这些判据钉住什么
//
// ① 三个工具有结构化参数（content / duration_seconds 分开）
// ② 命令字符串由服务端拼装，形状与 GUI 端 executeHomeagentCmd 的解析一致
// ③ 长内容走 content_path，不内联
// ④ 超时/不在线/缺能力都给出可操作的错误（不是一句 "failed"）

// ---- 测试脚手架 ----

// newCapabilityDevice 起一台「已在线 + 全能力」的假设备，并返回收到的命令。
//
// 用 caps=["cmd"]：deviceSupportsTool 把 cmd 视为历史全能力标记，
// 于是三个工具的能力门都会放行 —— 判据才能测到工具本身而不是被门挡住。
func newCapabilityDevice(t *testing.T, deviceID string, reply func(cmd string) map[string]interface{}) (*devicectlDevice, *[]string) {
	t.Helper()
	reg := NewRegistry()
	token := "tk-cap-" + deviceID
	reg.SetAcceptToken(func(p string) bool { return p == token })
	reg.SetMediaDir(t.TempDir())

	dev := &devicectlDevice{reg: reg}
	got := &[]string{}

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	t.Cleanup(srv.Close)

	cli := dialTestWS(t, srv.URL, token)
	t.Cleanup(cli.close)

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"` + deviceID +
		`","name":"能力机","kind":"computer","caps":["cmd"]}}`))
	cli.readHelloAckAndBind(t, token)

	go func() {
		for {
			op, payload, err := cli.readMsg()
			if err != nil {
				return
			}
			if op != 0x1 {
				continue
			}
			var msg map[string]interface{}
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			if msg["op"] != "cmd" {
				continue
			}
			cmd, _ := msg["command"].(string)
			reqID, _ := msg["req_id"].(string)
			*got = append(*got, cmd)
			out := reply(cmd)
			if out == nil {
				out = map[string]interface{}{"status": "ok", "output": "done"}
			}
			out["op"] = "cmd_result"
			out["req_id"] = reqID
			out["device_id"] = deviceID
			cli.sendText(mustJSON(out))
		}
	}()

	// 等设备进入 registry 的在线表（bind 之后才可见）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m, ok := reg.Get(deviceID); ok && m.Online {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return dev, got
}

// ---- ① screensue：内容与时长是**独立参数** ----

func TestScreensue_StructuredArgsBecomeOneCommand(t *testing.T) {
	dev, got := newCapabilityDevice(t, "cap-sue", func(string) map[string]interface{} {
		return map[string]interface{}{"status": "ok", "output": "screensue shown on display 0 for 30s"}
	})

	res, err := dev.Execute("screensue", map[string]interface{}{
		"device_id":        "cap-sue",
		"content":          "三点开会",
		"duration_seconds": 30,
	})
	if err != nil {
		t.Fatalf("screensue 应成功: %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["device_id"] != "cap-sue" {
		t.Errorf("返回应带 device_id，实际 %v", m["device_id"])
	}
	if len(*got) != 1 {
		t.Fatalf("应下发恰好 1 条命令，实际 %d: %v", len(*got), *got)
	}
	// 协议形状：<capability> <秒数> <内容>（GUI executeHomeagentCmd 按首个纯数字 token 取时长）
	if w := "screensue 30 三点开会"; (*got)[0] != w {
		t.Errorf("命令形状错：\n got=%q\nwant=%q", (*got)[0], w)
	}
}

// TestScreensue_ContentWithQuotesAndSpacesSurvives 是这次修复的**核心判据**。
//
// ★ 判据的是「HTML 含引号与空格时还能不能正确送达」——生产上正是它导致
//
//	JSON 参数非法（982 字节解析失败）。结构化参数后内容不经 JSON 转义层，
//	本判据保证服务端拼装不破坏它。
func TestScreensue_ContentWithQuotesAndSpacesSurvives(t *testing.T) {
	dev, got := newCapabilityDevice(t, "cap-quote", func(string) map[string]interface{} {
		return map[string]interface{}{"status": "ok", "output": "ok"}
	})
	html := `<meta charset="utf-8"><div style="color:red">A B   C</div>`
	if _, err := dev.Execute("screensue", map[string]interface{}{
		"device_id": "cap-quote", "content": html,
	}); err != nil {
		t.Fatalf("含引号内容应成功: %v", err)
	}
	// 无时长 ⇒ 不加秒数前缀（设备端默认 5 秒）
	want := "screensue " + html
	if (*got)[0] != want {
		t.Errorf("含引号内容被破坏：\n got=%q\nwant=%q", (*got)[0], want)
	}
}

func TestScreensue_DurationZeroMeansPersistent(t *testing.T) {
	dev, got := newCapabilityDevice(t, "cap-zero", func(string) map[string]interface{} {
		return map[string]interface{}{"status": "ok", "output": "persistent"}
	})
	// duration_seconds=0 ⇒ 必须**显式传 0**，不能省略
	// （省略 = 设备端默认 5 秒，0 = 常驻 —— 两者语义不同）
	if _, err := dev.Execute("screensue", map[string]interface{}{
		"device_id": "cap-zero", "content": "公告", "duration_seconds": 0,
	}); err != nil {
		t.Fatalf("常驻显示应成功: %v", err)
	}
	if (*got)[0] != "screensue 0 公告" {
		t.Errorf("0 必须显式下发（区别于省略=默认5秒），实际 %q", (*got)[0])
	}
}

// ---- ② 长内容走文件，不内联 ----

func TestScreensue_ContentPathReadsFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "board.html")
	big := "<h1>看板</h1>" + strings.Repeat("<p>x</p>", 500)
	if err := os.WriteFile(fp, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	dev, got := newCapabilityDevice(t, "cap-path", func(string) map[string]interface{} {
		return map[string]interface{}{"status": "ok", "output": "ok"}
	})

	if _, err := dev.Execute("screensue", map[string]interface{}{
		"device_id": "cap-path", "content_path": fp,
	}); err != nil {
		t.Fatalf("content_path 应成功: %v", err)
	}
	// 内容来自文件，逐字节一致
	if !strings.Contains((*got)[0], big) {
		t.Errorf("命令应含文件全文（%d 字节），实际长度 %d", len(big), len((*got)[0]))
	}
}

func TestDeviceContentArg_RejectsBothOrNeither(t *testing.T) {
	// 两个都给 ⇒ 报错（歧义比默认取一个更危险）
	if _, err := deviceContentArg("x", "/tmp/y", "content"); err == nil {
		t.Error("content 与 content_path 同时给必须报错")
	}
	// 都不给 ⇒ 报错，且说明该怎么给
	_, err := deviceContentArg("", "", "content")
	if err == nil {
		t.Fatal("都不给必须报错")
	}
	if !strings.Contains(err.Error(), "content_path") {
		t.Errorf("错误应提示可用 content_path，实际: %v", err)
	}
	// 文件不存在 ⇒ 带路径
	if _, err := deviceContentArg("", "/nope/x.html", "content"); err == nil ||
		!strings.Contains(err.Error(), "/nope/x.html") {
		t.Errorf("文件读取失败应带路径，实际: %v", err)
	}
	// 空文件 ⇒ 与「不存在」区分
	dir := t.TempDir()
	empty := filepath.Join(dir, "e.html")
	os.WriteFile(empty, nil, 0o644)
	if _, err := deviceContentArg("", empty, "content"); err == nil ||
		!strings.Contains(err.Error(), "空") {
		t.Errorf("空文件应明说，实际: %v", err)
	}
	// 超限 ⇒ 说明大小与上限，并提示改用文件路径
	big := filepath.Join(dir, "big.html")
	os.WriteFile(big, []byte(strings.Repeat("x", deviceContentMaxBytes+10)), 0o644)
	_, err = deviceContentArg("", big, "content")
	if err == nil || !strings.Contains(err.Error(), "过大") {
		t.Errorf("超限应报错并说明，实际: %v", err)
	}
}

// ---- ③ camerasue：拍照/录像由 duration_seconds 区分 ----

func TestCamerasue_PhotoVsVideo(t *testing.T) {
	dev, got := newCapabilityDevice(t, "cap-cam", func(cmd string) map[string]interface{} {
		if cmd == "camerasue" {
			return map[string]interface{}{"status": "ok", "output": "photo ok", "mime": "image/jpeg", "size": 123}
		}
		return map[string]interface{}{"status": "ok", "output": "video ok", "mime": "video/mp4", "size": 456}
	})

	// 拍照：不带时长
	res, err := dev.Execute("camerasue", map[string]interface{}{"device_id": "cap-cam"})
	if err != nil {
		t.Fatalf("拍照应成功: %v", err)
	}
	if (*got)[0] != "camerasue" {
		t.Errorf("拍照命令应为裸 camerasue，实际 %q", (*got)[0])
	}
	if m, _ := res.(map[string]interface{}); m["mode"] != "photo" {
		t.Errorf("mode 应为 photo，实际 %v", m["mode"])
	}
	// ★ mime/size 必须透传：录像不能只回一句「成功」。
	//   size 经 JSON 回来是 float64（不是 int）—— 这是边界事实，
	//   断言要按 JSON 的实际类型比，否则判据自己会假红。
	if m, _ := res.(map[string]interface{}); m["mime"] != "image/jpeg" || toIntArg(m["size"]) != 123 {
		t.Errorf("mime/size 应透传，实际 mime=%v size=%v", m["mime"], m["size"])
	}

	// 录像：带时长
	res, err = dev.Execute("camerasue", map[string]interface{}{
		"device_id": "cap-cam", "duration_seconds": 5,
	})
	if err != nil {
		t.Fatalf("录像应成功: %v", err)
	}
	if (*got)[1] != "camerasue 5" {
		t.Errorf("录像命令应为 camerasue 5，实际 %q", (*got)[1])
	}
	if m, _ := res.(map[string]interface{}); m["mode"] != "video" {
		t.Errorf("mode 应为 video，实际 %v", m["mode"])
	}
}

func TestCamerasue_RejectsNegativeAndOverlong(t *testing.T) {
	dev, got := newCapabilityDevice(t, "cap-cam2", func(string) map[string]interface{} {
		return map[string]interface{}{"status": "ok", "output": "ok"}
	})
	if _, err := dev.Execute("camerasue", map[string]interface{}{
		"device_id": "cap-cam2", "duration_seconds": -1,
	}); err == nil {
		t.Error("负时长必须报错")
	}
	if _, err := dev.Execute("camerasue", map[string]interface{}{
		"device_id": "cap-cam2", "duration_seconds": 9999,
	}); err == nil || !strings.Contains(err.Error(), "300") {
		t.Errorf("超长录像应报错并说明上限，实际: %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("被拒的请求不应下发命令，实际下发 %v", *got)
	}
}

// ---- ④ speakeruse ----

func TestSpeakeruse_StructuredArg(t *testing.T) {
	dev, got := newCapabilityDevice(t, "cap-spk", func(string) map[string]interface{} {
		return map[string]interface{}{"status": "ok", "output": "spoken"}
	})
	if _, err := dev.Execute("speakeruse", map[string]interface{}{
		"device_id": "cap-spk", "text": "三点开会，请准时",
	}); err != nil {
		t.Fatalf("speakeruse 应成功: %v", err)
	}
	if (*got)[0] != "speakeruse 三点开会，请准时" {
		t.Errorf("命令形状错：%q", (*got)[0])
	}
	// text_path 变体
	dir := t.TempDir()
	fp := filepath.Join(dir, "say.txt")
	os.WriteFile(fp, []byte("来自文件的问候"), 0o644)
	if _, err := dev.Execute("speakeruse", map[string]interface{}{
		"device_id": "cap-spk", "text_path": fp,
	}); err != nil {
		t.Fatalf("text_path 应成功: %v", err)
	}
	if (*got)[1] != "speakeruse 来自文件的问候" {
		t.Errorf("text_path 内容未送达：%q", (*got)[1])
	}
}

// ---- ⑤ 数字参数的字符串形态（模型常给 "30" 而不是 30）----

func TestToIntArg_AcceptsStringNumbers(t *testing.T) {
	// 类型断言静默失败退回默认值，正是「参数丢了但不报错」——
	// 模型给字符串 "30" 是常见形态，必须认。
	for _, v := range []interface{}{30, int64(30), float64(30), "30", " 30 "} {
		if got := toIntArg(v); got != 30 {
			t.Errorf("toIntArg(%#v)=%d，期望 30", v, got)
		}
	}
	if got := toIntArg("abc"); got != 0 {
		t.Errorf("非数字应返回 0，实际 %d", got)
	}
}

// ---- ⑥ 错误必须可操作：不在线 / 缺能力 ----

func TestCapabilityTools_OfflineErrorSaysRetry(t *testing.T) {
	reg := NewRegistry()
	dev := &devicectlDevice{reg: reg}
	// 设备根本不存在
	_, err := dev.Execute("screensue", map[string]interface{}{
		"device_id": "ghost", "content": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "devicedetect") {
		t.Errorf("设备不存在应指向 devicedetect，实际: %v", err)
	}
}

func TestScreensee_TimeoutErrorDistinguishesNotYetReplied(t *testing.T) {
	// 设备在线但永不回执 ⇒ 错误必须说清「命令已下发但没回」，
	// 而不是让 agent 以为设备拒绝执行（实测它会换参数重试）。
	dev, _ := newCapabilityDevice(t, "cap-slow", func(string) map[string]interface{} {
		time.Sleep(200 * time.Millisecond)
		return map[string]interface{}{"status": "ok", "output": "late"}
	})
	// 用一个必然超时的短超时直接测 pushHomeagentCapability
	_, reqID, err := dev.pushHomeagentCapability("cap-slow", "speakeruse hi", 50*time.Millisecond)
	if err == nil {
		t.Fatal("超时必须报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "回执") || !strings.Contains(msg, reqID) {
		t.Errorf("超时错误应说明「未回执」并带上 req_id=%s 便于复查，实际: %v", reqID, err)
	}
	if !strings.Contains(msg, "device_ctl_cmdresult") {
		t.Errorf("超时错误应指向复查工具，实际: %v", err)
	}
}
