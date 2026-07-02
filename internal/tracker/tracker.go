package tracker

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

type Tracker struct {
	mu         sync.Mutex
	dataDir    string
	workDir    string
	lowerDir   string
	upperDir   string
	mergeDir   string
	mounted    bool
	active     bool
	before     *FSState
	changeSets []*ChangeSet
}

func NewTracker(dataDir, workDir string) *Tracker {
	return &Tracker{
		dataDir:    dataDir,
		workDir:    workDir,
		lowerDir:   filepath.Join(workDir, "lower"),
		upperDir:   filepath.Join(workDir, "upper"),
		mergeDir:   filepath.Join(workDir, "merged"),
		changeSets: make([]*ChangeSet, 0),
	}
}

func (t *Tracker) Init() error {
	for _, d := range []string{t.lowerDir, t.upperDir, t.mergeDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("create overlay dir %s: %w", d, err)
		}
	}
	log.Printf("[tracker] initialized (work=%s)", t.workDir)
	return nil
}

func (t *Tracker) Start() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.mounted {
		return nil
	}

	if err := t.mountOverlay(); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	t.mounted = true
	t.active = true

	t.before = t.capture()

	log.Printf("[tracker] overlay mounted at %s", t.mergeDir)
	return nil
}

func (t *Tracker) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.mounted {
		return nil
	}

	if err := t.umountOverlay(); err != nil {
		return fmt.Errorf("umount overlay: %w", err)
	}
	t.mounted = false
	t.active = false
	return nil
}

func (t *Tracker) PreAction(action string) *ChangeSet {
	t.mu.Lock()
	defer t.mu.Unlock()

	cs := NewChangeSet(action)
	t.before = t.capture()
	return cs
}

func (t *Tracker) PostAction(action string) *ChangeSet {
	t.mu.Lock()
	defer t.mu.Unlock()

	after := t.capture()
	changes := diffStates(t.before, after)

	cs := NewChangeSet(action)
	cs.Files = changes
	if len(changes) > 0 {
		t.changeSets = append(t.changeSets, cs)
		t.saveChangeSet(cs)
		log.Printf("[tracker] action=%s changed=%d files", action, len(changes))
		for _, f := range changes {
			log.Printf("  %s: %s", f.Type, f.Path)
		}
	}

	t.before = t.capture()
	return cs
}

func (t *Tracker) HasChanges() bool {
	return len(t.changeSets) > 0
}

func (t *Tracker) ChangeSets() []*ChangeSet {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]*ChangeSet, len(t.changeSets))
	copy(result, t.changeSets)
	return result
}

func (t *Tracker) capture() *FSState {
	state, err := captureFSState(t.upperDir)
	if err != nil {
		return &FSState{Files: make(map[string]FileChange), Root: t.upperDir}
	}
	return state
}

func (t *Tracker) mountOverlay() error {
	workDir := filepath.Join(t.workDir, "work")
	os.MkdirAll(workDir, 0755)

	args := []string{
		"-t", "overlay",
		"overlay",
		"-o", fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", t.lowerDir, t.upperDir, workDir),
		t.mergeDir,
	}

	cmd := exec.Command("mount", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount overlayfs failed: %s: %w", string(output), err)
	}
	return nil
}

func (t *Tracker) umountOverlay() error {
	cmd := exec.Command("umount", t.mergeDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("umount overlayfs failed: %s: %w", string(output), err)
	}
	return nil
}

func (t *Tracker) saveChangeSet(cs *ChangeSet) {
	dir := filepath.Join(t.dataDir, "changesets")
	os.MkdirAll(dir, 0755)

	path := filepath.Join(dir, cs.ID+".json")
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		log.Printf("[tracker] save changeset %s: %v", cs.ID, err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("[tracker] write changeset %s: %v", cs.ID, err)
	}
}

func (t *Tracker) Rollback() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.mounted {
		if err := t.umountOverlay(); err != nil {
			return fmt.Errorf("umount for rollback: %w", err)
		}
	}

	if err := os.RemoveAll(t.upperDir); err != nil {
		return fmt.Errorf("remove upper: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(t.workDir, "work")); err != nil {
		return fmt.Errorf("remove work: %w", err)
	}

	if err := os.MkdirAll(t.upperDir, 0755); err != nil {
		return fmt.Errorf("recreate upper: %w", err)
	}

	t.changeSets = nil
	t.before = nil
	t.mounted = false

	log.Printf("[tracker] rollback complete")
	return nil
}

func (t *Tracker) MergeDir() string {
	return t.mergeDir
}

func (t *Tracker) Stats() map[string]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	totalChanges := 0
	for _, cs := range t.changeSets {
		totalChanges += len(cs.Files)
	}
	return map[string]interface{}{
		"mounted":         t.mounted,
		"active":          t.active,
		"change_sets":     len(t.changeSets),
		"total_changes":   totalChanges,
		"merge_dir":       t.mergeDir,
		"upper_dir":       t.upperDir,
	}
}
