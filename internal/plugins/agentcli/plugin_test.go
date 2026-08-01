package agentcli

import (
	"encoding/json"
	"testing"
	"time"

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
	p := New("agentcli")
	tc := newToolCapture()
	sdk := sdk.New("agentcli", sdk.SDKConfig{
		RegTool:  tc.RegisterTool,
		RegStage: tc.RegisterStage,
		RegAPI:   tc.RegisterAPI,
		Settings: sdk.NewSettings("agentcli", nil),
	})
	if err := p.Start(sdk); err != nil {
		return nil, nil, err
	}
	return p, tc, nil
}

func TestKeyMapping(t *testing.T) {
	tests := []struct {
		key      string
		expected []byte
	}{
		{"enter", []byte{0x0D}},
		{"tab", []byte{0x09}},
		{"escape", []byte{0x1B}},
		{"esc", []byte{0x1B}},
		{"backspace", []byte{0x7F}},
		{"delete", []byte{0x1B, 0x5B, 0x33, 0x7E}},
		{"home", []byte{0x1B, 0x5B, 0x48}},
		{"end", []byte{0x1B, 0x5B, 0x46}},
		{"up", []byte{0x1B, 0x5B, 0x41}},
		{"down", []byte{0x1B, 0x5B, 0x42}},
		{"left", []byte{0x1B, 0x5B, 0x44}},
		{"right", []byte{0x1B, 0x5B, 0x43}},
		{"page_up", []byte{0x1B, 0x5B, 0x35, 0x7E}},
		{"page_down", []byte{0x1B, 0x5B, 0x36, 0x7E}},
		{"ctrl_a", []byte{0x01}},
		{"ctrl_z", []byte{0x1A}},
		{"alt_a", []byte{0x1B, 'a'}},
		{"alt_z", []byte{0x1B, 'z'}},
		{"f1", []byte{0x1B, 0x5B, 0x50}},
		{"f4", []byte{0x1B, 0x5B, 0x53}},
		{"f12", []byte{0x1B, 0x5B, 0x32, 0x34, 0x7E}},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			result, err := mapKey(tt.key)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytesEqual(result, tt.expected) {
				t.Fatalf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestKeyMappingInvalid(t *testing.T) {
	_, err := mapKey("unknown_key")
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListEmpty(t *testing.T) {
	p, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	handler := tc.handlers["terminal_list"]
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
	if resp["count"].(float64) != 0 {
		t.Fatalf("expected count 0, got %v", resp["count"])
	}
}

func TestCreateAndCloseTerminal(t *testing.T) {
	p, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	createHandler := tc.handlers["terminal_create"]
	result, err := createHandler(map[string]interface{}{
		"command": "echo hello",
		"timeout": "10s",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var createResp map[string]interface{}
	json.Unmarshal(data, &createResp)

	if v, ok := createResp["error"]; ok {
		t.Skipf("PTY not available in this environment: %v", v)
	}
	if createResp["status"] != "created" {
		t.Fatalf("expected status created, got %v", createResp["status"])
	}

	id := createResp["id"].(string)
	if id == "" {
		t.Fatal("expected non-empty terminal id")
	}

	// Give the terminal a moment to output
	time.Sleep(200 * time.Millisecond)

	// Read output
	readHandler := tc.handlers["terminal_read"]
	result, err = readHandler(map[string]interface{}{
		"id":    id,
		"clear": true,
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ = json.Marshal(result)
	var readResp map[string]interface{}
	json.Unmarshal(data, &readResp)
	if readResp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", readResp["status"])
	}

	// Close
	closeHandler := tc.handlers["terminal_close"]
	result, err = closeHandler(map[string]interface{}{
		"id": id,
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ = json.Marshal(result)
	var closeResp map[string]interface{}
	json.Unmarshal(data, &closeResp)
	if closeResp["status"] != "closed" {
		t.Fatalf("expected status closed, got %v", closeResp["status"])
	}
}

func TestCreateTerminalMissingArgs(t *testing.T) {
	p, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	handler := tc.handlers["terminal_create"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if v, ok := resp["error"]; ok {
		t.Skipf("PTY not available in this environment: %v", v)
	}
	if resp["status"] != "created" {
		t.Fatalf("expected status created, got %v", resp["status"])
	}
	id := resp["id"].(string)
	p.handleClose(map[string]interface{}{"id": id})
}

func TestWriteToNonexistentTerminal(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["terminal_write"]
	result, err := handler(map[string]interface{}{
		"id":    "nonexistent",
		"input": "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error for nonexistent terminal")
	}
}

func TestReadNonexistentTerminal(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["terminal_read"]
	result, err := handler(map[string]interface{}{
		"id": "nonexistent",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error for nonexistent terminal")
	}
}

func TestResizeNonexistentTerminal(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["terminal_resize"]
	result, err := handler(map[string]interface{}{
		"id":   "nonexistent",
		"rows": float64(40),
		"cols": float64(120),
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error for nonexistent terminal")
	}
}

func TestCloseNonexistentTerminal(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["terminal_close"]
	result, err := handler(map[string]interface{}{
		"id": "nonexistent",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error for nonexistent terminal")
	}
}

func TestTerminalWriteRequiresId(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["terminal_write"]
	result, err := handler(map[string]interface{}{
		"input": "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error when id is missing")
	}
}

func TestTerminalWriteRequiresContent(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["terminal_write"]
	result, err := handler(map[string]interface{}{
		"id": "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error when both input and key are missing")
	}
}

func TestToolsRegistered(t *testing.T) {
	p, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	expectedTools := []string{
		"terminal_create",
		"terminal_write",
		"terminal_read",
		"terminal_resize",
		"terminal_close",
		"terminal_list",
	}

	for _, name := range expectedTools {
		if _, ok := tc.handlers[name]; !ok {
			t.Errorf("tool %s not registered", name)
		}
	}
}
