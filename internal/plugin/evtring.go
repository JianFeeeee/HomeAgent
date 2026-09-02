package plugin

import (
	"encoding/json"
	"sync"

	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin/proc"
	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// EventRing 是 Bus 与 proc.EvtRing 之间的适配层。
//
// 把内核的事件总线接到共享内存事件环：Bus.Publish → handler
// 把事件序列化写入 EvtRing slot → eventfd 通知子进程。
// 不改 Bus 自身结构（保护零 API 变动）。
type EventRing struct {
	ring *proc.EvtRing
	bus  *events.Bus
	efd  int
	mu   sync.Mutex
}

func NewEventRing(ring *proc.EvtRing, efd int, bus *events.Bus) *EventRing {
	return &EventRing{ring: ring, bus: bus, efd: efd}
}

// Subscribe 在 Bus 上注册一个把事件分发到事件环的 handler，返回取消函数。
//
// 不改 Bus 自身结构——handler 把事件序列化后写入环并 post eventfd，
// Bus 侧按 EventType 精确匹配分发（与现有逻辑完全一致）。
func (er *EventRing) Subscribe(eventType pubsdk.EventType) func() {
	return er.bus.Subscribe(events.EventType(eventType), func(evt *events.Event) {
		payload, err := json.Marshal(evt)
		if err != nil {
			return
		}
		er.ring.WritePush(pubsdk.EventType(evt.Type), payload)
		proc.EvtfdNotify(er.efd)
	})
}

// EvtRingSubscribe 实现 proc.EvtRingSubscriber 接口。
// 按事件类型列表订阅，返回统一取消函数。
func (er *EventRing) EvtRingSubscribe(types []pubsdk.EventType) func() {
	unsubscribes := make([]func(), 0, len(types))
	for _, t := range types {
		unsubscribes = append(unsubscribes, er.Subscribe(t))
	}
	return func() {
		for _, fn := range unsubscribes {
			fn()
		}
	}
}
