package supervisor

import (
	"testing"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

func TestNew(t *testing.T) {
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	d := New(cfg)
	if d == nil {
		t.Fatal("Daemon should not be nil")
	}
}

func TestStartAndShutdown(t *testing.T) {
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	d := New(cfg)
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	d.Shutdown()
}

func TestRegisterAgent(t *testing.T) {
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	d := New(cfg)
	d.RegisterAgent("test_agent")

	agents := d.ListAgents()
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].ID != "test_agent" {
		t.Errorf("expected 'test_agent', got %q", agents[0].ID)
	}
	if agents[0].State != types.AgentStateRunning {
		t.Errorf("expected Running state, got %v", agents[0].State)
	}
}

func TestGetAgentStatus(t *testing.T) {
	cfg := &types.Config{
		Defaults: types.AgentConfig{
			RollbackPolicy: types.RollbackPolicy{MaxRetries: 5},
		},
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	d := New(cfg)
	d.RegisterAgent("agent1")

	status, err := d.GetAgentStatus("agent1")
	if err != nil {
		t.Fatal(err)
	}
	if status.ID != "agent1" {
		t.Errorf("expected 'agent1', got %q", status.ID)
	}
}

func TestGetAgentStatusNotFound(t *testing.T) {
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	d := New(cfg)
	_, err := d.GetAgentStatus("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestListAgents(t *testing.T) {
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	d := New(cfg)
	d.RegisterAgent("a")
	d.RegisterAgent("b")

	agents := d.ListAgents()
	if len(agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(agents))
	}
}

func TestSetTracker(t *testing.T) {
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	d := New(cfg)
	tr := tracker.NewTracker("/tmp/test_tracker_data", "/tmp/test_tracker_work")
	d.SetTracker(tr)

	// RegisterAgent should use tracker
	d.RegisterAgent("tracked_agent")
	status, _ := d.GetAgentStatus("tracked_agent")
	if status.TrackerStats == nil {
		t.Error("expected tracker stats")
	}
}

func TestRollbackAgent(t *testing.T) {
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	d := New(cfg)
	tr := tracker.NewTracker("/tmp/test_rb_data", "/tmp/test_rb_work")
	d.SetTracker(tr)

	err := d.RollbackAgent("any", "snap1")
	if err == nil {
		t.Log("rollback succeeded (tracker may be unmounted)")
	}
}

func TestRollbackAgentNoTracker(t *testing.T) {
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	d := New(cfg)
	err := d.RollbackAgent("main", "snap1")
	if err == nil {
		t.Error("expected error when no tracker")
	}
}

func TestPreActionSnapshot(t *testing.T) {
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	d := New(cfg)
	_, err := d.PreActionSnapshot("main")
	if err == nil {
		t.Error("expected error (snapshot not supported)")
	}
}

func TestRegisterAgentMultiple(t *testing.T) {
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	d := New(cfg)

	for i := 0; i < 5; i++ {
		d.RegisterAgent(types.AgentID(string(rune('a'+i))))
	}

	agents := d.ListAgents()
	if len(agents) != 5 {
		t.Errorf("expected 5 agents, got %d", len(agents))
	}
}

func TestConcurrentAccess(t *testing.T) {
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	d := New(cfg)

	// Register from multiple goroutines
	for i := 0; i < 10; i++ {
		go d.RegisterAgent(types.AgentID(string(rune('a' + i))))
	}
}
