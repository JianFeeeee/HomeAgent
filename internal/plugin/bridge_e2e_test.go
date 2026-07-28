//go:build windows

package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func TestBridgeE2E_WebPlugin(t *testing.T) {
	exeDir, _ := os.Executable()
	// Find web example build relative to the homeagent repo root
	haRoot := findHomeAgentRoot(t, exeDir)
	dllPath := filepath.Join(haRoot, "..", "homeagentsdk", "example", "web", "build", "plugin.dll")
	if _, err := os.Stat(dllPath); os.IsNotExist(err) {
		t.Fatalf("web plugin DLL not found at %s\nRun: cd example/web && plugindev build --target windows/amd64", dllPath)
	}

	// Track captured tools and stages
	var capturedTools []sdk.ToolDef
	var capturedStages []sdk.Stage

	regTool := func(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
		capturedTools = append(capturedTools, def)
		t.Logf("  registered tool: %s", name)
		return nil
	}
	regStage := func(stage sdk.Stage, handler sdk.StageHandler) {
		capturedStages = append(capturedStages, stage)
		t.Logf("  registered stage: %s", stage)
	}
	regAPI := func(name string) error {
		t.Logf("  registered API: %s", name)
		return nil
	}

	sett := sdk.NewSettings("web", nil)
	psdk := sdk.New("web", sdk.SDKConfig{Settings: sett, RegTool: regTool, RegStage: regStage, RegAPI: regAPI})

	plg, err := newDLLPlugin(dllPath, "web", nil)
	if err != nil {
		t.Fatalf("newDLLPlugin failed: %v", err)
	}
	defer plg.Stop()

	// Start — this calls NewPlugin + StartPlugin + registerTools + registerStages
	if err := plg.Start(psdk); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Verify tools were captured
	if len(capturedTools) == 0 {
		t.Fatal("no tools were registered by web plugin")
	}
	t.Logf("Captured %d tools:", len(capturedTools))
	for _, d := range capturedTools {
		t.Logf("  - %s: %s", d.Name, d.Description[:min(len(d.Description), 60)])
	}

	// Check specific expected tools
	webSearch, webFetch := false, false
	for _, d := range capturedTools {
		if d.Name == "web_search" {
			webSearch = true
			if d.Description == "" {
				t.Error("web_search has empty description")
			}
			params := d.Parameters
			if params == nil {
				t.Error("web_search has nil parameters")
			} else {
				if _, ok := params["properties"]; !ok {
					t.Error("web_search parameters missing 'properties'")
				}
			}
		}
		if d.Name == "web_fetch" {
			webFetch = true
		}
	}
	if !webSearch {
		t.Error("expected tool 'web_search' not registered")
	}
	if !webFetch {
		t.Error("expected tool 'web_fetch' not registered")
	}

	// Verify bridge exports work via direct C ABI calls
	t.Logf("Bridge exports: getTools=%x invokeTool=%x freeCStr=%x",
		plg.getTools, plg.invokeTool, plg.freeCStr)

	// GetToolDefsJSON
	if plg.getTools != 0 {
		toolDefsJSON := callGetToolDefsJSON(t, plg)
		if len(toolDefsJSON) == 0 {
			t.Error("GetToolDefsJSON returned empty array, expected tools")
		}
		for _, d := range toolDefsJSON {
			t.Logf("  bridge tool: %s", d["name"])
		}
	}

	// InvokeToolJSON — test with the search tool
	if plg.invokeTool != 0 {
		result := callInvokeToolJSON(t, plg, "web_search", map[string]interface{}{
			"query": "test",
			"count": 1,
		})
		t.Logf("InvokeToolJSON result keys: %v", keysOfMap(result))
		// Should get a result map (might be error if no network, but should not crash)
		if errStr, ok := result["error"]; ok {
			t.Logf("  (expected — tool returned error: %v)", errStr)
		}
	}
}

func TestBridgeE2E_SanitizerStages(t *testing.T) {
	exeDir, _ := os.Executable()
	haRoot := findHomeAgentRoot(t, exeDir)
	dllPath := filepath.Join(haRoot, "..", "homeagentsdk", "example", "sanitizer", "build", "plugin.dll")
	if _, err := os.Stat(dllPath); os.IsNotExist(err) {
		t.Skip("sanitizer DLL not built")
	}

	var capturedStages []sdk.Stage
	regStage := func(stage sdk.Stage, handler sdk.StageHandler) {
		capturedStages = append(capturedStages, stage)
		t.Logf("  registered stage: %s", stage)
	}

	sett := sdk.NewSettings("sanitizer", nil)
	psdk := sdk.New("sanitizer", sdk.SDKConfig{Settings: sett,
		RegTool: func(name string, def sdk.ToolDef, handler sdk.ToolHandler) error { return nil },
		RegStage: regStage,
		RegAPI:   func(name string) error { return nil },
	})

	plg, err := newDLLPlugin(dllPath, "sanitizer", nil)
	if err != nil {
		t.Fatalf("newDLLPlugin failed: %v", err)
	}
	defer plg.Stop()

	if err := plg.Start(psdk); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if len(capturedStages) == 0 {
		t.Fatal("no stages registered by sanitizer")
	}
	found := false
	for _, s := range capturedStages {
		if s == sdk.StagePostAction {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected post_action stage, got %v", capturedStages)
	}

	// Verify bridge GetStagesJSON
	if plg.getStages != 0 {
		ret, _, _ := syscall.SyscallN(plg.getStages, plg.handle)
		if ret != 0 {
			stagesJSON := cStringPtrToString(ret)
			if plg.freeCStr != 0 {
				syscall.SyscallN(plg.freeCStr, ret)
			}
			t.Logf("GetStagesJSON: %s", stagesJSON)
			if !contains(t, stagesJSON, "post_action") {
				t.Error("GetStagesJSON missing post_action")
			}
		}
	}
}

// --- helpers ---

func findHomeAgentRoot(t *testing.T, exeDir string) string {
	t.Helper()
	// Walk up from test binary directory looking for homeagent/
	dir := exeDir
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "internal", "plugin")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("cannot find homeagent root")
	return ""
}

func callGetToolDefsJSON(t *testing.T, plg *dllPlugin) []map[string]interface{} {
	t.Helper()
	ret, _, _ := syscall.SyscallN(plg.getTools, plg.handle)
	if ret == 0 {
		t.Fatal("GetToolDefsJSON returned nil")
	}
	jsonStr := cStringPtrToString(ret)
	if plg.freeCStr != 0 {
		syscall.SyscallN(plg.freeCStr, ret)
	}
	var defs []map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &defs); err != nil {
		t.Fatalf("GetToolDefsJSON parse error: %v", err)
	}
	return defs
}

func callInvokeToolJSON(t *testing.T, plg *dllPlugin, toolName string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	argsJSON, _ := json.Marshal(args)
	cToolName := append([]byte(toolName), 0)
	cArgs := append(argsJSON, 0)

	ret, _, _ := syscall.SyscallN(
		plg.invokeTool,
		plg.handle,
		uintptr(unsafe.Pointer(&cToolName[0])),
		uintptr(unsafe.Pointer(&cArgs[0])),
	)
	if ret == 0 {
		t.Fatal("InvokeToolJSON returned nil")
	}
	jsonStr := cStringPtrToString(ret)
	if plg.freeCStr != 0 {
		syscall.SyscallN(plg.freeCStr, ret)
	}
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		t.Fatalf("InvokeToolJSON parse error: %v (json=%s)", err, jsonStr)
	}
	return result
}

func keysOfMap(m map[string]interface{}) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func contains(t *testing.T, s, substr string) bool {
	t.Helper()
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
