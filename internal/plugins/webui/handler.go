package webui

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	agentCore "gitcode.com/JianFeeeee/HomeAgent/internal/agent/core"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/meta"
	luaVM "gitcode.com/JianFeeeee/HomeAgent/internal/lua"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	"gitcode.com/JianFeeeee/HomeAgent/internal/skill"
	"gitcode.com/JianFeeeee/HomeAgent/internal/supervisor"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

//go:embed dashboard.html
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
	supervisor     *supervisor.Daemon
	memory         *memory.GraphDB
	indexer        *memory.Indexer
	skills         *skill.Manager
	lua            *luaVM.VM
	config         *types.Config
	startTime      time.Time
	iom            *agentIO.IOManager
	textMem        *text.Memory
	knowledge      *knowledge.Store
	tracker        *tracker.Tracker
	cfgReg         *internalConfig.ConfigRegistry
	pluginReg      *plugin.Registry
	eventBus       *events.Bus
	statusProvider agentCore.StatusProvider
	sessionMu      sync.Mutex
	sessions       map[string]time.Time
}

func NewHandler(sup *supervisor.Daemon, mem *memory.GraphDB, sk *skill.Manager, lua *luaVM.VM, cfg *types.Config, iom *agentIO.IOManager, tm *text.Memory, ks *knowledge.Store, tr *tracker.Tracker, cr *internalConfig.ConfigRegistry, pr *plugin.Registry, evBus *events.Bus, sp agentCore.StatusProvider) *Handler {
	var idx *memory.Indexer
	if mem != nil {
		idx = memory.NewIndexer(mem)
	}
	return &Handler{
		supervisor:     sup,
		memory:         mem,
		indexer:        idx,
		skills:         sk,
		lua:            lua,
		config:         cfg,
		startTime:      time.Now(),
		iom:            iom,
		textMem:        tm,
		knowledge:      ks,
		tracker:        tr,
		cfgReg:         cr,
		pluginReg:      pr,
		eventBus:       evBus,
		statusProvider: sp,
		sessions:       make(map[string]time.Time),
	}
}

func (h *Handler) getWebUIConfig() (apiKey, username, password string, ttl time.Duration) {
	ttl = 24 * time.Hour
	if h.cfgReg == nil {
		return
	}
	ps := h.cfgReg.PluginConfig("webui")
	if v, _ := ps.Get("api_key"); v != nil {
		apiKey, _ = v.(string)
	}
	if v, _ := ps.Get("username"); v != nil {
		username, _ = v.(string)
	}
	if v, _ := ps.Get("password"); v != nil {
		password, _ = v.(string)
	}
	if v, _ := ps.Get("session_ttl_hours"); v != nil {
		switch n := v.(type) {
		case float64:
			if n > 0 { ttl = time.Duration(n) * time.Hour }
		case string:
			if i, err := strconv.Atoi(n); err == nil && i > 0 { ttl = time.Duration(i) * time.Hour }
		}
	}
	if username == "" { username = "admin" }
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
	mux.HandleFunc("/api/v1/skills", h.requireAPI(h.handleSkills))
	mux.HandleFunc("/api/v1/memory", h.requireAPI(h.handleMemory))
	mux.HandleFunc("/api/v1/memory/", h.requireAPI(h.handleMemory))
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
	mux.HandleFunc("/api/v1/chat/events", h.requireAPI(h.handleChatEvents))
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
	var body struct { Username string `json:"username"`; Password string `json:"password"` }
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
	agents := h.supervisor.ListAgents()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":    "running",
		"uptime":    time.Since(h.startTime).String(),
		"agents":    len(agents),
		"version":   meta.Version,
		"startedAt": h.startTime,
	})
}

func (h *Handler) handleKernel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.statusProvider == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "kernel status provider not available"})
		return
	}
	writeJSON(w, http.StatusOK, h.statusProvider.GetKernelStatus())
}

func (h *Handler) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
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
		h.config.Agents = append(h.config.Agents, cfg)
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
			status, err := h.supervisor.GetAgentStatus(agentID)
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
		snap, err := h.supervisor.PreActionSnapshot(agentID)
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
	snapID := types.SnapshotID(parts[2])
	if err := h.supervisor.RollbackAgent(agentID, snapID); err != nil {
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

func (h *Handler) handleSkills(w http.ResponseWriter, r *http.Request) {
	if h.skills == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "skills not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"skills": h.skills.List()})
	case http.MethodPost:
		var req struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if err := h.skills.Install(req.Name, req.Content); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "installed", "name": req.Name})
	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name query param required"})
			return
		}
		if err := h.skills.Uninstall(name); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "uninstalled", "name": name})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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
		result, err := h.memory.Recall(keywords, nil, depth, "")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodPost:
		var req struct {
			Triples   []memory.Triple `json:"triples"`
			SessionID string          `json:"session_id"`
			TurnID    int             `json:"turn_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		ec, rc, err := h.memory.Commit(req.Triples, req.SessionID, req.TurnID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]int{"entities_created": ec, "relations_created": rc})
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
	injected := h.indexer.BuildContext(userInput)
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

func (h *Handler) handleKnowledge(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if h.knowledge == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge not available"})
			return
		}
		query := r.URL.Query().Get("q")
		if query != "" {
			results := h.knowledge.Search(query, 10)
			writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"categories": h.knowledge.List(),
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
	if h.lua == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "lua vm not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"adapters": h.lua.ListAdapters()})
	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		path := fmt.Sprintf("%s/%s.lua", h.lua.AdapterDir(), req.Name)
		if err := os.WriteFile(path, []byte(req.Code), 0644); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if err := h.lua.ReloadAll(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "loaded", "name": req.Name})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleAdapterByID(w http.ResponseWriter, r *http.Request) {
	if h.lua == nil {
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
		for _, a := range h.lua.ListAdapters() {
			if a.Name == name {
				writeJSON(w, http.StatusOK, a)
				return
			}
		}
		http.NotFound(w, r)
	case http.MethodDelete:
		path := fmt.Sprintf("%s/%s.lua", h.lua.AdapterDir(), name)
		if err := os.Remove(path); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "adapter not found"})
			return
		}
		h.lua.ReloadAll()
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
		"endpoints":      h.config.Defaults.LLMEndpoints,
	})
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

	resp := h.iom.InjectTextSync("cli", body.Message)
	if resp == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent unavailable"})
		return
	}
	content, _ := resp.Payload["content"].(string)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"response": content,
	})
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
	if h.eventBus == nil {
		fmt.Fprintf(w, "event: error\ndata: {\"msg\":\"event bus unavailable\"}\n\n")
		flusher.Flush()
		return
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	writeCh := make(chan string, 64)
	defer close(writeCh)

	go func() {
		for line := range writeCh {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}()

	unsub := h.eventBus.Subscribe(events.EventAll, func(evt *events.Event) {
		data, _ := json.Marshal(evt)
		select {
		case writeCh <- fmt.Sprintf("event: %s\ndata: %s", evt.Type, string(data)):
		default:
		}
	})
	defer unsub()

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
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, h.config)
	case http.MethodPut:
		var cfg types.Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid config"})
			return
		}
		h.config = &cfg
		writeJSON(w, http.StatusOK, map[string]string{"status": "config_updated"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	if h.cfgReg == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "config registry not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		prefix := r.URL.Query().Get("prefix")
		values := make(map[string]interface{})
		meta := make(map[string]*internalConfig.ConfigDef)

		if strings.HasPrefix(prefix, "plugin.") {
			// 插件配置：从插件自身 config_<name> 表读取
			pluginName := prefix[7:]
			ps := h.cfgReg.PluginConfig(pluginName)
			keys, _ := ps.List("")
			for _, k := range keys {
				v, _ := ps.Get(k)
				fullKey := prefix + "." + k
				values[fullKey] = v
				if def := h.cfgReg.GetDef(fullKey); def != nil {
					meta[fullKey] = def
				}
			}
		} else {
			// 核心配置：从 core config 表读取
			keys := h.cfgReg.List(prefix)
			for _, k := range keys {
				v, _ := h.cfgReg.Get(k)
				values[k] = v
			}
			defs := h.cfgReg.ListDefs(prefix)
			for _, d := range defs {
				meta[d.Key] = d
			}
		}

		plugins := []string{"core"}
		if h.pluginReg != nil {
			for _, p := range h.pluginReg.List() {
				plugins = append(plugins, "plugin."+p)
			}
		}
		sort.Strings(plugins)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"settings": values,
			"meta":     meta,
			"plugins":  plugins,
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
		if strings.HasPrefix(body.Key, "plugin.") {
			parts := strings.SplitN(body.Key, ".", 3)
			if len(parts) >= 3 {
				ps := h.cfgReg.PluginConfig(parts[1])
				if err := ps.Set(parts[2], body.Value); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
					return
				}
			}
		} else {
			if err := h.cfgReg.Set(body.Key, body.Value); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
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

	if h.iom == nil {
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

	response := h.iom.InjectTextSync("http", lastMsg.Content)
	if response == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no response from agent"})
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
				"index": 0,
				"delta": map[string]interface{}{},
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
	if h.cfgReg == nil {
		return addr
	}
	ps := h.cfgReg.PluginConfig("pluginmgr")
	if v, err := ps.Get("http_addr"); err == nil {
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

	if path == "reload" && r.Method == http.MethodPost {
		if h.pluginReg == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin registry not available"})
			return
		}
		dir := ""
		if h.cfgReg != nil {
			if v, _ := h.cfgReg.Get("core.plugin.dir"); v != nil {
				dir, _ = v.(string)
			}
		}
		if _, err := h.pluginReg.Reload(dir); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
		return
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
		w.Write([]byte(dashboardHTML))
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
