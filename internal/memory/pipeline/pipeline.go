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
	cutoff := time.Now().AddDate(0, 0, -d.cfg.RetentionDays)
	type fileInfo struct {
		name string
		mod  time.Time
	}
	var files []fileInfo
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".jsonl" {
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

	// 提取对话中的关键信息，而不是直接 dump 原文
	// 规则1: "我的名字是X" / "我叫X" → (用户, 姓名, X)
	if name := extractName(userContent); name != "" {
		triples = append(triples, memory.Triple{Subject: "用户", Relation: "姓名", Object: name})
	}
	// 规则2: "我住在X" / "我家在X" → (用户, 居住地, X)
	if loc := extractLocation(userContent); loc != "" {
		triples = append(triples, memory.Triple{Subject: "用户", Relation: "居住地", Object: loc})
	}
	// 规则3: "我喜欢X" / "我爱X" → (用户, 喜好, X)
	if like := extractLike(userContent); like != "" {
		triples = append(triples, memory.Triple{Subject: "用户", Relation: "喜好", Object: like})
	}
	// 规则4: "我X岁" / "我的年龄是X" → (用户, 年龄, X)
	if age := extractAge(userContent); age != "" {
		triples = append(triples, memory.Triple{Subject: "用户", Relation: "年龄", Object: age})
	}
	// 规则5: "我的工作是X" / "我在X工作" → (用户, 职业, X)
	if job := extractJob(userContent); job != "" {
		triples = append(triples, memory.Triple{Subject: "用户", Relation: "职业", Object: job})
	}

	return triples
}

func extractName(s string) string {
	patterns := []struct {
		prefix string
		suffix string
	}{
		{"我叫", ""},
		{"我的名字是", ""},
		{"我是", ""},
		{"名字是", ""},
	}
	s = strings.TrimSpace(s)
	for _, p := range patterns {
		if strings.HasPrefix(s, p.prefix) {
			candidate := strings.TrimPrefix(s, p.prefix)
			if p.suffix != "" && strings.Contains(candidate, p.suffix) {
				candidate = candidate[:strings.Index(candidate, p.suffix)]
			}
			candidate = strings.TrimSpace(candidate)
			// 取第一个空格/逗号/句号前的内容
			for _, sep := range []string{"，", "。", " ", ","} {
				if idx := strings.Index(candidate, sep); idx > 0 {
					candidate = candidate[:idx]
				}
			}
			if len(candidate) > 0 && len(candidate) < 20 {
				return candidate
			}
		}
	}
	return ""
}

func extractLocation(s string) string {
	s = strings.TrimSpace(s)
	after := ""
	switch {
	case strings.HasPrefix(s, "我住在"):
		after = strings.TrimPrefix(s, "我住在")
	case strings.HasPrefix(s, "我家在"):
		after = strings.TrimPrefix(s, "我家在")
	case strings.HasPrefix(s, "我居住在"):
		after = strings.TrimPrefix(s, "我居住在")
	case strings.HasPrefix(s, "住在"):
		after = strings.TrimPrefix(s, "住在")
	default:
		return ""
	}
	for _, sep := range []string{"。", "，", " ", ","} {
		if idx := strings.Index(after, sep); idx > 0 {
			after = after[:idx]
		}
	}
	if len(after) > 0 && len(after) < 50 {
		return strings.TrimSpace(after)
	}
	return ""
}

func extractLike(s string) string {
	s = strings.TrimSpace(s)
	after := ""
	switch {
	case strings.HasPrefix(s, "我喜欢"):
		after = strings.TrimPrefix(s, "我喜欢")
	case strings.HasPrefix(s, "我爱"):
		after = strings.TrimPrefix(s, "我爱")
	case strings.HasPrefix(s, "我最喜欢"):
		after = strings.TrimPrefix(s, "我最喜欢")
	default:
		return ""
	}
	for _, sep := range []string{"。", "，", " ", ","} {
		if idx := strings.Index(after, sep); idx > 0 {
			after = after[:idx]
		}
	}
	if len(after) > 0 && len(after) < 50 {
		return strings.TrimSpace(after)
	}
	return ""
}

func extractAge(s string) string {
	s = strings.TrimSpace(s)
	after := ""
	switch {
	case strings.HasPrefix(s, "我"):
		rest := strings.TrimPrefix(s, "我")
		if strings.Contains(rest, "岁") {
			after = rest[:strings.Index(rest, "岁")]
		} else if strings.HasPrefix(rest, "的年龄是") {
			after = strings.TrimPrefix(rest, "的年龄是")
		} else {
			return ""
		}
	default:
		return ""
	}
	for _, sep := range []string{"。", "，", " ", ","} {
		if idx := strings.Index(after, sep); idx > 0 {
			after = after[:idx]
		}
	}
	if len(after) > 0 && len(after) < 5 {
		return strings.TrimSpace(after)
	}
	return ""
}

func extractJob(s string) string {
	s = strings.TrimSpace(s)
	after := ""
	switch {
	case strings.HasPrefix(s, "我的工作是"):
		after = strings.TrimPrefix(s, "我的工作是")
	case strings.HasPrefix(s, "我在"):
		rest := strings.TrimPrefix(s, "我在")
		if strings.Contains(rest, "工作") {
			after = rest[:strings.Index(rest, "工作")]
		} else {
			return ""
		}
	case strings.HasPrefix(s, "我是"):
		rest := strings.TrimPrefix(s, "我是")
		// "我是一个程序员" / "我是老师"
		for _, keyword := range []string{"一个", "一名", "一位"} {
			if strings.HasPrefix(rest, keyword) {
				rest = strings.TrimPrefix(rest, keyword)
				break
			}
		}
		// 职业通常较短，先看看
		after = rest
	default:
		return ""
	}
	for _, sep := range []string{"。", "，", " ", ",", "。"} {
		if idx := strings.Index(after, sep); idx > 0 {
			after = after[:idx]
		}
	}
	if len(after) > 0 && len(after) < 20 {
		return strings.TrimSpace(after)
	}
	return ""
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
