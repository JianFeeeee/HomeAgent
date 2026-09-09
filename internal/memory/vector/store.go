package vector

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

// Vectorizer 接口：将文本转为向量
//
// 多模态嵌入新增可选的 EmbedImage：支持视觉嵌入的实现者覆写此方法，
// 不支持的（TF-IDF 等）在默认实现里返回 ErrNotSupported。
type Vectorizer interface {
	Vectorize(text string) Vector
	EmbedImage(img []byte, mime string) (Vector, error)
}

// MultimodalEmbedder 扩展 Vectorizer，提供直接返回 dense 向量的方法
// 与模型生命周期管理。CLIP 等视觉向量化器实现此接口；未启用时用空 stub。
type MultimodalEmbedder interface {
	Vectorizer
	VectorizeDense(text string) ([]float64, error)
	EmbedImageDense(img []byte, mime string) ([]float64, error)
	Fingerprint() string
	Dim() int
	Loaded() bool
	Close()
}

// ErrNotSupported 表示 Vectorizer 不支持图像嵌入，调用方按文本描述降级。
var ErrNotSupported = fmt.Errorf("vectorizer does not support image embedding")

// Vector 是带权特征映射：feature → weight
type Vector map[string]float64

// Store 向量存储，支持近似查询
type Store struct {
	mu    sync.RWMutex
	docs  []DocVector
	dim   int
	index *InvertedIndex
}

type DocVector struct {
	ID     string
	Vector Vector
	Text   string
	Meta   map[string]string
}

func NewStore() *Store {
	return &Store{
		index: NewInvertedIndex(),
	}
}

func (s *Store) Insert(id, text string, vec Vector, meta map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.docs = append(s.docs, DocVector{
		ID: id, Vector: vec, Text: text, Meta: meta,
	})
	s.index.Add(id, vec)
}

func (s *Store) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make([]DocVector, 0, len(s.docs))
	for _, d := range s.docs {
		if d.ID != id {
			filtered = append(filtered, d)
		}
	}
	s.docs = filtered
	s.index.Remove(id)
}

func (s *Store) Search(query Vector, topK int) []DocVector {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.docs) == 0 || len(query) == 0 {
		return nil
	}

	candidates := s.index.Search(query, len(s.docs))
	type scored struct {
		doc   DocVector
		score float64
	}

	var results []scored
	seen := make(map[string]bool)
	for _, id := range candidates {
		if seen[id] {
			continue
		}
		seen[id] = true
		for _, d := range s.docs {
			if d.ID == id {
				score := CosineSimilarity(query, d.Vector)
				if score > 0.05 {
					results = append(results, scored{d, score})
				}
				break
			}
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if len(results) > topK {
		results = results[:topK]
	}

	out := make([]DocVector, len(results))
	for i, r := range results {
		out[i] = r.doc
	}
	return out
}

func (s *Store) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.docs)
}

func (s *Store) All() []DocVector {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]DocVector, len(s.docs))
	copy(out, s.docs)
	return out
}

// Tokenizer 将文本拆分为词级 token
type Tokenizer func(string) []string

// NGramTokenizer 创建字符 n-gram tokenizer（降级方案）
func NGramTokenizer(maxN int) Tokenizer {
	return func(text string) []string {
		return extractNGrams(text, maxN)
	}
}

// TFIDFVectorizer 使用 tokenizer + TF-IDF
type TFIDFVectorizer struct {
	mu        sync.RWMutex
	tokenizer Tokenizer
	docFreq   map[string]float64 // feature → 文档频率
	totalDocs int
}

func NewTFIDFVectorizer(tokenizer Tokenizer) *TFIDFVectorizer {
	if tokenizer == nil {
		tokenizer = NGramTokenizer(2)
	}
	return &TFIDFVectorizer{
		tokenizer: tokenizer,
		docFreq:   make(map[string]float64),
	}
}

func (v *TFIDFVectorizer) Train(docs []string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.docFreq = make(map[string]float64)
	v.totalDocs = len(docs)

	seen := make(map[string]map[string]bool)
	for _, doc := range docs {
		features := v.tokenizer(doc)
		key := doc
		if seen[key] == nil {
			seen[key] = make(map[string]bool)
		}
		for _, f := range features {
			if !seen[key][f] {
				seen[key][f] = true
				v.docFreq[f]++
			}
		}
	}
}

func (v *TFIDFVectorizer) Vectorize(text string) Vector {
	v.mu.RLock()
	defer v.mu.RUnlock()

	features := v.tokenizer(text)
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
		if v.totalDocs < 3 {
			vec[f] = tfNorm
			continue
		}
		df := v.docFreq[f]
		if df <= 0 {
			continue
		}
		// 平滑 IDF，高频词趋近 0，低频词趋近 log(N)
		idf := math.Log(float64(v.totalDocs+1) / (df + 1))
		if idf < 0.1 {
			continue
		}
		vec[f] = tfNorm * idf
	}
	return vec
}

// extractNGrams 提取 n-gram 特征（主要用于中文）
func extractNGrams(text string, maxN int) []string {
	runes := []rune(strings.ToLower(text))
	var features []string
	seen := make(map[string]bool)

	for n := 1; n <= maxN; n++ {
		for i := 0; i <= len(runes)-n; i++ {
			gram := string(runes[i : i+n])
			gram = strings.TrimSpace(gram)
			if gram == "" {
				continue
			}
			if !seen[gram] {
				seen[gram] = true
				features = append(features, gram)
			}
		}
	}
	return features
}

func CosineSimilarity(a, b Vector) float64 {
	var dot, normA, normB float64
	for f, va := range a {
		dot += va * b[f]
		normA += va * va
	}
	for _, vb := range b {
		normB += vb * vb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// InvertedIndex 倒排索引，加速向量搜索
type InvertedIndex struct {
	mu       sync.RWMutex
	postings map[string]map[string]float64 // feature → {docID: weight}
}

func NewInvertedIndex() *InvertedIndex {
	return &InvertedIndex{
		postings: make(map[string]map[string]float64),
	}
}

func (idx *InvertedIndex) Add(docID string, vec Vector) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for feature, weight := range vec {
		if idx.postings[feature] == nil {
			idx.postings[feature] = make(map[string]float64)
		}
		idx.postings[feature][docID] = weight
	}
}

func (idx *InvertedIndex) Remove(docID string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for feature, postings := range idx.postings {
		delete(postings, docID)
		if len(postings) == 0 {
			delete(idx.postings, feature)
		}
	}
}

func (idx *InvertedIndex) Search(query Vector, maxResults int) []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	scores := make(map[string]float64)
	for feature, qw := range query {
		if postings, ok := idx.postings[feature]; ok {
			for docID, dw := range postings {
				scores[docID] += qw * dw
			}
		}
	}

	type pair struct {
		id    string
		score float64
	}
	var sorted []pair
	for id, score := range scores {
		sorted = append(sorted, pair{id, score})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].score > sorted[j].score
	})

	if len(sorted) > maxResults {
		sorted = sorted[:maxResults]
	}
	out := make([]string, len(sorted))
	for i, p := range sorted {
		out[i] = p.id
	}
	return out
}
