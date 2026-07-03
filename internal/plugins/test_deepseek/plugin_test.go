package test_deepseek

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/plugin/sdk"
)

type mockSettings struct {
	data map[string]string
}

func (m *mockSettings) Get(key string) (interface{}, error) {
	v, ok := m.data[key]
	if !ok {
		return nil, nil
	}
	return v, nil
}
func (m *mockSettings) Set(key string, value interface{}) error {
	m.data[key] = value.(string)
	return nil
}
func (m *mockSettings) List(prefix string) ([]string, error) {
	var keys []string
	for k := range m.data {
		keys = append(keys, k)
	}
	return keys, nil
}

func mockAPI(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test123" {
			t.Fatalf("expected Bearer sk-test123, got %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("expected application/json, got %s", r.Header.Get("Content-Type"))
		}

		var reqBody map[string]interface{}
		json.NewDecoder(r.Body).Decode(&reqBody)
		if reqBody["model"] != "test-model" {
			t.Fatalf("expected test-model, got %v", reqBody["model"])
		}
		if reqBody["stream"] != false {
			t.Fatalf("expected stream=false, got %v", reqBody["stream"])
		}

		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"message": map[string]string{
						"content": "你好！我是 DeepSeek。",
					},
				},
			},
			"usage": map[string]int{
				"prompt_tokens":     10,
				"completion_tokens": 20,
				"total_tokens":      30,
			},
		})
	}))
}

func TestCallDeepSeekSuccess(t *testing.T) {
	ts := mockAPI(t)
	defer ts.Close()

	sett := &mockSettings{data: map[string]string{
		"base_url": ts.URL,
		"model":    "test-model",
		"api_key":  "sk-test123",
	}}

	result, err := callDeepSeek("你好", sett, ts.Client())
	if err != nil {
		t.Fatalf("callDeepSeek: %v", err)
	}

	// 通过 JSON 反序列化验证（避免匿名 struct 类型断言问题）
	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
	if resp["response"] != "你好！我是 DeepSeek。" {
		t.Fatalf("expected response 你好！我是 DeepSeek。, got %v", resp["response"])
	}
	if resp["model"] != "test-model" {
		t.Fatalf("expected model test-model, got %v", resp["model"])
	}
	usage := resp["usage"].(map[string]interface{})
	if usage["total_tokens"].(float64) != 30 {
		t.Fatalf("expected 30 total tokens, got %v", usage["total_tokens"])
	}
}

func TestCallDeepSeekNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer ts.Close()

	sett := &mockSettings{data: map[string]string{
		"base_url": ts.URL,
		"model":    "test-model",
		"api_key":  "sk-bad",
	}}
	result, err := callDeepSeek("hi", sett, ts.Client())
	if err != nil {
		t.Fatalf("callDeepSeek: %v", err)
	}
	m := result.(map[string]interface{})
	if m["status"] != "failed" {
		t.Fatalf("expected status failed, got %v", m["status"])
	}
}

func TestCallDeepSeekDefaultPrompt(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody map[string]interface{}
		json.NewDecoder(r.Body).Decode(&reqBody)
		msgs := reqBody["messages"].([]interface{})
		msg := msgs[0].(map[string]interface{})
		if msg["content"] == "你好，请用一句话介绍你自己" {
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"message": map[string]string{"content": "ok"}},
				},
			})
			return
		}
		t.Fatalf("unexpected prompt: %v", msg["content"])
	}))
	defer ts.Close()

	sett := &mockSettings{data: map[string]string{
		"base_url": ts.URL,
		"model":    "test-model",
		"api_key":  "sk-test",
	}}
	bus := sdk.NewInProcessBus()
	api := NewWithClient(bus, ts.Client())
	api.SetSettings(sett)

	handler := api.Tools()["test_deepseek"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)
	if resp["status"] != "ok" {
		t.Fatalf("expected ok, got %v", resp["status"])
	}
}

func TestCallDeepSeekFallbackAPIKey(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := "Bearer REDACTED_API_KEY"
		if r.Header.Get("Authorization") != expected {
			t.Fatalf("expected %s, got %s", expected, r.Header.Get("Authorization"))
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]string{"content": "ok"}},
			},
		})
	}))
	defer ts.Close()

	// 不设 api_key 触发 fallback
	sett := &mockSettings{data: map[string]string{
		"base_url": ts.URL,
		"model":    "test-model",
	}}
	result, err := callDeepSeek("hi", sett, ts.Client())
	if err != nil {
		t.Fatalf("callDeepSeek: %v", err)
	}
	m := result.(map[string]interface{})
	if m["status"] != "ok" {
		t.Fatalf("expected ok, got %v", m["status"])
	}
}

func TestNewWithClient(t *testing.T) {
	ts := mockAPI(t)
	defer ts.Close()

	sett := &mockSettings{data: map[string]string{
		"base_url": ts.URL,
		"model":    "test-model",
		"api_key":  "sk-test123",
	}}
	bus := sdk.NewInProcessBus()
	api := NewWithClient(bus, ts.Client())
	api.SetSettings(sett)

	handler := api.Tools()["test_deepseek"]
	if handler == nil {
		t.Fatal("test_deepseek tool not registered")
	}

	result, err := handler(map[string]interface{}{"prompt": "你好"})
	if err != nil {
		t.Fatalf("tool handler: %v", err)
	}
	m := result.(map[string]interface{})
	if m["status"] != "ok" {
		t.Fatalf("expected ok, got %v", m["status"])
	}
	if m["response"] != "你好！我是 DeepSeek。" {
		t.Fatalf("expected response, got %v", m["response"])
	}
}

func TestNewDefaultClient(t *testing.T) {
	bus := sdk.NewInProcessBus()
	api := New(bus)
	if api == nil {
		t.Fatal("New returned nil")
	}
	if api.Name != "test_deepseek" {
		t.Fatalf("expected name test_deepseek, got %s", api.Name)
	}
	tools := api.Tools()
	if _, ok := tools["test_deepseek"]; !ok {
		t.Fatal("test_deepseek tool not registered")
	}
}
