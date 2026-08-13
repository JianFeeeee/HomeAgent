package pipeline

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/nlp"
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
	embedder  nlp.Vectorizer
}

func (d *Distiller) SetEmbedder(ev nlp.Vectorizer) { d.embedder = ev }

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
	path := filepath.Join(d.rawPath, fmt.Sprintf("raw_%d.tsv", time.Now().UnixNano()))
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
	cutoff := time.Now().AddDate(0, 0, -d.cfg.RetentionDays)
	type fileInfo struct {
		name string
		mod  time.Time
	}
	var files []fileInfo
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if ext != ".tsv" && ext != ".jsonl" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(d.rawPath, entry.Name()))
			continue
		}
		files = append(files, fileInfo{name: entry.Name(), mod: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	loaded := 0
	const maxStartupRecords = 5000
	for _, entry := range files {
		if loaded >= maxStartupRecords {
			break
		}
		path := filepath.Join(d.rawPath, entry.name)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		func() {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			buf := make([]byte, 0, 64*1024)
			scanner.Buffer(buf, 1024*1024)
			for scanner.Scan() {
				if loaded >= maxStartupRecords {
					break
				}
				line := scanner.Text()
				parts := splitLine(line)
				if len(parts) < 5 {
					continue
				}
				ts, err := strconv.ParseInt(parts[4], 10, 64)
				if err != nil {
					continue
				}
				createdAt := time.Unix(ts, 0)
				if createdAt.Before(cutoff) {
					continue
				}
				d.records = append(d.records, RawRecord{
					ID: d.nextID, SessionID: parts[1], Role: parts[2], Content: parts[3], CreatedAt: createdAt,
				})
				d.nextID++
				loaded++
			}
		}()
	}
	if loaded >= maxStartupRecords {
		log.Printf("[memory] distiller startup load capped at %d recent records", loaded)
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
	batchSize := d.cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 50
	}
	// 每 tick 取前 N 条未蒸馏记录（无 RetentionDays 门槛），蒸馏成功才标记/移除
	var toDistill []RawRecord
	var remaining []RawRecord
	for _, r := range d.records {
		if !r.Distilled && len(toDistill) < batchSize {
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

	distilled := 0
	for i := 0; i < len(toDistill); i += batchSize {
		end := i + batchSize
		if end > len(toDistill) {
			end = len(toDistill)
		}
		if d.distillBatch(toDistill[i:end]) {
			distilled += end - i
		} else {
			// 蒸馏失败：记录写回待处理队列，下次 tick 重试
			d.mu.Lock()
			d.records = append(toDistill[i:end], d.records...)
			d.mu.Unlock()
		}
	}
	d.cleanupRawFiles()
	if distilled > 0 {
		log.Printf("[memory] distilled %d records", distilled)
	}
}

// distillBatch 蒸馏一批记录，全部成功返回 true，任一失败返回 false（调用方重试）
func (d *Distiller) distillBatch(batch []RawRecord) bool {
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
	triples := extractKeyTriples(userContent, assistantContent, d.embedder)
	if len(triples) > 0 {
		sessionID := ""
		for sid := range sessionIDs {
			sessionID = sid
			break
		}
		if _, _, err := d.db.Commit(triples, sessionID, 0); err != nil {
			log.Printf("[memory] distill commit: %v", err)
			return false
		}
	}
	return true
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

func extractKeyTriples(userContent, assistantContent string, embedder nlp.Vectorizer) []memory.Triple {
	var triples []memory.Triple

	e := nlp.NewExtractor(nil)
	if embedder != nil {
		e.SetEmbedder(embedder)
	}
	text := userContent
	if assistantContent != "" {
		text += assistantContent
	}
	result := e.Extract(text)
	if result != nil {
		for _, nt := range result.Triples {
			mt := nlp.ToMemoryTriple(nt)
			if mt.Subject != "" && mt.Relation != "" && mt.Object != "" {
				triples = append(triples, mt)
			}
		}
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
	if data == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(data, "\n"), "\n")
}

func splitLine(line string) []string {
	if line == "" {
		return nil
	}
	return strings.SplitN(line, "\t", 5)
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
