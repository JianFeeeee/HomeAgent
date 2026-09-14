package remotedevice

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 心跳回包必须**真的发出去**：pong 只有两个字节，且设备空闲时没有任何别的写
// 会顺带把 bufio 缓冲刷出去——`writePong` 一旦忘了 Flush，pong 就永远留在
// 服务端缓冲里。
//
// 这就是「device channel 不稳定」的真因（实测）：客户端每 30s 发一个 ping，
// 服务端算好了 pong 却没发；客户端的读循环设的是 2 倍 ping 间隔（默认 60s）
// 读超时，于是**每 60 秒准点断开一次**，重连后 outputch 被注销又注册，
// 模型侧看到的就是工具/通道凭空消失又出现。
//
// 本用例只发一个 ping，随后**什么都不发**：pong 必须在无后续流量的情况下到达。
func TestWSPingGetsPongWhileIdle(t *testing.T) {
	reg := NewRegistry()
	token := "test-token-ping"
	reg.SetAcceptToken(func(provided string) bool { return provided == token })

	srv := httptest.NewServer(http.HandlerFunc(reg.ServeWS))
	defer srv.Close()

	cli := dialTestWS(t, srv.URL, token)
	defer cli.close()

	// 先走完 hello + bind（服务端要先把设备登记进 conns，pong 才写得回来）。
	cli.sendText([]byte(`{"op":"hello","device":{"device_id":"ping-dev","name":"前端机","kind":"computer","caps":["cmd"]}}`))
	cli.readHelloAckAndBind(t, token)

	cli.sendFrame(0x9, nil) // ping

	if err := cli.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	payload, isClose, opcode, err := readFrame(cli.rw.Reader)
	if err != nil {
		t.Fatalf("2s 内没收到 pong（writePong 忘了 Flush？）: %v", err)
	}
	if isClose {
		t.Fatal("连接被关闭，而不是回了 pong")
	}
	if opcode != 0xa {
		t.Fatalf("期望 pong(0xa)，实际 opcode=%#x payload=%q", opcode, payload)
	}
	if len(payload) != 0 {
		t.Fatalf("pong 不该带负载，实际 %q", payload)
	}
}
