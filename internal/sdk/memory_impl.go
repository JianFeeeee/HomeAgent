package sdk

import (
	"fmt"
	"log"
	"strconv"
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
//     而非 CommitWithMedia，于是 sentences 表没有落点，媒体块无从挂接；
//   - 知识库：Query 只回 ID/Title/Content，Insert 只写这三个，读写两个方向
//     都把媒体元数据裁掉。
//
// 现在的规则：内部结构有的字段一律透传；媒体一律变成一等记忆块。
// 媒体存储为 nil 时整条链路静默降级为纯文本行为（媒体是记忆增强，不是必需品）。

const sdkShortDigestLen = 12

func sdkShortDigest(d string) string {
	if len(d) > sdkShortDigestLen {
		return d[:sdkShortDigestLen]
	}
	return d
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

// sdkBlocksFromDigests 为显式 digest 列表构造一等块（去重）。
// digest 可以是短前缀，内部会先补全。
func sdkBlocksFromDigests(ms *media.Store, digests []string) []memory.MemoryBlock {
	if ms == nil || len(digests) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var blocks []memory.MemoryBlock
	for _, d := range digests {
		if d == "" {
			continue
		}
		full, err := ms.ResolvePrefix(d)
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
			MIME:       mime,
			Tool:       tool,
			OriginPath: a.Name,
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

// Commit 把插件的三元组写入图库，并把结构化 MediaDigests 变成 L3 一等块。
// 媒体通过 sentence --contains--> block 结构边挂接，不读写任何正文 marker。
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
			MediaDigests: t.MediaDigests,
		}
		ts = append(ts, mt)
	}

	sentenceIDs, _, _, err := m.db.CommitWithMedia(ts, "plugin", 0)
	if err != nil {
		return err
	}
	m.bindSentences(sentenceIDs, ts)
	return nil
}

// bindSentences 把每个三元组显式携带的媒体变成 L3 一等记忆块，
// 并以 sentence --contains--> block 结构边关联。
//
// 不再往句子文本里写 marker、也不再从文本反解 digest：归属由结构化字段直接给出。
func (m *graphMemory) bindSentences(sentenceIDs map[string]int64, triples []memory.Triple) {
	if m.ms == nil || m.db == nil || len(sentenceIDs) == 0 {
		return
	}
	bound := 0
	for _, t := range triples {
		if len(t.MediaDigests) == 0 {
			continue
		}
		sid := sentenceIDs[t.SentenceText]
		if sid == 0 {
			continue
		}
		for _, b := range sdkBlocksFromDigests(m.ms, t.MediaDigests) {
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

// Append 追加一条文本事件。
//
// 文本记忆是追加写 JSONL 的原始日志，只有字符串字段，没有块容器；
// 因此附件在这里无法结构化存下。不假装用文本标记承载媒体——
// 需要保存媒体请用文档记忆或图记忆（它们持有一等记忆块）。
func (m *textMemoryImpl) Append(evt TextEvent) error {
	if m.tm == nil {
		return nil
	}
	if len(evt.Attachments) > 0 {
		log.Printf("[sdk media] 插件 %s 向文本记忆追加了 %d 份附件，已忽略："+
			"文本层是字符串日志，不具备块存储；请改用文档/图记忆保存媒体", m.plugin, len(evt.Attachments))
	}
	return m.tm.Append(text.Event{
		Timestamp: evt.Timestamp, Source: evt.Role, Input: evt.Content, AgentID: evt.Channel,
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

// Query 检索文档，并从文档持有的一等块补齐媒体元数据。
// 只返回 digest/MIME，不返回字节或生成式描述；需要字节时按 digest 单取。
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
// 唯一的来源是文档直接持有的一等记忆块：媒体不靠正文标记、
// 也不靠任何生成的描述文本。CAS 只提供 MIME 等元数据。
func (m *docMemoryImpl) fillMedia(out *Doc, d *doc.Doc) {
	if m.ms == nil || d == nil {
		return
	}
	for _, b := range d.Blocks {
		if b.PayloadDigest == "" {
			continue
		}
		out.MediaDigests = append(out.MediaDigests, b.PayloadDigest)
		att := MediaAttachment{Digest: b.PayloadDigest, MIME: b.MIME}
		if it, err := m.ms.Stat(b.PayloadDigest); err == nil && it != nil {
			att.MIME = it.MIME
		}
		out.Attachments = append(out.Attachments, att)
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

// Insert 写入文档。
func (m *docMemoryImpl) Insert(d *Doc) error { return m.InsertWithMedia(d, nil) }

// InsertWithMedia 写入文档并关联媒体。
//
// 媒体直接成为文档持有的一等记忆块：落进 CAS 拿到 digest，
// 再变成块挂到文档上。不往正文写 marker——文档向量会融合这些块的
// 媒体向量（同一统一空间），图片按自己的向量被召回。
func (m *docMemoryImpl) InsertWithMedia(d *Doc, attachments []MediaAttachment) error {
	if m.ds == nil || d == nil {
		return nil
	}
	target := &doc.Doc{ID: d.ID, Summary: d.Title, Content: d.Content}
	if target.Source == "" {
		target.Source = "plugin:" + m.plugin
	}

	digests := m.storeAttachments(attachments)

	// 一等记忆块：文档直接持有块本身，CAS 只提供字节与向量。
	target.Blocks = appendBlocks(target.Blocks, sdkBlocksFromDigests(m.ms, digests))

	if err := m.ds.Insert(target); err != nil {
		return err
	}
	// 回填给调用方：ID 是新建时内核生成的。
	d.ID = target.ID
	return nil
}

// storeAttachments 把附件落库，返回全部完整 digest。
func (m *docMemoryImpl) storeAttachments(atts []MediaAttachment) []string {
	if m.ms == nil || len(atts) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var digests []string
	for _, a := range atts {
		full, err := sdkPutAttachment(m.ms, a, "plugin_doc:"+m.plugin)
		if err != nil {
			// 媒体存不进去不该让文档写入失败——它是记忆增强，不是文档必需品
			log.Printf("[sdk media] 插件 %s 文档附件入库失败: %v", m.plugin, err)
			continue
		}
		if seen[full] {
			continue
		}
		seen[full] = true
		digests = append(digests, full)
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
