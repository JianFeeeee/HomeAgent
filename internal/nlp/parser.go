package nlp

import "gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"

// Parser 依存句法分析器接口
type Parser interface {
	Parse(text string) (*ParseResult, error)
}

// Vectorizer 向量化接口，复用 memory/vector 或 memory/static_embedder
type Vectorizer interface {
	Vectorize(text string) vector.Vector
}

// Extractor 三元组提取器
type Extractor struct {
	parser   Parser
	fallack  Parser // 降级用 POS 模板解析器
	embedder Vectorizer // 可选：用于 TransE 语义验证
}

// NewExtractor 创建提取器，parser 为 nil 时纯用 fallback
func NewExtractor(parser Parser) *Extractor {
	return &Extractor{
		parser:  parser,
		fallack: newFallbackParser(),
	}
}

// SetEmbedder 设置词嵌入向量化器，用于候选三元组的语义验证
func (e *Extractor) SetEmbedder(ev Vectorizer) {
	e.embedder = ev
}

// Extract 从文本中提取三元组
// 优先使用 parser，失败/无结果时自动降级到 fallback
// 如果设置了 embedder，还会做 h+r≈t 向量验证过滤
func (e *Extractor) Extract(text string) *TripleSet {
	if text == "" {
		return &TripleSet{Src: "", Err: nil}
	}

	var allTriples []Triple
	src := ""

	sentences := splitSentences(text)
	for _, sentence := range sentences {
		if sentence == "" {
			continue
		}
		var triples []Triple

		// 主线：依存解析 + 模板匹配
		if e.parser != nil {
			result, err := e.parser.Parse(sentence)
			if err == nil && result != nil && len(result.Tokens) > 1 {
				triples = extractFromDep(result)
				if len(triples) > 0 {
					src = "dep_parser"
				}
			}
		}

		// 降级：POS 模板匹配
		if len(triples) == 0 && e.fallack != nil {
			result, err := e.fallack.Parse(sentence)
			if err == nil && result != nil && len(result.Tokens) > 1 {
				triples = extractFromPOS(result)
				if len(triples) > 0 {
					src = "fallback"
				}
			}
		}

		// 向量验证（可选）：用 h+r≈t 过滤不合理三元组
		if len(triples) > 0 && e.embedder != nil {
			triples = verifyTriples(triples, e.embedder)
		}

		allTriples = append(allTriples, triples...)
	}

	if len(allTriples) > 0 {
		return &TripleSet{Triples: allTriples, Src: src}
	}
	return &TripleSet{Src: src}
}

// verifyTriples 使用 TransE 打分 (h+r≈t) 验证三元组，过滤低分项
func verifyTriples(triples []Triple, embedder Vectorizer) []Triple {
	var kept []Triple
	for _, t := range triples {
		h := embedder.Vectorize(t.Subject)
		r := embedder.Vectorize(t.Relation)
		tv := embedder.Vectorize(t.Object)

		hr := addVectors(h, r)
		sim := vector.CosineSimilarity(hr, tv)

		// 语义一致性过低 → 过滤（除非 fallback 无其他候选）
		if sim >= 0.25 {
			t.Score *= (0.5 + 0.5*sim)
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return triples
	}
	return kept
}

func addVectors(a, b vector.Vector) vector.Vector {
	out := make(vector.Vector)
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}
