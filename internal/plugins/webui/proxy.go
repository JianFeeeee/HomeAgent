package webui

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	pluginpkg "gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// 通用反向代理：插件声明自带 HTTP 服务（plugin.json 的 proxies），
// HomeAgent 按 **Host 子域标签** 把它们从 webui 的同一端口反代出去。
//
// ## 为什么是 Host 路由而不是路径前缀
//
// 插件自带 UI 普遍使用根绝对路径（实测 huawei_smarthome 的前端是
// `api('/api/status')` → `fetch('/api/status')`）。若挂在 `/p/huawei/` 下，
// 这些请求会打到 HomeAgent 自己的 `/api/status`，**静默错路由**。路径前缀方案
// 要么要求所有插件改前端，要么做 HTML/JS 内容重写——后者对拼进 JS 字符串的
// 绝对路径只是"按概率能用"，会产生「页面能开、某个按钮就坏」的静默故障。
//
// Host 路由下，插件前端的根路径天然正确，**插件零改动**。且它正好匹配
// 「只穿透一个端口」：webui 监听 0.0.0.0:8080，按 Host 分发；外层 frp/nginx
// 是单端口隧道，**不需要为每个插件加一条映射**。
//
// ## 默认基座：*.localhost（零配置）
//
// RFC 6761 规定 `*.localhost` 必须解析到 loopback，现代浏览器原生支持。
// 于是默认基座是 `<标签>.localhost:<webui端口>`——装完即可用，
// **不需要 DNS、证书、/etc/hosts 或任何配置**。远程访问时配置 `base_domain`
// （如 webui.example.com）即切成 `<标签>.webui.example.com`。
//
// ## 认证
//
// 逐条由插件声明（ProxyAuthHomeAgent / ProxyAuthNone），默认 HomeAgent 统一保护。
// 但 Host 路由下**子域与主门户不同源**，浏览器不会把门户的 homeagent_session
// 发给子域——所以受保护模式下由反代层校验门户会话/API Key，校验通过后放行。
// 详见 authorizeProxy。

// ProxyRoute 是一条**已解析**的反代路由（声明 + 归属插件 + 校验结果）。
type ProxyRoute struct {
	Plugin string // 声明该服务的插件名
	Name   string // 声明内的服务标识（展示用，如 "ui"）
	Host   string // 子域标签（小写，已归一化）
	Target string // 上游地址（原样，含可能的 scheme/路径前缀）
	WS     bool   // 是否允许 WebSocket 升级
	Auth   string // 生效的鉴权模式（已归一化）
	Err    string // 非空表示该条声明被拒绝及原因（不参与路由，仅展示）

	upstream *url.URL
	reverse  *httputil.ReverseProxy
}

// proxyTable 是全部反代路由的**不可变快照**。
//
// 用快照 + 原子替换而不是加锁读写 map：反代处于每个请求的热路径上，
// 而声明只在启动/插件重载时变化。读路径无锁，重载时整体换指针。
type proxyTable struct {
	routes  map[string]*ProxyRoute // key = 小写 host 标签
	ordered []*ProxyRoute          // 稳定顺序（展示/配置页用）
	base    string                 // 基域名（"" 表示用 localhost）
}

var (
	proxyMu      sync.RWMutex
	proxySnap    *proxyTable
	proxyDirty   bool  // 声明有变更、需要重建快照
	proxySeenVer int64 // 上次建表时看到的内置声明版本号
)

// manualProxyRoutes 是「手填」来源：用户在 webui 设置页配置的额外/覆盖条目。
//
// 保持原始文本（每行 `标签 上游地址 [选项]`），解析在 rebuildProxyTable 里做，
// 解析失败不会让设置页爆炸，而是作为一条 Err 条目展示出来。
var manualProxyRoutes string

// SetManualProxyRoutes 注入手填的声明（插件 Start 时从设置读取）。
func SetManualProxyRoutes(raw string) {
	proxyMu.Lock()
	manualProxyRoutes = raw
	proxyDirty = true
	proxySnap = nil
	proxyMu.Unlock()
}

// InvalidateProxyRoutes 标记声明有变更（插件启停/重载后调用）。
func InvalidateProxyRoutes() {
	proxyMu.Lock()
	proxyDirty = true
	proxySnap = nil
	proxyMu.Unlock()
}

// proxyBaseDomain 返回基域名：配置了就用，否则回落到 localhost。
func proxyBaseDomain(settings sdk.SettingsAPI) string {
	if settings != nil {
		if v, err := settings.Get("base_domain"); err == nil && v != nil {
			if s, ok := v.(string); ok {
				if d := strings.Trim(strings.ToLower(strings.TrimSpace(s)), "."); d != "" {
					return d
				}
			}
		}
	}
	return "localhost"
}

// proxyHostLabel 从请求 Host 里抽出子域标签。
//
// 处理三种输入：
//   - `huawei.localhost:8080` → "huawei"
//   - `huawei.webui.example.com`（base_domain=webui.example.com）→ "huawei"
//   - `webui.example.com`（基域名本身）→ ""（不是插件路由，交给主站）
//
// 刻意只认**单层**子域（不匹配 `a.b.webui.example.com`）：多级标签会让
// 「哪个是插件、哪个是基域」变得含混，且容易被 `..` 类输入绕过判断。
func proxyHostLabel(host, base string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return ""
	}
	if hp, _, err := net.SplitHostPort(h); err == nil {
		h = hp
	} else if i := strings.LastIndexByte(h, ':'); i >= 0 {
		// 无括号的 IPv6 等异常输入：丢弃端口段
		h = h[:i]
	}
	h = strings.Trim(h, ".")
	if base == "" {
		base = "localhost"
	}
	base = strings.ToLower(base)
	if h == base {
		return ""
	}
	suffix := "." + base
	if !strings.HasSuffix(h, suffix) {
		return ""
	}
	label := strings.TrimSuffix(h, suffix)
	// 只认单层标签：出现 `.` 说明是多级子域，不接。
	if label == "" || strings.Contains(label, ".") {
		return ""
	}
	return label
}

// buildProxyTable 由「插件声明 + 手填条目」构建路由表。
//
// 冲突与非法条目的处理原则：**宁可报出来，不可静默丢弃**。被拒绝的条目
// 仍会出现在表里（Err 非空），在配置页可见；只是不参与路由。
func buildProxyTable(decls []proxyDecl, manualText string, settings sdk.SettingsAPI) *proxyTable {
	base := proxyBaseDomain(settings)
	t := &proxyTable{routes: map[string]*ProxyRoute{}, base: base}

	add := func(r *ProxyRoute) {
		t.ordered = append(t.ordered, r)
		if r.Err != "" {
			return
		}
		key := strings.ToLower(r.Host)
		if prev, dup := t.routes[key]; dup {
			// 冲突：保留先到者，后来者标错。**不做后者覆盖**——那会让先声明者
			// 静默消失，用户以为两个插件都挂上了。
			r.Err = fmt.Sprintf("子域标签 %q 已被插件 %s 的服务 %s 占用", r.Host, prev.Plugin, prev.Name)
			return
		}
		t.routes[key] = r
	}

	for _, d := range decls {
		r := &ProxyRoute{
			Plugin: d.Plugin,
			Name:   d.Name,
			Host:   d.Host,
			Target: d.Target,
			WS:     d.WebSocket,
			Auth:   sdk.EffectiveProxyAuth(d.Auth),
		}
		if msg := sdk.ValidateProxyDecl(sdk.ProxyDecl{
			Name: d.Name, Host: d.Host, Target: d.Target, WebSocket: d.WebSocket, Auth: d.Auth,
		}); msg != "" {
			r.Err = msg
		} else if r.Name == "" {
			r.Name = d.Plugin
		}
		add(r)
	}

	for _, d := range parseManualRoutes(manualText) {
		r := &ProxyRoute{
			Plugin: "manual",
			Name:   d.Name,
			Host:   d.Host,
			Target: d.Target,
			WS:     d.WebSocket,
			Auth:   sdk.EffectiveProxyAuth(d.Auth),
		}
		if msg := sdk.ValidateProxyDecl(sdk.ProxyDecl{
			Name: d.Name, Host: d.Host, Target: d.Target, WebSocket: d.WebSocket, Auth: d.Auth,
		}); msg != "" {
			r.Err = msg
		}
		add(r)
	}

	// 给合法路由预建 ReverseProxy（每条一个，避免每请求分配）。
	for _, r := range t.ordered {
		if r.Err != "" {
			continue
		}
		u, err := parseUpstream(r.Target)
		if err != nil {
			r.Err = "解析上游地址失败: " + err.Error()
			delete(t.routes, strings.ToLower(r.Host))
			continue
		}
		r.upstream = u
		r.reverse = newReverseProxy(u)
		log.Printf("[webui] 反代: %s.%s → %s (plugin=%s ws=%v auth=%s)",
			r.Host, base, r.Target, r.Plugin, r.WS, r.Auth)
	}

	sort.SliceStable(t.ordered, func(i, j int) bool {
		if t.ordered[i].Err != t.ordered[j].Err {
			return t.ordered[i].Err == ""
		}
		if t.ordered[i].Host != t.ordered[j].Host {
			return t.ordered[i].Host < t.ordered[j].Host
		}
		return t.ordered[i].Plugin < t.ordered[j].Plugin
	})
	return t
}

// parseUpstream 把声明里的 Target 解析成 *url.URL。
// 允许省略 scheme（默认 http）与端口（http→80 / https→443）。
func parseUpstream(target string) (*url.URL, error) {
	s := strings.TrimSpace(target)
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("只支持 http/https 上游，得到 %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("缺少主机部分")
	}
	return u, nil
}

// newReverseProxy 构造一个 httputil.ReverseProxy。
//
// 一次性消除手写反代的历史缺陷：
//  1. **逐帧 Flush**：httputil.ReverseProxy 在响应带 FlushInterval 或识别到
//     text/event-stream 时会 Flush；这里显式设 -1（立即 flush），否则上游的
//     SSE/流式响应会被缓冲到上游关闭才下发（旧实现实测：3 帧 200ms 间隔的
//     流，客户端在 +600ms 一次性收到全部）。
//  2. **不跟随上游 3xx**：旧实现用 http.DefaultClient（默认跟最多 10 跳），
//     上游 302 到内网地址时反代自己跟过去、失败就回 502，并把内网 URL
//     泄给客户端。ReverseProxy 默认不跟随重定向，3xx 原样透传。
//  3. **补齐转发头**：SetXForwarded 注入 X-Forwarded-For/Host/Proto，
//     旧实现完全不注入，上游无法判断真实来源。
func newReverseProxy(u *url.URL) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.SetXForwarded()
			// 透传子域标签给上游（插件据此可感知自己被挂在哪个标签下）。
			pr.Out.Header.Set("X-HA-Proxy-Host", pr.In.Host)
			// 上游可能自带鉴权，浏览器带来的门户 cookie 不应泄漏给它。
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-API-Key")
		},
		FlushInterval: -1, // 立即 flush：SSE/长轮询逐帧下发
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[webui] 反代 %s 失败: %v", r.Host, err)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `{"error":"上游不可达: %s"}`, strings.ReplaceAll(err.Error(), `"`, `'`))
		},
		ModifyResponse: func(resp *http.Response) error {
			// 上游 Set-Cookie 的 Path 若为 "/"，会因 Host 路由而只作用于该子域，
			// 天然隔离，无需重写。但 Domain 若被上游写成裸域会把 cookie 打到
			// 主门户域上，属于跨插件越权——剥掉它，交给浏览器按当前 host 收窄。
			cookies := resp.Cookies()
			if len(cookies) > 0 {
				resp.Header.Del("Set-Cookie")
				for _, c := range cookies {
					c.Domain = ""
					resp.Header.Add("Set-Cookie", c.String())
				}
			}
			return nil
		},
	}
	return rp
}

// currentProxyTable 返回当前快照；需要时按声明重建。
//
// declProvider 由插件在 Start 时注入（见 handler.go 的 SetProxyDeclProvider），
// 它负责从插件目录读 manifest 并归一化。这里做成回调而不是直接依赖内部状态，
// 是为了让反代层可单测（测试注入假声明）。
var declProvider func() []proxyDecl

// SetProxyDeclProvider 注入「读取全部插件声明」的回调。
func SetProxyDeclProvider(fn func() []proxyDecl) { declProvider = fn }

func currentProxyTable() *proxyTable {
	// 内置插件的声明是运行期登记的（插件 Start 时），版本号一变就重建——
	// 比每次请求都重新聚合一遍便宜得多。
	if sdk.BuiltinProxyVersion() != proxySeenVer {
		proxyMu.Lock()
		proxyDirty = true
		proxyMu.Unlock()
	}
	proxyMu.RLock()
	if !proxyDirty && proxySnap != nil {
		t := proxySnap
		proxyMu.RUnlock()
		return t
	}
	manual := manualProxyRoutes
	proxyMu.RUnlock()

	proxyMu.Lock()
	defer proxyMu.Unlock()
	if proxySnap != nil && !proxyDirty {
		return proxySnap
	}
	var decls []proxyDecl
	if declProvider != nil {
		decls = declProvider()
	}
	// 内置插件的运行期声明（无 plugin.json，扫目录发现不到）。
	for plugin, list := range sdk.BuiltinProxyDecls() {
		for _, d := range list {
			host := d.Host
			if host == "" {
				host = sdk.NormalizeProxyHost(plugin)
			}
			name := d.Name
			if name == "" {
				name = "service"
			}
			decls = append(decls, proxyDecl{
				Plugin: plugin, Name: name, Host: strings.ToLower(host),
				Target: d.Target, WebSocket: d.WebSocket, Auth: d.Auth,
			})
		}
	}
	proxySnap = buildProxyTable(decls, manual, nil)
	proxyDirty = false
	proxySeenVer = sdk.BuiltinProxyVersion()
	return proxySnap
}

// serveProxyHost 是挂在根路由前的 Host 分发入口。
// 返回 true 表示已处理该请求。
func (h *Handler) serveProxyHost(w http.ResponseWriter, r *http.Request) bool {
	base := "localhost"
	if h.settings != nil {
		base = proxyBaseDomain(h.settings)
	}
	label := proxyHostLabel(r.Host, base)
	if label == "" {
		return false
	}
	t := currentProxyTable()
	route, ok := t.routes[label]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("没有插件声明子域 %q（基域名 %s）", label, base),
			"hint":  "在插件 plugin.json 的 proxies 里声明，或在 webui 设置页手填",
		})
		return true
	}

	// WebSocket 升级必须由插件显式声明。未声明时明确拒绝，而不是把升级请求
	// 当普通请求透传——后者表现为前端不断重连、日志里看不出原因。
	if isWebSocketUpgrade(r) && !route.WS {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("插件 %s 的服务 %s 未声明 websocket", route.Plugin, route.Name),
		})
		return true
	}

	if route.Auth == sdk.ProxyAuthHomeAgent && !h.authorizeProxy(w, r) {
		return true // 已写 401
	}

	if route.upstream == nil || route.reverse == nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "路由未就绪"})
		return true
	}
	route.reverse.ServeHTTP(w, r)
	return true
}

// authorizeProxy 校验受保护路由的访问者。
//
// Host 路由下子域与门户不同源，浏览器**不会**自动带上门户 cookie；因此这里
// 接受三种凭证，任一通过即放行：
//  1. 门户会话 cookie（用户同浏览器访问过门户时；
//     SameSite=Lax 在同站子域导航下会带上，同站不同源仍算同一 site）；
//  2. X-API-Key / Bearer（脚本与非浏览器客户端）；
//  3. `?__token=` 查询参数（便于在新标签页里直接打开，见"服务入口"）。
//
// 三者都没有时返回 401 并给出**可操作提示**（告诉用户先登录门户），
// 而不是把请求静默透传给上游。
func (h *Handler) authorizeProxy(w http.ResponseWriter, r *http.Request) bool {
	if h.validAPIKey(r) || h.validSession(r) {
		return true
	}
	// ?__token= 形式：与 X-API-Key 同一把密钥，用于「点一下直接打开」的入口。
	if tok := strings.TrimSpace(r.URL.Query().Get("__token")); tok != "" {
		apiKey, _, _, _ := h.getWebUIConfig()
		if apiKey != "" && tok == apiKey {
			return true
		}
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="homeagent"`)
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error": "该服务由 HomeAgent 统一保护，需要先登录门户或用 X-API-Key 访问",
		"hint":  "浏览器先访问门户登录；脚本用 -H 'X-API-Key: <key>' 或 ?__token=<key>",
	})
	return false
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// proxyServiceEntry 是「服务入口」条目：给前端渲染选项卡用。
type proxyServiceEntry struct {
	Plugin   string `json:"plugin"`
	PluginZh string `json:"plugin_name"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	URL      string `json:"url"`  // 完整可点 URL（带端口/协议，按当前请求推导）
	Auth     string `json:"auth"` // homeagent | none
	WS       bool   `json:"websocket"`
	Target   string `json:"target"`
	OK       bool   `json:"ok"` // false = 声明被拒或上游不可达（见 error）
	Error    string `json:"error,omitempty"`
}

// listProxyServices 汇总反代服务清单（含被拒条目，供配置页排错）。
// schemePort 由调用方按当前请求推导（本机 http:8080 / 远程 https:443 等）。
func (h *Handler) listProxyServices(scheme, hostPort string) []proxyServiceEntry {
	t := currentProxyTable()
	metas := map[string]sdk.PluginMeta{}
	if h.pluginMgr != nil {
		metas = h.pluginMgr.PluginMetas()
	}
	out := make([]proxyServiceEntry, 0, len(t.ordered))
	for _, r := range t.ordered {
		e := proxyServiceEntry{
			Plugin: r.Plugin,
			Name:   r.Name,
			Host:   r.Host,
			Auth:   r.Auth,
			WS:     r.WS,
			Target: r.Target,
			OK:     r.Err == "",
			Error:  r.Err,
		}
		if m, ok := metas[r.Plugin]; ok {
			e.PluginZh = m.NameZh
		}
		if e.PluginZh == "" {
			e.PluginZh = r.Plugin
		}
		if r.Err == "" {
			e.URL = fmt.Sprintf("%s://%s.%s%s", scheme, r.Host, t.base, hostPort)
		}
		out = append(out, e)
	}
	return out
}

// ---- 声明读取（自动发现）----

// proxyDecl 是归一化后的插件声明（与 SDK 的 ProxyDecl 同形，另带插件名）。
type proxyDecl struct {
	Plugin    string
	Name      string
	Host      string
	Target    string
	WebSocket bool
	Auth      string
}

// readPluginProxyDecls 扫描插件目录里的 plugin.json，聚合 proxies 声明。
//
// 为什么在 webui 侧读而不是问内核：manifest 解析（internal/plugin.ReadManifest）
// 是纯文件读取，webui 已能拿到插件目录（sdk.PluginManager.PluginDir()），
// 这样**无需给内核接口加方法**即可实现自动发现——插件进程没起来也照样发现，
// 便于给出「声明了但不可达」的准确报错。
//
// 内置插件（编译进内核、无独立目录）不参与：它们要暴露服务应走内核自身路由。
func readPluginProxyDecls(pluginDir string) []proxyDecl {
	if pluginDir == "" {
		return nil
	}
	entries, err := readDirNames(pluginDir)
	if err != nil {
		log.Printf("[webui] 读取插件目录失败（反代自动发现跳过）: %v", err)
		return nil
	}
	var out []proxyDecl
	for _, name := range entries {
		m, err := pluginpkg.ReadManifest(filepath.Join(pluginDir, name))
		if err != nil {
			continue // 非插件目录/无 manifest：静默跳过
		}
		for i, p := range m.Proxies {
			host := strings.TrimSpace(p.Host)
			explicitHost := host != ""
			if !explicitHost {
				host = sdk.NormalizeProxyHost(m.Name)
			}
			sname := strings.TrimSpace(p.Name)
			if sname == "" {
				sname = "service"
			}
			// 只在**未显式指定** host 时加序号：插件写死的 host 是它的
			// 刻意选择（如 remotedevice 的 "devices"），被自动编号覆盖会让
			// 声明静默失效——用户按文档访问 devices.<base> 就是 404。
			if !explicitHost && i > 0 {
				host = fmt.Sprintf("%s-%d", host, i+1)
			}
			out = append(out, proxyDecl{
				Plugin:    m.Name,
				Name:      sname,
				Host:      strings.ToLower(host),
				Target:    p.Target,
				WebSocket: p.WebSocket,
				Auth:      p.Auth,
			})
		}
	}
	return out
}

// parseManualRoutes 解析手填条目。格式（每行一条，空行与 # 注释跳过）：
//
//	<子域标签> <上游地址> [ws] [auth=none|homeagent]
//
// 例：
//
//	grafana 127.0.0.1:3000
//	devices 127.0.0.1:9890 ws auth=none
//
// 手填条目与插件声明同表竞争：先声明者占住标签，后来者（无论来源）被标错，
// 便于用户发现"我手填的标签和某插件撞了"。
func parseManualRoutes(text string) []proxyDecl {
	var out []proxyDecl
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			out = append(out, proxyDecl{
				Plugin: "manual", Name: "invalid", Host: "invalid",
				Target: "",
				Auth:   "homeagent",
			})
			continue
		}
		d := proxyDecl{Plugin: "manual", Name: fields[0], Host: strings.ToLower(fields[0]), Target: fields[1]}
		for _, opt := range fields[2:] {
			lo := strings.ToLower(opt)
			switch {
			case lo == "ws" || lo == "websocket":
				d.WebSocket = true
			case strings.HasPrefix(lo, "auth="):
				d.Auth = strings.TrimPrefix(lo, "auth=")
			default:
				d.Target = ""
			}
		}
		out = append(out, d)
	}
	return out
}

// ---- 小工具（避免为两行逻辑引入额外依赖文件）----

func readDirNames(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// proxyUpstreamReachable 探测上游是否可达（供服务入口列表显示状态）。
func proxyUpstreamReachable(target string) error {
	u, err := parseUpstream(target)
	if err != nil {
		return err
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		if u.Scheme == "https" {
			host = net.JoinHostPort(host, "443")
		} else {
			host = net.JoinHostPort(host, "80")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return err
	}
	return c.Close()
}

// ---- HTTP 接口：服务入口清单 ----

// proxySchemeAndPort 按当前请求推导对外访问的 scheme 与端口。
//
// 为什么不能写死 http:8080：远程访问常经 nginx/frp（https:443），
// 写死会让「服务入口」给出的链接点不开。优先取反代头，回退请求自身。
func (h *Handler) proxySchemeAndPort(r *http.Request) (string, string) {
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(p, ",")[0]))
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	// Host 可能带端口；子域链接要沿用同一个端口（单端口穿透的前提）。
	if _, port, err := net.SplitHostPort(host); err == nil {
		return scheme, ":" + port
	}
	if scheme == "https" {
		return scheme, "" // 443 省略
	}
	// 按 Host 头推断不出端口（如反代层剥了），退回到监听地址的端口。
	if _, port, err := net.SplitHostPort(r.Host); err == nil {
		return scheme, ":" + port
	}
	if h.hostPort != "" {
		return scheme, h.hostPort
	}
	return scheme, ""
}

// handleProxyServices 返回反代服务清单（含被拒条目与可达性）。
func (h *Handler) handleProxyServices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scheme, port := h.proxySchemeAndPort(r)
	svcs := h.listProxyServices(scheme, port)
	// 可达性探测：并发带超时，避免一个坏上游拖住整个清单。
	var wg sync.WaitGroup
	for i := range svcs {
		if !svcs[i].OK {
			continue
		}
		wg.Add(1)
		go func(e *proxyServiceEntry) {
			defer wg.Done()
			if err := proxyUpstreamReachable(e.Target); err != nil {
				e.OK = false
				e.Error = "上游不可达: " + err.Error()
			}
		}(&svcs[i])
	}
	wg.Wait()
	base := "localhost"
	if h.settings != nil {
		base = proxyBaseDomain(h.settings)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"services":    svcs,
		"base_domain": base,
		"scheme":      scheme,
		"port":        port,
	})
}

// handleProxyInfo 返回反代能力总览（给设置页说明用）。
func (h *Handler) handleProxyInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	t := currentProxyTable()
	mode := "host"
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"mode":        mode,
		"base_domain": t.base,
		"count":       len(t.routes),
		"total":       len(t.ordered),
		"manual":      strings.TrimSpace(manualProxyRoutes) != "",
	})
}
