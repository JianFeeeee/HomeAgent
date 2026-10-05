package remotedevice

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleDevicePushRequiresPayload 钉住「payload 缺失必须报错」。
//
// 背景（2026-10-05 实测）：本端点此前不校验 payload，于是
//
//	POST {"device_id":"gui-JianF","command":"homeagent-clipboardsee"}
//	  → command 字段被静默丢弃，payload 为 nil
//	  → 设备收到 {"payload":null}，op 为空，直接忽略
//	  → 服务端 PushJSON 写入 socket 成功，返回 200 {"status":"ok"}
//
// 一次「什么也没发生」的操作在报文上是彻底的成功：排查时只能靠设备侧
// 日志才发现命令没到，极易误判成链路/鉴权/心跳问题。
//
// 本测试不依赖真设备：只验校验分支，且明确断言「非法请求不得返回 ok」。
func TestHandleDevicePushRequiresPayload(t *testing.T) {
	p := &Plugin{}
	p.registry = NewRegistry()

	cases := []struct {
		name     string
		body     string
		wantCode int
		wantOK   bool   // 响应里是否出现 "ok"
		wantErr  string // 期望出现在 error 里的关键词（空=默认 "payload"）
	}{
		{
			name:     "缺 payload（把 command 写在顶层，是最常见的误解）",
			body:     `{"device_id":"gui-JianF","command":"homeagent-clipboardsee"}`,
			wantCode: http.StatusBadRequest,
			wantOK:   false,
		},
		{
			name:     "payload 为 null",
			body:     `{"device_id":"gui-JianF","payload":null}`,
			wantCode: http.StatusBadRequest,
			wantOK:   false,
		},
		{
			// device_id 先于 payload 校验（更基础的参数先报错），
			// 所以此例报的是 device_id 缺失——行为正确，只是错因不同。
			name:     "既缺 device_id 又缺 payload：先报 device_id",
			body:     `{"command":"homeagent-clipboardsee"}`,
			wantCode: http.StatusBadRequest,
			wantOK:   false,
			wantErr:  "device_id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/device/push", strings.NewReader(tc.body))
			p.handleDevicePush(w, r)

			if w.Code != tc.wantCode {
				t.Fatalf("状态码 = %d，期望 %d（body=%s）", w.Code, tc.wantCode, w.Body.String())
			}
			// 关键断言：绝不能报成功。上一版就是这里返回了 {"status":"ok"}。
			if tc.wantOK == false && strings.Contains(w.Body.String(), `"ok"`) {
				t.Fatalf("非法请求返回了成功：%s", w.Body.String())
			}
			var got map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("响应不是 JSON：%s", w.Body.String())
			}
			msg, _ := got["error"].(string)
			if msg == "" {
				t.Fatalf("400 响应里没有 error 字段：%s", w.Body.String())
			}
			// 报错文案要能自解释：使用者多半是把 command 写在了顶层。
			want := tc.wantErr
			if want == "" {
				want = "payload"
			}
			if !strings.Contains(msg, want) {
				t.Fatalf("报错未提及 %q，实际：%s", want, msg)
			}
		})
	}
}
