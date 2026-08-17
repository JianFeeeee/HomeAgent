package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func TestExtractDescription(t *testing.T) {
	content := "# Plugin\n\nThis is a test plugin.\nversion: 1.0.0"
	desc := extractDescription(content)
	if desc != "This is a test plugin." {
		t.Errorf("expected 'This is a test plugin.', got %q", desc)
	}
}

func TestExtractField(t *testing.T) {
	content := "version: 1.0.0\nauthor: test\nio_type: io"
	if v := extractField(content, "version"); v != "1.0.0" {
		t.Errorf("expected '1.0.0', got %q", v)
	}
	if v := extractField(content, "author"); v != "test" {
		t.Errorf("expected 'test', got %q", v)
	}
	if v := extractField(content, "io_type"); v != "io" {
		t.Errorf("expected 'io', got %q", v)
	}
}

func TestExtractFieldCaseInsensitive(t *testing.T) {
	content := "Version: 2.0.0"
	if v := extractField(content, "version"); v != "2.0.0" {
		t.Errorf("expected '2.0.0', got %q", v)
	}
}

func TestExtractFieldMissing(t *testing.T) {
	if v := extractField("no fields here", "version"); v != "" {
		t.Errorf("expected '', got %q", v)
	}
}

func TestExtractIOConfigFull(t *testing.T) {
	content := `# QQ Plugin
io_type: io
io_input_route: qq
io_output_route: qq
io_output_caps: text,file,image`

	cfg := extractIOConfig(content)
	if cfg == nil {
		t.Fatal("expected IOConfig")
	}
	if cfg.Type != "io" {
		t.Errorf("expected type 'io', got %q", cfg.Type)
	}
	if cfg.InputRoute != "qq" {
		t.Errorf("expected input_route 'qq', got %q", cfg.InputRoute)
	}
	if cfg.OutputRoute != "qq" {
		t.Errorf("expected output_route 'qq', got %q", cfg.OutputRoute)
	}
	if len(cfg.OutputCaps) != 3 || cfg.OutputCaps[0] != "text" {
		t.Errorf("expected caps [text file image], got %v", cfg.OutputCaps)
	}
}

func TestExtractIOConfigMinimal(t *testing.T) {
	content := `# Plugin
io_type: input`
	cfg := extractIOConfig(content)
	if cfg == nil {
		t.Fatal("expected IOConfig")
	}
	if cfg.Type != "input" {
		t.Errorf("expected 'input', got %q", cfg.Type)
	}
	if cfg.InputRoute != "" {
		t.Errorf("expected empty input_route, got %q", cfg.InputRoute)
	}
}

func TestExtractIOConfigNil(t *testing.T) {
	cfg := extractIOConfig("# No IO config here")
	if cfg != nil {
		t.Errorf("expected nil, got %+v", cfg)
	}
}

func TestExtractToolDefsBasic(t *testing.T) {
	content := `# Plugin
description

## hello_tool
Say hello to someone
- name: The person to greet

## add_numbers
Add two numbers together
- a: First number
- b: Second number`

	defs := extractToolDefs(content)
	if len(defs) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(defs))
	}

	if defs[0].Name != "hello_tool" {
		t.Errorf("expected 'hello_tool', got %q", defs[0].Name)
	}
	if defs[0].Description != "Say hello to someone" {
		t.Errorf("expected 'Say hello to someone', got %q", defs[0].Description)
	}

	props := defs[0].Parameters["properties"].(map[string]interface{})
	if _, ok := props["name"]; !ok {
		t.Errorf("expected 'name' parameter")
	}
	p := props["name"].(map[string]interface{})
	if p["description"] != "The person to greet" {
		t.Errorf("expected desc 'The person to greet', got %q", p["description"])
	}
}

func TestExtractToolDefsToolWithColon(t *testing.T) {
	content := `# Plugin

### Tool: my_tool
Do something
- param: Description`

	defs := extractToolDefs(content)
	if len(defs) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(defs))
	}
	if defs[0].Name != "my_tool" {
		t.Errorf("expected 'my_tool', got %q", defs[0].Name)
	}
}

func TestExtractToolDefsSkipsNonToolSections(t *testing.T) {
	content := `# Plugin

## Usage
This is how to use the plugin

## Examples
Some examples here

## real_tool
This is an actual tool
- param: value`

	defs := extractToolDefs(content)
	if len(defs) != 1 {
		t.Fatalf("expected 1 tool (non-tool sections skipped), got %d", len(defs))
	}
	if defs[0].Name != "real_tool" {
		t.Errorf("expected 'real_tool', got %q", defs[0].Name)
	}
}

func TestExtractToolDefsEmpty(t *testing.T) {
	defs := extractToolDefs("# Just a title\nNo tools here")
	if len(defs) != 0 {
		t.Errorf("expected 0 tools, got %d", len(defs))
	}
}

func TestExtractToolDefsCodeBlock(t *testing.T) {
	content := "# Plugin\n\n## my_tool\nA tool\n- param: desc\n\n```\n## not_a_tool\nThis is inside a code block\n```\n\n## another_tool\nAnother one\n- x: y"

	defs := extractToolDefs(content)
	if len(defs) != 2 {
		t.Fatalf("expected 2 tools (code block skipped), got %d", len(defs))
	}
	if defs[0].Name != "my_tool" || defs[1].Name != "another_tool" {
		t.Errorf("unexpected tool names: %v", defs)
	}
}

func TestExtractToolDefsNoParams(t *testing.T) {
	content := `# Plugin

## simple_tool
A tool with no parameters`

	defs := extractToolDefs(content)
	if len(defs) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(defs))
	}
	if defs[0].Name != "simple_tool" {
		t.Errorf("expected 'simple_tool', got %q", defs[0].Name)
	}
	props := defs[0].Parameters["properties"].(map[string]interface{})
	if len(props) != 0 {
		t.Errorf("expected no params, got %d", len(props))
	}
}

func TestPluginManagerInterfaceReloadOne(t *testing.T) {
	// 编译期契约：Registry 必须实现 PluginManager（含 ReloadOne 单插件重载）。
	var _ sdk.PluginManager = (*Registry)(nil)
}

func TestRegistryIncrementalReload(t *testing.T) {
	dir := t.TempDir()
	plgDir := filepath.Join(dir, "plugins")
	os.MkdirAll(plgDir, 0755)

	// 一个 Lua 插件
	luaDir := filepath.Join(plgDir, "reloaddemo")
	os.MkdirAll(luaDir, 0755)
	os.WriteFile(filepath.Join(luaDir, "plugin.json"), []byte(`{"name":"reloaddemo","entry":"main.lua"}`), 0644)
	writeLua := func(body string) {
		os.WriteFile(filepath.Join(luaDir, "main.lua"), []byte(`local plugin = { name = "reloaddemo" }
function plugin.start(sdk) sdk.log("info", "`+body+`") end
function plugin.stop() end
return plugin
`), 0644)
	}
	writeLua("v1")

	reg := NewRegistry()
	reg.SetPluginDir(plgDir)
	reg.SetConfigRegistry(internalConfig.NewConfigRegistry(""))


	// 首次 Reload：应加载 1 个
	msg, err := reg.Reload(plgDir)
	if err != nil {
		t.Fatalf("first reload: %v", err)
	}
	if len(reg.List()) != 1 {
		t.Fatalf("first reload loaded=%d, want 1 (%s)", len(reg.List()), msg)
	}

	// 无变更再 Reload：不应重载（0 changed, 1 unchanged）
	msg, err = reg.Reload(plgDir)
	if err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if !strings.Contains(msg, "0 plugins") || !strings.Contains(msg, "1 unchanged") {
		t.Errorf("unchanged reload should skip: %q", msg)
	}

	// 修改 main.lua → 应重载该插件
	writeLua("v2")
	msg, err = reg.Reload(plgDir)
	if err != nil {
		t.Fatalf("changed reload: %v", err)
	}
	if !strings.Contains(msg, "1 plugins") {
		t.Errorf("changed reload should reload 1: %q", msg)
	}
}
