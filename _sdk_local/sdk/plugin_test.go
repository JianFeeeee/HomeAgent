package sdk

import (
	"testing"
)

func TestRegisterStageGlobalDefault(t *testing.T) {
	called := false
	regStage := func(stage Stage, handler StageHandler) {
		called = true
	}
	s := &PluginSDK{regStage: regStage, name: "test"}
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error { return nil })

	if !called {
		t.Error("global scope: handler not registered")
	}
}

func TestRegisterStageGlobalExplicit(t *testing.T) {
	called := false
	regStage := func(stage Stage, handler StageHandler) {
		called = true
	}
	s := &PluginSDK{regStage: regStage, name: "test"}
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error { return nil }, StageScopeGlobal)

	if !called {
		t.Error("global scope: handler not registered")
	}
}

func TestRegisterStageOwnToolsMatch(t *testing.T) {
	var registered StageHandler
	regStage := func(stage Stage, handler StageHandler) {
		registered = handler
	}
	s := &PluginSDK{regStage: regStage, name: "myplugin"}
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error { return nil }, StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler not registered")
	}

	ctx := &StageContext{}
	ctx.ToolCalls = []ToolCall{{Plugin: "myplugin", Name: "my_tool"}}
	ctx.ToolResults = nil

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestRegisterStageOwnToolsSkipOtherPlugin(t *testing.T) {
	var registered StageHandler
	regStage := func(stage Stage, handler StageHandler) {
		registered = handler
	}
	s := &PluginSDK{regStage: regStage, name: "myplugin"}

	callCount := 0
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error {
		callCount++
		return nil
	}, StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler not registered")
	}

	ctx := &StageContext{}
	ctx.ToolCalls = []ToolCall{{Plugin: "other", Name: "other_tool"}}

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if callCount != 0 {
		t.Error("handler should not be called for other plugin's tool")
	}
}

func TestRegisterStageOwnToolsNonToolcallDegrades(t *testing.T) {
	regStage := func(stage Stage, handler StageHandler) {
		if stage != StagePreAction {
			t.Errorf("expected StagePreAction, got %s", stage)
		}
	}
	s := &PluginSDK{regStage: regStage, name: "test"}
	s.RegisterStage(StagePreAction, func(ctx *StageContext) error { return nil }, StageScopeOwnTools)
}

func TestRegisterStageOwnToolsStageBeforeToolcallNoToolCalls(t *testing.T) {
	var registered StageHandler
	regStage := func(stage Stage, handler StageHandler) {
		registered = handler
	}
	s := &PluginSDK{regStage: regStage, name: "myplugin"}

	callCount := 0
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error {
		callCount++
		return nil
	}, StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler not registered")
	}

	ctx := &StageContext{}

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if callCount != 0 {
		t.Error("handler should not be called when ToolCalls is empty")
	}
}

func TestRegisterStageOwnToolsStageAfterToolcallMatch(t *testing.T) {
	var registered StageHandler
	regStage := func(stage Stage, handler StageHandler) {
		registered = handler
	}
	s := &PluginSDK{regStage: regStage, name: "myplugin"}

	callCount := 0
	s.RegisterStage(StageAfterToolcall, func(ctx *StageContext) error {
		callCount++
		return nil
	}, StageScopeOwnTools)

	if registered == nil {
		t.Fatal("handler not registered")
	}

	ctx := &StageContext{}
	ctx.ToolResults = []ToolResult{{Plugin: "myplugin", Name: "my_tool"}}

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if callCount != 1 {
		t.Error("handler should be called for own plugin's tool result")
	}
}

func TestRegisterStageOwnToolsStageAfterToolcallSkip(t *testing.T) {
	var registered StageHandler
	regStage := func(stage Stage, handler StageHandler) {
		registered = handler
	}
	s := &PluginSDK{regStage: regStage, name: "myplugin"}

	callCount := 0
	s.RegisterStage(StageAfterToolcall, func(ctx *StageContext) error {
		callCount++
		return nil
	}, StageScopeOwnTools)

	ctx := &StageContext{}
	ctx.ToolResults = []ToolResult{{Plugin: "other", Name: "other_tool"}}

	err := registered(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if callCount != 0 {
		t.Error("handler should not be called for other plugin's tool result")
	}
}

func TestRegisterStageOwnToolsNilRegStage(t *testing.T) {
	s := &PluginSDK{name: "test"}
	s.RegisterStage(StageBeforeToolcall, func(ctx *StageContext) error { return nil }, StageScopeOwnTools)
}

func TestToolDefNoMemory(t *testing.T) {
	def := ToolDef{
		Name:     "test_tool",
		NoMemory: true,
	}
	if !def.NoMemory {
		t.Error("NoMemory should be true")
	}
	def2 := ToolDef{Name: "normal_tool"}
	if def2.NoMemory {
		t.Error("default NoMemory should be false")
	}
}

func TestRegisterTextCleaner(t *testing.T) {
	s := &PluginSDK{name: "test"}

	c1 := func(text string) string { return text + "_c1" }
	c2 := func(text string) string { return text + "_c2" }

	s.RegisterTextCleaner(c1)
	s.RegisterTextCleaner(c2)

	cleaners := s.TextCleaners()
	if len(cleaners) != 2 {
		t.Fatalf("expected 2 cleaners, got %d", len(cleaners))
	}

	got := cleaners[0]("hello")
	if got != "hello_c1" {
		t.Errorf("expected hello_c1, got %s", got)
	}

	got = cleaners[1]("hello")
	if got != "hello_c2" {
		t.Errorf("expected hello_c2, got %s", got)
	}
}

func TestTextCleanersEmpty(t *testing.T) {
	s := New("test", nil, nil, nil, nil, nil)
	cleaners := s.TextCleaners()
	if cleaners == nil {
		t.Error("TextCleaners should return empty slice, not nil")
	}
	if len(cleaners) != 0 {
		t.Errorf("expected 0 cleaners, got %d", len(cleaners))
	}
}
