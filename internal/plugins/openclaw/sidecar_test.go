package openclaw

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLaunchSidecarNoMainJS(t *testing.T) {
	tmpDir := t.TempDir()
	sp, err := launchSidecar(tmpDir, "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sp != nil {
		t.Fatal("expected nil for dir without main.js")
	}
}

func TestLaunchSidecarAndListTools(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join("testdata", "echoplugin", "main.js")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read test plugin: %v", err)
	}
	dst := filepath.Join(tmpDir, "main.js")
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatalf("write test plugin: %v", err)
	}

	sp, err := launchSidecar(tmpDir, "echoplugin")
	if err != nil {
		t.Fatalf("launch sidecar: %v", err)
	}
	defer sp.Close()

	tools, err := sp.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	if len(tools) == 0 {
		t.Fatal("expected at least one tool")
	}

	found := false
	for _, tool := range tools {
		if tool.Name == "echo" {
			found = true
			if tool.Description == "" {
				t.Error("expected non-empty description for echo tool")
			}
		}
	}
	if !found {
		t.Fatal("expected 'echo' tool in list")
	}
	t.Logf("tools: %+v", tools)
}

func TestCallEchoTool(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join("testdata", "echoplugin", "main.js")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read test plugin: %v", err)
	}
	dst := filepath.Join(tmpDir, "main.js")
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatalf("write test plugin: %v", err)
	}

	sp, err := launchSidecar(tmpDir, "echoplugin")
	if err != nil {
		t.Fatalf("launch sidecar: %v", err)
	}
	defer sp.Close()

	result, err := sp.CallTool("echo", map[string]interface{}{
		"text": "hello world",
	})
	if err != nil {
		t.Fatalf("call echo tool: %v", err)
	}

	expected := "Echo: hello world"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
	t.Logf("echo result: %s", result)
}

func TestCallAddTool(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join("testdata", "echoplugin", "main.js")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read test plugin: %v", err)
	}
	dst := filepath.Join(tmpDir, "main.js")
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatalf("write test plugin: %v", err)
	}

	sp, err := launchSidecar(tmpDir, "echoplugin")
	if err != nil {
		t.Fatalf("launch sidecar: %v", err)
	}
	defer sp.Close()

	result, err := sp.CallTool("add", map[string]interface{}{
		"a": 3.0,
		"b": 4.0,
	})
	if err != nil {
		t.Fatalf("call add tool: %v", err)
	}

	expected := "7"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
	t.Logf("add result: %s", result)
}

func TestCallNonexistentTool(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join("testdata", "echoplugin", "main.js")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read test plugin: %v", err)
	}
	dst := filepath.Join(tmpDir, "main.js")
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatalf("write test plugin: %v", err)
	}

	sp, err := launchSidecar(tmpDir, "echoplugin")
	if err != nil {
		t.Fatalf("launch sidecar: %v", err)
	}
	defer sp.Close()

	_, err = sp.CallTool("nonexistent", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent tool")
	}
	t.Logf("expected error: %v", err)
}

func TestConcurrentCalls(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join("testdata", "echoplugin", "main.js")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read test plugin: %v", err)
	}
	dst := filepath.Join(tmpDir, "main.js")
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatalf("write test plugin: %v", err)
	}

	sp, err := launchSidecar(tmpDir, "echoplugin")
	if err != nil {
		t.Fatalf("launch sidecar: %v", err)
	}
	defer sp.Close()

	done := make(chan bool, 5)
	for i := 0; i < 5; i++ {
		go func(n int) {
			result, err := sp.CallTool("add", map[string]interface{}{
				"a": float64(n),
				"b": float64(n * 2),
			})
			if err != nil {
				t.Errorf("concurrent call %d: %v", n, err)
			}
			if result != fmt.Sprintf("%d", n + n*2) {
				t.Errorf("call %d: expected %d, got %s", n, n + n*2, result)
			}
			done <- true
		}(i)
	}
	for i := 0; i < 5; i++ {
		<-done
	}
}
