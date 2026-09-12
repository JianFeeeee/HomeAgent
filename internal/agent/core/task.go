package core

// 任务状态机（M1：行为等价的纯重构）。
//
// 背景与设计见 docs/zh/input-scheduler-design.md。
//
// M1 只做一件事：把原先「一个 425 行的 process() 大函数」拆成
// **显式 step 游标 + TaskFrame**。目的不是加能力，而是让「现场」变成数据——
// 之后 M3 才能把帧存进 suspendPool 并在安全点恢复。
//
// 行为等价的判据：既有全部 agent 测试通过，且 R3/X3（见设计文档 §11）通过。
//
// 本文件**不引入**优先级、抢占、队列与并发；那些在 M2 起逐层加上。
//
// Step 与安全点（设计文档 §4.2）：
//   step 与 step 之间是安全点；StepToolExec（工具执行）与 StepPrepare 中的
//   ONNX/落盘片段是**临界区**，执行中不可抢占。
//
// 与设计文档的差异：文档里的 StepBeforeOutput / StepAfterOutput / StepCommit /
// StepFinish 属于 emitResponse 与 processInput 层，M1 不动它们（M6 再迁入帧）。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// Step 是任务状态机的游标。
type Step int

const (
	// StepPrepare 构建消息与工具表、应用中断标记、跑 pre_action 阶段。
	StepPrepare Step = iota
	// StepLLM 轮次顶部（中断/占位）+ LLM 调用（含 provider 回退与重试）+ post_action。
	StepLLM
	// StepToolBegin 取本批下一个工具，跑 before_toolcall；被拒/插件不健康则跳过。
	StepToolBegin
	// StepToolExec 执行工具。**临界区**：副作用不可回滚，执行中不是安全点。
	StepToolExec
	// StepToolAfter after_toolcall 阶段、上下文裁剪、消息与事件组装、批后中断检查。
	StepToolAfter
	// StepTurnEnd 收尾本批并进入下一轮。
	StepTurnEnd
)

// stepOutcome 是一次 step 执行的结果。
type stepOutcome int

const (
	// outcomeContinue 继续执行下一个 step（游标可能停在原地以表达"重跑本 step"）。
	outcomeContinue stepOutcome = iota
	// outcomeDone 任务成功结束，响应在 frame.Response。
	outcomeDone
	// outcomeFailed 任务失败结束，错误在 frame.Err。
	outcomeFailed
)

// TaskFrame 承载一个任务在安全点之间必须存活的所有状态。
//
// 不变量（设计文档 §8.1 I3）：帧是**纯数据**；不得持有任何锁或资源跨越安全点。
type TaskFrame struct {
	Input    string
	StageCtx *sdk.StageContext

	// 跨轮次状态
	Msgs               []agentAPI.Message
	Tools              []interface{}
	ToolsUsed          []string
	ToolResults        []ToolResultItem
	Turn               int
	LastBatchReplyOnly bool

	// 当前工具批
	PendingTools  []agentAPI.ToolCall
	ToolIdx       int
	ReplyOnly     bool
	ContentOnce   bool
	CurTool       agentAPI.ToolCall
	CurToolPlugin string
	CurResult     string
	Resp          *agentAPI.CompletionResponse

	// 游标与终态
	Step     Step
	Response string
	Err      error
}

func (a *Agent) newTaskFrame(input string, stageCtx *sdk.StageContext) *TaskFrame {
	return &TaskFrame{Input: input, StageCtx: stageCtx, Step: StepPrepare}
}

// process 驱动状态机直到任务结束，返回与原实现完全相同的四元组。
//
// 保留该签名是为了让 M1 成为纯内部重构：所有调用方（processInput /
// processConsolidation / 测试）无需改动。
func (a *Agent) process(input string, stageCtx *sdk.StageContext) (response string, toolsUsed []string, toolResults []ToolResultItem, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.provider == nil {
		return "", nil, nil, fmt.Errorf("agent: no LLM provider configured")
	}

	f := a.newTaskFrame(input, stageCtx)
	// 步数上限只是防"转移缺失导致死循环"的护栏；正常任务远达不到。
	const maxSteps = 1 << 20
	for i := 0; i < maxSteps; i++ {
		switch a.step(f) {
		case outcomeDone:
			return f.Response, f.ToolsUsed, f.ToolResults, nil
		case outcomeFailed:
			return "", f.ToolsUsed, f.ToolResults, f.Err
		}
	}
	return "", f.ToolsUsed, f.ToolResults,
		fmt.Errorf("agent: task step budget exhausted（状态机未收敛，疑似转移缺失）")
}

// step 执行恰好一个 step。
func (a *Agent) step(f *TaskFrame) stepOutcome {
	switch f.Step {
	case StepPrepare:
		return a.stepPrepare(f)
	case StepLLM:
		return a.stepLLM(f)
	case StepToolBegin:
		return a.stepToolBegin(f)
	case StepToolExec:
		return a.stepToolExec(f)
	case StepToolAfter:
		return a.stepToolAfter(f)
	case StepTurnEnd:
		return a.stepTurnEnd(f)
	default:
		f.Err = fmt.Errorf("agent: unknown task step %d", f.Step)
		return outcomeFailed
	}
}

// stepPrepare 构建本轮任务的初始帧。
func (a *Agent) stepPrepare(f *TaskFrame) stepOutcome {
	budget := ComputeTokenBudget(a.provider, a.systemPrompt)

	memContext := a.buildMemoryContext(f.Input, budget.MemoryTokens)
	sysPrompt := a.buildSystemPrompt(memContext, f.Input)
	f.Tools = a.buildToolDefs()

	f.Msgs = a.buildMessages(sysPrompt, f.Input, budget.ContextTokens)
	// 工具提醒（interrupt）：以 system 角色注入，不让模型误认为用户发言
	if a.interruptInput {
		last := f.Msgs[len(f.Msgs)-1]
		last.Role = "system"
		last.Content = "[中断消息] " + last.Content
		f.Msgs[len(f.Msgs)-1] = last
		a.interruptInput = false
	}
	if blocks, ok := f.StageCtx.Extra["media_blocks"].([]agentAPI.ContentBlock); ok && len(blocks) > 0 {
		if len(f.Msgs) > 0 {
			f.Msgs[len(f.Msgs)-1].Blocks = blocks
		}
	}

	log.Printf("[agent] tool call loop start, max_ctx=%d target=%d fixed=%d mem=%d ctx=%d %d tools, %d events, personality=%t, docs=%d",
		budget.MaxContext, budget.TargetUsage, budget.FixedTokens, budget.MemoryTokens, budget.ContextTokens,
		len(f.Tools), a.context.Len(),
		a.personality != nil && a.personality.Content != "",
		a.docStoreSize())

	if a.runStage(sdk.StagePreAction, f.StageCtx) {
		f.Response = *f.StageCtx.Response
		return outcomeDone
	}
	if len(f.StageCtx.ContextMsgs) > 0 {
		for _, m := range f.StageCtx.ContextMsgs {
			role, _ := m["role"].(string)
			content, _ := m["content"].(string)
			if role != "" {
				f.Msgs = append(f.Msgs, agentAPI.Message{Role: role, Content: content})
			}
		}
	}

	f.Step = StepLLM
	return outcomeContinue
}

// stepLLM 是轮次顶部与 LLM 调用。
//
// 取消（context.Canceled 且 agent 未退出）时**留在本 step 并 Turn++**——等价于
// 原实现的 `continue`：重新排空中断、补占位、重新请求。抢占挂起将在 M3 从这里接管。
func (a *Agent) stepLLM(f *TaskFrame) stepOutcome {
	for _, interrupt := range a.drainInterrupts() {
		f.Msgs = append(f.Msgs, agentAPI.Message{
			Role:    "system",
			Content: "[中断消息] " + interrupt,
		})
	}

	// zen 兼容网关要求请求的最后一条消息必须是 user(thinking 续写模式校验),
	// 工具轮产出的 tool/assistant 消息作结尾会被 400 拒绝,故补一条 user 占位。
	f.Msgs = dropContinuationPlaceholders(f.Msgs)
	if last := f.Msgs[len(f.Msgs)-1]; last.Role == "assistant" || last.Role == "tool" {
		f.Msgs = append(f.Msgs, agentAPI.Message{
			Role:    "user",
			Content: continuationFor(f.LastBatchReplyOnly),
		})
	}

	req := &agentAPI.CompletionRequest{
		Messages:        f.Msgs,
		MaxTokens:       4096,
		Tools:           f.Tools,
		ToolChoice:      "auto",
		DisableThinking: !a.thinkingEnabled,
	}

	providers := a.resolveProviders(req)
	resp, llmErr := a.callLLMWithFallback(req, providers)

	if llmErr != nil {
		if errors.Is(llmErr, context.Canceled) && a.ctx.Err() == nil {
			if a.currentOutputChannel == "_consolidation_" {
				f.Err = fmt.Errorf("interrupted by user input")
				return outcomeFailed
			}
			f.Turn++
			return outcomeContinue // 重跑 StepLLM
		}
		f.Err = fmt.Errorf("all %d providers failed, last error: %w", len(providers), llmErr)
		return outcomeFailed
	}

	f.StageCtx.LLMText = resp.Content
	f.StageCtx.ReasoningContent = resp.ReasoningContent
	f.StageCtx.TokenUsage = map[string]int{
		"prompt_tokens":     resp.TokenUsage.Prompt,
		"completion_tokens": resp.TokenUsage.Completion,
		"total_tokens":      resp.TokenUsage.Total,
	}
	f.StageCtx.ToolCalls = convertToolCalls(resp.ToolCalls)
	for i := range f.StageCtx.ToolCalls {
		if f.StageCtx.ToolCalls[i].Plugin == "" {
			f.StageCtx.ToolCalls[i].Plugin = a.resolveToolPlugin(f.StageCtx.ToolCalls[i].Name)
		}
	}
	if a.runStage(sdk.StagePostAction, f.StageCtx) {
		f.Response = *f.StageCtx.Response
		return outcomeDone
	}
	resp.Content = f.StageCtx.LLMText
	resp.ToolCalls = convertBackToolCalls(f.StageCtx.ToolCalls)

	chainPayload := map[string]interface{}{
		"content":    resp.Content,
		"reasoning":  resp.ReasoningContent,
		"tool_calls": resp.ToolCalls,
		"phase":      "intermediate",
		"turn":       f.Turn,
	}
	if resp.TokenUsage.Total > 0 {
		chainPayload["usage"] = map[string]int{
			"prompt":     resp.TokenUsage.Prompt,
			"completion": resp.TokenUsage.Completion,
			"total":      resp.TokenUsage.Total,
		}
	}
	a.publishEvent(events.EventAgentLLMChain, chainPayload)

	if resp.ReasoningContent != "" {
		a.publishEvent(events.EventReasoning, map[string]interface{}{
			"content": resp.ReasoningContent,
			"channel": a.currentOutputChannel,
		})
	}

	if len(resp.ToolCalls) == 0 {
		f.Response = resp.Content
		return outcomeDone
	}

	// 本批是否全部是输出通道发送（=模型刚交付了给用户的回复）。
	// 必须在执行前判定：执行过程中的中断/拒绝分支会 continue/break，放在循环里统计会漏。
	f.Resp = resp
	f.ReplyOnly = true
	for _, tc := range resp.ToolCalls {
		if !isOutputDeliveryTool(tc.Name) {
			f.ReplyOnly = false
			break
		}
	}
	f.ContentOnce = true
	f.PendingTools = resp.ToolCalls
	f.ToolIdx = 0
	f.Step = StepToolBegin
	return outcomeContinue
}

// stepToolBegin 取本批下一个工具；批已耗尽或发生中断则进入收尾。
func (a *Agent) stepToolBegin(f *TaskFrame) stepOutcome {
	if f.ToolIdx >= len(f.PendingTools) {
		f.Step = StepTurnEnd
		return outcomeContinue
	}
	tc := f.PendingTools[f.ToolIdx]

	if len(a.interceptCh) > 0 {
		for _, interrupt := range a.drainInterrupts() {
			f.Msgs = append(f.Msgs, agentAPI.Message{Role: "system", Content: "[中断消息] " + interrupt})
		}
		a.publishEvent(events.EventToolCall, map[string]interface{}{
			"tool":    tc.Name,
			"plugin":  a.resolveToolPlugin(tc.Name),
			"args":    tc.Arguments,
			"status":  "interrupted",
			"reason":  "user interrupt before execution",
			"channel": a.currentOutputChannel,
		})
		f.Step = StepTurnEnd
		return outcomeContinue
	}

	f.ToolsUsed = append(f.ToolsUsed, tc.Name)
	pluginName := a.resolveToolPlugin(tc.Name)
	log.Printf("[agent] executing tool: %s (plugin=%s, id=%s)", tc.Name, pluginName, tc.ID)
	if tc.RawArguments != "" {
		log.Printf("[agent] tool %s raw_arguments: %s", tc.Name, truncateStr(tc.RawArguments, 300))
	}

	sdkTC := sdk.ToolCall{ID: tc.ID, Name: tc.Name, Plugin: pluginName, Arguments: tc.Arguments}
	f.StageCtx.ToolCalls = []sdk.ToolCall{sdkTC}
	f.StageCtx.ToolResults = nil
	if a.runStage(sdk.StageBeforeToolcall, f.StageCtx) {
		result := fmt.Sprintf("工具 %s 已被插件拒绝", tc.Name)
		f.Msgs = append(f.Msgs, agentAPI.Message{Role: "assistant", ToolCalls: []agentAPI.ToolCall{tc}})
		f.Msgs = append(f.Msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
		a.publishEvent(events.EventToolCall, map[string]interface{}{
			"tool":    tc.Name,
			"plugin":  pluginName,
			"args":    tc.Arguments,
			"result":  result,
			"status":  "denied",
			"channel": a.currentOutputChannel,
		})
		f.ToolIdx++
		return outcomeContinue
	}
	tc.Arguments = f.StageCtx.ToolCalls[0].Arguments

	if pluginName != "" && !a.pluginHealth.isHealthy(pluginName) {
		result := fmt.Sprintf("插件 %s 处于崩溃状态，已跳过执行，等待自动恢复重载", pluginName)
		log.Printf("[agent] skip tool %s: plugin %s unhealthy", tc.Name, pluginName)
		f.Msgs = append(f.Msgs, agentAPI.Message{Role: "assistant", ToolCalls: []agentAPI.ToolCall{tc}})
		f.Msgs = append(f.Msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
		f.ToolIdx++
		return outcomeContinue
	}

	f.CurTool = tc
	f.CurToolPlugin = pluginName
	f.Step = StepToolExec
	return outcomeContinue
}

// stepToolExec 执行工具。**临界区**：见设计文档 §4.3。
func (a *Agent) stepToolExec(f *TaskFrame) stepOutcome {
	result := a.executeToolCall(f.CurTool)
	f.CurResult = result
	f.ToolResults = append(f.ToolResults, ToolResultItem{Name: f.CurTool.Name, Output: result})
	log.Printf("[agent] tool %s result: %s", f.CurTool.Name, truncateStr(result, 100))

	f.StageCtx.ToolResults = []sdk.ToolResult{{
		CallID: f.CurTool.ID, Name: f.CurTool.Name, Plugin: f.CurToolPlugin,
		Success: true, Result: result,
	}}
	f.Step = StepToolAfter
	return outcomeContinue
}

// stepToolAfter 是工具执行后的全部后处理（阶段、裁剪、消息与事件）。
func (a *Agent) stepToolAfter(f *TaskFrame) stepOutcome {
	tc := f.CurTool
	pluginName := f.CurToolPlugin
	result := f.CurResult

	a.runStage(sdk.StageAfterToolcall, f.StageCtx)
	if len(f.StageCtx.ToolResults) > 0 {
		if r, ok := f.StageCtx.ToolResults[0].Result.(string); ok {
			result = r
		}
	}
	// ContextPolicy: prune 工具调用后执行上下文裁剪（§13.8）
	if def := a.stageHost.ToolDef(tc.Name); def != nil && def.ContextPolicy == "prune" {
		if a.context != nil {
			topK := a.maxContextSize - 1
			if topK < 1 {
				topK = 1
			}
			// 查询向量取**清洗后**的有效内容，否则噪声（ANSI/base64/JSON 包装）
			// 会把相关性打分带偏，裁掉本该保留的事件。
			a.context.Prune(a.toolOutputForQuery(tc.Name, result), topK, a.docStore)
		}
	}

	msgContent := ""
	if f.ContentOnce {
		msgContent = f.Resp.Content
		f.ContentOnce = false
	}
	f.Msgs = append(f.Msgs, agentAPI.Message{
		Role: "assistant", Content: msgContent,
		ReasoningContent: f.Resp.ReasoningContent,
		ToolCalls:        []agentAPI.ToolCall{tc},
	})

	// 多模态工具结果：插件通过 SDK.SetToolBlocks 注入 image_url/audio_url block。
	//
	// 媒体不挂在 tool message 上，而是另起一条紧随其后的 user message——
	// 这也是插件文案一直在说的「注入后续对话」。
	// 为何不能挂 tool message：同一张图、同一模型、三轮实测——
	//   图在 user message      → 3/3 读到
	//   图在 tool message      → 0/3（模型答「没能读到这张图」）
	//   tool 纯文本 + 后接 user → 3/3 读到
	// tool message 那轮 prompt_tokens 反而更高（7967 vs 7089），base64 确实
	// 进了上游，但 role=tool 上的多模态 content 数组不被当作可视内容。
	//
	// 主模型不支持该模态时更不能直接塞：网关会把 image_url 静默剥离后仍
	// 返回 200，模型回答「我没有看到图片」而内核以为注入成功。改走回退链。
	toolMsg := agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result}
	var mediaMsg *agentAPI.Message
	if rawBlocks := a.io.ConsumeToolBlocks(); len(rawBlocks) > 0 {
		var blocks []agentAPI.ContentBlock
		for _, b := range rawBlocks {
			if cb, ok := b.(pubsdk.ContentBlock); ok {
				// 跨包类型拷贝（pubsdk.ContentBlock → agentAPI.ContentBlock）
				block := agentAPI.ContentBlock{Type: cb.Type, Text: cb.Text}
				if cb.ImageURL != nil {
					block.ImageURL = &agentAPI.ImageURL{URL: cb.ImageURL.URL, Detail: cb.ImageURL.Detail}
				}
				if cb.AudioURL != nil {
					block.AudioURL = &agentAPI.AudioURL{URL: cb.AudioURL.URL}
				}
				blocks = append(blocks, block)
			}
		}
		if len(blocks) > 0 {
			// 先落进 CAS：无论下面走直视还是回退转写，媒体本体都该进记忆。
			a.stageMediaDigests(a.captureBlockMedia(blocks, tc.Name)...)

			if native, fallbackText := a.prepareToolBlocks(blocks); len(native) > 0 {
				// 能直视：另起一条 user message 承载媒体，并补一句来源说明。
				mediaBlocks := append([]agentAPI.ContentBlock{{
					Type: "text",
					Text: fmt.Sprintf("[以下是 %s 注入的媒体内容]", tc.Name),
				}}, native...)
				mediaMsg = &agentAPI.Message{Role: "user", Blocks: mediaBlocks}
			} else if fallbackText != "" {
				toolMsg.Content = result + "\n\n" + fallbackText
				result = toolMsg.Content
				if len(f.ToolResults) > 0 {
					f.ToolResults[len(f.ToolResults)-1].Output = result
				}
			}
		}
	}
	f.Msgs = append(f.Msgs, toolMsg)
	if mediaMsg != nil {
		// 必须紧跟在 toolMsg 之后：中间插入其他消息会让 tool_call_id 配对断开。
		f.Msgs = append(f.Msgs, *mediaMsg)
	}

	a.publishEvent(events.EventToolCall, map[string]interface{}{
		"tool":    tc.Name,
		"plugin":  pluginName,
		"args":    tc.Arguments,
		"result":  result,
		"status":  "ok",
		"channel": a.currentOutputChannel,
	})

	f.ToolIdx++
	if len(a.interceptCh) > 0 {
		for _, interrupt := range a.drainInterrupts() {
			f.Msgs = append(f.Msgs, agentAPI.Message{Role: "system", Content: "[中断消息] " + interrupt})
		}
		f.Step = StepTurnEnd
		return outcomeContinue
	}
	f.Step = StepToolBegin
	return outcomeContinue
}

// stepTurnEnd 收尾本批并进入下一轮。
func (a *Agent) stepTurnEnd(f *TaskFrame) stepOutcome {
	// 供下一轮顶部选择补位文案。
	f.LastBatchReplyOnly = f.ReplyOnly
	f.Turn++
	f.Step = StepLLM
	return outcomeContinue
}

// resolveProviders 按请求模型解析候选 provider（保持原语义）。
func (a *Agent) resolveProviders(req *agentAPI.CompletionRequest) []agentAPI.Provider {
	var providers []agentAPI.Provider
	if a.providerManager != nil {
		var allProviders []agentAPI.Provider
		if req.Model != "" && !strings.EqualFold(req.Model, "AUTO") {
			allProviders = a.providerManager.ResolveForModel(req.Model)
		} else {
			allProviders = a.providerManager.OrderedProviders()
		}
		providers = make([]agentAPI.Provider, 0, len(allProviders))
		for _, p := range allProviders {
			if a.providerManager.IsAvailable(p.Name()) {
				providers = append(providers, p)
			}
		}
	}
	if len(providers) == 0 {
		providers = []agentAPI.Provider{a.provider}
	}
	return providers
}

// callLLMWithFallback 在候选 provider 间回退，并把同源瞬时错误重试一次。
// 逐行等价于原 process() 内的双层循环。
func (a *Agent) callLLMWithFallback(req *agentAPI.CompletionRequest, providers []agentAPI.Provider) (*agentAPI.CompletionResponse, error) {
	var resp *agentAPI.CompletionResponse
	var llmErr error

	for pi, fbProvider := range providers {
		if pi > 0 {
			log.Printf("[agent] LLM fallback: trying provider %q (fallback #%d/%d)",
				fbProvider.Name(), pi, len(providers)-1)
		}

		// 同源瞬时错误重试：网关瞬断（502/503/504/429/网络抖动）通常秒级恢复，
		// 直接跳下一个 provider（或直接报错）会丢掉本可成功的请求。
		// 凭证错误（401/403）与用户中断不重试。
		const maxAttempts = 2
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if attempt > 1 {
				log.Printf("[agent] provider %q transient failure, retry %d/%d in 2s: %v",
					fbProvider.Name(), attempt, maxAttempts, llmErr)
				select {
				case <-time.After(2 * time.Second):
				case <-a.ctx.Done():
					llmErr = a.ctx.Err()
				}
				if llmErr == nil || errors.Is(llmErr, context.Canceled) || errors.Is(llmErr, context.DeadlineExceeded) {
					break
				}
			}

			fCtx, fCancel := context.WithCancel(a.ctx)
			a.llmMu.Lock()
			a.cancelLLM = fCancel
			a.llmMu.Unlock()

			resp, llmErr = chatStreamWithFallback(fCtx, fbProvider, req, a)

			a.llmMu.Lock()
			a.cancelLLM = nil
			a.llmMu.Unlock()
			fCancel()

			if llmErr == nil {
				a.providerManager.ResetAvailability(fbProvider.Name())
				if fbProvider != a.provider {
					a.provider = fbProvider
					log.Printf("[agent] switched active provider to %q after fallback", fbProvider.Name())
				}
				break
			}

			// 用户中断：立即终止，不重试也不换 provider
			if errors.Is(llmErr, context.Canceled) {
				break
			}
			// 凭证错误：重试无意义，跳出重试循环进入 provider 标记/切换
			var pe *agentAPI.ProviderError
			if errors.As(llmErr, &pe) && (pe.StatusCode == 401 || pe.StatusCode == 403) {
				break
			}
			// 其余错误（含 5xx/429/网络）：还有重试机会则继续，否则跳出
		}

		if llmErr == nil {
			break
		}
		if errors.Is(llmErr, context.Canceled) {
			break
		}
		var pe *agentAPI.ProviderError
		if errors.As(llmErr, &pe) && (pe.StatusCode == 401 || pe.StatusCode == 403) {
			a.providerManager.ReportStatus(fbProvider.Name(), pe.StatusCode)
			log.Printf("[agent] provider %q marked unavailable (HTTP %d)", fbProvider.Name(), pe.StatusCode)
		} else {
			a.providerManager.MarkUnavailable(fbProvider.Name())
		}
		log.Printf("[agent] provider %q failed: %v", fbProvider.Name(), llmErr)
	}

	return resp, llmErr
}
