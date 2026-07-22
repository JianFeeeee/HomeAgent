package core

import (
	"fmt"
	"log"
	"strings"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
)

func (a *Agent) executeSpawnChild(tc agentAPI.ToolCall) string {
	task, _ := tc.Arguments["task"].(string)
	if task == "" {
		return "请提供 task 参数"
	}

	a.childMu.Lock()
	a.childNextID++
	taskID := fmt.Sprintf("child_%d", a.childNextID)
	a.childMu.Unlock()

	go a.runChildTask(taskID, task)

	return fmt.Sprintf("子任务已启动（ID: %s），完成后会自动通知你，届时请使用 child_result 工具查看输出", taskID)
}

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
			case strings.HasPrefix(ct.Name, "output_send__") || ct.Name == "output_list_channels":
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

	a.childMu.Lock()
	a.childResults[taskID] = finalResult
	a.childMu.Unlock()

	log.Printf("[child] %s done: %s", taskID, truncateStr(finalResult, 100))

	notification := fmt.Sprintf("子任务 %s 已完成，请调用 child_result 工具查看输出", taskID)
	select {
	case a.selfInputCh <- notification:
	default:
		log.Printf("[child] self input channel full, dropping notification for %s", taskID)
	}
}

func (a *Agent) executeChildResultTool(tc agentAPI.ToolCall) string {
	taskID, _ := tc.Arguments["task_id"].(string)
	if taskID == "" {
		return "请提供 task_id 参数"
	}

	a.childMu.Lock()
	result, ok := a.childResults[taskID]
	if !ok {
		a.childMu.Unlock()

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
		a.provider = a.providerManager.Get(name)
		return fmt.Sprintf("已切换到 LLM 源: %s", name)

	default:
		return fmt.Sprintf("未知的 LLM 工具: %s", tc.Name)
	}
}


