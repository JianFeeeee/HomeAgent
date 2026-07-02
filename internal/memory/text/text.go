package text

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event — 原始 I/O 事件记录，写入 JSONL
type Event struct {
	Timestamp int64    `json:"ts"`
	Source    string   `json:"source"`
	Input     string   `json:"input"`
	Response  string   `json:"response,omitempty"`
	ToolsUsed []string `json:"tools_used,omitempty"`
	AgentID   string   `json:"agent_id,omitempty"`
}

// Memory — 文本记忆：追加写 JSONL，按时间/大小旋转
type Memory struct {
	dir      string
	interval time.Duration
	maxSize  int64

	mu      sync.Mutex
	current *os.File
	encoder *json.Encoder
	created time.Time
	size    int64

	stopCh chan struct{}
}

type Option func(*Memory)

func WithRotationInterval(d time.Duration) Option {
	return func(m *Memory) { m.interval = d }
}

func WithMaxSizeBytes(n int64) Option {
	return func(m *Memory) { m.maxSize = n }
}

func New(dir string, opts ...Option) *Memory {
	m := &Memory{
		dir:      dir,
		interval: 24 * time.Hour,
		maxSize:  10 * 1024 * 1024,
		stopCh:   make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func (m *Memory) Start() error {
	if err := os.MkdirAll(m.dir, 0755); err != nil {
		return fmt.Errorf("text memory dir: %w", err)
	}
	if err := m.openCurrent(); err != nil {
		return err
	}
	go m.rotationLoop()
	return nil
}

func (m *Memory) Stop() {
	close(m.stopCh)
	m.mu.Lock()
	if m.current != nil {
		m.current.Close()
	}
	m.mu.Unlock()
}

func (m *Memory) Append(evt Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.needRotate() {
		m.rotateLocked()
	}

	if err := m.encoder.Encode(evt); err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	m.current.Sync()
	return nil
}

func (m *Memory) needRotate() bool {
	return time.Since(m.created) > m.interval || m.size > m.maxSize
}

func (m *Memory) rotateLocked() {
	if m.current != nil {
		m.current.Close()
	}
	m.openCurrent()
}

func (m *Memory) openCurrent() error {
	now := time.Now()
	name := fmt.Sprintf("text_%s.jsonl", now.Format("2006-01-02_15-04-05"))
	path := filepath.Join(m.dir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open text log %s: %w", path, err)
	}

	stat, _ := f.Stat()
	m.current = f
	m.encoder = json.NewEncoder(f)
	m.created = now
	m.size = stat.Size()
	return nil
}

func (m *Memory) rotationLoop() {
	ticker := time.NewTicker(m.interval / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			if m.needRotate() {
				m.rotateLocked()
				log.Printf("[text memory] rotated log file")
			}
			m.mu.Unlock()
		case <-m.stopCh:
			return
		}
	}
}

// Replay — 从 JSONL 文件流式回放事件
func (m *Memory) Replay(fn func(Event) error) error {
	m.mu.Lock()
	files, err := m.listFiles()
	m.mu.Unlock()
	if err != nil {
		return err
	}

	for _, fpath := range files {
		if err := m.replayFile(fpath, fn); err != nil {
			return err
		}
	}
	return nil
}

func (m *Memory) replayFile(path string, fn func(Event) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var evt Event
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if err := fn(evt); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (m *Memory) listFiles() ([]string, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "text_") && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, filepath.Join(m.dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

// RecentEvents — 返回最近 n 条事件（跨所有文件的最新事件）
func (m *Memory) RecentEvents(n int) ([]Event, error) {
	var all []Event
	err := m.Replay(func(evt Event) error {
		all = append(all, evt)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

func (m *Memory) FileCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	files, err := m.listFiles()
	if err != nil {
		return 0
	}
	return len(files)
}

func (m *Memory) Stats() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	files, _ := m.listFiles()
	return map[string]interface{}{
		"file_count":      len(files),
		"current_size":    m.size,
		"rotation_bytes":  m.maxSize,
		"rotation_interval": m.interval.String(),
		"dir":             m.dir,
	}
}
