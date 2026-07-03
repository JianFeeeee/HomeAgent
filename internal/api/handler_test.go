package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
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

// === 全流程集成测试：ConfigRegistry → WebUI → HTTP API ===

func TestSettingsAPIFlow(t *testing.T) {
	cfgReg := internalConfig.NewConfigRegistry("")
	cfgReg.Register("core.llm.model", "deepseek-v4-flash")
	cfgReg.Register("core.llm.base_url", "https://api.deepseek.com")
	cfgReg.Register("core.daemon.listen_addr", ":8080")
	cfgReg.Register("plugin.qq.access_token", "secret123")

	sup := supervisor.New(&types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	})
	sup.Start()
	defer sup.Shutdown()

	pluginReg := plugin.NewRegistry()
	h := NewHandler(sup, nil, nil, nil, &types.Config{}, nil, nil, nil, nil, cfgReg, pluginReg)

	t.Run("GET_settings_lists_keys_and_plugins", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
		w := httptest.NewRecorder()
		h.handleSettings(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		settings, ok := resp["settings"].(map[string]interface{})
		if !ok {
			t.Fatal("settings not a map")
		}
		if v, _ := settings["core.llm.model"].(string); v != "deepseek-v4-flash" {
			t.Fatalf("expected deepseek-v4-flash, got %v", settings["core.llm.model"])
		}

		plugins, ok := resp["plugins"].([]interface{})
		if !ok || len(plugins) == 0 {
			t.Fatal("expected plugins list")
		}
		if plugins[0] != "core" {
			t.Fatalf("expected first plugin 'core', got %v", plugins[0])
		}
	})

	t.Run("GET_settings_with_prefix", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings?prefix=core.daemon", nil)
		w := httptest.NewRecorder()
		h.handleSettings(w, req)

		var resp map[string]interface{}
		json.NewDecoder(w.Body).Decode(&resp)
		settings := resp["settings"].(map[string]interface{})

		if _, ok := settings["core.daemon.listen_addr"]; !ok {
			t.Fatal("expected core.daemon.listen_addr in filtered results")
		}
		if _, ok := settings["core.llm.model"]; ok {
			t.Fatal("core.llm.model should not be in core.daemon filtered results")
		}
	})

	t.Run("PUT_settings_updates_value", func(t *testing.T) {
		body := `{"key":"core.llm.model","value":"gpt-4"}`
		req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.handleSettings(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		val, err := cfgReg.Get("core.llm.model")
		if err != nil {
			t.Fatalf("Get error: %v", err)
		}
		if v, _ := val.(string); v != "gpt-4" {
			t.Fatalf("expected gpt-4, got %v", val)
		}
	})

	t.Run("PUT_settings_invalid_body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader("not json"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.handleSettings(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})

	t.Run("handleStatic_returns_webui_html", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		h.handleStatic(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		body, _ := io.ReadAll(w.Body)
		html := string(body)
		if !strings.Contains(html, "settings-layout") {
			t.Fatal("HTML should contain settings-layout class")
		}
		if !strings.Contains(html, "settings-sidebar") {
			t.Fatal("HTML should contain settings-sidebar class")
		}
		if !strings.Contains(html, "saveSetting") {
			t.Fatal("HTML should contain saveSetting JS function")
		}
		if !strings.Contains(html, "api('/settings'") {
			t.Fatal("HTML should call api('/settings')")
		}
	})

	t.Run("settings_not_available_without_registry", func(t *testing.T) {
		h2 := NewHandler(sup, nil, nil, nil, &types.Config{}, nil, nil, nil, nil, nil, nil)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
		w := httptest.NewRecorder()
		h2.handleSettings(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
	})
}

func TestSettingsWithPluginRegistry(t *testing.T) {
	cfgReg := internalConfig.NewConfigRegistry("")
	cfgReg.Register("core.test.key", "value")
	cfgReg.Register("plugin.testplug.apikey", "abc123")

	sup := supervisor.New(&types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	})
	sup.Start()
	defer sup.Shutdown()

	pluginReg := plugin.NewRegistry()
	h := NewHandler(sup, nil, nil, nil, &types.Config{}, nil, nil, nil, nil, cfgReg, pluginReg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	w := httptest.NewRecorder()
	h.handleSettings(w, req)

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)

	plugins, _ := resp["plugins"].([]interface{})
	foundCore := false
	for _, p := range plugins {
		if p == "core" {
			foundCore = true
			break
		}
	}
	if !foundCore {
		t.Fatal("expected 'core' in plugins list")
	}
}
