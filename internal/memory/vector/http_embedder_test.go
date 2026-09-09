package vector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPEmbedder_RequiresEndpoint(t *testing.T) {
	_, err := NewHTTPEmbedder(HTTPEmbedderConfig{Dimension: 512})
	if err == nil {
		t.Fatal("应拒绝空 endpoint")
	}
}

func TestHTTPEmbedder_RequiresDimension(t *testing.T) {
	_, err := NewHTTPEmbedder(HTTPEmbedderConfig{Endpoint: "http://localhost"})
	if err == nil {
		t.Fatal("应拒绝 dimension<=0")
	}
}

func TestHTTPEmbedder_TextEmbedding(t *testing.T) {
	// 模拟返回 4 维向量的外部服务
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("期望 POST，实际 %s", r.Method)
		}
		var req httpEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Modality != "text" {
			t.Errorf("期望 modality=text，实际 %s", req.Modality)
		}
		if req.Text == "" {
			t.Fatal("text 不应为空")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"embedding":[0.1,0.2,0.3,0.4]}`))
	}))
	defer srv.Close()

	e, err := NewHTTPEmbedder(HTTPEmbedderConfig{
		Endpoint:  srv.URL,
		Dimension: 4,
		Model:     "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if !e.Loaded() {
		t.Fatal("应处于 loaded 状态")
	}

	vec, err := e.VectorizeDense("hello world")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 4 || vec[0] != 0.1 || vec[3] != 0.4 {
		t.Errorf("向量不符合预期: %v", vec)
	}
	if e.Fingerprint() != "http:test-model:4" {
		t.Errorf("指纹不符合预期: %s", e.Fingerprint())
	}
}

func TestHTTPEmbedder_ImageEmbedding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req httpEmbedRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Modality != "image" {
			t.Errorf("期望 modality=image，实际 %s", req.Modality)
		}
		if req.MIME != "image/png" {
			t.Errorf("期望 mime=image/png，实际 %s", req.MIME)
		}
		w.Write([]byte(`{"embedding":[0.5,0.5,0.5]}`))
	}))
	defer srv.Close()

	e, err := NewHTTPEmbedder(HTTPEmbedderConfig{
		Endpoint:  srv.URL,
		Dimension: 3,
		Model:     "img-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	vec, err := e.EmbedImageDense([]byte("fake-png-data"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 3 {
		t.Errorf("期望 3 维，实际 %d", len(vec))
	}
}

func TestHTTPEmbedder_CustomFingerprint(t *testing.T) {
	e, err := NewHTTPEmbedder(HTTPEmbedderConfig{
		Endpoint:    "http://localhost:1234",
		Dimension:   512,
		Fingerprint: "jina-v5-omni-nano:2026",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Fingerprint() != "jina-v5-omni-nano:2026" {
		t.Errorf("自定义指纹未生效: %s", e.Fingerprint())
	}
}

func TestHTTPEmbedder_DimensionMismatchReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"embedding":[1,2]}`)) // 返回 2 维，配置期望 4
	}))
	defer srv.Close()

	e, err := NewHTTPEmbedder(HTTPEmbedderConfig{
		Endpoint:  srv.URL,
		Dimension: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	_, err = e.VectorizeDense("test")
	if err == nil {
		t.Fatal("维度不匹配时应返回错误")
	}
}

func TestHTTPEmbedder_ServerErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("gateway down"))
	}))
	defer srv.Close()

	e, err := NewHTTPEmbedder(HTTPEmbedderConfig{
		Endpoint:  srv.URL,
		Dimension: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	_, err = e.VectorizeDense("test")
	if err == nil {
		t.Fatal("服务端错误时应返回错误")
	}
}

func TestHTTPEmbedder_ClosePreventsFurtherCalls(t *testing.T) {
	e, err := NewHTTPEmbedder(HTTPEmbedderConfig{
		Endpoint:  "http://localhost:1234",
		Dimension: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	if e.Loaded() {
		t.Fatal("关闭后 Loaded() 应返回 false")
	}
	_, err = e.VectorizeDense("test")
	if err == nil {
		t.Fatal("关闭后应返回错误")
	}
}
