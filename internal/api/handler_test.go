package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/supervisor"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

func newTestHandler(t *testing.T) (*Handler, *supervisor.Daemon) {
	t.Helper()
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	sup := supervisor.New(cfg)
	sup.Start()

	return NewHandler(sup, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil), sup
}

func TestHandleStatus(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	w := httptest.NewRecorder()
	h.handleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "running" {
		t.Errorf("expected running, got %v", resp["status"])
	}
}

func TestHandleStatusMethodNotAllowed(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/status", nil)
	w := httptest.NewRecorder()
	h.handleStatus(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleAgents(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()
	sup.RegisterAgent("test_agent")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	w := httptest.NewRecorder()
	h.handleAgents(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	agents, ok := resp["agents"].([]interface{})
	if !ok || len(agents) == 0 {
		t.Error("expected agents list")
	}
}

func TestHandleAgentByID(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()
	sup.RegisterAgent("my_agent")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/my_agent", nil)
	w := httptest.NewRecorder()
	h.handleAgentByID(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["id"] != "my_agent" {
		t.Errorf("expected my_agent, got %v", resp["id"])
	}
}

func TestHandleAgentByIDNotFound(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/nonexistent", nil)
	w := httptest.NewRecorder()
	h.handleAgentByID(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleKnowledgeSearch(t *testing.T) {
	ks := knowledge.NewStore(t.TempDir())
	ks.Start()
	ks.Add("test_doc", "this is test content for searching")

	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	sup := supervisor.New(cfg)
	sup.Start()
	defer sup.Shutdown()

	h := NewHandler(sup, nil, nil, nil, cfg, nil, nil, ks, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge?q=test", nil)
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	results, ok := resp["results"].([]interface{})
	if !ok || len(results) == 0 {
		t.Error("expected search results")
	}
}

func TestHandleKnowledgeCreate(t *testing.T) {
	ks := knowledge.NewStore(t.TempDir())
	ks.Start()

	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	sup := supervisor.New(cfg)
	sup.Start()
	defer sup.Shutdown()

	h := NewHandler(sup, nil, nil, nil, cfg, nil, nil, ks, nil, nil, nil)

	body := `{"name":"new_doc","content":"fresh content"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", w.Code)
	}
}

func TestHandleKnowledgeUnavailable(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge?q=test", nil)
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleMemoryUnavailable(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory?q=test", nil)
	w := httptest.NewRecorder()
	h.handleMemory(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleTrackerNotAvailable(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tracker", nil)
	w := httptest.NewRecorder()
	h.handleTracker(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleTrackerStats(t *testing.T) {
	tr := tracker.NewTracker(t.TempDir(), t.TempDir())

	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	sup := supervisor.New(cfg)
	sup.Start()
	defer sup.Shutdown()

	h := NewHandler(sup, nil, nil, nil, cfg, nil, nil, nil, tr, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tracker", nil)
	w := httptest.NewRecorder()
	h.handleTracker(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleOpenAICompletionsNoMessages(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	body := `{"model":"test"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleOpenAICompletions(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleOpenAICompletionsLastMsgNotUser(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	body := `{"messages":[{"role":"assistant","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleOpenAICompletions(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleOpenAICompletionsMethodNotAllowed(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	h.handleOpenAICompletions(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleStaticServesHTML(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.handleStatic(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "HomeAgent Dashboard") {
		t.Error("expected dashboard HTML")
	}
}

func TestHandleStaticNotFound(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	w := httptest.NewRecorder()
	h.handleStatic(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleConfigGet(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	w := httptest.NewRecorder()
	h.handleConfig(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRegisterRoutes(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	tests := []struct {
		path   string
		method string
		code   int
	}{
		{"/api/v1/status", http.MethodGet, http.StatusOK},
		{"/api/v1/agents", http.MethodGet, http.StatusOK},
		{"/api/v1/config", http.MethodGet, http.StatusOK},
		{"/api/v1/network", http.MethodGet, http.StatusOK},
		{"/", http.MethodGet, http.StatusOK},
		{"/api/v1/memory", http.MethodGet, http.StatusServiceUnavailable},
		{"/api/v1/knowledge", http.MethodGet, http.StatusServiceUnavailable},
		{"/api/v1/tracker", http.MethodGet, http.StatusServiceUnavailable},
		{"/api/v1/adapters", http.MethodGet, http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != tt.code {
			t.Errorf("%s %s: expected %d, got %d", tt.method, tt.path, tt.code, w.Code)
		}
	}
}

func TestHandleAdapterByIDNotFound(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/adapters/nonexistent", nil)
	w := httptest.NewRecorder()
	h.handleAdapterByID(w, req)

	// Returns 503 when lua VM is not available
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleAdaptersUnavailable(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/adapters", nil)
	w := httptest.NewRecorder()
	h.handleAdapters(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}
