package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

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
	if blocks, ok := stageCtx.Extra["media_blocks"].([]agentAPI.ContentBlock); ok && len(blocks) > 0 {
		if len(msgs) > 0 {
			msgs[len(msgs)-1].Blocks = blocks
		}
	}

	log.Printf("[agent] tool call loop start, %d tools, %d context events, personality=%t, docs=%d",
		len(tools), a.context.Len(),
		a.personality != nil && a.personality.Content != "",
		a.docStoreSize())

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

	for turn := 0; ; turn++ {
		for _, interrupt := range a.drainInterrupts() {
			msgs = append(msgs, agentAPI.Message{
				Role:    "system",
				Content: interrupt,
			})
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

		var providers []agentAPI.Provider
		if a.providerManager != nil {
			allProviders := a.providerManager.OrderedProviders()
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

			fCtx, fCancel := context.WithCancel(a.ctx)
			a.llmMu.Lock()
			a.cancelLLM = fCancel
			a.llmMu.Unlock()

			resp, llmErr = fbProvider.Chat(fCtx, req)

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
					return "", toolsUsed, fmt.Errorf("interrupted by user input")
				}
				continue
			}
			return "", toolsUsed, fmt.Errorf("all %d providers failed, last error: %w",
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
			return *stageCtx.Response, toolsUsed, nil
		}
		resp.Content = stageCtx.LLMText
		resp.ToolCalls = convertBackToolCalls(stageCtx.ToolCalls)

		chainPayload := map[string]interface{}{
			"content":          resp.Content,
			"reasoning":        resp.ReasoningContent,
			"tool_calls":       resp.ToolCalls,
			"phase":            "intermediate",
			"turn":             turn,
		}
		if resp.TokenUsage.Total > 0 {
			chainPayload["usage"] = map[string]int{
				"prompt":     resp.TokenUsage.Prompt,
				"completion": resp.TokenUsage.Completion,
				"total":      resp.TokenUsage.Total,
			}
		}
		a.publishEvent(events.EventAgentLLMChain, chainPayload)

		if len(resp.ToolCalls) == 0 {
			return resp.Content, toolsUsed, nil
		}

		for _, tc := range resp.ToolCalls {
			if len(a.interceptCh) > 0 {
				for _, interrupt := range a.drainInterrupts() {
					msgs = append(msgs, agentAPI.Message{Role: "system", Content: interrupt})
				}
				a.publishEvent(events.EventToolCall, map[string]interface{}{
					"tool":   tc.Name,
					"plugin": a.resolveToolPlugin(tc.Name),
					"args":   tc.Arguments,
					"status": "interrupted",
					"reason": "user interrupt before execution",
				})
				break
			}

			toolsUsed = append(toolsUsed, tc.Name)
			pluginName := a.resolveToolPlugin(tc.Name)
			log.Printf("[agent] executing tool: %s (plugin=%s, id=%s)", tc.Name, pluginName, tc.ID)

			sdkTC := sdk.ToolCall{ID: tc.ID, Name: tc.Name, Plugin: pluginName, Arguments: tc.Arguments}
			stageCtx.ToolCalls = []sdk.ToolCall{sdkTC}
			stageCtx.ToolResults = nil
			if a.runStage(sdk.StageBeforeToolcall, stageCtx) {
				result := fmt.Sprintf("工具 %s 已被插件拒绝", tc.Name)
				msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: resp.Content, ToolCalls: []agentAPI.ToolCall{tc}})
				msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
				a.publishEvent(events.EventToolCall, map[string]interface{}{
					"tool":   tc.Name,
					"plugin": pluginName,
					"args":   tc.Arguments,
					"result": result,
					"status": "denied",
				})
				continue
			}
			tc.Arguments = stageCtx.ToolCalls[0].Arguments

			if pluginName != "" && !a.pluginHealth.isHealthy(pluginName) {
				result := fmt.Sprintf("插件 %s 处于崩溃状态，已跳过执行，等待自动恢复重载", pluginName)
				log.Printf("[agent] skip tool %s: plugin %s unhealthy", tc.Name, pluginName)
				msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: resp.Content, ToolCalls: []agentAPI.ToolCall{tc}})
				msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
				continue
			}

			result := a.executeToolCall(tc)
			log.Printf("[agent] tool %s result: %s", tc.Name, truncateStr(result, 100))

			stageCtx.ToolResults = []sdk.ToolResult{{CallID: tc.ID, Name: tc.Name, Plugin: pluginName, Success: true, Result: result}}
			a.runStage(sdk.StageAfterToolcall, stageCtx)
			if len(stageCtx.ToolResults) > 0 {
				if r, ok := stageCtx.ToolResults[0].Result.(string); ok {
					result = r
				}
			}

			argsJSON, _ := json.Marshal(tc.Arguments)
			a.recordToolCall(tc.Name, string(argsJSON), result)

			msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: resp.Content, ToolCalls: []agentAPI.ToolCall{tc}})
			msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})

			a.publishEvent(events.EventToolCall, map[string]interface{}{
				"tool":   tc.Name,
				"plugin": pluginName,
				"args":   tc.Arguments,
				"result": result,
				"status": "ok",
			})

			if len(a.interceptCh) > 0 {
				for _, interrupt := range a.drainInterrupts() {
					msgs = append(msgs, agentAPI.Message{Role: "system", Content: interrupt})
				}
				break
			}
		}
	}
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

func (a *Agent) recordToolCall(name, args, result string) {
	a.toolCallRingMu.Lock()
	defer a.toolCallRingMu.Unlock()

	if len(args) > 200 {
		args = args[:200] + "..."
	}

	var resultStub string
	var fullResult string
	if len(a.toolCallRing) < 5 {
		fullResult = result
	}
	if len(result) > 80 {
		resultStub = result[:80] + "..."
	} else {
		resultStub = result
	}

	rec := ToolCallRecord{
		Timestamp:  time.Now(),
		Name:       name,
		Args:       args,
		ResultStub: resultStub,
		FullResult: fullResult,
	}

	if len(a.toolCallRing) >= a.toolCallRingMax {
		a.toolCallRing = a.toolCallRing[1:]
	}
	a.toolCallRing = append(a.toolCallRing, rec)
}

func (a *Agent) formatToolCallRing() string {
	a.toolCallRingMu.Lock()
	defer a.toolCallRingMu.Unlock()

	if len(a.toolCallRing) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("【已执行工具记录(最近40条)】\n")
	start := 0
	if len(a.toolCallRing) > 40 {
		start = len(a.toolCallRing) - 40
	}
	for i, rec := range a.toolCallRing[start:] {
		if len(rec.FullResult) > 0 {
			sb.WriteString(fmt.Sprintf("  [%d] %s: %s(%s)=%s\n", i+1,
				rec.Timestamp.Format("15:04:05"), rec.Name, rec.Args,
				truncateStr(rec.FullResult, 120)))
		} else {
			sb.WriteString(fmt.Sprintf("  [%d] %s: %s(%s) → (已缓存，具体结果通过文本记忆层获取)\n", i+1,
				rec.Timestamp.Format("15:04:05"), rec.Name, rec.Args))
		}
	}
	return sb.String()
}

func (a *Agent) formatMergedTimeline() string {
	a.context.mu.Lock()
	events := make([]*ContextEvent, len(a.context.events))
	copy(events, a.context.events)
	a.context.mu.Unlock()

	a.toolCallRingMu.Lock()
	ring := make([]ToolCallRecord, len(a.toolCallRing))
	copy(ring, a.toolCallRing)
	a.toolCallRingMu.Unlock()

	if len(events) == 0 && len(ring) == 0 {
		return ""
	}

	type timelineEntry struct {
		ts    time.Time
		label string
		text  string
	}
	entries := make([]timelineEntry, 0, len(events)+len(ring))

	for _, e := range events {
		text := fmt.Sprintf("[对话] %s: %s", e.Source, e.Input)
		if len(e.ToolsUsed) > 0 {
			text += fmt.Sprintf(" → 调用工具: %s", strings.Join(e.ToolsUsed, ", "))
		}
		if e.Response != "" {
			text += fmt.Sprintf(" → %s", truncateStr(e.Response, 120))
		}
		entries = append(entries, timelineEntry{ts: e.Timestamp, label: "对话", text: text})
	}

	for _, r := range ring {
		text := fmt.Sprintf("[工具] %s(%s)", r.Name, r.Args)
		if r.FullResult != "" {
			text += fmt.Sprintf(" = %s", truncateStr(r.FullResult, 120))
		} else {
			text += " → (结果已缓存，可通过文本记忆层获取)"
		}
		entries = append(entries, timelineEntry{ts: r.Timestamp, label: "工具", text: text})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ts.Before(entries[j].ts)
	})

	var sb strings.Builder
	sb.WriteString("【对话时序】\n")
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("[%s] %s\n", e.ts.Format("15:04:05"), e.text))
	}
	return sb.String()
}

func (a *Agent) buildMessages(sysPrompt, input string) []agentAPI.Message {
	msgs := []agentAPI.Message{{Role: "system", Content: sysPrompt}}

	if ctxStr := a.formatMergedTimeline(); ctxStr != "" {
		msgs = append(msgs, agentAPI.Message{Role: "system", Content: ctxStr})
	}

	msgs = append(msgs, agentAPI.Message{Role: "user", Content: input})
	return msgs
}
