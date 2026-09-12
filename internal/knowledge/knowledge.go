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
	Preview string             `json:"preview"` // 前 200 字摘要
	Tags    []string           `json:"tags"`
	Vector  map[string]float64 `json:"vector"` // TF-IDF 特征向量（top-N 特征）
	Size    int                `json:"size"`   // 内容总字节数
}

// TreeIndex — 树状索引节点
type TreeIndex struct {
	Name     string                `json:"name"`
	Children map[string]*TreeIndex `json:"children,omitempty"`
	Items    []IndexItem           `json:"items,omitempty"` // 此节点下的知识条目（含向量）
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

	// lex 是**词法路**索引（TF-IDF），与 vec（稠密路：词向量/多模态空间）相互独立。
	//
	// 为何要两路：词向量取平均后各向异性明显——所有文档都挤在语料均值方向附近，
	// 真实 KB（33 条）上自检索 top-1 只有 15%、前两名平均只差 0.013，排序基本是噪声。
	// 融合后 MRR 0.271→0.376、前两名差距 0.013→0.128（同一份数据实测），
	// 且「词都在停用词里」的查询（稠密路给空向量）能靠词法路救回来。
	lex *vector.Store

	mu        sync.RWMutex
	items     map[string]*Knowledge
	summaries []string

	indexPath  string
	vectorizer vector.Vectorizer // 可选：词嵌入向量化器，优先于 TF-IDF
}

func NewStore(root string) *Store {
	return &Store{
		root:      root,
		indexPath: filepath.Join(root, ".index.json"),
		vec:       vector.NewStore(),
		lex:       newLexicalStore(),
		veczer:    vector.NewTFIDFVectorizer(memory.TokenizeWords),
		items:     make(map[string]*Knowledge),
	}
}

// newLexicalStore 造词法路存储。阈值设为 0：TF-IDF 余弦量级只有 0.0~0.2，
// 沿用稠密路的 0.05 会把大量有效候选静默砍掉（实测 MRR 0.307→0.193）。
func newLexicalStore() *vector.Store {
	st := vector.NewStore()
	st.SetMinScore(0)
	return st
}

// SetVectorizer 设置词嵌入向量化器，优先于 TF-IDF
func (s *Store) SetVectorizer(v vector.Vectorizer) {
	s.vectorizer = v
}

// ReindexWithVectorizer 用给定的向量化器重建所有知识条目的向量索引
func (s *Store) ReindexWithVectorizer(v vector.Vectorizer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("[knowledge] reindex with vectorizer (%d items)", len(s.items))
	s.vec = vector.NewStore()
	s.lex = newLexicalStore()
	// 词法路的 IDF 必须建在全语料上（否则 IDF 没意义）
	if len(s.summaries) > 0 {
		s.veczer.Train(s.summaries)
	}
	for _, k := range s.items {
		text := k.Name + " " + k.Content
		s.vec.Insert(k.Name, k.Name+": "+k.Content, v.Vectorize(text), map[string]string{
			"name": k.Name, "path": k.Path,
		})
		s.lex.Insert(k.Name, k.Name+": "+k.Content, s.veczer.Vectorize(text), nil)
	}
	log.Printf("[knowledge] reindex complete (dense=%d lex=%d)", s.vec.Size(), s.lex.Size())
}

// vectorize 优先使用词嵌入向量化器，不可用时回退到 TF-IDF
func (s *Store) vectorize(text string) vector.Vector {
	if s.vectorizer != nil {
		return s.vectorizer.Vectorize(text)
	}
	return s.veczer.Vectorize(text)
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

// 融合权重：稠密路（词向量）与词法路（TF-IDF）。
// 取值由真实 KB 上的权重扫描定（rankdiag_test.go 的 KB_DIAG_SWEEP）：
// 1.0 = 修复前的「只用稠密路」行为，作为对照基线。
var densePathWeight = 0.5

// Search 融合两路召回：稠密路（词向量/多模态空间）+ 词法路（TF-IDF）。
//
// 为何不能只用稠密路：词向量取平均后各向异性明显，真实 KB 上自检索 top-1 只有 15%，
// 前两名平均只差 0.013（等于没区分度）；且全为停用词的查询会得到**空向量**，
// 直接搜不出任何东西（"最近更新" 就撞上这个）。词法路对专名/术语/短查询强，
// 两路各自**按查询内最大值归一化**后加权融合，排序才可信。
func (s *Store) Search(query string, topK int) []*Knowledge {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if topK <= 0 {
		topK = 5
	}
	if s.vec.Size() == 0 && s.lex.Size() == 0 {
		return nil
	}
	// 两路各自对**全部**文档打分：
	//  - 稠密路的特征是维索引，几乎每篇都命中，"候选"就是全量；
	//  - 词法路只召回与查询共词的文档（这正是它的长处：专名/术语）。
	// 为何不先截候选再融合：截断后只能拿**候选内**最大值归一化，路与路之间的
	// 相对权重就随候选集漂移——实测同一份 KB 上自检索 MRR 从 0.376 掉到 0.197。
	// KB 规模下全量 cosine 的代价可忽略；真到数万条再上 ANN 也不迟。
	denseHits := s.vec.SearchScored(s.vectorize(query), s.vec.Size())
	lexHits := s.lex.SearchScored(s.veczer.Vectorize(query), s.lex.Size())
	if len(denseHits) == 0 && len(lexHits) == 0 {
		return nil
	}

	scores := make(map[string]float64, len(denseHits)+len(lexHits))
	addPath := func(hits []vector.DocVectorHit, weight float64) {
		max := 0.0
		for _, h := range hits {
			if h.Score > max {
				max = h.Score
			}
		}
		if max <= 0 {
			return // 该路对这条查询没有信号（如空向量），全量让给另一路
		}
		for _, h := range hits {
			scores[h.Doc.ID] += weight * h.Score / max
		}
	}
	addPath(denseHits, densePathWeight)
	addPath(lexHits, 1-densePathWeight)

	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j] // 分数相同时按名字定序（保证结果可重复）
	})

	var out []*Knowledge
	for _, id := range ids {
		if k, ok := s.items[id]; ok {
			out = append(out, k)
		}
		if len(out) >= topK {
			break
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

	// 覆盖同名条目时必须先摘掉旧向量。
	//
	// vector.Store.Insert 是**追加**语义（s.docs = append + index.Add），不按 id
	// 去重。少了这一步，更新一条知识会在向量索引里留下上一版的副本：条目数看起来
	// 是对的，只有向量数比条目数多——而检索可能因此命中已被替换掉的旧内容。
	s.vec.Remove(id)

	text := name + " " + content
	vec := s.vectorize(text)
	s.vec.Insert(id, name+": "+content, vec, map[string]string{
		"name": name, "path": path,
	})
	// 词法路同样去重后重建这条；IDF 统计沿用现有语料（重启时 scanAll 会全量重训）
	s.lex.Remove(id)
	s.lex.Insert(id, name+": "+content, s.veczer.Vectorize(text), nil)
	s.summaries = append(s.summaries, name+" "+content)

	if err := s.writeIndexLocked(); err != nil {
		log.Printf("[knowledge] write index error after adding %s: %v", name, err)
	}
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

	vec := s.vectorize(query)
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
	s.lex.Remove(id)
	if err := s.writeIndexLocked(); err != nil {
		log.Printf("[knowledge] write index error after removing %s: %v", name, err)
	}
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
	return s.buildTreeLocked()
}

// buildTreeLocked 与 BuildTree 同义，但**不取锁**——供已持写锁的路径调用。
// 为什么需要：writeIndex 会走 BuildTree（RLock），而 Add/Remove 持的是写锁，
// 直接调用会死锁；此前就是因此把索引写丢进了无追踪的 goroutine 里，
// 结果是「失败只打日志」+ 与调用方（含测试的临时目录清理）竞态。
func (s *Store) buildTreeLocked() *TreeIndex {
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
		vec := s.vectorize(k.Name + " " + k.Content)
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

	vec := s.vectorize(query)
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writeIndexLocked()
}

// writeIndexLocked 与 writeIndex 同义但**不取锁**（调用方已持锁）。
func (s *Store) writeIndexLocked() error {
	tree := s.buildTreeLocked()
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

	s.lex = newLexicalStore()
	for _, k := range s.items {
		text := k.Name + " " + k.Content
		s.vec.Insert(k.Name, k.Name+": "+k.Content, s.vectorize(text), map[string]string{
			"name": k.Name, "path": k.Path,
		})
		s.lex.Insert(k.Name, k.Name+": "+k.Content, s.veczer.Vectorize(text), nil)
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
