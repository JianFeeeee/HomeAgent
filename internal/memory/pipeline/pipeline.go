package pipeline

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
)

type RawRecord struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	Distilled bool      `json:"distilled"`
}

type DistillerConfig struct {
	Interval      time.Duration `json:"interval"`
	RetentionDays int           `json:"retention_days"`
	BatchSize     int           `json:"batch_size"`
}

type Distiller struct {
	mu        sync.Mutex
	db        *memory.GraphDB
	rawPath   string
	records   []RawRecord
	nextID    int64
	cfg       DistillerConfig
	ctx       context.Context
	cancel    context.CancelFunc
	onMemory  func(input, response string)
}

func NewDistiller(db *memory.GraphDB, dataDir string, cfg DistillerConfig) *Distiller {
	ctx, cancel := context.WithCancel(context.Background())
	return &Distiller{
		db:      db,
		rawPath: filepath.Join(dataDir, "memory", "raw"),
		cfg:     cfg,
		ctx:     ctx,
		cancel:  cancel,
	}
}

func (d *Distiller) OnMemoryCandidate(fn func(input, response string)) {
	d.onMemory = fn
}

func (d *Distiller) Start() {
	if err := os.MkdirAll(d.rawPath, 0755); err != nil {
		log.Printf("[memory] create raw path: %v", err)
	}
	d.loadExisting()
	log.Printf("[memory] distiller started (interval: %v, retention: %d days)", d.cfg.Interval, d.cfg.RetentionDays)
	go d.distillLoop()
}

func (d *Distiller) Stop() {
	d.cancel()
	d.flush()
}

func (d *Distiller) Append(sessionID string, role string, content string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextID++
	d.records = append(d.records, RawRecord{
		ID: d.nextID, SessionID: sessionID, Role: role,
		Content: content, CreatedAt: time.Now(),
	})
}

func (d *Distiller) flush() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.records) == 0 {
		return
	}
	path := filepath.Join(d.rawPath, fmt.Sprintf("raw_%d.jsonl", time.Now().UnixNano()))
	f, err := os.Create(path)
	if err != nil {
		log.Printf("[memory] flush error: %v", err)
		return
	}
	defer f.Close()
	for _, r := range d.records {
		line := fmt.Sprintf("%d\t%s\t%s\t%s\t%d\n", r.ID, r.SessionID, r.Role, r.Content, r.CreatedAt.Unix())
		f.WriteString(line)
	}
}

func (d *Distiller) loadExisting() {
	entries, err := os.ReadDir(d.rawPath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(d.rawPath, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range parseLines(string(data)) {
			parts := splitLine(line)
			if len(parts) >= 4 {
				d.records = append(d.records, RawRecord{
					ID: d.nextID, SessionID: parts[1], Role: parts[2], Content: parts[3],
				})
				d.nextID++
			}
		}
	}
}

func (d *Distiller) distillLoop() {
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.distillOnce()
		case <-d.ctx.Done():
			return
		}
	}
}

func (d *Distiller) distillOnce() {
	d.mu.Lock()
	cutoff := time.Now().AddDate(0, 0, -d.cfg.RetentionDays)
	var toDistill []RawRecord
	var remaining []RawRecord
	for _, r := range d.records {
		if r.CreatedAt.Before(cutoff) && !r.Distilled {
			toDistill = append(toDistill, r)
		} else {
			remaining = append(remaining, r)
		}
	}
	d.records = remaining
	d.mu.Unlock()

	if len(toDistill) == 0 {
		return
	}

	batchSize := d.cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 50
	}
	for i := 0; i < len(toDistill); i += batchSize {
		end := i + batchSize
		if end > len(toDistill) {
			end = len(toDistill)
		}
		d.distillBatch(toDistill[i:end])
	}
	d.cleanupRawFiles()
	log.Printf("[memory] distilled %d records", len(toDistill))
}

func (d *Distiller) distillBatch(batch []RawRecord) {
	var userContent, assistantContent string
	sessionIDs := make(map[string]bool)
	for _, r := range batch {
		sessionIDs[r.SessionID] = true
		if r.Role == "user" {
			userContent += r.Content + " "
		} else {
			assistantContent += r.Content + " "
		}
	}
	triples := extractKeyTriples(userContent, assistantContent)
	if len(triples) > 0 {
		sessionID := ""
		for sid := range sessionIDs {
			sessionID = sid
			break
		}
		if _, _, err := d.db.Commit(triples, sessionID, 0); err != nil {
			log.Printf("[memory] distill commit: %v", err)
		}
	}
}

func (d *Distiller) cleanupRawFiles() {
	entries, err := os.ReadDir(d.rawPath)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -(d.cfg.RetentionDays + 1))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(d.rawPath, entry.Name()))
		}
	}
}

func extractKeyTriples(userContent, assistantContent string) []memory.Triple {
	var triples []memory.Triple
	if len(userContent) > 0 && len(userContent) < 500 {
		triples = append(triples, memory.Triple{Subject: "用户", Relation: "提及", Object: truncate(userContent, 200)})
	}
	if len(assistantContent) > 0 && len(assistantContent) < 500 {
		triples = append(triples, memory.Triple{Subject: "AI", Relation: "回应", Object: truncate(assistantContent, 200)})
	}
	return triples
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func parseLines(data string) []string {
	var lines []string
	current := ""
	for _, ch := range data {
		if ch == '\n' {
			if current != "" {
				lines = append(lines, current)
			}
			current = ""
		} else {
			current += string(ch)
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func splitLine(line string) []string {
	var parts []string
	current := ""
	for _, ch := range line {
		if ch == '\t' {
			parts = append(parts, current)
			current = ""
		} else {
			current += string(ch)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}

func (d *Distiller) GetRecentRecords(limit int) []RawRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(d.records)
	if n == 0 {
		return nil
	}
	if limit > 0 && limit < n {
		n = limit
	}
	result := make([]RawRecord, n)
	copy(result, d.records[len(d.records)-n:])
	return result
}

func (d *Distiller) Stats() map[string]interface{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]interface{}{
		"raw_records":    len(d.records),
		"interval":       d.cfg.Interval.String(),
		"retention_days": d.cfg.RetentionDays,
	}
}
