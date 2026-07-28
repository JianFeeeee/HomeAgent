//go:build !onnxruntime

package nlp

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed models/vocab.json models/pos_vocab.json
var vocabFS embed.FS

// ONNXParser 在未启用 onnxruntime 时作为规则式降级解析器。
// 使用内嵌词表实现基于词典的 POS 标注 + 基于 POS 序列的依存关系推断。
type ONNXParser struct {
	vocab    map[string]int
	posVocab map[string]int
}

type ONNXConfig struct {
	ModelPath string // 留空使用内嵌规则引擎
	DataDir   string // 仅在 onnxruntime 启用时使用
}

func NewONNXParser(cfg ONNXConfig) (*ONNXParser, error) {
	vocab := make(map[string]int)
	data, err := vocabFS.ReadFile("models/vocab.json")
	if err != nil {
		return nil, fmt.Errorf("read vocab: %w", err)
	}

	var raw struct {
		Word map[string]int `json:"word"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		// 尝试直接解析为 flat map
		var flat map[string]int
		if err2 := json.Unmarshal(data, &flat); err2 != nil {
			return nil, fmt.Errorf("parse vocab: %w", err)
		}
		vocab = flat
	} else {
		vocab = raw.Word
	}

	posVocab := make(map[string]int)
	data, err = vocabFS.ReadFile("models/pos_vocab.json")
	if err != nil {
		return nil, fmt.Errorf("read pos_vocab: %w", err)
	}
	if err := json.Unmarshal(data, &posVocab); err != nil {
		return nil, fmt.Errorf("parse pos_vocab: %w", err)
	}

	return &ONNXParser{vocab: vocab, posVocab: posVocab}, nil
}

func (p *ONNXParser) Parse(text string) (*ParseResult, error) {
	if text == "" {
		return &ParseResult{}, nil
	}

	// Phase 1: 基于词表的最大匹配分词
	tokens := p.tokenize(text)
	if len(tokens) == 0 {
		return &ParseResult{}, nil
	}

	// Phase 2: 基于词表的规则式 POS 标注
	pos := p.tagPOS(tokens)

	// Phase 3: 基于 POS 序列的依存头推断
	heads := p.inferHeads(tokens, pos)

	// Phase 4: 关系标签推断
	rels := p.inferRels(tokens, pos, heads)

	return &ParseResult{
		Tokens:  tokens,
		POS:     pos,
		Heads:   heads,
		DepRels: rels,
	}, nil
}

func (p *ONNXParser) tokenize(text string) []string {
	runes := []rune(text)
	var tokens []string
	buf := []rune{}
	for _, r := range runes {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if len(buf) > 0 {
				tokens = append(tokens, string(buf))
				buf = buf[:0]
			}
			continue
		}
		buf = append(buf, r)
		// 最长匹配：检查当前 buf 是否在词表中
		if _, ok := p.vocab[string(buf)]; !ok && len(buf) > 0 {
			// 回退：取 buf[:-1] 作为词，继续
			if _, ok2 := p.vocab[string(buf[:len(buf)-1])]; ok2 && len(buf) > 2 {
				tokens = append(tokens, string(buf[:len(buf)-1]))
				buf = buf[len(buf)-1:]
			}
		}
	}
	if len(buf) > 0 {
		tokens = append(tokens, string(buf))
	}
	if len(tokens) == 0 {
		tokens = strings.Fields(text)
	}
	return tokens
}

func (p *ONNXParser) tagPOS(tokens []string) []string {
	pos := make([]string, len(tokens))
	for i, t := range tokens {
		pos[i] = p.guessPOS(t)
	}
	return pos
}

func (p *ONNXParser) guessPOS(word string) string {
	if _, ok := p.vocab[word]; !ok {
		// OOV: 基于启发式
		if len(word) == 0 {
			return "X"
		}
		if isPunct([]rune(word)[0]) {
			return "PUNCT"
		}
		if isDigit(word) {
			return "NUM"
		}
		return "X"
	}
	// 对词表中的词，基于可用特征判断
	runes := []rune(word)
	if len(runes) == 0 {
		return "X"
	}
	first := runes[0]
	if isPunct(first) {
		return "PUNCT"
	}
	return "NOUN"
}

func isPunct(r rune) bool {
	return (r >= 0x3000 && r <= 0x303F) || // CJK 标点
		(r >= 0xFF00 && r <= 0xFFEF) || // 全角
		r == '.' || r == ',' || r == '!' || r == '?' ||
		r == ';' || r == ':' || r == '"' || r == '\'' ||
		r == '(' || r == ')' || r == '[' || r == ']' ||
		r == '{' || r == '}' || r == '。' || r == '，' ||
		r == '！' || r == '？' || r == '；' || r == '：' ||
		r == '、' || r == '‘' || r == '’' || r == '“' || r == '”'
}

func isDigit(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			if r < 0xFF10 || r > 0xFF19 { // 全角数字
				return false
			}
		}
	}
	return len(s) > 0
}

// inferHeads 基于 POS 序列的规则式依存头推断。
// 动词通常作为根（head=0），名词依附于动词，形容词依附于名词。
func (p *ONNXParser) inferHeads(tokens []string, pos []string) []int {
	n := len(tokens)
	heads := make([]int, n)

	// 找到第一个动词作为根
	rootIdx := -1
	for i, tag := range pos {
		if tag == "VERB" {
			rootIdx = i
			break
		}
	}
	if rootIdx < 0 {
		rootIdx = 0
	}
	heads[rootIdx] = 0

	for i := 0; i < n; i++ {
		if i == rootIdx {
			continue
		}
		switch pos[i] {
		case "NOUN", "PROPN":
			// 名词指向最近的动词或前一个名词
			if i < rootIdx {
				heads[i] = rootIdx
			} else {
				heads[i] = rootIdx
			}
		case "ADJ", "ADV":
			// 修饰语指向前一个名词或动词
			if i > 0 {
				heads[i] = i - 1
			} else {
				heads[i] = rootIdx
			}
		case "NUM", "DET":
			// 限定词指向前一个名词
			if i > 0 {
				heads[i] = i - 1
			} else {
				heads[i] = rootIdx
			}
		case "PUNCT":
			heads[i] = rootIdx
		default:
			heads[i] = rootIdx
		}
	}
	return heads
}

// inferRels 基于 POS 对的关系标签推断。
func (p *ONNXParser) inferRels(tokens []string, pos []string, heads []int) []string {
	n := len(tokens)
	rels := make([]string, n)
	for i := 0; i < n; i++ {
		if heads[i] == 0 {
			rels[i] = "ROOT"
			continue
		}
		h := heads[i]
		if h < 0 || h >= n {
			rels[i] = "dep"
			continue
		}
		rels[i] = posToRel(pos[h], pos[i])
	}
	return rels
}

func posToRel(headPOS, depPOS string) string {
	switch {
	case depPOS == "NOUN" || depPOS == "PROPN":
		return "nsubj"
	case depPOS == "ADJ":
		return "amod"
	case depPOS == "ADV":
		return "advmod"
	case depPOS == "NUM" || depPOS == "DET":
		return "det"
	case depPOS == "VERB":
		return "xcomp"
	case depPOS == "PUNCT":
		return "punct"
	default:
		return "dep"
	}
}
