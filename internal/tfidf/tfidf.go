// Package tfidf 提供 TF-IDF 向量化作为 MultimodalEmbedder 的一个实现。
//
// 设计：核心（内侧）只认 vector.MultimodalEmbedder 接口；本包是外层可插拔
// 的一个具体实现，与 Jina HTTP / ONNX CLIP 并列。启动时由 cmd/homed 按配置
// 选择注入，核心代码零改动。
//
// 稀疏向量 → 稠密桥接：TF-IDF 产出的是稀疏 map[feature]weight，通过特征哈希
// 投射到固定维度（默认 4096）的 []float64，供统一的 cosine 检索使用。
// 哈希碰撞在 4096 维下可接受（英文单词 ~50k，碰撞率 ~3%）。
package tfidf

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"
)

// Tokenizer 将文本拆分为词级 token。
type Tokenizer func(text string) []string

// Vector 是带权特征映射：feature → weight（稀疏表示）。
type Vector map[string]float64

// Embedder 实现 vector.MultimodalEmbedder，将 TF-IDF 稀疏向量投射为固定维度稠密向量。
type Embedder struct {
	mu        sync.RWMutex
	tokenizer Tokenizer
	docFreq   map[string]int
	totalDocs int
	dim       int
	loaded    bool
	fingerprint string
}

// NewEmbedder 创建 TF-IDF 多模态嵌入器。
// dim 是投射后的稠密维度（默认 4096，哈希空间大小）。
func NewEmbedder(tokenizer Tokenizer, dim int) *Embedder {
	if dim <= 0 {
		dim = 4096
	}
	return &Embedder{
		tokenizer: tokenizer,
		docFreq:   make(map[string]int),
		dim:       dim,
	}
}

// Train 用文档集训练 IDF 统计。启动时调用一次。
func (e *Embedder) Train(docs []string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.totalDocs = len(docs)
	e.docFreq = make(map[string]int)

	for _, doc := range docs {
		features := e.tokenizer(doc)
		seen := make(map[string]bool)
		for _, f := range features {
			if !seen[f] {
				e.docFreq[f]++
				seen[f] = true
			}
		}
	}
	e.fingerprint = fmt.Sprintf("tfidf:d%d:f%d", e.totalDocs, len(e.docFreq))
	e.loaded = true
}

// vectorize 将文本转为 TF-IDF 稀疏向量（内部方法，调用方已持锁）。
func (e *Embedder) vectorize(text string) Vector {
	features := e.tokenizer(text)
	tf := make(map[string]float64)
	for _, f := range features {
		tf[f]++
	}
	maxTF := 0.0
	for _, c := range tf {
		if c > maxTF {
			maxTF = c
		}
	}

	vec := make(Vector)
	for f, count := range tf {
		tfNorm := count / maxTF
		if e.totalDocs < 3 {
			vec[f] = tfNorm
			continue
		}
		df := e.docFreq[f]
		if df <= 0 {
			continue
		}
		idf := math.Log(float64(e.totalDocs+1) / float64(df+1))
		if idf < 0.1 {
			continue
		}
		vec[f] = tfNorm * idf
	}
	return vec
}

// sparseToDense 将稀疏向量投射为固定维度稠密向量（FNV-1a 哈希映射）。
func (e *Embedder) sparseToDense(sparse Vector) []float64 {
	dense := make([]float64, e.dim)
	var norm float64
	for feature, weight := range sparse {
		idx := fnvHash(feature) % uint32(e.dim)
		dense[idx] += weight
		norm += weight * weight
	}
	// L2 归一化
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range dense {
			dense[i] /= norm
		}
	}
	return dense
}

func fnvHash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// --- vector.MultimodalEmbedder 接口实现 ---

func (e *Embedder) VectorizeDense(text string) ([]float64, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.loaded {
		return nil, fmt.Errorf("tfidf: not trained")
	}
	sparse := e.vectorize(text)
	return e.sparseToDense(sparse), nil
}

func (e *Embedder) EmbedImageDense(img []byte, mime string) ([]float64, error) {
	return nil, fmt.Errorf("tfidf: image embedding not supported")
}

func (e *Embedder) Fingerprint() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.fingerprint
}

func (e *Embedder) Dim() int { return e.dim }

func (e *Embedder) Loaded() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.loaded
}

func (e *Embedder) Close() {}

// --- 检索（供 document.Store 使用，非接口方法）---

// DocHit 是一条检索命中。
type DocHit struct {
	ID    string
	Score float64
}

// SearchableIndex 是支持 TF-IDF 倒排检索的索引。
// document.Store 在 TF-IDF 模式下使用此索引替代 brute-force。
type SearchableIndex struct {
	mu    sync.RWMutex
	docs  map[string]Vector // id → tfidf sparse vector
	texts map[string]string // id → 原文
	emb   *Embedder
}

// NewSearchableIndex 创建可检索索引。
func NewSearchableIndex(emb *Embedder) *SearchableIndex {
	return &SearchableIndex{
		docs:  make(map[string]Vector),
		texts: make(map[string]string),
		emb:   emb,
	}
}

// Add 添加或更新一条文档。
func (idx *SearchableIndex) Add(id, text string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.emb.mu.RLock()
	vec := idx.emb.vectorize(text)
	idx.emb.mu.RUnlock()
	idx.docs[id] = vec
	idx.texts[id] = text
}

// Remove 移除一条文档。
func (idx *SearchableIndex) Remove(id string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	delete(idx.docs, id)
	delete(idx.texts, id)
}

// Search 用查询文本检索 topK 个最相似的文档。
func (idx *SearchableIndex) Search(query string, topK int) []DocHit {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if len(idx.docs) == 0 {
		return nil
	}

	idx.emb.mu.RLock()
	qVec := idx.emb.vectorize(query)
	idx.emb.mu.RUnlock()

	type scored struct {
		id    string
		score float64
	}
	var results []scored
	for id, docVec := range idx.docs {
		score := sparseCosine(qVec, docVec)
		if score > 0.01 {
			results = append(results, scored{id, score})
		}
	}

	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	if len(results) > topK {
		results = results[:topK]
	}

	out := make([]DocHit, len(results))
	for i, r := range results {
		out[i] = DocHit{ID: r.id, Score: r.score}
	}
	return out
}

// sparseCosine 计算两个稀疏向量的余弦相似度。
func sparseCosine(a, b Vector) float64 {
	var dot, na, nb float64
	for f, va := range a {
		dot += va * b[f]
		na += va * va
	}
	for _, vb := range b {
		nb += vb * vb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

// Text 返回文档原文（供调试/展示）。
func (idx *SearchableIndex) Text(id string) string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.texts[id]
}

// IDs 返回所有文档 ID（供重建索引）。
func (idx *SearchableIndex) IDs() []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	ids := make([]string, 0, len(idx.docs))
	for id := range idx.docs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Size 返回索引中的文档数。
func (idx *SearchableIndex) Size() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.docs)
}

// Train 用文档集训练并建立索引。
func (idx *SearchableIndex) Train(docs map[string]string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for id, text := range docs {
		idx.emb.mu.RLock()
		vec := idx.emb.vectorize(text)
		idx.emb.mu.RUnlock()
		idx.docs[id] = vec
		idx.texts[id] = text
	}
}

// TokenizeWords 是默认的中英文分词器（需外部注入 jieba 分词函数）。
// 外层 cmd/homed 负责注入，核心不直接依赖 jieba。
var TokenizeWords Tokenizer = func(text string) []string {
	// 简单 fallback：按空白和标点拆分
	return strings.Fields(text)
}
