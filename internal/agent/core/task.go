package core

// 任务状态机（M1：行为等价的纯重构）。
//
// 背景与设计见 docs/zh/input-scheduler-design.md。
//
// M1 只做一件事：把原先「一个 425 行的 process() 大函数」拆成
// **显式 step 游标 + TaskFrame**。目的不是加能力，而是让「现场」变成数据——
// 之后 M3 才能把帧存进 suspendStack 并在安全点恢复。
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
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
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
	// outcomeSuspended 任务在安全点被抢占挂起，帧已保存（M3b 起使用）。
	outcomeSuspended
)

// taskTerminal 是任务的终态种类（设计文档 §7：每个任务恰有一个终态）。
type taskTerminal int

const (
	// terminalNone 任务尚未结束，需进入 run 段。
	terminalNone taskTerminal = iota
	// terminalOK 正常完成（已提交上下文并回执）。
	terminalOK
	// terminalError 执行出错（已提交错误响应）。
	terminalError
	// terminalStageShortCircuit 被 on_input 阶段短路（响应已发出）。
	terminalStageShortCircuit
	// terminalSkipped 未进入执行：解析失败或被去重。
	terminalSkipped
	// terminalConsolidation 走记忆整理专用路径，已处理完毕。
	terminalConsolidation
	// terminalSuspended 被抢占挂起，等待恢复（M3b 起使用）。
	terminalSuspended
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

	// ---- 任务层现场（原 processInput 的局部变量）----
	//
	// 这些字段让帧覆盖 prepare → step… → finish 全生命周期：挂起发生在 run 段的
	// 安全点，恢复后由 finish 段统一提交（context.Append + emitResponse +
	// emitMemoryCandidate），因此挂起不会重复提交。
	Evt         *agentIO.InputEvent
	CleanInput  string
	IsInterrupt bool
	StartedAt   time.Time
	Terminal    taskTerminal
	// OutputChannel 是本任务的输出通道（来源通道的稳定副本）。
	//
	// 这是本任务通道的**唯一**来源：内核不持有"当前通道"可变状态（N0 已删除
	// Agent.currentOutputChannel）。那类字段会被后来的任务覆盖，而被打断任务
	// 恢复时不重新 prepare（resumeTask 只 rebase 前缀），于是两任务串台——
	// 被打断任务的回复发到中断任务的通道上（见
	// TestPreempt_ResumeKeepsOwnOutputChannel）。
	OutputChannel string

	// PrefixLen 是 stepPrepare 构建的**基础前缀**长度（system + timeline + 用户输入）。
	// 恢复时用它把「本任务自己的现场」接回重建后的前缀之上（见 rebaseFramePrefix）。
	PrefixLen int
	// InputBlocks 是本轮输入携带的多模态块；重建前缀时要重新挂回。
	InputBlocks []agentAPI.ContentBlock
}

// outputChannelOf 从**输入事件**推导本次输出应走的通道。
//
// 内核不持有"当前通道"可变状态：那类字段会被后来的任务（中断任务）覆盖，
// 使被打断任务恢复后的提示词/事件标签串台。通道只跟着事件与帧走。
func outputChannelOf(evt *agentIO.InputEvent) string {
	if evt == nil {
		return ""
	}
	if evt.OutputChannel != "" {
		return evt.OutputChannel
	}
	return evt.Source
}

// isCriticalChannel 报告某个通道是否是**整任务不可抢占**的临界区。
//
// 目前只有 `_consolidation_`（记忆整理直接改图库）。工具执行/ONNX/CAS 属于
// **单步**临界区，由"只在 step 之间检查让位"天然保护，不在这里列。
func isCriticalChannel(channel string) bool {
	return channel == channelConsolidation
}

func (a *Agent) newTaskFrame(input string, stageCtx *sdk.StageContext) *TaskFrame {
	return &TaskFrame{Input: input, StageCtx: stageCtx, Step: StepPrepare}
}

// runTaskSteps 驱动状态机直到任务结束或被抢占挂起。
//
// 这是 M1 的驱动循环，M3a 从 process() 抽出来，使调用方可以拿到
// outcomeSuspended 并把帧留给调度器保存。
func (a *Agent) runTaskSteps(f *TaskFrame) stepOutcome {
	// 步数上限只是防"转移缺失导致死循环"的护栏；正常任务远达不到。
	const maxSteps = 1 << 20
	for i := 0; i < maxSteps; i++ {
		// 安全点：只在 step 之间检查让位。临界区（StepToolExec）不在此列，
		// 因为让位信号由 interruptLoop 置位、而本循环是唯一读帧者。
		if !isCriticalChannel(f.OutputChannel) && a.sched.preemptGrantedFor() && a.sched.canSuspend() {
			return outcomeSuspended
		}
		// 工具轮次硬上限（设计文档 D6）：在发起下一轮 LLM 前收尾。
		// f.Turn 只在 stepTurnEnd 递增，所以它等于「已完成的工具批数」；
		// 因此这里允许 maxToolTurns 批，而不会多跑第 maxToolTurns+1 轮。
		if f.Step == StepLLM && a.maxToolTurns > 0 && f.Turn >= a.maxToolTurns {
			log.Printf("[agent] 已达最大工具轮次 %d（turn=%d），强制收尾", a.maxToolTurns, f.Turn)
			if f.Resp != nil && strings.TrimSpace(f.Resp.Content) != "" {
				f.Response = f.Resp.Content
			} else {
				f.Response = fmt.Sprintf("[系统] 已达到最大工具轮次 %d，任务中止。", a.maxToolTurns)
			}
			return outcomeDone
		}
		switch a.step(f) {
		case outcomeDone:
			return outcomeDone
		case outcomeFailed:
			return outcomeFailed
		case outcomeSuspended:
			return outcomeSuspended
		}
	}
	f.Err = fmt.Errorf("agent: task step budget exhausted（状态机未收敛，疑似转移缺失）")
	return outcomeFailed
}

// process 是保留给 processConsolidation 与测试的薄壳，返回与原实现相同的四元组。
//
// 注意：M3a 起**不再持 a.mu**——调度器是唯一执行者，而挂起不能持锁。
func (a *Agent) process(input string, stageCtx *sdk.StageContext) (response string, toolsUsed []string, toolResults []ToolResultItem, err error) {
	if a.provider == nil {
		return "", nil, nil, fmt.Errorf("agent: no LLM provider configured")
	}
	f := a.newTaskFrame(input, stageCtx)
	switch a.runTaskSteps(f) {
	case outcomeDone:
		return f.Response, f.ToolsUsed, f.ToolResults, nil
	case outcomeFailed:
		return "", f.ToolsUsed, f.ToolResults, f.Err
	default:
		// 不该发生：process() 不参与挂起（只有 runInputTask 会）。
		return "", f.ToolsUsed, f.ToolResults,
			fmt.Errorf("agent: task suspended outside scheduler")
	}
}

// runInputTask 是一个输入任务的完整生命周期：prepare → run → finish。
//
// 它是原 processInput 的全部职责，被拆成三段而不是一个大函数，目的只有一个：
// 让帧可以跨安全点被挂起——挂起后由调度器保存，恢复时接着 run 段继续，
// 而 finish 段（上下文提交与回执）只在任务真正结束时执行一次。
//
// M3a 还没有抢占，因此 outcomeSuspended 只会由 M3b 的抢占检查产生。
func (a *Agent) runInputTask(evt *agentIO.InputEvent) (*TaskFrame, stepOutcome) {
	// 临界区标记由调度器 goroutine 维护，任务结束（含挂起）即清。
	// interceptLoop 读它来决定“能不能取消”，因此必须是原子的。
	defer a.sched.setCritical(false)

	f, term := a.prepareInputTask(evt)
	switch term {
	case terminalSkipped, terminalStageShortCircuit, terminalConsolidation:
		return nil, outcomeDone
	}

	out := a.runTaskSteps(f)
	if out == outcomeSuspended {
		f.Terminal = terminalSuspended
		return f, outcomeSuspended
	}
	a.finishInputTask(f, out)
	return f, out
}

// rebaseFramePrefix 把被挂起任务的上下文现场「加载回中断任务之上」。
//
// 语义（用户明确）：
//   - 中断打断时，被挂起任务自到达以来累积的全部现场（含 toolcall）被保护；
//   - 中断在上一个任务之前的**完整状态**上开始运行（所以中断看不到本任务的部分进展）；
//   - 中断结束后，把被挂起任务与其现场加载回中断任务**之上**再继续——
//     即中断已提交的那段上下文留在下面（前缀），本任务自己的现场落回其上。
//
// 实现：重建基础前缀（system + timeline + 用户输入）；由于中断结束时已把它的
// 输入/输出提交进 a.context，重建出的 timeline 已含中断的效果；再把本任务
// 自己的尾部（Stage 上下文 + 工具轮产物 + 占位）原样接回。
func (a *Agent) rebaseFramePrefix(f *TaskFrame) {
	if f == nil || f.PrefixLen <= 0 || f.PrefixLen > len(f.Msgs) {
		return
	}
	tail := append([]agentAPI.Message(nil), f.Msgs[f.PrefixLen:]...)

	budget := ComputeTokenBudget(a.provider, a.systemPrompt)
	memContext := a.buildMemoryContext(f.Input, budget.MemoryTokens)
	sysPrompt := a.buildSystemPrompt(memContext, f.Input)
	prefix := a.buildMessages(sysPrompt, f.Input, a.contextTokenBudget(budget))

	// 重建会丢掉 prepare 段对尾部消息的两处改写，这里等价地补回。
	if f.IsInterrupt && len(prefix) > 0 {
		last := prefix[len(prefix)-1]
		last.Role = "system"
		last.Content = "[中断消息] " + last.Content
		prefix[len(prefix)-1] = last
	}
	if len(f.InputBlocks) > 0 && len(prefix) > 0 {
		prefix[len(prefix)-1].Blocks = f.InputBlocks
	}

	f.Msgs = append(prefix, tail...)
	f.PrefixLen = len(prefix)
}

// prepareInputTask 执行 processInput 的前半段（去重、通道解析、阶段、裁剪、
// 输入事件落上下文）。返回终态不为 terminalNone 时调用方不得进入 run 段。
func (a *Agent) prepareInputTask(evt *agentIO.InputEvent) (*TaskFrame, taskTerminal) {
	start := time.Now()

	in, ok := a.resolveInput(evt)
	if !ok {
		// 空输入（文本与媒体都空）：没有可处理内容，但同步调用方仍在等回执。
		a.emitSkippedReply(evt, "empty_input")
		return nil, terminalSkipped
	}

	// 去重按文本做：webui/GUI 断线重连会重放未确认消息。
	// 带媒体时跳过——媒体输入的 alt 文案（"[从 qq 收到了 image]"）对不同图片
	// 是同一句，拿它去重会把连发的两张图误判成重复。
	if len(in.blocks) == 0 && a.isDuplicateInput(evt.Source, in.text) {
		log.Printf("[agent] dropped duplicate input from %s: %s", evt.Source, truncateStr(in.text, 60))
		// 去重是「不处理」而不是「不回」，否则同步调用方（cli/clawhub 无超时）
		// 会永久挂起（设计文档 §7 不变量 I5、§11.3 X2/X4）。
		a.emitSkippedReply(evt, "duplicate")
		return nil, terminalSkipped
	}

	// 通道只从**输入事件**推导，内核不持有"当前通道"可变状态
	// （见 outputChannelOf；这消除了中断任务覆盖它导致被打断任务串台的整类问题）。
	// 进入本任务的临界区属性（记忆整理整任务不可抢占）。
	// 必须在 processConsolidation 之前设置——它就在下面同步执行。
	a.sched.setCritical(isCriticalChannel(outputChannelOf(evt)))

	if evt.OutputChannel == channelConsolidation {
		a.processConsolidation(evt, in.text)
		return nil, terminalConsolidation
	}

	// pendingMedia 让 describe_image / transcribe_audio / ocr_image 拿到本轮媒体的
	// 原始 data/url，也是这三个工具是否出现在工具表里的开关。仅对用户直接上传成立
	//（payload 里才有 data/url）；插件注入的是成品 block，取不到原始数据。
	if evt.Type == "image" || evt.Type == "audio" {
		a.pendingMedia = evt.Payload
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
	a.interruptInput = isInterrupt
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
		return nil, terminalStageShortCircuit
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
	rawPayload := map[string]interface{}{"content": input, "source": evt.Source}
	for _, k := range []string{"upload_url", "upload_type", "upload_size", "upload_name"} {
		if v, ok := evt.Payload[k]; ok {
			rawPayload[k] = v
		}
	}
	a.publishEvent(events.EventRawInput, rawPayload)

	archived := a.pruneOnInput(evt, cleanInput)
	if archived > 0 {
		log.Printf("[agent] pruned %d low-relevance events to document memory", archived)
	}

	// 本轮 inputch（处理表按它记账）+ contextfull 检测（只有驻留子设了钩子）。
	a.tableMu.Lock()
	a.currentInputch = outputChannelOf(evt)
	a.tableMu.Unlock()

	if !isInterrupt {
		a.context.Append(ContextEvent{
			Timestamp: start,
			Source:    evt.Source,
			Input:     input,
		})
	}

	f := a.newTaskFrame(input, stageCtx)
	f.Evt = evt
	f.CleanInput = cleanInput
	f.IsInterrupt = isInterrupt
	f.StartedAt = start
	// 通道记进帧：恢复时用它把 agent 级字段改回来（见 TaskFrame.OutputChannel）。
	f.OutputChannel = outputChannelOf(evt)
	return f, terminalNone
}

// finishInputTask 执行 processInput 的后半段（日志、上下文提交、回执、记忆候选）。
//
// 只在任务真正结束时调用一次——这正是不变量 I5（每任务恰一次终态）的落点。
func (a *Agent) finishInputTask(f *TaskFrame, out stepOutcome) {
	evt := f.Evt

	// pendingMedia 是「本轮」语义：任务结束即清（挂起时保留，见 runInputTask）。
	if evt != nil && (evt.Type == "image" || evt.Type == "audio") {
		a.pendingMedia = nil
	}

	if out == outcomeFailed {
		log.Printf("[agent] process %s error: %v", evt.Type, f.Err)
		resp := fmt.Sprintf("处理错误: %v", f.Err)
		a.emitResponse(evt, resp)
		a.context.Append(ContextEvent{Timestamp: time.Now(), Source: "agent", Input: f.Input, Response: resp})
		f.Terminal = terminalError
		return
	}

	// inputch 处理表：本轮**未主动写入**时由系统自动写（保证每轮必有记录）。
	// 只有驻留子会用到（根 agent 的 children 为 0 时这只是几个空操作）。
	a.autoRecordInputch(f)

	elapsed := time.Since(f.StartedAt)
	log.Printf("[agent] %s from %s → response (%dms, tools=%v)",
		evt.Type, evt.Source, elapsed.Milliseconds(), f.ToolsUsed)

	// 本轮捕获的媒体一起挂到这条事件上：用户上传的、插件注入的，以及模型调
	// multimodal_see_picture / see_video 时经 SetToolBlocks 注入的。
	turnEvt := ContextEvent{
		Timestamp:   time.Now(),
		Source:      "agent",
		Input:       f.CleanInput,
		Response:    f.Response,
		ToolsUsed:   f.ToolsUsed,
		ToolResults: f.ToolResults,
	}
	a.bindEventMedia(&turnEvt, a.drainMediaDigests())
	a.context.Append(turnEvt)

	a.emitResponse(evt, f.Response)

	if !f.StageCtx.NoMemory {
		a.emitMemoryCandidate(evt.Source, f.CleanInput, f.Response, f.ToolResults, f.ToolsUsed)
	}
	f.Terminal = terminalOK
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

	f.Msgs = a.buildMessages(sysPrompt, f.Input, a.contextTokenBudget(budget))
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
		f.InputBlocks = blocks
	}
	// 基础前缀到此为止（system + timeline + 用户输入）；其后的 Stage 上下文
	// 与工具轮产物都属于“本任务自己的现场”，恢复时要接回重建后的前缀之上。
	f.PrefixLen = len(f.Msgs)

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

	// 上下文占满检测：此刻 f.Msgs 已建好（含 system + timeline + 本轮输入）。
	// 只有驻留子设了 onContextFull ⇒ 对根 agent 是 no-op。
	a.checkContextFull(f)

	f.Step = StepLLM
	return outcomeContinue
}

// stepLLM 是轮次顶部与 LLM 调用。
//
// 取消（context.Canceled 且 agent 未退出）时**留在本 step 并 Turn++**——等价于
// 原实现的 `continue`：重新排空中断、补占位、重新请求。抢占挂起将在 M3 从这里接管。
func (a *Agent) stepLLM(f *TaskFrame) stepOutcome {
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
	resp, llmErr := a.callLLMWithFallback(req, providers, f.OutputChannel)

	if llmErr != nil {
		if errors.Is(llmErr, context.Canceled) && a.ctx.Err() == nil {
			if f.OutputChannel == channelConsolidation {
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
			"channel": f.OutputChannel,
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
			"channel": f.OutputChannel,
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
	result := a.executeToolCall(f.CurTool, f.OutputChannel)
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
		"channel": f.OutputChannel,
	})

	f.ToolIdx++
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
func (a *Agent) callLLMWithFallback(req *agentAPI.CompletionRequest, providers []agentAPI.Provider, channel string) (*agentAPI.CompletionResponse, error) {
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

			resp, llmErr = chatStreamWithFallback(fCtx, fbProvider, req, a, channel)

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
