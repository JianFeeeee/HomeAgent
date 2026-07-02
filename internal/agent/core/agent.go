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
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
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

	// 定期心跳蒸馏
	distillInterval time.Duration

	// 上下文裁剪：活跃上下文最大条数，超出按相关性裁剪
	maxContextSize int

	// 当前请求的输出通道（mutex 保护，process() 内独占）
	currentOutputChannel string
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
	MaxContextSize  int // 活跃上下文最大条数，超出按相关性裁剪
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
		context:         NewRelevanceContext(),
		systemPrompt:    cfg.SystemPrompt,
		ctx:             ctx,
		cancel:          cancel,
		maxTurns:        cfg.MaxToolTurns,
		docStore:        cfg.DocStore,
		knowledge:       cfg.Knowledge,
		personality:     cfg.Personality,
		pluginReg:       cfg.PluginReg,
		distillInterval:  cfg.DistillInterval,
		maxContextSize:   cfg.MaxContextSize,
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

	a.context.Append(ContextEvent{
		Timestamp: start,
		Source:    evt.Source,
		Input:     input,
	})

	response, toolsUsed, err := a.process(input)
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
}

// process — 内部处理，带工具循环
func (a *Agent) process(input string) (response string, toolsUsed []string, err error) {
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

		if len(resp.ToolCalls) == 0 {
			return resp.Content, toolsUsed, nil
		}

		for _, tc := range resp.ToolCalls {
			toolsUsed = append(toolsUsed, tc.Name)
			log.Printf("[agent] executing tool: %s (id=%s)", tc.Name, tc.ID)
			result := a.executeToolCall(tc)
			log.Printf("[agent] tool %s result: %s", tc.Name, truncateStr(result, 100))
			msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: resp.Content, ToolCalls: []agentAPI.ToolCall{tc}})
			msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
		}
	}

	return "", toolsUsed, fmt.Errorf("tool execution exceeded %d turns", a.maxTurns)
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

	if a.indexer != nil {
		for _, td := range a.indexer.GetToolDefinitions() {
			tools = append(tools, td)
		}
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
			}
		}
	}

	// 4. 实体向量同义合并
	result, err := a.memory.Recall(nil, nil, 1, "")
	if err != nil || result == nil || len(result.Entities) < 2 {
		return
	}

	merged := 0
	for i := 0; i < len(result.Entities); i++ {
		for j := i + 1; j < len(result.Entities); j++ {
			if isSimilarName(result.Entities[i].Name, result.Entities[j].Name) {
				if result.Entities[i].MentionCount >= result.Entities[j].MentionCount {
					log.Printf("[agent] reorg: merging '%s' → '%s'", result.Entities[j].Name, result.Entities[i].Name)
				} else {
					log.Printf("[agent] reorg: merging '%s' → '%s'", result.Entities[i].Name, result.Entities[j].Name)
				}
				merged++
			}
		}
	}

	if merged > 0 {
		log.Printf("[agent] graph reorg: merged %d similar entities", merged)
	} else {
		log.Printf("[agent] graph reorg: no merges needed")
	}
}

// isSimilarName — 使用字符 bigram Jaccard 相似度判断实体名是否同义
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

func isSimilarName(a, b string) bool {
	if a == b {
		return false // 自带跳过
	}
	runesA, runesB := []rune(a), []rune(b)
	if len(runesA) < 2 || len(runesB) < 2 {
		return false
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
		return false
	}

	jaccard := float64(intersect) / float64(union)
	return jaccard > 0.5
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
