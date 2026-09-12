package core

import (
	"context"
	"encoding/json"
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

// continuationPlaceholder 是工具轮之后补的 user 占位内容。
//
// zen 兼容网关要求请求最后一条必须是 user（thinking 续写模式校验），工具轮
// 产出 assistant/tool 结尾会被 400 拒绝；首轮 system 结尾不补，否则会覆盖
// 真实用户输入。
//
// 用独立常量 + 精确等值判定，是因为这条消息是**核心自己插入的**、不是用户输入，
// 所以可以安全地按内容识别并在补位前移除上一条，保证至多一条。
const continuationPlaceholder = "请根据以上工具结果继续。"

// replyDeliveredPlaceholder 是「本批工具调用全部是输出通道发送」之后补的占位。
//
// 为何不能继续用通用的「请继续」：异步通道（qq/wechat）的回复**只能**经
// output_send__* 交付（纯文本不送达，见 buildSystemPrompt 的输出规则）。于是
// 模型「已经回复完了」的表达形式就是一个工具调用，而紧随其后的
// 「请根据以上工具结果继续。」会被读成「还要再做一步」——能做的「一步」恰好
// 还是再发一条消息。两者叠加成自我强化的发送循环：生产实测单轮 34 次
// output_send__qq、持续 514 秒，直到 QQ 插件自己的循环保险拒绝发送才停下。
//
// 所以这里换成一条明确的终止许可：已回复完就直接返回纯文本收尾。
const replyDeliveredPlaceholder = "若你的回复已完成，直接返回纯文本即可结束本轮，无需再调用任何工具。"

// continuationFor 选择工具轮之后补位的 user 占位文案。
// replyOnly 表示上一批工具调用全部是输出通道发送（即模型刚交付了回复）。
func continuationFor(replyOnly bool) string {
	if replyOnly {
		return replyDeliveredPlaceholder
	}
	return continuationPlaceholder
}

// isOutputDeliveryTool 判断工具是否是「向输出通道交付内容」。
// output_send__{channel}_help 只是查询用法，不算交付。
func isOutputDeliveryTool(name string) bool {
	return strings.HasPrefix(name, "output_send__") && !strings.HasSuffix(name, "_help")
}

// isContinuationPlaceholder 判断一条 user 消息是否是本机制插入的占位。
// 只按两个常量精确匹配，不碰任何真实用户消息。
func isContinuationPlaceholder(m agentAPI.Message) bool {
	return m.Role == "user" &&
		(m.Content == continuationPlaceholder || m.Content == replyDeliveredPlaceholder)
}

// toolOutputForQuery 返回用于相关性计算的工具输出**有效内容**。
//
// 为什么要过 Cleaner 而不是直接用原始 result：ContextPolicy=prune 的入参是
// **相关性查询向量**——它决定保留/归档哪些上下文事件。原始工具输出里混着
// ANSI 转义、base64、JSON 包装等噪声，直接拿去向量化会让打分失真。
// 而 ToolDef.Cleaner 的契约本就写着“仅在向量化/jieba/蒸馏时调用”，裁剪正是
// 在向量化，所以这里必须过它（此前只在构建事件向量时用了，裁剪查询漏了）。
//
// Cleaner 未注册或 RPC 失败时回退原文（清洗是计算层优化，不能因此丢内容）；
// 返回空串时也回退——空串会让查询向量退化成零向量，裁剪就失去判据。
func (a *Agent) toolOutputForQuery(toolName, raw string) string {
	if a.stageHost == nil {
		return raw
	}
	cleaner := a.stageHost.ToolDefCleaner(toolName)
	if cleaner == nil {
		return raw
	}
	if cleaned := cleaner(raw); cleaned != "" {
		return cleaned
	}
	return raw
}

// dropContinuationPlaceholders 移除此前由本机制插入的 user 占位。
//
// 为什么必须移除而不仅仅是“不再追加”：`msgs` 在循环外创建、循环内只增不减，
// 占位是核心自己插的、不是用户说的话。不移除的话，prompt 里就会线性叠上
// N 条一模一样的“继续”，把前缀上下文（含记忆注入）往后挤。
func dropContinuationPlaceholders(msgs []agentAPI.Message) []agentAPI.Message {
	out := msgs[:0]
	for _, m := range msgs {
		if isContinuationPlaceholder(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func (a *Agent) process(input string, stageCtx *sdk.StageContext) (response string, toolsUsed []string, toolResults []ToolResultItem, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.provider == nil {
		return "", nil, nil, fmt.Errorf("agent: no LLM provider configured")
	}

	budget := ComputeTokenBudget(a.provider, a.systemPrompt)

	memContext := a.buildMemoryContext(input, budget.MemoryTokens)
	sysPrompt := a.buildSystemPrompt(memContext, input)
	tools := a.buildToolDefs()

	msgs := a.buildMessages(sysPrompt, input, budget.ContextTokens)
	// 工具提醒（interrupt）：以 system 角色注入，不让模型误认为用户发言
	if a.interruptInput {
		last := msgs[len(msgs)-1]
		last.Role = "system"
		last.Content = "[中断消息] " + last.Content
		msgs[len(msgs)-1] = last
		a.interruptInput = false
	}
	if blocks, ok := stageCtx.Extra["media_blocks"].([]agentAPI.ContentBlock); ok && len(blocks) > 0 {
		if len(msgs) > 0 {
			msgs[len(msgs)-1].Blocks = blocks
		}
	}

	log.Printf("[agent] tool call loop start, max_ctx=%d target=%d fixed=%d mem=%d ctx=%d %d tools, %d events, personality=%t, docs=%d",
		budget.MaxContext, budget.TargetUsage, budget.FixedTokens, budget.MemoryTokens, budget.ContextTokens,
		len(tools), a.context.Len(),
		a.personality != nil && a.personality.Content != "",
		a.docStoreSize())

	if a.runStage(sdk.StagePreAction, stageCtx) {
		return *stageCtx.Response, toolsUsed, toolResults, nil
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

	// lastBatchReplyOnly 记录上一批工具调用是否全部是输出通道发送。
	lastBatchReplyOnly := false

	for turn := 0; ; turn++ {
		for _, interrupt := range a.drainInterrupts() {
			msgs = append(msgs, agentAPI.Message{
				Role:    "system",
				Content: "[中断消息] " + interrupt,
			})
		}

		// zen 兼容网关要求请求的最后一条消息必须是 user(thinking 续写模式校验),
		// 工具轮产出的 tool/assistant 消息作结尾会被 400 拒绝,故补一条 user 占位。
		// 注意:仅当尾部确为工具轮产物(assistant/tool)时才补位;首轮 system 上下文结尾不补,
		// 否则会错误覆盖实际用户输入(如 injectSourceContext 追加的 system 说明)。
		//
		// 补位前先移除前面轮次插入的同类占位，保证占位**不随轮次线性累积**——
		// 占位是核心插的传输层附加物，不是用户发言，不该在 prompt 里叠成 N 条。
		//
		// 文案分情况：上一批全是 output_send__* 时不能说“继续”，详见
		// replyDeliveredPlaceholder 的说明。
		msgs = dropContinuationPlaceholders(msgs)
		if last := msgs[len(msgs)-1]; last.Role == "assistant" || last.Role == "tool" {
			msgs = append(msgs, agentAPI.Message{
				Role:    "user",
				Content: continuationFor(lastBatchReplyOnly),
			})
		}

		req := &agentAPI.CompletionRequest{
			Messages:        msgs,
			MaxTokens:       4096,
			Tools:           tools,
			ToolChoice:      "auto",
			DisableThinking: !a.thinkingEnabled,
		}

		var providers []agentAPI.Provider
		if a.providerManager != nil {
			// 精确模型名走 byModel 路由；AUTO/空走优先级链
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
						log.Printf("[agent] switched active provider to %q after fallback",
							fbProvider.Name())
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

		if llmErr != nil {
			if errors.Is(llmErr, context.Canceled) && a.ctx.Err() == nil {
				if a.currentOutputChannel == "_consolidation_" {
					return "", toolsUsed, toolResults, fmt.Errorf("interrupted by user input")
				}
				continue
			}
			return "", toolsUsed, toolResults, fmt.Errorf("all %d providers failed, last error: %w",
				len(providers), llmErr)
		}

		stageCtx.LLMText = resp.Content
		stageCtx.ReasoningContent = resp.ReasoningContent
		stageCtx.TokenUsage = map[string]int{
			"prompt_tokens":     resp.TokenUsage.Prompt,
			"completion_tokens": resp.TokenUsage.Completion,
			"total_tokens":      resp.TokenUsage.Total,
		}
		stageCtx.ToolCalls = convertToolCalls(resp.ToolCalls)
		for i := range stageCtx.ToolCalls {
			if stageCtx.ToolCalls[i].Plugin == "" {
				stageCtx.ToolCalls[i].Plugin = a.resolveToolPlugin(stageCtx.ToolCalls[i].Name)
			}
		}
		if a.runStage(sdk.StagePostAction, stageCtx) {
			return *stageCtx.Response, toolsUsed, toolResults, nil
		}
		resp.Content = stageCtx.LLMText
		resp.ToolCalls = convertBackToolCalls(stageCtx.ToolCalls)

		chainPayload := map[string]interface{}{
			"content":    resp.Content,
			"reasoning":  resp.ReasoningContent,
			"tool_calls": resp.ToolCalls,
			"phase":      "intermediate",
			"turn":       turn,
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
			return resp.Content, toolsUsed, toolResults, nil
		}

		// 本批是否全部是输出通道发送（=模型刚交付了给用户的回复）。
		// 必须在执行前判定：执行过程中的中断/拒绝分支会 continue/break，
		// 放在循环里统计会漏。
		replyOnly := true
		for _, tc := range resp.ToolCalls {
			if !isOutputDeliveryTool(tc.Name) {
				replyOnly = false
				break
			}
		}

		contentOnce := true
		for _, tc := range resp.ToolCalls {
			if len(a.interceptCh) > 0 {
				for _, interrupt := range a.drainInterrupts() {
					msgs = append(msgs, agentAPI.Message{Role: "system", Content: "[中断消息] " + interrupt})
				}
				a.publishEvent(events.EventToolCall, map[string]interface{}{
					"tool":    tc.Name,
					"plugin":  a.resolveToolPlugin(tc.Name),
					"args":    tc.Arguments,
					"status":  "interrupted",
					"reason":  "user interrupt before execution",
					"channel": a.currentOutputChannel,
				})
				break
			}

			toolsUsed = append(toolsUsed, tc.Name)
			pluginName := a.resolveToolPlugin(tc.Name)
			log.Printf("[agent] executing tool: %s (plugin=%s, id=%s)", tc.Name, pluginName, tc.ID)
			if tc.RawArguments != "" {
				log.Printf("[agent] tool %s raw_arguments: %s", tc.Name, truncateStr(tc.RawArguments, 300))
			}

			sdkTC := sdk.ToolCall{ID: tc.ID, Name: tc.Name, Plugin: pluginName, Arguments: tc.Arguments}
			stageCtx.ToolCalls = []sdk.ToolCall{sdkTC}
			stageCtx.ToolResults = nil
			if a.runStage(sdk.StageBeforeToolcall, stageCtx) {
				result := fmt.Sprintf("工具 %s 已被插件拒绝", tc.Name)
				msgs = append(msgs, agentAPI.Message{Role: "assistant", ToolCalls: []agentAPI.ToolCall{tc}})
				msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
				a.publishEvent(events.EventToolCall, map[string]interface{}{
					"tool":    tc.Name,
					"plugin":  pluginName,
					"args":    tc.Arguments,
					"result":  result,
					"status":  "denied",
					"channel": a.currentOutputChannel,
				})
				continue
			}
			tc.Arguments = stageCtx.ToolCalls[0].Arguments

			if pluginName != "" && !a.pluginHealth.isHealthy(pluginName) {
				result := fmt.Sprintf("插件 %s 处于崩溃状态，已跳过执行，等待自动恢复重载", pluginName)
				log.Printf("[agent] skip tool %s: plugin %s unhealthy", tc.Name, pluginName)
				msgs = append(msgs, agentAPI.Message{Role: "assistant", ToolCalls: []agentAPI.ToolCall{tc}})
				msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
				continue
			}

			result := a.executeToolCall(tc)
			toolResults = append(toolResults, ToolResultItem{Name: tc.Name, Output: result})
			log.Printf("[agent] tool %s result: %s", tc.Name, truncateStr(result, 100))

			stageCtx.ToolResults = []sdk.ToolResult{{CallID: tc.ID, Name: tc.Name, Plugin: pluginName, Success: true, Result: result}}
			a.runStage(sdk.StageAfterToolcall, stageCtx)
			if len(stageCtx.ToolResults) > 0 {
				if r, ok := stageCtx.ToolResults[0].Result.(string); ok {
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
					// 查询向量取**清洗后**的有效内容，否则噪声（ANSI/base64/JSON
					// 包装）会把相关性打分带偏，裁掉本该保留的事件。
					a.context.Prune(a.toolOutputForQuery(tc.Name, result), topK, a.docStore)
				}
			}

			msgContent := ""
			if contentOnce {
				msgContent = resp.Content
				contentOnce = false
			}
			msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: msgContent, ReasoningContent: resp.ReasoningContent, ToolCalls: []agentAPI.ToolCall{tc}})

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
					// 不存的后果是 ToolResultItem.Output 只剩那句
					// "[已将图片注入后续对话] /tmp/x.png"，文件一删线索就断了。
					a.stageMediaDigests(a.captureBlockMedia(blocks, tc.Name)...)

					if native, fallbackText := a.prepareToolBlocks(blocks); len(native) > 0 {
						// 能直视：另起一条 user message 承载媒体，并补一句来源说明，
						// 否则模型会把它当成用户新发的图而不是工具拉回来的。
						mediaBlocks := append([]agentAPI.ContentBlock{{
							Type: "text",
							Text: fmt.Sprintf("[以下是 %s 注入的媒体内容]", tc.Name),
						}}, native...)
						mediaMsg = &agentAPI.Message{Role: "user", Blocks: mediaBlocks}
					} else if fallbackText != "" {
						// 回退链已把媒体转写成文字：并进 tool message 的纯文本 content，
						// 不再另起消息（文字在 tool message 里本来就能被读到）。
						toolMsg.Content = result + "\n\n" + fallbackText
						result = toolMsg.Content
						if len(toolResults) > 0 {
							toolResults[len(toolResults)-1].Output = result
						}
					}
				}
			}
			msgs = append(msgs, toolMsg)
			if mediaMsg != nil {
				// 必须紧跟在 toolMsg 之后：中间插入其他消息会让 tool_call_id 配对断开。
				msgs = append(msgs, *mediaMsg)
			}

			a.publishEvent(events.EventToolCall, map[string]interface{}{
				"tool":    tc.Name,
				"plugin":  pluginName,
				"args":    tc.Arguments,
				"result":  result,
				"status":  "ok",
				"channel": a.currentOutputChannel,
			})

			if len(a.interceptCh) > 0 {
				for _, interrupt := range a.drainInterrupts() {
					msgs = append(msgs, agentAPI.Message{Role: "system", Content: "[中断消息] " + interrupt})
				}
				break
			}
		}

		// 供下一轮顶部选择补位文案。
		lastBatchReplyOnly = replyOnly
	}
}

// chatStreamWithFallback 优先流式调用 provider，失败时回退非流式 Chat()。
//
// 流式路径：ChatStream 拿到 chunk channel，逐块累积 content/reasoning_content，
// 并发布 EventReasoningDelta / EventContentDelta 增量事件（新订阅者可选订，
// 旧订阅者不认识自然忽略）。流结束后拼出与 Chat() 等价的 CompletionResponse
// 返回——process() 的后续逻辑（stageCtx/聚合事件/工具循环）完全不变。
//
// 回退条件：ChatStream 返回错误（连接失败、provider 不支持流式）。
// 已收到部分 chunk 后出错则不回退（避免重复生成），直接返回已累积内容。
//
// 超时收益：首包 ~1-3s 到达即建立活性，后续只要 token 在流动就不会触发
// 空闲超时；总生成时长不再受限於 180s 整体超时。
func chatStreamWithFallback(ctx context.Context, p agentAPI.Provider, req *agentAPI.CompletionRequest, a *Agent) (*agentAPI.CompletionResponse, error) {
	ch, err := p.ChatStream(ctx, req)
	if err != nil {
		log.Printf("[agent] stream connect failed (%v), falling back to non-stream chat", err)
		return p.Chat(ctx, req)
	}

	resp, accErr := accumulateStream(ctx, ch, a)

	// 中断/超时取消必须保持取消语义传给调用方（与原 Chat() 行为一致：
	// 被 cancel 时丢弃已收内容返回 err），让 process() 的 continue 分支
	// 重启轮次并以 [中断消息] 注入打断内容。绝不能把部分内容当成功返回，
	// 否则用户打断会被无视、继续执行工具/输出。
	if errors.Is(accErr, context.Canceled) || errors.Is(accErr, context.DeadlineExceeded) {
		// 通知客户端：本轮流式作废，清空 delta 累积并定格已显示内容
		if a != nil {
			a.publishEvent(events.EventContentDelta, map[string]interface{}{
				"content": "",
				"channel": a.currentOutputChannel,
				"reset":   true,
			})
		}
		return resp, accErr
	}

	if accErr == nil {
		return resp, nil
	}

	// 其他错误（网络中断等）：已累积到实质内容则返回部分结果，否则回退非流式
	if resp != nil && (resp.Content != "" || len(resp.ToolCalls) > 0) {
		log.Printf("[agent] stream interrupted mid-way (%v), returning partial result", accErr)
		return resp, nil
	}
	log.Printf("[agent] stream failed before content (%v), falling back to non-stream chat", accErr)
	return p.Chat(ctx, req)
}

// toolCallAcc 累积流式 tool call 的各个分片。OpenAI 风格：每个 index 的
// id/name/arguments 跨多个 chunk 增量到达，arguments 是 JSON 字符串分片。
type toolCallAcc struct {
	id      string
	name    string
	argsRaw strings.Builder
}

// accumulateStream 消费 chunk channel，累积为完整 CompletionResponse，
// 同时发布增量事件。返回的 response 与非流式 Chat() 的返回等价。
func accumulateStream(ctx context.Context, ch <-chan agentAPI.StreamChunk, a *Agent) (*agentAPI.CompletionResponse, error) {
	resp := &agentAPI.CompletionResponse{
		ToolCalls: make([]agentAPI.ToolCall, 0),
	}
	accs := make(map[int]*toolCallAcc) // index → 累积中的 tool call
	var lastFinish string

	flushToolCall := func(idx int) {
		acc := accs[idx]
		if acc == nil {
			return
		}
		if acc.name == "" {
			log.Printf("[agent] stream tool_call idx=%d flushed with EMPTY name (args=%q) — dropped", idx, truncateStr(acc.argsRaw.String(), 120))
			delete(accs, idx)
			return
		}
		args, argsOK := parseToolArgsJSON(acc.argsRaw.String())
		raw := strings.TrimSpace(acc.argsRaw.String())
		// 空参诊断：区分「上游没发分片」(raw="")、「混拼污染」(解析失败) 与「合法空对象」({})。
		if !argsOK {
			log.Printf("[agent] stream tool_call %s (idx=%d) argument fragments invalid JSON: %q", acc.name, idx, truncateStr(raw, 200))
		} else if raw == "" {
			log.Printf("[agent] stream tool_call %s (idx=%d) received NO argument fragments", acc.name, idx)
		}
		tc := agentAPI.ToolCall{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: args,
		}
		resp.ToolCalls = append(resp.ToolCalls, tc)
		delete(accs, idx)
	}

	for {
		select {
		case ck, ok := <-ch:
			if !ok {
				for idx := range accs {
					flushToolCall(idx)
				}
				if lastFinish != "" {
					resp.FinishReason = lastFinish
				}
				return resp, nil
			}

			if ck.ReasoningContent != "" {
				resp.ReasoningContent += ck.ReasoningContent
				if a != nil {
					a.publishEvent(events.EventReasoningDelta, map[string]interface{}{
						"content": ck.ReasoningContent,
						"channel": a.currentOutputChannel,
					})
				}
			}
			if ck.Content != "" {
				resp.Content += ck.Content
				if a != nil {
					a.publishEvent(events.EventContentDelta, map[string]interface{}{
						"content": ck.Content,
						"channel": a.currentOutputChannel,
					})
				}
			}

			// 增量 tool call 分片：OpenAI 风格按 index 字段拼接 id/name/arguments。
			// 注意必须用分片自带的 StreamIndex（上游 JSON "index"），不能用 Go
			// range 序号：每个 SSE chunk 通常只含一个 tool_call 元素，slice 序号
			// 恒为 0，并行多工具调用（index=0,1,2...）的分片会全部污染到同一个桶，
			// 导致 name 相互覆盖、args 碎片混拼解析失败（空参数工具调用）。
			for _, tc := range ck.ToolCalls {
				idx := tc.StreamIndex
				if idx == 0 && tc.Name == "" && tc.RawArguments == "" {
					continue
				}
				acc := accs[idx]
				if acc == nil {
					acc = &toolCallAcc{}
					accs[idx] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Name != "" {
					acc.name = tc.Name
				}
				// arguments 以 JSON 字符串分片到达（OpenAI 标准），拼接后最终解析
				if tc.RawArguments != "" {
					acc.argsRaw.WriteString(tc.RawArguments)
				}
			}

			if ck.Done && ck.FinishReason != "" {
				lastFinish = ck.FinishReason
			}
			if ck.Usage != nil {
				resp.TokenUsage = *ck.Usage
			}

		case <-ctx.Done():
			for idx := range accs {
				flushToolCall(idx)
			}
			return resp, ctx.Err()
		}
	}
}

// parseToolArgsJSON 将经过完整拼接的 tool call arguments JSON 字符串解析为 map。
// 第二个返回值 ok=false 表示分片拼接结果不是合法 JSON（分片污染/丢失），
// 与「合法的空对象 {}」相区分。
func parseToolArgsJSON(s string) (map[string]interface{}, bool) {
	if strings.TrimSpace(s) == "" {
		return map[string]interface{}{}, true
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err == nil && m != nil {
		return m, true
	}
	return map[string]interface{}{}, false
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

func (a *Agent) formatMergedTimeline(maxTokens int) string {
	a.context.mu.Lock()
	events := make([]*ContextEvent, len(a.context.events))
	copy(events, a.context.events)
	a.context.mu.Unlock()

	if len(events) == 0 {
		return ""
	}

	// 第一轮：从最新到最旧，计算在预算内能放多少条
	headerTokens := EstimateTokens("【对话时序】\n")
	remaining := maxTokens - headerTokens
	include := 0
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		est := len(e.Source) + len(e.Input) + 40
		if e.Response != "" {
			est += 120
		}
		estTokens := est * 2
		if remaining-estTokens < 0 && include > 0 {
			break
		}
		remaining -= estTokens
		include++
	}
	if include == 0 && len(events) > 0 {
		include = 1
	}

	// 第二轮：按时间正序渲染
	start := len(events) - include
	if start < 0 {
		start = 0
	}
	var sb strings.Builder
	sb.WriteString("【对话时序】\n")
	for _, e := range events[start:] {
		sb.WriteString(fmt.Sprintf("[%s] %s: %s",
			e.Timestamp.Format("15:04:05"), e.Source, e.Input))
		if len(e.ToolsUsed) > 0 {
			sb.WriteString(fmt.Sprintf(" → 调用工具: %s", strings.Join(e.ToolsUsed, ", ")))
		}
		if e.Response != "" {
			sb.WriteString(fmt.Sprintf(" → %s", truncateStr(e.Response, 120)))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func (a *Agent) buildMessages(sysPrompt, input string, ctxTokens int) []agentAPI.Message {
	msgs := []agentAPI.Message{{Role: "system", Content: sysPrompt}}

	if ctxTok := a.formatMergedTimeline(ctxTokens); ctxTok != "" {
		msgs = append(msgs, agentAPI.Message{Role: "system", Content: ctxTok})
	}

	msgs = append(msgs, agentAPI.Message{Role: "user", Content: input})
	return msgs
}
