package core

import (
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
)

func (a *Agent) executeToolCall(tc agentAPI.ToolCall, channel string) (ret string) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			log.Printf("[agent] tool %s panic: %v\n%s", tc.Name, r, stack)

			if pluginName := a.resolveToolPlugin(tc.Name); pluginName != "" {
				if a.pluginHealth.recordCrash(pluginName) {
					log.Printf("[agent] plugin %s exceeded crash threshold, scheduling reload", pluginName)
				}
			}

			ret = fmt.Sprintf("工具 %s 执行崩溃: %v", tc.Name, r)
		}
	}()

	done := make(chan string, 1)
	go func() {
		done <- a.executeToolCallInner(tc, channel)
	}()

	select {
	case result := <-done:
		return result
	case <-time.After(60 * time.Second):
		log.Printf("[agent] tool %s timed out after 60s", tc.Name)
		return fmt.Sprintf("工具 %s 执行超时（60秒），已取消", tc.Name)
	}
}

func (a *Agent) executeToolCallInner(tc agentAPI.ToolCall, channel string) string {
	switch {
	case tc.Name == "persona_set":
		return a.executePersonaTool(tc)
	case strings.HasPrefix(tc.Name, "memory_"):
		return a.executeMemoryTool(tc)
	case strings.HasPrefix(tc.Name, "social_"):
		return a.executeSocialTool(tc)
	case strings.HasPrefix(tc.Name, "knowledge_"):
		return a.executeKnowledgeTool(tc)
	case strings.HasPrefix(tc.Name, "doc_"):
		return a.executeDocTool(tc)
	case strings.HasPrefix(tc.Name, "output_send__") && strings.HasSuffix(tc.Name, "_help"):
		return a.executeOutputSendHelp(tc)
	case strings.HasPrefix(tc.Name, "output_send__"):
		return a.executeOutputSendTool(tc)
	case tc.Name == "output_list_channels":
		return a.executeOutputListChannels()
	case tc.Name == "input_channels":
		return a.executeInputChannels(tc)
	case tc.Name == "plgreload":
		return a.executePluginReload()
	case tc.Name == "get_plugin_tools":
		pluginName, _ := tc.Arguments["plugin_name"].(string)
		return a.executeGetPluginTools(pluginName)
	case tc.Name == "spawn_child":
		return a.executeSpawnChild(tc, channel)
	case tc.Name == "child_result":
		return a.executeChildResultTool(tc)
	case strings.HasPrefix(tc.Name, "llm_"):
		return a.executeLLMTool(tc)
	case tc.Name == "describe_image":
		return a.executeDescribeImage(tc)
	case tc.Name == "transcribe_audio":
		return a.executeTranscribeAudio(tc)
	case tc.Name == "ocr_image":
		return a.executeOCRImage(tc)
	}

	if a.stageHost != nil {
		if result, err := a.stageHost.ExecuteTool(tc.Name, tc.Arguments); err == nil {
			return fmt.Sprintf("%v", result)
		} else if !strings.Contains(err.Error(), "not found in any plugin") {
			return fmt.Sprintf("工具 %s 执行失败: %v", tc.Name, err)
		}
	}

	if a.tracker != nil {
		a.tracker.PreAction(tc.Name)
	}
	result, err := a.io.ExecuteTool(tc.Name, tc.Arguments)
	if a.tracker != nil {
		if cs := a.tracker.PostAction(tc.Name); cs != nil && len(cs.Files) > 0 {
			log.Printf("[agent] tool %s changed %d files (changeset: %s)", tc.Name, len(cs.Files), cs.ID)
		}
	}
	if err != nil {
		return fmt.Sprintf("工具 %s 执行失败: %v", tc.Name, err)
	}
	return fmt.Sprintf("%v", result)
}

func (a *Agent) executeMemoryTool(tc agentAPI.ToolCall) string {
	g := a.graphMem()
	if g == nil {
		if tc.Name == "memory_document_query" {
			return a.executeDocTool(tc)
		}
		return "图记忆系统不可用"
	}
	// 整理类工具需要**完整内核**的记忆整理面（块/媒体/结构操作）。
	// 轻量内核（驻留子）只有图记忆共同面 ⇒ 这些操作明确不可用，不静默降级。
	requireFull := func() string {
		if a.memory == nil {
			return "本 agent 是轻量内核：只能读写图记忆，记忆整理（合并/删除/清理/编辑/统计）不可用"
		}
		return ""
	}
	switch tc.Name {
	case "memory_recall":
		query, _ := tc.Arguments["query_intent"].(string)
		depth, _ := tc.Arguments["depth"].(float64)
		if depth <= 0 {
			depth = 2
		}
		if query == "" {
			return "请输入查询关键词"
		}
		// 关键词提取：支持逗号分隔和自然语言
		keywords := strings.Split(query, ",")
		if len(keywords) == 1 {
			keywords = memory.ExtractKeywords(query)
		}
		result, err := g.Recall(keywords, nil, int(depth), "")
		if err != nil {
			return fmt.Sprintf("记忆检索失败: %v", err)
		}
		if len(result.Entities) == 0 && len(result.Relations) == 0 {
			return "未找到相关记忆"
		}
		if a.indexer != nil {
			names := make([]string, len(result.Entities))
			for i, e := range result.Entities {
				names[i] = e.Name
			}
			a.indexer.MarkRecalled(names...)
		}
		var parts []string
		parts = append(parts, fmt.Sprintf("找到 %d 个相关实体:", len(result.Entities)))
		for _, e := range result.Entities {
			parts = append(parts, fmt.Sprintf("- %s (提及%d次, 类型:%s)", e.Name, e.MentionCount, e.Type))
		}
		parts = append(parts, fmt.Sprintf("找到 %d 条关系:", len(result.Relations)))
		for i, r := range result.Relations {
			if i >= 10 {
				parts = append(parts, "...更多关系被截断")
				break
			}
			parts = append(parts, fmt.Sprintf("- %s →(%s)→ %s", r.SourceName, r.RelationType, r.TargetName))
		}
		// 命中的关系若挂着媒体块，把媒体说明附在结果末尾。
		//
		// 关系行只有实体名和关系类型，看不出"这条记忆当时还带了一张图"。
		// 媒体块以结构边与句子相连，需经关系→句子反查。
		// 不附上的后果：agent 显式查了图记忆，却仍然不知道有图。
		if mc := a.mediaContextForRelations(result.Relations); mc != "" {
			parts = append(parts, "", "关联媒体:", mc)
		}
		return strings.Join(parts, "\n")

	case "memory_block_merge":
		if msg := requireFull(); msg != "" {
			return msg
		}
		entityA, _ := tc.Arguments["entity_a"].(string)
		entityB, _ := tc.Arguments["entity_b"].(string)
		rounds, _ := tc.Arguments["rounds"].(float64)
		if entityA == "" || entityB == "" || rounds <= 0 {
			return "entity_a、entity_b 和 rounds 不能为空"
		}
		if entityA > entityB {
			entityA, entityB = entityB, entityA
		}
		key := entityA + "||" + entityB
		a.noMergeMu.Lock()
		a.noMergeMarkers[key] = int(rounds)
		a.noMergeMu.Unlock()
		return fmt.Sprintf("已标记「%s」与「%s」在 %d 轮内不合并", entityA, entityB, int(rounds))

	case "memory_commit":
		triplesData, ok := tc.Arguments["triples"].([]interface{})
		if !ok {
			return "参数格式错误，需要 triples 数组"
		}
		var triples []memory.Triple
		for _, td := range triplesData {
			if m, ok := td.(map[string]interface{}); ok {
				t := memory.Triple{
					Subject:      getString(m, "subject"),
					Relation:     getString(m, "relation"),
					Object:       getString(m, "object"),
					SentenceText: getString(m, "sentence_text"),
				}
				// 模型显式关联的媒体：结构化字段随三元组一起提交，
				// 由 commitTriplesWithMedia 变成 L3 一等块并与句子建边——
				// 不再把 marker 写进句子文本。
				if digests := getStringSlice(m, "media_digests"); len(digests) > 0 {
					t.MediaDigests = a.resolveMediaDigests(digests)
					// 块边需要句子作端点。模型没给原句时用三元组本身拼一句
					// 自然语言——不能造一段 marker 文本，那正是被废弃的东西。
					if t.SentenceText == "" && len(t.MediaDigests) > 0 {
						t.SentenceText = fmt.Sprintf("%s%s%s。", t.Subject, t.Relation, t.Object)
					}
				}
				if t.Subject != "" && t.Relation != "" && t.Object != "" {
					triples = append(triples, t)
				}
			}
		}
		if len(triples) == 0 {
			return "没有有效的三元组"
		}
		// remember 工具是用户/模型显式写入，不涉及归档删除，
		// 因此不需要 mediaBound——没有旧引用要释放。
		ec, rc, mb, err := a.commitTriplesWithMedia(triples, string(a.id), 0, nil)
		if err != nil {
			return fmt.Sprintf("记忆写入失败: %v", err)
		}
		if mb > 0 {
			return fmt.Sprintf("已写入 %d 个实体和 %d 条关系，关联 %d 份媒体", ec, rc, mb)
		}
		return fmt.Sprintf("已写入 %d 个实体和 %d 条关系", ec, rc)

	case "memory_introspect":
		if msg := requireFull(); msg != "" {
			return msg
		}
		stats, err := a.memory.Introspect()
		if err != nil {
			return fmt.Sprintf("查询失败: %v", err)
		}
		return fmt.Sprintf("记忆统计: %v", stats)

	case "memory_document_query":
		return a.executeDocTool(tc)

	case "memory_merge":
		if msg := requireFull(); msg != "" {
			return msg
		}
		source, _ := tc.Arguments["source"].(string)
		target, _ := tc.Arguments["target"].(string)
		if source == "" || target == "" {
			return "source 和 target 不能为空"
		}
		count, err := a.memory.MergeEntities(source, target)
		if err != nil {
			return fmt.Sprintf("合并失败: %v", err)
		}
		return fmt.Sprintf("已将「%s」合并到「%s」，source 已彻底删除，%d 条关系已重定向", source, target, count)

	case "memory_delete_entity":
		if msg := requireFull(); msg != "" {
			return msg
		}
		name, _ := tc.Arguments["name"].(string)
		if name == "" {
			return "name 不能为空"
		}
		if err := a.memory.DeleteEntity(name); err != nil {
			return fmt.Sprintf("删除失败: %v", err)
		}
		return fmt.Sprintf("已彻底删除实体「%s」及其所有关联关系", name)

	case "memory_purge":
		if msg := requireFull(); msg != "" {
			return msg
		}
		criteria := make(map[string]string)
		if v, ok := tc.Arguments["subject_contains"].(string); ok && v != "" {
			criteria["subject_contains"] = v
		}
		if v, ok := tc.Arguments["relation_type"].(string); ok && v != "" {
			criteria["relation_type"] = v
		}
		if v, ok := tc.Arguments["target_contains"].(string); ok && v != "" {
			criteria["target_contains"] = v
		}
		mode, _ := tc.Arguments["mode"].(string)
		if mode == "" {
			mode = "soft"
		}
		n, err := a.memory.Purge(criteria, mode)
		if err != nil {
			return fmt.Sprintf("删除图记忆失败: %v", err)
		}

		textRemoved := 0
		if a.textMem != nil {
			if subj, ok := criteria["subject_contains"]; ok && subj != "" {
				textRemoved, _ = a.textMem.PurgeByFilter(func(evt text.Event) bool {
					return strings.Contains(evt.Source, subj) || strings.Contains(evt.Input, subj) || strings.Contains(evt.Response, subj)
				})
			}
		}
		parts := []string{fmt.Sprintf("已%s删除 %d 条图记忆关系", mode, n)}
		if textRemoved > 0 {
			parts = append(parts, fmt.Sprintf("清理 %d 条文本记忆日志", textRemoved))
		}
		return strings.Join(parts, "，")

	case "memory_edit":
		if msg := requireFull(); msg != "" {
			return msg
		}
		oldSubject, _ := tc.Arguments["old_subject"].(string)
		oldRelation, _ := tc.Arguments["old_relation"].(string)
		oldObject, _ := tc.Arguments["old_object"].(string)
		if oldSubject == "" || oldRelation == "" || oldObject == "" {
			return "old_subject、old_relation、old_object 不能为空"
		}
		newSubject, _ := tc.Arguments["new_subject"].(string)
		newRelation, _ := tc.Arguments["new_relation"].(string)
		newObject, _ := tc.Arguments["new_object"].(string)
		if newSubject == "" && newRelation == "" && newObject == "" {
			return "至少提供一个新值（new_subject / new_relation / new_object）"
		}
		if newSubject == "" {
			newSubject = oldSubject
		}
		if newRelation == "" {
			newRelation = oldRelation
		}
		if newObject == "" {
			newObject = oldObject
		}
		n, err := a.memory.Purge(map[string]string{
			"subject_contains": oldSubject,
			"relation_type":    oldRelation,
			"target_contains":  oldObject,
		}, "hard")
		if err != nil {
			return fmt.Sprintf("编辑图记忆失败（删除旧记录）: %v", err)
		}
		triples := []memory.Triple{{
			Subject:  newSubject,
			Relation: newRelation,
			Object:   newObject,
		}}
		ec, rc, err := a.memory.Commit(triples, string(a.id), 0)
		if err != nil {
			return fmt.Sprintf("编辑图记忆失败（写入新记录）: %v", err)
		}

		textReplaced := 0
		if a.textMem != nil && oldSubject != "" {
			textReplaced, _ = a.textMem.ReplaceByFilter(
				func(evt text.Event) bool {
					return strings.Contains(evt.Input, oldSubject) || strings.Contains(evt.Response, oldSubject)
				},
				func(evt text.Event) text.Event {
					evt.Input = strings.ReplaceAll(evt.Input, oldSubject, newSubject)
					evt.Response = strings.ReplaceAll(evt.Response, oldSubject, newSubject)
					return evt
				},
			)
		}
		result := fmt.Sprintf("已编辑记忆：删除 %d 条旧关系，写入 %d 个实体 + %d 条新关系", n, ec, rc)
		if textReplaced > 0 {
			result += fmt.Sprintf("，更新 %d 条文本记忆日志", textReplaced)
		}
		return result

	default:
		return fmt.Sprintf("未知的记忆工具: %s", tc.Name)
	}
}

func (a *Agent) executeSocialTool(tc agentAPI.ToolCall) string {
	if a.social == nil {
		return "人物关系网不可用（social store 未初始化）"
	}
	switch tc.Name {
	case "person_query":
		name, _ := tc.Arguments["name"].(string)
		if name == "" {
			return "请输入人物名称"
		}
		profile, err := a.social.GetPerson(name)
		if err != nil {
			return fmt.Sprintf("查询人物失败: %v", err)
		}
		var parts []string
		parts = append(parts, fmt.Sprintf("▎%s 的档案", name))
		if len(profile.Traits) > 0 {
			parts = append(parts, "【特质】")
			for k, v := range profile.Traits {
				parts = append(parts, fmt.Sprintf("  %s: %s", k, v))
			}
		}
		if len(profile.Relations) > 0 {
			parts = append(parts, "【社交关系】")
			for _, r := range profile.Relations {
				parts = append(parts, fmt.Sprintf("  %s —(%s)—→ %s", name, r.Relation, r.Person))
			}
		}
		if len(profile.Traits) == 0 && len(profile.Relations) == 0 {
			parts = append(parts, "  （尚无记录）")
		}
		return strings.Join(parts, "\n")

	case "person_set_trait":
		name, _ := tc.Arguments["name"].(string)
		trait, _ := tc.Arguments["trait"].(string)
		value, _ := tc.Arguments["value"].(string)
		if name == "" || trait == "" || value == "" {
			return "name、trait、value 都不能为空"
		}
		if err := a.social.SetTrait(name, trait, value); err != nil {
			return fmt.Sprintf("设置特质失败: %v", err)
		}
		return fmt.Sprintf("已记录：%s 的 %s = %s", name, trait, value)

	case "person_relate":
		personA, _ := tc.Arguments["person_a"].(string)
		relation, _ := tc.Arguments["relation"].(string)
		personB, _ := tc.Arguments["person_b"].(string)
		if personA == "" || relation == "" || personB == "" {
			return "person_a、relation、person_b 都不能为空"
		}
		if err := a.social.AddRelation(personA, relation, personB); err != nil {
			return fmt.Sprintf("建立关系失败: %v", err)
		}
		return fmt.Sprintf("已记录：%s —(%s)—→ %s", personA, relation, personB)

	case "person_network":
		name, _ := tc.Arguments["name"].(string)
		depth := int(getFloat(tc.Arguments, "depth"))
		if depth <= 0 {
			depth = 2
		}
		if name == "" {
			return "请输入人物名称"
		}
		profiles, err := a.social.GetNetwork(name, depth)
		if err != nil {
			return fmt.Sprintf("查询社交网络失败: %v", err)
		}
		if len(profiles) == 0 {
			return fmt.Sprintf("未找到 %s 的社交网络", name)
		}
		var parts []string
		parts = append(parts, fmt.Sprintf("▎%s 的社交网络（%d 度）", name, depth))
		for _, p := range profiles {
			if p.Name == name {
				continue
			}
			parts = append(parts, fmt.Sprintf("  · %s", p.Name))
			for k, v := range p.Traits {
				parts = append(parts, fmt.Sprintf("    %s: %s", k, v))
			}
			for _, r := range p.Relations {
				if r.Person != name {
					parts = append(parts, fmt.Sprintf("    —(%s)—→ %s", r.Relation, r.Person))
				}
			}
		}
		return strings.Join(parts, "\n")

	default:
		return fmt.Sprintf("未知的人物工具: %s", tc.Name)
	}
}

func (a *Agent) executeKnowledgeTool(tc agentAPI.ToolCall) string {
	if a.knowledge == nil {
		return "知识库不可用"
	}
	switch tc.Name {
	case "knowledge_search":
		query, _ := tc.Arguments["query"].(string)
		topK := int(getFloat(tc.Arguments, "top_k"))
		if topK <= 0 {
			topK = 5
		}
		if query == "" {
			return "请输入查询关键词"
		}
		results := a.knowledge.Search(query, topK)
		if len(results) == 0 {
			return "未找到相关知识"
		}
		var parts []string
		for i, k := range results {
			if i >= topK {
				break
			}
			label := k.Name
			if k.Category != "" {
				label = k.Category + "/" + k.Name
			}
			parts = append(parts, fmt.Sprintf("[%s]\n%s", label, truncateStr(k.Content, 200)))
		}
		return strings.Join(parts, "\n---\n")

	case "knowledge_create":
		name, _ := tc.Arguments["name"].(string)
		content, _ := tc.Arguments["content"].(string)
		if name == "" || content == "" {
			return "name 和 content 不能为空"
		}
		if err := a.knowledge.Add(name, content); err != nil {
			return fmt.Sprintf("知识创建失败: %v", err)
		}
		return fmt.Sprintf("知识「%s」已创建并向量化索引（%d 字符）", name, len(content))

	case "knowledge_list":
		tree := a.knowledge.BuildTree()
		return formatTree(tree, 0)

	case "knowledge_delete":
		name, _ := tc.Arguments["name"].(string)
		if name == "" {
			return "name 不能为空"
		}
		if err := a.knowledge.Remove(name); err != nil {
			return fmt.Sprintf("知识删除失败: %v", err)
		}
		return fmt.Sprintf("知识「%s」已删除", name)

	default:
		return fmt.Sprintf("未知的知识工具: %s", tc.Name)
	}
}

func (a *Agent) executeDocTool(tc agentAPI.ToolCall) string {
	if a.docStore == nil {
		return "文档记忆不可用"
	}
	switch tc.Name {
	case "doc_query":
		query, _ := tc.Arguments["query"].(string)
		topK := int(getFloat(tc.Arguments, "top_k"))
		if topK <= 0 {
			topK = 3
		}
		if query == "" {
			return "请输入查询内容"
		}
		docs := a.docStore.Consume(query, topK)
		if len(docs) == 0 {
			return "未找到相关文档记忆"
		}
		var parts []string
		var refs []string
		for i, d := range docs {
			parts = append(parts, fmt.Sprintf("[%d] %s (来源: %s)", i+1, d.Summary, d.Source))
			if len(d.Tags) > 0 {
				parts = append(parts, "  标签: "+strings.Join(d.Tags, ", "))
			}
			content := d.Content
			if len(content) > 2000 {
				content = content[:2000] + "..."
			}
			// 媒体块标签单独一行进冷存事件：正文可能被上面的 2000 字截断，
			// 截掉之后模型就不知道这篇文档带过图。
			if labels := a.blockLabelsForDoc(d); labels != "" {
				content = content + "\n关联媒体: " + labels
			}
			a.context.InsertByTimestamp(ContextEvent{
				Timestamp: d.CreatedAt,
				Source:    "cold_storage",
				Input:     fmt.Sprintf("加载文档记忆: %s", query),
				Response:  content,
			})
			refs = append(refs, fmt.Sprintf("#%d(%s)", i+1, d.Summary))
		}
		return fmt.Sprintf("已加载 %d 篇文档记忆: %s\n(完整内容参见对话时序中 cold_storage 事件)",
			len(docs), strings.Join(refs, ", "))

	case "doc_commit":
		content, _ := tc.Arguments["content"].(string)
		summary, _ := tc.Arguments["summary"].(string)
		if content == "" {
			return "content 不能为空"
		}
		if summary == "" {
			summary = truncateStr(content, 100)
		}

		tagsRaw, _ := tc.Arguments["tags"].([]interface{})
		var tags []string
		for _, t := range tagsRaw {
			if s, ok := t.(string); ok {
				tags = append(tags, s)
			}
		}

		doc := &document.Doc{
			Summary: summary,
			Content: content,
			Tags:    tags,
			Source:  "manual",
		}

		// 模型显式关联的媒体：直接变成文档持有的一等块。
		// 不再往正文写 marker——文档向量会融合这些块的媒体向量，
		// 图片按自己的向量被检索。
		for _, d := range a.resolveMediaDigests(getStringSlice(tc.Arguments, "media_digests")) {
			if b, ok := a.blockFromDigest(d); ok {
				doc.Blocks = append(doc.Blocks, b)
			}
		}

		if err := a.docStore.Insert(doc); err != nil {
			return fmt.Sprintf("文档写入失败: %v", err)
		}
		if n := len(doc.Blocks); n > 0 {
			return fmt.Sprintf("文档已提交 (id: %s, 摘要: %s, 关联 %d 份媒体)", doc.ID, summary, n)
		}
		return fmt.Sprintf("文档已提交 (id: %s, 摘要: %s)", doc.ID, summary)

	default:
		return fmt.Sprintf("未知的文档工具: %s", tc.Name)
	}
}
