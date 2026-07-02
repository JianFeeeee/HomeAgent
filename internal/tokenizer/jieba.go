package tokenizer

import (
	"strings"
	"sync"

	jieba "github.com/yanyiwu/gojieba"
)

type Jieba struct {
	mu     sync.Mutex
	handle *jieba.Jieba
}

var (
	global *Jieba
	once   sync.Once
)

func Global() *Jieba {
	once.Do(func() {
		global = &Jieba{
			handle: jieba.NewJieba(),
		}
	})
	return global
}

func (j *Jieba) Close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle != nil {
		j.handle.Free()
		j.handle = nil
	}
}

func (j *Jieba) ExtractKeywords(text string, topK int) []string {
	j.mu.Lock()
	defer j.mu.Unlock()

	words := j.handle.ExtractWithWeight(text, topK)
	result := make([]string, 0, len(words))
	seen := make(map[string]bool)

	for _, w := range words {
		if seen[w.Word] {
			continue
		}
		if len([]rune(w.Word)) < 2 {
			continue
		}
		seen[w.Word] = true
		result = append(result, w.Word)
	}

	return result
}

func (j *Jieba) Cut(text string) []string {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.handle.Cut(text, true)
}

func (j *Jieba) Tag(text string) map[string]string {
	j.mu.Lock()
	defer j.mu.Unlock()

	words := j.handle.Tag(text)
	result := make(map[string]string, len(words))
	for _, pair := range words {
		if idx := strings.Index(pair, "/"); idx > 0 {
			result[pair[:idx]] = pair[idx+1:]
		} else {
			result[pair] = ""
		}
	}
	return result
}
