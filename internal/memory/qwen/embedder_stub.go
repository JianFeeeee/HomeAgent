//go:build !onnxruntime

package qwen

import "fmt"

// Embedder 在未启用 onnxruntime 时为 no-op 实现。
// 默认构建不链接 onnxruntime，保持原有 fastText/TF-IDF 行为不变。
type Embedder struct {
	loaded bool
}

func New(_ string) (*Embedder, error) {
	return nil, fmt.Errorf("qwen embedder requires build tag 'onnxruntime' (go build -tags onnxruntime)")
}

func (e *Embedder) Fingerprint() string { return "" }
func (e *Embedder) Dim() int            { return 0 }
func (e *Embedder) Loaded() bool        { return e.loaded }
func (e *Embedder) Close()              {}

func (e *Embedder) VectorizeDense(_ string) ([]float64, error) {
	return nil, fmt.Errorf("qwen embedder not available")
}

func (e *Embedder) EmbedImageDense(_ []byte, _ string) ([]float64, error) {
	return nil, fmt.Errorf("qwen embedder not available")
}
