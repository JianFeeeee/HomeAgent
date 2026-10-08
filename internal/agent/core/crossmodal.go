package core

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/JianFeeeee/HomeAgent/internal/memory/document"
	"github.com/JianFeeeee/HomeAgent/internal/memory/media"
)

// CrossModalHit 是跨模态检索融合后的一条候选。
//
// 统一的检索单元是记忆块而非 CAS 全库：媒体在 L0/L2/L3 都由层容器持有，
// 只有仍被某层记忆块持有的媒体才可召回。Doc 是 L2 文档；Media 是该块携带的
// 原生媒体坐标。两路分数尺度不同，融合前各自归一化，见 fuseCrossModal。
type CrossModalHit struct {
	Doc        *document.Doc // 文本路命中的文档；视觉路命中时为 nil
	Media      *media.Item   // 视觉路命中的媒体；文本路命中时也可能带关联媒体
	MediaScore float64       // 视觉路原始 cosine（无则 0）
	DocScore   float64       // 文本路原始 cosine（无则 0）
	Fused      float64       // 归一化加权融合分，供最终排序
	// 该媒体同时被两路命中（文本路经文档关联、视觉路直接命中）时，
	// DoubleHit=true —— 双信号确认，应排在只被一路命中的候选之前。
	DoubleHit bool
}

// CrossModalFusionConfig 控制文本路与视觉路的融合行为。
// 默认各路权重 0.5，双命中加权 0.15；不同模型/场景可按实测调整。
type CrossModalFusionConfig struct {
	WeightText     float64 // 文本路融合权重（默认 0.5）
	WeightVisual   float64 // 视觉路融合权重（默认 0.5）
	DoubleHitBonus float64 // 双命中额外加分（默认 0.15）
	MinMaxEps      float64 // min-max 归一化除零保护（默认 1e-12）
}

var defaultFusionConfig = CrossModalFusionConfig{
	WeightText:     0.5,
	WeightVisual:   0.5,
	DoubleHitBonus: 0.15,
	MinMaxEps:      1e-12,
}

func (c CrossModalFusionConfig) textWeight() float64 {
	if c.WeightText <= 0 {
		return defaultFusionConfig.WeightText
	}
	return c.WeightText
}
func (c CrossModalFusionConfig) visualWeight() float64 {
	if c.WeightVisual <= 0 {
		return defaultFusionConfig.WeightVisual
	}
	return c.WeightVisual
}
func (c CrossModalFusionConfig) doubleHitBonus() float64 {
	return c.DoubleHitBonus
}
func (c CrossModalFusionConfig) minMaxEps() float64 {
	if c.MinMaxEps <= 0 {
		return defaultFusionConfig.MinMaxEps
	}
	return c.MinMaxEps
}

// retrieveCrossModal 是跨模态并行检索的统一入口。
//
// 策略（两路并行，召回真正最相似的）：
//  1. 文本路：query 整段文本编码后查文档层（Doc.DenseVec 已融合其块的媒体向量），
//     命中文档若持有媒体块，直接带上该块。
//  2. 视觉路：query 经多模态模型文本编码 → 与媒体块向量比余弦
//     （QueryMediaScored），覆盖文本向量没写到的视觉内容。
//  3. 融合：两条路候选各自 min-max 归一化到 [0,1]，加权求和后降序，取 topK。
//     同一媒体被两路同时命中视为双信号确认，额外加权。
//
// 多模态空间未配置时视觉路为空，退化为纯文本路（等价旧 docStore.Query）。
func (a *Agent) retrieveCrossModal(query string, topK int, cfg CrossModalFusionConfig) []CrossModalHit {
	if topK <= 0 {
		topK = 5
	}
	// 融合前各取 2× 余量，保证融合排序后 topK 仍有足够候选。
	per := topK * 2
	if per < 8 {
		per = 8
	}

	// ---- 文本路 ----
	var textHits []CrossModalHit
	if a.docStore != nil {
		for _, dh := range a.docStore.QueryScored(query, per) {
			hit := CrossModalHit{Doc: dh.Doc, DocScore: dh.Score}
			// 命中文档若持有一等记忆块，把首个媒体块一并带上。
			if a.mediaStore != nil && len(dh.Doc.Blocks) > 0 {
				if it, err := a.mediaStore.Stat(dh.Doc.Blocks[0].PayloadDigest); err == nil {
					hit.Media = it
				}
			}
			textHits = append(textHits, hit)
		}
	}

	// ---- 视觉路（多模态文本编码 → 当前记忆层持有的媒体块）----
	var visualHits []CrossModalHit
	if a.multimodalSpace != nil && a.multimodalSpace.Loaded() && a.mediaStore != nil {
		qv, err := a.multimodalSpace.VectorizeDense(query)
		if err != nil {
			log.Printf("[crossmodal] 多模态文本编码失败: %v", err)
		} else if mh, err := a.mediaStore.QueryMediaScored(qv, a.multimodalSpace.Fingerprint(), per); err != nil {
			log.Printf("[crossmodal] 媒体记忆检索失败: %v", err)
		} else {
			// 只有仍被某层记忆块持有的媒体才可召回：CAS 是全库字节存储，
			// 直接拿它的检索结果会把已无处可归的内容也从记忆里翻出来。
			held := a.heldMediaDigests()
			for _, h := range mh {
				if h.Item == nil || !held[h.Item.Digest] {
					continue
				}
				visualHits = append(visualHits, CrossModalHit{
					Media: h.Item, MediaScore: h.Score,
				})
			}
		}
	}

	return fuseCrossModal(textHits, visualHits, topK, cfg)
}

// fuseCrossModal 把文本路与视觉路候选按各自归一化分融合排序。
//
// 归一化模板：两路分数尺度不可直接相加，先各自在路内 min-max 到 [0,1]：
//
//	norm(x) = (x - min) / (max - min)，max==min 时置 1
//
// 再加权求和：fused = wText·normText + wVisual·normVisual。同一媒体两路都命中
// （经文档关联 + 视觉直接）时 DoubleHit，在加权分上再加双信号确认分。
// 权重通过 CrossModalFusionConfig 按场景配置，不同模型/版本可按实测调整。
func fuseCrossModal(textHits, visualHits []CrossModalHit, topK int, cfg CrossModalFusionConfig) []CrossModalHit {
	norm := func(hits []CrossModalHit, pick func(CrossModalHit) float64) []float64 {
		out := make([]float64, len(hits))
		if len(hits) == 0 {
			return out
		}
		maxV, minV := pick(hits[0]), pick(hits[0])
		for _, h := range hits[1:] {
			v := pick(h)
			if v > maxV {
				maxV = v
			}
			if v < minV {
				minV = v
			}
		}
		for i, h := range hits {
			v := pick(h)
			if maxV-minV < cfg.minMaxEps() {
				out[i] = 1
				continue
			}
			out[i] = (v - minV) / (maxV - minV)
		}
		return out
	}
	textN := norm(textHits, func(h CrossModalHit) float64 { return h.DocScore })
	visualN := norm(visualHits, func(h CrossModalHit) float64 { return h.MediaScore })

	byKey := make(map[string]*CrossModalHit)
	var keys []string
	key := func(h CrossModalHit) string {
		if h.Doc != nil {
			return "doc:" + h.Doc.ID
		}
		if h.Media != nil {
			return "media:" + h.Media.Digest
		}
		return ""
	}

	// 先并入视觉路（视觉媒体是独立实体）
	for i, h := range visualHits {
		k := key(h)
		if k == "" {
			continue
		}
		clone := h
		clone.Fused = cfg.visualWeight() * visualN[i]
		byKey[k] = &clone
		keys = append(keys, k)
	}
	// 再并入文本路：命中的文档是独立实体；带媒体的文档若其媒体 digest
	// 已在视觉路（双命中），合并到同一候选并标记 DoubleHit。
	for i, h := range textHits {
		if h.Doc == nil {
			continue
		}
		if h.Media != nil {
			if ex, ok := byKey["media:"+h.Media.Digest]; ok {
				ex.DoubleHit = true
				ex.Doc = h.Doc
				ex.Fused += cfg.textWeight()*textN[i] + cfg.doubleHitBonus()
				continue
			}
		}
		k := "doc:" + h.Doc.ID
		if ex, ok := byKey[k]; ok {
			ex.Doc = h.Doc
			ex.DoubleHit = false
			ex.Fused += cfg.textWeight() * textN[i]
			continue
		}
		clone := h
		clone.Fused = cfg.textWeight() * textN[i]
		byKey[k] = &clone
		keys = append(keys, k)
	}

	var merged []CrossModalHit
	for _, k := range keys {
		if c := byKey[k]; c != nil {
			merged = append(merged, *c)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].DoubleHit != merged[j].DoubleHit {
			return merged[i].DoubleHit
		}
		return merged[i].Fused > merged[j].Fused
	})
	if len(merged) > topK {
		merged = merged[:topK]
	}
	return merged
}

// crossModalMarkdown 把融合候选渲染成注入上下文的文本。
// 文档行给出摘要；媒体行只给 MIME + 短 digest（不再有生成的描述）。
func (a *Agent) crossModalMarkdown(hits []CrossModalHit) string {
	if len(hits) == 0 {
		return ""
	}
	var lines []string
	for i, h := range hits {
		marker := ""
		switch {
		case h.DoubleHit:
			marker = "（图文双命中）"
		case h.Doc != nil:
			marker = "（文本命中）"
		case h.Media != nil:
			marker = "（视觉命中）"
		}
		parts := []string{fmt.Sprintf("[%d]", i+1)}
		if h.Doc != nil {
			parts = append(parts, h.Doc.Summary)
			if h.Doc.Source != "" {
				parts = append(parts, fmt.Sprintf("(来源:%s)", h.Doc.Source))
			}
		}
		if h.Media != nil {
			if line := mediaLabel(h.Media); line != "" {
				parts = append(parts, line)
			}
		}
		parts = append(parts, fmt.Sprintf("相关度:%.2f%s", h.Fused, marker))
		lines = append(lines, strings.Join(parts, " "))
	}
	return "【跨模态相关记忆】\n" + strings.Join(lines, "\n")
}
