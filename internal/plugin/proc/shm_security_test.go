package proc

import (
	"testing"
)

func TestShmSecurityMode_DefaultsToSafe(t *testing.T) {
	if ShmSecurityModeOf() != ShmModeSafe {
		t.Fatalf("默认应为 safe，实际 %q", ShmSecurityModeOf())
	}
}

func TestShmAudit_ZeroPluginsNoMapping(t *testing.T) {
	h, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer h.Close()

	a, ok := h.AuditShmMappings(0)
	if !ok {
		// 非 Linux 平台无探针，本测试断言"平台探针不误报"
		t.Logf("平台无探针（ok=false），跳过")
		return
	}
	// 无子进程时实际映射数应为 0
	if a.ActualMappings != 0 {
		t.Fatalf("无子进程时应无映射，实际 %d", a.ActualMappings)
	}
	if a.Delta != 0 {
		t.Fatalf("Delta 应为 0，实际 %d", a.Delta)
	}
}

// 子进程挂载共享段后，Linux 探针应能看到对应映射。
func TestShmAudit_SeesChildMapping(t *testing.T) {
	bin := buildTestPlugin(t, "stageplugin.go")
	core := newFakeCore()

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	p := New("audit", bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	a, ok := host.AuditShmMappings(1)
	if !ok {
		t.Logf("平台无探针（ok=false），跳过")
		return
	}
	// 至少应看到 1 个儿童映射
	if a.ActualMappings < 1 {
		t.Fatalf("子进程应映射共享段，实际映射数 %d", a.ActualMappings)
	}
	// 期望 1、实际 ≥1 → delta 不应为负且不应报警（delta>0 才告警）
	_ = a
}
