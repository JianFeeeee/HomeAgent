package text

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	m := New(t.TempDir())
	if m == nil {
		t.Fatal("expected non-nil memory")
	}
}

func TestAppendAndReplay(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	evt := Event{
		Timestamp: time.Now().Unix(),
		Source:    "test",
		Input:     "hello",
		Response:  "world",
	}
	if err := m.Append(evt); err != nil {
		t.Fatal(err)
	}

	var count int
	err := m.Replay(func(e Event) error {
		count++
		if e.Input != "hello" || e.Response != "world" {
			t.Errorf("unexpected event: %+v", e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 event, got %d", count)
	}
}

func TestAppendMultiple(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	for i := 0; i < 10; i++ {
		m.Append(Event{
			Timestamp: time.Now().Unix(),
			Source:    "test",
			Input:     "msg",
		})
	}

	var count int
	m.Replay(func(e Event) error { count++; return nil })
	if count != 10 {
		t.Errorf("expected 10 events, got %d", count)
	}
}

func TestRecentEvents(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	for i := 0; i < 10; i++ {
		m.Append(Event{
			Timestamp: time.Now().Unix(),
			Source:    "test",
			Input:     "msg",
		})
	}

	recent, err := m.RecentEvents(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 3 {
		t.Errorf("expected 3 recent events, got %d", len(recent))
	}
}

func TestFileCount(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	if m.FileCount() != 1 {
		t.Errorf("expected 1 file, got %d", m.FileCount())
	}
}

func TestConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Append(Event{
				Timestamp: time.Now().Unix(),
				Source:    "test",
				Input:     "concurrent",
			})
		}()
	}
	wg.Wait()

	var count int
	m.Replay(func(e Event) error { count++; return nil })
	if count != 20 {
		t.Errorf("expected 20 events, got %d", count)
	}
}

func TestPurgeByFilter(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.Append(Event{Timestamp: 1, Source: "keep", Input: "a"})
	m.Append(Event{Timestamp: 2, Source: "delete", Input: "b"})
	m.Append(Event{Timestamp: 3, Source: "keep", Input: "c"})

	removed, err := m.PurgeByFilter(func(e Event) bool {
		return e.Source == "delete"
	})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}

	var count int
	m.Replay(func(e Event) error { count++; return nil })
	if count != 2 {
		t.Errorf("expected 2 events after purge, got %d", count)
	}
}

func TestReplaceByFilter(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.Append(Event{Timestamp: 1, Source: "old", Input: "a"})

	replaced, err := m.ReplaceByFilter(
		func(e Event) bool { return e.Source == "old" },
		func(e Event) Event { e.Source = "new"; return e },
	)
	if err != nil {
		t.Fatal(err)
	}
	if replaced != 1 {
		t.Errorf("expected 1 replaced, got %d", replaced)
	}

	var evt Event
	m.Replay(func(e Event) error { evt = e; return nil })
	if evt.Source != "new" {
		t.Errorf("expected source 'new', got %q", evt.Source)
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, WithMaxSizeBytes(100)) // small max size to trigger rotation
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	// Write enough data to trigger rotation
	for i := 0; i < 50; i++ {
		m.Append(Event{
			Timestamp: time.Now().Unix(),
			Source:    "test",
			Input:     strings.Repeat("x", 50),
		})
	}

	if m.FileCount() > 1 {
		t.Logf("rotation triggered: %d files", m.FileCount())
	}
}

func TestStats(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, WithMaxSizeBytes(1000), WithRotationInterval(30*time.Minute))
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.Append(Event{Timestamp: time.Now().Unix(), Source: "test", Input: "hello"})

	stats := m.Stats()
	if stats["file_count"] != 1 {
		t.Errorf("expected 1 file, got %v", stats["file_count"])
	}
	if stats["rotation_bytes"].(int64) != 1000 {
		t.Errorf("expected 1000 bytes, got %v", stats["rotation_bytes"])
	}
}

func TestStopWithoutStart(t *testing.T) {
	m := New(t.TempDir())
	// Should not panic
	m.Stop()
}

func TestEmptyDir(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	var count int
	m.Replay(func(e Event) error { count++; return nil })
	if count != 0 {
		t.Errorf("expected 0 events in empty dir, got %d", count)
	}
}

func TestFilePersistence(t *testing.T) {
	dir := t.TempDir()

	// Write events
	m1 := New(dir)
	m1.Start()
	m1.Append(Event{Timestamp: 100, Source: "test", Input: "persist"})
	m1.Stop()

	// Read back with new instance
	m2 := New(dir)
	m2.Start()
	defer m2.Stop()

	var count int
	m2.Replay(func(e Event) error {
		count++
		return nil
	})
	if count != 1 {
		t.Errorf("expected 1 persisted event, got %d", count)
	}
}

func TestListFiles(t *testing.T) {
	dir := t.TempDir()
	// Create a non-text file that should be ignored
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("ignored"), 0644)

	m := New(dir)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.Append(Event{Timestamp: 1, Source: "test", Input: "a"})

	files, err := m.listFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("expected 1 file, got %d", len(files))
	}
}

func TestPurgeAll(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.Append(Event{Timestamp: 1, Source: "test", Input: "a"})
	m.Append(Event{Timestamp: 2, Source: "test", Input: "b"})

	removed, err := m.PurgeByFilter(func(e Event) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}

	var count int
	m.Replay(func(e Event) error { count++; return nil })
	if count != 0 {
		t.Errorf("expected 0 events after full purge, got %d", count)
	}
}
