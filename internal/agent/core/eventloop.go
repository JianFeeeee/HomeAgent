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
	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
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
		case msg := <-a.selfInputCh:
			a.handleSelfInput(msg)
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

// channelConsolidation 标记记忆整理类自输入：无记忆路径处理，
// 不写入对话上下文、不向任何输出通道 emit 响应。
const channelConsolidation = "_consolidation_"

// selfInputMsg 自循环输入消息。channel 决定处理路径：
//   - channelConsolidation：记忆整理，无记忆（不污染上下文/知识库）
//   - 其他值（如 "cli"、"webui"）：正常输入路径，写入上下文并 emit 响应
//     （典型场景：子 Agent 完成通知，需让父 Agent 感知并可回复用户）
type selfInputMsg struct {
	text    string
	channel string
}

func (a *Agent) handleSelfInput(msg selfInputMsg) {
	if msg.channel == "" {
		msg.channel = channelConsolidation // 兼容空值：默认走整理路径
	}
	a.processInput(&agentIO.InputEvent{
		Source:        "system",
		Type:          "text",
		Payload:       map[string]interface{}{"content": msg.text},
		OutputChannel: msg.channel,
	})
}

func (a *Agent) handleInput(evt *agentIO.InputEvent) {
	switch evt.Type {
	case "text", "image", "audio":
		a.processInput(evt)

	case "event":
		log.Printf("[agent] event from %s: %v", evt.Source, evt.Payload)

	case "command":
		cmd, _ := evt.Payload["command"].(string)
		log.Printf("[agent] command from %s: %s", evt.Source, cmd)

	default:
		log.Printf("[agent] unknown event type from %s: %s", evt.Source, evt.Type)
	}
}

// inputPayload 是一次输入在「模态」这个维度上的全部内容。
//
// 拆出这个结构，是为了让 processInput 只有一条主干：模态不再决定走哪个函数，
// 只决定这里的字段填不填。此前 text 与 image/audio 各有一个 process 函数，
// 媒体那条缺了去重、no_memory、通道 Cleaner、中断语义、EventRawInput 五项——
// 不是因为媒体不需要，而是复制粘贴之后文本那条继续演进、媒体那条没跟上。
type inputPayload struct {
	// text 是进 LLM 与记忆的文本。纯媒体输入时它是 mediaToBlocks 给的 alt 文案。
	text string
	// blocks 非空表示本轮带多模态内容，随当前轮的 message 一起发给模型。
	blocks []agentAPI.ContentBlock
	// mediaType 供插件在 stage 里判断本轮媒体的模态。
	mediaType string
	// captureTool 是媒体落进 CAS 时记录的来源标签。
	captureTool string
}

// resolveInput 把 InputEvent 归一成 inputPayload。
//
// 三种来源在这里合流：
//  1. evt.Type 是 image/audio —— 用户直接发的媒体，payload 里是 data/url；
//  2. evt.Type 是 text 且 payload 带 media_blocks —— 插件经 IOInjector 的
//     InjectInputMedia / InjectInputMediaSync / InjectInterruptMedia 注入的
//     媒体，块已经是成品；
//  3. 纯文本。
//
// 第 2 种此前无处可去：注入方把块放进 payload，而文本路径不看这个键，
// 于是插件注入的媒体到 payload 就断了，且不报错。
func (a *Agent) resolveInput(evt *agentIO.InputEvent) (inputPayload, bool) {
	switch evt.Type {
	case "image", "audio":
		blocks, alt := a.mediaToBlocks(evt.Payload, evt.Type, evt.Source)
		return inputPayload{
			text:        alt,
			blocks:      blocks,
			mediaType:   evt.Type,
			captureTool: "input_" + evt.Type,
		}, true
	}

	text, _ := evt.Payload["content"].(string)
	blocks, mediaType := injectedBlocks(evt.Payload)
	// 文本与媒体都空才算无效输入：只带图不带字是合法的（插件注入常这样）。
	if text == "" && len(blocks) == 0 {
		return inputPayload{}, false
	}
	return inputPayload{
		text:        text,
		blocks:      blocks,
		mediaType:   mediaType,
		captureTool: "inject_" + evt.Source,
	}, true
}

// injectedBlocks 取出 payload 里插件注入的多模态块。
//
// 两种静态类型都要认：内核内部注入直接给 []agentAPI.ContentBlock，
// 而经公共 SDK 的 IOInjector 过来的是 []pubsdk.ContentBlock。两者字段完全一致，
// 但 Go 不会自动转换，只认一种的后果是另一种被静默丢弃。
func injectedBlocks(payload map[string]interface{}) ([]agentAPI.ContentBlock, string) {
	var blocks []agentAPI.ContentBlock
	switch v := payload["media_blocks"].(type) {
	case []agentAPI.ContentBlock:
		blocks = v
	case []pubsdk.ContentBlock:
		blocks = make([]agentAPI.ContentBlock, 0, len(v))
		for _, b := range v {
			nb := agentAPI.ContentBlock{Type: b.Type, Text: b.Text}
			if b.ImageURL != nil {
				nb.ImageURL = &agentAPI.ImageURL{URL: b.ImageURL.URL, Detail: b.ImageURL.Detail}
			}
			if b.AudioURL != nil {
				nb.AudioURL = &agentAPI.AudioURL{URL: b.AudioURL.URL}
			}
			blocks = append(blocks, nb)
		}
	}
	if len(blocks) == 0 {
		return nil, ""
	}
	// 模态由块自身判定，注入方不必额外声明。图优先：一次注入里图片是主体。
	mediaType := ""
	for _, b := range blocks {
		if b.ImageURL != nil {
			return blocks, "image"
		}
		if b.AudioURL != nil {
			mediaType = "audio"
		}
	}
	return blocks, mediaType
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
				Type:     "image_url",
				ImageURL: &agentAPI.ImageURL{URL: imgURL, Detail: "auto"},
			})
		} else if mediaType == "audio" {
			blocks = append(blocks, agentAPI.ContentBlock{
				Type:     "audio_url",
				AudioURL: &agentAPI.AudioURL{URL: imgURL},
			})
		}
	}

	return blocks, alt
}

// processInput 是全部模态输入的唯一主干。
//
// 文本、用户上传的图/音频、插件注入的多模态块走同一条路径，因此去重、
// no_memory、通道 Cleaner、中断语义、EventRawInput、媒体入 CAS、媒体记忆绑定
// 对所有模态一致——不会再出现「文本路径加了功能、媒体路径没跟上」。
func (a *Agent) processInput(evt *agentIO.InputEvent) {
	start := time.Now()

	in, ok := a.resolveInput(evt)
	if !ok {
		return
	}

	// 去重按文本做：webui/GUI 断线重连会重放未确认消息。
	// 带媒体时跳过——媒体输入的 alt 文案（"[从 qq 收到了 image]"）对不同图片
	// 是同一句，拿它去重会把连发的两张图误判成重复。
	if len(in.blocks) == 0 && a.isDuplicateInput(evt.Source, in.text) {
		log.Printf("[agent] dropped duplicate input from %s: %s", evt.Source, truncateStr(in.text, 60))
		return
	}

	a.currentOutputChannel = evt.OutputChannel
	if a.currentOutputChannel == "" {
		a.currentOutputChannel = evt.Source
	}

	if evt.OutputChannel == "_consolidation_" {
		a.processConsolidation(evt, in.text)
		return
	}

	// pendingMedia 让 describe_image / transcribe_audio / ocr_image 拿到本轮媒体的
	// 原始 data/url，也是这三个工具是否出现在工具表里的开关。仅对用户直接上传成立
	//（payload 里才有 data/url）；插件注入的是成品 block，取不到原始数据。
	if evt.Type == "image" || evt.Type == "audio" {
		a.pendingMedia = evt.Payload
		defer func() { a.pendingMedia = nil }()
	}

	// 媒体先落进 CAS。不存的后果是 ContextEvent.Input 只剩一句 alt 文本，
	// base64 随 message 数组发给模型后就丢了。
	if len(in.blocks) > 0 {
		a.stageMediaDigests(a.captureBlockMedia(in.blocks, in.captureTool)...)
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

	stageCtx := a.stageCtxFromInput(in.text, evt.Source, "")
	stageCtx.Extra["input_source"] = evt.Source
	stageCtx.Extra["output_channel"] = evt.OutputChannel
	if len(in.blocks) > 0 {
		stageCtx.Extra["media_blocks"] = in.blocks
		stageCtx.Extra["media_type"] = in.mediaType
	}
	if noMemory {
		stageCtx.NoMemory = true
	}
	a.injectSourceContext(stageCtx, evt)

	if a.runStage(sdk.StageOnInput, stageCtx) {
		a.emitResponse(evt, *stageCtx.Response)
		return
	}

	input := stageCtx.RawMessage

	// 计算层用的清洗文本（不改原文）：通道 Cleaner 提取语义内容后用于向量化/提关键词
	cleanInput := input
	if a.io != nil {
		if chDef, ok := a.io.GetInputChannelDef(evt.Source); ok && chDef.Cleaner != nil {
			cleanInput = chDef.Cleaner(input)
		}
	}

	// upload_* 字段一并转发：webui 的 EventRawInput 订阅方靠它们还原附件卡片。
	// 媒体路径此前把整个 payload 塞进 content（一个 map），订阅方按 string 断言
	// 直接失败 → 用户发的图从不出现在聊天记录里。
	rawPayload := map[string]interface{}{"content": input, "source": evt.Source}
	for _, k := range []string{"upload_url", "upload_type", "upload_size", "upload_name"} {
		if v, ok := evt.Payload[k]; ok {
			rawPayload[k] = v
		}
	}
	a.publishEvent(events.EventRawInput, rawPayload)

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
		log.Printf("[agent] process %s error: %v", evt.Type, err)
		resp := fmt.Sprintf("处理错误: %v", err)
		a.emitResponse(evt, resp)
		a.context.Append(ContextEvent{Timestamp: time.Now(), Source: "agent", Input: input, Response: resp})
		return
	}

	elapsed := time.Since(start)
	log.Printf("[agent] %s from %s → response (%dms, tools=%v)", evt.Type, evt.Source, elapsed.Milliseconds(), toolsUsed)

	// 本轮捕获的媒体一起挂到这条事件上：用户上传的、插件注入的，以及模型调
	// multimodal_see_picture / see_video 时经 SetToolBlocks 注入的（后者在
	// process() 里被捕获，纯文本输入也会有）。
	turnEvt := ContextEvent{
		Timestamp:   time.Now(),
		Source:      "agent",
		Input:       cleanInput,
		Response:    response,
		ToolsUsed:   toolsUsed,
		ToolResults: toolResults,
	}
	a.bindEventMedia(&turnEvt, a.drainMediaDigests())
	if s := a.mediaSummaryForEvent(turnEvt.Blocks); s != "" {
		turnEvt.Input = turnEvt.Input + "\n" + s
	}
	a.context.Append(turnEvt)

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
