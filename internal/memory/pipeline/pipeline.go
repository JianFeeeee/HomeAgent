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

	// persisted 表示该记录已经写在磁盘 raw 文件里。
	// 未导出：只影响本进程的落盘行为，不进 JSON。
	// 作用：flush 只写**尚未落盘**的记录，否则 loadExisting 载入的记录
	// 会被再写一份，重启后同一批记录从新旧两个文件各读回一次。
	persisted bool
}

type DistillerConfig struct {
	Interval      time.Duration `json:"interval"`
	RetentionDays int           `json:"retention_days"`
	BatchSize     int           `json:"batch_size"`
}

type Distiller struct {
	mu       sync.Mutex
	db       *memory.GraphDB
	rawPath  string
	records  []RawRecord
	nextID   int64
	cfg      DistillerConfig
	ctx      context.Context
	cancel   context.CancelFunc
	onMemory func(input, response string)
	embedder nlp.Vectorizer
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

// Stopped 报告蒸馏循环是否已被 Stop() 取消。
//
// Stop() 里的 cancel() 是同步生效的，所以本方法在 Stop() 返回后立即为 true，
// 不受循环 goroutine 何时退出的影响。启动自检、健康检查用它确认
// 「Start 之后没有被立即 Stop 掉」——历史回归：main() 拆分时
// initMemoryStack 里残留一句 defer distiller.Stop()，函数一返回就把刚起的
// 循环杀了，10min 心跳从不运行。Start() 之前返回 false（尚未被停）。
func (d *Distiller) Stopped() bool {
	select {
	case <-d.ctx.Done():
		return true
	default:
		return false
	}
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
	d.flushLocked()
}

// flushLocked 把**尚未落盘**的记录追加写入一个新的 raw 文件（原子写）。
//
// 只写 !persisted 的记录：loadExisting 载入的记录已经在磁盘上，若 flush 再把
// 它们整体重写一份，重启后同一批记录会同时从旧文件与新文件被读回，
// 实体 mention_count 与关系被重复蒸馏。
func (d *Distiller) flushLocked() {
	var pending []RawRecord
	for _, r := range d.records {
		if !r.persisted {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return
	}
	path := filepath.Join(d.rawPath, fmt.Sprintf("raw_%d.tsv", time.Now().UnixNano()))
	var sb strings.Builder
	for _, r := range pending {
		fmt.Fprintf(&sb, "%d\t%s\t%s\t%s\t%d\n", r.ID, r.SessionID, r.Role, r.Content, r.CreatedAt.Unix())
	}
	if err := writeFileAtomic(path, []byte(sb.String())); err != nil {
		log.Printf("[memory] flush error: %v", err)
		return
	}
	for i := range d.records {
		d.records[i].persisted = true
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
					persisted: true,
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
			// 成功即从磁盘 raw 文件里删除这些行，否则重启后 loadExisting
			// 会把它们当未蒸馏记录重新读回，每次启动重蒸同一批历史。
			d.removeRawRecords(toDistill[i:end])
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

// writeFileAtomic 写临时文件再 rename，避免进程在写一半时崩溃留下半个文件。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// rawKey 唯一标识一条原始记录，用于在 raw 文件里按内容定位并删除。
//
// 为什么按内容而不是 ID：loadExisting 读回记录时会重新分配连续 ID，
// 文件里的 ID 与内存 ID 并不一一对应。
func rawKey(session, role, content string, ts int64) string {
	return session + "\x00" + role + "\x00" + content + "\x00" + strconv.FormatInt(ts, 10)
}

// removeRawRecords 从磁盘 raw 文件中删除已成功蒸馏的记录。
//
// 蒸馏成功后记录若只从内存移除、磁盘文件不动，下次启动 loadExisting 会把
// 它们当未蒸馏记录重新读回，导致每次重启都重蒸同一批历史（实体
// mention_count 膨胀，且 distillBatch 的 sessionID 取自 map 首个键，
// 不确定性会放大重复）。
func (d *Distiller) removeRawRecords(batch []RawRecord) {
	if len(batch) == 0 {
		return
	}
	drop := make(map[string]bool, len(batch))
	for _, r := range batch {
		drop[rawKey(r.SessionID, r.Role, r.Content, r.CreatedAt.Unix())] = true
	}
	entries, err := os.ReadDir(d.rawPath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if ext != ".tsv" && ext != ".jsonl" {
			continue
		}
		path := filepath.Join(d.rawPath, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var kept []string
		removed := false
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if line == "" {
				continue
			}
			parts := splitLine(line)
			if len(parts) >= 5 {
				if ts, err := strconv.ParseInt(parts[4], 10, 64); err == nil &&
					drop[rawKey(parts[1], parts[2], parts[3], ts)] {
					removed = true
					continue
				}
			}
			kept = append(kept, line)
		}
		if !removed {
			continue
		}
		if len(kept) == 0 {
			os.Remove(path)
			continue
		}
		if err := writeFileAtomic(path, []byte(strings.Join(kept, "\n")+"\n")); err != nil {
			log.Printf("[memory] rewrite raw %s: %v", entry.Name(), err)
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

	// 注意：这里**不**做噪音过滤。对话蒸馏的抽取器（与 doc→graph 共用一个
	// NLP 提取器）历史上就没有常用词闸门：CutExact 那层当年只挂在 doc→graph 上。
	// 而且 pipeline_test 明确断言「我 --读书--> 杭州」必须被抽出（代词作主语是
	// 该路的既定行为）。要不要在对话路也拦常用词是行为决策，不在此处擅改，
	// 参见 memory.IsNoiseEntity 的说明。
	return triples
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
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
