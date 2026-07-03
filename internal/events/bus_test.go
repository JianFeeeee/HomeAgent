package events

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestBusPublishSubscribe(t *testing.T) {
	bus := NewBus()
	var count int32

	bus.Subscribe(EventRawInput, func(evt *Event) {
		atomic.AddInt32(&count, 1)
	})

	bus.Publish(&Event{
		Type:    EventRawInput,
		Source:  "test",
		Payload: map[string]interface{}{"content": "hello"},
	})

	if c := atomic.LoadInt32(&count); c != 1 {
		t.Errorf("expected 1, got %d", c)
	}
}

func TestBusWildcard(t *testing.T) {
	bus := NewBus()
	var count int32

	bus.Subscribe(EventAll, func(evt *Event) {
		atomic.AddInt32(&count, 1)
	})

	bus.Publish(&Event{Type: EventRawInput, Source: "test"})
	bus.Publish(&Event{Type: EventToolCall, Source: "test"})

	if c := atomic.LoadInt32(&count); c != 2 {
		t.Errorf("expected 2, got %d", c)
	}
}

func TestBusUnsubscribe(t *testing.T) {
	bus := NewBus()
	var count int32

	handler := func(evt *Event) {
		atomic.AddInt32(&count, 1)
	}
	unsub := bus.Subscribe(EventRawInput, handler)

	bus.Publish(&Event{Type: EventRawInput, Source: "test"})
	unsub()
	bus.Publish(&Event{Type: EventRawInput, Source: "test"})

	if c := atomic.LoadInt32(&count); c != 1 {
		t.Errorf("expected 1 after unsub, got %d", c)
	}
}

func TestBusNoMatch(t *testing.T) {
	bus := NewBus()
	var count int32

	bus.Subscribe(EventRawInput, func(evt *Event) {
		atomic.AddInt32(&count, 1)
	})

	bus.Publish(&Event{Type: EventAgentOutput, Source: "test"})

	if c := atomic.LoadInt32(&count); c != 0 {
		t.Errorf("expected 0, got %d", c)
	}
}

func TestBusConcurrent(t *testing.T) {
	bus := NewBus()
	var count int32

	bus.Subscribe(EventAll, func(evt *Event) {
		atomic.AddInt32(&count, 1)
	})

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			bus.Publish(&Event{Type: EventRawInput, Source: "test"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}

	if c := atomic.LoadInt32(&count); c != 100 {
		t.Errorf("expected 100, got %d", c)
	}
}
