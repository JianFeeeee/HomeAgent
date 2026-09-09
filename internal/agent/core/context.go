package core

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

type ToolResultItem struct {
	Name   string `json:"name"`
	Output string `json:"output"`
}

type ContextEvent struct {
	// ID 是事件的稳定标识，媒体引用（media_refs.owner_id）挂在它上面。
	//
	// 惰性生成：只有真的要挂媒体时才赋值（见 bindEventMedia）。
	// 全量生成会让每条事件都多一个字段进 context.json，而绝大多数对话没有媒体。
	// omitempty 保证存量 context.json 读回来时该字段为空，不影响任何既有行为。
	ID          string           `json:"id,omitempty"`
	Timestamp   time.Time        `json:"timestamp"`
	Source      string           `json:"source"`
	Input       string           `json:"input"`
	Response    string           `json:"response,omitempty"`
	ToolsUsed   []string         `json:"tools_used,omitempty"`
	ToolResults []ToolResultItem `json:"tool_results,omitempty"`
	// --- 原生多模态记忆（v1.2.0）---
	// 媒体不是描述文本的附件，而是与文本同生命周期的记忆块。Vec 坐标在媒体
	// 首次进入 L0 时计算一次并存于 CAS；L0→L2→L3 只迁移 Media digest 引用，
	// 三层始终复用同一坐标。描述仅是可选的文本语义通道，不再决定媒体是否存在。
	Media     []string      `json:"media,omitempty"`
	Vector    vector.Vector `json:"-"` // 稀疏词向量（TF-IDF/fastText 空间）
	DenseVec  []float64     `json:"-"` // 稠密多模态向量（与媒体/文档共享空间）
}

const contextFlushInterval = 5 * time.Second

type RelevanceContext struct {
	mu               sync.Mutex
	events           []*ContextEvent
	embedder         *memory.StaticEmbedder
	denseSpace       vector.MultimodalEmbedder
	savePath         string
	saveTimer        *time.Timer
	dirty            bool
	toolDefLookup    func(name string) *sdk.ToolDef
	channelDefLookup func(name string) (sdk.ChannelDef, bool)

	// mediaStore 只用于 Prune 时把媒体引用从事件转给归档文档。
	// 为 nil 时引用转移静默跳过（媒体存储未启用）。
	mediaStore *media.Store
}

// SetMediaStore 注入媒体存储，供 L0→L2 归档时转移媒体引用。
func (c *RelevanceContext) SetMediaStore(s *media.Store) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mediaStore = s
}

// transferMediaRefs 把被归档事件的媒体引用转给目标文档（调用方已持 c.mu）。
//
// 先挂后销：若反序，引用计数会瞬时归零，此时若后台 GC 正在跑
// 就会把仍被记忆引用的内容当孤儿清掉。
func (c *RelevanceContext) transferMediaRefs(archive []scoredEvent, docID string) {
	if c.mediaStore == nil || docID == "" {
		return
	}
	for _, s := range archive {
		evt := s.event
		if evt == nil || evt.ID == "" || len(evt.Media) == 0 {
			continue
		}
		for _, d := range evt.Media {
			if err := c.mediaStore.AddRef(d, media.OwnerDocument, docID); err != nil {
				log.Printf("[media] 归档转移 AddRef 失败 (%s → doc %s): %v", shortDigest(d), docID, err)
			}
		}
		if _, err := c.mediaStore.DropOwner(media.OwnerContext, evt.ID); err != nil {
			log.Printf("[media] 归档转移 DropOwner 失败 (evt %s): %v", evt.ID, err)
		}
	}
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

// SetDenseSpace 注入稠密多模态向量空间。配置后 L0 相关性裁剪可用稠密向量
// 余弦（与媒体检索、文档检索共享同一空间），未配置时退化到稀疏词向量。
func (c *RelevanceContext) SetDenseSpace(ds vector.MultimodalEmbedder) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.denseSpace = ds
}

func (c *RelevanceContext) SetToolDefLookup(fn func(name string) *sdk.ToolDef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolDefLookup = fn
}

func (c *RelevanceContext) SetChannelDefLookup(fn func(name string) (sdk.ChannelDef, bool)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channelDefLookup = fn
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
		c.computeVector(evt)
	}
	c.events = events
}

func textForVector(evt *ContextEvent, toolDefLookup func(name string) *sdk.ToolDef, channelDefLookup func(name string) (sdk.ChannelDef, bool)) string {
	var text string
	switch {
	case evt.Source == "agent" && evt.Response != "":
		text = evt.Response
	case evt.Source == "cold_storage":
		text = evt.Input + " " + evt.Response
	default:
		text = evt.Input
	}

	// 计算层：应用输入通道的 Cleaner（不改原文，仅在计算层清洗）
	if channelDefLookup != nil {
		if chDef, ok := channelDefLookup(evt.Source); ok && chDef.Cleaner != nil {
			text = chDef.Cleaner(text)
		}
		if chDef, ok := channelDefLookup(evt.Source); ok && chDef.NoMemory {
			return ""
		}
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

// toolOutputClean 根据工具定义的 NoMemory/Cleaner 清洗输出，用于计算层。
// 返回 "" 表示跳过（NoMemory），否则返回清洗后文本（Cleaner 或原文）。
func (c *RelevanceContext) toolOutputClean(name, output string) string {
	if c.toolDefLookup == nil {
		return output
	}
	def := c.toolDefLookup(name)
	if def == nil {
		return output
	}
	if def.NoMemory {
		return ""
	}
	if def.Cleaner != nil {
		return def.Cleaner(output)
	}
	return output
}

// inputChannelClean 根据输入通道的 Def 清洗输入文本，用于计算层。
func (c *RelevanceContext) inputChannelClean(source, input string) string {
	if c.channelDefLookup == nil {
		return input
	}
	chDef, ok := c.channelDefLookup(source)
	if !ok {
		return input
	}
	if chDef.Cleaner != nil {
		return chDef.Cleaner(input)
	}
	return input
}

// channelCleanerForDoc 返回 ChannelCleaner，使 document 包在存档时能按来源查找 Cleaner。
func (c *RelevanceContext) channelCleanerForDoc() document.ChannelCleaner {
	if c.channelDefLookup == nil {
		return nil
	}
	return func(source string) func(string) string {
		chDef, ok := c.channelDefLookup(source)
		if !ok {
			return nil
		}
		return chDef.Cleaner
	}
}

func (c *RelevanceContext) computeVector(evt *ContextEvent) {
	text := textForVector(evt, c.toolDefLookup, c.channelDefLookup)
	if text == "" {
		return
	}
	// 稀疏向量始终计算（TF-IDF/fastText，退化时仍可用）
	evt.Vector = c.embedder.Vectorize(text)
	// 稠密向量仅在配置了多模态空间时计算
	if c.denseSpace != nil && c.denseSpace.Loaded() {
		if dv, err := c.denseSpace.VectorizeDense(text); err == nil {
			evt.DenseVec = dv
		}
	}
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

	c.computeVector(&evt)
	c.events = append(c.events, &evt)

	c.save()
}

func (c *RelevanceContext) InsertByTimestamp(evt ContextEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.computeVector(&evt)

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

// scoredEvent 是 Prune 里按相关度排序的事件。
//
// 提为包级类型（原先是 Prune 内的局部类型）：transferMediaRefs 需要
// 把待归档列表传进去，局部类型无法出现在方法签名上。
type scoredEvent struct {
	event *ContextEvent
	score float64
	idx   int
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

	// 优先使用稠密向量余弦（与媒体/文档共享空间）；退化到稀疏词向量。
	var queryDense []float64
	useDense := false
	if c.denseSpace != nil && c.denseSpace.Loaded() {
		if dv, err := c.denseSpace.VectorizeDense(currentInput); err == nil {
			queryDense = dv
			useDense = true
		}
	}
	queryVec := c.embedder.VectorizeClean(currentInput)

	scoredEvents := make([]scoredEvent, len(candidates))
	for i, evt := range candidates {
		var score float64
		if useDense && len(evt.DenseVec) == len(queryDense) {
			score = vector.DenseCosine(queryDense, evt.DenseVec)
		} else {
			score = vector.CosineSimilarity(queryVec, evt.Vector)
		}
		scoredEvents[i] = scoredEvent{event: evt, score: score, idx: i}
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
				Media:       append([]string(nil), s.event.Media...),
			}
		}
		doc, err := docStore.ContextToDoc("context_archived", entries, c.embedder, nil, c.toolOutputClean, c.channelCleanerForDoc())
		if err == nil && doc != nil {
			archived = len(entries)
			// 媒体引用随事件一起从 L0 转到 L2：先把引用挂到归档文档上，
			// 再注销原事件的引用。顺序不能反——先销后挂会让引用计数
			// 瞬时归零，若此时 GC 正在跑（后台任务）就会把仍被记忆引用的
			// 内容当孤儿清掉。
			c.transferMediaRefs(archive, doc.ID)
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
