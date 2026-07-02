package snapshot

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/container"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

type Manager struct {
	mu        sync.RWMutex
	dataDir   string
	container *container.Manager
	snapshots map[types.AgentID][]types.Snapshot
}

func NewManager(dataDir string, cm *container.Manager) *Manager {
	return &Manager{
		dataDir:   filepath.Join(dataDir, "snapshots"),
		container: cm,
		snapshots: make(map[types.AgentID][]types.Snapshot),
	}
}

func (m *Manager) Create(ctx context.Context, agentID types.AgentID, containerID string, reason string) (*types.Snapshot, error) {
	snapDir := filepath.Join(m.dataDir, string(agentID))
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}

	snapID := types.SnapshotID(fmt.Sprintf("snap_%s_%d", agentID, time.Now().UnixNano()))
	imageTag := fmt.Sprintf("homeagent/snap-%s:%s", agentID, snapID)
	imagePath := filepath.Join(snapDir, string(snapID)+".tar")

	if err := m.container.Commit(ctx, containerID, imageTag); err != nil {
		return nil, fmt.Errorf("commit container: %w", err)
	}
	if err := m.container.SaveImage(ctx, imageTag, imagePath); err != nil {
		return nil, fmt.Errorf("save image: %w", err)
	}

	info, err := os.Stat(imagePath)
	var size int64
	if err == nil {
		size = info.Size()
	}

	snap := types.Snapshot{
		ID:          snapID,
		AgentID:     agentID,
		CreatedAt:   time.Now(),
		Reason:      reason,
		Size:        size,
		DockerImage: imageTag,
		Valid:       true,
	}

	m.mu.Lock()
	m.snapshots[agentID] = append(m.snapshots[agentID], snap)
	m.mu.Unlock()

	log.Printf("[snapshot] created %s for agent %s (reason: %s, size: %d bytes)", snapID, agentID, reason, size)

	m.enforceLimit(agentID)

	return &snap, nil
}

func (m *Manager) Restore(ctx context.Context, agentID types.AgentID, containerID string, snapID types.SnapshotID) error {
	m.mu.RLock()
	snapshots := m.snapshots[agentID]
	var target *types.Snapshot
	for _, s := range snapshots {
		if s.ID == snapID && s.Valid {
			target = &s
			break
		}
	}
	m.mu.RUnlock()

	if target == nil {
		return fmt.Errorf("snapshot %s not found or invalid", snapID)
	}

	snapDir := filepath.Join(m.dataDir, string(agentID))
	imagePath := filepath.Join(snapDir, string(snapID)+".tar")

	if _, err := os.Stat(imagePath); os.IsNotExist(err) {
		return fmt.Errorf("snapshot file %s not found", imagePath)
	}

	if err := m.container.Stop(ctx, containerID); err != nil {
		log.Printf("[snapshot] warning: stop container during restore: %v", err)
	}

	if err := m.container.Remove(ctx, containerID); err != nil {
		return fmt.Errorf("remove container for restore: %w", err)
	}

	if err := m.container.LoadImage(ctx, imagePath); err != nil {
		return fmt.Errorf("load snapshot image: %w", err)
	}

	log.Printf("[snapshot] restored agent %s to snapshot %s", agentID, snapID)
	return nil
}

func (m *Manager) List(agentID types.AgentID) []types.Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	snapshots := m.snapshots[agentID]
	result := make([]types.Snapshot, len(snapshots))
	copy(result, snapshots)

	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})

	return result
}

func (m *Manager) Latest(agentID types.AgentID) *types.Snapshot {
	snapshots := m.List(agentID)
	if len(snapshots) == 0 {
		return nil
	}
	return &snapshots[0]
}

func (m *Manager) MarkInvalid(agentID types.AgentID, snapID types.SnapshotID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, s := range m.snapshots[agentID] {
		if s.ID == snapID {
			m.snapshots[agentID][i].Valid = false
			return
		}
	}
}

func (m *Manager) enforceLimit(agentID types.AgentID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	snapshots := m.snapshots[agentID]
	if len(snapshots) <= 20 {
		return
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].CreatedAt.Before(snapshots[j].CreatedAt)
	})

	toRemove := len(snapshots) - 20
	for i := 0; i < toRemove; i++ {
		s := snapshots[i]
		snapDir := filepath.Join(m.dataDir, string(agentID))
		imagePath := filepath.Join(snapDir, string(s.ID)+".tar")
		os.Remove(imagePath)
	}

	m.snapshots[agentID] = snapshots[toRemove:]
}

func (m *Manager) Cleanup(agentID types.AgentID, keep int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	snapshots := m.snapshots[agentID]
	if len(snapshots) <= keep {
		return
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].CreatedAt.Before(snapshots[j].CreatedAt)
	})

	toRemove := len(snapshots) - keep
	for i := 0; i < toRemove; i++ {
		s := snapshots[i]
		snapDir := filepath.Join(m.dataDir, string(agentID))
		imagePath := filepath.Join(snapDir, string(s.ID)+".tar")
		os.Remove(imagePath)
	}

	m.snapshots[agentID] = snapshots[toRemove:]
}
