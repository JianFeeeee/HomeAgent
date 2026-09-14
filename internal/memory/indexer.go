package memory

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

type Indexer struct {
	db       *GraphDB
	vec      *vector.Store
	veczer   *vector.TFIDFVectorizer
	mu       sync.RWMutex
	trained  bool
	recalled map[string]bool // 已通过工具调用显式召回的实体名，自动注入时跳过

	// recalledOrder 记录 recalled 的插入顺序，用于超限时按 FIFO 淘汰。
	recalledOrder []string
}

// maxRecalledEntities 是「已召回实体」去重集的上限。
//
// 无上限时它只增不减：进程活得越久，被永久跳过的实体越多，自动注入
// 越来越「沉默」——一个只在长跑进程里才暴露的隐蔽退化。超限后淘汰最旧的
// 名字（允许重新注入），而不是丢弃整个集合。
const maxRecalledEntities = 1024

func NewIndexer(db *GraphDB) *Indexer {
	return &Indexer{
		db:       db,
		vec:      vector.NewStore(),
		veczer:   vector.NewTFIDFVectorizer(TokenizeWords),
		recalled: make(map[string]bool),
	}
}

// MarkRecalled 标记实体名已被工具调用显式召回，后续自动注入时跳过
func (idx *Indexer) MarkRecalled(names ...string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, name := range names {
		if idx.recalled[name] {
			continue
		}
		idx.recalled[name] = true
		idx.recalledOrder = append(idx.recalledOrder, name)
	}
	for len(idx.recalledOrder) > maxRecalledEntities {
		oldest := idx.recalledOrder[0]
		idx.recalledOrder = idx.recalledOrder[1:]
		delete(idx.recalled, oldest)
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

	input := CleanText(userInput)

	// 1. 向量搜索：从实体名向量索引中找到相关实体
	vectorEntities := idx.vectorSearchEntities(input)

	// 2. 关键词搜索：已有逻辑
	keywords := ExtractKeywords(input)
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

	// 过滤已被工具调用显式召回的实体，避免重复注入
	idx.mu.RLock()
	filtered := result.Entities[:0]
	for _, e := range result.Entities {
		if !idx.recalled[e.Name] {
			filtered = append(filtered, e)
		}
	}
	idx.mu.RUnlock()

	ctx := &InjectedContext{
		Entities:  filtered,
		Relations: nil,
	}

	if len(filtered) > 0 {
		summary := buildIndexSummary(filtered)
		ctx.Summary = summary
		ctx.TokenEstimate = estimateTokens(summary) + len(filtered)*8
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
- triples: [{"subject": "实体名", "relation": "关系类型", "object": "目标实体",
            "sentence_text": "原始句子（可选）", "media_digests": ["图片digest（可选）"]}]
  填了 media_digests，日后从这条记忆就能取回当时那张图/那段音频。

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
									"sentence_text": map[string]interface{}{
										"type":        "string",
										"description": "可选：这条三元组的原始句子。填了才能日后从图谱回到原文。",
									},
									"media_digests": map[string]interface{}{
										"type":        "array",
										"description": "可选：这条记忆关联的媒体 digest（对话或 memory_recall 的「关联媒体」里显示的十六进制串，短的即可）。填了以后从这条记忆能取回原图/音频。",
										"items":       map[string]interface{}{"type": "string"},
									},
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
