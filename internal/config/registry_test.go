package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryBasic(t *testing.T) {
	r := NewConfigRegistry("")
	r.Register("core.llm.model", "deepseek-v4-flash")
	r.Register("plugin.qq.access_token", "abc123")

	val, err := r.Get("core.llm.model")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if v, ok := val.(string); !ok || v != "deepseek-v4-flash" {
		t.Fatalf("expected deepseek-v4-flash, got %v", val)
	}

	keys := r.List("core")
	if len(keys) != 1 || keys[0] != "core.llm.model" {
		t.Fatalf("expected [core.llm.model], got %v", keys)
	}

	r.Set("core.llm.model", "gpt-4")
	val, _ = r.Get("core.llm.model")
	if v, _ := val.(string); v != "gpt-4" {
		t.Fatalf("expected gpt-4, got %v", val)
	}
}

func TestRegistryPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	r := NewConfigRegistry(path)
	r.Register("core.log_level", "debug")
	r.Set("plugin.test.key", 42)
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	r2 := NewConfigRegistry(path)
	val, err := r2.Get("plugin.test.key")
	if err != nil {
		t.Fatalf("Get after reload: %v", err)
	}
	if v, _ := val.(float64); v != 42 {
		t.Fatalf("expected 42, got %v", val)
	}
}

func TestRegistryDelete(t *testing.T) {
	r := NewConfigRegistry("")
	r.Register("a.b", "1")
	r.Register("a.c", "2")
	r.Delete("a.b")
	keys := r.List("a")
	if len(keys) != 1 || keys[0] != "a.c" {
		t.Fatalf("expected [a.c], got %v", keys)
	}
}

func TestRegistryDump(t *testing.T) {
	r := NewConfigRegistry("")
	r.Register("x", 1)
	r.Register("y", "two")
	dump := r.Dump()
	if len(dump) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(dump))
	}
}

func TestRegistryUnknownKey(t *testing.T) {
	r := NewConfigRegistry("")
	_, err := r.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestRegistryFlush(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	r := NewConfigRegistry(path)
	r.Set("k", "v")
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	data, _ := os.ReadFile(path)
	if len(data) == 0 {
		t.Fatal("expected persisted data")
	}
}

func TestRegistryFlushIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	r := NewConfigRegistry(path)
	r.Set("k", "v")
	r.Flush()
	r.Flush() // second flush should not error
}
