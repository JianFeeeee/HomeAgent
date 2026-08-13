package core

import (
	"fmt"
	"log"
	"runtime/debug"
	"time"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func (a *Agent) runStage(stage sdk.Stage, ctx *sdk.StageContext) bool {
	payload := map[string]interface{}{
		"phase":   string(stage),
		"channel": a.currentOutputChannel,
	}
	if ctx != nil && len(ctx.ToolCalls) > 0 {
		payload["tool"] = ctx.ToolCalls[0].Name
	}
	a.publishEvent(events.EventStage, payload)
	if a.stageHost == nil {
		return false
	}
	ctx.Phase = stage
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[agent] stage %q plugin panic: %v\n%s", stage, r, debug.Stack())
			}
		}()
		a.stageHost.RunStage(stage, ctx)
	}()
	return ctx.Response != nil
}

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

func (a *Agent) stageCtxFromInput(input, userID, groupID string) *sdk.StageContext {
	return &sdk.StageContext{
		RawMessage: input,
		UserID:     userID,
		GroupID:    groupID,
		Phase:      sdk.StageOnInput,
		Extra:      make(map[string]interface{}),
	}
}

func (a *Agent) injectSourceContext(stageCtx *sdk.StageContext, evt *agentIO.InputEvent) {
	if stageCtx == nil || evt == nil {
		return
	}
	source := evt.Source
	if source == "" {
		source = "unknown"
	}
	channel := evt.OutputChannel
	if channel == "" {
		channel = source
	}
	content := fmt.Sprintf("当前输入来源: %s；默认输出通道: %s。", source, channel)
	if flag, _ := evt.Payload["interrupt"].(bool); flag {
		content = fmt.Sprintf("这是一条打断输入。来源: %s；默认输出通道: %s。", source, channel)
	}
	stageCtx.ContextMsgs = append(stageCtx.ContextMsgs, map[string]interface{}{
		"role":    "system",
		"content": content,
	})
}
