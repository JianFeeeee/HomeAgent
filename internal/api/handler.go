package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	luaVM "gitcode.com/JianFeeeee/HomeAgent/internal/lua"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
	"gitcode.com/JianFeeeee/HomeAgent/internal/skill"
	"gitcode.com/JianFeeeee/HomeAgent/internal/supervisor"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

type Handler struct {
	supervisor *supervisor.Daemon
	memory     *memory.GraphDB
	indexer    *memory.Indexer
	skills     *skill.Manager
	lua        *luaVM.VM
	config     *types.Config
	startTime  time.Time
	iom        *agentIO.IOManager
	textMem    *text.Memory
	knowledge  *knowledge.Store
}

func NewHandler(sup *supervisor.Daemon, mem *memory.GraphDB, sk *skill.Manager, lua *luaVM.VM, cfg *types.Config, iom *agentIO.IOManager, tm *text.Memory, ks *knowledge.Store) *Handler {
	var idx *memory.Indexer
	if mem != nil {
		idx = memory.NewIndexer(mem)
	}
	return &Handler{
		supervisor: sup,
		memory:     mem,
		indexer:    idx,
		skills:     sk,
		lua:        lua,
		config:     cfg,
		startTime:  time.Now(),
		iom:        iom,
		textMem:    tm,
		knowledge:  ks,
	}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/status", h.handleStatus)
	mux.HandleFunc("/api/v1/agents", h.handleAgents)
	mux.HandleFunc("/api/v1/agents/", h.handleAgentByID)
	mux.HandleFunc("/api/v1/skills", h.handleSkills)
	mux.HandleFunc("/api/v1/memory", h.handleMemory)
	mux.HandleFunc("/api/v1/memory/", h.handleMemory)
	mux.HandleFunc("/api/v1/memory/context", h.handleMemoryContext)
	mux.HandleFunc("/api/v1/memory/tools", h.handleMemoryTools)
	mux.HandleFunc("/api/v1/memory/text", h.handleTextMemory)
	mux.HandleFunc("/api/v1/network", h.handleNetwork)
	mux.HandleFunc("/api/v1/config", h.handleConfig)
	mux.HandleFunc("/api/v1/knowledge", h.handleKnowledge)
	mux.HandleFunc("/api/v1/knowledge/", h.handleKnowledge)
	mux.HandleFunc("/api/v1/adapters", h.handleAdapters)
	mux.HandleFunc("/api/v1/adapters/", h.handleAdapterByID)
	mux.HandleFunc("/v1/chat/completions", h.handleOpenAICompletions)
	mux.HandleFunc("/", h.handleStatic)
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
		"version":   "0.1.0",
		"startedAt": h.startTime,
	})
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
		// future: purge
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

// OpenAI 兼容 API — 所有输入走 IO 抽象层（中断）
func (h *Handler) handleOpenAICompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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

	// 取最后一条 user 消息作为输入
	lastMsg := req.Messages[len(req.Messages)-1]
	if lastMsg.Role != "user" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "last message must be from user"})
		return
	}

	// 通过 IO 抽象层同步注入（中断式）
	response := h.iom.InjectTextSync("http", lastMsg.Content)

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
					"content": response.Payload["content"],
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     len(lastMsg.Content) / 2,
			"completion_tokens": len(response.Payload["content"].(string)) / 2,
			"total_tokens":      (len(lastMsg.Content) + len(response.Payload["content"].(string))) / 2,
		},
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webuiHTML)
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

var webuiHTML = []byte(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>HomeAgent Dashboard</title>
<style>
*{margin:0;padding:0;box-sizing:border-box;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif}
body{background:#0f172a;color:#e2e8f0;min-height:100vh}
nav{background:#1e293b;padding:12px 24px;display:flex;align-items:center;gap:24px;border-bottom:1px solid #334155}
nav h1{font-size:18px;font-weight:600;color:#38bdf8}
nav a{color:#94a3b8;text-decoration:none;font-size:14px;cursor:pointer}
nav a:hover{color:#38bdf8;text-decoration:none}
nav a.active{color:#38bdf8;border-bottom:2px solid #38bdf8}
.container{padding:24px;max-width:1400px;margin:0 auto}
.card{background:#1e293b;border:1px solid #334155;border-radius:12px;padding:20px;margin-bottom:16px}
.card h2{font-size:16px;font-weight:600;margin-bottom:12px;color:#f1f5f9}
.status-dot{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:8px}
.dot-green{background:#22c55e}
.dot-yellow{background:#eab308}
.dot-red{background:#ef4444}
.grid-2{display:grid;grid-template-columns:1fr 1fr;gap:16px}
.grid-3{display:grid;grid-template-columns:1fr 1fr 1fr;gap:16px}
.stat-value{font-size:28px;font-weight:700;color:#38bdf8}
.stat-label{font-size:12px;color:#64748b;margin-top:4px}
table{width:100%;border-collapse:collapse;font-size:13px}
th{text-align:left;padding:8px 12px;color:#64748b;font-weight:500;border-bottom:1px solid #334155;font-size:12px;text-transform:uppercase}
td{padding:8px 12px;border-bottom:1px solid #1e293b}
.status-badge{display:inline-block;padding:2px 8px;border-radius:4px;font-size:11px;font-weight:500}
.badge-running{background:#166534;color:#86efac}
.badge-stopped{background:#7f1d1d;color:#fca5a5}
.btn{padding:6px 14px;border-radius:6px;border:none;font-size:12px;cursor:pointer;font-weight:500}
.btn-primary{background:#2563eb;color:#fff}
.btn-primary:hover{background:#1d4ed8}
.btn-danger{background:#dc2626;color:#fff}
.btn-sm{padding:4px 10px;font-size:11px}
.tab-content{display:none}
.tab-content.active{display:block}
input,textarea,select{background:#0f172a;border:1px solid #334155;border-radius:6px;padding:8px 12px;color:#e2e8f0;font-size:13px;width:100%;margin-bottom:12px}
label{display:block;font-size:12px;color:#94a3b8;margin-bottom:4px}
h3{font-size:14px;font-weight:600;color:#f1f5f9;margin-bottom:8px}
pre{background:#0f172a;border-radius:6px;padding:12px;font-size:12px;overflow-x:auto;color:#a5b4fc}
</style>
</head>
<body>
<nav>
<h1>🦞 HomeAgent</h1>
<a class="active" onclick="switchTab('overview')">概览</a>
<a onclick="switchTab('memory')">图记忆</a>
<a onclick="switchTab('skills')">技能</a>
<a onclick="switchTab('network')">网络</a>
<a onclick="switchTab('config')">配置</a>
</nav>
<div class="container" id="app">
<div id="tab-overview" class="tab-content active"></div>
<div id="tab-memory" class="tab-content"></div>
<div id="tab-skills" class="tab-content"></div>
<div id="tab-network" class="tab-content"></div>
<div id="tab-config" class="tab-content"></div>
</div>
<script>
let state={status:null};
async function api(p,o={}){const r=await fetch('/api/v1'+p,{headers:{'Content-Type':'application/json',...o.headers},...o});return r.json()}
function switchTab(n){document.querySelectorAll('.tab-content').forEach(e=>e.classList.remove('active'));document.getElementById('tab-'+n).classList.add('active');document.querySelectorAll('nav a').forEach(e=>e.classList.remove('active'));document.querySelector('nav a[onclick*="'+n+'"]')?.classList.add('active');renderAll()}
async function renderAll(){try{state.status=await api('/status')}catch(e){}renderOverview();renderMemory();renderSkills();renderNetwork();renderConfig()}
function renderOverview(){const s=state.status||{};document.getElementById('tab-overview').innerHTML='<div class="grid-3">'+statCard('运行状态',s.status||'unknown')+statCard('运行时间',s.uptime||'-')+statCard('版本',s.version||'-')+'</div>'}
function statCard(l,v){return '<div class="card"><div class="stat-value">'+v+'</div><div class="stat-label">'+l+'</div></div>'}
function renderMemory(){document.getElementById('tab-memory').innerHTML='<div class="card"><h2>图记忆</h2><p style="color:#94a3b8">agent 通过 memory_recall / memory_commit 自动管理</p></div>'}
function renderSkills(){document.getElementById('tab-skills').innerHTML='<div class="card"><h2>技能</h2><p style="color:#94a3b8">SKILL.md 插件通过 IO 层注入</p></div>'}
function renderNetwork(){document.getElementById('tab-network').innerHTML='<div class="card"><h2>网络</h2><p style="color:#94a3b8">LLM API 连通性监控</p></div>'}
function renderConfig(){document.getElementById('tab-config').innerHTML='<div class="card"><h2>配置</h2><pre>'+JSON.stringify(state.status,null,2)+'</pre></div>'}
renderAll();setInterval(renderAll,30000);
</script>
</body>
</html>`)
