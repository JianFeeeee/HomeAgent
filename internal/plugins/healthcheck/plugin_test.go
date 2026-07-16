package healthcheck

import (
	"encoding/json"
	"os"
	"testing"

	agentCore "gitcode.com/JianFeeeee/HomeAgent/internal/agent/core"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	doc "gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

type toolCapture struct {
	handlers map[string]sdk.ToolHandler
	defs     map[string]sdk.ToolDef
}

func newToolCapture() *toolCapture {
	return &toolCapture{
		handlers: make(map[string]sdk.ToolHandler),
		defs:     make(map[string]sdk.ToolDef),
	}
}

func (tc *toolCapture) RegisterTool(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
	tc.handlers[name] = handler
	tc.defs[name] = def
	return nil
}
func (tc *toolCapture) RegisterStage(stage sdk.Stage, handler sdk.StageHandler) {}
func (tc *toolCapture) RegisterAPI(name string) error                          { return nil }

func setupPlugin() (*Plugin, *toolCapture, error) {
	sh := agentCore.NewStageHost()
	iom := agentIO.NewIOManager()
	pr := plugin.NewRegistry()

	Configure(sh, iom, pr, nil, nil, nil, nil, nil)
	p := New("healthcheck")
	tc := newToolCapture()
	sdk := sdk.New("healthcheck", sdk.SDKConfig{RegTool: tc.RegisterTool, RegStage: tc.RegisterStage, RegAPI: tc.RegisterAPI})
	if err := p.Start(sdk); err != nil {
		return nil, nil, err
	}
	return p, tc, nil
}

func TestToolsRegistered(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	expected := []string{
		"healthcheck",
		"healthcheck_plugins",
		"healthcheck_tools",
		"healthcheck_memory",
		"healthcheck_report",
		"healthcheck_perf",
	}
	for _, name := range expected {
		if _, ok := tc.handlers[name]; !ok {
			t.Errorf("tool %q not registered", name)
		}
	}
}

func TestHealthcheckFull(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}

	checks := resp["checks"].([]interface{})
	if len(checks) == 0 {
		t.Fatal("expected at least some checks")
	}

	for _, c := range checks {
		cr := c.(map[string]interface{})
		name := cr["name"].(string)
		pass := cr["pass"].(bool)
		if !pass && cr["status"] != "skip" {
			t.Errorf("check %q failed: %v (detail: %v)", name, cr["status"], cr["detail"])
		}
	}
}

func TestHealthcheckPlugins(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck_plugins"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
}

func TestHealthcheckToolsList(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck_tools"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
}

func TestHealthcheckMemoryNotAvailable(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck_memory"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	// Memory is nil in this setup, so it should skip gracefully
	if _, ok := resp["pass"]; ok {
		pass := resp["pass"].(bool)
		if !pass {
			t.Fatalf("expected pass=true when memory is nil, got false: %v", resp)
		}
	}
}

func TestHealthcheckWithMemory(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hc_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	memDB, err := memory.NewGraphDB(tmpDir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer memDB.Close()

	sh := agentCore.NewStageHost()
	iom := agentIO.NewIOManager()
	pr := plugin.NewRegistry()

	Configure(sh, iom, pr, memDB, nil, nil, nil, nil)
	p := New("healthcheck")
	tc := newToolCapture()
	sdk := sdk.New("healthcheck", sdk.SDKConfig{RegTool: tc.RegisterTool, RegStage: tc.RegisterStage, RegAPI: tc.RegisterAPI})
	if err := p.Start(sdk); err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck_memory"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
	if pass, ok := resp["pass"].(bool); !ok || !pass {
		t.Fatalf("expected pass=true, got pass=%v status=%v detail=%v", pass, resp["status"], resp["detail"])
	}
}

func TestHealthcheckWithKnowledge(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hc_know_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ks := knowledge.NewStore(tmpDir)
	if err := ks.Start(); err != nil {
		t.Fatal(err)
	}
	defer ks.Stop()

	sh := agentCore.NewStageHost()
	iom := agentIO.NewIOManager()
	pr := plugin.NewRegistry()

	Configure(sh, iom, pr, nil, ks, nil, nil, nil)
	p := New("healthcheck")
	tc := newToolCapture()
	sdk := sdk.New("healthcheck", sdk.SDKConfig{RegTool: tc.RegisterTool, RegStage: tc.RegisterStage, RegAPI: tc.RegisterAPI})
	if err := p.Start(sdk); err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}

	checks := resp["checks"].([]interface{})
	var knowledgeCheck map[string]interface{}
	for _, c := range checks {
		cr := c.(map[string]interface{})
		if cr["name"] == "knowledge" {
			knowledgeCheck = cr
			break
		}
	}

	if knowledgeCheck == nil {
		t.Fatal("expected knowledge check in results")
	}
}

func TestHealthcheckWithDocStore(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hc_doc_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ds := doc.NewStore(tmpDir)
	if err := ds.Start(); err != nil {
		t.Fatal(err)
	}
	defer ds.Stop()

	sh := agentCore.NewStageHost()
	iom := agentIO.NewIOManager()
	pr := plugin.NewRegistry()

	Configure(sh, iom, pr, nil, nil, ds, nil, nil)
	p := New("healthcheck")
	tc := newToolCapture()
	sdk := sdk.New("healthcheck", sdk.SDKConfig{RegTool: tc.RegisterTool, RegStage: tc.RegisterStage, RegAPI: tc.RegisterAPI})
	if err := p.Start(sdk); err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}

	checks := resp["checks"].([]interface{})
	var docCheck map[string]interface{}
	for _, c := range checks {
		cr := c.(map[string]interface{})
		if cr["name"] == "documents" {
			docCheck = cr
			break
		}
	}

	if docCheck == nil {
		t.Fatal("expected documents check in results")
	}
}

func TestLLMReportCollection(t *testing.T) {
	p := &Plugin{name: "healthcheck"}
	if len(p.reports) != 0 {
		t.Fatal("expected empty reports")
	}
	p.mu.Lock()
	p.reports = append(p.reports, llmReport{ToolName: "test_tool", Status: "ok", Detail: "test passed"})
	count := len(p.reports)
	p.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected 1 report, got %d", count)
	}
	if p.reports[0].ToolName != "test_tool" {
		t.Fatalf("expected tool_name=test_tool, got %s", p.reports[0].ToolName)
	}
}

func TestConfigureNilStageHost(t *testing.T) {
	Configure(nil, nil, nil, nil, nil, nil, nil, nil)
	if hcStageHost != nil {
		t.Fatal("expected hcStageHost to be nil")
	}
}


