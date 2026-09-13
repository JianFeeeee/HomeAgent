package core

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
)

// childTaskState 是一个子任务的生命周期状态。
//
// delivered 代替了早期的“读到即删”：完成通知会写进持久上下文
// （formatMergedTimeline 每轮重新注入），模型之后还会再查。读一次就删的
// 话，第二次查询返回“不存在或已过期”——那是一个**永远不会成功的可操作
// 信号**，模型只能一遍遍地重试/汇报，循环永不结束。
type childTaskState struct {
	running   bool
	result    string
	delivered bool  // 结果是否已交付过（用于幂等应答）
	seq       int64 // 完成顺序，用于有界淘汰
}

// maxRetainedChildTasks 是保留的已完成子任务上限（防结果无限占用内存）。
const maxRetainedChildTasks = 20

// evictChildTasksLocked 淘汰最旧的已完成子任务。调用方必须持有 childMu。
func (a *Agent) evictChildTasksLocked() {
	for len(a.childTasks) > maxRetainedChildTasks {
		oldestID := ""
		var oldestSeq int64
		for id, st := range a.childTasks {
			if st.running {
				continue
			}
			if oldestID == "" || st.seq < oldestSeq {
				oldestID, oldestSeq = id, st.seq
			}
		}
		if oldestID == "" {
			return // 剩下全是运行中的，不淘汰
		}
		delete(a.childTasks, oldestID)
	}
}

func (a *Agent) executeSpawnChild(tc agentAPI.ToolCall, parentChannel string) string {
	task, _ := tc.Arguments["task"].(string)
	if task == "" {
		if b, _ := json.Marshal(tc.Arguments); len(b) > 2 {
			log.Printf("[spawn] task empty but arguments present: %s", truncateStr(string(b), 300))
		}
		return "请提供 task 参数"
	}
	maxTurns := 0
	if v, ok := tc.Arguments["max_turns"].(float64); ok {
		maxTurns = int(v)
	}
	if maxTurns < 1 {
		maxTurns = defaultChildMaxTurns
	}
	if maxTurns > 30 {
		maxTurns = 30
	}

	a.childMu.Lock()
	a.childNextID++
	taskID := fmt.Sprintf("child_%d", a.childNextID)
	a.childMu.Unlock()

	// parentChannel 由调用方（任务帧）传入：子任务完成通知要回到**发起这次
	// spawn 的那个任务**的通道，而不是"内核当前通道"（那个概念已删除）。
	if parentChannel == "" || parentChannel == channelConsolidation {
		parentChannel = "cli"
	}

	a.childMu.Lock()
	a.childTasks[taskID] = &childTaskState{running: true}
	a.childMu.Unlock()
	go a.runChildTask(taskID, task, parentChannel, maxTurns)

	return fmt.Sprintf("子任务已启动（ID: %s，最多 %d 轮）。完成后会自动通知你，届时用 child_result 查看输出即可（**只需查询一次**）", taskID, maxTurns)
}

// defaultChildMaxTurns 子 Agent 默认工具轮数（可被 spawn_child 的 max_turns 参数覆盖）。
const defaultChildMaxTurns = 5

func (a *Agent) runChildTask(taskID, task string, parentChannel string, maxTurns int) {
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

	allTools := a.buildToolDefs()
	childTools := make([]interface{}, 0, len(allTools))
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
		if strings.HasPrefix(name, "output_send__") || name == "output_list_channels" || name == "spawn_child" || name == "plgreload" {
			continue
		}
		childTools = append(childTools, t)
	}

	var finalResult string
	for turn := 0; turn < maxTurns; turn++ {
		req := &agentAPI.CompletionRequest{
			Messages:        msgs,
			MaxTokens:       4096,
			Tools:           childTools,
			ToolChoice:      "auto",
			DisableThinking: !a.thinkingEnabled,
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
			case strings.HasPrefix(ct.Name, "output_send__") || ct.Name == "output_list_channels":
				result = fmt.Sprintf("子 Agent 不允许调用输出工具: %s", ct.Name)
			case ct.Name == "spawn_child" || ct.Name == "plgreload":
				result = fmt.Sprintf("子 Agent 不允许调用系统工具: %s", ct.Name)
			default:
				result = a.executeToolCall(ct, parentChannel)
			}
			msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: resp.Content, ToolCalls: []agentAPI.ToolCall{ct}})
			msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: ct.ID, Content: result})
		}
	}

	if finalResult == "" {
		finalResult = "子 Agent 执行超时（超过 5 轮）"
	}

	a.childMu.Lock()
	if st := a.childTasks[taskID]; st != nil {
		st.running = false
		st.result = finalResult
		a.childSeq++
		st.seq = a.childSeq
	}
	a.evictChildTasksLocked()
	a.childMu.Unlock()

	log.Printf("[child] %s done: %s", taskID, truncateStr(finalResult, 100))

	notification := fmt.Sprintf("子任务 %s 已完成。请用 child_result 工具查看输出（只需查询一次；重复查询不会返回失败）。", taskID)
	a.injectSelfChannel(selfInputMsg{
		text:    notification,
		channel: parentChannel, // 回到父对话通道，正常处理（写入上下文 + emit 响应）
	})
}

// executeChildResultTool 取回子任务结果。
//
// **幂等**：结果不会被“读到即删”，重复查询返回同一结果或一条明确提示。
// 这一点至关重要——完成通知会长期留在持久上下文里（formatMergedTimeline
// 每轮重新注入），如果重复查询返回“不存在”这种失败信号，模型会认定任务
// 未完成而无限重试（实测单轮 35 次工具调用、持续 514 秒）。
func (a *Agent) executeChildResultTool(tc agentAPI.ToolCall) string {
	taskID, _ := tc.Arguments["task_id"].(string)
	if taskID == "" {
		return "请提供 task_id 参数"
	}

	a.childMu.Lock()
	st, ok := a.childTasks[taskID]
	if !ok {
		a.childMu.Unlock()
		return fmt.Sprintf("子任务 %s 不存在：从未创建该 ID（请核对 spawn_child 返回的 ID 拼写）", taskID)
	}
	if st.running {
		a.childMu.Unlock()
		return fmt.Sprintf("子任务 %s 仍在运行中，尚未完成。请等待完成通知后再查询。", taskID)
	}
	first := !st.delivered
	st.delivered = true
	result := st.result
	a.childMu.Unlock()

	if first {
		return fmt.Sprintf("【子任务 %s 结果】\n%s", taskID, result)
	}
	// 重复查询不是失败：明确告诉模型“任务已完成、结果已给过”，让它停止重试。
	return fmt.Sprintf("【子任务 %s 已完成】结果已在上文提供（见先前的 child_result 工具结果），无需重复查询；请直接基于上文结果继续。", taskID)
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
		a.provider = a.providerManager.Get(name)
		return fmt.Sprintf("已切换到 LLM 源: %s", name)

	default:
		return fmt.Sprintf("未知的 LLM 工具: %s", tc.Name)
	}
}
