package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentCore "gitcode.com/JianFeeeee/HomeAgent/internal/agent/core"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"gitcode.com/JianFeeeee/HomeAgent/internal/supervisor"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

func testSDK(cfg sdk.SDKConfig) *sdk.PluginSDK {
	if cfg.EventBus == nil {
		cfg.EventBus = events.NewBus()
	}
	return sdk.New("webui", cfg)
}

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

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Config:     sdk.NewConfig(cfg),
	})
	return NewHandler(s), sup
}

func TestAuthMiddleware(t *testing.T) {
	cfgReg := internalConfig.NewConfigRegistry("")
	cfgReg.PluginConfig("webui").Set("api_key", "test-api-key")
	cfgReg.PluginConfig("webui").Set("username", "admin")
	cfgReg.PluginConfig("webui").Set("password", "secret-pass")
	cfgReg.PluginConfig("webui").Set("session_ttl_hours", "24")

	sup := supervisor.New(&types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	})
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Settings:   sdk.NewSettings("webui", cfgReg),
		Config:     sdk.NewConfig(&types.Config{}),
	})
	h := NewHandler(s)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	t.Run("api_requires_auth", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", w.Code)
		}
	})

	t.Run("api_key_allows_access", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		req.Header.Set("X-API-Key", "test-api-key")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})

	t.Run("login_sets_session_cookie", func(t *testing.T) {
		body := `{"username":"admin","password":"secret-pass"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if len(w.Result().Cookies()) == 0 {
			t.Fatal("expected session cookie")
		}
	})

	t.Run("root_redirects_to_login_without_session", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("expected 302, got %d", w.Code)
		}
	})
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

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Knowledge:  sdk.NewKnowledge(ks),
		Config:     sdk.NewConfig(cfg),
	})
	h := NewHandler(s)

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

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Knowledge:  sdk.NewKnowledge(ks),
		Config:     sdk.NewConfig(cfg),
	})
	h := NewHandler(s)

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

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Tracker:    tr,
		Config:     sdk.NewConfig(cfg),
	})
	h := NewHandler(s)

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
	cfgReg := internalConfig.NewConfigRegistry("")
	cfgReg.PluginConfig("webui").Set("api_key", "test-api-key")
	cfgReg.PluginConfig("webui").Set("username", "admin")
	cfgReg.PluginConfig("webui").Set("password", "secret-pass")

	sup := supervisor.New(&types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	})
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Settings:   sdk.NewSettings("webui", cfgReg),
		Config:     sdk.NewConfig(&types.Config{}),
	})
	h := NewHandler(s)

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
		{"/", http.MethodGet, http.StatusFound},
		{"/api/v1/memory", http.MethodGet, http.StatusServiceUnavailable},
		{"/api/v1/knowledge", http.MethodGet, http.StatusServiceUnavailable},
		{"/api/v1/tracker", http.MethodGet, http.StatusServiceUnavailable},
		{"/api/v1/adapters", http.MethodGet, http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		if strings.HasPrefix(tt.path, "/api/v1/") {
			req.Header.Set("X-API-Key", "test-api-key")
		}
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
	cfgReg.Register("webui.listen_addr", ":8080")
	cfgReg.Register("plugin.qq.access_token", "secret123")

	sup := supervisor.New(&types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	})
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Settings:   sdk.NewSettings("webui", cfgReg),
		Config:     sdk.NewConfig(&types.Config{}),
	})
	h := NewHandler(s)

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
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings?prefix=webui", nil)
		w := httptest.NewRecorder()
		h.handleSettings(w, req)

		var resp map[string]interface{}
		json.NewDecoder(w.Body).Decode(&resp)
		settings := resp["settings"].(map[string]interface{})

		if _, ok := settings["webui.listen_addr"]; !ok {
			t.Fatal("expected webui.listen_addr in filtered results")
		}
		if _, ok := settings["core.llm.model"]; ok {
			t.Fatal("core.llm.model should not be in webui filtered results")
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
		s2 := testSDK(sdk.SDKConfig{
			Supervisor: supervisor.NewSDKAdapter(sup),
			Config:     sdk.NewConfig(&types.Config{}),
		})
		h2 := NewHandler(s2)
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

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Settings:   sdk.NewSettings("webui", cfgReg),
		Config:     sdk.NewConfig(&types.Config{}),
	})
	h := NewHandler(s)

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

// === 端到端测试：Handler + IOManager + Agent + HTTP ===

type echoProvider struct{ name string }

func (p *echoProvider) Name() string { return p.name }
func (p *echoProvider) MaxContextTokens() int { return 8192 }
func (p *echoProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	content := "echo: " + lastUserContent(req.Messages)
	return &agentAPI.CompletionResponse{Content: content, FinishReason: "stop"}, nil
}
func (p *echoProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	ch := make(chan agentAPI.StreamChunk, 1)
	ch <- agentAPI.StreamChunk{Content: "mock", Done: true}
	return ch, nil
}

func lastUserContent(msgs []agentAPI.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

func init() {
	// 避免测试时自动输出
}

func TestHandleCompletionsEndToEnd(t *testing.T) {
	iom := agentIO.NewIOManager()

	// 启动一个最小 Agent，使用 echoProvider（不调真实 LLM）
	memDB, err := memory.NewGraphDB(t.TempDir() + "/graph.db")
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	defer memDB.Close()

	pm := agentAPI.NewProviderManager()
	pm.Register("echo", &echoProvider{name: "echo"})

	agent := agentCore.New(agentCore.AgentConfig{
		ID:              "test",
		SystemPrompt:    "你是测试助手",
		Provider:        &echoProvider{name: "echo"},
		ProviderManager: pm,
		IO:              iom,
		Memory:          memDB,
		Indexer:         nil,
		ContextSavePath: "",
	})
	agent.Start()
	defer agent.Stop()

	sup := supervisor.New(&types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	})
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		IOManager:  iom,
		Config:     sdk.NewConfig(&types.Config{}),
	})
	h := NewHandler(s)

	t.Run("POST_chat_completions_returns_echo", func(t *testing.T) {
		body := `{"model":"test","messages":[{"role":"user","content":"你好"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		h.handleOpenAICompletions(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		choices, ok := resp["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			t.Fatal("expected choices")
		}
		msg, ok := choices[0].(map[string]interface{})["message"].(map[string]interface{})
		if !ok {
			t.Fatal("expected message")
		}
		if msg["content"] != "echo: 你好" {
			t.Fatalf("expected 'echo: 你好', got '%v'", msg["content"])
		}
	})

	t.Run("POST_chat_completions_no_iom_returns_503", func(t *testing.T) {
		s2 := testSDK(sdk.SDKConfig{
			Supervisor: supervisor.NewSDKAdapter(sup),
		})
		h2 := NewHandler(s2)
		body := `{"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h2.handleOpenAICompletions(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", w.Code)
		}
	})

	t.Run("POST_chat_completions_400_on_no_messages", func(t *testing.T) {
		body := `{"model":"test"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.handleOpenAICompletions(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})

	t.Run("POST_chat_completions_400_on_non_user_last_msg", func(t *testing.T) {
		body := `{"messages":[{"role":"assistant","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.handleOpenAICompletions(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})
}
