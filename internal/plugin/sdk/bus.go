package sdk

import "fmt"

type EventBus interface {
	Publish(event *Event)
	Subscribe(eventType EventType, handler EventHandler) func()
}

type InProcessBus struct {
	subs map[EventType][]EventHandler
}

func NewInProcessBus() *InProcessBus {
	return &InProcessBus{
		subs: make(map[EventType][]EventHandler),
	}
}

func (b *InProcessBus) Publish(evt *Event) {
	for _, h := range b.subs[EventAll] {
		h(evt)
	}
	if evt.Type != EventAll {
		for _, h := range b.subs[evt.Type] {
			h(evt)
		}
	}
}

func (b *InProcessBus) Subscribe(eventType EventType, handler EventHandler) func() {
	b.subs[eventType] = append(b.subs[eventType], handler)
	return func() {
		list := b.subs[eventType]
		for i, h := range list {
			if fmt.Sprintf("%p", h) == fmt.Sprintf("%p", handler) {
				b.subs[eventType] = append(list[:i], list[i+1:]...)
				break
			}
		}
	}
}
