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

// eventLoop 已由 scheduler.go 的 schedulerLoop 取代（M2）。
//
// 原实现直接在 select 里处理 inputCh/selfInputCh，没有任何可枚举的队列、
// 无法承载优先级与抢占；现在任务先入就绪队列，由选择函数 pickTaskIndex 决定下一个。
// 兼容性说明：M2 全部任务为 LevelBackground，因此行为等价于原先的 FIFO。

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

			// 决策交给调度器：requestPreempt 总会登记中断（进中断队列或 immediate，
			// 因而不会丢），仅当它会真抢占时才告诉我“该取消可取消的步骤”。
			// 本 goroutine 不碰任何帧——只写中断队列与让位信号。
			//
			// 级别由来源声明（InjectOptions.Priority → payload["priority"]）；
			// 未声明一律 L1。L4（“立即打断”）只有内核级插件能声明，
			// 外部插件即便报了 L4 也会被夹到 L3；内核自身另有 raiseKernelInterrupt。
			level := interruptLevel(evt, a.isKernelLevelSource(evt.Source))
			if a.sched.requestPreempt(clone, level) {
				a.cancelCurrentLLM()
			}

		case <-a.ctx.Done():
			return
		}
	}
}

// cancelCurrentLLM 取消正在进行的 LLM 请求（若有）。
//
// 只有 LLM 流式步骤是可取消的；工具 RPC / ONNX / CAS 在 v1 是临界区，
// 取消对它们无效——让位信号会等它们自然结束后的安全点（设计文档 D2）。
func (a *Agent) cancelCurrentLLM() {
	a.llmMu.Lock()
	cancel := a.cancelLLM
	a.llmMu.Unlock()
	if cancel != nil {
		cancel()
		log.Printf("[agent] LLM request cancelled by preemption")
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

// selfEvent 把内核自循环消息归一成输入事件。
func selfEvent(msg selfInputMsg) *agentIO.InputEvent {
	if msg.channel == "" {
		msg.channel = channelConsolidation // 兼容空值：默认走整理路径
	}
	return &agentIO.InputEvent{
		Source:        "system",
		Type:          "text",
		Payload:       map[string]interface{}{"content": msg.text},
		OutputChannel: msg.channel,
	}
}

func (a *Agent) handleSelfInput(msg selfInputMsg) {
	_, _ = a.runInputTask(selfEvent(msg))
}

func (a *Agent) handleInput(evt *agentIO.InputEvent) {
	switch evt.Type {
	case "text", "image", "audio":
		_, _ = a.runInputTask(evt)

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

// emitSkippedReply 给被跳过任务的**同步**调用方一个终态。
//
// 为什么要单独一条路径而不是复用 emitResponse：跳过意味着“我们没有处理这条输入”，
// 不应对外发 agent_output 事件（否则 WebUI 聊天记录会凭空多出一条空消息），
// 但必须写 ResponseCh——否则 cli/clawhub 这类无超时的同步注入会永久挂起。
//
// 非阻塞写：ResponseCh 由同步调用方以 cap=1 创建，调用方超时离开后仍可写入。
func (a *Agent) emitSkippedReply(evt *agentIO.InputEvent, reason string) {
	if evt == nil || evt.ResponseCh == nil {
		return
	}
	ch := evt.OutputChannel
	if ch == "" {
		ch = evt.Source
	}
	payload := map[string]interface{}{
		"content":    "",
		"request_id": evt.RequestID,
		"skipped":    true,
		"reason":     reason,
	}
	select {
	case evt.ResponseCh <- &agentIO.OutputEvent{
		RequestID:     evt.RequestID,
		Target:        evt.Source,
		Type:          "text",
		Payload:       payload,
		Done:          true,
		OutputChannel: ch,
	}:
	default:
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

// pruneOnInput 按声明的上下文策略裁剪上下文，返回归档的事件数。
//
// 默认**不裁剪**：ContextPolicy 必须在注入点（payload 的 context_policy）
// 或通道定义（ChannelDef.ContextPolicy）上显式声明为 prune 才会裁剪。
//
// 为什么把无条件裁剪改成需声明：裁剪会把低相关事件归档到文档记忆并从上下文里
// 移走，是破坏性的。此前每条输入都裁一次，于是「谁把上下文裁了」在排查时无从
// 得知；而插件注入的内容也会被不相关的内容挤掉。按来源/注入点声明后，触发条件
// 是可枚举、可审计的。
//
// 查询向量取**清洗后**的输入（通道 Cleaner 的输出），与工具侧同一套语义：
// 原始输入里的 ANSI/base64/JSON 包装会把相关性打分带偏，裁掉本该保留的事件。
func (a *Agent) pruneOnInput(evt *agentIO.InputEvent, cleanInput string) int {
	if a.context == nil || !a.pruneDeclared(evt) {
		return 0
	}
	topK := a.maxContextSize - 1
	if topK < 1 {
		topK = 1
	}
	return a.context.Prune(cleanInput, topK, a.docStore)
}

// pruneDeclared 判定这次输入是否显式声明了裁剪。
//
// 优先级：注入点声明的（payload）> 通道声明的（ChannelDef）> 默认不裁剪。
// 注入点是更窄的声明面，同一通道下的不同注入可以有不同意图。
func (a *Agent) pruneDeclared(evt *agentIO.InputEvent) bool {
	if p, ok := evt.Payload["context_policy"].(string); ok && p != "" {
		return p == pubsdk.ContextPolicyPrune
	}
	if a.io != nil {
		if chDef, ok := a.io.GetInputChannelDef(evt.Source); ok {
			return chDef.ContextPolicy == pubsdk.ContextPolicyPrune
		}
	}
	return false
}

// cleanInputFor 解析这条输入在计算层应当使用的清洗文本。
//
// 优先级：注入点声明的 cleaner（payload.cleaner_name，引用某个已注册的通道
// cleaner）> 按 source 查到的通道 cleaner > 原文。
//
// 声明的 cleaner 名字查不到时**记日志并回退**，而不是静默当没声明：
// 注入是 fire-and-forget 的，插件那边看不到错误；至少要在内核日志里留下
// 「你声明的清洗没生效」的痕迹，否则排查时只能看到「记忆里的内容很脏」。
func (a *Agent) cleanInputFor(evt *agentIO.InputEvent, input string) string {
	if a.io == nil {
		return input
	}
	if name, ok := evt.Payload["cleaner_name"].(string); ok && name != "" {
		if chDef, ok := a.io.GetInputChannelDef(name); ok && chDef.Cleaner != nil {
			return chDef.Cleaner(input)
		}
		log.Printf("[agent] 注入声明了 cleaner_name=%q 但没有注册过该通道的 Cleaner，已回退", name)
	}
	if chDef, ok := a.io.GetInputChannelDef(evt.Source); ok && chDef.Cleaner != nil {
		return chDef.Cleaner(input)
	}
	return input
}
