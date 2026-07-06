// Package plugintest provides a test harness for external HomeAgent plugins.
//
// Usage:
//
//	import "gitcode.com/JianFeeeee/homeagent-sdk/hack/plugin-dev/testharness"
//
//	func TestMyPlugin(t *testing.T) {
//	    h := testharness.New(t, "./path/to/plugin.so")
//	    defer h.Close()
//
//	    result, err := h.CallTool("myplugin_my_tool", map[string]interface{}{
//	        "input": "hello",
//	    })
//	    if err != nil {
//	        t.Fatal(err)
//	    }
//	    t.Logf("result: %v", result)
//	}
package plugintest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"plugin"
	"strings"
	"sync"
	"testing"

	"gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// Harness is a test harness for loading and testing external Go plugins.
type Harness struct {
	t       *testing.T
	plug    sdk.Plugin
	sdk     *sdk.PluginSDK
	mu      sync.Mutex
	tools   map[string]sdk.ToolHandler
	stages  map[sdk.Stage][]sdk.StageHandler
	setting *mockSettings
}

// New loads a plugin .so and starts it with a mock SDK.
// soPath is the path to the compiled plugin.so file.
func New(t *testing.T, soPath string) *Harness {
	t.Helper()

	absPath, err := filepath.Abs(soPath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	if _, err := os.Stat(absPath); err != nil {
		t.Fatalf("plugin not found: %s", absPath)
	}

	pkg, err := plugin.Open(absPath)
	if err != nil {
		t.Fatalf("plugin.Open: %v", err)
	}

	sym, err := pkg.Lookup("NewPlugin")
	if err != nil {
		t.Fatalf("NewPlugin symbol not found: %v", err)
	}
	newPlugin, ok := sym.(func(name string, config map[string]interface{}) (sdk.Plugin, error))
	if !ok {
		t.Fatal("NewPlugin has wrong signature")
	}

	name := filepath.Base(filepath.Dir(absPath))
	plug, err := newPlugin(name, nil)
	if err != nil {
		t.Fatalf("NewPlugin: %v", err)
	}

	h := &Harness{
		t:      t,
		plug:   plug,
		tools:  make(map[string]sdk.ToolHandler),
		stages: make(map[sdk.Stage][]sdk.StageHandler),
		setting: &mockSettings{
			data: make(map[string]interface{}),
			defs: make(map[string]sdk.ConfigDef),
		},
	}

	h.sdk = sdk.New(name, h.setting, h.regTool, h.regStage, nil)

	if err := plug.Start(h.sdk); err != nil {
		t.Fatalf("plugin.Start: %v", err)
	}

	return h
}

func (h *Harness) regTool(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tools[name] = handler
	return nil
}

func (h *Harness) regStage(stage sdk.Stage, handler sdk.StageHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stages[stage] = append(h.stages[stage], handler)
}

// Plug returns the loaded plugin instance.
func (h *Harness) Plug() sdk.Plugin { return h.plug }

// SDK returns the mock PluginSDK.
func (h *Harness) SDK() *sdk.PluginSDK { return h.sdk }

// Settings returns the mock settings store for test assertions.
func (h *Harness) Settings() *mockSettings { return h.setting }

// ToolNames returns all registered tool names.
func (h *Harness) ToolNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	names := make([]string, 0, len(h.tools))
	for n := range h.tools {
		names = append(names, n)
	}
	return names
}

// CallTool invokes a registered tool handler with the given arguments.
func (h *Harness) CallTool(name string, args map[string]interface{}) (interface{}, error) {
	h.mu.Lock()
	handler, ok := h.tools[name]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("tool %q not registered", name)
	}
	return handler(args)
}

// Close stops the plugin.
func (h *Harness) Close() {
	if err := h.plug.Stop(); err != nil {
		h.t.Logf("plugin.Stop: %v", err)
	}
}

// AssertToolRegistered fails if the tool is not registered.
func (h *Harness) AssertToolRegistered(name string) {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.tools[name]; !ok {
		h.t.Fatalf("expected tool %q to be registered", name)
	}
}

// AssertToolResult checks that calling a tool returns the expected JSON output.
func (h *Harness) AssertToolResult(name string, args map[string]interface{}, expected map[string]interface{}) {
	h.t.Helper()
	got, err := h.CallTool(name, args)
	if err != nil {
		h.t.Fatalf("tool %q: %v", name, err)
	}
	gotJSON, _ := json.Marshal(got)
	expJSON, _ := json.Marshal(expected)
	if string(gotJSON) != string(expJSON) {
		h.t.Fatalf("tool %q:\ngot:  %s\nexp:  %s", name, gotJSON, expJSON)
	}
}

// mockSettings implements sdk.SettingsAPI for testing.
type mockSettings struct {
	mu   sync.Mutex
	data map[string]interface{}
	defs map[string]sdk.ConfigDef
}

func (m *mockSettings) Get(key string) (interface{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("key %q not found", key)
	}
	return v, nil
}

func (m *mockSettings) Set(key string, value interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func (m *mockSettings) List(prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.data {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (m *mockSettings) RegisterDef(def sdk.ConfigDef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defs[def.Key] = def
}

func (m *mockSettings) Defs(prefix string) []*sdk.ConfigDef {
	m.mu.Lock()
	defer m.mu.Unlock()
	var defs []*sdk.ConfigDef
	for _, d := range m.defs {
		if prefix == "" || strings.HasPrefix(d.Key, prefix) {
			defs = append(defs, &d)
		}
	}
	return defs
}

func (m *mockSettings) Dump() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]interface{})
	for k, v := range m.data {
		cp[k] = v
	}
	return cp
}

func (m *mockSettings) Plugins() []string { return nil }
