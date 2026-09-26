package pluginmgr

import (
	"net"
	"strings"
	"testing"
)

// TestTwoInstances_ListenIndependently 是「监听地址必须是实例状态」的**端到端**判据。
//
// 为什么不能只断言字段不共享：那是实现细节，且容易被「变异后仍通过」的弱测试骗过
// （我第一版就写了那样的测试，变异证明它没有牙）。真正的性质是：
//
//	两个实例能**同时**成功监听，且各自 HTTPURL 指向自己那个端口。
//
// 曾经的缺陷（包级可变全局 `var HTTPAddr` + Start() 反写它 + startHTTPServer 读它）
// 恰好会违背这一点：两个实例共用同一个地址 → 第二个 Listen 报
// `bind: address already in use`，实测在生产机上与 homed 抢 9876。
//
// 用 `127.0.0.1:0` 让 OS 分配端口，避免测试自身依赖任何固定端口。
func TestTwoInstances_ListenIndependently(t *testing.T) {
	a, b := New("a"), New("b")
	a.httpAddr, b.httpAddr = "127.0.0.1:0", "127.0.0.1:0"

	a.startHTTPServer()
	b.startHTTPServer()
	t.Cleanup(func() {
		_ = a.Stop()
		_ = b.Stop()
	})

	ua, ub := a.HTTPURL(), b.HTTPURL()
	if ua == "" || ub == "" {
		t.Fatalf("实例未成功监听：a=%q b=%q（包级全局会让第二个 bind 失败）", ua, ub)
	}
	if ua == ub {
		t.Fatalf("两个实例报出同一个地址 %q —— 监听地址被共享了，不是实例状态", ua)
	}

	// 两个地址都必须**真的可连**（不能只报一个字符串）
	for name, u := range map[string]string{"a": ua, "b": ub} {
		addr := strings.TrimPrefix(u, "http://")
		ln, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("实例 %s 报的地址 %s 不可连: %v", name, addr, err)
		}
		ln.Close()
	}
}
