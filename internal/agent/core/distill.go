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

func (a *Agent) distillLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] distillLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.distillLoop()
		}
	}()
	if a.docStore == nil && a.memory == nil {
		return
	}
	ticker := time.NewTicker(a.distillInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Printf("[agent] heartbeat distill tick")
			a.distillContext()
			a.reorgGraph()
			a.autoReloadPlugins()
		case <-a.ctx.Done():
			return
		}
	}
}

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



func (a *Agent) reorgGraph() {
	if a.memory == nil {
		return
	}

	log.Printf("[agent] graph reorg start")

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
			triples := docToTriples(doc)
			if len(triples) > 0 {
				ec, rc, err := a.memory.Commit(triples, string(a.id)+"_doc_archival", 0)
				if err != nil {
					log.Printf("[agent] doc→graph archival error: %v", err)
					continue
				}
				log.Printf("[agent] doc→graph: %s → %d entities, %d relations", doc.ID, ec, rc)
				a.docStore.Remove(doc.ID)
			}
		}
	}

	result, err := a.memory.Recall(nil, nil, 1, "")
	if err != nil || result == nil || len(result.Entities) < 2 {
		return
	}

	maxCandidates := 5
	candidates := 0
	for i := 0; i < len(result.Entities) && candidates < maxCandidates; i++ {
		for j := i + 1; j < len(result.Entities) && candidates < maxCandidates; j++ {
			ea, eb := result.Entities[i].Name, result.Entities[j].Name
			if ea > eb {
				ea, eb = eb, ea
			}
			key := ea + "||" + eb
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
			sim := entitySimilarity(result.Entities[i].Name, result.Entities[j].Name)
			if sim > 0.75 {
				candidates++
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

	if candidates > 0 {
		log.Printf("[agent] graph reorg: %d merge candidates sent for LLM decision", candidates)
	} else {
		log.Printf("[agent] graph reorg: no similar entities found")
	}

	a.evaluateGraphQuality()
}

func (a *Agent) evaluateGraphQuality() {
	if a.memory == nil {
		return
	}

	pending, err := a.memory.RecallPending(10)
	if err != nil {
		log.Printf("[agent] recall pending relations error: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}

	var lowQuality []string
	var pendingIDs []int64
	var skipIDs []int64
	for _, r := range pending {
		isLow := false
		if (r.SourceName == "用户" || r.SourceName == "AI") &&
			(r.RelationType == "提及" || r.RelationType == "回应") {
			isLow = true
		} else if r.RelationType == "关联" {
			isLow = true
		} else if r.Confidence < 0.3 && r.RelationType != "" {
			isLow = true
		}
		if !isLow {
			skipIDs = append(skipIDs, r.ID)
			continue
		}
		pendingIDs = append(pendingIDs, r.ID)
		label := fmt.Sprintf("「%s」-「%s」→「%s」", r.SourceName, r.RelationType, r.TargetName)
		if r.RelationType == "关联" {
			label += "(jieba 共现)"
		} else if r.Confidence < 0.3 {
			label += fmt.Sprintf("(confidence=%.1f)", r.Confidence)
		}
		lowQuality = append(lowQuality, label)
	}

	if len(skipIDs) > 0 {
		a.memory.UpdateEvalStatusBatch(skipIDs, "approved")
	}

	if len(lowQuality) == 0 {
		return
	}

	if err := a.memory.UpdateEvalStatusBatch(pendingIDs, "evaluating"); err != nil {
		log.Printf("[agent] mark relations evaluating error: %v", err)
		return
	}

	a.enqueueConsolidationTask(ConsolidationTask{
		Type: "graph_quality",
		Reason: fmt.Sprintf(
			"图数据库中发现 %d 条低质量关系，请逐条判断是否应该删除（保留 = keep，删除 = discard）：\n%s",
			len(lowQuality),
			strings.Join(lowQuality, "\n"),
		),
		Data: map[string]interface{}{
			"candidates": lowQuality,
			"action":     "evaluate_quality",
		},
	})

	log.Printf("[agent] graph quality: %d pending relations sent for LLM evaluation", len(lowQuality))
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

func docToTriples(doc *document.Doc) []memory.Triple {
	var triples []memory.Triple
	if doc == nil {
		return triples
	}

	if doc.Source == "graph" || doc.Source == "" {
		return nil
	}

	triples = append(triples, memory.Triple{
		Subject:     "文档",
		SubjectType: "Concept",
		Relation:    "主题",
		Object:      doc.Summary,
		ObjectType:  "Topic",
		Confidence:  1.0,
	})

	lines := strings.Split(doc.Content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		terms := memory.CutExact(line)
		for i := 0; i < len(terms)-1; i++ {
			triples = append(triples, memory.Triple{
				Subject:     terms[i],
				SubjectType: "Concept",
				Relation:    "关联",
				Object:      terms[i+1],
				ObjectType:  "Concept",
				Confidence:  0.8,
			})
		}
	}

	if doc.Source != "" {
		triples = append(triples, memory.Triple{
			Subject:     "文档",
			SubjectType: "Concept",
			Relation:    "来源",
			Object:      doc.Source,
			ObjectType:  "Source",
			Confidence:  1.0,
		})
	}

	return triples
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
	a.currentOutputChannel = "_consolidation_"

	stageCtx := a.stageCtxFromInput(input, evt.Source, "")
	stageCtx.Extra["output_channel"] = evt.OutputChannel
	a.injectSourceContext(stageCtx, evt)

	_, toolsUsed, _, err := a.process(input, stageCtx)
	if err != nil {
		log.Printf("[agent] consolidation error: %v", err)
		return
	}

	if a.memory != nil {
		if n, err := a.memory.ResolveEvaluating(); err != nil {
			log.Printf("[agent] resolve evaluating relations error: %v", err)
		} else if n > 0 {
			log.Printf("[agent] resolved %d evaluating relations to approved", n)
		}
	}

	log.Printf("[agent] consolidation done (%dms, tools=%v)", time.Since(start).Milliseconds(), toolsUsed)
}
