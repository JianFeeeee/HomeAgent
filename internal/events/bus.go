package events

import (
	"fmt"
	"log"
	"sync"
)

type EventType string

const (
	EventRawInput      EventType = "raw_input"
	EventAgentOutput   EventType = "agent_output"
	EventAgentLLMChain EventType = "agent_llm_chain"
	EventToolCall      EventType = "tool_call"
	EventReasoning     EventType = "reasoning"
	EventStage         EventType = "stage"
	// EventScheduler 是输入调度器的状态变更事件（抢占/挂起/恢复），
	// 供状态页与诊断订阅（设计文档 §11 O2）。
	EventScheduler      EventType = "scheduler"
	EventSystem         EventType = "system"
	EventTerminalOutput EventType = "terminal_output"

	// 流式增量事件（LLM token 级）：核心改为流式后每收到一个增量块发布。
	// 订阅者可选订；不认识的旧订阅者自然忽略（Bus 按 EventType 精确匹配分发）。
	// 聚合事件 EventReasoning / EventAgentLLMChain 仍照常在每轮结束时全文发布，
	// 插件体系行为不变。
	EventReasoningDelta EventType = "reasoning_delta"
	EventContentDelta   EventType = "content_delta"

	// skill_detected：clawhubadapter（OpenClaw 兼容层）扫描 skills 目录时
	// 发现纯 SKILL 类型插件后发布，由原生 skillmgr 插件订阅并接管注册。
	// 职责链：发现者（兼容层）→ 移交事件 → 归属者（skillmgr）加载管理。
	EventSkillDetected EventType = "skill_detected"

	EventAll EventType = "*"
)

type Event struct {
	Type      EventType              `json:"type"`
	Source    string                 `json:"source"`
	Payload   map[string]interface{} `json:"payload"`
	Timestamp int64                  `json:"timestamp"`
}

type Handler func(event *Event)

type Bus struct {
	mu   sync.RWMutex
	subs map[EventType][]Handler
}

func NewBus() *Bus {
	return &Bus{
		subs: make(map[EventType][]Handler),
	}
}

func (b *Bus) Publish(evt *Event) {
	b.mu.RLock()
	allHandlers := make([]Handler, len(b.subs[EventAll]))
	copy(allHandlers, b.subs[EventAll])
	typeHandlers := make([]Handler, len(b.subs[evt.Type]))
	copy(typeHandlers, b.subs[evt.Type])
	b.mu.RUnlock()

	for _, h := range allHandlers {
		b.safeCall(h, evt)
	}
	for _, h := range typeHandlers {
		b.safeCall(h, evt)
	}
}

func (b *Bus) safeCall(h Handler, evt *Event) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[bus] handler panic: %v", r)
		}
	}()
	h(evt)
}

func (b *Bus) Subscribe(eventType EventType, handler Handler) func() {
	b.mu.Lock()
	b.subs[eventType] = append(b.subs[eventType], handler)
	b.mu.Unlock()

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		list := b.subs[eventType]
		for i, h := range list {
			if fmt.Sprintf("%p", h) == fmt.Sprintf("%p", handler) {
				b.subs[eventType] = append(list[:i], list[i+1:]...)
				break
			}
		}
	}
}
