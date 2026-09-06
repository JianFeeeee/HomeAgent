package core

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
)

// L3 图库的媒体引用绑定。
//
// 设计定位（方案 A：只做引用，不建媒体实体节点）：
// 图库里的实体与关系全部来自**描述文本**的 NLP 提取——媒体描述经
// mediaSummaryForEvent 进了 L0 事件的 Input，随归档进 L2 文档的 Content，
// 蒸馏时提取器自然会从描述文字里抽出实体和关系。
//
// 为何不把媒体本身建成实体节点：节点名只能从描述里取，而描述会被重新生成
// （换个视觉模型、补一次描述，名字就变了），于是同一张图会在图谱上留下
// 多个语义模糊的节点。检索能力靠描述文本已经具备，多这类节点只是噪声。
//
// 那么图库侧还需要什么：**反查**。图库里的句子写着「[image a1b2c3d4e5f6]
// 一张紫蓝红三色带图」，要能从这条句子找回那份字节。这就是
// media_refs 的 graph_sentence owner 的用途，也是这一层唯一要做的事。

// mediaDigestPattern 匹配事件摘要里的媒体标记 [<mime或kind> <短digest>]。
//
// 与 mediaSummaryForEvent 的输出格式对应。短 digest 是 12 位十六进制
// （shortDigest 的截断长度），这里放宽到 8-64 位以容忍将来调整截断长度，
// 以及有人手写了完整 digest 的情况。
var mediaDigestPattern = regexp.MustCompile(`\[[^\[\]]*?\b([0-9a-f]{8,64})\]`)

// mediaMarkerPattern 完整拆解一条媒体标记及其后跟的描述，
// 捕获组依次为：标签（mime 或 kind）、短 digest、该行剩余的描述文本。
//
// 与 mediaSummaryForEvent 的输出格式严格对应：
//
//	[image/png a1b2c3d4e5f6] 一张紫蓝红三色带图
//
// 描述取到行尾而非贪婪到底：一条事件可能挂多个媒体，各占一行。
var mediaMarkerPattern = regexp.MustCompile(`\[([^\[\]\s]+)\s+([0-9a-f]{8,64})\]([^\n]*)`)

// mediaMarker 是从文档正文里解析出的一条媒体标记。
type mediaMarker struct {
	label       string // mime 或 kind，如 image/png
	shortDigest string
	description string
	raw         string // 原始整段，用作三元组的 SentenceText
}

// parseMediaMarkers 从文本里解析全部媒体标记。
//
// 为何需要它而不只是 extractMediaDigests：媒体入 L3 曾完全依赖 NLP 提取器
// 碰巧从描述文本里提出合规三元组——实测 LLM 的 477 字图片描述只产出
// 「水平 -分割-> 成」这种语法碎片，obj 仅 1 字被 validEntityName 拒掉，
// 于是整条媒体记忆进不了图库。而媒体自身的信息（digest / mime / 描述）
// 是确定的，不该受提取器运气支配。
func parseMediaMarkers(text string) []mediaMarker {
	if text == "" {
		return nil
	}
	ms := mediaMarkerPattern.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ms))
	var out []mediaMarker
	for _, m := range ms {
		d := m[2]
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, mediaMarker{
			label:       m[1],
			shortDigest: d,
			description: strings.TrimSpace(m[3]),
			raw:         strings.TrimSpace(m[0]),
		})
	}
	return out
}

// mediaEntityName 是媒体在图库里的实体名。
//
// 形如「图片 a1b2c3d4e5f6」。刻意用 digest 而非描述文本构成名字：
// 描述会被重新生成（换视觉模型、补描述），若名字取自描述，同一张图
// 就会在图谱上留下多个节点。digest 不变则名字不变。
// 长度也天然合规（validEntityName 要求 2–50 字符）。
func mediaEntityName(label, shortDigest string) string {
	kind := "媒体"
	switch {
	case strings.HasPrefix(label, "image"):
		kind = "图片"
	case strings.HasPrefix(label, "audio"):
		kind = "音频"
	case strings.HasPrefix(label, "video"):
		kind = "视频"
	}
	return kind + " " + shortDigest
}

// mediaTriplesFromText 为文本里的每条媒体标记产出确定的三元组。
//
// 这是媒体进 L3 的可靠路径：不经过 NLP 提取器，因此不受它对描述性文本
// 提取能力的影响。每条媒体至少产出一条「<媒体实体> -内容-> <描述摘要>」，
// 且 SentenceText 用原始标记段，保证 bindSentenceMedia 的正则必然能
// 反解到 digest——绑定从概率事件变成确定行为。
//
// 描述摘要截到 40 字：validEntityName 上限 50 字符，留出余量；
// 图谱节点名过长会让可视化和实体合并都难以处理，完整描述留在
// SentenceText 与 media 表里。
func mediaTriplesFromText(text string) []memory.Triple {
	markers := parseMediaMarkers(text)
	if len(markers) == 0 {
		return nil
	}
	var out []memory.Triple
	for _, m := range markers {
		name := mediaEntityName(m.label, m.shortDigest)

		// 类型三元组恒可产出，不依赖描述是否存在
		out = append(out, memory.Triple{
			Subject:      name,
			SubjectType:  "Media",
			Relation:     "类型",
			Object:       m.label,
			ObjectType:   "MimeType",
			Confidence:   1.0,
			SentenceText: m.raw,
		})

		desc := summarizeForEntity(m.description, 40)
		if desc == "" {
			continue
		}
		out = append(out, memory.Triple{
			Subject:      name,
			SubjectType:  "Media",
			Relation:     "内容",
			Object:       desc,
			ObjectType:   "Description",
			Confidence:   1.0,
			SentenceText: m.raw,
		})
	}
	return out
}

// summarizeForEntity 把描述压成可作实体名的短串。
//
// 取首个句子边界之前的内容，再按 rune 截断——直接按字节截会切坏 UTF-8，
// 图库里就会出现乱码实体名。空白与 Markdown 强调符号一并清掉，
// 否则「**整体构成**」这类标记会进实体名。
func summarizeForEntity(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.NewReplacer("**", "", "*", "", "\n", " ", "\t", " ").Replace(s)
	for _, sep := range []string{"。", "；", "，", ". ", "; "} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
			break
		}
	}
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > maxRunes {
		r = r[:maxRunes]
	}
	out := strings.TrimSpace(string(r))
	// 太短的残片（如单字）过不了 validEntityName，直接放弃比写进去更好
	if len([]rune(out)) < 2 {
		return ""
	}
	return out
}

// extractMediaDigests 从文本里找出所有媒体标记的 digest。
//
// 为何靠正则从文本反解，而不是让三元组结构携带 digest：三元组是 NLP
// 提取器从纯文本产出的（nlp.ToMemoryTriple 只填 Subject/Relation/Object/
// Confidence/SentenceText），提取链路上没有任何位置能塞进结构化的 digest。
// 若要贯通就得改 internal/nlp 的整条数据流——而媒体标记本身就是我们
// 自己按固定格式写进文本的，反解是这里最省的可靠做法。
func extractMediaDigests(text string) []string {
	if text == "" {
		return nil
	}
	matches := mediaDigestPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var out []string
	for _, m := range matches {
		d := m[1]
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// bindSentenceMedia 把句子文本里提到的媒体挂到对应的 sentences.id 上。
//
// sentenceIDs 来自 GraphDB.CommitWithMedia：句子文本 → sentences.id。
// 只处理本次真正写入了 sentences 表的句子，避免给历史句子重复挂引用
// （AddRef 幂等，重复挂不会涨计数，但白跑 SQL）。
//
// 返回实际绑定成功的引用数，这是调用方的安全依据：归档路径靠它判定
// 「引用真的转移到图库了吗」，不能用「Commit 没报错」代替——Commit 会
// 静默跳过实体名不合法（validEntityName 要求 2–50 字符）的三元组，
// 于是「无错但一条也没写进去」是真实会发生的：LLM 生成的长描述提不出
// 合规实体名，实测 456 字描述得到 0 entities 0 relations。
func (a *Agent) bindSentenceMedia(sentenceIDs map[string]int64) int {
	if a.mediaStore == nil || len(sentenceIDs) == 0 {
		return 0
	}

	bound := 0
	for text, sid := range sentenceIDs {
		if sid == 0 {
			continue
		}
		digests := extractMediaDigests(text)
		if len(digests) == 0 {
			continue
		}
		ownerID := strconv.FormatInt(sid, 10)
		for _, short := range digests {
			// 文本里是短 digest，media_refs 的主键要完整 digest。
			// 补全失败（内容已被 GC 清掉、或前缀有歧义）就跳过——
			// 挂一条对不上的引用比不挂更糟：DropOwner 永远匹配不到它。
			full, err := a.mediaStore.ResolvePrefix(short)
			if err != nil {
				continue
			}
			if err := a.mediaStore.AddRef(full, media.OwnerGraphSentence, ownerID); err != nil {
				log.Printf("[media] 句子引用绑定失败 (%s → sentence %s): %v", short, ownerID, err)
				continue
			}
			bound++
		}
	}
	if bound > 0 {
		log.Printf("[media] L3 图库绑定 %d 个媒体引用", bound)
	}
	return bound
}

// sentenceWithMediaMarkers 保证句子文本里带上这些 digest 的媒体标记。
//
// 存在的理由：媒体的绑定链是 SentenceText → sentences 表 → sentence_id →
// media_refs。模型只知道 digest（从 memory_recall 的「关联媒体」或对话里的
// 媒体标记读到），不该要求它自己按内核格式拼标记——格式写错的后果是引用
// 静默挂不上，模型也无从察觉。
//
// 已出现过的 digest 不重复追加：模型可能既写了标记又填了 media_digests。
func (a *Agent) sentenceWithMediaMarkers(sentence string, digests []string) string {
	if a.mediaStore == nil || len(digests) == 0 {
		return sentence
	}
	present := make(map[string]bool)
	for _, d := range extractMediaDigests(sentence) {
		present[d] = true
	}

	var add []string
	for _, d := range digests {
		if d == "" || present[shortDigest(d)] {
			continue
		}
		// 模型给的多半是短 digest（它在上下文里看到的就是短的），补全成完整
		// digest 才能进 media_refs 主键。补不上就跳过：内容可能已被 GC 清掉。
		full, err := a.mediaStore.ResolvePrefix(d)
		if err != nil {
			log.Printf("[media] 模型提交的 digest %s 无法解析: %v", d, err)
			continue
		}
		if line := a.mediaMarkerLine(full); line != "" {
			add = append(add, line)
			present[shortDigest(full)] = true
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

// docMediaContext 为一篇文档产出媒体说明，供 doc_query 拼进工具返回值。
//
// 优先读 media_refs（权威：谁挂上去的就是谁），为空时退回解析正文标记——
// 历史文档与经旧版路径写入的文档只有标记、没有引用。
func (a *Agent) docMediaContext(docID, content string) string {
	if a.mediaStore == nil {
		return ""
	}
	digests, err := a.mediaStore.Refs(media.OwnerDocument, docID)
	if err != nil {
		log.Printf("[media] 读取文档 %s 的媒体引用失败: %v", docID, err)
	}
	if len(digests) == 0 {
		for _, short := range extractMediaDigests(content) {
			full, err := a.mediaStore.ResolvePrefix(short)
			if err != nil {
				continue
			}
			digests = append(digests, full)
		}
	}
	var lines []string
	for _, d := range digests {
		if line := a.mediaMarkerLine(d); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "；")
}

// resolveMediaDigests 把模型给的（多为短）digest 补全成完整 digest。
//
// 补不上就丢弃那一条并记日志：模型可能凭印象编了个 digest，也可能内容已被
// 容量 GC 淘汰。挂一条对不上的引用比不挂更糟——digest 进了 media_refs 主键，
// 错了则 DropOwner 永远匹配不到它，那是一条永久泄漏的引用。
func (a *Agent) resolveMediaDigests(digests []string) []string {
	if a.mediaStore == nil || len(digests) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(digests))
	var out []string
	for _, d := range digests {
		full, err := a.mediaStore.ResolvePrefix(d)
		if err != nil {
			log.Printf("[media] 模型给的 digest %s 无法解析: %v", d, err)
			continue
		}
		if seen[full] {
			continue
		}
		seen[full] = true
		out = append(out, full)
	}
	return out
}

// bindDocMedia 把一组完整 digest 挂到文档 owner 上，返回成功条数。
//
// 与 releaseDocMedia 成对：文档归档进 L3 时释放，文档写入时绑定。
// 只绑不放会让磁盘只增不减，只放不绑会让 GC 误删仍被引用的内容。
func (a *Agent) bindDocMedia(docID string, digests []string) int {
	if a.mediaStore == nil || docID == "" || len(digests) == 0 {
		return 0
	}
	bound := 0
	for _, d := range digests {
		if err := a.mediaStore.AddRef(d, media.OwnerDocument, docID); err != nil {
			log.Printf("[media] 文档引用绑定失败 (%s → doc %s): %v", shortDigest(d), docID, err)
			continue
		}
		bound++
	}
	if bound > 0 {
		log.Printf("[media] 文档 %s 绑定 %d 个媒体引用", docID, bound)
	}
	return bound
}

// commitTriplesWithMedia 提交三元组并绑定句子里的媒体引用。
//
// 包一层是为了让所有「三元组入库」的调用点用同一条路径拿到媒体绑定，
// 而不必各自记得多调一次 bindSentenceMedia。
// mediaBound 是本次实际挂到 graph_sentence owner 上的引用数；归档路径靠它
// 判定能否安全释放旧引用。媒体存储关闭时恒为 0（此时也没有引用需要释放）。
func (a *Agent) commitTriplesWithMedia(triples []memory.Triple, sessionID string, turnID int) (entities, relations, mediaBound int, err error) {
	if a.memory == nil {
		return 0, 0, 0, fmt.Errorf("graph memory 未启用")
	}
	// 媒体存储关闭时退回普通 Commit，省掉 sentenceIDs 的 map 分配。
	if a.mediaStore == nil {
		ec, rc, cErr := a.memory.Commit(triples, sessionID, turnID)
		return ec, rc, 0, cErr
	}
	sentenceIDs, ec, rc, err := a.memory.CommitWithMedia(triples, sessionID, turnID)
	if err != nil {
		return ec, rc, 0, err
	}
	return ec, rc, a.bindSentenceMedia(sentenceIDs), nil
}

// RecallMediaForSentence 反查某条图库句子引用的媒体。
//
// 这是整层的目的：几个月后从图谱走到一条句子，要能取回当时那份字节
// （若尚未被容量 GC 淘汰）。返回的是完整 digest，调用方用
// mediaStore.Get 取内容、Stat 取描述与元数据。
func (a *Agent) RecallMediaForSentence(sentenceID int64) ([]string, error) {
	if a.mediaStore == nil {
		return nil, nil
	}
	return a.mediaStore.Refs(media.OwnerGraphSentence, strconv.FormatInt(sentenceID, 10))
}

// sentenceIDsFromRelations 收集一批关系引用的句子 id（去重、去零）。
//
// 关系行本身不持有媒体，媒体挂在句子上（graph_sentence owner）。
// 因此"这次召回涉及哪些媒体"必须经由关系 → 句子 → media_refs 这条路。
func sentenceIDsFromRelations(relations []memory.Relation) []int64 {
	if len(relations) == 0 {
		return nil
	}
	seen := make(map[int64]bool, len(relations))
	var out []int64
	for _, r := range relations {
		if r.SentenceID == 0 || seen[r.SentenceID] {
			continue
		}
		seen[r.SentenceID] = true
		out = append(out, r.SentenceID)
	}
	return out
}

// mediaContextForRelations 是 mediaContextForSentences 的关系入口。
//
// 单独包一层是因为两个调用点（自动注入的 buildMemoryContext 与显式的
// memory_recall 工具）拿到的都是关系列表，不该各自重复"关系→句子"这步。
func (a *Agent) mediaContextForRelations(relations []memory.Relation) string {
	return a.mediaContextForSentences(sentenceIDsFromRelations(relations))
}

// mediaContextForInjectedEntities 为自动注入路径产出媒体说明。
//
// 单独一条路径是因为 Indexer.BuildContext 刻意不返回关系
// （Relations 恒为 nil，只给实体索引以省 token，细节留给 memory_recall）。
// 于是自动注入拿不到 sentence_id，必须用命中的实体名再查一次关系。
//
// 这次额外查询只为取 sentence_id，深度固定 1：媒体是"这条记忆当时带的图"，
// 不需要顺着关系network 扩散——扩散只会带出无关媒体并挤占 token。
func (a *Agent) mediaContextForInjectedEntities(injected *memory.InjectedContext) string {
	if a.mediaStore == nil || a.memory == nil || injected == nil || len(injected.Entities) == 0 {
		return ""
	}
	names := make([]string, 0, len(injected.Entities))
	for _, e := range injected.Entities {
		names = append(names, e.Name)
	}
	res, err := a.memory.Recall(nil, names, 1, "")
	if err != nil || res == nil {
		return ""
	}
	return a.mediaContextForRelations(res.Relations)
}

// mediaContextForSentences 给一组句子附上媒体说明，供召回时拼进提示词。
//
// 输出形如「句子 #12 关联媒体：[image/png a1b2c3d4e5f6] 一张紫蓝红三色带图」。
// 描述文本本就在句子里，这里补的是「内容是否还在、能否重新看图」这个信息——
// 描述永存而字节可能已被淘汰，两者状态不同。
func (a *Agent) mediaContextForSentences(sentenceIDs []int64) string {
	if a.mediaStore == nil || len(sentenceIDs) == 0 {
		return ""
	}
	var lines []string
	for _, sid := range sentenceIDs {
		digests, err := a.mediaStore.Refs(media.OwnerGraphSentence, strconv.FormatInt(sid, 10))
		if err != nil || len(digests) == 0 {
			continue
		}
		var parts []string
		for _, d := range digests {
			if line := a.mediaMarkerLine(d); line != "" {
				parts = append(parts, line)
			}
		}
		if len(parts) > 0 {
			lines = append(lines, fmt.Sprintf("句子 #%d 关联媒体：%s", sid, strings.Join(parts, "；")))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}
