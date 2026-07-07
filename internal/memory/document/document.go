package document

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

// Doc — 记忆文档：由上下文提炼而来
type Doc struct {
	ID          string            `json:"id"`
	Summary     string            `json:"summary"`
	Content     string            `json:"content"`
	Tags        []string          `json:"tags"`
	Entities    []string          `json:"entities"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	Source      string            `json:"source"` // context / graph / manual
	Meta        map[string]string `json:"meta,omitempty"`
	AccessCount int               `json:"access_count"`    // 访问次数
	LastAccess  time.Time         `json:"last_access"`     // 最后访问时间
}

// Store — 文档记忆存储，包含向量索引
type Store struct {
	dir    string
	vec    *vector.Store
	veczer *vector.TFIDFVectorizer
	mu     sync.RWMutex

	docs      map[string]*Doc
	summaries []string // 用于训练向量化器

	dirty bool
}

func NewStore(dir string) *Store {
	return &Store{
		dir:    dir,
		vec:    vector.NewStore(),
		veczer: vector.NewTFIDFVectorizer(2),
		docs:   make(map[string]*Doc),
	}
}

func (s *Store) Start() error {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return fmt.Errorf("document store dir: %w", err)
	}
	if err := s.loadAll(); err != nil {
		log.Printf("[document memory] load error: %v", err)
	}
	log.Printf("[document memory] started with %d docs, %d vectors", len(s.docs), s.vec.Size())
	return nil
}

func (s *Store) Stop() {
	s.flush()
}

// Insert 创建/更新文档
func (s *Store) Insert(doc *Doc) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if doc.ID == "" {
		doc.ID = fmt.Sprintf("doc_%d", time.Now().UnixNano())
		doc.CreatedAt = time.Now()
	}
	doc.UpdatedAt = time.Now()
	doc.LastAccess = time.Now()
	if doc.AccessCount == 0 {
		doc.AccessCount = 1
	}

	s.docs[doc.ID] = doc

	vec := s.veczer.Vectorize(doc.Summary + " " + doc.Content)
	s.vec.Insert(doc.ID, doc.Summary, vec, doc.Meta)

	// 更新训练集
	s.summaries = append(s.summaries, doc.Summary)

	s.dirty = true
	return nil
}

// ContextToDoc — 将一段上下文对话历史提炼为文档（带内容去重）
func (s *Store) ContextToDoc(source string, entries []ContextEntry) (*Doc, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	var parts []string
	for _, e := range entries {
		line := fmt.Sprintf("[%s] %s: %s", e.Timestamp.Format("15:04"), e.Source, e.Content)
		if e.Response != "" {
			line += fmt.Sprintf(" → %s", truncate(e.Response, 100))
		}
		parts = append(parts, line)
	}
	content := strings.Join(parts, "\n")
	contentHash := simpleHash(content)

	// 去重：检查是否已有相同 hash 的文档（在锁内完成创建/更新）
	summary := summarizeEntries(entries)
	tags := extractTags(entries)
	entities := extractEntities(entries)

	s.mu.Lock()
	for _, d := range s.docs {
		if d.Meta != nil && d.Meta["content_hash"] == contentHash {
			d.UpdatedAt = time.Now()
			d.LastAccess = time.Now()
			d.Content = content
			d.Source = source
			d.Summary = summary
			d.Tags = tags
			d.Entities = entities
			s.dirty = true
			s.mu.Unlock()
			return d, nil
		}
	}

	id := fmt.Sprintf("doc_%d", time.Now().UnixNano())
	doc := &Doc{
		ID:          id,
		Summary:     summary,
		Content:     content,
		Tags:        tags,
		Entities:    entities,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		LastAccess:  time.Now(),
		AccessCount: 1,
		Source:      source,
		Meta:        map[string]string{"content_hash": contentHash},
	}
	s.docs[id] = doc
	s.dirty = true
	s.mu.Unlock()
	return doc, nil
}

// Consume — 向量相似度查询并移除文档（召回后即从冷存储删除，避免重复记忆）
func (s *Store) Consume(text string, topK int) []*Doc {
	s.mu.Lock()
	defer s.mu.Unlock()

	if topK <= 0 {
		topK = 5
	}

	vec := s.veczer.Vectorize(text)
	results := s.vec.Search(vec, topK)

	var docs []*Doc
	for _, r := range results {
		if d, ok := s.docs[r.ID]; ok {
			delete(s.docs, r.ID)
			s.vec.Remove(r.ID)
			s.dirty = true
			docs = append(docs, d)
		}
	}
	return docs
}

// Query — 向量相似度查询文档
func (s *Store) Query(text string, topK int) []*Doc {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if topK <= 0 {
		topK = 5
	}

	vec := s.veczer.Vectorize(text)
	results := s.vec.Search(vec, topK)

	var docs []*Doc
	for _, r := range results {
		if d, ok := s.docs[r.ID]; ok {
			d.AccessCount++
			d.LastAccess = time.Now()
			docs = append(docs, d)
		}
	}
	return docs
}

// Reindex — 重新训练并重建向量索引
func (s *Store) Reindex() {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("[document memory] reindexing %d docs", len(s.docs))

	s.veczer.Train(s.summaries)

	s.vec = vector.NewStore()
	for _, doc := range s.docs {
		vec := s.veczer.Vectorize(doc.Summary + " " + doc.Content)
		s.vec.Insert(doc.ID, doc.Summary, vec, doc.Meta)
	}

	log.Printf("[document memory] reindex complete (%d vectors)", s.vec.Size())
}

func (s *Store) Stats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[string]interface{}{
		"doc_count":     len(s.docs),
		"vector_count":  s.vec.Size(),
		"summary_count": len(s.summaries),
		"dir":           s.dir,
	}
}

// FindColdDocs — 查找冷文档：超过 maxAge 未访问且访问次数 <= minAccess
func (s *Store) FindColdDocs(maxAge time.Duration, minAccess int) []*Doc {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := time.Now().Add(-maxAge)
	var cold []*Doc
	for _, d := range s.docs {
		if d.AccessCount <= minAccess && d.LastAccess.Before(cutoff) {
			cold = append(cold, d)
		}
	}
	return cold
}

func (s *Store) RecentDocs(n int) []*Doc {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var list []*Doc
	for _, d := range s.docs {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
	if len(list) > n {
		list = list[:n]
	}
	return list
}

// Remove 从文档存储中删除指定 ID 的文档
func (s *Store) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.docs[id]; ok {
		delete(s.docs, id)
		s.vec.Remove(id)
		s.dirty = true
	}
}

// ——— internal ———

func (s *Store) loadAll() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") || !strings.HasPrefix(e.Name(), "doc_") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc Doc
		if err := json.Unmarshal(data, &doc); err != nil {
			continue
		}
		s.docs[doc.ID] = &doc
		s.summaries = append(s.summaries, doc.Summary)
	}

	// 训练向量化器
	if len(s.summaries) > 0 {
		s.veczer.Train(s.summaries)
	}

	// 重建向量索引
	for _, doc := range s.docs {
		vec := s.veczer.Vectorize(doc.Summary + " " + doc.Content)
		s.vec.Insert(doc.ID, doc.Summary, vec, nil)
	}

	return nil
}

func (s *Store) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dirty {
		return
	}

	for _, doc := range s.docs {
		path := filepath.Join(s.dir, doc.ID+".json")
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			continue
		}
		os.WriteFile(path, data, 0644)
	}
	s.dirty = false
}

type ContextEntry struct {
	Timestamp time.Time
	Source    string
	Content   string
	Response  string
}

func summarizeEntries(entries []ContextEntry) string {
	if len(entries) == 0 {
		return ""
	}
	sources := make(map[string]int)
	var topics []string
	for _, e := range entries {
		sources[e.Source]++
		words := extractKeywords(e.Content)
		topics = append(topics, words...)
	}

	summary := fmt.Sprintf("来自 %d 个来源的 %d 条对话", len(sources), len(entries))
	var srcList []string
	for s := range sources {
		srcList = append(srcList, s)
	}
	summary += " (" + strings.Join(srcList, ", ") + ")"

	if len(topics) > 0 {
		seen := make(map[string]bool)
		var uniq []string
		for _, t := range topics {
			if !seen[t] {
				seen[t] = true
				uniq = append(uniq, t)
			}
		}
		if len(uniq) > 5 {
			uniq = uniq[:5]
		}
		summary += " 涉及: " + strings.Join(uniq, ", ")
	}

	return summary
}

func extractTags(entries []ContextEntry) []string {
	tagSet := make(map[string]bool)
	for _, e := range entries {
		for _, kw := range extractKeywords(e.Content) {
			tagSet[kw] = true
		}
	}
	var tags []string
	for t := range tagSet {
		if len(tags) >= 10 {
			break
		}
		tags = append(tags, t)
	}
	return tags
}

func extractEntities(entries []ContextEntry) []string {
	// 简易实体提取：提取引号内的内容、粗体/标记词
	var entities []string
	seen := make(map[string]bool)
	for _, e := range entries {
		for _, kw := range extractKeywords(e.Content) {
			if len(kw) >= 2 && !seen[kw] {
				seen[kw] = true
				entities = append(entities, kw)
			}
		}
	}
	if len(entities) > 20 {
		entities = entities[:20]
	}
	return entities
}

func extractKeywords(text string) []string {
	stopWords := map[string]bool{
		"的": true, "了": true, "是": true, "在": true, "有": true,
		"和": true, "就": true, "不": true, "人": true, "都": true,
		"一": true, "一个": true, "上": true, "也": true, "很": true,
		"到": true, "说": true, "要": true, "去": true, "你": true,
		"会": true, "着": true, "没有": true, "看": true, "好": true,
		"自己": true, "这": true, "他": true, "她": true, "它": true,
		"什么": true, "怎么": true, "为什么": true, "如何": true,
		"我": true, "我们": true, "你们": true, "他们": true, "这个": true,
		"那个": true, "可以": true, "吗": true, "吧": true, "啊": true,
	}

	var keywords []string
	runes := []rune(text)

	// bi-gram
	for i := 0; i < len(runes)-1; i++ {
		word := string(runes[i : i+2])
		if !stopWords[word] && len(strings.TrimSpace(word)) == len(word) {
			keywords = append(keywords, word)
		}
	}
	return keywords
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) > max {
		return string(runes[:max]) + "..."
	}
	return s
}

func simpleHash(s string) string {
	// 简单的基于内容的哈希，用于去重
	h := 0
	for _, r := range s {
		h = h*31 + int(r)
	}
	return fmt.Sprintf("h%08x", h)
}
