package events

import (
	"fmt"
	"sync"
)

type EventType string

const (
	EventRawInput    EventType = "raw_input"
	EventAgentOutput EventType = "agent_output"
	EventToolCall    EventType = "tool_call"
	EventReasoning   EventType = "reasoning"
	EventSystem      EventType = "system"
	EventAll         EventType = "*"
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
		h(evt)
	}
	for _, h := range typeHandlers {
		h(evt)
	}
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
