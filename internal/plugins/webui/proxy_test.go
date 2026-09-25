package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// proxyTestSettings 造一份带 api_key 的 webui 插件设置，供受保护路由用
// X-API-Key 通过鉴权（真实部署里浏览器走门户会话，脚本走 key）。
func proxyTestSettings(t *testing.T) *sdk.PluginSDK {
	t.Helper()
	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)
	return testSDK(sdk.SDKConfig{Settings: sdk.NewSettings("webui", cfgReg)})
}

// authHeaders 给受保护路由的测试请求带上凭证。
func authHeaders(r *http.Request) {
	r.Header.Set("X-API-Key", "test-api-key")
}

// ---- 子域标签抽取 ----

func TestProxyHostLabel(t *testing.T) {
	cases := []struct {
		host, base, want string
	}{
		// 默认基座：*.localhost（零配置可用）
		{"huawei.localhost:8080", "localhost", "huawei"},
		{"huawei.localhost", "localhost", "huawei"},
		{"HUAWEI.LOCALHOST:8080", "localhost", "huawei"}, // 大小写不敏感
		// 基域名自身不是插件路由
		{"localhost:8080", "localhost", ""},
		{"localhost", "localhost", ""},
		// 自定义基域名
		{"devices.webui.example.com", "webui.example.com", "devices"},
		{"webui.example.com", "webui.example.com", ""},
		// 多级子域不接（避免"哪个是插件"含混）
		{"a.b.webui.example.com", "webui.example.com", ""},
		// 非本基域名的 Host 不接
		{"evil.com", "webui.example.com", ""},
		{"notlocalhost.com", "localhost", ""},
		// 边界/异常输入
		{"", "localhost", ""},
		{"   ", "localhost", ""},
		{".localhost:8080", "localhost", ""},
		{"..localhost", "localhost", ""},
		// 后缀但标签为空
		{".webui.example.com", "webui.example.com", ""},
	}
	for _, c := range cases {
		if got := proxyHostLabel(c.host, c.base); got != c.want {
			t.Errorf("proxyHostLabel(%q, %q) = %q，期望 %q", c.host, c.base, got, c.want)
		}
	}
}

// ---- 手填条目解析 ----

func TestParseManualRoutes(t *testing.T) {
	text := `
# 注释行
grafana 127.0.0.1:3000
devices 127.0.0.1:9890 ws auth=none

prom http://127.0.0.1:9090
`
	got := parseManualRoutes(text)
	if len(got) != 3 {
		t.Fatalf("解析出 %d 条，期望 3：%+v", len(got), got)
	}
	if got[0].Host != "grafana" || got[0].Target != "127.0.0.1:3000" || got[0].WebSocket {
		t.Errorf("第 1 条不对: %+v", got[0])
	}
	if got[1].Host != "devices" || !got[1].WebSocket || got[1].Auth != "none" {
		t.Errorf("第 2 条没解析出 ws/auth=none: %+v", got[1])
	}
	if got[2].Target != "http://127.0.0.1:9090" {
		t.Errorf("第 3 条带 scheme 的地址被破坏: %+v", got[2])
	}
	// 空白与注释行不得产出条目
	if len(parseManualRoutes("\n\n#x\n")) != 0 {
		t.Error("空行/注释行不应产出条目")
	}
	// 大写标签归一化为小写
	if r := parseManualRoutes("GRAFANA 127.0.0.1:3000"); len(r) != 1 || r[0].Host != "grafana" {
		t.Errorf("标签未归一化为小写: %+v", r)
	}
}

// ---- 路由表构建：冲突不得静默覆盖 ----

func TestBuildProxyTableConflictNotSilentlyOverridden(t *testing.T) {
	decls := []proxyDecl{
		{Plugin: "a", Name: "ui", Host: "dup", Target: "127.0.0.1:1001"},
		{Plugin: "b", Name: "ui", Host: "dup", Target: "127.0.0.1:1002"},
	}
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl { return decls })
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	tbl := currentProxyTable()
	// 先到者占住标签
	r, ok := tbl.routes["dup"]
	if !ok {
		t.Fatal("先声明的条目应占住标签")
	}
	if r.Plugin != "a" {
		t.Errorf("标签被后者覆盖了：当前属于 %s，期望 a（后者应被判冲突）", r.Plugin)
	}
	// 后者必须仍然可见（带错误），而不是静默消失
	var loser *ProxyRoute
	for _, x := range tbl.ordered {
		if x.Plugin == "b" {
			loser = x
		}
	}
	if loser == nil {
		t.Fatal("冲突的后者从表里消失了——用户将无法察觉两个插件撞了标签")
	}
	if loser.Err == "" {
		t.Error("冲突条目应带错误说明")
	}
}

// ---- 非法声明不得进路由，但必须可见 ----

func TestBuildProxyTableKeepsInvalidVisible(t *testing.T) {
	decls := []proxyDecl{
		{Plugin: "bad", Name: "x", Host: "bad_host", Target: "127.0.0.1:1"}, // host 非法
		{Plugin: "good", Name: "y", Host: "good", Target: "127.0.0.1:2"},
	}
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl { return decls })
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	tbl := currentProxyTable()
	if _, ok := tbl.routes["bad_host"]; ok {
		t.Error("非法声明不应参与路由")
	}
	if _, ok := tbl.routes["good"]; !ok {
		t.Error("合法声明应参与路由")
	}
	var bad *ProxyRoute
	for _, x := range tbl.ordered {
		if x.Plugin == "bad" {
			bad = x
		}
	}
	if bad == nil || bad.Err == "" {
		t.Error("非法声明必须留在表里并带原因（配置页要能看见）")
	}
}

// ---- Host 分发端到端：这是「通用反代」的核心行为 ----

func TestServeProxyHostRoutesByHost(t *testing.T) {
	var gotPath, gotXFF, gotProto, gotCookie string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotProto = r.Header.Get("X-Forwarded-Proto")
		gotCookie = r.Header.Get("Cookie")
		w.WriteHeader(200)
		w.Write([]byte("from-upstream"))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{Plugin: "demo", Name: "ui", Host: "demo", Target: up.Listener.Addr().String()}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.Host = "demo.localhost:8080"
	authHeaders(r)
	r.Header.Set("Cookie", "homeagent_session=SECRET")
	if !h.serveProxyHost(rec, r) {
		t.Fatal("插件子域的请求应被反代处理")
	}
	if rec.Code != 200 || rec.Body.String() != "from-upstream" {
		t.Fatalf("反代未透传：code=%d body=%q", rec.Code, rec.Body.String())
	}
	// ★ 插件前端的根绝对路径必须原样到达上游（这正是选 Host 路由的理由）
	if gotPath != "/api/status" {
		t.Errorf("上游收到的路径 = %q，期望 /api/status（根路径必须原样保留）", gotPath)
	}
	if gotXFF == "" {
		t.Error("未注入 X-Forwarded-For（旧实现缺失项）")
	}
	if gotProto == "" {
		t.Error("未注入 X-Forwarded-Proto（旧实现缺失项）")
	}
	// 门户 cookie 不得泄漏给上游
	if strings.Contains(gotCookie, "SECRET") {
		t.Errorf("门户会话 cookie 被泄漏给上游: %q", gotCookie)
	}
}

// 未声明的子域必须明确报错，而不是静默落到主站页面（那会让用户以为地址对了）
func TestServeProxyHostUnknownLabel(t *testing.T) {
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	h := NewHandler(nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "nosuchplugin.localhost:8080"
	if !h.serveProxyHost(rec, r) {
		t.Fatal("插件子域（即使未声明）应由反代层应答")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未声明子域应返回 404，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "nosuchplugin") {
		t.Error("错误信息应含具体标签，便于排错")
	}
}

// 主站 Host 不得被反代层截走
func TestServeProxyHostMainSiteUntouched(t *testing.T) {
	h := NewHandler(nil)
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "localhost:8080"
	if h.serveProxyHost(httptest.NewRecorder(), r) {
		t.Error("主站 Host 不应被反代层处理")
	}
}

// ---- WebSocket 必须显式声明 ----

func TestProxyWebSocketRequiresDeclaration(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{
			{Plugin: "nws", Name: "ui", Host: "nws", Target: "127.0.0.1:1", WebSocket: false},
			{Plugin: "wsx", Name: "gw", Host: "wsx", Target: "127.0.0.1:1", WebSocket: true},
		}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(nil)
	// 未声明 ws：升级请求必须被明确拒绝（400），而不是当普通请求透传
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ws", nil)
	r.Host = "nws.localhost:8080"
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	h.serveProxyHost(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未声明 websocket 的升级请求应 400，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "websocket") {
		t.Error("拒绝原因应说明缺 websocket 声明")
	}
}

// ---- 认证可声明：none 直通，homeagent 拦截 ----

func TestProxyAuthDeclaration(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{
			{Plugin: "open", Name: "ui", Host: "open", Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthNone},
			{Plugin: "prot", Name: "ui", Host: "prot", Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthHomeAgent},
			{Plugin: "dflt", Name: "ui", Host: "dflt", Target: up.Listener.Addr().String()}, // 空 = 受保护
		}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(nil)

	// auth=none：匿名也必须通（设备链路的前提）
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "open.localhost:8080"
	h.serveProxyHost(rec, r)
	if rec.Code != 200 {
		t.Errorf("auth=none 应放行匿名请求，实际 %d", rec.Code)
	}

	// 显式 homeagent 与默认（空）：无凭证必须 401
	for _, host := range []string{"prot.localhost:8080", "dflt.localhost:8080"} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		h.serveProxyHost(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 无凭证应 401，实际 %d", host, rec.Code)
		}
		var body map[string]string
		json.Unmarshal(rec.Body.Bytes(), &body)
		if body["hint"] == "" {
			t.Errorf("%s 的 401 应给出可操作提示", host)
		}
	}
}

// ---- 上游 3xx 必须原样透传（不得由反代跟随）----

func TestProxyDoesNotFollowUpstreamRedirect(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/go" {
			// 指向内网敏感地址：旧实现（http.DefaultClient）会跟过去并把
			// 内网 URL 泄给客户端 / 或回 502
			http.Redirect(w, r, "http://127.0.0.1:1/internal-secret", http.StatusFound)
			return
		}
		w.Write([]byte("leaked"))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{Plugin: "demo", Name: "ui", Host: "demo", Target: up.Listener.Addr().String()}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/go", nil)
	r.Host = "demo.localhost:8080"
	authHeaders(r)
	h.serveProxyHost(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("上游 302 应原样透传，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if hits != 1 {
		t.Errorf("反代跟随了重定向（上游被打了 %d 次，应为 1）", hits)
	}
	if strings.Contains(rec.Body.String(), "leaked") {
		t.Error("反代跟随重定向后把内网响应体返回给了客户端")
	}
}

// ---- 流式响应必须逐帧下发（不得缓冲到上游关闭）----
//
// 判据设计说明（这里踩过一次坑，记下来）：
// httputil.ReverseProxy 对 **text/event-stream** 与 **ContentLength = -1**
// （chunked）的响应会自动立即 flush，与 FlushInterval 无关。所以只测这两种，
// 判据是**假的**——把 FlushInterval 改成 0（关掉）也照样绿，变异验证抓不到。
// 真正依赖 FlushInterval 的是「**已知 Content-Length** 但分片慢速下发」的响应
// （长轮询、带进度的大文件）。本判据专门构造这种响应。
func TestProxyStreamsIncrementallyWithKnownLength(t *testing.T) {
	const total = 32
	release := make(chan struct{})
	started := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 显式 Content-Length：让 ReverseProxy 不走"自动立即 flush"分支，
		// 从而真正检验 FlushInterval 设置。
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(200)
		w.Write([]byte("first-chunk-here"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		<-release // 首片发出后阻塞：若反代缓冲，客户端读不到第一片
		w.Write([]byte(strings.Repeat("x", total-len("first-chunk-here"))))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{Plugin: "demo", Name: "ui", Host: "demo", Target: up.Listener.Addr().String()}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(&proxyTestHandler{h: NewHandler(proxyTestSettings(t))}).ServeHTTP(w, r)
	}))
	defer front.Close()

	req, _ := http.NewRequest("GET", front.URL+"/stream", nil)
	req.Host = "demo.localhost:8080"
	req.Header.Set("X-API-Key", "test-api-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	<-started
	buf := make([]byte, 64)
	got := make(chan int, 1)
	go func() {
		n, _ := resp.Body.Read(buf)
		got <- n
	}()
	select {
	case n := <-got:
		if n <= 0 {
			t.Fatalf("读第一片失败: n=%d", n)
		}
		if !strings.Contains(string(buf[:n]), "first-chunk-here") {
			t.Fatalf("第一片内容异常: %q", string(buf[:n]))
		}
	case <-timeAfter(2):
		close(release)
		t.Fatal("反代缓冲了响应：上游首片已 flush 且 Content-Length 已知，客户端却读不到（FlushInterval 未设为立即）")
	}
	close(release)
}

// proxyTestHandler 只暴露反代 Host 分发，避免测试依赖 requireWeb 的登录态。
type proxyTestHandler struct{ h *Handler }

func (p *proxyTestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.h.serveProxyHost(w, r) {
		return
	}
	http.NotFound(w, r)
}

// ---- 服务入口清单（给前端渲染选项卡）----

func TestListProxyServicesIncludesURLAndErrors(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{
			{Plugin: "huawei_smarthome", Name: "ui", Host: "huawei-smarthome", Target: "127.0.0.1:12100"},
			{Plugin: "broken", Name: "x", Host: "bad host", Target: "127.0.0.1:1"},
		}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	svcs := h.listProxyServices("http", ":8080")
	if len(svcs) != 2 {
		t.Fatalf("入口数 = %d，期望 2（含坏条目）", len(svcs))
	}
	var good, bad *proxyServiceEntry
	for i := range svcs {
		if svcs[i].Plugin == "huawei_smarthome" {
			good = &svcs[i]
		}
		if svcs[i].Plugin == "broken" {
			bad = &svcs[i]
		}
	}
	if good == nil || !good.OK {
		t.Fatalf("合法声明应 OK: %+v", good)
	}
	// 入口链接必须可直接点击：带 scheme、子域标签、同一端口
	want := "http://huawei-smarthome.localhost:8080"
	if good.URL != want {
		t.Errorf("服务入口 URL = %q，期望 %q", good.URL, want)
	}
	if bad == nil || bad.OK || bad.Error == "" {
		t.Errorf("坏条目必须带错误且 OK=false: %+v", bad)
	}
	if bad.URL != "" {
		t.Error("坏条目不应给出可点链接")
	}
}

func TestHandleProxyServicesJSON(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{Plugin: "demo", Name: "ui", Host: "demo", Target: "127.0.0.1:1"}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/proxy/services", nil)
	r.Host = "localhost:8080"
	authHeaders(r)
	h.handleProxyServices(rec, r)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var out struct {
		Services   []proxyServiceEntry `json:"services"`
		BaseDomain string              `json:"base_domain"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.BaseDomain != "localhost" {
		t.Errorf("base_domain = %q，期望 localhost（零配置默认）", out.BaseDomain)
	}
	// 上游 127.0.0.1:1 不可达 → 必须如实标出不 OK，而不是假装可用
	if len(out.Services) != 1 || out.Services[0].OK {
		t.Errorf("不可达上游应标 OK=false: %+v", out.Services)
	}
}

// 服务入口链接在 https 反代场景下必须换成 https（否则远程点不开）
func TestProxySchemeAndPortRemote(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "portal.example.com"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "portal.example.com")
	scheme, port := h.proxySchemeAndPort(r)
	if scheme != "https" {
		t.Errorf("scheme = %q，期望 https", scheme)
	}
	if port != "" {
		t.Errorf("https 默认端口应为空（省略 443），实际 %q", port)
	}
	// 本机 http:8080
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Host = "localhost:8080"
	s2, p2 := h.proxySchemeAndPort(r2)
	if s2 != "http" || p2 != ":8080" {
		t.Errorf("本机应为 http/:8080，实际 %q/%q", s2, p2)
	}
}

// timeAfter 是 time.After 的薄封装（测试里多处用，集中一处便于调整）。
func timeAfter(seconds int) <-chan time.Time {
	return time.After(time.Duration(seconds) * time.Second)
}

// ---- 端到端：模拟真实插件 UI 的「根绝对路径」行为 ----
//
// 这是选 Host 路由而非路径前缀的**核心理由**，必须有判据钉住：
// 插件前端写 `fetch('/api/status')`，经 Host 反代后必须打到**上游**的
// /api/status，而不是 webui 自己的 /api/v1/*。若哪天改成路径前缀方案，
// 这条会红。
func TestProxyPreservesRootAbsolutePathsLikeRealPluginUI(t *testing.T) {
	var got []string
	// 伪造一个「插件 UI + API」上游：页面里含根绝对路径的 fetch，
	// /api/status 返回插件自己的数据（与门户 /api/v1/status 完全不同）。
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<script>fetch('/api/status').then(r=>r.json())</script>`))
		case "/api/status":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"plugin":"huawei","devices":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{Plugin: "huawei_smarthome", Name: "ui", Host: "huawei-smarthome", Target: up.Listener.Addr().String()}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))

	// 1) 打开插件首页
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "huawei-smarthome.localhost:8080"
	authHeaders(r)
	if !h.serveProxyHost(rec, r) {
		t.Fatal("应被反代处理")
	}
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "fetch('/api/status')") {
		t.Fatalf("插件首页未透传: code=%d", rec.Code)
	}

	// 2) 页面里的根绝对路径请求 → 必须打到上游 plugins 的 /api/status
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/status", nil)
	r2.Host = "huawei-smarthome.localhost:8080"
	authHeaders(r2)
	if !h.serveProxyHost(rec2, r2) {
		t.Fatal("插件子域下的 /api/status 应被反代处理")
	}
	var data map[string]interface{}
	if err := json.Unmarshal(rec2.Body.Bytes(), &data); err != nil {
		t.Fatalf("上游 JSON 未透传: %s", rec2.Body.String())
	}
	if data["plugin"] != "huawei" {
		t.Fatalf("根绝对路径被错路由了：拿到的不是插件数据而是 %v", data)
	}
	// 3) 路径必须原样到上游（前缀剥除在这里是错的）
	if len(got) == 0 || got[len(got)-1] != "/api/status" {
		t.Errorf("上游收到的路径 = %v，末项应为 /api/status", got)
	}
}

// ---- 自动发现：读插件目录里的 plugin.json ----
//
// readPluginProxyDecls 是「自动发现」的核心，必须单独钉住：它决定了用户
// 是否需要手填端口。
func TestReadPluginProxyDecls(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		d := filepath.Join(dir, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "plugin.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// ① 显式 host + ws + auth
	write("a", `{"name":"a","entry":"plugin.bin","proxies":[
	  {"name":"ui","host":"aaa","target":"127.0.0.1:12100"},
	  {"name":"gw","host":"aaa-gw","target":"127.0.0.1:9890","websocket":true,"auth":"none"}]}`)
	// ② 省略 host → 由插件名派生（下划线转连字符）
	write("b_plugin", `{"name":"b_plugin","entry":"plugin.bin","proxies":[{"target":"127.0.0.1:9999"}]}`)
	// ③ 没有 proxies 字段 → 不产出
	write("c", `{"name":"c","entry":"plugin.bin"}`)
	// ④ 非插件目录（无 plugin.json）→ 跳过，不得 panic
	if err := os.MkdirAll(filepath.Join(dir, "notaplugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// ⑤ 坏 JSON → 跳过该插件，不影响其它
	write("d", `{ this is not json`)

	decls := readPluginProxyDecls(dir)
	if len(decls) != 3 {
		t.Fatalf("发现 %d 条声明，期望 3：%+v", len(decls), decls)
	}
	byHost := map[string]proxyDecl{}
	for _, d := range decls {
		byHost[d.Host] = d
	}
	if d, ok := byHost["aaa"]; !ok || d.Target != "127.0.0.1:12100" || d.WebSocket {
		t.Errorf("条目 aaa 不对: %+v", d)
	}
	if d, ok := byHost["aaa-gw"]; !ok || !d.WebSocket || d.Auth != "none" {
		t.Errorf("条目 aaa-gw 未带上 ws/auth: %+v", d)
	}
	// 省略 host 的必须由插件名派生为合法 DNS label
	if d, ok := byHost["b-plugin"]; !ok {
		t.Errorf("省略 host 的声明未按插件名派生（期望 b-plugin）: %+v", byHost)
	} else if !sdk.ValidProxyHostLabel(d.Host) {
		t.Errorf("派生的 host 不合法: %q", d.Host)
	}
	// 空目录不得 panic、返回空
	if got := readPluginProxyDecls(""); len(got) != 0 {
		t.Error("空目录应返回空")
	}
	if got := readPluginProxyDecls(filepath.Join(dir, "does-not-exist")); len(got) != 0 {
		t.Error("不存在的目录应返回空而不是 panic")
	}
}

// 自动发现的声明必须真的能路由（打通「扫目录 → 建表 → 转发」整条链）
func TestAutoDiscoveryRoutesEndToEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("discovered-upstream"))
	}))
	defer up.Close()

	dir := t.TempDir()
	d := filepath.Join(dir, "huawei_smarthome")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "plugin.json"), []byte(`{
	  "name":"huawei_smarthome","entry":"plugin.bin",
	  "proxies":[{"name":"ui","target":"`+up.Listener.Addr().String()+`","auth":"none"}]}`), 0o644)

	// 用真实的自动发现回调（而不是注入假声明）——这才测到发现链路
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl { return readPluginProxyDecls(dir) })
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	// 标签由插件名派生：huawei_smarthome → huawei-smarthome
	r.Host = "huawei-smarthome.localhost:8080"
	if !h.serveProxyHost(rec, r) {
		t.Fatal("自动发现的声明应被反代处理")
	}
	if rec.Code != 200 || rec.Body.String() != "discovered-upstream" {
		t.Fatalf("自动发现的声明未生效: code=%d body=%q", rec.Code, rec.Body.String())
	}
}
