package ipc

import (
	"testing"
	"time"
)

func TestPingAck(t *testing.T) {
	dir := t.TempDir()
	ok := true
	srv := NewServer(dir, func() *Status {
		return &Status{PID: 123, Boot: "normal", UptimeSec: 42, LLMOK: &ok, Tools: 5, LastDiag: "diag:network"}
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	cli := NewClient(dir)
	st, err := cli.Ping(2 * time.Second)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if st == nil {
		t.Fatal("nil status")
	}
	if st.PID != 123 || st.Boot != "normal" || st.Tools != 5 {
		t.Fatalf("status mismatch: %+v", st)
	}
	if st.LLMOK == nil || !*st.LLMOK {
		t.Fatal("LLMOK should be true")
	}
	if st.LastDiag != "diag:network" {
		t.Fatalf("LastDiag = %q", st.LastDiag)
	}
}

func TestPingTimeout(t *testing.T) {
	dir := t.TempDir()
	// 未启动 server → 立即超时
	cli := NewClient(dir)
	start := time.Now()
	_, err := cli.Ping(500 * time.Millisecond)
	if err == nil {
		t.Fatal("expected error when no server")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout took too long")
	}
}

func TestServerNoStatusFunc(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer(dir, nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()
	cli := NewClient(dir)
	st, err := cli.Ping(2 * time.Second)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if st != nil {
		t.Fatalf("expected nil status when no status func, got %+v", st)
	}
}
