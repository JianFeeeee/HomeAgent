package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

type ToolResultItem struct {
	Name   string `json:"name"`
	Output string `json:"output"`
}

type ContextEvent struct {
	Timestamp   time.Time         `json:"timestamp"`
	Source      string            `json:"source"`
	Input       string            `json:"input"`
	Response    string            `json:"response,omitempty"`
	ToolsUsed   []string          `json:"tools_used,omitempty"`
	ToolResults []ToolResultItem  `json:"tool_results,omitempty"`
	Vector      vector.Vector     `json:"-"`
}

const contextFlushInterval = 5 * time.Second

type RelevanceContext struct {
	mu            sync.Mutex
	events        []*ContextEvent
	embedder      *memory.StaticEmbedder
	savePath      string
	saveTimer     *time.Timer
	dirty         bool
	toolDefLookup func(name string) *sdk.ToolDef
}

func NewRelevanceContext(savePath string, embedder *memory.StaticEmbedder) *RelevanceContext {
	rc := &RelevanceContext{
		embedder: embedder,
		savePath: savePath,
	}
	if savePath != "" {
		rc.load()
	}
	return rc
}

func (c *RelevanceContext) SetToolDefLookup(fn func(name string) *sdk.ToolDef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolDefLookup = fn
}

func (c *RelevanceContext) load() {
	data, err := os.ReadFile(c.savePath)
	if err != nil {
		return
	}
	var events []*ContextEvent
	if err := json.Unmarshal(data, &events); err != nil {
		return
	}
	for _, evt := range events {
		evt.Vector = c.computeVector(evt)
	}
	c.events = events
}

func textForVector(evt *ContextEvent, toolDefLookup func(name string) *sdk.ToolDef) string {
	var text string
	switch {
	case evt.Source == "agent" && evt.Response != "":
		text = evt.Response
	case evt.Source == "cold_storage":
		text = evt.Input + " " + evt.Response
	default:
		text = evt.Input
	}

	// 计算层：附加工具输出，NoMemory 跳过，其余经 Cleaner 过滤
	if toolDefLookup != nil {
		noMemory := make(map[string]bool)
		for _, tr := range evt.ToolResults {
			def := toolDefLookup(tr.Name)
			if def != nil && def.NoMemory {
				noMemory[tr.Name] = true
			}
		}
		for _, tr := range evt.ToolResults {
			if noMemory[tr.Name] {
				continue
			}
			cleaned := tr.Output
			def := toolDefLookup(tr.Name)
			if def != nil && def.Cleaner != nil {
				cleaned = def.Cleaner(cleaned)
			}
			text += " " + cleaned
		}
	}

	return memory.CleanText(text)
}

func (c *RelevanceContext) computeVector(evt *ContextEvent) vector.Vector {
	return c.embedder.Vectorize(textForVector(evt, c.toolDefLookup))
}

func (c *RelevanceContext) Save() error {
	if c.savePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.savePath), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(c.events)
	if err != nil {
		return err
	}
	return os.WriteFile(c.savePath, data, 0644)
}

func (c *RelevanceContext) Append(evt ContextEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	evt.Vector = c.computeVector(&evt)
	c.events = append(c.events, &evt)

	c.save()
}

func (c *RelevanceContext) InsertByTimestamp(evt ContextEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	evt.Vector = c.computeVector(&evt)

	idx := sort.Search(len(c.events), func(i int) bool {
		return c.events[i].Timestamp.After(evt.Timestamp)
	})

	c.events = append(c.events, nil)
	copy(c.events[idx+1:], c.events[idx:])
	c.events[idx] = &evt

	c.save()
}

func (c *RelevanceContext) save() error {
	if c.savePath == "" {
		return nil
	}
	if !c.dirty {
		c.dirty = true
		if c.saveTimer == nil {
			c.saveTimer = time.AfterFunc(contextFlushInterval, c.flush)
		} else {
			c.saveTimer.Reset(contextFlushInterval)
		}
	}
	return nil
}

func (c *RelevanceContext) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return
	}
	data, err := json.Marshal(c.events)
	if err != nil {
		return
	}
	if err := os.WriteFile(c.savePath, data, 0644); err != nil {
		return
	}
	c.dirty = false
}

func (c *RelevanceContext) Prune(currentInput string, topK int, docStore *document.Store) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.events) <= topK {
		return 0
	}

	pCount := 10
	if pCount > len(c.events) {
		pCount = len(c.events)
	}
	protected := c.events[len(c.events)-pCount:]
	candidates := c.events[:len(c.events)-pCount]

	if len(candidates) == 0 {
		return 0
	}

	queryVec := c.embedder.VectorizeClean(currentInput)

	type scored struct {
		event *ContextEvent
		score float64
		idx   int
	}
	scoredEvents := make([]scored, len(candidates))
	for i, evt := range candidates {
		score := vector.CosineSimilarity(queryVec, evt.Vector)
		scoredEvents[i] = scored{event: evt, score: score, idx: i}
	}

	sort.Slice(scoredEvents, func(i, j int) bool {
		return scoredEvents[i].score > scoredEvents[j].score
	})

	keepCount := topK - len(protected)
	if keepCount < 0 {
		keepCount = 0
	}
	keep := scoredEvents
	if len(keep) > keepCount {
		keep = keep[:keepCount]
	}
	archive := scoredEvents[keepCount:]

	c.events = make([]*ContextEvent, 0, len(keep)+len(protected))
	for _, s := range keep {
		c.events = append(c.events, s.event)
	}
	c.events = append(c.events, protected...)

	sort.Slice(c.events, func(i, j int) bool {
		return c.events[i].Timestamp.Before(c.events[j].Timestamp)
	})

	archived := 0
	if docStore != nil && len(archive) > 0 {
		entries := make([]document.ContextEntry, len(archive))
		for i, s := range archive {
			entries[i] = document.ContextEntry{
				Timestamp:   s.event.Timestamp,
				Source:      s.event.Source,
				Content:     s.event.Input,
				Response:    s.event.Response,
				ToolResults: convertToolResults(s.event.ToolResults),
			}
		}
		doc, err := docStore.ContextToDoc("context_archived", entries, c.embedder)
		if err == nil && doc != nil {
			archived = len(entries)
		}
	}

	c.save()

	return archived
}

func (c *RelevanceContext) Format() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.events) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("【近期事件】\n")
	for _, e := range c.events {
		sb.WriteString(fmt.Sprintf("[%s] %s: %s", e.Timestamp.Format("15:04:05"), e.Source, e.Input))
		if e.Response != "" {
			sb.WriteString(fmt.Sprintf(" → %s", truncateStr(e.Response, 80)))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func (c *RelevanceContext) Recent(n int) []ContextEvent {
	c.mu.Lock()
	defer c.mu.Unlock()

	if n <= 0 || n > len(c.events) {
		n = len(c.events)
	}
	result := make([]ContextEvent, n)
	for i, evt := range c.events[len(c.events)-n:] {
		result[i] = *evt
	}
	return result
}

func (c *RelevanceContext) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func convertToolResults(items []ToolResultItem) []document.ToolResultItem {
	if items == nil {
		return nil
	}
	result := make([]document.ToolResultItem, len(items))
	for i, item := range items {
		result[i] = document.ToolResultItem{Name: item.Name, Output: item.Output}
	}
	return result
}


