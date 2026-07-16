package memory

import (
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/yanyiwu/gojieba"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

type LocalWordEmbedder struct {
	mu        sync.RWMutex
	jieba     *gojieba.Jieba
	stopWords map[string]bool

	docFreq   map[string]float64
	totalDocs int

	coOccur map[string]map[string]float64

	vocab   map[string]bool
	trained bool
}

func NewLocalWordEmbedder() *LocalWordEmbedder {
	sw := make(map[string]bool)
	for k, v := range stopWords {
		sw[k] = v
	}
	return &LocalWordEmbedder{
		jieba:     GetJieba(),
		stopWords: sw,
		docFreq:   make(map[string]float64),
		coOccur:   make(map[string]map[string]float64),
		vocab:     make(map[string]bool),
	}
}

func (e *LocalWordEmbedder) tokenize(text string) []string {
	if e.jieba == nil {
		return nil
	}
	words := e.jieba.Cut(text, true)
	var result []string
	seen := make(map[string]bool)
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" || e.stopWords[w] || seen[w] {
			continue
		}
		runes := []rune(w)
		if len(runes) < 2 {
			continue
		}
		seen[w] = true
		result = append(result, w)
	}
	return result
}

func (e *LocalWordEmbedder) Train(docs []string) {
	if e.jieba == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.docFreq = make(map[string]float64)
	e.coOccur = make(map[string]map[string]float64)
	e.vocab = make(map[string]bool)

	tokenized := make([][]string, len(docs))

	for i, doc := range docs {
		tokens := e.tokenize(doc)
		tokenized[i] = tokens

		seen := make(map[string]bool)
		for _, t := range tokens {
			e.vocab[t] = true
			if !seen[t] {
				e.docFreq[t]++
				seen[t] = true
			}
		}
	}
	e.totalDocs = len(docs)

	windowSize := 5
	for _, tokens := range tokenized {
		for i, word := range tokens {
			start := i - windowSize
			if start < 0 {
				start = 0
			}
			end := i + windowSize + 1
			if end > len(tokens) {
				end = len(tokens)
			}
			for j := start; j < end; j++ {
				if i == j {
					continue
				}
				ctx := tokens[j]
				if e.coOccur[word] == nil {
					e.coOccur[word] = make(map[string]float64)
				}
				e.coOccur[word][ctx]++
			}
		}
	}

	for word, ctxs := range e.coOccur {
		totalPairs := 0.0
		for _, count := range ctxs {
			totalPairs += count
		}
		pWord := e.docFreq[word] / float64(e.totalDocs)
		for ctx, count := range ctxs {
			pCtx := e.docFreq[ctx] / float64(e.totalDocs)
			pJoint := count / totalPairs
			pmi := math.Log2(pJoint / (pWord * pCtx))
			if pmi <= 0 {
				delete(ctxs, ctx)
			} else {
				ctxs[ctx] = pmi
			}
		}
		e.coOccur[word] = pruneTopK(ctxs, 50)
	}

	e.trained = true
}

func pruneTopK(m map[string]float64, k int) map[string]float64 {
	if len(m) <= k {
		return m
	}
	type kv struct {
		k string
		v float64
	}
	var sorted []kv
	for key, val := range m {
		sorted = append(sorted, kv{key, val})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].v > sorted[j].v
	})
	result := make(map[string]float64, k)
	for i := 0; i < k; i++ {
		result[sorted[i].k] = sorted[i].v
	}
	return result
}

func (e *LocalWordEmbedder) Vectorize(text string) vector.Vector {
	e.mu.RLock()
	useEmbedding := e.trained
	e.mu.RUnlock()

	tokens := e.tokenize(text)
	if len(tokens) == 0 {
		return vector.Vector{}
	}

	tf := make(map[string]float64)
	for _, t := range tokens {
		tf[t]++
	}
	maxTF := 0.0
	for _, count := range tf {
		if count > maxTF {
			maxTF = count
		}
	}

	vec := make(vector.Vector)

	if useEmbedding {
		e.mu.RLock()
		for word, count := range tf {
			tfidf := (count / maxTF) * idf(e.docFreq[word], e.totalDocs)

			if ctxs, ok := e.coOccur[word]; ok {
				for ctx, pmi := range ctxs {
					vec[ctx] += tfidf * pmi
				}
			}

			vec["__w__"+word] += tfidf
		}
		e.mu.RUnlock()
	} else {
		for word, count := range tf {
			tfNorm := count / maxTF
			var df float64
			e.mu.RLock()
			df = e.docFreq[word]
			e.mu.RUnlock()
			vec[word] = tfNorm * idf(df, e.totalDocs)
		}
	}

	return vec
}

func idf(df float64, total int) float64 {
	if df <= 0 || total <= 0 {
		return 1.0
	}
	return math.Log(float64(total+1)/(df+1)+1) + 1
}

func (e *LocalWordEmbedder) Trained() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.trained
}
