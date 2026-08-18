package webui

import (
	"bufio"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

//go:embed dashboard.html mascot.webp logo.svg
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

func init() {
	data, err := dashboardFS.ReadFile("dashboard.html")
	if err == nil {
		dashboardHTML = string(data)
	}
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
	llm        sdk.LLMAPI

	sessionMu sync.Mutex
	sessions  map[string]time.Time

	chatMu      sync.Mutex
	chatHistory []ChatMsg
	pendingIdx  int // chatHistory 中正在进行的 assistant 消息索引，-1 表示无
	cmdMu       sync.Mutex
	cmdHistory  []CmdExec
	termMu      sync.Mutex
	termStates  map[string]*termState
}

type ChatMsg struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
	Source           string         `json:"source,omitempty"`
	Time             string         `json:"time"`
}

type ChatToolCall struct {
	Tool   string      `json:"tool"`
	Name   string      `json:"name,omitempty"`
	Args   interface{} `json:"args,omitempty"`
	Result interface{} `json:"result,omitempty"`
	Status string      `json:"status,omitempty"`
	Plugin string      `json:"plugin,omitempty"`
}

type CmdExec struct {
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Status   string `json:"status"`
	Time     string `json:"time"`
}

type termState struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	Running   bool   `json:"running"`
	Output    string `json:"output"`
	CreatedAt string `json:"created_at"`
	Uptime    string `json:"uptime"`
	created   time.Time
}

const maxChatHistory = 200
const maxCmdHistory = 100
const maxTerminals = 50

func NewHandler(s *sdk.PluginSDK) *Handler {
	var (
		sup sdk.SupervisorAPI
		mem sdk.MemoryAPI
		idx sdk.IndexerAPI
		ad  sdk.AdapterAPI
		cfg sdk.ConfigAPI
		tm  sdk.TextMemoryAPI
		ks  sdk.KnowledgeAPI
		tr  sdk.TrackerAPI
		se  sdk.SettingsAPI
		pm  sdk.PluginManager
		st  sdk.StatusAPI
		llm sdk.LLMAPI
	)
	if s != nil {
		sup, mem, idx = s.Supervisor(), s.Memory(), s.Indexer()
		ad, cfg = s.Adapter(), s.Config()
		tm, ks, tr = s.TextMemory(), s.Knowledge(), s.Tracker()
		se, pm = s.Settings(), s.PluginMgr()
		st, llm = s.Status(), s.LLM()
	}
	h := &Handler{
		sdk:        s,
		supervisor: sup,
		memory:     mem,
		indexer:    idx,
		adapter:    ad,
		config:     cfg,
		startTime:  time.Now(),
		textMem:    tm,
		knowledge:  ks,
		tracker:    tr,
		settings:   se,
		pluginMgr:  pm,
		status:     st,
		llm:        llm,
		sessions:   make(map[string]time.Time),
		termStates: make(map[string]*termState),
		pendingIdx: -1,
	}
	h.loadChatHistory()
	if s != nil {
		go h.trackToolEvents()
		h.subscribeChatEvents()
		h.subscribeTerminalStream()
	}
	return h
}

// subscribeTerminalStream 常驻订阅终端实时画面推流（terminal_output 事件），
// 维护 termStates 的 Running 状态与全量输出缓冲，供 /api/v1/terminals 与前端轮询使用。
func (h *Handler) subscribeTerminalStream() {
	if h.sdk == nil {
		return
	}
	h.sdk.Subscribe(sdk.EventTerminalOutput, func(ev *sdk.Event) {
		id, _ := ev.Payload["terminal_id"].(string)
		if id == "" {
			return
		}
		output, _ := ev.Payload["output"].(string)
		running, _ := ev.Payload["running"].(bool)
		h.termMu.Lock()
		ts, ok := h.termStates[id]
		if !ok {
			ts = &termState{ID: id, created: time.Now()}
			h.termStates[id] = ts
		}
		ts.Running = running
		if output != "" {
			const maxTermOutput = 64 * 1024
			if len(ts.Output)+len(output) > maxTermOutput {
				excess := len(ts.Output) + len(output) - maxTermOutput
				if len(ts.Output) > excess {
					ts.Output = ts.Output[excess:]
				} else {
					ts.Output = ""
				}
			}
			ts.Output += output
		}
		h.termMu.Unlock()
	})
}

func (h *Handler) loadChatHistory() {
	if h.settings == nil {
		return
	}
	v, err := h.settings.Get("chathistory")
	if err != nil || v == nil {
		return
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return
	}
	var msgs []ChatMsg
	if err := json.Unmarshal([]byte(s), &msgs); err != nil {
		return
	}
	h.chatMu.Lock()
	h.chatHistory = msgs
	h.chatMu.Unlock()
}

func (h *Handler) trackToolEvents() {
	if h.sdk == nil {
		return
	}
	h.sdk.Subscribe(sdk.EventToolCall, func(ev *sdk.Event) {
		h.handleToolEvent(ev)
	})
}

// subscribeChatEvents 捕获所有通道（cli/qq/webui 等）的对话轮次，
// 与 handleChat 的注入一起构成完整的全通道对话历史。
func (h *Handler) subscribeChatEvents() {
	if h.sdk == nil {
		return
	}
	h.sdk.Subscribe(sdk.EventRawInput, func(ev *sdk.Event) {
		content, _ := ev.Payload["content"].(string)
		source, _ := ev.Payload["source"].(string)
		if content == "" {
			return
		}
		h.chatMu.Lock()
		h.pendingIdx = -1
		h.chatMu.Unlock()
		h.addChatMsg(ChatMsg{
			Role:    "user",
			Content: content,
			Source:  source,
			Time:    time.Unix(ev.Timestamp, 0).Format(time.RFC3339),
		})
	})
	h.sdk.Subscribe(sdk.EventToolCall, func(ev *sdk.Event) {
		tool, _ := ev.Payload["tool"].(string)
		if tool == "" {
			return
		}
		channel, _ := ev.Payload["channel"].(string)
		if channel == "_consolidation_" {
			return
		}
		plugin, _ := ev.Payload["plugin"].(string)
		status, _ := ev.Payload["status"].(string)
		if status == "" {
			status = "ok"
		}
		tc := ChatToolCall{
			Tool:   tool,
			Name:   tool,
			Args:   ev.Payload["args"],
			Result: ev.Payload["result"],
			Status: status,
			Plugin: plugin,
		}
		h.chatMu.Lock()
		msg := h.pendingAssistantLocked()
		if msg == nil {
			h.chatHistory = append(h.chatHistory, ChatMsg{Role: "assistant", Time: time.Now().Format(time.RFC3339)})
			h.pendingIdx = len(h.chatHistory) - 1
			msg = &h.chatHistory[h.pendingIdx]
		}
		msg.ToolCalls = append(msg.ToolCalls, tc)
		h.persistChatLocked()
		h.chatMu.Unlock()
	})
	h.sdk.Subscribe(sdk.EventReasoning, func(ev *sdk.Event) {
		content, _ := ev.Payload["content"].(string)
		if content == "" {
			return
		}
		channel, _ := ev.Payload["channel"].(string)
		if channel == "_consolidation_" {
			return
		}
		h.chatMu.Lock()
		msg := h.pendingAssistantLocked()
		if msg == nil {
			h.chatHistory = append(h.chatHistory, ChatMsg{Role: "assistant", Time: time.Now().Format(time.RFC3339)})
			h.pendingIdx = len(h.chatHistory) - 1
			msg = &h.chatHistory[h.pendingIdx]
		}
		msg.ReasoningContent += content
		h.persistChatLocked()
		h.chatMu.Unlock()
	})
	h.sdk.Subscribe(sdk.EventAgentOutput, func(ev *sdk.Event) {
		content, _ := ev.Payload["content"].(string)
		channel, _ := ev.Payload["channel"].(string)
		kind, _ := ev.Payload["kind"].(string)
		h.chatMu.Lock()
		// 输出通道主动输出(output_send__{通道})作为独立气泡,不并入最终回复
		if kind == "channel_output" {
			h.pendingIdx = -1
			h.chatMu.Unlock()
			if content == "" {
				return
			}
			h.addChatMsg(ChatMsg{
				Role:    "assistant",
				Content: content,
				Source:  channel,
				Time:    time.Unix(ev.Timestamp, 0).Format(time.RFC3339),
			})
			return
		}
		if msg := h.pendingAssistantLocked(); msg != nil && content != "" {
			msg.Content = content
			if channel != "" {
				msg.Source = channel
			}
			h.pendingIdx = -1
			h.persistChatLocked()
			h.chatMu.Unlock()
			return
		}
		h.pendingIdx = -1
		h.chatMu.Unlock()
		if content == "" {
			return
		}
		h.addChatMsg(ChatMsg{
			Role:    "assistant",
			Content: content,
			Source:  channel,
			Time:    time.Unix(ev.Timestamp, 0).Format(time.RFC3339),
		})
	})
}

// pendingAssistantLocked 返回 chatHistory 中当前进行中的 assistant 消息（已持有 chatMu）。
// 仅当最后一条是 assistant 且尚未产出最终内容时视为进行中，避免跨轮次误合并。
func (h *Handler) pendingAssistantLocked() *ChatMsg {
	if h.pendingIdx < 0 || h.pendingIdx >= len(h.chatHistory) {
		return nil
	}
	msg := &h.chatHistory[h.pendingIdx]
	if msg.Role != "assistant" || msg.Content != "" {
		return nil
	}
	return msg
}

func (h *Handler) persistChatLocked() {
	if h.settings == nil {
		return
	}
	b, _ := json.Marshal(h.chatHistory)
	_ = h.settings.Set("chathistory", string(b))
}

func (h *Handler) handleToolEvent(ev *sdk.Event) {
	payload := ev.Payload
	tool, _ := payload["tool"].(string)
	args, _ := payload["args"].(map[string]interface{})
	status, _ := payload["status"].(string)
	ts := time.Now()

	switch tool {
	case "cmd_run":
		exec := CmdExec{
			Command: getStr(args, "command"),
			Status:  status,
			Time:    ts.Format(time.RFC3339),
		}
		h.cmdMu.Lock()
		h.cmdHistory = append(h.cmdHistory, exec)
		if len(h.cmdHistory) > maxCmdHistory {
			h.cmdHistory = h.cmdHistory[len(h.cmdHistory)-maxCmdHistory:]
		}
		h.cmdMu.Unlock()

	case "terminal_create":
		id := getStr(args, "id")
		if id == "" {
			// agent 调用时不知道生成的 id，从工具结果中回填
			if res, ok := payload["result"].(map[string]interface{}); ok {
				id = getStr(res, "id")
			}
		}
		if id == "" {
			break
		}
		cmd := getStr(args, "command")
		if cmd == "" {
			if res, ok := payload["result"].(map[string]interface{}); ok {
				cmd = getStr(res, "command")
			}
		}
		now := time.Now()
		term := &termState{
			ID:        id,
			Command:   cmd,
			Running:   true,
			CreatedAt: now.Format(time.RFC3339),
			created:   now,
		}
		h.termMu.Lock()
		if old, ok := h.termStates[id]; ok {
			old.Command = cmd
			old.Running = true
			old.created = now
		} else {
			h.termStates[id] = term
		}
		if len(h.termStates) > maxTerminals {
			for k := range h.termStates {
				delete(h.termStates, k)
				break
			}
		}
		h.termMu.Unlock()

	case "terminal_close":
		id := getStr(args, "id")
		if id != "" {
			h.termMu.Lock()
			if t, ok := h.termStates[id]; ok {
				t.Running = false
			}
			h.termMu.Unlock()
		}
	}
}

func getStr(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
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
	mux.HandleFunc("/api/v1/chat/history", h.requireAPI(h.handleChatHistory))
	mux.HandleFunc("/api/v1/chat/events", h.requireAPI(h.handleChatEvents))
	mux.HandleFunc("/api/v1/terminals", h.requireAPI(h.handleTerminals))
	mux.HandleFunc("/api/v1/cmd/history", h.requireAPI(h.handleCmdHistory))
	mux.HandleFunc("/api/v1/kernel", h.requireAPI(h.handleKernel))
	mux.HandleFunc("/api/v1/plugins", h.requireAPI(h.handlePlugins))
	mux.HandleFunc("/api/v1/plugins/", h.requireAPI(h.handlePluginByID))
	// 设备网关（可配置反代到 remotedevice；默认禁用，未启用时返回 404）
	mux.HandleFunc("/api/v1/device/", h.requireAPI(h.handleDeviceGatewayProxy))
	mux.HandleFunc("/v1/chat/completions", h.requireAPI(h.handleOpenAICompletions))
	mux.HandleFunc("/", h.requireWeb(h.handleStatic))
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

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agentCount := 0
	if h.supervisor != nil {
		agentCount = len(h.supervisor.ListAgents())
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":    "running",
		"uptime":    time.Since(h.startTime).Round(time.Second).String(),
		"agents":    agentCount,
		"version":   sdk.SDKVersion,
		"startedAt": h.startTime,
	})
}

func (h *Handler) handleKernel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.status == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "kernel status provider not available"})
		return
	}
	writeJSON(w, http.StatusOK, h.status.GetKernelStatus())
}

func (h *Handler) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if h.supervisor == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "supervisor not available"})
			return
		}
		agents := h.supervisor.ListAgents()
		writeJSON(w, http.StatusOK, map[string]interface{}{"agents": agents})
	case http.MethodPost:
		var cfg types.AgentConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if cfg.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent id is required"})
			return
		}
		if h.config != nil {
			kcfg := h.config.Get()
			kcfg.Agents = append(kcfg.Agents, cfg)
			h.config.Put(kcfg)
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": string(cfg.ID)})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/agents/")
	parts := strings.Split(path, "/")
	agentID := types.AgentID(parts[0])
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			if h.supervisor == nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "supervisor not available"})
				return
			}
			status, err := h.supervisor.GetAgentStatus(string(agentID))
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, status)
		case http.MethodDelete:
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "id": string(agentID)})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	action := parts[1]
	switch action {
	case "snapshots":
		h.handleSnapshots(w, r, agentID, parts)
	case "rollback":
		h.handleRollback(w, r, agentID, parts)
	case "start", "stop", "restart":
		h.handleAgentAction(w, r, agentID, action)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (h *Handler) handleSnapshots(w http.ResponseWriter, r *http.Request, agentID types.AgentID, parts []string) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"agent_id": agentID, "snapshots": []map[string]interface{}{}})
	case http.MethodPost:
		if h.supervisor == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "supervisor not available"})
			return
		}
		snap, err := h.supervisor.PreActionSnapshot(string(agentID))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, snap)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleRollback(w http.ResponseWriter, r *http.Request, agentID types.AgentID, parts []string) {
	if r.Method != http.MethodPost || len(parts) < 3 {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.supervisor == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "supervisor not available"})
		return
	}
	snapID := types.SnapshotID(parts[2])
	if err := h.supervisor.RollbackAgent(string(agentID), string(snapID)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rollback_initiated", "agent": string(agentID), "snap": string(snapID)})
}

func (h *Handler) handleAgentAction(w http.ResponseWriter, r *http.Request, agentID types.AgentID, action string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": fmt.Sprintf("agent %s action not implemented by supervisor", action)})
}

func (h *Handler) handleMemory(w http.ResponseWriter, r *http.Request) {
	if h.memory == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory system not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		userInput := r.URL.Query().Get("q")
		keywords := strings.Split(userInput, ",")
		depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
		if depth <= 0 {
			depth = 2
		}
		entities, relations, err := h.memory.Recall(keywords, depth)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"entities": entities, "relations": relations})
	case http.MethodPost:
		var req struct {
			Triples []sdk.Triple `json:"triples"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if err := h.memory.Commit(req.Triples); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{"status": "committed", "committed": len(req.Triples)})
	case http.MethodDelete:
		var req struct {
			Criteria map[string]string `json:"criteria"`
			Mode     string            `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		deleted, err := h.memory.Purge(req.Criteria, req.Mode)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleMemoryContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.indexer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "indexer not available"})
		return
	}
	userInput := r.URL.Query().Get("q")
	injected, err := h.indexer.BuildContext(userInput)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"context":        h.indexer.FormatContext(injected),
		"summary":        injected.Summary,
		"entities":       injected.Entities,
		"token_estimate": injected.TokenEstimate,
		"tool_prompt":    h.indexer.BuildToolPrompt(),
	})
}

func (h *Handler) handleMemoryTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.indexer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "indexer not available"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tools":       h.indexer.GetToolDefinitions(),
		"tool_prompt": h.indexer.BuildToolPrompt(),
	})
}

func (h *Handler) handleMemoryGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.memory == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory system not available"})
		return
	}
	data, err := h.memory.GraphData()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": data})
}

func (h *Handler) handleKnowledge(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if h.knowledge == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge not available"})
			return
		}
		query := r.URL.Query().Get("q")
		if query != "" {
			results, err := h.knowledge.Search(query, 10)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
			return
		}
		categories, err := h.knowledge.List()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"categories": categories,
			"stats":      h.knowledge.Stats(),
		})

	case http.MethodPost:
		if h.knowledge == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge not available"})
			return
		}
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "multipart/form-data") {
			if err := r.ParseMultipartForm(10 << 20); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			name := r.FormValue("name")
			file, _, err := r.FormFile("file")
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file required"})
				return
			}
			defer file.Close()
			buf := make([]byte, 10<<20)
			n, _ := file.Read(buf)
			content := string(buf[:n])
			if err := h.knowledge.Add(name, content); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]string{"status": "created", "name": name})
			return
		}

		var req struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if req.Name == "" || req.Content == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and content required"})
			return
		}
		if err := h.knowledge.Add(req.Name, req.Content); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "created", "name": req.Name})

	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name query param required"})
			return
		}
		if err := h.knowledge.Remove(name); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "name": name})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleTextMemory(w http.ResponseWriter, r *http.Request) {
	if h.textMem == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "text memory not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		recent, _ := h.textMem.RecentEvents(50)
		stats := h.textMem.Stats()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"stats":  stats,
			"recent": recent,
		})
	case http.MethodDelete:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "not_implemented"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleAdapters(w http.ResponseWriter, r *http.Request) {
	if h.adapter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "lua vm not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"adapters": h.adapter.List()})
	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if err := h.adapter.Load(req.Name, req.Code); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "loaded", "name": req.Name})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleAdapterByID(w http.ResponseWriter, r *http.Request) {
	if h.adapter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "lua vm not available"})
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/adapters/")
	if name == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		for _, a := range h.adapter.List() {
			if a.Name == name {
				writeJSON(w, http.StatusOK, a)
				return
			}
		}
		http.NotFound(w, r)
	case http.MethodDelete:
		if err := h.adapter.Remove(name); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "adapter not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "name": name})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleNetwork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"network_status": "monitoring",
		"endpoints":      h.config.Get().Defaults.LLMEndpoints,
	})
}

// chatSaveThrottle 控制写盘频率：最多每 3 秒写一次
const chatSaveThrottle = 3 * time.Second

func (h *Handler) addChatMsg(msg ChatMsg) {
	h.chatMu.Lock()
	h.chatHistory = append(h.chatHistory, msg)
	if len(h.chatHistory) > maxChatHistory {
		drop := len(h.chatHistory) - maxChatHistory
		h.chatHistory = h.chatHistory[drop:]
		if h.pendingIdx >= 0 {
			h.pendingIdx -= drop
			if h.pendingIdx < 0 {
				h.pendingIdx = -1
			}
		}
	}
	h.persistChatLocked()
	h.chatMu.Unlock()
}

func (h *Handler) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	h.chatMu.Lock()
	result := make([]ChatMsg, len(h.chatHistory))
	copy(result, h.chatHistory)
	h.chatMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"messages": result})
}

func (h *Handler) handleTerminals(w http.ResponseWriter, r *http.Request) {
	h.termMu.Lock()
	terms := make([]*termState, 0, len(h.termStates))
	for _, ts := range h.termStates {
		ts.Uptime = time.Since(ts.created).Round(time.Second).String()
		terms = append(terms, ts)
	}
	h.termMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"terminals": terms})
}

func (h *Handler) handleCmdHistory(w http.ResponseWriter, r *http.Request) {
	h.cmdMu.Lock()
	result := make([]CmdExec, len(h.cmdHistory))
	copy(result, h.cmdHistory)
	h.cmdMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"history": result})
}

func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Message    string `json:"message"`
		DeviceID   string `json:"device_id"`   // 消息来源设备（GUI/受控设备），可选
		DeviceName string `json:"device_name"` // 设备显示名，可选
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if body.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	if h.sdk == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent unavailable"})
		return
	}
	// 来源编码：带设备身份时用 webui/{device_id}（agent 经 injectSourceContext 可见来源）；
	// 无设备时保持 webui（兼容旧调用）。device_name 一并注入便于 agent 识别。
	source := "webui"
	if body.DeviceID != "" {
		source = "webui/" + body.DeviceID
	}
	payload := map[string]interface{}{"content": body.Message}
	if body.DeviceID != "" {
		payload["device_id"] = body.DeviceID
		payload["device_name"] = body.DeviceName
	}
	// 带超时的上下文，防止 InjectTextSync 长时间阻塞 HTTP 请求
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	respCh := make(chan *agentIO.OutputEvent, 1)
	go func() {
		respCh <- h.sdk.InjectInputSync(source, "webui", "text", payload)
	}()

	var resp *agentIO.OutputEvent
	select {
	case resp = <-respCh:
	case <-ctx.Done():
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "agent timeout (60s)"})
		return
	}

	if resp == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent unavailable"})
		return
	}
	content, _ := resp.Payload["content"].(string)
	reasoning, _ := resp.Payload["reasoning_content"].(string)
	result := map[string]interface{}{
		"response": content,
	}
	if reasoning != "" {
		result["reasoning_content"] = reasoning
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleChatEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	done := r.Context().Done()
	if h.sdk == nil {
		fmt.Fprintf(w, "event: error\ndata: {\"msg\":\"event bus unavailable\"}\n\n")
		flusher.Flush()
		return
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	writeCh := make(chan string, 64)
	defer close(writeCh)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[SSE] writer panic: %v", r)
			}
		}()
		for line := range writeCh {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}()

	log.Printf("[SSE] handler started, subscribing to events")

	// 解析 Last-Event-ID（断线重连时客户端携带）
	lastEventID := r.Header.Get("Last-Event-ID")
	if lastEventID != "" {
		log.Printf("[SSE] client reported Last-Event-ID: %s", lastEventID)
	}

	subTypes := []string{"agent_output", "reasoning", "agent_error", "tool_call", "stage", "agent_llm_chain", "terminal_output"}
	var unsubs []func()
	var seq int64
	for _, t := range subTypes {
		t2 := t
		unsub := h.sdk.Subscribe(sdk.EventType(t2), func(evt *sdk.Event) {
			if evt.Type == sdk.EventToolCall {
				toolName, _ := evt.Payload["tool"].(string)
				log.Printf("[SSE] received tool_call event: tool=%s", toolName)
			}
			data, _ := json.Marshal(evt)
			seq++
			select {
			case writeCh <- fmt.Sprintf("id: %d-%d\nevent: %s\ndata: %s\n", evt.Timestamp, seq, evt.Type, string(data)):
				if evt.Type == sdk.EventToolCall {
					toolName, _ := evt.Payload["tool"].(string)
					log.Printf("[SSE] wrote tool_call to writeCh: tool=%s", toolName)
				}
			default:
				log.Printf("[SSE] DROPPED event %s (writeCh full, len=%d)", evt.Type, len(writeCh))
			}
		})
		unsubs = append(unsubs, unsub)
	}
	defer func() {
		for _, unsub := range unsubs {
			unsub()
		}
	}()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			// 心跳直接写 w 并 flush（绕过 writeCh，事件密集/队列满时也能保活长连接，
			// 避免远程 nginx 网关因长时间无字节而 504/半开）。
			if _, err := fmt.Fprintf(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	if h.config == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "config not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, h.config.Get())
	case http.MethodPut:
		var cfg types.Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid config"})
			return
		}
		h.config.Put(&cfg)
		writeJSON(w, http.StatusOK, map[string]string{"status": "config_updated"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	if h.settings == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "config registry not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		prefix := r.URL.Query().Get("prefix")
		values := make(map[string]interface{})
		meta := make(map[string]*sdk.ConfigDef)

		if strings.HasPrefix(prefix, "plugin.") {
			// 插件配置：从插件自身 config_<name> 表读取
			pluginName := prefix[7:]
			keys, _ := h.settings.ListPlugin(pluginName, "")
			for _, k := range keys {
				v, _ := h.settings.GetPlugin(pluginName, k)
				fullKey := prefix + "." + k
				values[fullKey] = v
			}
			for _, def := range h.settings.DefsPlugin(pluginName, "") {
				fullKey := prefix + "." + def.Key
				meta[fullKey] = def
			}
		} else {
			// 核心配置：从 core config 表读取（键可为任意前缀，如 core.llm.*、webui.*）
			all := h.settings.Dump()
			var keys []string
			for k := range all {
				if strings.HasPrefix(k, prefix) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				values[k] = all[k]
			}
			for _, d := range h.settings.DefsCore(prefix) {
				meta[d.Key] = d
				// 有 def 但 DB 中尚无值的 key，用 default 填充以便在 WebUI 中显示和编辑
				if _, exists := values[d.Key]; !exists {
					values[d.Key] = d.Default
				}
			}
			// 无前缀时同时加载所有插件配置
			if prefix == "" {
				for _, p := range h.settings.Plugins() {
					if p == "core" {
						continue
					}
					pkeys, _ := h.settings.ListPlugin(p, "")
					for _, k := range pkeys {
						v, _ := h.settings.GetPlugin(p, k)
						fullKey := "plugin." + p + "." + k
						values[fullKey] = v
					}
					for _, def := range h.settings.DefsPlugin(p, "") {
						fullKey := "plugin." + p + "." + def.Key
						meta[fullKey] = def
					}
				}
			}
		}

		plugins := []string{"core"}
		for _, p := range h.settings.Plugins() {
			if p != "core" {
				plugins = append(plugins, "plugin."+p)
			}
		}
		var pm map[string]sdk.PluginMeta
		if h.pluginMgr != nil {
			pm = h.pluginMgr.PluginMetas()
		}
		var disabledPlugins []sdk.DisabledPluginInfo
		if h.pluginMgr != nil {
			disabledPlugins = h.pluginMgr.ListDisabledPlugins()
		}
		sort.Strings(plugins)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"settings":         values,
			"meta":             meta,
			"plugins":          plugins,
			"plugin_meta":      pm,
			"disabled_plugins": disabledPlugins,
		})
	case http.MethodPut:
		var body struct {
			Key   string      `json:"key"`
			Value interface{} `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		// 整数型 float64（如 QQ 号 2.198972886e+09）规范化为 int64，
		// 避免被 fmt.Sprint 以科学计数法存库导致后续读取/解析失败。
		if f, ok := body.Value.(float64); ok && f == math.Trunc(f) && math.Abs(f) < 1e15 {
			body.Value = int64(f)
		}
		deleting := body.Value == nil
		if strings.HasPrefix(body.Key, "plugin.") {
			parts := strings.SplitN(body.Key, ".", 3)
			if len(parts) >= 3 {
				var err error
				if deleting {
					err = h.settings.RemovePlugin(parts[1], parts[2])
				} else {
					err = h.settings.SetPlugin(parts[1], parts[2], body.Value)
				}
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
					return
				}
			}
		} else {
			var err error
			if deleting {
				err = h.settings.RemoveCore(body.Key)
			} else {
				err = h.settings.SetCore(body.Key, body.Value)
			}
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
		if strings.HasPrefix(body.Key, "core.llm.") && h.llm != nil {
			if err := h.llm.ReloadFromConfig(); err != nil {
				log.Printf("[webui] failed to reload LLM providers: %v", err)
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleOpenAICompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.sdk == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "IO manager not available"})
		return
	}

	var req struct {
		Model       string          `json:"model"`
		Messages    []openAIMessage `json:"messages"`
		Stream      bool            `json:"stream"`
		Temperature float64         `json:"temperature"`
		MaxTokens   int             `json:"max_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "messages is required"})
		return
	}

	lastMsg := req.Messages[len(req.Messages)-1]
	if lastMsg.Role != "user" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "last message must be from user"})
		return
	}

	// 带超时的上下文
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	respCh := make(chan *agentIO.OutputEvent, 1)
	go func() {
		respCh <- h.sdk.InjectTextSync("http", "http", lastMsg.Content)
	}()

	var response *agentIO.OutputEvent
	select {
	case response = <-respCh:
	case <-ctx.Done():
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "agent timeout (60s)"})
		return
	}

	if response == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no response from agent"})
		return
	}

	content, _ := response.Payload["content"].(string)
	reasoningContent, _ := response.Payload["reasoning_content"].(string)
	usage, _ := response.Payload["usage"].(map[string]interface{})

	if req.Stream {
		h.writeOpenAIStream(w, req.Model, content, reasoningContent, usage)
		return
	}

	resp := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
	}
	if reasoningContent != "" {
		resp["choices"].([]map[string]interface{})[0]["message"].(map[string]interface{})["reasoning_content"] = reasoningContent
	}
	if usage != nil {
		resp["usage"] = usage
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) writeOpenAIStream(w http.ResponseWriter, model, content, reasoningContent string, usage map[string]interface{}) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 如果有 reasoning_content，先发送一个 reasoning chunk
	if reasoningContent != "" {
		reasoningChunk := map[string]interface{}{
			"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"delta": map[string]interface{}{
						"content":           "",
						"reasoning_content": reasoningContent,
					},
					"finish_reason": nil,
				},
			},
		}
		data, _ := json.Marshal(reasoningChunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// content chunk
	contentChunk := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"content": content,
				},
				"finish_reason": nil,
			},
		},
	}
	data, _ := json.Marshal(contentChunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	// finish chunk
	finishChunk := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "stop",
			},
		},
	}
	if usage != nil {
		finishChunk["usage"] = usage
	}
	data, _ = json.Marshal(finishChunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (h *Handler) handleTracker(w http.ResponseWriter, r *http.Request) {
	if h.tracker == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "tracker not available"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/tracker")
	path = strings.TrimPrefix(path, "/")

	switch {
	case path == "changesets" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"changesets": h.tracker.ChangeSets(),
			"count":      len(h.tracker.ChangeSets()),
		})
	case path == "rollback" && r.Method == http.MethodPost:
		if err := h.tracker.Rollback(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "rollback_complete"})
	case path == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"stats":       h.tracker.Stats(),
			"has_changes": h.tracker.HasChanges(),
			"changesets":  len(h.tracker.ChangeSets()),
		})
	case path == "" && r.Method == http.MethodDelete:
		if err := h.tracker.Rollback(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// ======== Remote Device Gateway (proxied to remotedevice, opt-in) ========

// deviceGatewayEnabled / deviceGatewayAddr 由 webui 插件启动时从设置读取并注入。
// 默认禁用：用户显式配置 device_gateway_enabled=true 后，/api/v1/device/* 才会反代到
// remotedevice 插件（self-contained），避免与 remotedevice 耦合。
var (
	deviceGatewayEnabled bool
	deviceGatewayAddr    string
	deviceGatewayToken   string
)

// handleDeviceGatewayProxy 将 /api/v1/device/* 反代到 remotedevice 内部 HTTP 服务。
// 鉴权：本端走 requireAPI（webui API key），转发时带 remotedevice 的 token（X-API-Key）。
// WS 升级请求（Upgrade: websocket）走 hijack 双向字节透传（标准库 http.Client 不支持 101 升级）。
func (h *Handler) handleDeviceGatewayProxy(w http.ResponseWriter, r *http.Request) {
	if !deviceGatewayEnabled {
		http.NotFound(w, r)
		return
	}
	addr := deviceGatewayAddr
	if addr == "" {
		addr = "127.0.0.1:9890"
	}
	path := r.URL.Path // 保留 /api/v1/device/... 全路径
	url := "http://" + addr + path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}

	// WebSocket 升级：hijack 双向透传（支持 WS over 远程 homed）
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		h.proxyWebSocket(w, r, addr, path)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header = r.Header.Clone()
	if deviceGatewayToken != "" {
		req.Header.Set("X-API-Key", deviceGatewayToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "device gateway unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// proxyWebSocket 用 TCP 直连 + hijack 将客户端 WS 连接双向透传到设备网关。
func (h *Handler) proxyWebSocket(w http.ResponseWriter, r *http.Request, addr, path string) {
	// 设备网关默认仅监听 127.0.0.1（remotedevice），反代目标即内网 homed 本机或指定 addr。
	// 用 net.Dial 直连网关并手动发起 WS 升级握手（net/http 客户端不支持 ws:// 升级）。
	host, port := addr, "9890"
	if h2, p2, ok := splitHostPort(addr); ok {
		host, port = h2, p2
	}
	target := net.JoinHostPort(host, port)
	upConn, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		http.Error(w, "ws upstream dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upConn.Close()

	// 手动构造 WS 升级请求（保留客户端头 + 注入网关 token）
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		key = "homeagent-proxy-random-key"
	}
	reqPath := path
	if r.URL.RawQuery != "" {
		reqPath += "?" + r.URL.RawQuery
	}
	var b strings.Builder
	b.WriteString("GET " + reqPath + " HTTP/1.1\r\n")
	b.WriteString("Host: " + addr + "\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	if deviceGatewayToken != "" {
		b.WriteString("X-API-Key: " + deviceGatewayToken + "\r\n")
	}
	for k, vv := range r.Header {
		kl := strings.ToLower(k)
		if kl == "upgrade" || kl == "connection" || kl == "sec-websocket-key" || kl == "sec-websocket-version" || kl == "host" || kl == "x-api-key" || kl == "authorization" {
			continue
		}
		for _, v := range vv {
			b.WriteString(k + ": " + v + "\r\n")
		}
	}
	b.WriteString("\r\n")
	if _, err := upConn.Write([]byte(b.String())); err != nil {
		http.Error(w, "ws upstream write: "+err.Error(), http.StatusBadGateway)
		return
	}

	// 读上游 101 响应
	br := bufio.NewReader(upConn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		http.Error(w, "ws upstream response: "+err.Error(), http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		http.Error(w, "ws upstream status: "+resp.Status, http.StatusBadGateway)
		return
	}

	// 客户端 hijack：把 101 响应头写给客户端并接管双向连接
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	// 向上游 101 响应头转发给客户端
	if err := resp.Write(brw); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}

	// 双向透传（WS 帧字节不动）：
	// 客户端 -> 上游
	errCh := make(chan struct{}, 2)
	go func() {
		io.Copy(upConn, brw)
		if tc, ok := upConn.(interface{ CloseWrite() error }); ok {
			tc.CloseWrite()
		}
		errCh <- struct{}{}
	}()
	// 上游 -> 客户端
	go func() {
		wb := bufio.NewWriter(clientConn)
		io.Copy(wb, br)
		wb.Flush()
		errCh <- struct{}{}
	}()
	<-errCh
}

// splitHostPort 拆分 addr 为 host/port；无端口时返回 ok=false。
func splitHostPort(addr string) (string, string, bool) {
	if strings.Contains(addr, ":") {
		h, p, err := net.SplitHostPort(addr)
		if err == nil {
			return h, p, true
		}
	}
	return addr, "", false
}

// ======== Plugin Management (proxied to pluginmgr HTTP API) ========

func (h *Handler) pluginmgrAddr() string {
	addr := "127.0.0.1:9876"
	if h.settings == nil {
		return addr
	}
	if v, err := h.settings.GetPlugin("pluginmgr", "http_addr"); err == nil {
		if s, ok := v.(string); ok && s != "" {
			addr = s
		}
	}
	return addr
}

func (h *Handler) proxyToPluginmgr(w http.ResponseWriter, r *http.Request, path string) {
	addr := h.pluginmgrAddr()
	url := "http://" + addr + path
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header = r.Header.Clone()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (h *Handler) handlePlugins(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.proxyToPluginmgr(w, r, "/plugins")
	case http.MethodPost:
		h.proxyToPluginmgr(w, r, "/plugins")
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handlePluginByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/plugins/")
	path = strings.TrimSuffix(path, "/")

	if path == "disabled" && r.Method == http.MethodGet {
		if h.pluginMgr == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin manager not available"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"disabled": h.pluginMgr.ListDisabledPlugins()})
		return
	}

	if path == "reload" && r.Method == http.MethodPost {
		if h.pluginMgr == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin registry not available"})
			return
		}
		if _, err := h.pluginMgr.ReloadPlugins(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
		return
	}

	if idx := strings.LastIndex(path, "/"); idx > 0 {
		name := path[:idx]
		action := path[idx+1:]
		if r.Method == http.MethodPost {
			switch action {
			case "disable":
				if h.pluginMgr == nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin manager not available"})
					return
				}
				if err := h.pluginMgr.DisablePlugin(name, "webui"); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
				return

			case "enable":
				if h.pluginMgr == nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin manager not available"})
					return
				}
				if err := h.pluginMgr.EnablePlugin(name); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, map[string]string{"status": "enabled"})
				return
			}
		}
	}

	switch r.Method {
	case http.MethodGet:
		h.proxyToPluginmgr(w, r, "/plugins/"+path)
	case http.MethodDelete:
		h.proxyToPluginmgr(w, r, "/plugins/"+path)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
