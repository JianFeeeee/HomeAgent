package sdk

import (
	"testing"
)

func TestInProcessBus(t *testing.T) {
	bus := NewInProcessBus()
	var called bool

	bus.Subscribe(EventRawInput, func(evt *Event) {
		called = true
		if evt.Source != "test" {
			t.Errorf("expected source test, got %s", evt.Source)
		}
	})

	bus.Publish(&Event{
		Type:   EventRawInput,
		Source: "test",
	})

	if !called {
		t.Error("handler was not called")
	}
}

func TestInProcessBusWildcard(t *testing.T) {
	bus := NewInProcessBus()
	count := 0

	bus.Subscribe(EventAll, func(evt *Event) {
		count++
	})

	bus.Publish(&Event{Type: EventRawInput, Source: "s1"})
	bus.Publish(&Event{Type: EventToolCall, Source: "s2"})

	if count != 2 {
		t.Errorf("expected 2, got %d", count)
	}
}

func TestPluginAPI(t *testing.T) {
	bus := NewInProcessBus()
	api := NewPluginAPI("test", "1.0.0", bus, nil, nil)

	if api.Name != "test" {
		t.Errorf("expected test, got %s", api.Name)
	}
	if api.Version != "1.0.0" {
		t.Errorf("expected 1.0.0, got %s", api.Version)
	}

	var stageCalled bool
	api.RegisterStage(StageOnInput, func(ctx *StageContext) error {
		stageCalled = true
		if ctx.RawMessage != "hello" {
			t.Errorf("expected hello, got %s", ctx.RawMessage)
		}
		return nil
	})

	ctx := &StageContext{RawMessage: "hello"}
	for _, handler := range api.StageHandlers(StageOnInput) {
		handler(ctx)
	}

	if !stageCalled {
		t.Error("stage handler was not called")
	}
}

func TestPluginAPITool(t *testing.T) {
	api := NewPluginAPI("test", "1.0.0", nil, nil, nil)

	err := api.RegisterTool("test_tool", func(args map[string]interface{}) (interface{}, error) {
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("register tool: %v", err)
	}

	if _, ok := api.Tools()["test_tool"]; !ok {
		t.Error("tool not found")
	}

	// duplicate registration should fail
	err = api.RegisterTool("test_tool", func(args map[string]interface{}) (interface{}, error) {
		return "ok", nil
	})
	if err == nil {
		t.Error("expected error on duplicate tool registration")
	}
}

func TestPluginAPIStageShortCircuit(t *testing.T) {
	api := NewPluginAPI("test", "1.0.0", nil, nil, nil)

	api.RegisterStage(StageOnInput, func(ctx *StageContext) error {
		resp := "intercepted"
		ctx.Response = &resp
		return nil
	})

	ctx := &StageContext{RawMessage: "hello"}
	handlers := api.StageHandlers(StageOnInput)
	if len(handlers) != 1 {
		t.Fatalf("expected 1 handler, got %d", len(handlers))
	}
	handlers[0](ctx)

	if ctx.Response == nil || *ctx.Response != "intercepted" {
		t.Errorf("expected intercepted, got %v", ctx.Response)
	}
}

func TestAllStages(t *testing.T) {
	stages := AllStages()
	expected := []Stage{
		StageOnInput, StagePreAction, StagePostAction,
		StageBeforeToolcall, StageAfterToolcall,
		StageBeforeOutput, StageAfterOutput,
	}
	for _, s := range expected {
		if !stages[s] {
			t.Errorf("missing stage: %s", s)
		}
	}
	if len(stages) != len(expected) {
		t.Errorf("expected %d stages, got %d", len(expected), len(stages))
	}
}
