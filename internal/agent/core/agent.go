package core

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentPkg "gitcode.com/JianFeeeee/HomeAgent/internal/agent"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/plugin/sdk"
	"gitcode.com/JianFeeeee/HomeAgent/internal/skill"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

// ContextEvent 和 RelevanceContext 定义在 context.go

// Agent — 单 agent，不区分会话/实例
type Agent struct {
	mu           sync.Mutex
	id           types.AgentID
	provider     agentAPI.Provider
	io           *agentIO.IOManager
	memory       *memory.GraphDB
	indexer      *memory.Indexer
	skills       *skill.Manager
	tracker      *tracker.Tracker
	context      *RelevanceContext
	systemPrompt string
	ctx          context.Context
	cancel       context.CancelFunc
	maxTurns     int

	// 文档记忆（第二层）
	docStore *document.Store

	// 知识库
	knowledge *knowledge.Store

	// 人格设定
	personality *agentPkg.Personality

	// 插件注册表（用于 plgreload）
	pluginReg *plugin.Registry
	pluginDir string

	// 定期心跳蒸馏
	distillInterval time.Duration

	// 上下文裁剪：活跃上下文最大条数，超出按相关性裁剪
	maxContextSize int

	// 当前请求的输出通道（mutex 保护，process() 内独占）
	currentOutputChannel string

	// 阶段管道：插件消息流编辑
	stageHost *StageHost
	eventBus  *events.Bus
}

type AgentConfig struct {
	ID           types.AgentID
	SystemPrompt string
	Provider     agentAPI.Provider
	IO           *agentIO.IOManager
	Memory       *memory.GraphDB
	Indexer      *memory.Indexer
	Skills       *skill.Manager
	Tracker      *tracker.Tracker
	MaxToolTurns int

	DocStore        *document.Store
	Knowledge       *knowledge.Store
	Personality     *agentPkg.Personality
	PluginReg       *plugin.Registry
	PluginDir       string
	DistillInterval time.Duration
	MaxContextSize  int              // 活跃上下文最大条数，超出按相关性裁剪
	ContextSavePath string           // 上下文持久化路径，空则不持久化
	StageHost     *StageHost
	EventBus      *events.Bus
}

func New(cfg AgentConfig) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	if cfg.MaxToolTurns <= 0 {
		cfg.MaxToolTurns = 10
	}
	if cfg.DistillInterval <= 0 {
		cfg.DistillInterval = 30 * time.Minute
	}
	if cfg.MaxContextSize <= 0 {
		cfg.MaxContextSize = 30
	}
	return &Agent{
		id:              cfg.ID,
		provider:        cfg.Provider,
		io:              cfg.IO,
		memory:          cfg.Memory,
		indexer:         cfg.Indexer,
		skills:          cfg.Skills,
		tracker:         cfg.Tracker,
		context:         NewRelevanceContext(cfg.ContextSavePath),
		systemPrompt:    cfg.SystemPrompt,
		ctx:             ctx,
		cancel:          cancel,
		maxTurns:        cfg.MaxToolTurns,
		docStore:        cfg.DocStore,
		knowledge:       cfg.Knowledge,
		personality:     cfg.Personality,
		pluginReg:       cfg.PluginReg,
		pluginDir:       cfg.PluginDir,
		distillInterval:  cfg.DistillInterval,
		maxContextSize:   cfg.MaxContextSize,
		stageHost:        cfg.StageHost,
		eventBus:         cfg.EventBus,
	}
}

func (a *Agent) Start() {
	go a.eventLoop()
	go a.distillLoop()
	log.Printf("[agent] %s started, waiting for IO interrupts", a.id)
}

func (a *Agent) Stop() {
	a.cancel()
}

func (a *Agent) ID() types.AgentID { return a.id }

func (a *Agent) eventLoop() {
	for {
		select {
		case evt := <-a.io.InputChan():
			a.handleInput(evt)
		case <-a.ctx.Done():
			return
		}
	}
}

func (a *Agent) handleInput(evt *agentIO.InputEvent) {
	switch evt.Type {
	case "text":
		input, _ := evt.Payload["content"].(string)
		if input == "" {
			return
		}
		a.processTextInput(evt, input)

	case "event":
		log.Printf("[agent] event from %s: %v", evt.Source, evt.Payload)

	case "command":
		cmd, _ := evt.Payload["command"].(string)
		log.Printf("[agent] command from %s: %s", evt.Source, cmd)

	default:
		log.Printf("[agent] unknown event type from %s: %s", evt.Source, evt.Type)
	}
}

func (a *Agent) processTextInput(evt *agentIO.InputEvent, input string) {
	start := time.Now()

	// 设置该请求的输出通道（默认 = 输入事件配套的通道）
	a.currentOutputChannel = evt.OutputChannel
	if a.currentOutputChannel == "" {
		a.currentOutputChannel = evt.Source
	}

	// 记忆整理任务：不路由到外部输出通道
	if evt.OutputChannel == "_consolidation_" {
		a.processConsolidation(input)
		return
	}

	// === Stage: on_input — 消息到达，插件可拦截 ===
	stageCtx := a.stageCtxFromInput(input, evt.Source, "")
	a.publishEvent(events.EventRawInput, map[string]interface{}{
		"content": input,
		"source":  evt.Source,
	})
	if a.runStage(sdk.StageOnInput, stageCtx) {
		a.emitResponse(evt, *stageCtx.Response)
		return
	}
	input = stageCtx.RawMessage

	a.context.Append(ContextEvent{
		Timestamp: start,
		Source:    evt.Source,
		Input:     input,
	})

	response, toolsUsed, err := a.process(input, stageCtx)
	if err != nil {
		log.Printf("[agent] process error: %v", err)
		resp := fmt.Sprintf("处理错误: %v", err)
		a.emitResponse(evt, resp)
		a.context.Append(ContextEvent{Timestamp: time.Now(), Source: "agent", Input: input, Response: resp})
		return
	}

	elapsed := time.Since(start)
	log.Printf("[agent] input from %s → response (%dms, tools=%v)", evt.Source, elapsed.Milliseconds(), toolsUsed)

	a.context.Append(ContextEvent{
		Timestamp: time.Now(),
		Source:    "agent",
		Input:     input,
		Response:  response,
		ToolsUsed: toolsUsed,
	})

	// 基于相关性裁剪上下文：保留与当前输入最相关的 maxContextSize 条
	archived := a.context.Prune(response, a.maxContextSize, a.docStore)
	if archived > 0 {
		log.Printf("[agent] pruned %d low-relevance events to document memory", archived)
	}

	a.emitResponse(evt, response)

	a.emitMemoryCandidate(evt.Source, input, response, toolsUsed)
}

func (a *Agent) emitResponse(evt *agentIO.InputEvent, response string) {
	// === Stage: before_output — 最终文本就绪，插件可改写 ===
	stageCtx := &sdk.StageContext{
		FinalText: response,
		Phase:     sdk.StageBeforeOutput,
	}
	a.runStage(sdk.StageBeforeOutput, stageCtx)
	response = stageCtx.FinalText

	// 读取当前输出通道（可能已被 AI 通过 output_set_channel 切换）
	ch := a.currentOutputChannel
	if ch == "" {
		ch = evt.OutputChannel
	}
	if ch == "" {
		ch = evt.Source
	}

	a.io.EmitOutputTo(evt.Source, ch, "text", map[string]interface{}{
		"content":    response,
		"request_id": evt.RequestID,
	})

	if evt.ResponseCh != nil {
		evt.ResponseCh <- &agentIO.OutputEvent{
			RequestID:     evt.RequestID,
			Target:        evt.Source,
			Type:          "text",
			Payload:       map[string]interface{}{"content": response},
			Done:          true,
			OutputChannel: ch,
		}
	}

	// === Stage: after_output — 输出完成，插件只读 ===
	a.publishEvent(events.EventAgentOutput, map[string]interface{}{
		"content": response,
		"channel": ch,
		"source":  evt.Source,
	})
	stageCtx.Phase = sdk.StageAfterOutput
	a.runStage(sdk.StageAfterOutput, stageCtx)
}

// process — 内部处理，带工具循环和阶段管道
func (a *Agent) process(input string, stageCtx *sdk.StageContext) (response string, toolsUsed []string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	memContext := a.buildMemoryContext(input)
	sysPrompt := a.buildSystemPrompt(memContext, input)
	tools := a.buildToolDefs()

	msgs := a.buildMessages(sysPrompt, input)

	log.Printf("[agent] tool call loop start, %d tools, %d context events, personality=%t, docs=%d",
		len(tools), a.context.Len(),
		a.personality != nil && a.personality.Content != "",
		a.docStoreSize())

	// === Stage: pre_action — 上下文就绪，即将调用 LLM ===
	if a.runStage(sdk.StagePreAction, stageCtx) {
		return *stageCtx.Response, toolsUsed, nil
	}
	if len(stageCtx.ContextMsgs) > 0 {
		for _, m := range stageCtx.ContextMsgs {
			role, _ := m["role"].(string)
			content, _ := m["content"].(string)
			if role != "" {
				msgs = append(msgs, agentAPI.Message{Role: role, Content: content})
			}
		}
	}

	for turn := 0; turn < a.maxTurns; turn++ {
		req := &agentAPI.CompletionRequest{
			Messages:   msgs,
			MaxTokens:  4096,
			Tools:      tools,
			ToolChoice: "auto",
			ExtraBody: map[string]interface{}{
				"thinking": map[string]interface{}{"type": "disabled"},
			},
		}

		resp, err := a.provider.Chat(a.ctx, req)
		if err != nil {
			return "", toolsUsed, fmt.Errorf("provider: %w", err)
		}

		// === Stage: post_action — LLM 返回，插件可审查/修改 ===
		stageCtx.LLMText = resp.Content
		stageCtx.ToolCalls = convertToolCalls(resp.ToolCalls)
		if a.runStage(sdk.StagePostAction, stageCtx) {
			return *stageCtx.Response, toolsUsed, nil
		}
		resp.Content = stageCtx.LLMText
		resp.ToolCalls = convertBackToolCalls(stageCtx.ToolCalls)

		if len(resp.ToolCalls) == 0 {
			return resp.Content, toolsUsed, nil
		}

		for _, tc := range resp.ToolCalls {
			toolsUsed = append(toolsUsed, tc.Name)
			log.Printf("[agent] executing tool: %s (id=%s)", tc.Name, tc.ID)

			// === Stage: before_toolcall — 插件可拒绝/改参 ===
			sdkTC := sdk.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
			stageCtx.ToolCalls = []sdk.ToolCall{sdkTC}
			stageCtx.ToolResults = nil
			if a.runStage(sdk.StageBeforeToolcall, stageCtx) {
				result := fmt.Sprintf("工具 %s 已被插件拒绝", tc.Name)
				msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: resp.Content, ToolCalls: []agentAPI.ToolCall{tc}})
				msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
				a.publishEvent(events.EventToolCall, map[string]interface{}{
					"tool":   tc.Name,
					"args":   tc.Arguments,
					"result": result,
					"status": "denied",
				})
				continue
			}
			tc.Arguments = stageCtx.ToolCalls[0].Arguments

			result := a.executeToolCall(tc)
			log.Printf("[agent] tool %s result: %s", tc.Name, truncateStr(result, 100))

			// === Stage: after_toolcall — 插件可改结果 ===
			stageCtx.ToolResults = []sdk.ToolResult{{CallID: tc.ID, Name: tc.Name, Success: true, Result: result}}
			a.runStage(sdk.StageAfterToolcall, stageCtx)
			if len(stageCtx.ToolResults) > 0 {
				if r, ok := stageCtx.ToolResults[0].Result.(string); ok {
					result = r
				}
			}

			msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: resp.Content, ToolCalls: []agentAPI.ToolCall{tc}})
			msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})

			a.publishEvent(events.EventToolCall, map[string]interface{}{
				"tool":   tc.Name,
				"args":   tc.Arguments,
				"result": result,
				"status": "ok",
			})
		}
	}

	return "", toolsUsed, fmt.Errorf("tool execution exceeded %d turns", a.maxTurns)
}

func convertToolCalls(tcs []agentAPI.ToolCall) []sdk.ToolCall {
	if tcs == nil {
		return nil
	}
	result := make([]sdk.ToolCall, len(tcs))
	for i, tc := range tcs {
		result[i] = sdk.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
	}
	return result
}

func convertBackToolCalls(tcs []sdk.ToolCall) []agentAPI.ToolCall {
	if tcs == nil {
		return nil
	}
	result := make([]agentAPI.ToolCall, len(tcs))
	for i, tc := range tcs {
		result[i] = agentAPI.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
	}
	return result
}

func (a *Agent) docStoreSize() int {
	if a.docStore == nil {
		return 0
	}
	s := a.docStore.Stats()
	if n, ok := s["doc_count"]; ok {
		if ni, ok := n.(int); ok {
			return ni
		}
	}
	return 0
}

func (a *Agent) buildMessages(sysPrompt, input string) []agentAPI.Message {
	msgs := []agentAPI.Message{{Role: "system", Content: sysPrompt}}

	ctxStr := a.context.Format()
	if ctxStr != "" {
		msgs = append(msgs, agentAPI.Message{Role: "system", Content: ctxStr})
	}

	msgs = append(msgs, agentAPI.Message{Role: "user", Content: input})
	return msgs
}

func (a *Agent) executeToolCall(tc agentAPI.ToolCall) string {
	switch {
	case strings.HasPrefix(tc.Name, "memory_"):
		return a.executeMemoryTool(tc)
	case strings.HasPrefix(tc.Name, "knowledge_"):
		return a.executeKnowledgeTool(tc)
	case strings.HasPrefix(tc.Name, "doc_"):
		return a.executeDocTool(tc)
	case tc.Name == "output_set_channel":
		return a.executeOutputChannelTool(tc)
	case tc.Name == "output_send":
		return a.executeOutputSendTool(tc)
	case tc.Name == "output_list_channels":
		return a.executeOutputListChannels()
	case tc.Name == "plgreload":
		return a.executePluginReload()
	case tc.Name == "spawn_child":
		return a.executeSpawnChild(tc)
	}

	// 插件工具（通过 SDK RegisterTool 注册）
	if a.stageHost != nil {
		if result, err := a.stageHost.ExecuteTool(tc.Name, tc.Arguments); err == nil {
			return fmt.Sprintf("%v", result)
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
	if a.memory == nil {
		// 即使图记忆不可用，文档记忆仍可查询
		if tc.Name == "memory_document_query" {
			return a.executeDocTool(tc)
		}
		return "图记忆系统不可用"
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
		result, err := a.memory.Recall(strings.Split(query, ","), nil, int(depth), "")
		if err != nil {
			return fmt.Sprintf("记忆检索失败: %v", err)
		}
		if len(result.Entities) == 0 && len(result.Relations) == 0 {
			return "未找到相关记忆"
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
		return strings.Join(parts, "\n")

	case "memory_commit":
		triplesData, ok := tc.Arguments["triples"].([]interface{})
		if !ok {
			return "参数格式错误，需要 triples 数组"
		}
		var triples []memory.Triple
		for _, td := range triplesData {
			if m, ok := td.(map[string]interface{}); ok {
				t := memory.Triple{
					Subject:  getString(m, "subject"),
					Relation: getString(m, "relation"),
					Object:   getString(m, "object"),
				}
				if t.Subject != "" && t.Relation != "" && t.Object != "" {
					triples = append(triples, t)
				}
			}
		}
		if len(triples) == 0 {
			return "没有有效的三元组"
		}
		ec, rc, err := a.memory.Commit(triples, string(a.id), 0)
		if err != nil {
			return fmt.Sprintf("记忆写入失败: %v", err)
		}
		return fmt.Sprintf("已写入 %d 个实体和 %d 条关系", ec, rc)

	case "memory_introspect":
		stats, err := a.memory.Introspect()
		if err != nil {
			return fmt.Sprintf("查询失败: %v", err)
		}
		return fmt.Sprintf("记忆统计: %v", stats)

	case "memory_document_query":
			return a.executeDocTool(tc)

	case "memory_merge":
		source, _ := tc.Arguments["source"].(string)
		target, _ := tc.Arguments["target"].(string)
		if source == "" || target == "" {
			return "source 和 target 不能为空"
		}
		count, err := a.memory.MergeEntities(source, target)
		if err != nil {
			return fmt.Sprintf("合并失败: %v", err)
		}
		return fmt.Sprintf("已将「%s」合并到「%s」，%d 条关系已重定向", source, target, count)

	default:
		return fmt.Sprintf("未知的记忆工具: %s", tc.Name)
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
			parts = append(parts, fmt.Sprintf("[%s]\n%s", k.Name, truncateStr(k.Content, 200)))
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
		names := a.knowledge.List()
		if len(names) == 0 {
			return "知识库为空"
		}
		return "知识分类: " + strings.Join(names, ", ")

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
		docs := a.docStore.Query(query, topK)
		if len(docs) == 0 {
			return "未找到相关文档记忆"
		}
		var parts []string
		for i, d := range docs {
			parts = append(parts, fmt.Sprintf("[%d] %s (来源: %s)", i+1, d.Summary, d.Source))
			if len(d.Tags) > 0 {
				parts = append(parts, "  标签: "+strings.Join(d.Tags, ", "))
			}
		}
		return strings.Join(parts, "\n")

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
			Summary:   summary,
			Content:   content,
			Tags:      tags,
			Source:    "manual",
		}
		if err := a.docStore.Insert(doc); err != nil {
			return fmt.Sprintf("文档写入失败: %v", err)
		}
		return fmt.Sprintf("文档已提交 (id: %s, 摘要: %s)", doc.ID, summary)

	default:
		return fmt.Sprintf("未知的文档工具: %s", tc.Name)
	}
}

func (a *Agent) buildMemoryContext(input string) string {
	if a.indexer == nil {
		return ""
	}
	injected := a.indexer.BuildContext(input)
	return a.indexer.FormatContext(injected)
}

func (a *Agent) buildSystemPrompt(memContext string, userInput string) string {
	prompt := a.systemPrompt
	if prompt == "" {
		prompt = "你是一个智能家庭管家，持续运行。"
	}

	// 人格设定 — 固定，不变
	if a.personality != nil {
		if pp := a.personality.InjectPrompt(); pp != "" {
			prompt += "\n\n" + pp
		}
	}

	// 图记忆上下文（索引摘要）
	if memContext != "" {
		prompt += "\n\n" + memContext
	}

	// 文档记忆 — 查询相关文档摘要注入
	if a.docStore != nil {
		docs := a.docStore.Query(userInput, 3)
		if len(docs) > 0 {
			var parts []string
			parts = append(parts, "【相关记忆文档】")
			for i, d := range docs {
				parts = append(parts, fmt.Sprintf("  [%d] %s", i+1, d.Summary))
			}
			prompt += "\n\n" + strings.Join(parts, "\n")
		}
	}

	if a.skills != nil {
		if sp := a.skills.GetInjectedPrompt(); sp != "" {
			prompt += "\n\n" + sp
		}
	}

	if a.indexer != nil {
		prompt += "\n\n" + a.indexer.BuildToolPrompt()
	}

	return prompt
}

func (a *Agent) buildToolDefs() []interface{} {
	var tools []interface{}

	if a.io != nil {
		for _, td := range a.io.GetAllTools() {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        td.Name,
					"description": td.Description,
					"parameters":  td.Parameters,
				},
			})
		}
	}

	// 插件注册的工具（通过 SDK RegisterTool）
	if a.stageHost != nil {
		for _, td := range a.stageHost.GetToolDefs() {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        td.Name,
					"description": td.Description,
					"parameters":  td.Parameters,
				},
			})
		}
	}

	if a.indexer != nil {
		for _, td := range a.indexer.GetToolDefinitions() {
			tools = append(tools, td)
		}
	}

	// 实体合并工具（心跳检测到冲突时 LLM 使用）
	if a.memory != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_merge",
				"description": "合并两个同义实体：将所有关系从 source 重定向到 target，source 标记为 merged。仅在有明确证据时使用。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"source": map[string]interface{}{"type": "string", "description": "被合并的实体名（合并后消失）"},
						"target": map[string]interface{}{"type": "string", "description": "保留的实体名"},
					},
					"required": []string{"source", "target"},
				},
			},
		})
	}

	// 知识库工具
	if a.knowledge != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "knowledge_search",
				"description": "搜索知识库。输入查询关键词，返回相关知识内容。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{"type": "string", "description": "查询关键词"},
						"top_k": map[string]interface{}{"type": "integer", "description": "返回数量", "default": 5},
					},
					"required": []string{"query"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "knowledge_list",
				"description": "列出知识库中所有知识分类。",
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		})
	}

	// 知识创建工具
	if a.knowledge != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "knowledge_create",
				"description": "创建新知识。将知识写入知识库（knowledge/目录），自动向量化索引。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{"type": "string", "description": "知识名称（用作目录名）"},
						"content": map[string]interface{}{"type": "string", "description": "知识内容，支持 Markdown"},
					},
					"required": []string{"name", "content"},
				},
			},
		})
	}

	// 文档记忆工具
	if a.docStore != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "doc_query",
				"description": "查询文档记忆。输入查询内容，返回相关文档摘要。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{"type": "string", "description": "查询内容"},
						"top_k": map[string]interface{}{"type": "integer", "description": "返回数量", "default": 3},
					},
					"required": []string{"query"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "doc_commit",
				"description": "提交一条文档记忆。将重要信息显式写入文档记忆层。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"content": map[string]interface{}{"type": "string", "description": "文档内容"},
						"summary": map[string]interface{}{"type": "string", "description": "摘要（可选）"},
						"tags": map[string]interface{}{
							"type":        "array",
							"description": "标签列表",
							"items":       map[string]interface{}{"type": "string"},
						},
					},
					"required": []string{"content"},
				},
			},
		})
	}

	// 插件重载工具
	if a.pluginReg != nil && a.pluginDir != "" {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "plgreload",
				"description": "重载 plugins/ 目录的所有插件。扫描目录变更，原子化替换 IO 设备。",
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		})
	}

	// 子任务工具
	tools = append(tools, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "spawn_child",
			"description": "创建一个子 Agent 执行独立任务。子 Agent 使用传统上下文（无持久记忆），任务完成即销毁。适用于需要多步推理但不需要写入长期记忆的场景，例如：计算、分析、生成报告草稿等。",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task": map[string]interface{}{
						"type":        "string",
						"description": "要子 Agent 完成的任务描述。请描述清晰、完整，包含所有必要背景。",
					},
				},
				"required": []string{"task"},
			},
		},
	})

	// 输出通道工具
	tools = append(tools, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "output_set_channel",
			"description": "切换当前对话的输出通道。例如从 voice 切换到 email，后续所有回复将通过新通道发送。",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"channel": map[string]interface{}{
						"type":        "string",
						"description": "输出通道名称: voice (语音), email (邮件), screen (屏幕), http (HTTP)",
						"enum":        []interface{}{"voice", "email", "screen", "http"},
					},
				},
				"required": []string{"channel"},
			},
		},
	})
	tools = append(tools, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "output_list_channels",
			"description": "列出所有可用输出通道及其能力（如 text/file/image/audio）和可调用工具。",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	})
	tools = append(tools, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "output_send",
			"description": "通过指定输出通道立即发送一条消息，不等待主回复。用于异步通知、中间进度等场景。",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"channel": map[string]interface{}{
						"type":        "string",
						"description": "输出通道: voice, email, screen, http",
					},
					"content": map[string]interface{}{
						"type":        "string",
						"description": "消息内容",
					},
				},
				"required": []string{"channel", "content"},
			},
		},
	})

	return tools
}

// ConsolidationTask 心跳检测到的记忆整理任务，通过 IO 发送给 Agent 让 LLM 决策
type ConsolidationTask struct {
	Type   string      `json:"type"`   // "entity_merge", "relation_conflict", "doc_archival"
	Reason string      `json:"reason"` // 人类可读的描述
	Data   interface{} `json:"data"`   // 任务相关数据
}

// enqueueConsolidationTask 将记忆整理任务注入到 Agent 输入队列
func (a *Agent) enqueueConsolidationTask(task ConsolidationTask) {
	msg := fmt.Sprintf("【记忆整理任务】\n类型: %s\n说明: %s", task.Type, task.Reason)
	a.io.InjectTextTo("system", "_consolidation_", msg)
	log.Printf("[agent] enqueued consolidation task: %s", task.Reason)
}

// distillLoop — 定期心跳：上下文→文档 + 图→文档 + 图重整
func (a *Agent) distillLoop() {
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
			a.syncGraphToDocs()
			a.reorgGraph()
		case <-a.ctx.Done():
			return
		}
	}
}

func (a *Agent) distillContext() {
	if a.docStore == nil {
		return
	}
	// 心跳时执行一次安全裁剪（兜底）
	// 上下文的主要裁剪在 processTextInput 中基于相关性执行
	_ = a.context.Len()
}

// syncGraphToDocs — 将图记忆的实体和关系注入文档记忆层
func (a *Agent) syncGraphToDocs() {
	if a.memory == nil || a.docStore == nil {
		return
	}

	// 拉取图记忆统计
	stats, err := a.memory.Introspect()
	if err != nil {
		return
	}

	entityCount, _ := stats["entity_count"].(int)
	if entityCount == 0 {
		return
	}

	// 查询热点实体，生成文档
	result, err := a.memory.Recall(nil, nil, 1, "")
	if err != nil || result == nil {
		return
	}

	if len(result.Entities) == 0 && len(result.Relations) == 0 {
		return
	}

	// 构建摘要文档
	var summaryParts []string
	summaryParts = append(summaryParts, fmt.Sprintf("图记忆快照: %d 个热点实体", len(result.Entities)))
	for _, e := range result.Entities {
		summaryParts = append(summaryParts, fmt.Sprintf("- %s (%s, %d次)", e.Name, e.Type, e.MentionCount))
	}
	if len(result.Relations) > 0 {
		summaryParts = append(summaryParts, "关联关系:")
		for i, r := range result.Relations {
			if i >= 10 {
				break
			}
			summaryParts = append(summaryParts, fmt.Sprintf("  %s →(%s)→ %s", r.SourceName, r.RelationType, r.TargetName))
		}
	}

	doc := &document.Doc{
		Summary:  fmt.Sprintf("图记忆索引 (%d 实体, %d 关系)", len(result.Entities), len(result.Relations)),
		Content:  strings.Join(summaryParts, "\n"),
		Tags:     []string{"graph_memory", "auto_sync"},
		Entities: extractEntityNames(result.Entities),
		Source:   "graph",
	}
	if err := a.docStore.Insert(doc); err != nil {
		log.Printf("[agent] graph→doc sync error: %v", err)
	} else {
		log.Printf("[agent] graph→doc synced: %s", doc.Summary)
	}
}

func extractEntityNames(entities []memory.Entity) []string {
	names := make([]string, len(entities))
	for i, e := range entities {
		names[i] = e.Name
	}
	return names
}

// reorgGraph — 图数据库重整：向量索引更新 + 同义实体合并+消歧
func (a *Agent) reorgGraph() {
	if a.memory == nil {
		return
	}

	log.Printf("[agent] graph reorg start")

	// 1. 同步实体名到向量索引（Indexer 的向量搜索）
	if a.indexer != nil {
		if err := a.indexer.Sync(); err != nil {
			log.Printf("[agent] indexer sync error: %v", err)
		}
	}

	// 2. 更新文档记忆的向量索引
	if a.docStore != nil {
		a.docStore.Reindex()
	}

	// 3. 冷文档→图记忆归化
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

	// 4. 实体同义冲突检测 → 交由 LLM 决策
	result, err := a.memory.Recall(nil, nil, 1, "")
	if err != nil || result == nil || len(result.Entities) < 2 {
		return
	}

	candidates := 0
	for i := 0; i < len(result.Entities); i++ {
		for j := i + 1; j < len(result.Entities); j++ {
			sim := entitySimilarity(result.Entities[i].Name, result.Entities[j].Name)
			if sim > 0.5 {
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
						"entity_a":        result.Entities[i].Name,
						"entity_a_type":   result.Entities[i].Type,
						"entity_a_mentions": result.Entities[i].MentionCount,
						"entity_b":        result.Entities[j].Name,
						"entity_b_type":   result.Entities[j].Type,
						"entity_b_mentions": result.Entities[j].MentionCount,
						"similarity":      sim,
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
}

// entitySimilarity 计算两个实体名的相似度（字符 bigram Jaccard）
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

	intersect := 0
	for i := 0; i < len(runesB)-1; i++ {
		if setA[string(runesB[i:i+2])] {
			intersect++
		}
	}

	union := len(setA) + len(runesB) - 1 - intersect
	if union <= 0 {
		return 0
	}

	return float64(intersect) / float64(union)
}

// docToTriples 将文档转为图记忆三元组
func docToTriples(doc *document.Doc) []memory.Triple {
	var triples []memory.Triple
	if doc == nil {
		return triples
	}

	triples = append(triples, memory.Triple{
		Subject:  "文档",
		Relation: "包含内容",
		Object:   doc.Summary,
	})

	for _, entity := range doc.Entities {
		triples = append(triples, memory.Triple{
			Subject:  "文档",
			Relation: "提及实体",
			Object:   entity,
		})
	}

	for _, tag := range doc.Tags {
		triples = append(triples, memory.Triple{
			Subject:  "文档",
			Relation: "标签",
			Object:   tag,
		})
	}

	if doc.Source != "" {
		triples = append(triples, memory.Triple{
			Subject:  "文档",
			Relation: "来源",
			Object:   doc.Source,
		})
	}

	return triples
}

func (a *Agent) emitMemoryCandidate(source, input, response string, toolsUsed []string) {
	a.io.EmitOutput("memory", "memory_candidate", map[string]interface{}{
		"source":     source,
		"input":      input,
		"response":   response,
		"tools_used": toolsUsed,
		"agent_id":   string(a.id),
		"timestamp":  time.Now().Unix(),
	})
}

// executeOutputChannelTool — AI 切换当前请求的输出通道
// 在 process() 内调用，mutex 保护，只有一个请求在执行
// processConsolidation 处理后台记忆整理任务（不发外部输出）
func (a *Agent) processConsolidation(input string) {
	start := time.Now()
	a.currentOutputChannel = "_consolidation_"
	a.context.Append(ContextEvent{
		Timestamp: start,
		Source:    "system",
		Input:     input,
	})
	response, toolsUsed, err := a.process(input, &sdk.StageContext{RawMessage: input})
	if err != nil {
		log.Printf("[agent] consolidation error: %v", err)
		return
	}
	a.context.Append(ContextEvent{
		Timestamp: time.Now(),
		Source:    "agent",
		Input:     input,
		Response:  response,
		ToolsUsed: toolsUsed,
	})
	_ = a.context.Prune(response, a.maxContextSize, a.docStore)
	// 只写入记忆，不发外部输出
	a.emitMemoryCandidate("system", input, response, toolsUsed)
	log.Printf("[agent] consolidation done (%dms, tools=%v)", time.Since(start).Milliseconds(), toolsUsed)
}

func (a *Agent) executeOutputChannelTool(tc agentAPI.ToolCall) string {
	channel, _ := tc.Arguments["channel"].(string)
	if channel == "" {
		return "请指定输出通道名称，可选: voice, email, screen, http"
	}
	a.currentOutputChannel = channel
	return fmt.Sprintf("输出通道已切换至: %s，后续输出将通过此通道", channel)
}

// executeOutputSendTool — AI 通过指定通道发送消息（校验通道能力）
func (a *Agent) executeOutputSendTool(tc agentAPI.ToolCall) string {
	channel, _ := tc.Arguments["channel"].(string)
	content, _ := tc.Arguments["content"].(string)
	if channel == "" || content == "" {
		return "channel 和 content 不能为空"
	}

	caps := a.io.GetChannelCapabilities(channel)
	if caps == 0 {
		return fmt.Sprintf("通道 [%s] 不存在或不可用。可用通道请用 output_list_channels 查看", channel)
	}
	if !caps.Supports(agentIO.CapText) {
		return fmt.Sprintf("通道 [%s] 不支持文本输出（能力: %s）", channel, caps.String())
	}

	a.io.EmitTextTo("agent_io", channel, content)
	return fmt.Sprintf("已通过 [%s] 通道发送", channel)
}

// executeOutputListChannels — 列出所有可用通道及其能力
func (a *Agent) executeOutputListChannels() string {
	channels := a.io.ListChannels()
	if len(channels) == 0 {
		return "没有可用通道"
	}
	var parts []string
	parts = append(parts, "可用通道:")
	for _, ch := range channels {
		if ch.OutputCaps == 0 {
			continue // 纯输入通道不列出
		}
		parts = append(parts, fmt.Sprintf("  - %s: [%s] %s", ch.Name, ch.OutputCaps.String(), ch.Description))
		for _, t := range ch.Tools {
			parts = append(parts, fmt.Sprintf("     工具: %s - %s", t.Name, t.Description))
		}
	}
	return strings.Join(parts, "\n")
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// executePluginReload — 重载所有插件（原子替换 IO 设备）
func (a *Agent) executePluginReload() string {
	if a.pluginReg == nil {
		return "插件系统未启用"
	}
	msg, err := a.pluginReg.Reload(a.pluginDir)
	if err != nil {
		return fmt.Sprintf("插件重载失败: %v", err)
	}
	return msg
}

// executeSpawnChild 创建子 Agent 执行独立任务
// 子 Agent 使用传统上下文（单轮对话），无持久记忆，任务完即销毁
func (a *Agent) executeSpawnChild(tc agentAPI.ToolCall) string {
	task, _ := tc.Arguments["task"].(string)
	if task == "" {
		return "请提供 task 参数"
	}

	sysPrompt := fmt.Sprintf(`你是 HomeAgent 的子任务助手。
请完成以下任务。完成即可，无需保留记忆或查询历史。
任务: %s`, task)

	msgs := []agentAPI.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: task},
	}

	// 子 Agent 无特殊工具，只保留基础 tool 定义（无记忆/知识/文档工具）
	childTools := []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "output_send",
				"description": "通过指定输出通道发送消息",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"channel": map[string]interface{}{"type": "string", "description": "输出通道"},
						"content": map[string]interface{}{"type": "string", "description": "消息内容"},
					},
					"required": []string{"channel", "content"},
				},
			},
		},
	}

	for turn := 0; turn < 5; turn++ {
		req := &agentAPI.CompletionRequest{
			Messages:   msgs,
			MaxTokens:  4096,
			Tools:      childTools,
			ToolChoice: "auto",
			ExtraBody: map[string]interface{}{
				"thinking": map[string]interface{}{"type": "disabled"},
			},
		}

		resp, err := a.provider.Chat(a.ctx, req)
		if err != nil {
			return fmt.Sprintf("子 Agent 执行失败: %v", err)
		}

		if len(resp.ToolCalls) == 0 {
			return resp.Content
		}

		for _, ct := range resp.ToolCalls {
			var result string
			if ct.Name == "output_send" {
				channel, _ := ct.Arguments["channel"].(string)
				content, _ := ct.Arguments["content"].(string)
				if channel != "" && content != "" {
					a.io.EmitTextTo("child_agent", channel, content)
					result = fmt.Sprintf("已通过 [%s] 通道发送", channel)
				} else {
					result = "channel 和 content 不能为空"
				}
			} else {
				result = fmt.Sprintf("子 Agent 无法调用工具 %s", ct.Name)
			}
			msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: resp.Content, ToolCalls: []agentAPI.ToolCall{ct}})
			msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: ct.ID, Content: result})
		}
	}

	return "子 Agent 执行超时（超过 5 轮）"
}

// runStage — 运行阶段管道，若插件 Response 被设置则返回 true（短路）
func (a *Agent) runStage(stage sdk.Stage, ctx *sdk.StageContext) bool {
	if a.stageHost == nil {
		return false
	}
	ctx.Phase = stage
	a.stageHost.RunStage(stage, ctx)
	return ctx.Response != nil
}

// publishEvent — 发布系统事件
func (a *Agent) publishEvent(evtType events.EventType, payload map[string]interface{}) {
	if a.eventBus == nil {
		return
	}
	a.eventBus.Publish(&events.Event{
		Type:      evtType,
		Source:    string(a.id),
		Payload:   payload,
		Timestamp: time.Now().Unix(),
	})
}

// stageCtxFromInput — 根据输入构建阶段上下文
func (a *Agent) stageCtxFromInput(input, userID, groupID string) *sdk.StageContext {
	return &sdk.StageContext{
		RawMessage: input,
		UserID:     userID,
		GroupID:    groupID,
		Phase:      sdk.StageOnInput,
		Extra:      make(map[string]interface{}),
	}
}

func getFloat(m map[string]interface{}, key string) float64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		}
	}
	return 0
}

func truncateStr(s string, max int) string {
	runes := []rune(s)
	if len(runes) > max {
		return string(runes[:max]) + "..."
	}
	return s
}
