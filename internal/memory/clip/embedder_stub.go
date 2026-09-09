//go:build !onnxruntime

package clip

import (
	"fmt"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

// Embedder 在未启用 onnxruntime 时为 no-op 实现。
// 构建时不链接 onnxruntime，default 构建保持原有 fastText/TF-IDF 行为不变。
type Embedder struct {
	loaded bool
}

func New(_ string) (*Embedder, error) {
	return nil, fmt.Errorf("clip embedder requires build tag 'onnxruntime' (go build -tags onnxruntime)")
}

func (e *Embedder) Fingerprint() string              { return "" }
func (e *Embedder) Dim() int                         { return 0 }
func (e *Embedder) Loaded() bool                     { return e.loaded }
func (e *Embedder) Vectorize(_ string) vector.Vector { return nil }
func (e *Embedder) EmbedImage(_ []byte, _ string) (vector.Vector, error) {
	return nil, fmt.Errorf("clip embedder not available")
}
func (e *Embedder) VectorizeDense(_ string) ([]float64, error) {
	return nil, fmt.Errorf("clip embedder not available")
}
func (e *Embedder) EmbedImageDense(_ []byte, _ string) ([]float64, error) {
	return nil, fmt.Errorf("clip embedder not available")
}
func (e *Embedder) Close() {}
