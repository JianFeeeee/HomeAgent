package system

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsProtectedPath(t *testing.T) {
	cases := map[string]bool{
		"/etc/resolv.conf":        true,
		"/etc/hosts":              true,
		"/etc/apt/sources.list":   true,
		"/etc/environment":        true,
		"/tmp/foo.txt":            false,
		"/home/user/x":            false,
		"/usr/local/bin/homed":    false,
	}
	for p, want := range cases {
		if got := IsProtectedPath(p); got != want {
			t.Errorf("IsProtectedPath(%q) = %v, want %v", p, got, want)
		}
	}
	// 发行版/部署路径注入：显式前缀集可扩展受保护范围
	SetProtectedPaths([]string{"/opt/llm-mock", "/home/newqqagent"})
	if !IsProtectedPath("/opt/llm-mock/mock_server.py") {
		t.Error("explicit prefix /opt/llm-mock should be protected")
	}
	if !IsProtectedPath("/home/newqqagent/config.yaml") {
		t.Error("explicit prefix /home/newqqagent should be protected")
	}
	SetProtectedPaths(nil) // 恢复默认
	if IsProtectedPath("/opt/llm-mock/mock_server.py") {
		t.Error("default should not protect /opt/llm-mock")
	}
}

func TestArchiveBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	// 受保护路径
	target := "/etc/ArchiveBeforeWrite.test.tmp"
	os.WriteFile(target, []byte("original"), 0644)
	defer os.Remove(target)

	archived, err := ArchiveBeforeWrite(dir, target)
	if err != nil {
		t.Fatalf("ArchiveBeforeWrite: %v", err)
	}
	if !archived {
		t.Fatal("expected archive to happen")
	}
	dst := filepath.Join(dir, "file_baseline", strings.TrimPrefix(target, "/"))
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read archived: %v", err)
	}
	if string(data) != "original" {
		t.Fatalf("archived content = %q, want original", data)
	}

	// 幂等：同内容不重复
	archived2, err := ArchiveBeforeWrite(dir, target)
	if err != nil {
		t.Fatalf("ArchiveBeforeWrite #2: %v", err)
	}
	if archived2 {
		t.Fatal("expected idempotent archive (no repeat)")
	}

	// 非保护路径不存档
	tmp := filepath.Join(t.TempDir(), "x.txt")
	os.WriteFile(tmp, []byte("x"), 0644)
	archived3, err := ArchiveBeforeWrite(dir, tmp)
	if err != nil {
		t.Fatalf("ArchiveBeforeWrite non-protected: %v", err)
	}
	if archived3 {
		t.Fatal("non-protected path should not archive")
	}
}

func TestRestoreFileFromBaseline(t *testing.T) {
	dir := t.TempDir()
	target := "/etc/RestoreFileFromBaseline.test.tmp"
	os.WriteFile(target, []byte("v1"), 0644)
	defer os.Remove(target)

	ArchiveBeforeWrite(dir, target)
	os.WriteFile(target, []byte("v2"), 0644)

	restored, err := RestoreFileFromBaseline(dir, target)
	if err != nil {
		t.Fatalf("RestoreFileFromBaseline: %v", err)
	}
	if !restored {
		t.Fatal("expected restore to happen")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "v1" {
		t.Fatalf("restored content = %q, want v1", data)
	}
}

func TestCaptureNetworkBaselineRoundtrip(t *testing.T) {
	base, err := CaptureNetwork()
	if err != nil {
		t.Fatalf("CaptureNetwork: %v", err)
	}
	if base.CapturedAt == "" {
		t.Fatal("captured_at empty")
	}
	if base.ResolvConf == "" {
		t.Log("warning: no /etc/resolv.conf readable on this host")
	}

	dir := t.TempDir()
	if err := SaveNetworkBaseline(dir, base); err != nil {
		t.Fatalf("SaveNetworkBaseline: %v", err)
	}
	loaded, err := LoadNetworkBaseline(dir)
	if err != nil {
		t.Fatalf("LoadNetworkBaseline: %v", err)
	}
	if loaded.ResolvConf != base.ResolvConf {
		t.Fatal("resolv.conf roundtrip mismatch")
	}
	if loaded.CapturedAt != base.CapturedAt {
		t.Fatal("captured_at roundtrip mismatch")
	}
}

func TestMismatchedFiles(t *testing.T) {
	dir := t.TempDir()
	base := &NetworkBaseline{ResolvConf: "# baselinetest", Hosts: "# hosts"}
	// RestoreFiles 只写"当前内容与基线不同且基线非空"的文件；若 resolv.conf 恰好与
	// 测试用的假基线一致仍存在，则跳过。此处以空字段基线验证幂等（不破坏真实 /etc）。
	emptyBase := &NetworkBaseline{ResolvConf: "", Hosts: ""}
	if changed, err := emptyBase.RestoreFiles(); err != nil {
		t.Fatalf("RestoreFiles empty: %v", err)
	} else if len(changed) > 0 {
		t.Fatalf("empty baseline should change nothing, got %v", changed)
	}
	if base.Summary() == "" {
		t.Fatal("summary empty")
	}
	if _, err := LoadNetworkBaseline(dir); err == nil {
		t.Fatal("expected error loading missing baseline")
	}
}

func TestExtraProxyKeys(t *testing.T) {
	base := &NetworkBaseline{ProxyEnv: map[string]string{
		"http_proxy":  "http://p:8080",
		"HTTP_PROXY":  "http://p:8080",
		"ENVIRONMENT": "FOO=bar",
	}}
	keys := base.ExtraProxyKeys()
	if len(keys) != 1 || keys[0] != "ENVIRONMENT" {
		t.Fatalf("ExtraProxyKeys = %v, want [ENVIRONMENT]", keys)
	}
}
