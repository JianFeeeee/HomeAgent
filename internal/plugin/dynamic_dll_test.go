//go:build windows

package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTryLoadDLL_NoFile(t *testing.T) {
	dir := t.TempDir()
	plg, err := tryLoadDLL(dir, "nonexistent", nil)
	if err != nil {
		t.Fatalf("tryLoadDLL on empty dir should not error: %v", err)
	}
	if plg != nil {
		t.Fatal("expected nil for non-existent plugin.dll")
	}
}

func TestTryLoadDLL_Invalid(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "plugin.dll"), []byte("not a real dll"), 0644)

	plg, err := tryLoadDLL(dir, "baddll", nil)
	t.Logf("plg=%v err=%v", plg, err)

	if err == nil && plg == nil {
		t.Fatal("expected error or non-nil plugin for existing file")
	}
}
