package sdk

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"

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

// sdkBindText 把文本里引用的媒体挂到 owner 上，返回新挂上的条数。
//
// 短 digest 补全失败（内容已 GC、或前缀有歧义）就跳过那一条：挂一条对不上的
// 引用比不挂更糟——owner_kind/owner_id/digest 三者进了主键，digest 错了则
// DropOwner 永远匹配不到它，那是一条永久泄漏的引用。
//
// done 记录本次已处理过的 digest。AddRef 幂等，重复挂不会多出一条引用，
// 但会让计数虚高——文档路径先按附件挂一遍、再扫正文标记挂一遍，
// 同一份媒体会被数两次，日志里「绑定 2 个」而实际只有 1 条引用。
func sdkBindText(ms *media.Store, text, ownerKind, ownerID string, done map[string]bool) int {
	if ms == nil || text == "" || ownerID == "" {
		return 0
	}
	bound := 0
	for _, m := range sdkMarkerPattern.FindAllStringSubmatch(text, -1) {
		full, err := ms.ResolvePrefix(m[2])
		if err != nil {
			continue
		}
		if done != nil && done[full] {
			continue
		}
		if err := ms.AddRef(full, ownerKind, ownerID); err != nil {
			continue
		}
		if done != nil {
			done[full] = true
		}
		bound++
	}
	return bound
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

// Commit 把插件的三元组写入图库，并把三元组引用的媒体挂到句子上。
//
// 媒体的绑定链是 SentenceText → sentences 表 → sentence_id → media_refs。
// 旧实现丢掉 SentenceText 又走 Commit（不回 sentenceIDs），这条链一步都走不通：
// 插件即便按格式写好标记，媒体也永远挂不上。
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

// bindSentences 把每条句子里引用的媒体挂到该句子的 graph_sentence owner 上。
func (m *graphMemory) bindSentences(sentenceIDs map[string]int64) {
	if m.ms == nil || len(sentenceIDs) == 0 {
		return
	}
	bound := 0
	for text, sid := range sentenceIDs {
		if sid == 0 {
			continue
		}
		// 每条句子一个独立的 done 集：同一份媒体挂在不同句子上是两条
		// 合法引用（owner_id 不同），不该被跨句子去重。
		bound += sdkBindText(m.ms, text, media.OwnerGraphSentence,
			strconv.FormatInt(sid, 10), map[string]bool{})
	}
	if bound > 0 {
		log.Printf("[sdk media] 插件 %s 的三元组绑定 %d 个媒体引用", m.plugin, bound)
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
// 文本记忆是追加写 JSONL，没有稳定 owner_id 可挂 media_refs，所以媒体在这一层
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
		m.fillMedia(out[i])
	}
	return out
}

// fillMedia 填充文档的媒体字段。
//
// 优先用 media_refs（权威：谁挂上去的就是谁），为空时退回解析正文标记——
// 历史文档与经旧版插件写入的文档只有标记、没有引用。
func (m *docMemoryImpl) fillMedia(out *Doc) {
	if m.ms == nil {
		return
	}
	digests, err := m.ms.Refs(media.OwnerDocument, out.ID)
	if err != nil {
		log.Printf("[sdk media] 读取文档 %s 的媒体引用失败: %v", out.ID, err)
	}
	if len(digests) == 0 {
		out.Attachments = sdkAttachmentsFromText(m.ms, out.Content)
		for _, a := range out.Attachments {
			out.MediaDigests = append(out.MediaDigests, a.Digest)
		}
		return
	}
	out.MediaDigests = digests
	for _, d := range digests {
		it, err := m.ms.Stat(d)
		if err != nil || it == nil {
			continue
		}
		out.Attachments = append(out.Attachments, MediaAttachment{
			Digest: it.Digest, MIME: it.MIME, Description: it.Description,
		})
	}
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

	if err := m.ds.Insert(target); err != nil {
		return err
	}
	// 回填给调用方：ID 是新建时内核生成的，Content 含内核补的标记。
	d.ID = target.ID
	d.Content = target.Content

	m.bindDocMedia(target, digests)
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

// bindDocMedia 把附件与正文标记引用的媒体一起挂到文档 owner 上。
func (m *docMemoryImpl) bindDocMedia(target *doc.Doc, digests []string) {
	if m.ms == nil || target.ID == "" {
		return
	}
	bound := 0
	done := make(map[string]bool, len(digests))
	for _, full := range digests {
		if done[full] {
			continue
		}
		if err := m.ms.AddRef(full, media.OwnerDocument, target.ID); err != nil {
			log.Printf("[sdk media] 文档引用绑定失败 (%s → doc %s): %v",
				sdkShortDigest(full), target.ID, err)
			continue
		}
		done[full] = true
		bound++
	}
	// 插件手写在正文里的标记同样要挂上，否则那些媒体在文档里可见却无主。
	// 共用 done：附件刚挂过的那些是同一份媒体（内核自己把标记补进了正文）。
	bound += sdkBindText(m.ms, target.Content, media.OwnerDocument, target.ID, done)
	if bound > 0 {
		log.Printf("[sdk media] 插件 %s 写入文档 %s，绑定 %d 个媒体引用",
			m.plugin, target.ID, bound)
	}
}

// Remove 删除文档，同时释放它持有的媒体引用。
//
// 旧实现只删文档不解引用，于是那些媒体永久处于「被引用」状态：GC 不回收，
// 磁盘只增不减。内核的归档路径（distill 的 releaseDocMedia）做了这一步，
// 插件路径漏了同一步。
func (m *docMemoryImpl) Remove(id string) {
	if m.ds == nil {
		return
	}
	if m.ms != nil && id != "" {
		if n, err := m.ms.DropOwner(media.OwnerDocument, id); err != nil {
			log.Printf("[sdk media] 释放文档 %s 的媒体引用失败: %v", id, err)
		} else if n > 0 {
			log.Printf("[sdk media] 文档 %s 删除，释放 %d 个媒体引用", id, n)
		}
	}
	m.ds.Remove(id)
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
