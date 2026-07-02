package memory

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

type Indexer struct {
	db     *GraphDB
	vec    *vector.Store
	veczer *vector.TFIDFVectorizer
	mu     sync.RWMutex
	trained bool
}

func NewIndexer(db *GraphDB) *Indexer {
	return &Indexer{
		db:     db,
		vec:    vector.NewStore(),
		veczer: vector.NewTFIDFVectorizer(2),
	}
}

// Sync 从图数据库中同步实体名到向量索引
func (idx *Indexer) Sync() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if idx.db == nil {
		return nil
	}

	result, err := idx.db.Recall(nil, nil, 1, "")
	if err != nil || result == nil {
		return err
	}

	// 收集实体名
	var names []string
	for _, e := range result.Entities {
		names = append(names, e.Name)
	}

	if len(names) == 0 {
		return nil
	}

	// 训练向量化器
	idx.veczer.Train(names)

	// 重建向量索引
	idx.vec = vector.NewStore()
	for _, e := range result.Entities {
		vec := idx.veczer.Vectorize(e.Name)
		idx.vec.Insert(fmt.Sprintf("entity_%d", e.ID), e.Name, vec, map[string]string{
			"type": "entity",
			"name": e.Name,
		})
	}

	idx.trained = true
	log.Printf("[indexer] synced %d entities to vector index", len(names))
	return nil
}

type InjectedContext struct {
	Entities      []Entity   `json:"entities"`
	Relations     []Relation `json:"relations"`
	Summary       string     `json:"summary"`
	TokenEstimate int        `json:"token_estimate"`
}

func (idx *Indexer) BuildContext(userInput string) *InjectedContext {
	if idx.db == nil {
		return &InjectedContext{Summary: ""}
	}

	// 1. 向量搜索：从实体名向量索引中找到相关实体
	vectorEntities := idx.vectorSearchEntities(userInput)

	// 2. 关键词搜索：已有逻辑
	keywords := extractKeywords(userInput)
	if len(keywords) == 0 && len(vectorEntities) == 0 {
		keywords = []string{userInput}
	}

	// 合并关键词和向量找到的实体名
	seedNames := make([]string, 0, len(vectorEntities))
	for _, e := range vectorEntities {
		seedNames = append(seedNames, e.Name)
	}
	allKeywords := append(keywords, seedNames...)

	result, err := idx.db.Recall(allKeywords, nil, 2, "")
	if err != nil || result == nil {
		return &InjectedContext{Summary: ""}
	}

	ctx := &InjectedContext{
		Entities:  result.Entities,
		Relations: nil,
	}

	if len(result.Entities) > 0 {
		summary := buildIndexSummary(result.Entities)
		ctx.Summary = summary
		ctx.TokenEstimate = estimateTokens(summary) + len(result.Entities)*8
	} else {
		ctx.Summary = ""
	}

	return ctx
}

// vectorSearchEntities 在实体名向量索引中搜索
func (idx *Indexer) vectorSearchEntities(query string) []Entity {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if !idx.trained || idx.vec.Size() == 0 {
		return nil
	}

	queryVec := idx.veczer.Vectorize(query)
	results := idx.vec.Search(queryVec, 5)

	var entities []Entity
	for _, r := range results {
		if r.Meta != nil && r.Meta["type"] == "entity" {
			entities = append(entities, Entity{Name: r.Meta["name"]})
		}
	}
	return entities
}

func (idx *Indexer) BuildToolPrompt() string {
	return `## 图记忆工具

你有以下工具可以操作长期图记忆系统：

### memory_recall
检索与关键词相关的实体和关系。
参数:
- query_intent: 查询关键词，逗号分隔
- depth: 遍历深度（默认2）

### memory_commit
将三元组写入图记忆。
参数:
- triples: [{"subject": "实体名", "relation": "关系类型", "object": "目标实体"}]

### memory_introspect
查看记忆统计信息。

### memory_purge
删除或修正记忆。
参数:
- criteria: {"subject_contains": "...", "relation_type": "..."}
- mode: "soft" | "supersede"

使用方法：在推理过程中调用对应的 tool，系统会自动执行并返回结果。`
}

func (idx *Indexer) FormatContext(ctx *InjectedContext) string {
	if ctx == nil || len(ctx.Entities) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("【记忆索引】")

	if ctx.Summary != "" {
		b.WriteString(" ")
		b.WriteString(ctx.Summary)
	}

	b.WriteString(fmt.Sprintf(" 索引: "))
	for i, e := range ctx.Entities {
		if i >= 5 {
			b.WriteString("…")
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(e.Name)
		if e.Type != "Concept" {
			b.WriteString("(" + e.Type + ")")
		}
	}

	b.WriteString(" | 需更多细节请用 memory_recall 查询")
	return b.String()
}

func (idx *Indexer) GetToolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_recall",
				"description": "检索图记忆。输入查询意图关键词，返回相关实体和关系。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query_intent": map[string]interface{}{
							"type":        "string",
							"description": "查询意图，支持逗号分隔多个关键词",
						},
						"depth": map[string]interface{}{
							"type":        "integer",
							"description": "遍历深度，默认2",
							"default":     2,
						},
					},
					"required": []string{"query_intent"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_commit",
				"description": "写入图记忆。将三元组列表写入长期记忆。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"triples": map[string]interface{}{
							"type":        "array",
							"description": "三元组列表",
							"items": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"subject":  map[string]interface{}{"type": "string"},
									"relation": map[string]interface{}{"type": "string"},
									"object":   map[string]interface{}{"type": "string"},
								},
								"required": []string{"subject", "relation", "object"},
							},
						},
					},
					"required": []string{"triples"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_introspect",
				"description": "查看图记忆统计信息：实体数量、关系数量、热点实体。",
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
	}
}

func extractKeywords(input string) []string {
	stopWords := map[string]bool{
		"的": true, "了": true, "是": true, "在": true, "有": true,
		"和": true, "就": true, "不": true, "人": true, "都": true,
		"一": true, "一个": true, "上": true, "也": true, "很": true,
		"到": true, "说": true, "要": true, "去": true, "你": true,
		"会": true, "着": true, "没有": true, "看": true, "好": true,
		"自己": true, "这": true, "他": true, "她": true, "它": true,
		"什么": true, "怎么": true, "为什么": true, "如何": true,
	}

	var keywords []string
	seen := make(map[string]bool)

	runes := []rune(input)

	bigram := []rune{}
	for _, r := range runes {
		bigram = append(bigram, r)
		if len(bigram) >= 2 {
			word := string(bigram)
			if !stopWords[word] && !seen[word] {
				seen[word] = true
				keywords = append(keywords, word)
			}
			bigram = bigram[1:]
		}
	}

	if len(keywords) == 0 && len(runes) > 0 {
		keywords = []string{string(runes)}
	}

	if len(keywords) > 5 {
		keywords = keywords[:5]
	}

	return keywords
}

func buildIndexSummary(entities []Entity) string {
	if len(entities) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("关联 %d 个记忆实体", len(entities)))

	topN := 3
	if len(entities) < topN {
		topN = len(entities)
	}
	b.WriteString("，高频：")
	for i := 0; i < topN; i++ {
		if i > 0 {
			b.WriteString("、")
		}
		b.WriteString(entities[i].Name)
	}

	return b.String()
}

func estimateTokens(s string) int {
	return len(s) / 2
}
