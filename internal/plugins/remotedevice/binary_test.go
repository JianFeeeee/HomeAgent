package remotedevice

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ===== 测试用最小 WS 客户端（模拟 GUI 设备桥）=====

type testWSClient struct {
	conn net.Conn
	rw   *bufio.ReadWriter
}

func dialTestWS(t *testing.T, url, token string) *testWSClient {
	t.Helper()
	req := "GET /api/v1/device/ws?token=" + token + " HTTP/1.1\r\n" +
		"Host: test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := readLine(br)
	if err != nil {
		t.Fatalf("read upgrade resp: %v", err)
	}
	if !strings.Contains(resp, "101") {
		t.Fatalf("expected 101, got %s", resp)
	}
	for {
		line, err := readLine(br)
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if line == "\r\n" || line == "" {
			break
		}
	}
	return &testWSClient{conn: conn, rw: &bufio.ReadWriter{Reader: br, Writer: bufio.NewWriter(conn)}}
}

func readLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		b, err := br.ReadByte()
		if err != nil {
			return sb.String(), err
		}
		sb.WriteByte(b)
		if b == '\n' {
			return sb.String(), nil
		}
	}
}

// sendText 发送客户端文本帧（带掩码，RFC6455 要求客户端帧必须掩码）
func (c *testWSClient) sendText(payload []byte) {
	c.sendFrame(0x1, payload)
}

func (c *testWSClient) sendBinary(payload []byte) {
	c.sendFrame(0x2, payload)
}

func (c *testWSClient) sendFrame(opcode byte, payload []byte) {
	maskKey := make([]byte, 4)
	rand.Read(maskKey)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ maskKey[i%4]
	}
	var hdr []byte
	hdr = append(hdr, 0x80|opcode)
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 0x80|126)
		ext := make([]byte, 2)
		binary.BigEndian.PutUint16(ext, uint16(n))
		hdr = append(hdr, ext...)
	default:
		hdr = append(hdr, 0x80|127)
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(n))
		hdr = append(hdr, ext...)
	}
	c.rw.Write(hdr)
	c.rw.Write(maskKey)
	c.rw.Write(masked)
	c.rw.Flush()
}

// readMsg 读一帧（跳过 pong），返回 opcode 与 payload
func (c *testWSClient) readMsg() (byte, []byte, error) {
	for {
		payload, isClose, opcode, err := readFrame(c.rw.Reader)
		if err != nil || isClose {
			return 0, nil, err
		}
		if opcode == 0xa {
			continue
		}
		return opcode, payload, nil
	}
}

func (c *testWSClient) close() { c.conn.Close() }

// ===== 端到端：hello/bind/cmd + 二进制分块回传（录像协议）=====

func TestWSBinaryChunkUpload(t *testing.T) {
	reg := NewRegistry()
	token := "test-token-123"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()
	url := srv.URL

	cli := dialTestWS(t, url, token)
	defer cli.close()

	// hello 登记
	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"gui-test","name":"测试机","kind":"computer","caps":["cmd"]}}`))
	op, payload, err := cli.readMsg()
	if err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}
	if op != 0x1 {
		t.Fatalf("expected text frame, got %x", op)
	}
	var ack map[string]interface{}
	json.Unmarshal(payload, &ack)
	if ack["op"] != "hello_ack" {
		t.Fatalf("expected hello_ack, got %v", ack)
	}

	// 模拟设备收到 cmd 后以二进制分块回传（cmd_data_start → 0x2×N → cmd_data_end）
	videoData := make([]byte, 20000) // 跨多个 8KB 块
	for i := range videoData {
		videoData[i] = byte(i % 251)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		cli.sendText(mustJSON(map[string]interface{}{
			"op": "cmd_data_start", "req_id": "req-video-1",
			"kind": "camera_video", "mime": "video/mp4",
			"total": len(videoData), "chunk_size": 8192,
		}))
		const chunk = 8192
		for off := 0; off < len(videoData); off += chunk {
			end := off + chunk
			if end > len(videoData) {
				end = len(videoData)
			}
			cli.sendBinary(videoData[off:end])
		}
		cli.sendText(mustJSON(map[string]interface{}{
			"op": "cmd_data_end", "req_id": "req-video-1", "status": "ok", "total": len(videoData),
		}))
	}()

	// 服务端等待聚合结果（AwaitResult 由 deliverResult 唤醒）
	res, err := reg.AwaitResult("req-video-1", 5*time.Second)
	if err != nil {
		t.Fatalf("await aggregated result: %v", err)
	}
	if res["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", res["status"])
	}
	if got, _ := res["size"].(int); got != len(videoData) {
		t.Fatalf("size mismatch: got %v want %d", res["size"], len(videoData))
	}

	// 校验 base64 数据完整性
	stored, ok := reg.GetResult("req-video-1")
	if !ok {
		t.Fatal("result not persisted")
	}
	b64, _ := stored["data_base64"].(string)
	if len(b64) == 0 {
		t.Fatal("data_base64 empty")
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if string(decoded) != string(videoData) {
		t.Fatal("decoded data mismatch")
	}
}

// ===== 端到端：PushData 下发音频（网关→设备 cmd_speech 协议）=====

func TestWSPushDataAudio(t *testing.T) {
	reg := NewRegistry()
	token := "test-token-456"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"audio-dev","name":"音频机","kind":"speaker"}}`))
	if _, _, err := cli.readMsg(); err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}

	audioData := []byte("RIFF....fake-wav-audio-data-for-testing....")

	// 异步下发音频
	errCh := make(chan error, 1)
	go func() {
		errCh <- reg.PushData("audio-dev", "req-speech-1", "speech", "audio/wav", audioData)
	}()

	// 设备侧按协议读取：start 文本帧 → N 个二进制帧 → end 文本帧
	var start map[string]interface{}
	var chunks [][]byte
	var end map[string]interface{}
	deadline := time.After(5 * time.Second)
	for end == nil {
		select {
		case <-deadline:
			t.Fatal("timeout reading speech protocol frames")
		default:
		}
		op, payload, err := cli.readMsg()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch op {
		case 0x1:
			var msg map[string]interface{}
			json.Unmarshal(payload, &msg)
			switch msg["op"] {
			case "cmd_speech_start":
				start = msg
			case "cmd_speech_end":
				end = msg
			}
		case 0x2:
			chunks = append(chunks, payload)
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("PushData error: %v", err)
	}

	if start == nil || start["op"] != "cmd_speech_start" {
		t.Fatal("missing cmd_speech_start")
	}
	if start["mime"] != "audio/wav" || start["kind"] != "speech" {
		t.Fatalf("unexpected start fields: %v", start)
	}
	if int(start["total"].(float64)) != len(audioData) {
		t.Fatalf("total mismatch: %v", start["total"])
	}
	if end["req_id"] != "req-speech-1" {
		t.Fatalf("unexpected end: %v", end)
	}
	var got []byte
	for _, c := range chunks {
		got = append(got, c...)
	}
	if string(got) != string(audioData) {
		t.Fatalf("audio data mismatch: got %d bytes want %d", len(got), len(audioData))
	}
}

// ===== PushData 对离线设备报错 =====

func TestPushDataOfflineDevice(t *testing.T) {
	reg := NewRegistry()
	err := reg.PushData("no-such-device", "req-x", "speech", "audio/wav", []byte{1})
	if err == nil || !strings.Contains(err.Error(), "not online") {
		t.Fatalf("expected not online error, got %v", err)
	}
}

// ===== screensee：截屏回传 + 视觉描述回调 =====

func TestScreenseeEndToEnd(t *testing.T) {
	reg := NewRegistry()
	token := "test-token-see"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })

	dev := &devicectlDevice{reg: reg}
	var gotDataURL string
	dev.SetSeeHandler(func(dataURL string, provider string) string {
		gotDataURL = dataURL
		return "屏幕上显示的是测试画面"
	})

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	// 设备 hello + bind（bind 需 token 才能被授权流程识别，这里直接手动授权）
	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"see-dev","name":"屏幕机","kind":"computer","caps":["cmd"]}}`))
	if _, _, err := cli.readMsg(); err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}
	reg.SetAuthorized("see-dev", true)

	// 设备侧循环收命令并回执（模拟 GUI screensee 实现）
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
			if msg["op"] == "cmd" && msg["command"] == "screensee" {
				reqID, _ := msg["req_id"].(string)
				fakeJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10} // JPEG magic
				b64 := base64.StdEncoding.EncodeToString(fakeJPEG)
				cli.sendText(mustJSON(map[string]interface{}{
					"op": "cmd_result", "req_id": reqID, "device_id": "see-dev",
					"status": "ok", "output": "data:image/jpeg;base64," + b64,
				}))
			}
		}
	}()

	// agent 调用 screensee
	res, err := dev.Execute("screensee", map[string]interface{}{"device_id": "see-dev"})
	if err != nil {
		t.Fatalf("screensee: %v", err)
	}
	m := res.(map[string]interface{})
	if m["description"] != "屏幕上显示的是测试画面" {
		t.Fatalf("unexpected description: %v", m["description"])
	}
	if !strings.HasPrefix(gotDataURL, "data:image/jpeg;base64,") {
		t.Fatalf("handler received bad dataURL: %s", gotDataURL)
	}

	// 未授权设备应拒绝
	reg.SetAuthorized("see-dev", false)
	if _, err := dev.Execute("screensee", map[string]interface{}{"device_id": "see-dev"}); err == nil {
		t.Fatal("expected unauthorized error")
	}
}

// ===== computeruse：鼠标/键盘控制命令下发 =====

func TestComputeruseEndToEnd(t *testing.T) {
	reg := NewRegistry()
	token := "test-token-cu"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })

	dev := &devicectlDevice{reg: reg}

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"cu-dev","name":"操控机","kind":"computer","caps":["cmd","computeruse"]}}`))
	if _, _, err := cli.readMsg(); err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}
	reg.SetAuthorized("cu-dev", true)

	// 设备侧收 computeruse 命令并回执
	var receivedCmd string
	done := make(chan struct{})
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
			if msg["op"] == "cmd" && msg["cmd_type"] == "homeagent" {
				receivedCmd, _ = msg["command"].(string)
				reqID, _ := msg["req_id"].(string)
				if strings.HasPrefix(receivedCmd, "computeruse ") {
					cli.sendText(mustJSON(map[string]interface{}{
						"op": "cmd_result", "req_id": reqID, "device_id": "cu-dev",
						"status": "ok", "output": "clicked at (3009,450)",
					}))
					close(done)
					return
				}
			}
		}
	}()

	// agent 调用 computeruse（结构化参数）
	res, err := dev.Execute("computeruse", map[string]interface{}{
		"device_id": "cu-dev", "action": "click", "x": float64(3009), "y": float64(450),
	})
	if err != nil {
		t.Fatalf("computeruse: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("device did not receive command")
	}
	// 验证下发的命令是合法 JSON 参数格式
	payloadJSON := strings.TrimPrefix(receivedCmd, "computeruse ")
	var params map[string]interface{}
	if err := json.Unmarshal([]byte(payloadJSON), &params); err != nil {
		t.Fatalf("command payload not valid JSON: %v (%s)", err, payloadJSON)
	}
	if params["action"] != "click" || params["x"] != float64(3009) || params["y"] != float64(450) {
		t.Fatalf("unexpected params: %v", params)
	}
	if m := res.(map[string]interface{}); m["status"] != "ok" {
		t.Fatalf("expected ok result: %v", m)
	}

	// 缺坐标应报错
	if _, err := dev.Execute("computeruse", map[string]interface{}{"device_id": "cu-dev", "action": "click"}); err == nil {
		t.Fatal("click without x/y should error")
	}
	// 未知 action 应报错
	if _, err := dev.Execute("computeruse", map[string]interface{}{"device_id": "cu-dev", "action": "fly"}); err == nil {
		t.Fatal("unknown action should error")
	}
	// type 需要 text
	if _, err := dev.Execute("computeruse", map[string]interface{}{"device_id": "cu-dev", "action": "type"}); err == nil {
		t.Fatal("type without text should error")
	}
}
