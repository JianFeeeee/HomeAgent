package webui

import (
	"context"
	"encoding/json"
	"io"
	"net"
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

func seedWebUIConfig(cfgReg *internalConfig.ConfigRegistry) {
	webuiCfg := cfgReg.PluginConfig("webui")
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "api_key", Default: ""})
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "username", Default: "admin"})
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "password", Default: ""})
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "session_ttl_hours", Default: "24"})
	webuiCfg.Set("api_key", "test-api-key")
	webuiCfg.Set("username", "admin")
	webuiCfg.Set("password", "secret-pass")
	webuiCfg.Set("session_ttl_hours", "24")
}

func TestAuthMiddleware(t *testing.T) {
	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)

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
	seedWebUIConfig(cfgReg)

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
		if !strings.Contains(html, "settings-tabs") {
			t.Fatal("HTML should contain settings-tabs class")
		}
		if !strings.Contains(html, "saveSetting") {
			t.Fatal("HTML should contain saveSetting JS function")
		}
		if !strings.Contains(html, `api("/settings"`) {
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

func (p *echoProvider) Name() string          { return p.name }
func (p *echoProvider) MaxContextTokens() int { return 8192 }
func (p *echoProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	content := "echo: " + lastUserContent(req.Messages)
	return &agentAPI.CompletionResponse{Content: content, FinishReason: "stop"}, nil
}
func (p *echoProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	// 流契约：chunk 发送完毕后必须 close(channel) 标识流结束（与
	// LuaAdaptedProvider.ChatStream 的 defer close(ch) 一致）；
	// accumulateStream 以 channel 关闭为终止条件，Done 只是 finish_reason 载体。
	// 内容与 Chat() 保持一致，保证端到端断言在流式/非流式两条路径下等价。
	ch := make(chan agentAPI.StreamChunk, 1)
	content := "echo: " + lastUserContent(req.Messages)
	ch <- agentAPI.StreamChunk{Content: content, Done: true, FinishReason: "stop"}
	close(ch)
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

// ===== client_msg_id 去重测试（防 GUI 断线重连消息重放）=====

func TestHandleChatClientMsgIDDedup(t *testing.T) {
	iom := agentIO.NewIOManager()

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

	t.Run("same_client_msg_id_replay_returns_cached_response", func(t *testing.T) {
		body := `{"message":"你好","client_msg_id":"msg-abc-123"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.handleChat(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("first request: expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var first map[string]interface{}
		json.NewDecoder(w.Body).Decode(&first)
		if first["response"] != "echo: 你好" {
			t.Fatalf("expected echo response, got %v", first["response"])
		}

		// 同 ID 重放：应直接复用首次结果，不重复注入 agent
		req2 := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(body))
		w2 := httptest.NewRecorder()
		h.handleChat(w2, req2)
		if w2.Code != http.StatusOK {
			t.Fatalf("replay: expected 200, got %d: %s", w2.Code, w2.Body.String())
		}
		var second map[string]interface{}
		json.NewDecoder(w2.Body).Decode(&second)
		if second["response"] != "echo: 你好" {
			t.Fatalf("replay expected same response, got %v", second["response"])
		}
		if second["deduplicated"] != true {
			t.Fatalf("replay expected deduplicated=true, got %v", second["deduplicated"])
		}
	})

	t.Run("different_client_msg_id_processed_normally", func(t *testing.T) {
		body := `{"message":"第二条","client_msg_id":"msg-def-456"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.handleChat(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["deduplicated"] == true {
			t.Fatal("new msg id should not be deduplicated")
		}
	})

	t.Run("no_client_msg_id_backward_compatible", func(t *testing.T) {
		body := `{"message":"旧客户端消息"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.handleChat(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// ===== agent 核心层内容级去重测试 =====

func TestAgentDuplicateInputDedup(t *testing.T) {
	iom := agentIO.NewIOManager()

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
	})
	agent.Start()
	defer agent.Stop()

	// 直接验证 isDuplicateInput 行为
	if agent.IsDuplicateInput("webui", "重复消息") {
		t.Fatal("first input should not be duplicate")
	}
	if !agent.IsDuplicateInput("webui", "重复消息") {
		t.Fatal("immediate same-content same-source should be duplicate")
	}
	if agent.IsDuplicateInput("webui", "不同消息") {
		t.Fatal("different content should not be duplicate")
	}
	if agent.IsDuplicateInput("qq", "重复消息") {
		t.Fatal("different source should not be duplicate")
	}
}

// TestSettingsNoCrossPluginLeak 锁住设置接口的两类泄漏：
//
//  1. meta 不得把别家插件的 def 复制进来（曾经 28 插件 × ~186 def = 5208 条，
//     96% 重复），也不得出现 plugin.<a>.plugin.<b>.<key> 这种幻影键——
//     按幻影键写回会落到错误插件的配置表里。
//  2. chathistory 是内部数据（生产实测 5.2MB），不得出现在设置响应里，
//     也不得经设置接口写入。
func TestSettingsNoCrossPluginLeak(t *testing.T) {
	cfgReg := internalConfig.NewConfigRegistry("")
	cfgReg.RegisterDef(internalConfig.ConfigDef{Key: "core.agent.max_tool_turns", Default: "10"})

	webuiCfg := cfgReg.PluginConfig("webui")
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "addr", Default: ":8080"})
	webuiCfg.Set("addr", ":8080")
	webuiCfg.Set("chathistory", `[{"role":"assistant","content":"secret blob"}]`)

	qqCfg := cfgReg.PluginConfig("qq")
	qqCfg.RegisterDef(internalConfig.ConfigDef{Key: "access_token", Default: ""})
	qqCfg.Set("access_token", "qq-token")

	sup := supervisor.New(&types.Config{Daemon: types.DaemonConfig{
		CheckInterval: time.Minute, HeartbeatInterval: 30 * time.Second,
	}})
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
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Settings map[string]interface{}            `json:"settings"`
		Meta     map[string]map[string]interface{} `json:"meta"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// 1) meta 里每个键只能出现一次 "plugin." 前缀，且内容与键同源
	for k, v := range resp.Meta {
		if strings.Count(k, "plugin.") > 1 {
			t.Fatalf("meta 出现幻影键：%q", k)
		}
		if inner, ok := v["key"].(string); ok && strings.HasPrefix(inner, "plugin.") {
			t.Fatalf("meta[%q].key 带命名空间前缀（会造成双重前缀）：%q", k, inner)
		}
	}
	// 2) webui 不能看到 qq 的 def，反之亦然
	if _, ok := resp.Meta["plugin.webui.addr"]; !ok {
		t.Fatalf("meta 缺少 plugin.webui.addr：%v", resp.Meta)
	}
	if _, ok := resp.Meta["plugin.webui.access_token"]; ok {
		t.Fatal("meta 里出现了别家插件的 def：plugin.webui.access_token")
	}
	if _, ok := resp.Meta["plugin.qq.access_token"]; !ok {
		t.Fatal("meta 缺少 plugin.qq.access_token")
	}
	if _, ok := resp.Meta["plugin.qq.addr"]; ok {
		t.Fatal("meta 里出现了别家插件的 def：plugin.qq.addr")
	}
	// 3) 内部数据不进设置面
	if _, ok := resp.Settings["plugin.webui.chathistory"]; ok {
		t.Fatal("settings 泄露了 chathistory 内部数据")
	}
	if _, ok := resp.Meta["plugin.webui.chathistory"]; ok {
		t.Fatal("meta 泄露了 chathistory")
	}
	if _, ok := resp.Settings["plugin.qq.access_token"]; !ok {
		t.Fatal("普通插件配置项应照常返回")
	}

	// 4) 内部数据也不可经设置接口写入
	body := `{"key":"plugin.webui.chathistory","value":"tampered"}`
	preq := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	preq.Header.Set("Content-Type", "application/json")
	pw := httptest.NewRecorder()
	h.handleSettings(pw, preq)
	if pw.Code != http.StatusBadRequest {
		t.Fatalf("写内部键应被拒（400），实际 %d", pw.Code)
	}
	got, _ := webuiCfg.Get("chathistory")
	if got != `[{"role":"assistant","content":"secret blob"}]` {
		t.Fatalf("内部数据被改写：%v", got)
	}
}

// TestListenOverrideAndBindFailure 钉住两个曾经静默的缺陷：
//
//  1. CLI --webui / webui.listen_addr 的覆盖必须真的生效（优先级高于插件 settings["addr"]）。
//     修复前内核只在插件设置键为空时才写，而 REGISTERDEF 建表时写的是默认 :8080，
//     覆盖因此永远是死配置。
//  2. 端口被占时 Start 必须返回错误。修复前监听在后台 goroutine 里做，
//     Start 永远返回 nil，WebUI 静默死亡而插件仍被当成加载成功。
func TestListenOverrideAndBindFailure(t *testing.T) {
	// 取一个确定空闲的地址
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)
	// 插件设置里故意放一个别的地址，用来证明覆盖的优先级
	cfgReg.PluginConfig("webui").Set("addr", "127.0.0.1:1")

	s := testSDK(sdk.SDKConfig{
		Settings:  sdk.NewSettings("webui", cfgReg),
		Config:    sdk.NewConfig(&types.Config{}),
		EventBus:  events.NewBus(),
		IOManager: agentIO.NewIOManager(),
	})

	SetListenOverride(addr)
	defer SetListenOverride("")

	p1 := New("webui")
	if err := p1.Start(s); err != nil {
		t.Fatalf("Start with override: %v", err)
	}
	defer p1.Stop()

	// 覆盖值必须真的在监听
	cli := &http.Client{Timeout: 2 * time.Second}
	resp, err := cli.Get("http://" + addr + "/login")
	if err != nil {
		t.Fatalf("覆盖地址未监听（%s）：%v", addr, err)
	}
	resp.Body.Close()

	// 同一地址再来一个插件实例 → 必须同步报错
	p2 := New("webui")
	err = p2.Start(s)
	if err == nil {
		p2.Stop()
		t.Fatal("端口被占用时 Start 应返回错误，而不是静默成功")
	}
	if !strings.Contains(err.Error(), "监听") {
		t.Fatalf("错误信息应说明监听失败，实际：%v", err)
	}
}

func TestResolveListenAddrPrecedence(t *testing.T) {
	SetListenOverride("")
	defer SetListenOverride("")
	if got := resolveListenAddr(""); got != ":8080" {
		t.Fatalf("默认应为 :8080，得到 %q", got)
	}
	if got := resolveListenAddr(":9001"); got != ":9001" {
		t.Fatalf("插件设置应生效，得到 %q", got)
	}
	SetListenOverride("127.0.0.1:9002")
	if got := resolveListenAddr(":9001"); got != "127.0.0.1:9002" {
		t.Fatalf("覆盖值应优先，得到 %q", got)
	}
}
