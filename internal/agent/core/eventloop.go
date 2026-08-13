package core

import (
	"fmt"
	"log"
	"runtime/debug"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func (a *Agent) eventLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] eventLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.eventLoop()
		}
	}()
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

func (a *Agent) interceptLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] interceptLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.interceptLoop()
		}
	}()
	for {
		select {
		case evt := <-a.io.InputInterruptChan():
			text, _ := evt.Payload["content"].(string)
			if text == "" {
				continue
			}
			log.Printf("[agent] interrupt from %s/%s: %s", evt.Source, evt.OutputChannel, truncateStr(text, 80))

			clone := &agentIO.InputEvent{
				RequestID:     evt.RequestID,
				Source:        evt.Source,
				Type:          evt.Type,
				Payload:       map[string]interface{}{},
				OutputChannel: evt.OutputChannel,
			}
			for k, v := range evt.Payload {
				clone.Payload[k] = v
			}
			clone.Payload["interrupt"] = true
			clone.Payload["interrupt_source"] = evt.Source
			clone.Payload["interrupt_channel"] = evt.OutputChannel

			a.llmMu.Lock()
			hasActiveLLM := a.cancelLLM != nil
			if hasActiveLLM {
				a.cancelLLM()
				log.Printf("[agent] LLM request cancelled by interrupt")
			}
			a.llmMu.Unlock()

			if hasActiveLLM {
				if a.currentOutputChannel == "_consolidation_" {
					log.Printf("[agent] consolidation interrupted, re-injecting input for %s/%s", evt.Source, evt.OutputChannel)
					a.io.InjectInputTo(evt.Source, evt.OutputChannel, "text", map[string]interface{}{
						"content":           text,
						"interrupt":         true,
						"interrupt_source":  evt.Source,
						"interrupt_channel": evt.OutputChannel,
					})
				} else {
					select {
					case a.interceptCh <- clone:
					default:
						log.Printf("[agent] intercept channel full, queuing input for %s", evt.Source)
						a.io.InjectInputTo(evt.Source, evt.OutputChannel, "text", map[string]interface{}{
							"content":           text,
							"interrupt":         true,
							"interrupt_source":  evt.Source,
							"interrupt_channel": evt.OutputChannel,
						})
					}
				}
			} else {
				a.io.InjectInputTo(evt.Source, evt.OutputChannel, "text", map[string]interface{}{
					"content":           text,
					"interrupt":         true,
					"interrupt_source":  evt.Source,
					"interrupt_channel": evt.OutputChannel,
				})
			}

		case <-a.ctx.Done():
			return
		}
	}
}

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

func (a *Agent) processMediaInput(evt *agentIO.InputEvent) {
	start := time.Now()
	a.pendingMedia = evt.Payload
	defer func() { a.pendingMedia = nil }()

	a.currentOutputChannel = evt.OutputChannel
	if a.currentOutputChannel == "" {
		a.currentOutputChannel = evt.Source
	}

	blocks, fallback := a.mediaToBlocks(evt.Payload, evt.Type, evt.Source)

	stageCtx := a.stageCtxFromInput(fallback, evt.Source, "")
	stageCtx.Extra = map[string]interface{}{
		"media_blocks":   blocks,
		"media_type":     evt.Type,
		"input_source":   evt.Source,
		"output_channel": evt.OutputChannel,
	}
	a.injectSourceContext(stageCtx, evt)

	if a.runStage(sdk.StageOnInput, stageCtx) {
		a.emitResponse(evt, *stageCtx.Response)
		return
	}

	a.publishEvent(events.EventRawInput, map[string]interface{}{
		"content": evt.Payload,
		"source":  evt.Source,
	})

	archived := a.context.Prune(fallback, a.maxContextSize-1, a.docStore)
	if archived > 0 {
		log.Printf("[agent] pruned %d low-relevance events to document memory", archived)
	}

	a.context.Append(ContextEvent{
		Timestamp: start,
		Source:    evt.Source,
		Input:     fallback,
	})

	response, toolsUsed, toolResults, err := a.process(fallback, stageCtx)
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
		Timestamp:   time.Now(),
		Source:      "agent",
		Input:       fallback,
		Response:    response,
		ToolsUsed:   toolsUsed,
		ToolResults: toolResults,
	})

	a.emitResponse(evt, response)

	if !stageCtx.NoMemory {
		a.emitMemoryCandidate(evt.Source, fallback, response, toolResults, toolsUsed)
	}
}

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

	a.currentOutputChannel = evt.OutputChannel
	if a.currentOutputChannel == "" {
		a.currentOutputChannel = evt.Source
	}

	if evt.OutputChannel == "_consolidation_" {
		a.processConsolidation(evt, input)
		return
	}

	noMemory := false
	if v, ok := evt.Payload["no_memory"].(bool); ok {
		noMemory = v
	}
	if !noMemory && a.io != nil {
		if chDef, ok := a.io.GetInputChannelDef(evt.Source); ok && chDef.NoMemory {
			noMemory = true
		}
	}

	// 工具提醒/中断（terminal_watch、timer 等）不是用户发言：
	// 以 system 角色注入 LLM，且不写入用户对话履历。
	isInterrupt, _ := evt.Payload["interrupt"].(bool)
	a.mu.Lock()
	a.interruptInput = isInterrupt
	a.mu.Unlock()
	if isInterrupt {
		noMemory = true
	}

	stageCtx := a.stageCtxFromInput(input, evt.Source, "")
	stageCtx.Extra["input_source"] = evt.Source
	stageCtx.Extra["output_channel"] = evt.OutputChannel
	if noMemory {
		stageCtx.NoMemory = true
	}
	a.injectSourceContext(stageCtx, evt)

	if a.runStage(sdk.StageOnInput, stageCtx) {
		a.emitResponse(evt, *stageCtx.Response)
		return
	}

	input = stageCtx.RawMessage

	// 计算层用的清洗文本（不改原文）：通道 Cleaner 提取语义内容后用于向量化/提关键词
	cleanInput := input
	if a.io != nil {
		if chDef, ok := a.io.GetInputChannelDef(evt.Source); ok && chDef.Cleaner != nil {
			cleanInput = chDef.Cleaner(input)
		}
	}

	a.publishEvent(events.EventRawInput, map[string]interface{}{
		"content": input,
		"source":  evt.Source,
	})

	archived := a.context.Prune(cleanInput, a.maxContextSize-1, a.docStore)
	if archived > 0 {
		log.Printf("[agent] pruned %d low-relevance events to document memory", archived)
	}

	if !isInterrupt {
		a.context.Append(ContextEvent{
			Timestamp: start,
			Source:    evt.Source,
			Input:     input,
		})
	}

	response, toolsUsed, toolResults, err := a.process(input, stageCtx)
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
		Timestamp:   time.Now(),
		Source:      "agent",
		Input:       cleanInput,
		Response:    response,
		ToolsUsed:   toolsUsed,
		ToolResults: toolResults,
	})

	a.emitResponse(evt, response)

	if !stageCtx.NoMemory {
		a.emitMemoryCandidate(evt.Source, cleanInput, response, toolResults, toolsUsed)
	}
}

func (a *Agent) emitResponse(evt *agentIO.InputEvent, response string) {
	stageCtx := &sdk.StageContext{
		FinalText: response,
		Phase:     sdk.StageBeforeOutput,
	}
	a.runStage(sdk.StageBeforeOutput, stageCtx)
	response = stageCtx.FinalText

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

	out := map[string]interface{}{
		"content": response,
		"channel": ch,
		"source":  evt.Source,
	}
	if stageCtx.ReasoningContent != "" {
		out["reasoning_content"] = stageCtx.ReasoningContent
	}
	a.publishEvent(events.EventAgentOutput, out)
	stageCtx.Phase = sdk.StageAfterOutput
	a.runStage(sdk.StageAfterOutput, stageCtx)
}

func (a *Agent) drainInterrupts() []string {
	var out []string
	for {
		select {
		case evt := <-a.interceptCh:
			if evt == nil {
				continue
			}
			text, _ := evt.Payload["content"].(string)
			if text == "" {
				continue
			}
			source := evt.Source
			if source == "" {
				source = "unknown"
			}
			channel := evt.OutputChannel
			if channel == "" {
				channel = source
			}
			out = append(out, fmt.Sprintf("[打断消息][来源:%s][输出通道:%s] %s", source, channel, text))
		default:
			return out
		}
	}
}
