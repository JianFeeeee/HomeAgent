package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// SentenceBlockID 由**原句文本**派生原句块的 ID。
//
// ★ 为什么在 memory 包而不是 distill
// -------------------------------
// 两条路径都需要它：迁移（MigrateLegacyTextEntities）与
// 蒸馏（distill.WritePayload）。而 distill 已导入 memory，
// 若放在 distill 会形成 memory → distill 的导入环。
//
// ★ 为什么 ID 必须由内容派生
// --------------------------
// 方案 A 要删 sentences 表，原句就得由块承载，而块 ID 必须**幂等**
// —— 迁移与蒸馏都会重复跑同一条原句。
// 迁移块自己用的是 blk_ent_<entityID>_<hash>（依赖行号，不是内容派生），
// 不能复用；这里用 sha256(TrimSpace(text))。
func SentenceBlockID(text string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return "blk_src_" + hex.EncodeToString(h[:12])
}

// SentenceBlockSource 是原句块的 source 标记。
const SentenceBlockSource = "sentence"

// NewSentenceBlock 构造承载原句的块。
//
// ★ 刻意**不带向量**
// ------------------
// 它是溯源锚点，不参与向量召回。理由是实测过的：
//
//	「13000端口」(7字)               容易被召回
//	「13010/13011 而非 12011」(20字)  召不回
//
// 长整句的句向量会把关键值稀释掉 —— 让原句进召回只会引入噪声。
// 省一次 embedding，也省一次无用的向量存储。
func NewSentenceBlock(text string, createdAt, updatedAt time.Time) MemoryBlock {
	return MemoryBlock{
		ID:        SentenceBlockID(text),
		Modality:  BlockText,
		Text:      strings.TrimSpace(text),
		Source:    SentenceBlockSource,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}
