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

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

// ChannelCleaner 按事件来源查找输入通道的 Cleaner 函数。
// 返回 nil 表示不使用额外清洗。
type ChannelCleaner func(source string) func(string) string

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
	Vector      vector.Vector     `json:"vector,omitempty"` // 预计算向量（与 context 同空间），nil 则用 TF-IDF 兜底
}

// Store — 文档记忆存储，包含向量索引
type Store struct {
	dir    string
	vec    *vector.Store
	veczer *vector.TFIDFVectorizer
	mu     sync.RWMutex

	docs       map[string]*Doc
	summaries  []string // 用于训练向量化器，最大 10000 条
	vectorizer vector.Vectorizer // 可选：与 context 同空间的向量化器

	dirty bool
}

func (s *Store) SetVectorizer(v vector.Vectorizer) {
	s.vectorizer = v
}

// ReindexWithVectorizer 用给定的向量化器重建所有文档的向量索引
func (s *Store) ReindexWithVectorizer(v vector.Vectorizer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("[document memory] reindex with vectorizer (%d docs)", len(s.docs))
	s.vec = vector.NewStore()
	for _, doc := range s.docs {
		doc.Vector = v.Vectorize(doc.Summary + " " + doc.Content)
		s.vec.Insert(doc.ID, doc.Summary, doc.Vector, doc.Meta)
	}
	log.Printf("[document memory] reindex with vectorizer complete (%d vectors)", s.vec.Size())
}

const maxSummaries = 10000

func NewStore(dir string) *Store {
	return &Store{
		dir:    dir,
		vec:    vector.NewStore(),
		veczer: vector.NewTFIDFVectorizer(memory.TokenizeWords),
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

	// 增量训练向量化器并加入向量索引
	s.addSummary(doc.Summary)
	vec := doc.Vector
	if vec == nil {
		vec = s.veczer.Vectorize(doc.Summary + " " + doc.Content)
	}
	s.vec.Insert(doc.ID, doc.Summary, vec, doc.Meta)

	// 立即写盘
	path := filepath.Join(s.dir, doc.ID+".json")
	data, _ := json.MarshalIndent(doc, "", "  ")
	os.WriteFile(path, data, 0644)

	s.dirty = true
	return nil
}

// ContextToDoc — 将一段上下文对话历史提炼为文档（带内容去重）
// cleanFn 可选，在计算层前统一过滤文本，不影响原文存储。
// toolCleanFn 可选，func(name, output string) string，按工具名对输出进行过滤/清洗：
//   - 返回 "" → 跳过该工具输出（NoMemory）
//   - 返回清洗后文本 → 用于计算层（Cleaner），原文不受影响
func (s *Store) ContextToDoc(source string, entries []ContextEntry, vec vector.Vectorizer, cleanFn func(string) string, toolCleanFn func(name, output string) string, channelCleaner ChannelCleaner) (*Doc, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	if cleanFn == nil {
		cleanFn = func(text string) string { return text }
	}

	var parts []string
	for _, e := range entries {
		line := fmt.Sprintf("[%s] %s: %s", e.Timestamp.Format("15:04"), e.Source, e.Content)
		if e.Response != "" {
			line += fmt.Sprintf(" → %s", truncate(e.Response, 100))
		}
		for _, tr := range e.ToolResults {
			line += fmt.Sprintf("\n  [工具] %s: %s", tr.Name, truncate(tr.Output, 200))
		}
		parts = append(parts, line)
	}
	content := strings.Join(parts, "\n")
	contentHash := simpleHash(content)

	summary := summarizeEntries(entries, cleanFn, toolCleanFn, channelCleaner)
	tags := extractTags(entries, cleanFn, toolCleanFn, channelCleaner)
	entities := extractEntities(entries, cleanFn, toolCleanFn, channelCleaner)

	s.mu.Lock()

	// 去重
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
	var docVec vector.Vector
	if vec != nil {
		docVec = vec.Vectorize(summary + " " + content)
	} else {
		docVec = s.veczer.Vectorize(summary + " " + content)
	}
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
		Vector:      docVec,
	}
	s.docs[id] = doc

	// 加入向量索引
	s.addSummary(summary)
	s.vec.Insert(id, summary, doc.Vector, nil)

	s.dirty = true
	s.mu.Unlock()

	// 立即写盘
	path := filepath.Join(s.dir, id+".json")
	data, _ := json.MarshalIndent(doc, "", "  ")
	os.WriteFile(path, data, 0644)

	return doc, nil
}

// Consume — 向量相似度查询并移除文档（召回后即从冷存储删除，避免重复记忆）
func (s *Store) Consume(text string, topK int) []*Doc {
	s.mu.Lock()
	defer s.mu.Unlock()

	if topK <= 0 {
		topK = 5
	}

	vec := s.vectorizeQuery(text)
	results := s.vec.Search(vec, topK)

	var docs []*Doc
	for _, r := range results {
		if d, ok := s.docs[r.ID]; ok {
			s.removeDoc(r.ID)
			s.dirty = true
			docs = append(docs, d)
		}
	}
	return docs
}

// vectorizeQuery 用语义向量化器（首选）或 TF-IDF（兜底）处理查询文本
func (s *Store) vectorizeQuery(text string) vector.Vector {
	if s.vectorizer != nil {
		return s.vectorizer.Vectorize(text)
	}
	return s.veczer.Vectorize(text)
}

// Query — 向量相似度查询文档
func (s *Store) Query(text string, topK int) []*Doc {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if topK <= 0 {
		topK = 5
	}

	vec := s.vectorizeQuery(text)
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
		vec := doc.Vector
		if vec == nil {
			vec = s.veczer.Vectorize(doc.Summary + " " + doc.Content)
		}
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
		s.removeDoc(id)
		s.dirty = true
	}
}

// ——— internal ———

// addSummary 添加一条摘要到训练集，超限时截断并触发重索引。
// 调用方必须已持有 s.mu 写锁。
func (s *Store) addSummary(summary string) {
	s.summaries = append(s.summaries, summary)
	if len(s.summaries) > maxSummaries {
		n := maxSummaries / 2
		copy(s.summaries, s.summaries[len(s.summaries)-n:])
		s.summaries = s.summaries[:n]
		s.veczer.Train(s.summaries)
		s.vec = vector.NewStore()
		for _, doc := range s.docs {
			vec := s.veczer.Vectorize(doc.Summary + " " + doc.Content)
			s.vec.Insert(doc.ID, doc.Summary, vec, nil)
		}
	}
}

// removeDoc 从内存索引和磁盘删除文档。
// 调用方必须已持有 s.mu 写锁。
func (s *Store) removeDoc(id string) {
	delete(s.docs, id)
	s.vec.Remove(id)
	path := filepath.Join(s.dir, id+".json")
	os.Remove(path)
}

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
		vec := doc.Vector
		if vec == nil {
			vec = s.veczer.Vectorize(doc.Summary + " " + doc.Content)
		}
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

type ToolResultItem struct {
	Name   string
	Output string
}

type ContextEntry struct {
	Timestamp   time.Time
	Source      string
	Content     string
	Response    string
	ToolResults []ToolResultItem
}

func summarizeEntries(entries []ContextEntry, cleanText func(string) string, toolCleanFn func(name, output string) string, channelCleaner ChannelCleaner) string {
	if len(entries) == 0 {
		return ""
	}
	sources := make(map[string]int)
	var topics []string
	for _, e := range entries {
		sources[e.Source]++
		content := e.Content
		if channelCleaner != nil {
			if c := channelCleaner(e.Source); c != nil {
				content = c(content)
			}
		}
		words := memory.ExtractKeywords(cleanText(content))
		topics = append(topics, words...)
		for _, tr := range e.ToolResults {
			out := tr.Output
			if toolCleanFn != nil {
				if c := toolCleanFn(tr.Name, tr.Output); c == "" {
					continue
				} else {
					out = c
				}
			}
			toolWords := memory.ExtractKeywords(out)
			topics = append(topics, toolWords...)
		}
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

func extractTags(entries []ContextEntry, cleanText func(string) string, toolCleanFn func(name, output string) string, channelCleaner ChannelCleaner) []string {
	tagSet := make(map[string]bool)
	for _, e := range entries {
		content := e.Content
		if channelCleaner != nil {
			if c := channelCleaner(e.Source); c != nil {
				content = c(content)
			}
		}
		for _, kw := range memory.ExtractKeywords(cleanText(content)) {
			tagSet[kw] = true
		}
		for _, tr := range e.ToolResults {
			out := tr.Output
			if toolCleanFn != nil {
				if c := toolCleanFn(tr.Name, tr.Output); c == "" {
					continue
				} else {
					out = c
				}
			}
			for _, kw := range memory.ExtractKeywords(out) {
				tagSet[kw] = true
			}
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

func extractEntities(entries []ContextEntry, cleanText func(string) string, toolCleanFn func(name, output string) string, channelCleaner ChannelCleaner) []string {
	var entities []string
	seen := make(map[string]bool)
	for _, e := range entries {
		content := e.Content
		if channelCleaner != nil {
			if c := channelCleaner(e.Source); c != nil {
				content = c(content)
			}
		}
		for _, kw := range memory.ExtractKeywords(cleanText(content)) {
			if len(kw) >= 2 && !seen[kw] {
				seen[kw] = true
				entities = append(entities, kw)
			}
		}
		for _, tr := range e.ToolResults {
			out := tr.Output
			if toolCleanFn != nil {
				if c := toolCleanFn(tr.Name, tr.Output); c == "" {
					continue
				} else {
					out = c
				}
			}
			for _, kw := range memory.ExtractKeywords(out) {
				if len(kw) >= 2 && !seen[kw] {
					seen[kw] = true
					entities = append(entities, kw)
				}
			}
		}
	}
	if len(entities) > 20 {
		entities = entities[:20]
	}
	return entities
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
