package webui

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

//go:embed dashboard.html mascot.webp logo.svg
var dashboardFS embed.FS

var dashboardHTML string

const loginHTML = `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1.0"><title>HomeAgent Login</title><style>body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#0f172a;color:#e2e8f0;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}.card{background:#1e293b;border:1px solid #334155;border-radius:12px;padding:28px;width:360px}h1{margin:0 0 16px;font-size:20px;color:#38bdf8}label{display:block;font-size:12px;color:#94a3b8;margin:10px 0 4px}input{width:100%;padding:10px 12px;border-radius:8px;border:1px solid #334155;background:#0f172a;color:#e2e8f0}button{width:100%;margin-top:16px;padding:10px 12px;border:none;border-radius:8px;background:#2563eb;color:#fff;font-weight:600;cursor:pointer}.err{margin-top:12px;color:#fca5a5;font-size:13px}</style></head><body><div class="card"><h1>HomeAgent</h1><form id="login-form"><label>用户名</label><input id="username" autocomplete="username"><label>密码</label><input id="password" type="password" autocomplete="current-password"><button type="submit">登录</button><div id="err" class="err"></div></form></div><script>document.getElementById('login-form').addEventListener('submit',async(e)=>{e.preventDefault();const username=document.getElementById('username').value;const password=document.getElementById('password').value;const err=document.getElementById('err');err.textContent='';const r=await fetch('/api/v1/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username,password})});if(r.ok){location.href='/';return}let data={};try{data=await r.json()}catch(_){}err.textContent=data.error||'登录失败'})</script></body></html>`

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
	cmdMu       sync.Mutex
	cmdHistory  []CmdExec
	termMu      sync.Mutex
	termStates  map[string]*termState
}

type ChatMsg struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Source           string `json:"source,omitempty"`
	Time             string `json:"time"`
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
	}
	h.loadChatHistory()
	if s != nil {
		go h.trackToolEvents()
		h.subscribeChatEvents()
	}
	return h
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
		h.addChatMsg(ChatMsg{
			Role:    "user",
			Content: content,
			Source:  source,
			Time:    time.Unix(ev.Timestamp, 0).Format(time.RFC3339),
		})
	})
	h.sdk.Subscribe(sdk.EventAgentOutput, func(ev *sdk.Event) {
		content, _ := ev.Payload["content"].(string)
		channel, _ := ev.Payload["channel"].(string)
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
		cmd := getStr(args, "command")
		now := time.Now()
		term := &termState{
			ID:        id,
			Command:   cmd,
			Running:   true,
			CreatedAt: now.Format(time.RFC3339),
			created:   now,
		}
		h.termMu.Lock()
		h.termStates[id] = term
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
	writeJSON(w, http.StatusOK, map[string]string{"status": fmt.Sprintf("%s_requested", action), "agent": string(agentID)})
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
			results, _ := h.knowledge.Search(query, 10)
			writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
			return
		}
		categories, _ := h.knowledge.List()
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

func (h *Handler) addChatMsg(msg ChatMsg) {
	h.chatMu.Lock()
	h.chatHistory = append(h.chatHistory, msg)
	if len(h.chatHistory) > maxChatHistory {
		h.chatHistory = h.chatHistory[len(h.chatHistory)-maxChatHistory:]
	}
	// persist to webui config table as compact JSON
	if h.settings != nil {
		b, _ := json.Marshal(h.chatHistory)
		_ = h.settings.Set("chathistory", string(b))
	}
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
		Message string `json:"message"`
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
	resp := h.sdk.InjectTextSync("webui", "webui", body.Message)
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
	subTypes := []string{"agent_output", "reasoning", "agent_error", "tool_call", "stage", "agent_llm_chain"}
	var unsubs []func()
	for _, t := range subTypes {
		t2 := t
		unsub := h.sdk.Subscribe(sdk.EventType(t2), func(evt *sdk.Event) {
			if evt.Type == sdk.EventToolCall {
				toolName, _ := evt.Payload["tool"].(string)
				log.Printf("[SSE] received tool_call event: tool=%s", toolName)
			}
			data, _ := json.Marshal(evt)
			select {
			case writeCh <- fmt.Sprintf("event: %s\ndata: %s\n", evt.Type, string(data)):
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
			select {
			case writeCh <- ": heartbeat":
			default:
			}
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

	response := h.sdk.InjectTextSync("http", "http", lastMsg.Content)
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
