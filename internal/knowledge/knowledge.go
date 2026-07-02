package knowledge

import (
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

// Knowledge — 单条知识
type Knowledge struct {
	Name      string            `json:"name"`
	Content   string            `json:"content"`
	Path      string            `json:"path"`
	Tags      []string          `json:"tags"`
	UpdatedAt time.Time         `json:"updated_at"`
	Meta      map[string]string `json:"meta,omitempty"`
}

// Store — 知识库，文件系统 + 向量索引
type Store struct {
	root   string
	vec    *vector.Store
	veczer *vector.TFIDFVectorizer
	mu     sync.RWMutex
	items  map[string]*Knowledge

	summaries []string
}

func NewStore(root string) *Store {
	return &Store{
		root:   root,
		vec:    vector.NewStore(),
		veczer: vector.NewTFIDFVectorizer(3),
		items:  make(map[string]*Knowledge),
	}
}

func (s *Store) Start() error {
	if err := os.MkdirAll(s.root, 0755); err != nil {
		return fmt.Errorf("knowledge root: %w", err)
	}
	if err := s.scanAll(); err != nil {
		log.Printf("[knowledge] scan error: %v", err)
	}
	log.Printf("[knowledge] started with %d items, %d vectors", len(s.items), s.vec.Size())
	return nil
}

func (s *Store) Stop() {}

// Search — 向量查询知识
func (s *Store) Search(query string, topK int) []*Knowledge {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if topK <= 0 {
		topK = 5
	}

	vec := s.veczer.Vectorize(query)
	results := s.vec.Search(vec, topK)

	var out []*Knowledge
	for _, r := range results {
		if k, ok := s.items[r.ID]; ok {
			out = append(out, k)
		}
	}
	return out
}

// Add — 添加或更新知识
func (s *Store) Add(name, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 创建知识目录
	dir := filepath.Join(s.root, sanitize(name))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create knowledge dir: %w", err)
	}

	// 写入知识文件
	path := filepath.Join(dir, "content.md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("write knowledge: %w", err)
	}

	now := time.Now()
	k := &Knowledge{
		Name:      name,
		Content:   content,
		Path:      path,
		Tags:      extractKeywords(name + " " + content),
		UpdatedAt: now,
	}

	// 生成 ID = 目录名
	id := sanitize(name)
	s.items[id] = k

	vec := s.veczer.Vectorize(name + " " + content)
	s.vec.Insert(id, name+": "+content, vec, map[string]string{
		"name": name, "path": path,
	})

	s.summaries = append(s.summaries, name+" "+content)

	log.Printf("[knowledge] added: %s (%d bytes)", name, len(content))
	return nil
}

// SearchCategories — 返回所有知识类别
func (s *Store) SearchCategories(query string, topK int) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if query == "" {
		var names []string
		for _, k := range s.items {
			names = append(names, k.Name)
		}
		sort.Strings(names)
		if len(names) > topK {
			names = names[:topK]
		}
		return names
	}

	vec := s.veczer.Vectorize(query)
	results := s.vec.Search(vec, topK)
	var names []string
	for _, r := range results {
		if k, ok := s.items[r.ID]; ok {
			names = append(names, k.Name)
		}
	}
	return names
}

func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := sanitize(name)
	dir := filepath.Join(s.root, id)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	delete(s.items, id)
	s.vec.Remove(id)
	return nil
}

func (s *Store) Stats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]interface{}{
		"knowledge_count": len(s.items),
		"vector_count":    s.vec.Size(),
		"root":            s.root,
	}
}

func (s *Store) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var names []string
	for _, k := range s.items {
		names = append(names, k.Name)
	}
	sort.Strings(names)
	return names
}

// ——— internal ———

func (s *Store) scanAll() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(s.root, entry.Name())
		contentPath := filepath.Join(dir, "content.md")
		data, err := os.ReadFile(contentPath)
		if err != nil {
			continue
		}

		name := entry.Name()
		content := string(data)
		now := time.Now()

		k := &Knowledge{
			Name:      name,
			Content:   content,
			Path:      contentPath,
			Tags:      extractKeywords(name + " " + content),
			UpdatedAt: now,
		}
		s.items[name] = k
		s.summaries = append(s.summaries, name+" "+content)
	}

	// 训练向量化器
	if len(s.summaries) > 0 {
		s.veczer.Train(s.summaries)
	}

	// 构建向量索引
	for _, k := range s.items {
		vec := s.veczer.Vectorize(k.Name + " " + k.Content)
		s.vec.Insert(k.Name, k.Name+": "+k.Content, vec, map[string]string{
			"name": k.Name, "path": k.Path,
		})
	}

	return nil
}

func sanitize(name string) string {
	name = strings.ToLower(name)
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, " ", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	return name
}

func extractKeywords(text string) []string {
	stopWords := map[string]bool{
		"的": true, "了": true, "是": true, "在": true, "有": true,
		"和": true, "就": true, "不": true, "都": true,
		"一": true, "一个": true, "也": true, "很": true,
		"到": true, "说": true, "要": true, "去": true,
		"会": true, "着": true, "没有": true, "看": true, "好": true,
		"自己": true, "这": true, "他": true, "她": true, "它": true,
		"什么": true, "怎么": true, "为什么": true, "如何": true,
		"我们": true, "你们": true, "他们": true, "这个": true,
		"那个": true, "可以": true, "吗": true, "吧": true, "啊": true,
	}

	var keywords []string
	runes := []rune(text)

	seen := make(map[string]bool)

	// bi-gram
	for i := 0; i < len(runes)-1; i++ {
		word := string(runes[i : i+2])
		if !stopWords[word] && len(strings.TrimSpace(word)) == len(word) && !seen[word] {
			seen[word] = true
			keywords = append(keywords, word)
		}
	}

	if len(keywords) > 10 {
		keywords = keywords[:10]
	}
	return keywords
}
