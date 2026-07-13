package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTryLoadLua_Basic(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{
		"name": "testlua",
		"name_zh": "测试Lua",
		"name_en": "Test Lua",
		"version": "1.0.0",
		"entry": "main.lua"
	}`), 0644)

	os.WriteFile(filepath.Join(dir, "main.lua"), []byte(`
local plugin = {
  name = "testlua"
}

function plugin.start(sdk)
  sdk.log("info", "testlua started")
  sdk.register_tool("testlua_hello", {
    description = "Hello tool",
    parameters = {type = "object", properties = {}}
  }, function(args)
    return {content = "hello from lua"}
  end)
end

function plugin.stop()
  sdk.log("info", "testlua stopped")
end

return plugin
`), 0644)

	plg, err := tryLoadLua(dir, "testlua", nil)
	if err != nil {
		t.Fatalf("tryLoadLua failed: %v", err)
	}
	if plg == nil {
		t.Fatal("tryLoadLua returned nil")
	}
	if plg.Name() != "testlua" {
		t.Fatalf("unexpected name: %s", plg.Name())
	}
	t.Logf("plugin loaded: %s", plg.Name())
}

func TestTryLoadLua_NoFile(t *testing.T) {
	dir := t.TempDir()
	plg, err := tryLoadLua(dir, "nonexistent", nil)
	if err != nil {
		t.Fatalf("tryLoadLua on empty dir should not error: %v", err)
	}
	if plg != nil {
		t.Fatal("expected nil for non-existent main.lua")
	}
}

func TestTryLoadLua_NoReturnTable(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"bad","entry":"main.lua"}`), 0644)
	os.WriteFile(filepath.Join(dir, "main.lua"), []byte(`
-- just code, no return table
local x = 1
sdk.log("info", "no return table test")
`), 0644)

	plg, err := tryLoadLua(dir, "bad", nil)
	if err != nil {
		t.Fatalf("tryLoadLua failed: %v", err)
	}
	if plg == nil {
		t.Fatal("tryLoadLua returned nil")
	}
	t.Logf("loaded plugin without return table: %s", plg.Name())
}

func TestTryLoadLua_GlobalSDK(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"globalsdk","entry":"main.lua"}`), 0644)
	os.WriteFile(filepath.Join(dir, "main.lua"), []byte(`
-- sdk is a global, should work without return table
sdk.log("info", "sdk is available as global")
sdk.register_tool("direct_tool", {
  description = "registered directly in top-level code"
}, function(args)
  return {result = "ok"}
end)
`), 0644)

	plg, err := tryLoadLua(dir, "globalsdk", nil)
	if err != nil {
		t.Fatalf("tryLoadLua failed: %v", err)
	}
	if plg == nil {
		t.Fatal("tryLoadLua returned nil")
	}

	lp := plg.(*luaPlugin)
	lp.mu.Lock()
	toolCount := len(lp.tools)
	lp.mu.Unlock()
	if toolCount != 1 {
		t.Fatalf("expected 1 tool registration, got %d", toolCount)
	}
	t.Logf("tool registered during load phase: OK")
}
