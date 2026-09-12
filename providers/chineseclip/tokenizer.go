// Package chineseclip 提供 Chinese-CLIP ViT-B/16 的 text+image 向量空间 provider。
//
// 为什么是它（而不是 Qwen3-VL-Embedding-2B / jina-v5-omni-nano）：
//   - 体积：721MB ONNX、实测常驻 1.15GB；Qwen 2B 需要 9.4GB，本机可用内存只有 5.3GB。
//   - 许可：Apache-2.0，可随发行版分发；jina-v5-omni-nano 是 CC BY-NC（不可商用）。
//   - 中文：原生在 ~2 亿中文图文对上训练。
//
// 代价（明确记录）：CLIP 是双塔对比学习，text↔image 是强项，但纯文本语义
// （text↔text）明显弱于 MLLM 型嵌入器。文本检索仍由既有词向量/TF-IDF 路径兜底，
// 本空间主要用于跨模态召回与相关性裁剪。需要视频或更强文本语义时应切回
// providers/qwen3vl（内存允许时）。
//
// 模态范围：仅 text 与 image。audio / video 返回 embedding.ErrUnsupportedModality，
// 绝不用别的模型向量冒充。
package chineseclip

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// BERT 的固定特殊 token（与官方 Chinese-CLIP 的 vocab.txt 一致）。
const (
	tokenCLS = "[CLS]"
	tokenSEP = "[SEP]"
	tokenPAD = "[PAD]"
	tokenUNK = "[UNK]"

	// maxInputCharsPerWord 与 HF BertTokenizer 一致：超过就整词判 UNK。
	maxInputCharsPerWord = 100
)

// Tokenizer 是 BERT WordPiece 分词器（Chinese-CLIP 官方配置：do_lower_case=true、
// strip_accents 生效、tokenize_chinese_chars=true）。
type Tokenizer struct {
	vocab     map[string]int32
	maxLength int
}

// LoadTokenizer 从模型目录读取 vocab.txt。目录里那份词表是产物的组成部分，
// provider 只依赖这个目录，不去猜任何外部路径。
func LoadTokenizer(dir string, maxLength int) (*Tokenizer, error) {
	path := filepath.Join(dir, "vocab.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("chineseclip: 读取词表 %s: %w", path, err)
	}
	if maxLength <= 0 {
		return nil, fmt.Errorf("chineseclip: max_length 必须为正，得到 %d", maxLength)
	}
	vocab := make(map[string]int32, 32768)
	for i, line := range strings.Split(string(data), "\n") {
		piece := strings.TrimRight(line, "\r")
		if piece == "" {
			continue
		}
		if _, dup := vocab[piece]; dup {
			// 词表出现重复行说明文件被写坏；不静默用后者覆盖前者。
			return nil, fmt.Errorf("chineseclip: 词表第 %d 行重复: %q", i+1, piece)
		}
		vocab[piece] = int32(len(vocab))
	}
	for _, special := range []string{tokenCLS, tokenSEP, tokenPAD, tokenUNK} {
		if _, ok := vocab[special]; !ok {
			return nil, fmt.Errorf("chineseclip: 词表缺少特殊 token %s", special)
		}
	}
	return &Tokenizer{vocab: vocab, maxLength: maxLength}, nil
}

// MaxLength 返回文本侧的最大 token 数（含特殊 token）。
func (t *Tokenizer) MaxLength() int { return t.maxLength }

// Encode 返回补齐到 maxLength 的 input_ids 与 attention_mask。
// attention_mask 与官方 tokenizer 的 padding='max_length' 行为一致：真实 token 为 1，
// padding 为 0。
func (t *Tokenizer) Encode(text string) ([]int64, []int64) {
	pieces := t.tokenize(text)

	// 预留 [CLS] 与 [SEP]；超长直接截断尾部（官方 truncation=True 的默认方向）。
	if limit := t.maxLength - 2; len(pieces) > limit {
		pieces = pieces[:limit]
	}

	ids := make([]int64, 0, t.maxLength)
	mask := make([]int64, 0, t.maxLength)
	ids = append(ids, int64(t.vocab[tokenCLS]))
	mask = append(mask, 1)
	for _, p := range pieces {
		ids = append(ids, int64(t.vocab[p]))
		mask = append(mask, 1)
	}
	ids = append(ids, int64(t.vocab[tokenSEP]))
	mask = append(mask, 1)

	for len(ids) < t.maxLength {
		ids = append(ids, int64(t.vocab[tokenPAD]))
		mask = append(mask, 0)
	}
	return ids, mask
}

// tokenize 复刻 HF BasicTokenizer + WordPieceTokenizer 的完整流水线。
func (t *Tokenizer) tokenize(text string) []string {
	var pieces []string
	for _, basic := range basicTokenize(text) {
		pieces = append(pieces, t.wordpiece(basic)...)
	}
	return pieces
}

// basicTokenize 实现 BasicTokenizer（空模型版）：清洗 → 中文逐字加空格 →
// 按空白切分 → 删音标 + 转小写 → 按标点再次切分。
func basicTokenize(text string) []string {
	cleaned := cleanText(text)
	var out []string
	for _, token := range strings.Fields(tokenizeChineseChars(cleaned)) {
		if len([]rune(token)) > maxInputCharsPerWord {
			// 与 HF 一致：超长基本 token 直接丢弃（后续不会产出 UNK）。
			continue
		}
		stripped := stripAccents(strings.ToLower(token))
		out = append(out, splitOnPunctuation(stripped)...)
	}
	return out
}

// cleanText 与 HF _clean_text 一致：丢弃 NUL/替换符与控制符，空白统一为空格。
func cleanText(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == 0 || r == 0xFFFD:
			continue
		case isControl(r):
			continue
		case isBERTWhitespace(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tokenizeChineseChars 在 CJK 字符两侧插入空格，使每个汉字成为独立基本 token。
func tokenizeChineseChars(text string) string {
	var b strings.Builder
	b.Grow(len(text) + 16)
	for _, r := range text {
		if isCJK(r) {
			b.WriteRune(' ')
			b.WriteRune(r)
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// stripAccents 与 HF _run_strip_accents 一致：NFD 分解后丢弃 Mn 组合记号
// （"café" → "cafe"）。
func stripAccents(text string) string {
	if isASCII(text) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range norm.NFD.String(text) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// splitOnPunctuation 与 HF _run_split_on_punc 一致：标点自成一段。
//
// 注意 ASCII 段必须显式列出：'$' '+' '=' '^' '`' '|' '~' 属于 Sc/Sm/Sk，
// 不是 Unicode P*，但它们也是标点（HF 用的是 ASCII 码点区间）。
func splitOnPunctuation(text string) []string {
	runes := []rune(text)
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range runes {
		if isBERTPunctuation(r) {
			flush()
			out = append(out, string(r))
			continue
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

// wordpiece 贪心最长匹配；整词任一段无法匹配则该词整体退化为 [UNK]。
func (t *Tokenizer) wordpiece(token string) []string {
	runes := []rune(token)
	if len(runes) > maxInputCharsPerWord {
		return []string{tokenUNK}
	}
	var out []string
	start := 0
	for start < len(runes) {
		end := len(runes)
		var cur string
		found := false
		for end > start {
			piece := string(runes[start:end])
			if start > 0 {
				piece = "##" + piece
			}
			if _, ok := t.vocab[piece]; ok {
				cur = piece
				found = true
				break
			}
			end--
		}
		if !found {
			return []string{tokenUNK}
		}
		out = append(out, cur)
		start = end
	}
	return out
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// isBERTWhitespace：HF _is_whitespace = 空格/制表/换行/回车 或 Unicode Zs。
func isBERTWhitespace(r rune) bool {
	if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

// isControl：HF _is_control = Cc/Cf，但制表/换行/回车不算。
func isControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r)
}

// isBERTPunctuation：ASCII 标点区间 或 Unicode P*。
func isBERTPunctuation(r rune) bool {
	if (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126) {
		return true
	}
	return unicode.IsPunct(r)
}

// isCJK：HF _tokenize_chinese_chars 使用的区间表。
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF,
		r >= 0x3400 && r <= 0x4DBF,
		r >= 0x20000 && r <= 0x2A6DF,
		r >= 0x2A700 && r <= 0x2B73F,
		r >= 0x2B740 && r <= 0x2B81F,
		r >= 0x2B820 && r <= 0x2CEAF,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0x2F800 && r <= 0x2FA1F:
		return true
	}
	return false
}
