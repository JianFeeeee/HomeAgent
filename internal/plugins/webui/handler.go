package webui

import (
	"bufio"
	"embed"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"net/http"
)

// 本文件是 WebUI 的骨架：嵌入式前端资源、Handler 结构、构造、路由表、
// 鉴权/会话/日志中间件与静态页。各资源的具体 handler 见同包 handler_*.go。

//go:embed dashboard.html dashboard.css dashboard.js mascot.webp logo.svg
var dashboardFS embed.FS

var dashboardHTML string

const loginHTML = `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1.0"><title>HomeAgent 登录</title><style>
:root{--sakura-300:#ffb3c8;--sakura-400:#ff7fac;--sakura-500:#f33b7c;--frost-300:#88c0d0;--text-primary:#e8e6ee;--text-secondary:#a0a3b5;--text-muted:#6e7284;--bg-primary:#0d0d16;--bg-card:rgba(24,24,38,0.72);--bg-input:rgba(13,13,22,0.6);--border-color:rgba(255,255,255,0.09);--glass-border:rgba(255,255,255,0.12);--glass-blur:20px;--radius-lg:18px;--radius-md:12px;--radius-pill:999px;--shadow-glow:0 0 18px rgba(243,59,124,0.35);--ease-out:cubic-bezier(.22,.61,.36,1)}
[data-theme="light"]{--text-primary:#23252e;--text-secondary:#5b5f73;--text-muted:#9aa0b5;--bg-primary:#f6f3f8;--bg-card:rgba(255,255,255,0.72);--bg-input:rgba(255,255,255,0.8);--border-color:rgba(35,37,46,0.1);--glass-border:rgba(255,255,255,0.75);--shadow-glow:0 0 18px rgba(243,59,124,0.28)}
*{box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Microsoft YaHei',sans-serif;background:radial-gradient(1200px 800px at 15% 0%,rgba(243,59,124,.22),transparent 55%),radial-gradient(1000px 700px at 90% 10%,rgba(136,192,208,.18),transparent 55%),radial-gradient(900px 600px at 50% 110%,rgba(163,184,255,.14),transparent 60%),var(--bg-primary);background-attachment:fixed;color:var(--text-primary);display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;padding:20px;transition:background .3s,color .2s;overflow:hidden}
body::before{content:'';position:fixed;inset:0;pointer-events:none;background-image:radial-gradient(rgba(255,255,255,.05) 1px,transparent 1px);background-size:28px 28px}
.login-wrap{width:100%;max-width:400px;position:relative;z-index:1}
.login-card{background:var(--bg-card);backdrop-filter:blur(var(--glass-blur)) saturate(1.4);-webkit-backdrop-filter:blur(var(--glass-blur)) saturate(1.4);border:1px solid var(--glass-border);border-radius:var(--radius-lg);padding:36px 32px 28px;box-shadow:0 20px 60px rgba(0,0,0,.45)}
.logo{width:84px;height:84px;margin:0 auto 16px;border-radius:50%;overflow:hidden;border:2px solid rgba(255,255,255,.25);box-shadow:0 8px 24px rgba(243,59,124,.35);background:#F8FAFC;display:flex;align-items:center;justify-content:center}
.logo img{width:100%;height:100%;object-fit:cover;display:block}
h1{margin:0 0 6px;font-size:22px;font-weight:700;text-align:center;letter-spacing:-.01em;background:linear-gradient(120deg,var(--sakura-400),var(--frost-300));-webkit-background-clip:text;background-clip:text;-webkit-text-fill-color:transparent}
.sub{margin:0 0 24px;font-size:12px;color:var(--text-muted);text-align:center}
label{display:block;font-size:11px;color:var(--text-secondary);margin:14px 0 6px;font-weight:500;letter-spacing:.03em}
input{width:100%;padding:11px 14px;border-radius:var(--radius-md);border:1px solid var(--border-color);background:var(--bg-input);color:var(--text-primary);font-size:14px;outline:none;transition:border .15s,box-shadow .15s}
input:focus{border-color:var(--sakura-400);box-shadow:var(--shadow-glow)}
input::placeholder{color:var(--text-muted)}
button{width:100%;margin-top:22px;padding:12px 14px;border:none;border-radius:var(--radius-md);background:linear-gradient(120deg,var(--sakura-500),var(--sakura-400));color:#fff;font-size:14px;font-weight:600;cursor:pointer;letter-spacing:.08em;transition:transform .15s var(--ease-out),box-shadow .2s,filter .2s}
button:hover{transform:translateY(-1px);filter:brightness(1.08);box-shadow:0 8px 24px rgba(243,59,124,.4)}
button:active{transform:translateY(0) scale(.98)}
button:disabled{opacity:.6;cursor:not-allowed;transform:none}
.err{margin-top:14px;color:#ff6b6b;font-size:13px;text-align:center;min-height:18px;transition:opacity .2s}
.foot{margin-top:20px;font-size:11px;color:var(--text-muted);text-align:center}
.foot svg{width:12px;height:12px;vertical-align:-2px}
@media(max-width:480px){.login-card{padding:28px 22px 22px}}
</style></head><body><div class="login-wrap"><div class="login-card"><div class="logo"><img src="/logo.svg" alt="HomeAgent"></div><h1>HomeAgent</h1><p class="sub">智能家居助手控制台</p><form id="login-form"><label>用户名</label><input id="username" autocomplete="username" placeholder="请输入用户名" required><label>密码</label><input id="password" type="password" autocomplete="current-password" placeholder="请输入密码" required><button type="submit" id="submit-btn">登 录</button><div id="err" class="err"></div></form></div><div class="foot">HomeAgent &middot; NapCat Theme</div></div><script>
(function(){var m=window.matchMedia('(prefers-color-scheme: light)');function apply(){document.documentElement.setAttribute('data-theme',m.matches?'light':'dark')}apply();m.addEventListener('change',apply)})();
document.getElementById('login-form').addEventListener('submit',async(e)=>{e.preventDefault();const username=document.getElementById('username').value.trim();const password=document.getElementById('password').value;const err=document.getElementById('err');const btn=document.getElementById('submit-btn');err.textContent='';if(!username||!password){err.textContent='请输入用户名和密码';return}btn.disabled=true;btn.textContent='登录中...';try{const r=await fetch('/api/v1/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username,password})});if(r.ok){location.href='/';return}let data={};try{data=await r.json()}catch(_){}err.textContent=data.error||'登录失败'}catch(_){err.textContent='网络错误,请重试'}finally{btn.disabled=false;btn.textContent='登 录'}});
document.getElementById('password').addEventListener('keydown',function(e){if(e.key==='Enter')document.getElementById('login-form').dispatchEvent(new Event('submit'))});
</script></body></html>`

// dashboardHTML 是组装好的控制台页面：dashboard.html 外壳 + dashboard.css + dashboard.js。
//
// 前端刻意没有构建链，所以拆分的办法是：外壳里留 {{DASHBOARD_CSS}} / {{DASHBOARD_JS}}
// 两个占位符，启动时把两个资产原样填回去——**发出的 HTML 与拆分前逐字节一致**，
// 但 6637 行的单文件变成「外壳 + 样式 + 脚本」三份，便于编辑与评审。
func init() {
	html, err := dashboardFS.ReadFile("dashboard.html")
	if err != nil {
		return
	}
	out := string(html)
	for _, a := range []struct{ placeholder, file string }{
		{"{{DASHBOARD_CSS}}", "dashboard.css"},
		{"{{DASHBOARD_JS}}", "dashboard.js"},
	} {
		body, err := dashboardFS.ReadFile(a.file)
		if err != nil {
			log.Printf("[webui] 读取前端资产 %s 失败: %v", a.file, err)
			continue
		}
		// 资产文件末尾的换行由占位符所在行自己的换行承担，避免多出空行。
		out = strings.Replace(out, a.placeholder, strings.TrimRight(string(body), "\n"), 1)
	}
	dashboardHTML = out
}

type Handler struct {
	sdk        *sdk.PluginSDK
	supervisor sdk.SupervisorAPI
	memory     sdk.MemoryAPI
	indexer    sdk.IndexerAPI
	adapter    sdk.AdapterAPI
	config     sdk.ConfigAPI
	startTime  time.Time
	textMem    sdk.TextMemoryAPI
	knowledge  sdk.KnowledgeAPI
	tracker    sdk.TrackerAPI
	settings   sdk.SettingsAPI
	pluginMgr  sdk.PluginManager
	status     sdk.StatusAPI
	term       sdk.TerminalAPI
	llm        sdk.LLMAPI

	// hostPort 是 webui 实际监听的 ":port"（用于推导服务入口链接）。
	hostPort string

	sessionMu sync.Mutex
	sessions  map[string]time.Time

	sseEvents *sseEventRing // SSE 事件环状缓冲区，Last-Event-ID 重放用

	chatMu      sync.Mutex
	chatHistory []ChatMsg
	pendingIdx  int   // chatHistory 中正在进行的 assistant 消息索引，-1 表示无
	chatSeq     int64 // 已分配的最大序号；单调递增，作增量查询游标

	// history 是聊天记录的独立存储（默认 <data>/webui_chat_history.json，
	// 插件设置 history_file 可改）。
	history *historyStore

	// 聊天记录写盘节流：chatDirty 由 persistChatLocked 置位，
	// chatPersistLoop 合并连续变更后落盘（chatSaveThrottle 去抖 +
	// chatSaveMaxDelay 兜底），Close 前强制落最后一次。
	chatDirty      bool
	chatDirtySince time.Time
	chatSaveWake   chan struct{}
	chatStop       chan struct{}
	chatLoopDone   chan struct{}
	chatCloseOnce  sync.Once

	chatMsgMu    sync.Mutex
	chatMsgCache map[string]*chatMsgEntry // client_msg_id -> 首次处理结果
	chatMsgOrder []string                 // FIFO 淘汰序
}

func NewHandler(s *sdk.PluginSDK) *Handler {
	var (
		sup  sdk.SupervisorAPI
		mem  sdk.MemoryAPI
		idx  sdk.IndexerAPI
		ad   sdk.AdapterAPI
		cfg  sdk.ConfigAPI
		tm   sdk.TextMemoryAPI
		ks   sdk.KnowledgeAPI
		tr   sdk.TrackerAPI
		se   sdk.SettingsAPI
		pm   sdk.PluginManager
		st   sdk.StatusAPI
		term sdk.TerminalAPI
		llm  sdk.LLMAPI
	)
	if s != nil {
		sup, mem, idx = s.Supervisor(), s.Memory(), s.Indexer()
		ad, cfg = s.Adapter(), s.Config()
		tm, ks, tr = s.TextMemory(), s.Knowledge(), s.Tracker()
		se, pm = s.Settings(), s.PluginMgr()
		st, llm = s.Status(), s.LLM()
		term = s.Terminal()
	}
	h := &Handler{
		sdk:          s,
		supervisor:   sup,
		memory:       mem,
		indexer:      idx,
		adapter:      ad,
		config:       cfg,
		startTime:    time.Now(),
		textMem:      tm,
		knowledge:    ks,
		tracker:      tr,
		settings:     se,
		pluginMgr:    pm,
		status:       st,
		term:         term,
		llm:          llm,
		sessions:     make(map[string]time.Time),
		pendingIdx:   -1,
		chatMsgCache: make(map[string]*chatMsgEntry),
		sseEvents:    newSSEEventRing(200),
		// 聊天记录独立存储：默认 <data>/webui_chat_history.json，
		// 插件设置 history_file 可改（相对路径按 data 目录解析）。
		history: newHistoryStore(resolveHistoryFile(settingString(se, "history_file"), webDataDir)),

		chatSaveWake: make(chan struct{}, 1),
		chatStop:     make(chan struct{}),
		chatLoopDone: make(chan struct{}),
	}
	go h.chatPersistLoop()
	h.loadChatHistory()
	if s != nil {
		h.subscribeChatEvents()
	}
	return h
}

func (h *Handler) getWebUIConfig() (apiKey, username, password string, ttl time.Duration) {
	ttl = 24 * time.Hour
	if h.settings == nil {
		return
	}
	if v, _ := h.settings.Get("api_key"); v != nil {
		apiKey, _ = v.(string)
	}
	if v, _ := h.settings.Get("username"); v != nil {
		username, _ = v.(string)
	}
	if v, _ := h.settings.Get("password"); v != nil {
		password, _ = v.(string)
	}
	if v, _ := h.settings.Get("session_ttl_hours"); v != nil {
		switch n := v.(type) {
		case float64:
			if n > 0 {
				ttl = time.Duration(n) * time.Hour
			}
		case string:
			if i, err := strconv.Atoi(n); err == nil && i > 0 {
				ttl = time.Duration(i) * time.Hour
			}
		}
	}
	if username == "" {
		username = "admin"
	}
	return
}

func (h *Handler) createSession() (string, time.Time, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	_, _, _, ttl := h.getWebUIConfig()
	expires := time.Now().Add(ttl)
	token := hex.EncodeToString(buf)
	h.sessionMu.Lock()
	h.sessions[token] = expires
	h.sessionMu.Unlock()
	return token, expires, nil
}

func (h *Handler) validSession(r *http.Request) bool {
	cookie, err := r.Cookie("homeagent_session")
	if err != nil || cookie.Value == "" {
		return false
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	expires, ok := h.sessions[cookie.Value]
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		delete(h.sessions, cookie.Value)
		return false
	}
	return true
}

func (h *Handler) validAPIKey(r *http.Request) bool {
	apiKey, _, _, _ := h.getWebUIConfig()
	if apiKey == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if got == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	return got != "" && got == apiKey
}

func (h *Handler) requireAPI(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey, _, _, _ := h.getWebUIConfig()
		if apiKey == "" && !h.validSession(r) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "webui api_key not configured"})
			return
		}
		if h.validAPIKey(r) || h.validSession(r) {
			fn(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
}

func (h *Handler) requireWeb(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// API key 客户端（ArkTS/GUI 远程连接）与 cookie session 同等放行：
		// agent 输出的 /files/、/uploads/ 附件 URL 会被非浏览器客户端直接加载，
		// 它们没有也不应有 web 登录态。
		if h.validAPIKey(r) {
			fn(w, r)
			return
		}
		_, username, password, _ := h.getWebUIConfig()
		if username == "" || password == "" {
			http.Error(w, "webui username/password not configured", http.StatusServiceUnavailable)
			return
		}
		if h.validSession(r) {
			fn(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// clientIP 提取请求的真实客户端 IP。
// 优先取 X-Forwarded-For / X-Real-IP（反向代理/frp 场景），回退 RemoteAddr。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// 取第一个（最接近客户端的地址）
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requestLog 每次请求的日志行（含 IP、认证方式、状态码）。
// 由 logged 中间件在请求完成后调用，authType 为 caller 预先判定。
func requestLog(r *http.Request, authType, ip string, status int, dur time.Duration) {
	// 认证方式判定（供排查谁调用了变更接口）
	if authType == "" {
		switch {
		case r.Header.Get("X-API-Key") != "" || strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "):
			authType = "api-key"
		case func() bool { c, err := r.Cookie("homeagent_session"); return err == nil && c.Value != "" }():
			authType = "session"
		default:
			authType = "none"
		}
	}
	log.Printf("[webui] %s %s from=%s auth=%s status=%d (%s)", r.Method, r.URL.Path, ip, authType, status, dur.Round(time.Millisecond))
}

// logged 中间件：包装任意 handler，记录请求 IP / 方法 / 路径 / 认证方式 / 状态码。
// 置于最外层（mux 之上），覆盖所有路由（含登录页、静态资源）。
func (h *Handler) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 预判认证方式（在 requireAPI/requireWeb 之前的原始请求判定）
		authType := "none"
		switch {
		case h.validAPIKey(r):
			authType = "api-key"
		case h.validSession(r):
			authType = "session"
		}
		// SSE 长连接：不阻塞在完成时记录（连接可能持续很久），启动即记一条
		if strings.HasSuffix(r.URL.Path, "/chat/events") {
			requestLog(r, authType, clientIP(r), http.StatusOK, 0)
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		requestLog(r, authType, clientIP(r), sw.status, time.Since(start))
	})
}

// statusWriter 包装 ResponseWriter 以捕获响应状态码。
// SSE 等流式写入直接透传不做缓冲。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	return sw.ResponseWriter.Write(b)
}

func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sw *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := sw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/login", h.handleLoginPage)
	mux.HandleFunc("/api/v1/login", h.handleLogin)
	mux.HandleFunc("/api/v1/logout", h.handleLogout)
	mux.HandleFunc("/api/v1/status", h.requireAPI(h.handleStatus))
	mux.HandleFunc("/api/v1/agents", h.requireAPI(h.handleAgents))
	mux.HandleFunc("/api/v1/agents/", h.requireAPI(h.handleAgentByID))
	mux.HandleFunc("/api/v1/memory", h.requireAPI(h.handleMemory))
	mux.HandleFunc("/api/v1/memory/", h.requireAPI(h.handleMemory))
	mux.HandleFunc("/api/v1/memory/graph", h.requireAPI(h.handleMemoryGraph))
	mux.HandleFunc("/api/v1/memory/context", h.requireAPI(h.handleMemoryContext))
	mux.HandleFunc("/api/v1/memory/tools", h.requireAPI(h.handleMemoryTools))
	mux.HandleFunc("/api/v1/memory/text", h.requireAPI(h.handleTextMemory))
	mux.HandleFunc("/api/v1/network", h.requireAPI(h.handleNetwork))
	mux.HandleFunc("/api/v1/config", h.requireAPI(h.handleConfig))
	mux.HandleFunc("/api/v1/settings", h.requireAPI(h.handleSettings))
	mux.HandleFunc("/api/v1/settings/", h.requireAPI(h.handleSettings))
	mux.HandleFunc("/api/v1/knowledge", h.requireAPI(h.handleKnowledge))
	mux.HandleFunc("/api/v1/knowledge/", h.requireAPI(h.handleKnowledge))
	mux.HandleFunc("/api/v1/adapters", h.requireAPI(h.handleAdapters))
	mux.HandleFunc("/api/v1/adapters/", h.requireAPI(h.handleAdapterByID))
	mux.HandleFunc("/api/v1/tracker", h.requireAPI(h.handleTracker))
	mux.HandleFunc("/api/v1/tracker/", h.requireAPI(h.handleTracker))
	mux.HandleFunc("/api/v1/chat", h.requireAPI(h.handleChat))
	// 用户上传文件并附带消息注入 agent（multipart：file + message）
	mux.HandleFunc("/api/v1/chat/file", h.requireAPI(h.handleChatFile))
	mux.HandleFunc("/api/v1/chat/history", h.requireAPI(h.handleChatHistory))
	mux.HandleFunc("/api/v1/chat/interrupt", h.requireAPI(h.handleChatInterrupt))
	mux.HandleFunc("/api/v1/chat/events", h.requireAPI(h.handleChatEvents))
	mux.HandleFunc("/api/v1/terminals", h.requireAPI(h.handleTerminals))
	mux.HandleFunc("/api/v1/cmd/history", h.requireAPI(h.handleCmdHistory))
	mux.HandleFunc("/api/v1/kernel", h.requireAPI(h.handleKernel))
	// 运行态小快照：调度器（排队/四级中断队列/中断栈）+ 驻留子 + 通道拓扑。
	// 单独一条是为了让前端能秒级刷新而不必反复拉 30KB 的 /kernel。
	mux.HandleFunc("/api/v1/runtime", h.requireAPI(h.handleRuntime))
	mux.HandleFunc("/api/v1/persona", h.requireAPI(h.handlePersona))
	mux.HandleFunc("/api/v1/plugins", h.requireAPI(h.handlePlugins))
	mux.HandleFunc("/api/v1/plugins/", h.requireAPI(h.handlePluginByID))
	// 设备网关（可配置反代到 remotedevice；默认禁用，未启用时返回 404）
	mux.HandleFunc("/api/v1/device/", h.requireAPI(h.handleDeviceGatewayProxy))
	// agent 发送的文件下载（webui_files 中转目录；requireWeb 与 dashboard 同源同鉴权）
	mux.HandleFunc("/files/", h.requireWeb(h.handleFiles))
	// 用户上传文件的下载（uploads 目录，同一安全模型）
	mux.HandleFunc("/uploads/", h.requireWeb(h.handleUploads))
	mux.HandleFunc("/v1/chat/completions", h.requireAPI(h.handleOpenAICompletions))
	// 反代服务入口清单：给前端渲染「插件 UI」选项卡。
	// 走 requireAPI：清单本身含上游地址，属于管理面信息，不该匿名可读。
	mux.HandleFunc("/api/v1/proxy/services", h.requireAPI(h.handleProxyServices))
	mux.HandleFunc("/api/v1/proxy", h.requireAPI(h.handleProxyInfo))
	// "/" 兜底：**先**尝试按 Host 分发到插件反代，不是插件子域才落到主站静态页。
	// 顺序很重要——ServeMux 只会把未被更具体模式匹配的请求交给这里。
	mux.HandleFunc("/", h.handleRoot)
}

// handleRoot 是根路由兜底：先看是不是插件反代的子域，不是再走主站。
func (h *Handler) handleRoot(w http.ResponseWriter, r *http.Request) {
	if h.serveProxyHost(w, r) {
		return
	}
	h.requireWeb(h.handleStatic)(w, r)
}

func (h *Handler) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.validSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(loginHTML))
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_, username, password, _ := h.getWebUIConfig()
	if username == "" || password == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "webui username/password not configured"})
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if body.Username != username || body.Password != password {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}
	token, expires, err := h.createSession()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "homeagent_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie("homeagent_session"); err == nil {
		h.sessionMu.Lock()
		delete(h.sessions, cookie.Value)
		h.sessionMu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "homeagent_session", Value: "", Path: "/", Expires: time.Unix(0, 0), MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		w.Write([]byte(dashboardHTML))
		return
	}
	if r.URL.Path == "/mascot.webp" {
		data, err := dashboardFS.ReadFile("mascot.webp")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/webp")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data)
		return
	}
	if r.URL.Path == "/logo.svg" {
		data, err := dashboardFS.ReadFile("logo.svg")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data)
		return
	}
	http.NotFound(w, r)
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
