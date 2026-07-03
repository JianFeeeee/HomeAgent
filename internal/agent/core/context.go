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

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

// ContextEvent — 单条上下文事件
type ContextEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source"`
	Input     string    `json:"input"`
	Response  string    `json:"response,omitempty"`
	ToolsUsed []string  `json:"tools_used,omitempty"`
	Vector    vector.Vector `json:"-"` // 缓存向量，避免重复计算
}

// RelevanceContext — 基于相关性的上下文管理，非固定阈值
type RelevanceContext struct {
	mu       sync.Mutex
	events   []*ContextEvent
	veczer   *vector.TFIDFVectorizer
	trained  bool
	savePath string // 持久化路径，空则不持久化
}

func NewRelevanceContext(savePath string) *RelevanceContext {
	rc := &RelevanceContext{
		veczer:   vector.NewTFIDFVectorizer(2),
		savePath: savePath,
	}
	if savePath != "" {
		rc.load()
	}
	return rc
}

// load 从文件恢复上下文事件
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
		evt.Vector = c.veczer.Vectorize(evt.Input + " " + evt.Response)
	}
	c.events = events
}

// Save 持久化上下文事件到文件
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

	evt.Vector = c.veczer.Vectorize(evt.Input + " " + evt.Response)
	c.events = append(c.events, &evt)

	// 增量训练向量化器
	c.trained = false

	c.save()
}

// save 无锁版本，Append/Prune 内部持有锁时调用
func (c *RelevanceContext) save() error {
	if c.savePath == "" {
		return nil
	}
	data, err := json.Marshal(c.events)
	if err != nil {
		return err
	}
	return os.WriteFile(c.savePath, data, 0644)
}

// Prune — 基于当前输入计算每条上下文的相关性，归档最不相关的
// 返回被归档的事件（转为文档），保留 topK 个最相关的在活跃上下文中
func (c *RelevanceContext) Prune(currentInput string, topK int, docStore *document.Store) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.events) <= topK {
		return 0
	}

	// 确保向量化器已训练
	c.ensureTrained()

	queryVec := c.veczer.Vectorize(currentInput)

	// 计算每条上下文与当前输入的相关性
	type scored struct {
		event *ContextEvent
		score float64
		idx   int
	}
	scoredEvents := make([]scored, len(c.events))
	for i, evt := range c.events {
		score := vector.CosineSimilarity(queryVec, evt.Vector)
		scoredEvents[i] = scored{event: evt, score: score, idx: i}
	}

	// 按相关性从高到低排序
	sort.Slice(scoredEvents, func(i, j int) bool {
		return scoredEvents[i].score > scoredEvents[j].score
	})

	// 保留 topK 最相关的
	keep := scoredEvents
	if len(keep) > topK {
		keep = keep[:topK]
	}
	archive := scoredEvents[topK:]

	// 重建 events 为保留的
	c.events = make([]*ContextEvent, len(keep))
	for i, s := range keep {
		c.events[i] = s.event
	}

	// 按时间重新排序
	sort.Slice(c.events, func(i, j int) bool {
		return c.events[i].Timestamp.Before(c.events[j].Timestamp)
	})

	// 归档到文档记忆
	archived := 0
	if docStore != nil && len(archive) > 0 {
		entries := make([]document.ContextEntry, len(archive))
		for i, s := range archive {
			entries[i] = document.ContextEntry{
				Timestamp: s.event.Timestamp,
				Source:    s.event.Source,
				Content:   s.event.Input,
				Response:  s.event.Response,
			}
		}
		doc, err := docStore.ContextToDoc("context_archived", entries)
		if err == nil && doc != nil {
			archived = len(archive)
		}
	}

	c.save()

	return archived
}

// Format — 输出活跃上下文的文本，用于注入 prompt
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

// Recent — 返回最近 n 条
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

// Len — 当前上下文事件数
func (c *RelevanceContext) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func (c *RelevanceContext) ensureTrained() {
	if !c.trained && len(c.events) > 0 {
		texts := make([]string, len(c.events))
		for i, evt := range c.events {
			texts[i] = evt.Input + " " + evt.Response
		}
		c.veczer.Train(texts)
		c.trained = true
	}
}
