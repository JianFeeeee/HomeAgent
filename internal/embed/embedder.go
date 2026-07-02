package embed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"
)

type Embedder interface {
	Embed(text string) ([]float64, error)
	Similarity(a, b []float64) float64
	Dimension() int
}

type OllamaEmbedder struct {
	client    *http.Client
	baseURL   string
	model     string
	dimension int
	mu        sync.RWMutex
}

func NewOllamaEmbedder(baseURL, model string, dimension int) *OllamaEmbedder {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	if dimension <= 0 {
		dimension = 768
	}

	return &OllamaEmbedder{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		baseURL:   baseURL,
		model:     model,
		dimension: dimension,
	}
}

func (e *OllamaEmbedder) Embed(text string) ([]float64, error) {
	if text == "" {
		return make([]float64, e.dimension), nil
	}

	reqBody := map[string]interface{}{
		"model": e.model,
		"prompt": text,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	resp, err := e.client.Post(e.baseURL+"/api/embeddings", "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("ollama api: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	return result.Embedding, nil
}

func (e *OllamaEmbedder) Similarity(a, b []float64) float64 {
	return cosineSimilarity(a, b)
}

func (e *OllamaEmbedder) Dimension() int {
	return e.dimension
}

type HashEmbedder struct {
	dimension int
}

func NewHashEmbedder(dimension int) *HashEmbedder {
	if dimension <= 0 {
		dimension = 64
	}
	return &HashEmbedder{dimension: dimension}
}

func (e *HashEmbedder) Embed(text string) ([]float64, error) {
	vec := make([]float64, e.dimension)
	runes := []rune(text)
	if len(runes) == 0 {
		return vec, nil
	}

	// Character-level hash embedding
	for i, r := range runes {
		h := hashRune(r)
		idx := i % e.dimension
		vec[idx] += float64(h) / 65536.0
	}

	// Normalize
	mag := 0.0
	for _, v := range vec {
		mag += v * v
	}
	if mag > 0 {
		mag = math.Sqrt(mag)
		for i := range vec {
			vec[i] /= mag
		}
	}

	return vec, nil
}

func (e *HashEmbedder) Similarity(a, b []float64) float64 {
	return cosineSimilarity(a, b)
}

func (e *HashEmbedder) Dimension() int {
	return e.dimension
}

func hashRune(r rune) uint64 {
	h := uint64(r)
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}

	if na == 0 || nb == 0 {
		return 0
	}

	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
