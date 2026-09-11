package sdk

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	doc "gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
)

// 插件侧记忆接口的实现（SDK 桥接层）。
//
// 这一层原先的缺陷是**静默裁字段**：插件把 Triple / Doc 交进来，包装层只挑
// 自己认识的几个字段转成内部结构，其余丢弃且不报错。两侧都中招：
//   - 图记忆：丢 Confidence/SubjectType/ObjectType/SentenceText，又走 Commit
//     而非 CommitWithMedia，于是 sentences 表没有落点，媒体引用无从挂起；
//   - 知识库：Query 只回 ID/Title/Content，Insert 只写这三个，读写两个方向
//     都把媒体元数据裁掉；Remove 不解引用，媒体永久算「被引用」，GC 收不掉。
//
// 现在的规则：内部结构有的字段一律透传；媒体一律走标记格式并挂到对应 owner。
// 媒体存储为 nil 时整条链路静默降级为纯文本行为（媒体是记忆增强，不是必需品）。

// ---------- 媒体标记（本层内部） ----------
//
// 标记是媒体在**纯文本记忆**里的表示形式：
//
//	[image/png a1b2c3d4e5f6] 一张紫蓝红三色带图
//	 └ label   └ 短 digest    └ 描述
//
// 之所以必须借文本承载：Doc.Content、sentences.text、文本记忆的 Input 全是
// 字符串，没有字段能挂结构化数据。描述文本是持久的语义记忆（检索靠它），
// digest 是回到字节的钥匙（反查靠它）。
//
// 格式与内核侧 graphmedia.go 的 mediaSummaryForEvent 一致——两边必须能互读
// 对方写下的标记，否则插件写入的媒体在内核归档时挂不上引用，且不报错。

const sdkShortDigestLen = 12

// sdkMarkerPattern 拆解一条标记，捕获组依次为 label、短 digest、该行剩余描述。
// digest 放宽到 8-64 位以容忍完整 digest 手写的情况；描述取到行尾而非贪婪到底，
// 因为一条记忆可能挂多份媒体、各占一行。
var sdkMarkerPattern = regexp.MustCompile(`\[([^\[\]\s]+)\s+([0-9a-f]{8,64})\]([^\n]*)`)

func sdkShortDigest(d string) string {
	if len(d) > sdkShortDigestLen {
		return d[:sdkShortDigestLen]
	}
	return d
}

// sdkMarkerFor 为一份已入库的媒体生成标记行。查不到就返回空串——
// 媒体可能已被 GC 清掉，此时不该凭空造出一条指向虚无的标记。
func sdkMarkerFor(ms *media.Store, digest string) string {
	it, err := ms.Stat(digest)
	if err != nil || it == nil {
		return ""
	}
	label := string(it.Kind)
	if it.MIME != "" {
		label = it.MIME
	}
	if it.Description == "" {
		// 「已入库但还没描述」与「压根没有媒体」必须可区分：
		// 描述由后台循环异步补齐，占位符保证补齐前这份媒体也不会从文本里消失。
		return fmt.Sprintf("[%s %s] (未描述)", label, sdkShortDigest(digest))
	}
	return fmt.Sprintf("[%s %s] %s", label, sdkShortDigest(digest), it.Description)
}

// sdkDigestsIn 返回文本里出现过的短 digest 集合，用于避免重复追加标记。
func sdkDigestsIn(s string) map[string]bool {
	out := map[string]bool{}
	for _, m := range sdkMarkerPattern.FindAllStringSubmatch(s, -1) {
		out[m[2]] = true
	}
	return out
}

// sdkBlockSeq 保证块 ID 全局唯一：Graph 的 memory_blocks 以 id 为主键，
// 不同文档里序号相同的块会在 L2→L3 迁移时相互覆盖。
var sdkBlockSeq int64

func sdkNewBlockID() string {
	return fmt.Sprintf("blk_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&sdkBlockSeq, 1))
}

// sdkBlockModality 把 CAS 的媒体大类映射为一等记忆块的模态。
func sdkBlockModality(k media.Kind) memory.BlockModality {
	switch k {
	case media.KindImage:
		return memory.BlockImage
	case media.KindVideo:
		return memory.BlockVideo
	case media.KindAudio:
		return memory.BlockAudio
	default:
		return memory.BlockText
	}
}

// sdkBlockForDigest 把一份已入库的媒体变成一个一等记忆块。
// 块自身携带 digest/向量/ fingerprint；CAS 只提供字节与元数据，不参与生命周期。
func sdkBlockForDigest(ms *media.Store, digest string) (memory.MemoryBlock, bool) {
	it, err := ms.Stat(digest)
	if err != nil || it == nil {
		return memory.MemoryBlock{}, false
	}
	return memory.MemoryBlock{
		ID:            sdkNewBlockID(),
		Modality:      sdkBlockModality(it.Kind),
		PayloadDigest: it.Digest,
		MIME:          it.MIME,
		Size:          it.Size,
		Width:         it.Width,
		Height:        it.Height,
		Vector:        it.Vec,
		Fingerprint:   it.VecModel,
		Tool:          it.Tool,
		CreatedAt:     it.FirstSeen,
	}, true
}

// sdkBlocksFromText 把文本标记里的媒体变成一等块（去重）。
func sdkBlocksFromText(ms *media.Store, text string) []memory.MemoryBlock {
	if ms == nil || text == "" {
		return nil
	}
	seen := map[string]bool{}
	var blocks []memory.MemoryBlock
	for _, m := range sdkMarkerPattern.FindAllStringSubmatch(text, -1) {
		full, err := ms.ResolvePrefix(m[2])
		if err != nil || seen[full] {
			continue
		}
		seen[full] = true
		if b, ok := sdkBlockForDigest(ms, full); ok {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

// sdkBlocksFromDigests 为显式 digest 列表构造一等块（去重）。
func sdkBlocksFromDigests(ms *media.Store, digests []string) []memory.MemoryBlock {
	if ms == nil || len(digests) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var blocks []memory.MemoryBlock
	for _, d := range digests {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		if b, ok := sdkBlockForDigest(ms, d); ok {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

// sdkPutAttachment 把一份附件解析成完整 digest。
//
// 两种入口：带 Data 的是新内容（落进 CAS，相同字节自动去重）；
// 只给 Digest 的是引用已有内容（补全前缀即可）。两者都不给则无效。
func sdkPutAttachment(ms *media.Store, a MediaAttachment, tool string) (string, error) {
	if len(a.Data) > 0 {
		mime := a.MIME
		if mime == "" {
			mime = "application/octet-stream"
		}
		return ms.Put(a.Data, media.Item{
			MIME:        mime,
			Tool:        tool,
			OriginPath:  a.Name,
			Description: a.Description,
		})
	}
	if a.Digest == "" {
		return "", fmt.Errorf("附件既无 data 也无 digest")
	}
	full, err := ms.ResolvePrefix(a.Digest)
	if err != nil {
		return "", fmt.Errorf("digest %s: %w", a.Digest, err)
	}
	return full, nil
}

// sdkAttachmentsFromText 从文本标记反解出附件元数据（不含字节），
// 让插件不必自己写正则去认标记。
func sdkAttachmentsFromText(ms *media.Store, s string) []MediaAttachment {
	if ms == nil || s == "" {
		return nil
	}
	var out []MediaAttachment
	seen := map[string]bool{}
	for _, m := range sdkMarkerPattern.FindAllStringSubmatch(s, -1) {
		full, err := ms.ResolvePrefix(m[2])
		if err != nil || seen[full] {
			continue
		}
		seen[full] = true
		att := MediaAttachment{Digest: full, MIME: m[1], Description: strings.TrimSpace(m[3])}
		if it, err := ms.Stat(full); err == nil && it != nil {
			att.MIME = it.MIME
			if it.Description != "" {
				att.Description = it.Description
			}
		}
		out = append(out, att)
	}
	return out
}

// ---------- 图记忆 ----------

type graphMemory struct {
	db     *memory.GraphDB
	ms     *media.Store
	plugin string
}

func NewGraphMemory(db *memory.GraphDB) MemoryAPI { return &graphMemory{db: db} }

// NewGraphMemoryWithMedia 创建带媒体能力的图记忆包装。plugin 仅用于日志溯源。
func NewGraphMemoryWithMedia(plugin string, db *memory.GraphDB, ms *media.Store) MemoryAPI {
	return &graphMemory{db: db, ms: ms, plugin: plugin}
}

func (m *graphMemory) Recall(query []string, depth int) ([]Entity, []Relation, error) {
	if m.db == nil {
		return nil, nil, nil
	}
	result, err := m.db.Recall(query, nil, depth, "")
	if err != nil {
		return nil, nil, err
	}
	entities := make([]Entity, len(result.Entities))
	for i, e := range result.Entities {
		entities[i] = Entity{Name: e.Name, Type: e.Type, MentionCount: e.MentionCount}
	}
	// Confidence 此前被丢弃：插件拿不到置信度就无法判断一条关系可不可信，
	// 只能把所有召回结果等同对待。
	relations := make([]Relation, len(result.Relations))
	for i, r := range result.Relations {
		relations[i] = Relation{
			SourceName:   r.SourceName,
			TargetName:   r.TargetName,
			RelationType: r.RelationType,
			Confidence:   r.Confidence,
		}
	}
	return entities, relations, nil
}

// Commit 把插件的三元组写入图库，并把三元组句子里的媒体变成 L3 一等块。
//
// 媒体的落点链是 SentenceText → sentences 表 → sentence_id → 块边。
// 旧实现丢掉 SentenceText 又走 Commit（不回 sentenceIDs），这条链一步都走不通。
func (m *graphMemory) Commit(triples []Triple) error {
	if m.db == nil {
		return nil
	}
	ts := make([]memory.Triple, 0, len(triples))
	for _, t := range triples {
		mt := memory.Triple{
			Subject:      t.Subject,
			Relation:     t.Relation,
			Object:       t.Object,
			Confidence:   t.Confidence,
			SubjectType:  t.SubjectType,
			ObjectType:   t.ObjectType,
			SentenceText: t.SentenceText,
		}
		if len(t.MediaDigests) > 0 {
			mt.SentenceText = m.sentenceWithMedia(mt.SentenceText, t.MediaDigests)
		}
		ts = append(ts, mt)
	}

	sentenceIDs, _, _, err := m.db.CommitWithMedia(ts, "plugin", 0)
	if err != nil {
		return err
	}
	m.bindSentences(sentenceIDs)
	return nil
}

// sentenceWithMedia 保证句子文本里带有这些 digest 的媒体标记。
//
// 让插件填 MediaDigests 就够，不必知道标记格式——否则格式写错的后果是
// 引用静默挂不上。已出现过的 digest 不重复追加：插件可能既手写了标记又填了
// MediaDigests，重复标记会让同一份媒体产生两条一样的句子引用。
func (m *graphMemory) sentenceWithMedia(sentence string, digests []string) string {
	present := sdkDigestsIn(sentence)
	var add []string
	for _, d := range digests {
		if d == "" || present[sdkShortDigest(d)] {
			continue
		}
		if m.ms == nil {
			// 没有媒体存储时也把 digest 留在文本里：拿不到描述，
			// 但将来存储可用时这条记忆仍能反查回字节。
			add = append(add, fmt.Sprintf("[media %s] (未描述)", sdkShortDigest(d)))
			present[sdkShortDigest(d)] = true
			continue
		}
		full, err := m.ms.ResolvePrefix(d)
		if err != nil {
			log.Printf("[sdk media] 插件 %s 提交的 digest %s 无法解析: %v", m.plugin, d, err)
			continue
		}
		if line := sdkMarkerFor(m.ms, full); line != "" {
			add = append(add, line)
			present[sdkShortDigest(full)] = true
		}
	}
	if len(add) == 0 {
		return sentence
	}
	if sentence == "" {
		return strings.Join(add, "\n")
	}
	return sentence + "\n" + strings.Join(add, "\n")
}

// bindSentences 把每条句子里引用的媒体变成 L3 的一等记忆块，
// 并建立 sentence --contains--> block 的结构边。
// 不再写 media_refs：块本身就是图的一部分，不需要 owner 账本保活。
func (m *graphMemory) bindSentences(sentenceIDs map[string]int64) {
	if m.ms == nil || m.db == nil || len(sentenceIDs) == 0 {
		return
	}
	bound := 0
	for text, sid := range sentenceIDs {
		if sid == 0 {
			continue
		}
		for _, b := range sdkBlocksFromText(m.ms, text) {
			if err := m.db.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
				log.Printf("[sdk media] 插件 %s 写入 L3 记忆块失败: %v", m.plugin, err)
				continue
			}
			if err := m.db.AddMemoryBlockEdge("sentence", strconv.FormatInt(sid, 10), "block", b.ID, "contains"); err != nil {
				log.Printf("[sdk media] 插件 %s 建立句子→块边失败: %v", m.plugin, err)
				continue
			}
			bound++
		}
	}
	if bound > 0 {
		log.Printf("[sdk media] 插件 %s 的三元组写入 %d 个 L3 记忆块", m.plugin, bound)
	}
}

func (m *graphMemory) Introspect() (map[string]interface{}, error) {
	if m.db == nil {
		return map[string]interface{}{}, nil
	}
	return m.db.Introspect()
}

func (m *graphMemory) MergeEntities(source, target string) (int, error) {
	if m.db == nil {
		return 0, nil
	}
	return m.db.MergeEntities(source, target)
}

func (m *graphMemory) Purge(criteria map[string]string, mode string) (int, error) {
	if m.db == nil {
		return 0, nil
	}
	return m.db.Purge(criteria, mode)
}

func (m *graphMemory) GraphData() (map[string]interface{}, error) {
	if m.db == nil {
		return map[string]interface{}{}, nil
	}
	return m.db.GraphData()
}

// ---------- 文本记忆 ----------

type textMemoryImpl struct {
	tm     *text.Memory
	ms     *media.Store
	plugin string
}

func NewTextMemory(tm *text.Memory) TextMemoryAPI { return &textMemoryImpl{tm: tm} }

// NewTextMemoryWithMedia 创建带媒体能力的文本记忆包装。
func NewTextMemoryWithMedia(plugin string, tm *text.Memory, ms *media.Store) TextMemoryAPI {
	return &textMemoryImpl{tm: tm, ms: ms, plugin: plugin}
}

// Append 追加一条文本事件；带附件时把媒体标记并进正文。
//
// 文本记忆是追加写 JSONL，没有结构化块存储，所以媒体在这一层
// 只能以标记形式存在。这不是妥协——描述文本才是持久的语义记忆，blob 只是缓存。
func (m *textMemoryImpl) Append(evt TextEvent) error {
	if m.tm == nil {
		return nil
	}
	content := evt.Content
	if len(evt.Attachments) > 0 && m.ms != nil {
		var lines []string
		for _, a := range evt.Attachments {
			d, err := sdkPutAttachment(m.ms, a, "plugin_text:"+m.plugin)
			if err != nil {
				log.Printf("[sdk media] 插件 %s 文本附件入库失败: %v", m.plugin, err)
				continue
			}
			if line := sdkMarkerFor(m.ms, d); line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) > 0 {
			if content == "" {
				content = strings.Join(lines, "\n")
			} else {
				content += "\n" + strings.Join(lines, "\n")
			}
		}
	}
	return m.tm.Append(text.Event{
		Timestamp: evt.Timestamp, Source: evt.Role, Input: content, AgentID: evt.Channel,
	})
}

func (m *textMemoryImpl) RecentEvents(n int) ([]TextEvent, error) {
	if m.tm == nil {
		return nil, nil
	}
	got, err := m.tm.RecentEvents(n)
	if err != nil {
		return nil, err
	}
	out := make([]TextEvent, len(got))
	for i, e := range got {
		out[i] = TextEvent{
			Role: e.Source, Content: e.Input, Timestamp: e.Timestamp, Channel: e.AgentID,
			Attachments: sdkAttachmentsFromText(m.ms, e.Input),
		}
	}
	return out, nil
}

func (m *textMemoryImpl) Stats() map[string]interface{} {
	if m.tm == nil {
		return map[string]interface{}{}
	}
	return m.tm.Stats()
}

// ---------- 文档记忆（知识库） ----------

type docMemoryImpl struct {
	ds     *doc.Store
	ms     *media.Store
	plugin string
}

func NewDocMemory(ds *doc.Store) DocMemoryAPI { return &docMemoryImpl{ds: ds} }

// NewDocMemoryWithMedia 创建带媒体能力的文档记忆包装。
func NewDocMemoryWithMedia(plugin string, ds *doc.Store, ms *media.Store) DocMemoryAPI {
	return &docMemoryImpl{ds: ds, ms: ms, plugin: plugin}
}

// Query 检索文档，并补齐媒体元数据。
//
// 旧实现只回 ID/Title/Content，插件即便拿到一篇带媒体的文档也看不出这里有
// 几份媒体、分别是什么。现在同时给出完整 digest 列表与 mime+描述，
// 但**不回字节**：一次检索可能命中几十份媒体，全塞回去会把跨进程消息撑爆，
// 需要字节时按 digest 单取。
func (m *docMemoryImpl) Query(text string, topK int) []*Doc {
	if m.ds == nil {
		return nil
	}
	got := m.ds.Query(text, topK)
	out := make([]*Doc, len(got))
	for i, d := range got {
		out[i] = &Doc{ID: d.ID, Title: d.Summary, Content: d.Content}
		m.fillMedia(out[i], d)
	}
	return out
}

// fillMedia 填充文档的媒体字段。
//
// 优先读一等记忆块（文档直接持有），为空时退回解析正文标记——
// 历史文档与经旧版插件写入的文档只有标记、没有块。
func (m *docMemoryImpl) fillMedia(out *Doc, d *doc.Doc) {
	if m.ms == nil {
		return
	}
	if d != nil && len(d.Blocks) > 0 {
		for _, b := range d.Blocks {
			if b.PayloadDigest == "" {
				continue
			}
			out.MediaDigests = append(out.MediaDigests, b.PayloadDigest)
			att := MediaAttachment{Digest: b.PayloadDigest, MIME: b.MIME, Description: ""}
			if it, err := m.ms.Stat(b.PayloadDigest); err == nil && it != nil {
				att.MIME = it.MIME
				att.Description = it.Description
			}
			out.Attachments = append(out.Attachments, att)
		}
		return
	}
	out.Attachments = sdkAttachmentsFromText(m.ms, out.Content)
	for _, a := range out.Attachments {
		out.MediaDigests = append(out.MediaDigests, a.Digest)
	}
}

// appendBlocks 把新的块追加到已有块之后（按 digest 去重）。
func appendBlocks(existing []memory.MemoryBlock, add []memory.MemoryBlock) []memory.MemoryBlock {
	seen := make(map[string]bool, len(existing))
	for _, b := range existing {
		seen[b.PayloadDigest] = true
	}
	for _, b := range add {
		if b.PayloadDigest != "" && seen[b.PayloadDigest] {
			continue
		}
		existing = append(existing, b)
		if b.PayloadDigest != "" {
			seen[b.PayloadDigest] = true
		}
	}
	return existing
}

// Insert 写入文档。正文里已有的媒体标记会被挂成文档级引用，
// 避免插件写进来的媒体在下一次 GC 时被当作无主内容清掉。
func (m *docMemoryImpl) Insert(d *Doc) error { return m.InsertWithMedia(d, nil) }

// InsertWithMedia 写入文档并关联媒体。
//
// 标记由内核补进 Content——插件不必知道标记格式，也就不会因为格式写错导致
// 引用挂不上。补标记必须在 ds.Insert 之前完成：向量索引用 Summary+Content
// 计算，标记进不去正文就检索不到这份媒体。
func (m *docMemoryImpl) InsertWithMedia(d *Doc, attachments []MediaAttachment) error {
	if m.ds == nil || d == nil {
		return nil
	}
	target := &doc.Doc{ID: d.ID, Summary: d.Title, Content: d.Content}
	if target.Source == "" {
		target.Source = "plugin:" + m.plugin
	}

	digests := m.storeAttachments(attachments, &target.Content)

	// 一等记忆块：文档直接持有块本身，CAS 只提供字节与向量。
	// 不再写 media_refs——块随文档一同存活或被删除，无需 owner 账本。
	target.Blocks = appendBlocks(target.Blocks,
		append(sdkBlocksFromDigests(m.ms, digests), sdkBlocksFromText(m.ms, target.Content)...))

	if err := m.ds.Insert(target); err != nil {
		return err
	}
	// 回填给调用方：ID 是新建时内核生成的，Content 含内核补的标记。
	d.ID = target.ID
	d.Content = target.Content
	return nil
}

// storeAttachments 把附件落库并把标记追加进 content，返回全部完整 digest。
func (m *docMemoryImpl) storeAttachments(atts []MediaAttachment, content *string) []string {
	if m.ms == nil || len(atts) == 0 {
		return nil
	}
	present := sdkDigestsIn(*content)
	var digests, lines []string
	for _, a := range atts {
		full, err := sdkPutAttachment(m.ms, a, "plugin_doc:"+m.plugin)
		if err != nil {
			// 媒体存不进去不该让文档写入失败——它是记忆增强，不是文档必需品
			log.Printf("[sdk media] 插件 %s 文档附件入库失败: %v", m.plugin, err)
			continue
		}
		digests = append(digests, full)
		if present[sdkShortDigest(full)] {
			continue // 插件自己写了标记，不重复追加
		}
		present[sdkShortDigest(full)] = true
		if line := sdkMarkerFor(m.ms, full); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > 0 {
		if *content == "" {
			*content = strings.Join(lines, "\n")
		} else {
			*content += "\n" + strings.Join(lines, "\n")
		}
	}
	return digests
}

// Remove 删除文档，并删除它持有的一等块所对应的内容（无其他块共享时）。
//
// 与文本块一致：删除块即删除内容。媒体字节是块的内容存储，
// 不单独做引用计数或 GC。
func (m *docMemoryImpl) Remove(id string) {
	if m.ds == nil {
		return
	}
	var digests []string
	if d := m.ds.Get(id); d != nil {
		for _, b := range d.Blocks {
			if b.PayloadDigest != "" {
				digests = append(digests, b.PayloadDigest)
			}
		}
	}
	m.ds.Remove(id)
	if m.ms == nil {
		return
	}
	// 仍被其它文档持有的 digest 不能删（同一份字节可能被多个块共享）。
	stillHeld := map[string]bool{}
	for _, b := range m.ds.Blocks() {
		stillHeld[b.PayloadDigest] = true
	}
	for _, d := range digests {
		if stillHeld[d] {
			continue
		}
		if err := m.ms.Delete(d); err != nil {
			log.Printf("[sdk media] 删除文档 %s 的内容失败 %s: %v", id, sdkShortDigest(d), err)
		}
	}
}

func (m *docMemoryImpl) Stats() map[string]interface{} {
	if m.ds == nil {
		return map[string]interface{}{}
	}
	return m.ds.Stats()
}

var _ MemoryAPI = (*graphMemory)(nil)
var _ TextMemoryAPI = (*textMemoryImpl)(nil)
var _ DocMemoryAPI = (*docMemoryImpl)(nil)
