package core

import (
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"time"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
	"gitcode.com/JianFeeeee/HomeAgent/internal/nlp"
)

type ConsolidationTask struct {
	Type   string      `json:"type"`
	Reason string      `json:"reason"`
	Data   interface{} `json:"data"`
}

func (a *Agent) enqueueConsolidationTask(task ConsolidationTask) {
	msg := fmt.Sprintf(
		"【记忆整理任务】\n类型: %s\n说明: %s\n\n注意：\n1. 仅使用 memory_merge 合并实体，或使用 memory_block_merge 标记不合并\n2. 不要使用 memory_commit 写入新的三元组\n3. 不要从这段任务文本中提取任何信息写入图库\n4. 只需要做出合并/不合并的判断并执行对应工具",
		task.Type, task.Reason,
	)
	a.injectSelf(msg)
	log.Printf("[agent] enqueued consolidation task: %s", task.Reason)
}

// ──────────────────────────────────────────────
// 四个独立心跳循环，各自拥有独立的 ticker 和配置
// ──────────────────────────────────────────────

// distillLoop 上下文裁剪（L1→L2），使用 distillInterval
func (a *Agent) distillLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] distillLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.distillLoop()
		}
	}()
	if a.docStore == nil {
		return
	}
	ticker := time.NewTicker(a.distillInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Printf("[agent] heartbeat distill tick")
			a.distillContext()
			a.autoReloadPlugins()
		case <-a.ctx.Done():
			return
		}
	}
}

// archiveLoop 冷文档归档（L2→L3），使用 archiveInterval
func (a *Agent) archiveLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] archiveLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.archiveLoop()
		}
	}()
	if a.memory == nil {
		return
	}
	ticker := time.NewTicker(a.archiveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Printf("[agent] heartbeat archive tick")
			a.archiveColdDocs()
		case <-a.ctx.Done():
			return
		}
	}
}

// mergeLoop 实体合并检测（GraphDB → LLM 裁决），使用 mergeInterval
func (a *Agent) mergeLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] mergeLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.mergeLoop()
		}
	}()
	if a.memory == nil {
		return
	}
	ticker := time.NewTicker(a.mergeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Printf("[agent] heartbeat merge tick")
			a.detectEntityMerge()
		case <-a.ctx.Done():
			return
		}
	}
}

// reviewLoop 关系复审（GraphDB → ClearSentenceID → CleanupOrphanedSentences），使用 reviewInterval
func (a *Agent) reviewLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] reviewLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.reviewLoop()
		}
	}()
	if a.memory == nil {
		return
	}
	ticker := time.NewTicker(a.reviewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Printf("[agent] heartbeat review tick")
			a.reviewRelations()
		case <-a.ctx.Done():
			return
		}
	}
}

// ──────────────────────────────────────────────
// 蒸馏逻辑
// ──────────────────────────────────────────────

func (a *Agent) distillContext() {
	if a.docStore == nil {
		return
	}
	n := a.context.Len()
	if n > a.maxContextSize*2 {
		archived := a.context.Prune("", a.maxContextSize, a.docStore)
		if archived > 0 {
			log.Printf("[agent] distill: pruned %d low-relevance events to document memory (total=%d)", archived, n)
		}
	}
}

// ──────────────────────────────────────────────
// 冷文档归档：docStore → GraphDB (L3→L4)
// ──────────────────────────────────────────────

func (a *Agent) archiveColdDocs() {
	if a.memory == nil {
		return
	}

	log.Printf("[agent] cold doc archival start")

	if a.indexer != nil {
		if err := a.indexer.Sync(); err != nil {
			log.Printf("[agent] indexer sync error: %v", err)
		}
	}

	if a.docStore != nil {
		a.docStore.Reindex()
	}

	if a.docStore != nil {
		coldDocs := a.docStore.FindColdDocs(72*time.Hour, 2)
		for _, doc := range coldDocs {
			triples := docToTriples(doc, a.embedder)
			if len(triples) == 0 {
				continue
			}
			ec, rc, blocks, err := a.commitTriplesWithMedia(triples, string(a.id)+"_doc_archival", 0, doc.Blocks)
			if err != nil {
				log.Printf("[agent] doc→graph archival error: %v", err)
				continue
			}

			// 归档的实质是「信息从 L2 搬到 L3」。一条实体、一条关系都没写进
			// 图库时，信息并没有搬过去，此时删文档等于直接丢数据。
			//
			// 这不是理论情形：Commit 会静默跳过实体名不合法的三元组
			//（validEntityName 要求 2–50 字符），而 LLM 生成的长描述几乎
			// 提不出合规实体名——实测 456 字图片描述得到 0 entities 0
			// relations，随后文档被删、媒体引用被释放、blob 被 GC 清掉，
			// 图片与描述彻底消失。保留文档，下一轮再试。
			if ec == 0 && rc == 0 {
				log.Printf("[agent] doc→graph: %s 未写入任何实体/关系，保留文档待下轮重试"+
					"（三元组 %d 条全被实体名校验拒绝）", doc.ID, len(triples))
				continue
			}
			log.Printf("[agent] doc→graph: %s → %d entities, %d relations, %d blocks", doc.ID, ec, rc, blocks)

			// 文档持有的一等块写入 L3，并以 document --contains--> block 边关联；
			// 块 ID 原样保留（迁移而非重建）。块迁走后删除文档即完成迁移。
			if len(doc.Blocks) > 0 {
				if bound := a.linkBlocksToDocument(doc.ID, doc.Blocks); bound != len(doc.Blocks) {
					log.Printf("[agent] doc→graph: %s 块迁移不完整 (%d/%d)，保留文档待下轮重试",
						doc.ID, bound, len(doc.Blocks))
					continue
				}
			}
			a.docStore.Remove(doc.ID)
		}
	}
}

// ──────────────────────────────────────────────
// 实体合并检测：GraphDB → LLM 裁决
// ──────────────────────────────────────────────

func (a *Agent) detectEntityMerge() {
	if a.memory == nil {
		return
	}

	log.Printf("[agent] entity merge detection start")

	result, err := a.memory.Recall(nil, nil, 1, "")
	if err != nil || result == nil || len(result.Entities) < 2 {
		return
	}

	llmCandidates := 0
	maxCandidates := 5

	for i := 0; i < len(result.Entities) && llmCandidates < maxCandidates; i++ {
		for j := i + 1; j < len(result.Entities) && llmCandidates < maxCandidates; j++ {
			ea, eb := result.Entities[i].Name, result.Entities[j].Name
			if ea > eb {
				ea, eb = eb, ea
			}
			key := ea + "||" + eb

			// 跳过已标记"不合并"的实体对
			a.noMergeMu.Lock()
			rounds, ok := a.noMergeMarkers[key]
			if ok {
				rounds--
				if rounds <= 0 {
					delete(a.noMergeMarkers, key)
				} else {
					a.noMergeMarkers[key] = rounds
				}
			}
			a.noMergeMu.Unlock()
			if ok {
				continue
			}

			// 复合相似度：字符二元组 + 语义向量（仅增强检测，不做自动合并）
			sim := entitySimilarity(result.Entities[i].Name, result.Entities[j].Name)
			semSim := entitySemanticSimilarity(result.Entities[i].Name, result.Entities[j].Name, a.embedder)
			if semSim > sim {
				sim = semSim
			}

			if sim > 0.75 {
				llmCandidates++
				a.enqueueConsolidationTask(ConsolidationTask{
					Type: "entity_merge",
					Reason: fmt.Sprintf(
						"实体「%s」(类型:%s, 提及%d次) 与「%s」(类型:%s, 提及%d次) 相似度 %.0f%%，可能指代同一事物，请判断是否需要合并",
						result.Entities[i].Name, result.Entities[i].Type, result.Entities[i].MentionCount,
						result.Entities[j].Name, result.Entities[j].Type, result.Entities[j].MentionCount,
						sim*100,
					),
					Data: map[string]interface{}{
						"entity_a":          result.Entities[i].Name,
						"entity_a_type":     result.Entities[i].Type,
						"entity_a_mentions": result.Entities[i].MentionCount,
						"entity_b":          result.Entities[j].Name,
						"entity_b_type":     result.Entities[j].Type,
						"entity_b_mentions": result.Entities[j].MentionCount,
						"similarity":        sim,
					},
				})
			}
		}
	}

	if llmCandidates > 0 {
		log.Printf("[agent] entity merge: %d merge candidates sent for LLM decision", llmCandidates)
	} else {
		log.Printf("[agent] entity merge: no similar entities found")
	}
}

// ──────────────────────────────────────────────
// 关系复审：GraphDB → ClearSentenceID → CleanupOrphanedSentences
// ──────────────────────────────────────────────

func (a *Agent) reviewRelations() {
	if a.memory == nil {
		return
	}

	log.Printf("[agent] relation review start")

	reviewCount := 0
	const maxReviewBatch = 5
	relResult, err := a.memory.Recall(nil, nil, 1, "")
	if err != nil || relResult == nil {
		return
	}
	for _, rel := range relResult.Relations {
		if reviewCount >= maxReviewBatch {
			break
		}
		if rel.SentenceID == 0 || rel.SentenceText == "" {
			continue
		}

		a.enqueueConsolidationTask(ConsolidationTask{
			Type: "relation_review",
			Reason: fmt.Sprintf(
				"【关系复审】原始句子: '%s'\n当前三元组: (%s → %s → %s) 置信度 %.2f\n请判断是否需要修正（如相对引用未解析、主宾颠倒、噪音三元组等），如需修正请用 memory_edit 工具",
				rel.SentenceText, rel.SourceName, rel.RelationType, rel.TargetName, rel.Confidence,
			),
			Data: map[string]interface{}{
				"relation_id":   rel.ID,
				"source":        rel.SourceName,
				"relation_type": rel.RelationType,
				"target":        rel.TargetName,
				"confidence":    rel.Confidence,
				"sentence":      rel.SentenceText,
			},
		})

		// 清除句子引用（复审后解除关联）
		if err := a.memory.ClearSentenceID(rel.ID); err != nil {
			log.Printf("[agent] clear sentence_id for relation %d: %v", rel.ID, err)
		}
		reviewCount++
	}

	if reviewCount > 0 {
		// 清理无引用的句子
		if deleted, err := a.memory.CleanupOrphanedSentences(); err != nil {
			log.Printf("[agent] cleanup orphaned sentences: %v", err)
		} else if deleted > 0 {
			log.Printf("[agent] cleanup %d orphaned sentences", deleted)
		}
		log.Printf("[agent] relation review: %d relations sent for review", reviewCount)
	}
}

// entitySemanticSimilarity 使用词嵌入向量余弦相似度计算实体名语义相似度
func entitySemanticSimilarity(a, b string, embedder *memory.StaticEmbedder) float64 {
	if a == "" || b == "" || embedder == nil || !embedder.Loaded() {
		return 0
	}
	va := embedder.Vectorize(a)
	vb := embedder.Vectorize(b)
	if len(va) == 0 || len(vb) == 0 {
		return 0
	}
	return vector.CosineSimilarity(va, vb)
}

func entitySimilarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1.0
	}
	runesA, runesB := []rune(a), []rune(b)
	if len(runesA) < 2 || len(runesB) < 2 {
		if len(runesA) == len(runesB) && len(runesA) == 1 {
			if runesA[0] == runesB[0] {
				return 1.0
			}
		}
		return 0
	}

	setA := make(map[string]bool)
	for i := 0; i < len(runesA)-1; i++ {
		setA[string(runesA[i:i+2])] = true
	}

	setB := make(map[string]bool)
	for i := 0; i < len(runesB)-1; i++ {
		setB[string(runesB[i:i+2])] = true
	}

	intersect := 0
	for bg := range setA {
		if setB[bg] {
			intersect++
		}
	}

	union := len(setA) + len(setB) - intersect
	if union <= 0 {
		return 0
	}

	return float64(intersect) / float64(union)
}

func docToTriples(doc *document.Doc, embedder nlp.Vectorizer) []memory.Triple {
	var triples []memory.Triple
	if doc == nil {
		return triples
	}

	if doc.Source == "graph" || doc.Source == "" {
		return nil
	}

	isArchivedContext := doc.Meta != nil && doc.Meta["is_archived_context"] == "true"

	// 文档元数据:仅当 summary 合理(非空、非模板化、长度适中)时才写「主题」
	if !isArchivedContext && doc.Summary != "" && len([]rune(doc.Summary)) < 80 && !isTemplateSummary(doc.Summary) {
		triples = append(triples, memory.Triple{
			Subject:     "文档",
			SubjectType: "Concept",
			Relation:    "主题",
			Object:      doc.Summary,
			ObjectType:  "Topic",
			Confidence:  1.0,
		})
	}

	// 媒体不再参与三元组：它作为一等块由 linkBlocksToDocument
	// 写入 L3 并以 document --contains--> block 边关联，
	// 不经过文本描述与 NLP 提取器。

	// NLP 通用提取
	e := nlp.NewExtractor(nil)
	if embedder != nil {
		e.SetEmbedder(embedder)
	}
	result := e.Extract(doc.Content)
	if result != nil {
		for _, nt := range result.Triples {
			mt := nlp.ToMemoryTriple(nt)
			if mt.Subject != "" && mt.Relation != "" && mt.Object != "" {
				triples = append(triples, mt)
			}
		}
	}

	// 仅当来源非归档上下文且非空时写「来源」——归档文档写死模板三元组属于垃圾
	if doc.Source != "" && doc.Source != "context_archived" {
		triples = append(triples, memory.Triple{
			Subject:     "文档",
			SubjectType: "Concept",
			Relation:    "来源",
			Object:      doc.Source,
			ObjectType:  "Source",
			Confidence:  1.0,
		})
	}

	// 噪音闸门：NLP 提取器不认常用词（「结果 / 什么 / 待命」都能当主语），
	// 而落库闸门 validEntityName 只管名字像不像名字。这一层是防止
	// 「每个文档的常用词都变成实体」的唯一防线（CutExact 时代的那层已随
	// 提取器换代丢失，见 memory.IsNoiseEntity 的说明）。
	return memory.FilterNoiseTriples(triples)
}

// isTemplateSummary 识别 summarizeEntries 生成的模板化摘要
// （形如「来自 N 个来源的 M 条对话 (src1, src2) 涉及: kw1, kw2」），
// 这类摘要无独立信息量,不应作为「主题」实体写入图库。
func isTemplateSummary(s string) bool {
	if s == "" {
		return true
	}
	return strings.HasPrefix(s, "来自 ") && strings.Contains(s, "条对话")
}

func (a *Agent) emitMemoryCandidate(source, input, response string, toolResults []ToolResultItem, toolsUsed []string) {
	a.io.EmitOutput("memory", "memory_candidate", map[string]interface{}{
		"source":       source,
		"input":        input,
		"response":     response,
		"tool_results": toolResults,
		"tools_used":   toolsUsed,
		"agent_id":     string(a.id),
		"timestamp":    time.Now().Unix(),
	})
}

func (a *Agent) processConsolidation(evt *agentIO.InputEvent, input string) {
	start := time.Now()

	stageCtx := a.stageCtxFromInput(input, evt.Source, "")
	stageCtx.Extra["output_channel"] = evt.OutputChannel
	a.injectSourceContext(stageCtx, evt)

	_, toolsUsed, _, err := a.process(input, stageCtx)
	if err != nil {
		log.Printf("[agent] consolidation error: %v", err)
		return
	}

	log.Printf("[agent] consolidation done (%dms, tools=%v)", time.Since(start).Milliseconds(), toolsUsed)
}
