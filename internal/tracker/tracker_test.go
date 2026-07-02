package tracker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewTracker(t *testing.T) {
	tr := NewTracker("/tmp/tracker_data", "/tmp/tracker_work")
	if tr == nil {
		t.Fatal("tracker should not be nil")
	}
	if tr.mergeDir != "/tmp/tracker_work/merged" {
		t.Errorf("unexpected mergeDir: %s", tr.mergeDir)
	}
}

func TestInit(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(filepath.Join(dir, "data"), filepath.Join(dir, "work"))
	if err := tr.Init(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{tr.lowerDir, tr.upperDir, tr.mergeDir} {
		if _, err := os.Stat(d); os.IsNotExist(err) {
			t.Errorf("dir %s should exist", d)
		}
	}
}

func TestNewChangeSet(t *testing.T) {
	cs := NewChangeSet("test_action")
	if cs.Action != "test_action" {
		t.Errorf("expected 'test_action', got %q", cs.Action)
	}
	if cs.ID == "" {
		t.Error("ID should not be empty")
	}
	if len(cs.Files) != 0 {
		t.Errorf("expected 0 files, got %d", len(cs.Files))
	}
}

func TestFileHash(t *testing.T) {
	f := t.TempDir() + "/test.txt"
	if err := os.WriteFile(f, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	hash, size, err := fileHash(f)
	if err != nil {
		t.Fatal(err)
	}
	if size != 5 {
		t.Errorf("expected size 5, got %d", size)
	}
	if hash == "" {
		t.Error("hash should not be empty")
	}
	// SHA256 of "hello"
	expected := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if hash != expected {
		t.Errorf("expected hash %s, got %s", expected, hash)
	}
}

func TestFileHashNotFound(t *testing.T) {
	_, _, err := fileHash("/nonexistent/file")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestFileInfo(t *testing.T) {
	f := t.TempDir() + "/info.txt"
	os.WriteFile(f, []byte("test"), 0644)

	size, modTime, err := fileInfo(f)
	if err != nil {
		t.Fatal(err)
	}
	if size != 4 {
		t.Errorf("expected size 4, got %d", size)
	}
	if modTime.IsZero() {
		t.Error("modTime should not be zero")
	}
}

func TestFileInfoNotFound(t *testing.T) {
	_, _, err := fileInfo("/nonexistent/file")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestCaptureFSStateEmpty(t *testing.T) {
	dir := t.TempDir()
	state, err := captureFSState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Files) != 0 {
		t.Errorf("expected 0 files, got %d", len(state.Files))
	}
	if state.Root != dir {
		t.Errorf("expected root %s, got %s", dir, state.Root)
	}
}

func TestCaptureFSState(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb"), 0644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0755)
	os.WriteFile(filepath.Join(dir, "sub", "c.txt"), []byte("ccc"), 0644)

	state, err := captureFSState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Files) != 3 {
		t.Errorf("expected 3 files, got %d", len(state.Files))
	}
	// should contain relative paths
	for _, p := range []string{"a.txt", "b.txt", "sub/c.txt"} {
		if _, ok := state.Files[p]; !ok {
			t.Errorf("missing file %s", p)
		}
	}
}

func TestDiffStatesCreated(t *testing.T) {
	dir := t.TempDir()
	before, _ := captureFSState(dir)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new file"), 0644)
	after, _ := captureFSState(dir)

	changes := diffStates(before, after)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Type != ChangeFileCreated {
		t.Errorf("expected created, got %s", changes[0].Type)
	}
	if changes[0].Path != "new.txt" {
		t.Errorf("expected 'new.txt', got %s", changes[0].Path)
	}
}

func TestDiffStatesModified(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("original"), 0644)
	before, _ := captureFSState(dir)
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("modified"), 0644)
	after, _ := captureFSState(dir)

	changes := diffStates(before, after)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Type != ChangeFileModified {
		t.Errorf("expected modified, got %s", changes[0].Type)
	}
}

func TestDiffStatesDeleted(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("delete me"), 0644)
	before, _ := captureFSState(dir)
	os.Remove(filepath.Join(dir, "f.txt"))
	after, _ := captureFSState(dir)

	changes := diffStates(before, after)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Type != ChangeFileDeleted {
		t.Errorf("expected deleted, got %s", changes[0].Type)
	}
}

func TestDiffStatesNoChange(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("stable"), 0644)
	before, _ := captureFSState(dir)
	after, _ := captureFSState(dir)

	changes := diffStates(before, after)
	if len(changes) != 0 {
		t.Errorf("expected 0 changes, got %d", len(changes))
	}
}

func TestDiffStatesMixed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("unchanged"), 0644)
	os.WriteFile(filepath.Join(dir, "delete.txt"), []byte("gone"), 0644)
	before, _ := captureFSState(dir)

	os.Remove(filepath.Join(dir, "delete.txt"))
	os.WriteFile(filepath.Join(dir, "add.txt"), []byte("new"), 0644)
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("changed"), 0644)
	after, _ := captureFSState(dir)

	changes := diffStates(before, after)
	if len(changes) != 3 {
		t.Fatalf("expected 3 changes, got %d", len(changes))
	}

	types := make(map[ChangeType]bool)
	for _, c := range changes {
		types[c.Type] = true
	}
	if !types[ChangeFileCreated] {
		t.Error("missing created")
	}
	if !types[ChangeFileModified] {
		t.Error("missing modified")
	}
	if !types[ChangeFileDeleted] {
		t.Error("missing deleted")
	}
}

func TestDiffStatesNilBefore(t *testing.T) {
	dir := t.TempDir()
	after, _ := captureFSState(dir)

	changes := diffStates(nil, after)
	if len(changes) != 0 {
		t.Errorf("expected 0 changes when before is nil, got %d", len(changes))
	}
}

func TestPreActionResetsBefore(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(filepath.Join(dir, "data"), filepath.Join(dir, "work"))
	tr.Init()

	// capture initial state
	cs := tr.PreAction("test")
	if cs == nil {
		t.Fatal("changeset should not be nil")
	}
	if cs.Action != "test" {
		t.Errorf("expected 'test', got %q", cs.Action)
	}
}

func TestPostActionNoChanges(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(filepath.Join(dir, "data"), filepath.Join(dir, "work"))
	tr.Init()

	tr.PreAction("noop")
	cs := tr.PostAction("noop")
	if cs == nil {
		t.Fatal("changeset should not be nil")
	}
	if len(cs.Files) != 0 {
		t.Errorf("expected 0 files for noop, got %d", len(cs.Files))
	}
}

func TestStats(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(filepath.Join(dir, "data"), filepath.Join(dir, "work"))
	tr.Init()

	stats := tr.Stats()
	if stats["mounted"].(bool) {
		t.Error("should not be mounted")
	}
	if stats["change_sets"].(int) != 0 {
		t.Errorf("expected 0 changesets, got %d", stats["change_sets"])
	}
}

func TestMergeDir(t *testing.T) {
	tr := NewTracker("/data", "/work")
	if tr.MergeDir() != "/work/merged" {
		t.Errorf("unexpected mergeDir: %s", tr.MergeDir())
	}
}

func TestHasChanges(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(filepath.Join(dir, "data"), filepath.Join(dir, "work"))
	tr.Init()

	if tr.HasChanges() {
		t.Error("should have no changes initially")
	}
}

func TestChangeSetsEmpty(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(filepath.Join(dir, "data"), filepath.Join(dir, "work"))
	tr.Init()

	cs := tr.ChangeSets()
	if len(cs) != 0 {
		t.Errorf("expected 0 changesets, got %d", len(cs))
	}
}

func TestCaptureDirNotExist(t *testing.T) {
	_, err := captureFSState("/tmp/nonexistent_test_dir_12345")
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}
