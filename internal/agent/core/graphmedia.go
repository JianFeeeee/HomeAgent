package core

import (
	"fmt"
	"log"
	"strings"

	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/document"
)

// L3 图库的媒体绑定。
//
// 媒体在 L3 是一等记忆块（memory_blocks），以结构边与承载它的节点相连：
//   sentence --contains--> block（对话/三元组产生的记忆）
//   document --contains--> block（L2 文档归档进 L3）
//
// 这里不再有任何 marker 文本、正则反解或"描述文本当索引"的路径：
// 媒体只按自己的统一空间向量被检索，图库/文档只记录它的结构归属。

// migrateLegacyGraphMedia 把 marker 反解出来的旧媒体实体迁移成原生一等块。
//
// 旧数据里媒体是 type=Media 的普通实体（「图片 a1b2c3d4e5f6」），
// 靠生成的描述文本当索引。迁移后它变成真正的记忆块，以
// sentence --contains--> block 结构边挂回原句子，旧实体与描述关系删除。
// 迁移幂等（实体处理完即删除），因此在每个 Agent 启动时跑一次是安全的。
func (a *Agent) migrateLegacyGraphMedia() {
	if a.memory == nil || a.mediaStore == nil {
		return
	}
	blocks, entities, err := a.memory.MigrateLegacyMediaEntities(func(short string) (memory.MemoryBlock, bool) {
		full, err := a.mediaStore.ResolvePrefix(short)
		if err != nil {
			return memory.MemoryBlock{}, false
		}
		return a.blockFromDigest(full)
	})
	if err != nil {
		log.Printf("[media] 旧媒体实体迁移失败（下轮重试）: %v", err)
		return
	}
	if blocks > 0 || entities > 0 {
		log.Printf("[media] 旧媒体实体迁移完成: 新建 %d 个原生块，删除 %d 个描述式实体", blocks, entities)
	}
}

// attachBlocksToSentenceBlock 把一组 digest 变成 L3 一等块并挂到**原句块**上。
// seed 允许复用已持有块的 ID（L2→L3 迁移保持块身份不变）。
// ★ sentenceBlockID 是**原句块 ID**（原为 sentences 表行号）。
//
//	句子的承载者已从 sentences 表迁移到 blk_src_<sha256(text[:12])>，
//	媒体边因此改挂「原句块 --contains--> 媒体块」。
func (a *Agent) attachBlocksToSentenceBlock(sentenceBlockID string, digests []string, seed map[string]memory.MemoryBlock, scene string) int {
	if a.mediaStore == nil || a.memory == nil || sentenceBlockID == "" {
		return 0
	}
	bound := 0
	for _, d := range digests {
		full, err := a.mediaStore.ResolvePrefix(d)
		if err != nil {
			log.Printf("[media] digest %s 无法解析: %v", d, err)
			continue
		}
		b, ok := seed[full]
		if !ok {
			if b, ok = a.blockFromDigest(full); !ok {
				continue
			}
		}
		// 块继承承载它的三元组的场景：块是流水线里最细的子项目，场景要落到它身上，
		// 否则「那场对话里发过来的那张图」在场面重现时永远取不回来。
		if b.Scene == "" {
			b.Scene = scene
		}
		if err := a.memory.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
			log.Printf("[media] L3 块写入失败 (%s): %v", shortDigest(full), err)
			continue
		}
		if err := a.memory.AddMemoryBlockEdge("block", sentenceBlockID, "block", b.ID, "contains"); err != nil {
			log.Printf("[media] 句子→块边建立失败 (%s): %v", shortDigest(full), err)
			continue
		}
		bound++
	}
	return bound
}

// linkBlocksToDocument 把文档持有的块写入 L3，并建立
// document --contains--> block 边。块的 ID 原样保留（迁移而非重建）。
func (a *Agent) linkBlocksToDocument(docID string, blocks []memory.MemoryBlock, scene string) int {
	if a.memory == nil || docID == "" || len(blocks) == 0 {
		return 0
	}
	if err := a.memory.PutDocumentNode(docID, ""); err != nil {
		log.Printf("[media] 写入 L3 文档节点失败 (%s): %v", docID, err)
		return 0
	}
	// 文档层与场景模型兼容：文档节点也进场景，好让「这个场面有哪些文档」
	// 可枚举、可统计（场景贯穿流水线的 doc 层落地）。
	if scene != "" {
		if err := a.memory.TagSceneDocument(scene, docID); err != nil {
			log.Printf("[media] 文档挂场景失败 (%s): %v", docID, err)
		}
	}
	// 文档层把场景传给块：归档进图库的块属于该文档的来源场面（QQ 归档的图
	// 就该挂在 chan:qq 上），否则 L3 里这批块在场景召回中不可见。
	for i := range blocks {
		if blocks[i].Scene == "" {
			blocks[i].Scene = scene
		}
	}
	if err := a.memory.PutMemoryBlocks(blocks); err != nil {
		log.Printf("[media] 写入 L3 记忆块失败 (doc %s): %v", docID, err)
		return 0
	}
	bound := 0
	for _, b := range blocks {
		if err := a.memory.AddMemoryBlockEdge("document", docID, "block", b.ID, "contains"); err != nil {
			log.Printf("[media] 文档→块边建立失败 (%s): %v", shortDigest(b.PayloadDigest), err)
			continue
		}
		bound++
	}
	return bound
}

// commitTriplesWithMedia 提交三元组并把三元组显式携带的媒体变成 L3 一等块。
//
// seed 是调用方已持有的一等块（如 L2 文档的 Blocks），用于保持块身份；
// 普通对话路径传 nil。blocks 是本次写入 L3 的块数。
func (a *Agent) commitTriplesWithMedia(triples []memory.Triple, sessionID string, turnID int, seed []memory.MemoryBlock) (entities, relations, blocks int, err error) {
	g := a.graphMem()
	if g == nil {
		return 0, 0, 0, fmt.Errorf("graph memory 未启用")
	}
	// 轻量内核（memory == nil，子只有图记忆）或没有媒体库时：只写图记忆。
	// 写目标由 a.graph 决定 —— 根落 main，子落自己的 temp。
	if a.memory == nil || a.mediaStore == nil {
		ec, rc, cErr := g.Commit(triples, sessionID, turnID)
		return ec, rc, 0, cErr
	}
	sentenceIDs, ec, rc, err := a.memory.CommitWithMedia(triples, sessionID, turnID)
	if err != nil {
		return ec, rc, 0, err
	}
	byDigest := make(map[string]memory.MemoryBlock, len(seed))
	for _, b := range seed {
		if b.PayloadDigest != "" {
			byDigest[b.PayloadDigest] = b
		}
	}
	for _, t := range triples {
		if len(t.MediaDigests) == 0 {
			continue
		}
		sid := sentenceIDs[t.SentenceText]
		if sid == "" {
			continue
		}
		blocks += a.attachBlocksToSentenceBlock(sid, t.MediaDigests, byDigest, t.Scene)
	}
	return ec, rc, blocks, nil
}

// RecallBlocksForSentence 反查某条图库句子持有的一等记忆块。
// RecallBlocksForSentence 按**原句块 ID**取回它承载的媒体块。
//
// ★ 签名从 int64 改为 string：挂载点已从「sentences 表行号」
//
//	改成「原句块 ID」（sentences 表退场后行号不存在）。
func (a *Agent) RecallBlocksForSentence(sentenceBlockID string) ([]memory.MemoryBlock, error) {
	if a.memory == nil {
		return nil, nil
	}
	return a.memory.BlocksForNode("block", sentenceBlockID)
}

// resolveMediaDigests 把模型给的（多为短）digest 补全成完整 digest。
//
// 补不上就丢弃那一条并记日志：模型可能凭印象编了个 digest，也可能内容已被删除。
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

// sentenceIDsFromRelations 收集一批关系引用的句子 id（去重、去零）。
//
// 关系行本身不持有媒体，媒体作为一等块以 sentence --contains--> block
// 结构边与句子相连；因此"这次召回涉及哪些媒体"必须经由关系 → 句子这一跳。
// sentenceIDsFromRelations 从关系里取出其原句的**块 ID**。
//
// ★ 从 Relation.SentenceID（旧 sentences 表行号）改为按 SentenceText 现算
//
//	blk_src_<hash>。
//
//	原因：Commit 块化后原句由块承载，sentences 表退场，
//	行号不再是稳定的挂载点；而 SentenceText 一直在 Relation 上。
//
//	这条链（mediaContextForSentences / mediaContextForRelations）
//	完全靠这个 ID 找回媒体块，所以改错了 = 媒体上下文整体失效。
func sentenceIDsFromRelations(relations []memory.Relation) []string {
	if len(relations) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(relations))
	var out []string
	for _, r := range relations {
		id := ""
		if r.SentenceText != "" {
			id = memory.SentenceBlockID(r.SentenceText)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// mediaContextForRelations 是 mediaContextForSentences 的关系入口。
func (a *Agent) mediaContextForRelations(relations []memory.Relation) string {
	return a.mediaContextForSentences(sentenceIDsFromRelations(relations))
}

// formatRecallRelations 渲染 memory_recall 的关系行，超 max 条截断。
//
// 带上原始句子（截断到 60 字）：三元组只是「A 关系 B」，脱离原句往往看不出
// 语气、条件与指代——`sentence_text` 的存在意义就是「日后从图谱回到原文」，
// 而 Recall 已经把句子 JOIN 出来了。此前只回显实体名与关系类型，导致模型
// 填了 sentence_text 也永远拿不回来，这个能力形同虚设。
func formatRecallRelations(relations []memory.Relation, max int) []string {
	var out []string
	for i, r := range relations {
		if max > 0 && i >= max {
			out = append(out, "...更多关系被截断")
			break
		}
		line := fmt.Sprintf("- %s →(%s)→ %s", r.SourceName, r.RelationType, r.TargetName)
		if s := strings.TrimSpace(r.SentenceText); s != "" {
			line += "  原句: \"" + truncateStr(s, 60) + "\""
		}
		out = append(out, line)
	}
	return out
}

// mediaContextForInjectedEntities 为自动注入路径产出媒体说明。
//
// Indexer.BuildContext 刻意不返回关系（只给实体索引以省 token），
// 因此这里用命中的实体名再查一次关系，只为拿到 sentence_id。
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

// blockLabelsForDoc 渲染文档持有块的标签（MIME + 短 digest），供 doc_query 展示。
func (a *Agent) blockLabelsForDoc(d *document.Doc) string {
	if a.mediaStore == nil || d == nil || len(d.Blocks) == 0 {
		return ""
	}
	var parts []string
	for _, b := range d.Blocks {
		it, err := a.mediaStore.Stat(b.PayloadDigest)
		if err != nil || it == nil {
			continue
		}
		if line := mediaLabel(it); line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "；")
}

// mediaContextForSentences 给一组句子附上其持有的一等块标签。
//
// 标签只含 MIME 与短 digest：图片按向量检索，标签的作用是告诉模型
// "这条记忆当时带着哪份媒体、可用该 digest 取回字节"。
func (a *Agent) mediaContextForSentences(sentenceBlockIDs []string) string {
	if a.mediaStore == nil || a.memory == nil || len(sentenceBlockIDs) == 0 {
		return ""
	}
	var lines []string
	for _, sid := range sentenceBlockIDs {
		blocks, err := a.memory.BlocksForNode("block", sid)
		if err != nil || len(blocks) == 0 {
			continue
		}
		var parts []string
		for _, b := range blocks {
			it, err := a.mediaStore.Stat(b.PayloadDigest)
			if err != nil || it == nil {
				continue
			}
			if line := mediaLabel(it); line != "" {
				parts = append(parts, line)
			}
		}
		if len(parts) > 0 {
			lines = append(lines, fmt.Sprintf("句子 %s 关联媒体：%s", shortID(sid), strings.Join(parts, "；")))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// shortID 把块 ID 压成可读短码（blk_src_<24hex> → 前 8 位 hex）。
// 媒体上下文是给模型看的，整串 hex 只会占 token 且不可读。
func shortID(blockID string) string {
	const pfx = "blk_src_"
	if strings.HasPrefix(blockID, pfx) {
		return blockID[len(pfx):]
	}
	if len(blockID) > 12 {
		return blockID[:12]
	}
	return blockID
}
