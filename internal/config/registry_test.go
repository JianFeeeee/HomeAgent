package config

import (
	"path/filepath"
	"testing"
	"time"
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
	path := filepath.Join(dir, "config.db")

	r := NewConfigRegistry(path)
	r.Register("core.log_level", "debug")
	r.Set("plugin.test.key", "42")
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	r.Close()

	r2 := NewConfigRegistry(path)
	val, err := r2.Get("plugin.test.key")
	if err != nil {
		t.Fatalf("Get after reload: %v", err)
	}
	if v, _ := val.(string); v != "42" {
		t.Fatalf("expected 42, got %v", val)
	}
	r2.Close()
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
	r.Register("x", "1")
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
	path := filepath.Join(dir, "config.db")
	r := NewConfigRegistry(path)
	r.Set("k", "v")
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	r.Close()

	// Reopen and verify persistence
	r2 := NewConfigRegistry(path)
	val, err := r2.Get("k")
	if err != nil {
		t.Fatalf("Get after flush: %v", err)
	}
	if v, _ := val.(string); v != "v" {
		t.Fatalf("expected v, got %v", val)
	}
	r2.Close()
}

func TestRegistryFlushIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")
	r := NewConfigRegistry(path)
	r.Set("k", "v")
	r.Flush()
	r.Flush() // second flush should not error
	r.Close()
}

func TestPluginConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")
	r := NewConfigRegistry(path)

	ps := r.PluginConfig("test_deepseek")
	ps.RegisterDef(ConfigDef{Key: "api_key", Default: "sk-test123"})
	if err := ps.Set("api_key", "sk-test123"); err != nil {
		t.Fatalf("PluginSettings.Set: %v", err)
	}

	val, err := ps.Get("api_key")
	if err != nil {
		t.Fatalf("PluginSettings.Get: %v", err)
	}
	if v, _ := val.(string); v != "sk-test123" {
		t.Fatalf("expected sk-test123, got %v", val)
	}

	keys, err := ps.List("")
	if err != nil {
		t.Fatalf("PluginSettings.List: %v", err)
	}
	if len(keys) != 1 || keys[0] != "api_key" {
		t.Fatalf("expected [api_key], got %v", keys)
	}

	// Core table should not contain plugin data
	coreKeys := r.List("")
	for _, k := range coreKeys {
		if k == "api_key" {
			t.Fatal("plugin key leaked into core config table")
		}
	}

	r.Close()
}

func TestSeedDefaultsToConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")

	r := NewConfigRegistry(path)
	r.SeedDefaults(dir)

	// Verify DB was seeded with expected number of keys
	keys := r.List("")
	if len(keys) == 0 {
		t.Fatal("SeedDefaults produced empty DB")
	}

	// Reconstruct config from DB
	cfg2 := r.ToConfig()

	if v := r.GetString("webui.listen_addr", ""); v != ":8080" {
		t.Fatalf("expected :8080, got %s", v)
	}
	if cfg2.LLM.Provider != "deepseek" {
		t.Fatalf("expected deepseek, got %s", cfg2.LLM.Provider)
	}
	if len(cfg2.LLM.Sources) == 0 {
		t.Fatal("expected at least 1 LLM source")
	}

	// Second SeedDefaults should be no-op (DB already has data)
	r.SeedDefaults(dir)
	if len(r.List("")) != len(keys) {
		t.Fatal("second SeedDefaults changed DB count")
	}

	r.Close()
}

func TestGetHelpers(t *testing.T) {
	r := NewConfigRegistry("")
	r.Set("str_key", "hello")
	r.Set("int_key", "42")
	r.Set("dur_key", "5m")
	r.Set("bool_key", "true")

	if got := r.GetString("str_key", ""); got != "hello" {
		t.Fatalf("GetString: expected hello, got %s", got)
	}
	if got := r.GetString("nonexistent", "fallback"); got != "fallback" {
		t.Fatalf("GetString fallback: expected fallback, got %s", got)
	}
	if got := r.GetInt("int_key", 0); got != 42 {
		t.Fatalf("GetInt: expected 42, got %d", got)
	}
	if got := r.GetInt("nonexistent", 99); got != 99 {
		t.Fatalf("GetInt fallback: expected 99, got %d", got)
	}
	if got := r.GetDuration("dur_key", 0); got != 5*time.Minute {
		t.Fatalf("GetDuration: expected 5m, got %v", got)
	}
	if got := r.GetDuration("nonexistent", 30*time.Second); got != 30*time.Second {
		t.Fatalf("GetDuration fallback: expected 30s, got %v", got)
	}
	if got := r.GetBool("bool_key", false); got != true {
		t.Fatalf("GetBool: expected true, got %v", got)
	}
	if got := r.GetBool("nonexistent", true); got != true {
		t.Fatalf("GetBool fallback: expected true, got %v", got)
	}
}
