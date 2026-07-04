package core

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentPkg "gitcode.com/JianFeeeee/HomeAgent/internal/agent"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/social"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
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
	providerManager *agentAPI.ProviderManager
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

	// 人物特质与关系网
	social *social.SocialStore

	// 文本记忆（原始对话日志）
	textMem *text.Memory

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

	// 自循环输入通道：核心内部任务（记忆消歧、系统维护），不经过 IO 层
	selfInputCh chan string

	// 子任务异步执行
	childMu      sync.Mutex
	childNextID  int64
	childResults map[string]string

	// 高优先级打断通道：interceptLoop 注入，process() 在工具循环轮次间非阻塞读取
	interceptCh chan string

	// 进行中的 LLM 请求取消函数，interceptLoop 可调用以在请求中打断
	cancelLLM context.CancelFunc
	llmMu     sync.Mutex

	// 模型思考模式（thinking/reasoning）
	thinkingEnabled bool

	// 启动时间
	startTime time.Time

	// 当前轮次的非文本媒体数据（图片/音频），供 describe_image 等工具访问
	pendingMedia map[string]interface{}

	// 非文本输入处理配置
	inputCfg types.InputProcessingConfig
}

type AgentConfig struct {
	ID           types.AgentID
	SystemPrompt string
	Provider     agentAPI.Provider
	ProviderManager *agentAPI.ProviderManager
	IO           *agentIO.IOManager
	Memory       *memory.GraphDB
	Indexer      *memory.Indexer
	Skills       *skill.Manager
	Tracker      *tracker.Tracker
	MaxToolTurns int

	DocStore        *document.Store
	Knowledge       *knowledge.Store
	SocialStore     *social.SocialStore
	TextMemory      *text.Memory
	Personality     *agentPkg.Personality
	PluginReg       *plugin.Registry
	PluginDir       string
	DistillInterval time.Duration
	MaxContextSize  int              // 活跃上下文最大条数，超出按相关性裁剪
	ContextSavePath string           // 上下文持久化路径，空则不持久化
	StageHost     *StageHost
	EventBus      *events.Bus
	ThinkingEnabled bool

	InputProcessing types.InputProcessingConfig // 非文本输入处理配置
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
		startTime:      time.Now(),
		provider:        cfg.Provider,
		providerManager: cfg.ProviderManager,
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
		social:          cfg.SocialStore,
		textMem:         cfg.TextMemory,
		personality:     cfg.Personality,
		pluginReg:       cfg.PluginReg,
		pluginDir:       cfg.PluginDir,
		distillInterval:  cfg.DistillInterval,
		maxContextSize:   cfg.MaxContextSize,
		stageHost:        cfg.StageHost,
		eventBus:         cfg.EventBus,
		selfInputCh:      make(chan string, 64),
		childResults:     make(map[string]string),
		interceptCh:      make(chan string, 64),
		thinkingEnabled:  cfg.ThinkingEnabled,
		inputCfg:         cfg.InputProcessing,
	}
}

func (a *Agent) Start() {
	go a.eventLoop()
	go a.interceptLoop()
	go a.distillLoop()
	log.Printf("[agent] %s started, waiting for IO interrupts", a.id)
}

func (a *Agent) Stop() {
	a.cancel()
}

func (a *Agent) ID() types.AgentID { return a.id }

// SelfInputChan 返回自循环输入通道（只读，供内部测试验证）
func (a *Agent) SelfInputChan() <-chan string {
	return a.selfInputCh
}

// injectSelf 向自循环通道发送内部任务（记忆消歧、系统维护）
// 线程安全，不阻塞发送者（通道缓冲 64）
func (a *Agent) injectSelf(task string) {
	select {
	case a.selfInputCh <- task:
	default:
		log.Printf("[agent] self input channel full, dropping task: %s", truncateStr(task, 80))
	}
}

func (a *Agent) eventLoop() {
	for {
		select {
		case evt := <-a.io.InputChan():
			a.handleInput(evt)
		case task := <-a.selfInputCh:
			a.handleSelfInput(task)
		case <-a.ctx.Done():
			return
		}
	}
}

// interceptLoop 独立 goroutine 监控中断通道。
// 两种路径投递：
//   a) 通过 cancelLLM + interceptCh 直接打断进行中的 LLM 请求
//   b) 通过 a.io.InjectInput() → InputChan → eventLoop（代理空闲时触发新处理循环）
func (a *Agent) interceptLoop() {
	for {
		select {
		case evt := <-a.io.InputInterruptChan():
			text, _ := evt.Payload["content"].(string)
			if text == "" {
				continue
			}
			log.Printf("[agent] interrupt from %s: %s", evt.Source, truncateStr(text, 80))

			// (a) 直接取消进行中的 LLM 请求
			a.llmMu.Lock()
			if a.cancelLLM != nil {
				a.cancelLLM()
				log.Printf("[agent] LLM request cancelled by interrupt")
			}
			a.llmMu.Unlock()

			// 注入拦截通道 — process() 在工具循环中非阻塞读取
			select {
			case a.interceptCh <- text:
			default:
			}

			// (b) 投递为新输入 — 代理空闲时 eventLoop 会消费
			a.io.InjectInput("interrupt", "text", map[string]interface{}{
				"content": fmt.Sprintf("[interrupt] %s: %s", evt.Source, text),
			})

		case <-a.ctx.Done():
			return
		}
	}
}

// handleSelfInput 处理自循环输入（内部任务，不经过 IO 层）
func (a *Agent) handleSelfInput(task string) {
	a.processTextInput(&agentIO.InputEvent{
		Source:        "system",
		Type:          "text",
		Payload:       map[string]interface{}{"content": task},
		OutputChannel: "_consolidation_",
	}, task)
}

func (a *Agent) handleInput(evt *agentIO.InputEvent) {
	switch evt.Type {
	case "text":
		input, _ := evt.Payload["content"].(string)
		if input == "" {
			return
		}
		a.processTextInput(evt, input)

	case "image", "audio":
		a.processMediaInput(evt)

	case "event":
		log.Printf("[agent] event from %s: %v", evt.Source, evt.Payload)

	case "command":
		cmd, _ := evt.Payload["command"].(string)
		log.Printf("[agent] command from %s: %s", evt.Source, cmd)

	default:
		log.Printf("[agent] unknown event type from %s: %s", evt.Source, evt.Type)
	}
}

// processMediaInput 处理图片/音频等非文本输入。
// 将媒体数据附着到对话中，LLM 可通过 describe_image / transcribe_audio 等工具自主处理。
func (a *Agent) processMediaInput(evt *agentIO.InputEvent) {
	start := time.Now()
	a.pendingMedia = evt.Payload
	defer func() { a.pendingMedia = nil }()

	a.currentOutputChannel = evt.OutputChannel
	if a.currentOutputChannel == "" {
		a.currentOutputChannel = evt.Source
	}

	blocks, fallback := a.mediaToBlocks(evt.Payload, evt.Type, evt.Source)

	a.context.Append(ContextEvent{
		Timestamp: start,
		Source:    evt.Source,
		Input:     fallback,
	})

	// stage 上下文携带 blocks，process() 会将其附着到 user message 上
	stageCtx := a.stageCtxFromInput(fallback, evt.Source, "")
	stageCtx.Extra = map[string]interface{}{
		"media_blocks": blocks,
		"media_type":   evt.Type,
	}

	a.publishEvent(events.EventRawInput, map[string]interface{}{
		"content": evt.Payload,
		"source":  evt.Source,
	})

	if a.runStage(sdk.StageOnInput, stageCtx) {
		a.emitResponse(evt, *stageCtx.Response)
		return
	}

	response, toolsUsed, err := a.process(fallback, stageCtx)
	if err != nil {
		log.Printf("[agent] process media error: %v", err)
		resp := fmt.Sprintf("处理错误: %v", err)
		a.emitResponse(evt, resp)
		a.context.Append(ContextEvent{Timestamp: time.Now(), Source: "agent", Input: fallback, Response: resp})
		return
	}

	elapsed := time.Since(start)
	log.Printf("[agent] %s from %s → response (%dms, tools=%v)", evt.Type, evt.Source, elapsed.Milliseconds(), toolsUsed)

	a.context.Append(ContextEvent{
		Timestamp: time.Now(),
		Source:    "agent",
		Input:     fallback,
		Response:  response,
		ToolsUsed: toolsUsed,
	})

	archived := a.context.Prune(response, a.maxContextSize, a.docStore)
	if archived > 0 {
		log.Printf("[agent] pruned %d low-relevance events to document memory", archived)
	}

	a.emitResponse(evt, response)
}

// mediaToBlocks 将媒体 payload 转为多模态 ContentBlock 数组和纯文本 fallback。
// source 是输入通道名，用于生成可读的描述文本（如"从 cli 收到了一张图片"）。
func (a *Agent) mediaToBlocks(payload map[string]interface{}, mediaType string, source string) ([]agentAPI.ContentBlock, string) {
	data, _ := payload["data"].(string)
	mime, _ := payload["mime"].(string)
	url, _ := payload["url"].(string)
	alt, _ := payload["alt"].(string)
	if alt == "" {
		if source == "" {
			source = "unknown"
		}
		alt = fmt.Sprintf("[从 %s 收到了 %s]", source, mediaType)
	}

	var blocks []agentAPI.ContentBlock

	// 文本描述块
	desc := ""
	switch mediaType {
	case "image":
		desc = a.inputCfg.Image.DescribePrompt
		if desc == "" {
			desc = fmt.Sprintf("从 %s 收到了一张图片，请使用 describe_image 工具查看详情。", source)
		}
	case "audio":
		desc = a.inputCfg.Audio.DescribePrompt
		if desc == "" {
			desc = fmt.Sprintf("从 %s 收到了一段音频，请使用 transcribe_audio 工具查看内容。", source)
		}
	}
	blocks = append(blocks, agentAPI.ContentBlock{Type: "text", Text: desc})

	if data != "" || url != "" {
		imgURL := url
		if data != "" {
			if mime == "" {
				mime = "image/png"
			}
			imgURL = "data:" + mime + ";base64," + data
		}
		if mediaType == "image" {
			blocks = append(blocks, agentAPI.ContentBlock{
				Type: "image_url",
				ImageURL: &agentAPI.ImageURL{URL: imgURL, Detail: "auto"},
			})
		} else if mediaType == "audio" {
			blocks = append(blocks, agentAPI.ContentBlock{
				Type: "audio_url",
				AudioURL: &agentAPI.AudioURL{URL: imgURL},
			})
		}
	}

	return blocks, alt
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

	noMemory := false
	if v, ok := evt.Payload["no_memory"].(bool); ok {
		noMemory = v
	}

	// === Stage: on_input — 消息到达，插件可拦截 ===
	stageCtx := a.stageCtxFromInput(input, evt.Source, "")
	stageCtx.NoMemory = noMemory
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

	if !stageCtx.NoMemory {
		a.emitMemoryCandidate(evt.Source, input, response, toolsUsed)
	}
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

	payload := map[string]interface{}{
		"content":    response,
		"request_id": evt.RequestID,
	}
	if stageCtx.ReasoningContent != "" {
		payload["reasoning_content"] = stageCtx.ReasoningContent
	}
	if stageCtx.TokenUsage != nil {
		payload["usage"] = stageCtx.TokenUsage
	}

	a.io.EmitOutputTo(evt.Source, ch, "text", payload)

	if evt.ResponseCh != nil {
		evt.ResponseCh <- &agentIO.OutputEvent{
			RequestID:     evt.RequestID,
			Target:        evt.Source,
			Type:          "text",
			Payload:       payload,
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

	if a.provider == nil {
		return "", nil, fmt.Errorf("agent: no LLM provider configured")
	}

	memContext := a.buildMemoryContext(input)
	sysPrompt := a.buildSystemPrompt(memContext, input)
	tools := a.buildToolDefs()

	msgs := a.buildMessages(sysPrompt, input)
	// 如果 stageCtx 携带多模态 blocks，附着到 user message 上
	if blocks, ok := stageCtx.Extra["media_blocks"].([]agentAPI.ContentBlock); ok && len(blocks) > 0 {
		if len(msgs) > 0 {
			msgs[len(msgs)-1].Blocks = blocks
		}
	}

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
		// === 高优先级打断：每次 LLM 调用前检查拦截通道 ===
		if text := a.drainInterrupt(); text != "" {
			msgs = append(msgs, agentAPI.Message{
				Role:    "system",
				Content: fmt.Sprintf("[打断消息] 用户发来一条紧急消息，请优先处理:\n%s", text),
			})
			log.Printf("[agent] interrupt injected before LLM call (turn %d)", turn)
		}

		eb := map[string]interface{}{}
		if !a.thinkingEnabled {
			eb["thinking"] = map[string]interface{}{"type": "disabled"}
		}
		req := &agentAPI.CompletionRequest{
			Messages:   msgs,
			MaxTokens:  4096,
			Tools:      tools,
			ToolChoice: "auto",
			ExtraBody:  eb,
		}

		// 可取消的 LLM 调用：interceptLoop 通过 cancelLLM 打断进行中的请求
		reqCtx, reqCancel := context.WithCancel(a.ctx)
		a.llmMu.Lock()
		a.cancelLLM = reqCancel
		a.llmMu.Unlock()

		resp, err := a.provider.Chat(reqCtx, req)

		a.llmMu.Lock()
		a.cancelLLM = nil
		a.llmMu.Unlock()
		reqCancel()

		if err != nil {
			return "", toolsUsed, fmt.Errorf("provider: %w", err)
		}

		// === Stage: post_action — LLM 返回，插件可审查/修改 ===
		stageCtx.LLMText = resp.Content
		stageCtx.ReasoningContent = resp.ReasoningContent
		stageCtx.TokenUsage = map[string]int{
			"prompt_tokens":     resp.TokenUsage.Prompt,
			"completion_tokens": resp.TokenUsage.Completion,
			"total_tokens":      resp.TokenUsage.Total,
		}
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
	case strings.HasPrefix(tc.Name, "social_"):
		return a.executeSocialTool(tc)
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
		// 标记已显式召回的实体，后续自动注入时跳过，避免重复
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

	case "memory_purge":
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

		// 也清理文本记忆中匹配源的数据
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
		// 先删旧的，再写新的（图记忆）
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

		// 也编辑文本记忆中匹配的内容
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
		docs := a.docStore.Consume(query, topK)
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
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_purge",
				"description": "删除指定条件的记忆关系。支持按主体、客体、关系类型筛选。谨慎使用。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"subject_contains": map[string]interface{}{"type": "string", "description": "主体名包含的关键词"},
						"relation_type":    map[string]interface{}{"type": "string", "description": "关系类型"},
						"target_contains":  map[string]interface{}{"type": "string", "description": "客体名包含的关键词"},
						"mode":             map[string]interface{}{"type": "string", "description": "soft（标记删除）/ hard（物理删除）", "default": "soft"},
					},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_edit",
				"description": "编辑记忆：删除旧的 relation 并写入新的。例如修正错误的实体名或关系类型。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"old_subject":  map[string]interface{}{"type": "string", "description": "旧主体名"},
						"old_relation": map[string]interface{}{"type": "string", "description": "旧关系类型"},
						"old_object":   map[string]interface{}{"type": "string", "description": "旧客体名"},
						"new_subject":  map[string]interface{}{"type": "string", "description": "新主体名（不填则不变）"},
						"new_relation": map[string]interface{}{"type": "string", "description": "新关系类型（不填则不变）"},
						"new_object":   map[string]interface{}{"type": "string", "description": "新客体名（不填则不变）"},
					},
					"required": []string{"old_subject", "old_relation", "old_object"},
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

	// 人物特质与关系网工具
	if a.social != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "person_query",
				"description": "查询指定人物的完整档案（特质+社交关系）。用于了解一个人的性格、喜好、背景和社交圈。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{"type": "string", "description": "人物名称"},
					},
					"required": []string{"name"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "person_set_trait",
				"description": "记录/更新一个人的特质（性格、喜好、习惯等）。例如：person_set_trait(name=\"张三\", trait=\"喜欢\", value=\"红色\")。如果该特质已存在则覆盖。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name":  map[string]interface{}{"type": "string", "description": "人物名称"},
						"trait": map[string]interface{}{"type": "string", "description": "特质名称，如：喜欢、性格、职业、年龄"},
						"value": map[string]interface{}{"type": "string", "description": "特质值，如：红色、开朗、工程师、25岁"},
					},
					"required": []string{"name", "trait", "value"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "person_relate",
				"description": "记录两个人之间的社交关系。例如：person_relate(person_a=\"张三\", relation=\"朋友\", person_b=\"李四\")。关系是双向的。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"person_a": map[string]interface{}{"type": "string", "description": "人物A"},
						"relation": map[string]interface{}{"type": "string", "description": "关系类型，如：朋友、家人、同事、邻居、同学"},
						"person_b": map[string]interface{}{"type": "string", "description": "人物B"},
					},
					"required": []string{"person_a", "relation", "person_b"},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "person_network",
				"description": "查询某人的社交网络（多度关系）。显示该人物周围的相关人物及其关系和特质。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name":  map[string]interface{}{"type": "string", "description": "人物名称"},
						"depth": map[string]interface{}{"type": "integer", "description": "关系深度（默认2）", "default": 2},
					},
					"required": []string{"name"},
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
			"description": "启动一个异步子 Agent 执行独立任务。子 Agent 后台运行，不阻塞当前对话。完成后系统会自动通知你，届时请调用 child_result 工具查看输出。",
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
	tools = append(tools, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "child_result",
			"description": "查询异步子 Agent 的执行结果。当收到'子任务已完成'的通知后，调用此工具获取输出。",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task_id": map[string]interface{}{
						"type":        "string",
						"description": "spawn_child 返回的任务 ID，如 child_1",
					},
				},
				"required": []string{"task_id"},
			},
		},
	})

	// LLM 源管理工具
	if a.providerManager != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "llm_list_sources",
				"description": "列出所有可用的 LLM 源（如 deepseek、openai、ollama），每个源有对应的 Lua 适配器和配置。如需切换 LLM 源，请使用 llm_set_source。",
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "llm_set_source",
				"description": "切换当前 LLM 源到指定名称。变更立即生效，后续对话将使用新的 LLM 源。源名称可通过 llm_list_sources 查看。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{
							"type":        "string",
							"description": "LLM 源名称（如 deepseek、openai、ollama）",
						},
					},
					"required": []string{"name"},
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

	// 媒体处理工具：仅当本轮有未处理的媒体数据时注册
	if a.pendingMedia != nil {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "describe_image",
				"description": "描述当前用户上传的图片内容。使用配置的多模态模型或默认 LLM 进行识别。调用此工具后你将获得图片的详细文字描述。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"provider": map[string]interface{}{
							"type":        "string",
							"description": "可选：用于图片描述的 LLM 源名称，不填则使用默认模型",
						},
						"detail": map[string]interface{}{
							"type":        "string",
							"description": "描述详细程度: high / low / auto",
							"default":     "high",
						},
					},
				},
			},
		})
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "transcribe_audio",
				"description": "转写当前用户上传的音频内容为文字。使用配置的多模态模型或默认 LLM 进行语音识别。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"provider": map[string]interface{}{
							"type":        "string",
							"description": "可选：用于音频转写的 LLM 源名称，不填则使用默认模型",
						},
					},
				},
			},
		})
		if a.inputCfg.Image.OCREnabled {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "ocr_image",
					"description": "对当前用户上传的图片执行 OCR 文字识别，提取图片中的文字内容。适用于截图、文档照片、菜单等场景。",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"language": map[string]interface{}{
								"type":        "string",
								"description": "OCR 语言（如 chi_sim+eng），默认自动",
							},
						},
					},
				},
			})
		}
	}

	return tools
}

// ConsolidationTask 心跳检测到的记忆整理任务，通过 IO 发送给 Agent 让 LLM 决策
type ConsolidationTask struct {
	Type   string      `json:"type"`   // "entity_merge", "relation_conflict", "doc_archival"
	Reason string      `json:"reason"` // 人类可读的描述
	Data   interface{} `json:"data"`   // 任务相关数据
}

// enqueueConsolidationTask 将记忆整理任务通过自循环通道注入 Agent（不经过 IO 层）
func (a *Agent) enqueueConsolidationTask(task ConsolidationTask) {
	msg := fmt.Sprintf("【记忆整理任务】\n类型: %s\n说明: %s", task.Type, task.Reason)
	a.injectSelf(msg)
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

	// 5. 图连接质量评估：由 LLM 判断低质量关系并丢弃
	a.evaluateGraphQuality()
}

func (a *Agent) evaluateGraphQuality() {
	if a.memory == nil {
		return
	}

	// 召回近期低 confidence 关系（使用默认 recall 获取最新实体和关系）
	result, err := a.memory.Recall(nil, nil, 1, "")
	if err != nil || result == nil || len(result.Relations) == 0 {
		return
	}

	// 选出低质量候选：generic 关系（如 distiller 自动生成的泛化关系）
	var lowQuality []string
	for _, r := range result.Relations {
		// 自动蒸馏生成的 (用户, 提及, ...) 和 (AI, 回应, ...) 通常是噪音
		if (r.SourceName == "用户" || r.SourceName == "AI") &&
			(r.RelationType == "提及" || r.RelationType == "回应") {
			lowQuality = append(lowQuality, fmt.Sprintf("「%s」-「%s」→「%s」", r.SourceName, r.RelationType, r.TargetName))
			continue
		}
		// 极低 mention 的实体+generic 关系
		if r.Confidence < 0.3 && r.RelationType != "" {
			lowQuality = append(lowQuality, fmt.Sprintf("「%s」-「%s」→「%s」(confidence=%.1f)", r.SourceName, r.RelationType, r.TargetName, r.Confidence))
		}
	}

	if len(lowQuality) == 0 {
		return
	}

	// 分批发送给 LLM 决策，每批最多 10 条
	batchSize := 10
	for i := 0; i < len(lowQuality); i += batchSize {
		end := i + batchSize
		if end > len(lowQuality) {
			end = len(lowQuality)
		}
		batch := lowQuality[i:end]

		a.enqueueConsolidationTask(ConsolidationTask{
			Type: "graph_quality",
			Reason: fmt.Sprintf(
				"图数据库中发现 %d 条低质量关系，请逐条判断是否应该删除（保留 = keep，删除 = discard）：\n%s",
				len(batch),
				strings.Join(batch, "\n"),
			),
			Data: map[string]interface{}{
				"candidates": batch,
				"action":     "evaluate_quality",
			},
		})
	}

	log.Printf("[agent] graph quality: %d low-quality connection batches sent for LLM evaluation", (len(lowQuality)+batchSize-1)/batchSize)
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

// executeSpawnChild 创建子 Agent 异步执行独立任务
// 不阻塞主 Agent，子任务完成后通过 selfInputCh 通知主 Agent 查看结果
func (a *Agent) executeSpawnChild(tc agentAPI.ToolCall) string {
	task, _ := tc.Arguments["task"].(string)
	if task == "" {
		return "请提供 task 参数"
	}

	// 生成唯一任务 ID
	a.childMu.Lock()
	a.childNextID++
	taskID := fmt.Sprintf("child_%d", a.childNextID)
	a.childMu.Unlock()

	// 异步启动子 Agent
	go a.runChildTask(taskID, task)

	return fmt.Sprintf("子任务已启动（ID: %s），完成后会自动通知你，届时请使用 child_result 工具查看输出", taskID)
}

// runChildTask 后台运行子 Agent 任务，完成后将结果存储并通过 selfInputCh 通知主 Agent
func (a *Agent) runChildTask(taskID, task string) {
	if a.provider == nil {
		log.Printf("[child] %s failed: no LLM provider configured", taskID)
		return
	}
	log.Printf("[child] %s started: %s", taskID, truncateStr(task, 80))

	sysPrompt := fmt.Sprintf(`你是 HomeAgent 的子任务助手。
请完成以下任务。完成即可，无需保留记忆或查询历史。
任务: %s`, task)

	msgs := []agentAPI.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: task},
	}

	// 子 Agent 可调用核心以外的全部工具（记忆/知识/文档/社交），但不能调用输出工具
	allTools := a.buildToolDefs()
	childTools := make([]interface{}, 0, len(allTools))
	outputTools := map[string]bool{"output_send": true, "output_set_channel": true, "output_list_channels": true, "spawn_child": true, "plgreload": true}
	for _, t := range allTools {
		toolMap, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := toolMap["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if !outputTools[name] {
			childTools = append(childTools, t)
		}
	}

	var finalResult string
	for turn := 0; turn < 5; turn++ {
		eb := map[string]interface{}{}
		if !a.thinkingEnabled {
			eb["thinking"] = map[string]interface{}{"type": "disabled"}
		}
		req := &agentAPI.CompletionRequest{
			Messages:   msgs,
			MaxTokens:  4096,
			Tools:      childTools,
			ToolChoice: "auto",
			ExtraBody:  eb,
		}

		resp, err := a.provider.Chat(a.ctx, req)
		if err != nil {
			finalResult = fmt.Sprintf("子 Agent 执行失败: %v", err)
			break
		}

		if len(resp.ToolCalls) == 0 {
			finalResult = resp.Content
			break
		}

		for _, ct := range resp.ToolCalls {
			var result string
			switch {
			case ct.Name == "output_send" || ct.Name == "output_set_channel" || ct.Name == "output_list_channels":
				result = fmt.Sprintf("子 Agent 不允许调用输出工具: %s", ct.Name)
			case ct.Name == "spawn_child" || ct.Name == "plgreload":
				result = fmt.Sprintf("子 Agent 不允许调用系统工具: %s", ct.Name)
			default:
				result = a.executeToolCall(ct)
			}
			msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: resp.Content, ToolCalls: []agentAPI.ToolCall{ct}})
			msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: ct.ID, Content: result})
		}
	}

	if finalResult == "" {
		finalResult = "子 Agent 执行超时（超过 5 轮）"
	}

	// 存储结果
	a.childMu.Lock()
	a.childResults[taskID] = finalResult
	a.childMu.Unlock()

	log.Printf("[child] %s done: %s", taskID, truncateStr(finalResult, 100))

	// 通过自循环通道通知主 Agent
	notification := fmt.Sprintf("子任务 %s 已完成，请调用 child_result 工具查看输出", taskID)
	select {
	case a.selfInputCh <- notification:
	default:
		log.Printf("[child] self input channel full, dropping notification for %s", taskID)
	}
}

// executeChildResultTool 查询子 Agent 执行结果
func (a *Agent) executeChildResultTool(tc agentAPI.ToolCall) string {
	taskID, _ := tc.Arguments["task_id"].(string)
	if taskID == "" {
		return "请提供 task_id 参数"
	}

	a.childMu.Lock()
	result, ok := a.childResults[taskID]
	if !ok {
		a.childMu.Unlock()

		// 可能还在执行中
		a.childMu.Lock()
		_, exists := a.childResults[taskID]
		a.childMu.Unlock()
		if !exists {
			return fmt.Sprintf("子任务 %s 不存在或已过期", taskID)
		}
	}
	delete(a.childResults, taskID)
	a.childMu.Unlock()

	return fmt.Sprintf("【子任务 %s 结果】\n%s", taskID, result)
}

func (a *Agent) executeLLMTool(tc agentAPI.ToolCall) string {
	if a.providerManager == nil {
		return "LLM 源管理器不可用"
	}
	switch tc.Name {
	case "llm_list_sources":
		sources := a.providerManager.List()
		if len(sources) == 0 {
			return "没有可用的 LLM 源"
		}
		parts := []string{"可用 LLM 源:"}
		for _, name := range sources {
			mark := " "
			if p := a.providerManager.Get(""); p != nil && p.Name() == name {
				mark = "→"
			}
			parts = append(parts, fmt.Sprintf("  %s %s", mark, name))
		}
		return strings.Join(parts, "\n")

	case "llm_set_source":
		name, _ := tc.Arguments["name"].(string)
		if name == "" {
			return "请提供源名称"
		}
		if err := a.providerManager.SetDefault(name); err != nil {
			return fmt.Sprintf("切换失败: %v", err)
		}
		a.mu.Lock()
		a.provider = a.providerManager.Get(name)
		a.mu.Unlock()
		return fmt.Sprintf("已切换到 LLM 源: %s", name)

	default:
		return fmt.Sprintf("未知的 LLM 工具: %s", tc.Name)
	}
}

// executeDescribeImage 调用多模态模型描述当前图片。
func (a *Agent) executeDescribeImage(tc agentAPI.ToolCall) string {
	if a.pendingMedia == nil {
		return "没有待处理的图片数据"
	}
	data, _ := a.pendingMedia["data"].(string)
	mime, _ := a.pendingMedia["mime"].(string)
	url, _ := a.pendingMedia["url"].(string)
	if data == "" && url == "" {
		return "图片数据为空"
	}

	providerName, _ := tc.Arguments["provider"].(string)
	detail, _ := tc.Arguments["detail"].(string)
	if detail == "" {
		detail = "high"
	}

	p := a.providerManager.Get(providerName)
	if p == nil {
		p = a.provider
	}

	prompt := a.inputCfg.Image.DescribePrompt
	if prompt == "" {
		prompt = "请详细描述这张图片的内容，包括其中的文字、物体、人物、场景等信息。"
	}

	imgURL := url
	if data != "" {
		if mime == "" {
			mime = "image/png"
		}
		imgURL = "data:" + mime + ";base64," + data
	}

	msg := agentAPI.Message{
		Role: "user",
		Blocks: []agentAPI.ContentBlock{
			{Type: "text", Text: prompt},
			{Type: "image_url", ImageURL: &agentAPI.ImageURL{URL: imgURL, Detail: detail}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := p.Chat(ctx, &agentAPI.CompletionRequest{
		Messages:  []agentAPI.Message{msg},
		MaxTokens: 2048,
	})
	if err != nil {
		return fmt.Sprintf("图片描述失败: %v", err)
	}
	return fmt.Sprintf("[图片描述] %s", resp.Content)
}

// executeTranscribeAudio 调用多模态模型转写/描述当前音频。
func (a *Agent) executeTranscribeAudio(tc agentAPI.ToolCall) string {
	if a.pendingMedia == nil {
		return "没有待处理的音频数据"
	}
	data, _ := a.pendingMedia["data"].(string)
	mime, _ := a.pendingMedia["mime"].(string)
	url, _ := a.pendingMedia["url"].(string)
	if data == "" && url == "" {
		return "音频数据为空"
	}

	providerName, _ := tc.Arguments["provider"].(string)
	p := a.providerManager.Get(providerName)
	if p == nil {
		p = a.provider
	}

	prompt := a.inputCfg.Audio.DescribePrompt
	if prompt == "" {
		prompt = "请转写这段音频的内容。"
	}

	audURL := url
	if data != "" {
		if mime == "" {
			mime = "audio/wav"
		}
		audURL = "data:" + mime + ";base64," + data
	}

	msg := agentAPI.Message{
		Role: "user",
		Blocks: []agentAPI.ContentBlock{
			{Type: "text", Text: prompt},
			{Type: "audio_url", AudioURL: &agentAPI.AudioURL{URL: audURL}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := p.Chat(ctx, &agentAPI.CompletionRequest{
		Messages:  []agentAPI.Message{msg},
		MaxTokens: 2048,
	})
	if err != nil {
		return fmt.Sprintf("音频转写失败: %v", err)
	}
	return fmt.Sprintf("[音频转写] %s", resp.Content)
}

// executeOCRImage 对图片执行 OCR 文字识别（通过多模态模型实现）。
func (a *Agent) executeOCRImage(tc agentAPI.ToolCall) string {
	if a.pendingMedia == nil {
		return "没有待处理的图片数据"
	}
	data, _ := a.pendingMedia["data"].(string)
	mime, _ := a.pendingMedia["mime"].(string)
	url, _ := a.pendingMedia["url"].(string)
	if data == "" && url == "" {
		return "图片数据为空"
	}

	p := a.provider

	imgURL := url
	if data != "" {
		if mime == "" {
			mime = "image/png"
		}
		imgURL = "data:" + mime + ";base64," + data
	}

	msg := agentAPI.Message{
		Role: "user",
		Blocks: []agentAPI.ContentBlock{
			{Type: "text", Text: "请识别这张图片中的所有文字内容，按原文输出。仅输出文字本身，不要添加额外描述。"},
			{Type: "image_url", ImageURL: &agentAPI.ImageURL{URL: imgURL, Detail: "high"}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := p.Chat(ctx, &agentAPI.CompletionRequest{
		Messages:  []agentAPI.Message{msg},
		MaxTokens: 4096,
	})
	if err != nil {
		return fmt.Sprintf("OCR 识别失败: %v", err)
	}
	return fmt.Sprintf("[OCR 结果] %s", resp.Content)
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
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	var truncated int
	for i := range s {
		if truncated >= max {
			return s[:i] + "..."
		}
		truncated++
	}
	return s
}

// drainInterrupt 非阻塞读取 interceptCh 中的一条打断消息。
// 若有多条，只取最先到达的一条（丢弃后续）。
func (a *Agent) drainInterrupt() string {
	select {
	case text := <-a.interceptCh:
		return text
	default:
		return ""
	}
}
