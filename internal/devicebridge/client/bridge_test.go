package client

import "testing"

// bind_ack 失败必须被识别（原实现完全不看 ok，失败静默）。
//
// 服务端 bind 被拒时回 {"op":"bind_ack","ok":false,"error":"bind rejected"}
// 并关闭连接。客户端若不看 ok，设备就静默失联：TCP/WS 是通的，
// 但设备从未登记进网关，命令永远下发不到 —— 只看"连接是否建立"的
// 健康检查会给出假阳性。
func TestBindFailureIsObservable(t *testing.T) {
	b := New("ws://127.0.0.1:1/api/v1/device/ws", "tok", "dev-1", "n", []string{"status"}, nil)

	if b.Bound() {
		t.Error("初始不应处于已绑定状态")
	}
	if b.BindError() != "" {
		t.Errorf("初始不应有绑定错误，实际 %q", b.BindError())
	}

	// 模拟收到 bind 失败
	b.markUnbound("bind rejected")
	if b.Bound() {
		t.Error("失败后不应报 Bound=true")
	}
	if b.BindError() != "bind rejected" {
		t.Errorf("失败原因应被保留，实际 %q", b.BindError())
	}

	// 成功后状态必须清空错误
	b.markBound()
	if !b.Bound() {
		t.Error("成功后应 Bound=true")
	}
	if b.BindError() != "" {
		t.Errorf("成功后应清空失败原因，实际 %q", b.BindError())
	}
}

// 绑定状态变更必须通知宿主（GUI/waiter 据此提示用户）。
func TestBindStateCallback(t *testing.T) {
	b := New("ws://127.0.0.1:1/api/v1/device/ws", "tok", "dev-2", "n", nil, nil)
	type ev struct {
		bound  bool
		reason string
	}
	var got []ev
	b.OnBoundState(func(bound bool, reason string) { got = append(got, ev{bound, reason}) })

	b.markUnbound("token mismatch")
	b.markBound()

	if len(got) != 2 {
		t.Fatalf("应收到 2 次回调，实际 %d", len(got))
	}
	if got[0].bound || got[0].reason != "token mismatch" {
		t.Errorf("第一次回调应为失败: %+v", got[0])
	}
	if !got[1].bound || got[1].reason != "" {
		t.Errorf("第二次回调应为成功: %+v", got[1])
	}
}
