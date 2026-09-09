package vector

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HTTPEmbedderConfig 配置一个外部多模态向量服务。
// 服务契约刻意很小：POST Endpoint，输入 modality/data/mime/side，返回 embedding。
// 任何云 API 或自建服务只需适配这一个协议，即可复用内核全部向量存储与检索链路。
type HTTPEmbedderConfig struct {
	Endpoint    string
	APIKey     string
	Model       string
	Dimension   int
	Timeout     time.Duration
	Fingerprint string
}

// HTTPEmbedder 是 MultimodalEmbedder 的外部 API 实现。
type HTTPEmbedder struct {
	cfg    HTTPEmbedderConfig
	client *http.Client
	mu     sync.Mutex
	closed bool
}

type httpEmbedRequest struct {
	Model    string `json:"model,omitempty"`
	Modality string `json:"modality"`
	Side     string `json:"side"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIME     string `json:"mime,omitempty"`
}

type httpEmbedResponse struct {
	Embedding []float64 `json:"embedding"`
	Data      []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data,omitempty"`
}

func NewHTTPEmbedder(cfg HTTPEmbedderConfig) (*HTTPEmbedder, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("vector: empty HTTP embedding endpoint")
	}
	if cfg.Dimension <= 0 {
		return nil, fmt.Errorf("vector: invalid HTTP embedding dimension %d", cfg.Dimension)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Fingerprint == "" {
		cfg.Fingerprint = "http:" + cfg.Model + fmt.Sprintf(":%d", cfg.Dimension)
	}
	return &HTTPEmbedder{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}, nil
}

func (e *HTTPEmbedder) VectorizeDense(text string) ([]float64, error) {
	return e.embed(httpEmbedRequest{Model: e.cfg.Model, Modality: string(ModalityText), Side: "query", Text: text})
}

func (e *HTTPEmbedder) EmbedImageDense(img []byte, mime string) ([]float64, error) {
	return e.embed(httpEmbedRequest{Model: e.cfg.Model, Modality: string(ModalityImage), Side: "document", Data: base64.StdEncoding.EncodeToString(img), MIME: mime})
}

func (e *HTTPEmbedder) embed(payload httpEmbedRequest) ([]float64, error) {
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("vector: HTTP embedder closed")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, e.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vector: HTTP embedding request: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vector: HTTP embedding status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out httpEmbedResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("vector: decode HTTP embedding: %w", err)
	}
	v := out.Embedding
	if len(v) == 0 && len(out.Data) > 0 {
		v = out.Data[0].Embedding
	}
	if len(v) != e.cfg.Dimension {
		return nil, fmt.Errorf("vector: HTTP embedding dimension %d, want %d", len(v), e.cfg.Dimension)
	}
	return v, nil
}

func (e *HTTPEmbedder) Fingerprint() string { return e.cfg.Fingerprint }
func (e *HTTPEmbedder) Dim() int            { return e.cfg.Dimension }
func (e *HTTPEmbedder) Loaded() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.closed
}
func (e *HTTPEmbedder) Close() {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
}
