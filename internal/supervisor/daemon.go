package supervisor

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/network"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

const directAgentID types.AgentID = "main"

type Daemon struct {
	cfg    *types.Config
	nm     *network.Monitor
	trk    *tracker.Tracker
	agents map[types.AgentID]*agentInstance
	mu     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc
}

type agentInstance struct {
	cfg        *types.AgentConfig
	state      types.AgentState
	health     types.HealthStatus
	lastHB     time.Time
	failCount  int
	useTracker bool
}

func New(cfg *types.Config) *Daemon {
	ctx, cancel := context.WithCancel(context.Background())
	nm := network.NewMonitor(cfg.Daemon.CheckInterval)

	return &Daemon{
		cfg:    cfg,
		nm:     nm,
		agents: make(map[types.AgentID]*agentInstance),
		ctx:    ctx,
		cancel: cancel,
	}
}

func (d *Daemon) SetTracker(trk *tracker.Tracker) {
	d.trk = trk
}

func (d *Daemon) Start() error {
	log.Println("[homed] starting HomeAgent daemon")

	go d.nm.Start(d.ctx, d.cfg.Defaults.LLMEndpoints)

	go d.healthLoop()

	log.Println("[homed] daemon started successfully")
	return nil
}

func (d *Daemon) Shutdown() {
	log.Println("[homed] shutting down...")
	d.cancel()

	d.mu.Lock()
	defer d.mu.Unlock()

	for id, agent := range d.agents {
		if agent.state == types.AgentStateRunning {
			log.Printf("[homed] stopping agent %s", id)
			agent.state = types.AgentStateStopped
		}
	}
}

func (d *Daemon) RegisterAgent(id types.AgentID) {
	d.mu.Lock()
	defer d.mu.Unlock()

	useTrk := d.trk != nil
	maxRetries := d.cfg.Defaults.RollbackPolicy.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	cfg := &types.AgentConfig{ID: id}
	cfg.RollbackPolicy.MaxRetries = maxRetries

	d.agents[id] = &agentInstance{
		cfg:        cfg,
		state:      types.AgentStateRunning,
		health:     types.HealthHealthy,
		lastHB:     time.Now(),
		useTracker: useTrk,
	}
	log.Printf("[homed] agent %s registered (tracker=%v, maxRetries=%d)", id, useTrk, maxRetries)
}

func (d *Daemon) healthLoop() {
	ticker := time.NewTicker(d.cfg.Daemon.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			d.checkAllAgents()
		case <-d.ctx.Done():
			return
		}
	}
}

func (d *Daemon) checkAllAgents() {
	d.mu.RLock()
	agents := make(map[types.AgentID]*agentInstance)
	for id, a := range d.agents {
		agents[id] = a
	}
	d.mu.RUnlock()

	for id, agent := range agents {
		d.checkAgent(id, agent)
	}
}

func (d *Daemon) checkAgent(id types.AgentID, agent *agentInstance) {
	netStatus := d.nm.AggregateResult()

	if !netStatus.LLMAPIReachable {
		agent.health = types.HealthDegraded
		agent.failCount++
		log.Printf("[homed] agent %s: LLM API unreachable (fail %d)", id, agent.failCount)
	} else {
		agent.health = types.HealthHealthy
		agent.failCount = 0
	}

	agent.lastHB = time.Now()

	if agent.failCount >= agent.cfg.RollbackPolicy.MaxRetries {
		d.handleFailure(id, agent)
	}
}

func (d *Daemon) handleFailure(id types.AgentID, agent *agentInstance) {
	log.Printf("[homed] agent %s failed %d times, initiating recovery", id, agent.failCount)

	if agent.useTracker && d.trk != nil {
		log.Printf("[homed] rolling back agent %s via change tracker", id)
		if err := d.trk.Rollback(); err != nil {
			log.Printf("[homed] tracker rollback failed: %v — restarting agent", err)
			d.restartAgent(id, agent)
			return
		}
		log.Printf("[homed] agent %s tracker rollback complete", id)
		agent.failCount = 0
		return
	}

	d.restartAgent(id, agent)
}

func (d *Daemon) restartAgent(id types.AgentID, agent *agentInstance) {
	log.Printf("[homed] resetting agent %s", id)

	agent.state = types.AgentStateStopped
	d.RegisterAgent(id)

	agent.failCount = 0
	log.Printf("[homed] agent %s reset", id)
}

func (d *Daemon) GetAgentStatus(id types.AgentID) (*AgentStatus, error) {
	d.mu.RLock()
	agent, ok := d.agents[id]
	d.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("agent %s not found", id)
	}

	netStatus := d.nm.AggregateResult()

	trackerStats := map[string]interface{}{"active": false}
	if d.trk != nil {
		trackerStats = d.trk.Stats()
	}

	return &AgentStatus{
		ID:           id,
		State:        agent.state,
		Health:       agent.health,
		Uptime:       time.Since(agent.lastHB),
		Network:      netStatus,
		TrackerStats: trackerStats,
	}, nil
}

func (d *Daemon) ListAgents() []AgentStatus {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var statuses []AgentStatus
	for id, agent := range d.agents {
		statuses = append(statuses, AgentStatus{
			ID:     id,
			State:  agent.state,
			Health: agent.health,
		})
	}
	return statuses
}

func (d *Daemon) PreActionSnapshot(id types.AgentID) (*types.Snapshot, error) {
	return nil, fmt.Errorf("snapshot not supported in direct mode — use tracker instead")
}

func (d *Daemon) RollbackAgent(id types.AgentID, snapID types.SnapshotID) error {
	if d.trk != nil {
		return d.trk.Rollback()
	}
	return fmt.Errorf("no tracker available for rollback")
}

// AgentStatus 已迁入内置 SDK，此处保留别名以兼容现有调用方。
type AgentStatus = sdk.AgentStatus
