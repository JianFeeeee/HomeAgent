package webui

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	agentCore "gitcode.com/JianFeeeee/HomeAgent/internal/agent/core"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
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

func init() {
	data, err := dashboardFS.ReadFile("dashboard.html")
	if err == nil {
		dashboardHTML = string(data)
	}
}

type Handler struct {
	supervisor    *supervisor.Daemon
	memory        *memory.GraphDB
	indexer       *memory.Indexer
	skills        *skill.Manager
	lua           *luaVM.VM
	config        *types.Config
	startTime     time.Time
	iom           *agentIO.IOManager
	textMem       *text.Memory
	knowledge     *knowledge.Store
	tracker       *tracker.Tracker
	cfgReg        *internalConfig.ConfigRegistry
	pluginReg     *plugin.Registry
	eventBus      *events.Bus
	statusProvider agentCore.StatusProvider
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
	mux.HandleFunc("/api/v1/settings", h.handleSettings)
	mux.HandleFunc("/api/v1/settings/", h.handleSettings)
	mux.HandleFunc("/api/v1/knowledge", h.handleKnowledge)
	mux.HandleFunc("/api/v1/knowledge/", h.handleKnowledge)
	mux.HandleFunc("/api/v1/adapters", h.handleAdapters)
	mux.HandleFunc("/api/v1/adapters/", h.handleAdapterByID)
	mux.HandleFunc("/api/v1/tracker", h.handleTracker)
	mux.HandleFunc("/api/v1/tracker/", h.handleTracker)
	mux.HandleFunc("/api/v1/chat", h.handleChat)
	mux.HandleFunc("/api/v1/chat/events", h.handleChatEvents)
	mux.HandleFunc("/api/v1/kernel", h.handleKernel)
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
		keys := h.cfgReg.List(prefix)
		values := make(map[string]interface{})
		for _, k := range keys {
			v, _ := h.cfgReg.Get(k)
			values[k] = v
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
		if err := h.cfgReg.Set(body.Key, body.Value); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
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
