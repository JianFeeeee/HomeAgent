package core

import (
	"testing"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/plugin/sdk"
)

func TestStageHostRegisterPlugin(t *testing.T) {
	host := NewStageHost()
	api := sdk.NewPluginAPI("test", "1.0.0", nil, nil, nil)

	api.RegisterTool("test_tool", func(args map[string]interface{}) (interface{}, error) {
		return "ok", nil
	})

	host.RegisterPlugin(api)

	if host.PluginCount() != 1 {
		t.Errorf("expected 1 plugin, got %d", host.PluginCount())
	}

	defs := host.GetToolDefs()
	if len(defs) != 1 {
		t.Errorf("expected 1 tool def, got %d", len(defs))
	}
	if defs[0].Name != "test_tool" {
		t.Errorf("expected test_tool, got %s", defs[0].Name)
	}
}

func TestStageHostExecuteTool(t *testing.T) {
	host := NewStageHost()
	api := sdk.NewPluginAPI("test", "1.0.0", nil, nil, nil)

	api.RegisterTool("hello", func(args map[string]interface{}) (interface{}, error) {
		return "world", nil
	})

	host.RegisterPlugin(api)

	result, err := host.ExecuteTool("hello", nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.(string) != "world" {
		t.Errorf("expected world, got %v", result)
	}

	_, err = host.ExecuteTool("nonexistent", nil)
	if err == nil {
		t.Error("expected error for nonexistent tool")
	}
}

func TestStageHostRunStage(t *testing.T) {
	host := NewStageHost()
	api := sdk.NewPluginAPI("test", "1.0.0", nil, nil, nil)

	var called bool
	api.RegisterStage(sdk.StageOnInput, func(ctx *sdk.StageContext) error {
		called = true
		return nil
	})

	host.RegisterPlugin(api)

	ctx := &sdk.StageContext{RawMessage: "hello"}
	host.RunStage(sdk.StageOnInput, ctx)

	if !called {
		t.Error("stage handler was not called")
	}
}

func TestStageHostRunStageShortCircuit(t *testing.T) {
	host := NewStageHost()

	api1 := sdk.NewPluginAPI("p1", "1.0.0", nil, nil, nil)
	api1.RegisterStage(sdk.StageOnInput, func(ctx *sdk.StageContext) error {
		resp := "short-circuited"
		ctx.Response = &resp
		return nil
	})

	var api2called bool
	api2 := sdk.NewPluginAPI("p2", "1.0.0", nil, nil, nil)
	api2.RegisterStage(sdk.StageOnInput, func(ctx *sdk.StageContext) error {
		api2called = true
		return nil
	})

	host.RegisterPlugin(api1)
	host.RegisterPlugin(api2)

	ctx := &sdk.StageContext{RawMessage: "hello"}
	host.RunStage(sdk.StageOnInput, ctx)

	if ctx.Response == nil || *ctx.Response != "short-circuited" {
		t.Errorf("expected short-circuited, got %v", ctx.Response)
	}
	if api2called {
		t.Error("api2 should not have been called after short circuit")
	}
}

func TestStageHostRunStageAll(t *testing.T) {
	host := NewStageHost()

	count := 0
	api1 := sdk.NewPluginAPI("p1", "1.0.0", nil, nil, nil)
	api1.RegisterStage(sdk.StageAfterOutput, func(ctx *sdk.StageContext) error {
		count++
		return nil
	})

	api2 := sdk.NewPluginAPI("p2", "1.0.0", nil, nil, nil)
	api2.RegisterStage(sdk.StageAfterOutput, func(ctx *sdk.StageContext) error {
		count++
		return nil
	})

	host.RegisterPlugin(api1)
	host.RegisterPlugin(api2)

	host.RunStageAll(sdk.StageAfterOutput, &sdk.StageContext{})

	if count != 2 {
		t.Errorf("expected 2 handlers called, got %d", count)
	}
}

func TestStageHostMultiplePlugins(t *testing.T) {
	host := NewStageHost()

	p1 := sdk.NewPluginAPI("p1", "1.0.0", nil, nil, nil)
	p1.RegisterTool("tool1", func(args map[string]interface{}) (interface{}, error) {
		return "from_p1", nil
	})

	p2 := sdk.NewPluginAPI("p2", "1.0.0", nil, nil, nil)
	p2.RegisterTool("tool2", func(args map[string]interface{}) (interface{}, error) {
		return "from_p2", nil
	})

	host.RegisterPlugin(p1)
	host.RegisterPlugin(p2)

	if host.PluginCount() != 2 {
		t.Errorf("expected 2 plugins, got %d", host.PluginCount())
	}

	r1, _ := host.ExecuteTool("tool1", nil)
	if r1.(string) != "from_p1" {
		t.Errorf("expected from_p1, got %v", r1)
	}

	r2, _ := host.ExecuteTool("tool2", nil)
	if r2.(string) != "from_p2" {
		t.Errorf("expected from_p2, got %v", r2)
	}
}

func TestStageHostEmpty(t *testing.T) {
	host := NewStageHost()

	if host.PluginCount() != 0 {
		t.Errorf("expected 0 plugins, got %d", host.PluginCount())
	}

	defs := host.GetToolDefs()
	if len(defs) != 0 {
		t.Errorf("expected 0 tool defs, got %d", len(defs))
	}

	_, err := host.ExecuteTool("anything", nil)
	if err == nil {
		t.Error("expected error on empty host")
	}

	// RunStage on empty host should not panic
	host.RunStage(sdk.StageOnInput, &sdk.StageContext{})
}
