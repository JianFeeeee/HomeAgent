package nlp

// ParseResult 依存句法分析结果
type ParseResult struct {
	Tokens  []string
	POS     []string
	Heads   []int    // 父节点索引，0=ROOT
	DepRels []string // 依存关系标签
}

// Triple 三元组 (subject, relation, object)
type Triple struct {
	Subject   string
	Relation  string
	Object    string
	Score     float64
	Src       string // "dep" / "fallback"
}

// TripleSet 提取结果
type TripleSet struct {
	Triples []Triple
	Src     string // "dep_parser" / "fallback" / ""
	Err     error
}
