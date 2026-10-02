package distill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
)

// 拆解产物的落库形态：句子 + 文本块 + 结构边。
//
// 为什么不是 entities/relations（本会话最初的错误做法）
// --------------------------------------------------
// 三元组在图里的正确形态是【节点】【关系边】【节点】，而节点已从纯文本实体
// 升级为带 vector/fingerprint/modality 的 memory_blocks（46f833c）。把拆解
// 结果 Commit 成 entities 是把新产出灌进正在退场的旧形态。
//
// 端点 kind 的既有设计（block.go:175 validGraphNodeKind）：
//
//	block / entity / sentence / document
//
// 句子的职责由 sentences 表承担（它已有 id、text、created_at），
// 块的职责是**可向量化的最小子项目**。所以正确的挂接是：
//
//	sentence(原句) --contains--> block(字段内容, 带向量)
//
// 与媒体块完全同构（graphmedia.go:73 的 sentence→block contains 边）。
// 不把原句也做成块 —— 那会与 sentences 表重复，且原句不需要单独向量
// （它的语义由它包含的字段块共同表达）。

// BlockPayload 是一次拆解的产物：原句 + 字段块 + 连接关系。
type BlockPayload struct {
	// Sentence 是原始记录文本（写入 sentences 表，作为结构边的源端点）。
	Sentence string
	// Fields 是按维度拆出的字段块。
	Fields []FieldBlock
}

// FieldBlock 是一个字段块：维度名 + 原样值。
type FieldBlock struct {
	Dimension string
	Value     string
}

// Blocks 把一条记录拆成块形态。
//
// 与 Split 的区别：Split 返回 Triple（旧形态，供对照期使用）；
// Blocks 返回块形态（新形态）。两者共用同一套拆解逻辑与闸门，
// 不重复实现——闸门（值原样校验、维度归一、长度上限、自环过滤）
// 是拆解质量的全部保障，分两处实现必然漂移。
func (e *Extractor) Blocks(ctx context.Context, record string) (*BlockPayload, error) {
	triples, err := e.Split(ctx, record)
	if err != nil {
		return nil, err
	}
	if len(triples) == 0 {
		return &BlockPayload{Sentence: record}, nil
	}
	payload := &BlockPayload{Sentence: record}
	for _, t := range triples {
		payload.Fields = append(payload.Fields, FieldBlock{
			Dimension: t.Relation,
			Value:     t.Object,
		})
	}
	return payload, nil
}

// BlockID 由句子与字段内容派生，保证同一记录重复拆解得到同一个块 ID。
//
// 为什么用内容摘要而不是随机 ID：蒸馏是**可重试**的（distillOnce 失败会
// 把记录写回队列下次重试），随机 ID 会让每次重试都产生一批新块，
// 同一句记忆在库里堆成 N 份。内容派生 ID 配合 PutMemoryBlocks 的
// ON CONFLICT 语义天然幂等。
func BlockID(sentence, dimension, value string) string {
	h := sha256.Sum256([]byte(sentence + "\x00" + dimension + "\x00" + value))
	return "blk_" + hex.EncodeToString(h[:12])
}

// ToMemoryBlock 把一个字段块转成可入库的 MemoryBlock。
//
// Vector/Fingerprint 由调用方填充（需要 embedding provider）。**provider
// 不可用时留空而不是编造** —— 无向量的块仍可被 BlocksForNode 按边查到，
// 只是不参与向量召回；编造一个零向量会让它参与检索并永远排在最后，
// 那是静默的错误记忆。
func (f FieldBlock) ToMemoryBlock(sentence string) memory.MemoryBlock {
	return memory.MemoryBlock{
		ID:       BlockID(sentence, f.Dimension, f.Value),
		Modality: memory.BlockText,
		Text:     f.Dimension + "=" + f.Value,
		// 保留维度名与值两个结构化字段进 source/tool 之外的位置？
		// 不：MemoryBlock 没有任意扩展字段，而 text_content 已经承载
		// "维度=值" 的完整语义（向量也基于它算）。需要拆出维度时按
		// 首个 '=' 切分即可，且维度名已受控词表归一（NormalizeDimension），
		// 不含 '='。
	}
}

// Dimension 从块文本里取回维度名（'=' 前）。取不到时返回空串。
//
// 用途：召回后要告诉模型「这条记忆是关于哪个维度的」而不只是原文串。
func Dimension(blockText string) string {
	i := strings.Index(blockText, "=")
	if i <= 0 {
		return ""
	}
	return blockText[:i]
}

// Value 从块文本里取回值（'=' 后）。
func Value(blockText string) string {
	i := strings.Index(blockText, "=")
	if i < 0 {
		return blockText
	}
	return blockText[i+1:]
}

// WritePayload 把拆解产物写入图：句子行 + 字段块 + contains 边。
//
// embed 为 nil 时块不带向量（不编造）。返回写入的块数。
//
// 顺序与失败语义：句子必须先落（边的一端要存在，AddMemoryBlockEdge 会校验），
// 而 PutMemoryBlocks 本身是事务、AddMemoryBlockEdge 也是事务 —— 这里
// 逐块提交而非整批原子：一次拆解里某个块写失败不该让其余块回滚，
// 因为每条边都是独立事实（块 A 与块 B 之间没有依赖）。
func WritePayload(ctx context.Context, g *memory.GraphDB, payload *BlockPayload,
	embed func(string) ([]float64, string)) (int, error) {
	if g == nil || payload == nil || strings.TrimSpace(payload.Sentence) == "" {
		return 0, fmt.Errorf("distill: empty payload")
	}
	if len(payload.Fields) == 0 {
		return 0, nil
	}

	sentenceID, err := g.EnsureSentence(payload.Sentence)
	if err != nil {
		return 0, fmt.Errorf("distill: ensure sentence: %w", err)
	}

	written := 0
	for _, f := range payload.Fields {
		b := f.ToMemoryBlock(payload.Sentence)
		if embed != nil {
			if vec, fp := embed(b.Text); len(vec) > 0 {
				b.Vector = vec
				b.Fingerprint = fp
			}
		}
		if err := g.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
			return written, fmt.Errorf("distill: put block %s: %w", b.ID, err)
		}
		if err := g.AddMemoryBlockEdge("sentence", fmt.Sprint(sentenceID),
			"block", b.ID, "contains"); err != nil {
			return written, fmt.Errorf("distill: link block %s: %w", b.ID, err)
		}
		written++
	}
	return written, nil
}
