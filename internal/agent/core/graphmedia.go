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
func (a *Agent) bindSentenceMedia(sentenceIDs map[string]int64) {
	if a.mediaStore == nil || len(sentenceIDs) == 0 {
		return
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
}

// commitTriplesWithMedia 提交三元组并绑定句子里的媒体引用。
//
// 包一层是为了让所有「三元组入库」的调用点用同一条路径拿到媒体绑定，
// 而不必各自记得多调一次 bindSentenceMedia。
func (a *Agent) commitTriplesWithMedia(triples []memory.Triple, sessionID string, turnID int) (int, int, error) {
	if a.memory == nil {
		return 0, 0, fmt.Errorf("graph memory 未启用")
	}
	// 媒体存储关闭时退回普通 Commit，省掉 sentenceIDs 的 map 分配。
	if a.mediaStore == nil {
		return a.memory.Commit(triples, sessionID, turnID)
	}
	sentenceIDs, ec, rc, err := a.memory.CommitWithMedia(triples, sessionID, turnID)
	if err != nil {
		return ec, rc, err
	}
	a.bindSentenceMedia(sentenceIDs)
	return ec, rc, nil
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
			it, err := a.mediaStore.Stat(d)
			if err != nil || it == nil {
				continue
			}
			label := string(it.Kind)
			if it.MIME != "" {
				label = it.MIME
			}
			desc := it.Description
			if desc == "" {
				desc = "(未描述)"
			}
			parts = append(parts, fmt.Sprintf("[%s %s] %s", label, shortDigest(d), desc))
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
