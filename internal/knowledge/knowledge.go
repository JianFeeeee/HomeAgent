package knowledge

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

type Knowledge struct {
	Name      string            `json:"name"`
	Content   string            `json:"content"`
	Path      string            `json:"path"`
	Category  string            `json:"category,omitempty"` // 父路径，如 "tech/go"
	Tags      []string          `json:"tags"`
	UpdatedAt time.Time         `json:"updated_at"`
	Meta      map[string]string `json:"meta,omitempty"`
}

// IndexItem — 索引条目，包含向量特征和内容摘要
type IndexItem struct {
	Name    string             `json:"name"`
	Preview string             `json:"preview"`          // 前 200 字摘要
	Tags    []string           `json:"tags"`
	Vector  map[string]float64 `json:"vector"`           // TF-IDF 特征向量（top-N 特征）
	Size    int                `json:"size"`             // 内容总字节数
}

// TreeIndex — 树状索引节点
type TreeIndex struct {
	Name     string              `json:"name"`
	Children map[string]*TreeIndex `json:"children,omitempty"`
	Items    []IndexItem         `json:"items,omitempty"` // 此节点下的知识条目（含向量）
}

func newTreeIndex(name string) *TreeIndex {
	return &TreeIndex{Name: name, Children: make(map[string]*TreeIndex)}
}

// compressVector 压缩向量：保留 topN 个权重最高的特征
func compressVector(v vector.Vector, topN int) map[string]float64 {
	if len(v) <= topN {
		out := make(map[string]float64, len(v))
		for k, w := range v {
			out[k] = w
		}
		return out
	}
	type kv struct {
		k string
		v float64
	}
	sorted := make([]kv, 0, len(v))
	for k, w := range v {
		sorted = append(sorted, kv{k, w})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].v > sorted[j].v
	})
	if topN > len(sorted) {
		topN = len(sorted)
	}
	sorted = sorted[:topN]
	out := make(map[string]float64, topN)
	for _, kv := range sorted {
		out[kv.k] = kv.v
	}
	return out
}

type Store struct {
	root   string
	vec    *vector.Store
	veczer *vector.TFIDFVectorizer
	mu     sync.RWMutex
	items  map[string]*Knowledge

	indexPath string
	summaries []string
}

func NewStore(root string) *Store {
	return &Store{
		root:      root,
		indexPath: filepath.Join(root, ".index.json"),
		vec:       vector.NewStore(),
		veczer:    vector.NewTFIDFVectorizer(memory.TokenizeWords),
		items:     make(map[string]*Knowledge),
	}
}

func (s *Store) Start() error {
	if err := os.MkdirAll(s.root, 0755); err != nil {
		return fmt.Errorf("knowledge root: %w", err)
	}
	if err := s.scanAll(); err != nil {
		log.Printf("[knowledge] scan error: %v", err)
	}
	// 重建索引文件
	if err := s.writeIndex(); err != nil {
		log.Printf("[knowledge] write index error: %v", err)
	}
	log.Printf("[knowledge] started with %d items, %d vectors", len(s.items), s.vec.Size())
	return nil
}

func (s *Store) Stop() {}

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

func (s *Store) Add(name, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 解析层级：将 "/" 作为路径分隔符
	category := ""
	leaf := name
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		category = name[:idx]
		leaf = name[idx+1:]
	}
	dirName := sanitize(leaf)
	if category != "" {
		dirName = sanitize(category) + "/" + dirName
	}
	dir := filepath.Join(s.root, dirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create knowledge dir: %w", err)
	}

	path := filepath.Join(dir, "content.md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("write knowledge: %w", err)
	}

	now := time.Now()
	id := sanitize(name)
	k := &Knowledge{
		Name:      id,
		Content:   content,
		Path:      path,
		Category:  sanitize(category),
		Tags:      memory.ExtractKeywords(name + " " + content),
		UpdatedAt: now,
	}
	s.items[id] = k

	vec := s.veczer.Vectorize(name + " " + content)
	s.vec.Insert(id, name+": "+content, vec, map[string]string{
		"name": name, "path": path,
	})
	s.summaries = append(s.summaries, name+" "+content)

	go func() {
		if err := s.writeIndex(); err != nil {
			log.Printf("[knowledge] write index error after adding %s: %v", name, err)
		}
	}()
	log.Printf("[knowledge] added: %s (%d bytes)", name, len(content))
	return nil
}

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
	go func() {
		if err := s.writeIndex(); err != nil {
			log.Printf("[knowledge] write index error after removing %s: %v", name, err)
		}
	}()
	return nil
}

func (s *Store) Stats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]interface{}{
		"knowledge_count": len(s.items),
		"vector_count":    s.vec.Size(),
		"root":            s.root,
		"index_file":      s.indexPath,
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

// BuildTree 从当前知识库构建树状索引（含向量特征）
func (s *Store) BuildTree() *TreeIndex {
	s.mu.RLock()
	defer s.mu.RUnlock()
	root := newTreeIndex("root")
	for _, k := range s.items {
		node := root
		if k.Category != "" {
			parts := strings.Split(k.Category, "/")
			for _, part := range parts {
				if part == "" {
					continue
				}
				if _, ok := node.Children[part]; !ok {
					node.Children[part] = newTreeIndex(part)
				}
				node = node.Children[part]
			}
		}
		// 获取该条目的向量并压缩
		vec := s.veczer.Vectorize(k.Name + " " + k.Content)
		preview := []rune(k.Content)
		previewStr := ""
		if len(preview) > 200 {
			previewStr = string(preview[:200]) + "..."
		} else {
			previewStr = string(preview)
		}
		item := IndexItem{
			Name:    k.Name,
			Preview: previewStr,
			Tags:    k.Tags,
			Vector:  compressVector(vec, 20),
			Size:    len(k.Content),
		}
		node.Items = append(node.Items, item)
	}
	return root
}

// SearchTree 树状搜索：在树节点下搜索，返回按分类聚合的结果
func (s *Store) SearchTree(query string, topK int) map[string][]*Knowledge {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if topK <= 0 {
		topK = 10
	}

	vec := s.veczer.Vectorize(query)
	results := s.vec.Search(vec, topK*2)

	categorized := make(map[string][]*Knowledge)
	for _, r := range results {
		if k, ok := s.items[r.ID]; ok {
			cat := k.Category
			if cat == "" {
				cat = "未分类"
			}
			categorized[cat] = append(categorized[cat], k)
		}
	}

	out := make(map[string][]*Knowledge)
	for cat, items := range categorized {
		if len(items) > topK {
			items = items[:topK]
		}
		out[cat] = items
	}
	return out
}

// writeIndex 写入 .index.json 树状索引文件（含向量和摘要）
func (s *Store) writeIndex() error {
	tree := s.BuildTree()
	data, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.indexPath, data, 0644)
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
		// skip hidden dirs
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		s.scanDir("", entry.Name())
	}

	if len(s.summaries) > 0 {
		s.veczer.Train(s.summaries)
	}

	for _, k := range s.items {
		vec := s.veczer.Vectorize(k.Name + " " + k.Content)
		s.vec.Insert(k.Name, k.Name+": "+k.Content, vec, map[string]string{
			"name": k.Name, "path": k.Path,
		})
	}

	return nil
}

// scanDir 递归扫描目录
// category: 父级路径（从知识库根目录算起），如 "tech/go"
// dirName: 当前目录相对路径（从知识库根目录算起）
func (s *Store) scanDir(category, dirName string) {
	dir := filepath.Join(s.root, dirName)
	contentPath := filepath.Join(dir, "content.md")
	data, err := os.ReadFile(contentPath)
	if err == nil {
		name := dirName
		content := string(data)
		now := time.Now()
		k := &Knowledge{
			Name:      name,
			Content:   content,
			Path:      contentPath,
			Category:  category,
			Tags:      memory.ExtractKeywords(dirName + " " + content),
			UpdatedAt: now,
		}
		s.items[name] = k
		s.summaries = append(s.summaries, name+" "+content)
		return
	}

	// 无 content.md => 是分类目录，递归子目录
	subEntries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, sub := range subEntries {
		if !sub.IsDir() || strings.HasPrefix(sub.Name(), ".") {
			continue
		}
		childDir := dirName + "/" + sub.Name()
		s.scanDir(dirName, childDir)
	}
}

func sanitize(name string) string {
	name = strings.ToLower(name)
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, " ", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	return name
}


