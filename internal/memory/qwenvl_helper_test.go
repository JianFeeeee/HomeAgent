package memory

import (
	"math"
	"sync"
	"testing"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/embedding"
	_ "gitcode.com/JianFeeeee/HomeAgent/providers/qwen3vl"
)

// qwenProbe 是 Qwen3-VL 的探针包装（带缓存）。
//
// 为什么必须有缓存：ONNX 前向每次约 0.3~1 秒，而实验要对 260 个块
// 两两比较（33658 次）—— 不缓存就是几小时。块文本重复度高，
// 缓存命中率很高（实测 260 个块里只有 255 个不同三元组键）。
type qwenProbe struct {
	ad  *vector.ProviderAdapter
	mu  sync.Mutex
	idx map[string][]float64
}

// openQwenVL 打开 Qwen3-VL 探针；模型目录不存在则跳过（t.Skip）。
func openQwenVL(t *testing.T) *qwenProbe {
	t.Helper()
	const dir = qwenModelDir
	p, err := embedding.Open("qwen3vl", embedding.Config{
		Options: map[string]string{"model_dir": dir},
	})
	if err != nil {
		t.Skipf("Qwen3-VL provider 打开失败（需 onnxruntime 与 %s）: %v", dir, err)
	}
	t.Cleanup(p.Close)
	ad, err := vector.AdaptProvider(p)
	if err != nil {
		t.Skipf("适配失败: %v", err)
	}
	return &qwenProbe{ad: ad, idx: map[string][]float64{}}
}

// vec 取文本向量（带缓存）。
func (q *qwenProbe) vec(text string) []float64 {
	q.mu.Lock()
	if v, ok := q.idx[text]; ok {
		q.mu.Unlock()
		return v
	}
	q.mu.Unlock()

	v, err := q.ad.VectorizeDense(text)
	if err != nil {
		return nil
	}
	q.mu.Lock()
	q.idx[text] = v
	q.mu.Unlock()
	return v
}

// Close 是为接口整齐留的（实际资源由 t.Cleanup 释放）。
func (q *qwenProbe) Close() {}

// cosineVec 是余弦相似度。
//
// ★ 自己实现而不用 internal/memory/vector.CosineSimilarity：
// 后者签名是 (Vector, Vector)（vector 包的类型别名 []float64），
// 这里直接用 []float64 更顺手，且探针不依赖那个包的类型。
func cosineVec(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		if i >= len(b) {
			return 0
		}
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
