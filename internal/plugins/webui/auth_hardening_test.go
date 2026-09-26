package webui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	"gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// ===== 登录入口的滥用防护 =====
//
// 真实风险：webui 门户经 frp 穿透到公网（https://homeagent.jianfgit.xyz/
// 实测直达），而 handleLogin 在加固前**零防护**：无速率限制、无失败计数、
// 口令用 == 明文比对、失败不审计。配合历史上出现过的弱口令习惯
// （日志里能看到「为截图登 WebUI 临时改密码」这类操作），等于把一个
// 可爆破的口子直接开到外网。
//
// 这组判据钉住四件事：有限流、限流不误伤、可退避、比对不泄漏信息。

const testAuthPassword = "correct-horse-battery"

func newAuthTestHandler(t *testing.T) *Handler {
	t.Helper()
	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)
	cfgReg.PluginConfig("webui").Set("password", testAuthPassword)
	h := NewHandler(testSDK(sdk.SDKConfig{Settings: sdk.NewSettings("webui", cfgReg)}))
	// 走生产真实入口 Handler() = proxyDispatch(logged(mux))。
	// 直接用 h.mux 会绕过 logged 中间件，测不到限流的实际生效位置。
	h.RegisterRoutes(http.NewServeMux())
	return h
}

func postLoginFrom(t *testing.T, h *Handler, ip, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"username":"` + user + `","password":"` + pass + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(body))
	req.RemoteAddr = ip + ":54321"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec
}

func tryLoginFrom(h *Handler, ip, user, pass string) int {
	body := `{"username":"` + user + `","password":"` + pass + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(body))
	req.RemoteAddr = ip + ":54321"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func tryLogin(h *Handler, user, pass string) int {
	return tryLoginFrom(h, "203.0.113.1", user, pass)
}

func postLoginRaw(t *testing.T, h *Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", bytes.NewReader(body))
	req.RemoteAddr = "203.0.113.1:54321"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec
}

// 连续失败必须被拦：放行到 30 次都不该一次 429 都没有。
func TestLoginRateLimitsAfterRepeatedFailures(t *testing.T) {
	h := newAuthTestHandler(t)

	ok, blocked := 0, 0
	for i := 0; i < 30; i++ {
		switch code := tryLogin(h, "admin", "wrong-password"); code {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			blocked++
		}
	}
	if ok > 0 {
		t.Errorf("错误口令居然登录成功了 %d 次", ok)
	}
	if blocked == 0 {
		t.Error("连续 30 次错误口令从未触发限流（一次 429 都没有）—— 门户可被暴力破解")
	}
}

// 限流必须按来源区分：否则一个 IP 的狂刷就能把所有人（含管理员）一起锁死，
// 限流本身就变成了 DoS 手段。
func TestLoginRateLimitIsPerSource(t *testing.T) {
	h := newAuthTestHandler(t)

	for i := 0; i < 30; i++ {
		tryLoginFrom(h, "203.0.113.9", "admin", "bad")
	}
	if code := tryLoginFrom(h, "203.0.113.9", "admin", "bad"); code != http.StatusTooManyRequests {
		t.Errorf("攻击者来源应被限流，实际 %d", code)
	}
	if code := tryLoginFrom(h, "198.51.100.7", "admin", testAuthPassword); code != http.StatusOK {
		t.Errorf("其他来源的正常登录被误伤（跨来源污染），实际 %d", code)
	}
}

// 成功必须清零：不能因为早先手滑输错几次就再也登不进。
//
// 判据强度说明：不能只「跑 30 次看有没有限流」—— 阈值只有 5，
// 跑 30 次时**无论有没有 Reset 都会限流**，那样的判据是假的
// （已实测：去掉 Reset 后本条依然绿）。必须**测出实际阈值**：
// 清零后应当重新拿到完整的窗口额度。
func TestLoginSuccessResetsCounter(t *testing.T) {
	h := newAuthTestHandler(t)

	// 先用 3 次失败「污染」计数（低于阈值，此时仍能登录）
	for i := 0; i < 3; i++ {
		tryLogin(h, "admin", "bad")
	}
	if code := tryLogin(h, "admin", testAuthPassword); code != http.StatusOK {
		t.Fatalf("少量失败后应仍能正常登录，实际 %d", code)
	}

	// 成功之后，额度必须**重新算满**：连错 loginMaxFails 次才该被拦。
	for i := 1; i <= loginMaxFails; i++ {
		if code := tryLogin(h, "admin", "bad"); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败期望 401，实际 %d —— 成功登录未清零计数（额度被提前扣掉了）",
				i, code)
		}
	}
	if code := tryLogin(h, "admin", "bad"); code != http.StatusTooManyRequests {
		t.Errorf("第 %d 次失败后应被限流，实际 %d", loginMaxFails+1, code)
	}
}

// 429 必须带 Retry-After，否则客户端/脚本无从判断何时该重试。
func TestLoginRateLimitedCarriesRetryAfter(t *testing.T) {
	h := newAuthTestHandler(t)

	for i := 0; i < 30; i++ {
		tryLogin(h, "admin", "bad")
	}
	rec := postLoginFrom(t, h, "203.0.113.1", "admin", "bad")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("期望 429，实际 %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 响应缺少 Retry-After 头")
	}
}

// 请求体必须限量：不限流的话一个请求就能把内存吃光。
//
// 判据要点：**不能只看状态码**。8MB 垃圾 JSON 会让解码器直接失败并返回
// 400，与「被限流拒绝」撞码 —— 那样这条判据是假的（改与不改都绿）。
// 所以断言大请求体在解码前就被挡下，即 413。
func TestLoginBodySizeLimited(t *testing.T) {
	h := newAuthTestHandler(t)

	body := make([]byte, 8<<20)
	for i := range body {
		body[i] = 'a'
	}
	rec := postLoginRaw(t, h, body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求体应返回 413，实际 %d（body=%.120s）—— "+
			"若为 400 说明只是解码失败而非体积限制，判据无效", rec.Code, rec.Body.String())
	}
	if code := tryLogin(h, "admin", testAuthPassword); code != http.StatusOK {
		t.Errorf("正常登录被体积限制误伤，实际 %d", code)
	}
}

// 失败原因不得可区分：不同失败给不同状态码或报文 = 可枚举用户名。
func TestLoginNoUsernameEnumeration(t *testing.T) {
	h := newAuthTestHandler(t)

	badUser := postLoginFrom(t, h, "203.0.113.1", "no-such-user-xyz", "whatever")
	badPass := postLoginFrom(t, h, "198.51.100.7", "admin", "wrong")

	if badUser.Code != badPass.Code {
		t.Errorf("不同失败原因返回不同状态码（%d vs %d），可用于枚举用户名",
			badUser.Code, badPass.Code)
	}
	if badUser.Body.String() != badPass.Body.String() {
		t.Errorf("不同失败原因返回不同响应体，可用于枚举用户名:\n  用户不存在: %s\n  口令错误  : %s",
			badUser.Body.String(), badPass.Body.String())
	}
}

// 限流必须是**有界**的：过期记录要被清掉，否则攻击者轮换来源 IP
// 就能把 map 喂成内存泄漏。
func TestLoginLimiterPrunesExpiredKeys(t *testing.T) {
	l := newLoginLimiter(loginMaxFails, loginWindow)

	for i := 0; i < 500; i++ {
		l.Fail("src-" + strconv.Itoa(i))
	}
	if n := l.liveKeys(); n != 500 {
		t.Errorf("记录数 = %d，期望 500", n)
	}
	// 推进到窗口之后并触发清理
	l.clockAdvance(loginWindow + time.Minute)
	l.prune()
	if n := l.liveKeys(); n != 0 {
		t.Errorf("过期后仍残留 %d 条记录 —— 轮换 IP 即可无限增长（内存泄漏）", n)
	}
}

// 计时器替代方案：验证 Allow 返回的重试时长不为 0。
// 返回 0 会让客户端立即重试 —— 那等于没有限流。
func TestLoginLimiterRetryNeverZero(t *testing.T) {
	l := newLoginLimiter(2, time.Hour)
	l.Fail("k")
	l.Fail("k")
	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("达到阈值后应被限流")
	}
	if retry <= 0 {
		t.Errorf("Retry-After 时长 = %v，必须为正（返回 0 会让客户端立即重试）", retry)
	}
}
