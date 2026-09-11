// Package qwen 实现 Qwen3-VL-Embedding 的字节级 BPE 分词器。
//
// 为什么不复用 clip 的 tokenizer：CLIP 用的是「小写化 + 空白规整 + 词表 BPE」，
// 而千问是 **GPT-2 式字节级 BPE**——先把输入按字节映射到一组可见 unicode，
// 再对映射后的字符串做 BPE 合并。两者的预处理不可互换，硬套会在中文和
// 空白较多的输入上产出完全不同的 token。
//
// 与上游（HuggingFace tokenizer.json 的 Rust 实现）对齐时的两处坑：
//
//  1. pre_tokenizer 正则里的 `\s+(?!\S)` 是**负向前瞻**，Go 的 RE2 不支持
//     lookaround。该分支只在「空白一直延伸到串尾」时命中，而此时贪婪的
//     `\s+` 会匹配完全相同的区间，所以直接删掉该分支即为等价改写。
//  2. Go 的 `\s` 只覆盖 ASCII，而 Rust regex 的 `\s` 是 Unicode
//     `\p{White_Space}`。不换成 \p{White_Space} 的话，全角空格、NBSP、
//     行分隔符等的切分点会与上游不一致。
package qwen3vl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 空白判定统一用 unicode.IsSpace（Unicode White_Space 属性）。
//
// 不能用 Go 正则里的 \s——那只覆盖 ASCII；也不能写 \p{White_Space}——Go 的
// regexp 只支持 script/category，不支持二进制属性（会报 invalid character
// class range）。上游 Rust regex 的 \s 正是 White_Space，所以这里以
// unicode.IsSpace 为准。

// specialToken 是一个 AddedToken：以整体形式优先匹配，不参与 BPE 拆分。
type specialToken struct {
	content string
	id      int
}

// Tokenizer 是千问的字节级 BPE 分词器。
type Tokenizer struct {
	vocab map[string]int
	ranks map[string]int

	// byteEnc 是 GPT-2 的 byte→unicode 映射：把 0..255 每个字节映到一个
	// 「可见且不会与正常文本冲突」的 unicode 码点。因为 BPE 词表基于文本构建，
	// 直接放原始字节会与合法 UTF-8 冲突。
	byteEnc map[byte]rune

	// specials 按 content 长度降序，保证「最长优先」——
	// 否则 `<|im_start|>` 可能被 `<|im_` 之类的短 token 先切走。
	specials []specialToken

	// MaxLen 是嵌入用途的截断上限（与导出脚本的 MAX_LENGTH 一致）。
	MaxLen int
}

// tokenizerJSON 只取我们需要的部分。
type tokenizerJSON struct {
	Model struct {
		Vocab  map[string]int `json:"vocab"`
		Merges []interface{}  `json:"merges"`
	} `json:"model"`
	AddedTokens []struct {
		ID      int    `json:"id"`
		Content string `json:"content"`
		Special bool   `json:"special"`
	} `json:"added_tokens"`
}

// LoadTokenizer 从模型目录加载 tokenizer.json。
func LoadTokenizer(modelDir string) (*Tokenizer, error) {
	raw, err := os.ReadFile(filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("read tokenizer.json: %w", err)
	}
	var tj tokenizerJSON
	if err := json.Unmarshal(raw, &tj); err != nil {
		return nil, fmt.Errorf("parse tokenizer.json: %w", err)
	}
	if len(tj.Model.Vocab) == 0 {
		return nil, fmt.Errorf("tokenizer.json 的 model.vocab 为空")
	}

	ranks := make(map[string]int, len(tj.Model.Merges))
	for i, m := range tj.Model.Merges {
		// merges 有两种形态：字符串 "a b"，或数组 ["a","b"]。
		var pair string
		switch v := m.(type) {
		case string:
			pair = v
		case []interface{}:
			if len(v) == 2 {
				a, _ := v[0].(string)
				b, _ := v[1].(string)
				pair = a + " " + b
			}
		}
		if pair != "" {
			if _, seen := ranks[pair]; !seen {
				ranks[pair] = i
			}
		}
	}

	t := &Tokenizer{
		vocab:   tj.Model.Vocab,
		ranks:   ranks,
		byteEnc: bytesToUnicode(),
		MaxLen:  512,
	}
	for _, at := range tj.AddedTokens {
		if at.Special && at.Content != "" {
			t.specials = append(t.specials, specialToken{content: at.Content, id: at.ID})
		}
	}
	// 最长优先，避免短 token 抢走长 token 的前缀。
	sort.Slice(t.specials, func(i, j int) bool {
		return len(t.specials[i].content) > len(t.specials[j].content)
	})
	return t, nil
}

// VocabSize 返回词表大小（诊断用）。
func (t *Tokenizer) VocabSize() int { return len(t.vocab) }

// SpecialID 返回特殊 token 的 id；不存在时 ok=false。
func (t *Tokenizer) SpecialID(content string) (int, bool) {
	for _, s := range t.specials {
		if s.content == content {
			return s.id, true
		}
	}
	return 0, false
}

// DefaultInstruction 是导出脚本随 embed_config.json 写入的默认指令。
const DefaultInstruction = "Represent the user's input."

// renderInstructionInput 按模型自带的对话模板拼输入（无构建标签，便于测试）。
//
// 必须与 HuggingFace processor 的 apply_chat_template(add_generation_prompt=True)
// 产出完全一致：指令放 system、正文放 user、以 assistant 起始符结尾。差一个
// 特殊 token，池化取到的「最后一个有效 token」位置就变了，嵌入也就不同——
// 而且不会报错。参考数据集里有该模板串的用例，能逐 token 对齐验证。
func renderInstructionInput(instruction, text string) string {
	if instruction == "" {
		instruction = DefaultInstruction
	}
	return "<|im_start|>system\n" + instruction +
		"<|im_end|>\n<|im_start|>user\n" + text +
		"<|im_end|>\n<|im_start|>assistant\n"
}

// Encode 把文本编码为 token id 序列（识别输入中已有的特殊 token，
// 但不执行 tokenizer.json 的 post_processor，也不做截断）。
func (t *Tokenizer) Encode(text string) []int {
	var ids []int
	for _, seg := range t.splitSpecials(text) {
		if seg.specialID >= 0 {
			ids = append(ids, seg.specialID)
			continue
		}
		ids = append(ids, t.encodeOrdinary(seg.text)...)
	}
	return ids
}

// encodeModelInput 执行 TextTower 输入所需的 tokenizer post_processor。
//
// tokenizer.json 的 TemplateProcessing 规则是 `$A <|endoftext|>`；HuggingFace
// 在 truncation=true 时先把 A 截到 maxLen-1，再保留末尾 post token。漏掉它不会
// 触发 ONNX 错误，却会改变池化位置和整条嵌入向量，因此不能直接用 Encode 的结果。
func (t *Tokenizer) encodeModelInput(text string, maxLen int) ([]int, error) {
	postID, ok := t.SpecialID("<|endoftext|>")
	if !ok {
		return nil, fmt.Errorf("tokenizer.json 缺少 post token <|endoftext|>")
	}
	if maxLen <= 0 {
		return nil, fmt.Errorf("maxLen 必须大于 0")
	}

	ids := t.Encode(text)
	if len(ids) >= maxLen {
		ids = ids[:maxLen-1]
	}
	return append(ids, postID), nil
}

// seg 是「普通文本」或「已识别的特殊 token」二选一。
type seg struct {
	text      string
	specialID int // -1 表示普通文本
}

// splitSpecials 把输入切成普通片段与特殊 token 片段。
//
// 为什么必须先切：`<|im_start|>` 在词表里是一个整体 id（151644），若走 BPE
// 会被拆成若干子 token，编码结果与上游不一致，模型看到的输入也就变了。
func (t *Tokenizer) splitSpecials(text string) []seg {
	if len(t.specials) == 0 || text == "" {
		return []seg{{text: text, specialID: -1}}
	}
	var out []seg
	for len(text) > 0 {
		// 找最靠前的特殊 token 出现位置（同位置取最长）。
		bestIdx, bestLen, bestID := -1, 0, -1
		for _, s := range t.specials {
			i := strings.Index(text, s.content)
			if i < 0 {
				continue
			}
			if bestIdx == -1 || i < bestIdx || (i == bestIdx && len(s.content) > bestLen) {
				bestIdx, bestLen, bestID = i, len(s.content), s.id
			}
		}
		if bestIdx == -1 {
			out = append(out, seg{text: text, specialID: -1})
			break
		}
		if bestIdx > 0 {
			out = append(out, seg{text: text[:bestIdx], specialID: -1})
		}
		out = append(out, seg{specialID: bestID})
		text = text[bestIdx+bestLen:]
	}
	return out
}

// encodeOrdinary 对普通文本做「切分 → 字节映射 → BPE 合并」。
func (t *Tokenizer) encodeOrdinary(text string) []int {
	if text == "" {
		return nil
	}
	var ids []int
	for _, piece := range t.preTokenize(text) {
		// 字节级映射：先把 piece 的 UTF-8 字节逐个映射成 unicode 字符。
		var sb strings.Builder
		for _, b := range []byte(piece) {
			sb.WriteRune(t.byteEnc[b])
		}
		for _, tok := range t.bpe(sb.String()) {
			if id, ok := t.vocab[tok]; ok {
				ids = append(ids, id)
			}
			// 词表里找不到的片段直接丢弃：正常情况不会发生
			//（词表覆盖全部 256 个字节级字符），发生即数据有问题。
		}
	}
	return ids
}

// ---- pre_tokenizer ----
//
// 上游是一条正则（tokenizer.json 的 pre_tokenizer.pretokenizers[0].pattern）：
//
//	(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}|
//	 ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+
//
// **为什么不用一个 Go 正则**：末两个分支里的 `\s+(?!\S)` 是负向前瞻，RE2 不
// 支持 lookaround；而且它的真实语义依赖**回溯**——`\s+` 先贪婪吃完整段空白，
// 发现后面是非空白导致 `(?!\S)` 失败，于是回退一个字符，正好留下末尾一个
// 空白给前面那些以 ` ?` / `[^…]?` 开头的分支合并。这个“留一个”直接决定
// 切分点（`"   leading"` 会切成 `"  "` + `" leading"` 而不是 `"   "` + `"leading"`），
// 近似改写必然对不上，所以按分支顺序显式实现。
func (t *Tokenizer) preTokenize(text string) []string {
	var out []string
	for len(text) > 0 {
		switch {
		case matchApostrophe(text) > 0:
			n := matchApostrophe(text)
			out = append(out, text[:n])
			text = text[n:]
		case matchWord(text) > 0:
			n := matchWord(text)
			out = append(out, text[:n])
			text = text[n:]
		case matchDigit(text) > 0:
			n := matchDigit(text)
			out = append(out, text[:n])
			text = text[n:]
		case matchPunct(text) > 0:
			n := matchPunct(text)
			out = append(out, text[:n])
			text = text[n:]
		case matchNewline(text) > 0:
			n := matchNewline(text)
			out = append(out, text[:n])
			text = text[n:]
		default:
			// `\s+(?!\S)|\s+` 合一：空白段。
			total, lastStart := wsRun(text)
			if total == 0 {
				// 兜底：不应到达（分支覆盖全部字符），防御性前进一个 rune。
				_, size := utf8.DecodeRuneInString(text)
				out = append(out, text[:size])
				text = text[size:]
				continue
			}
			n := total
			if total < len(text) && lastStart > 0 {
				n = lastStart // 后面还有非空白 → 回退掉末尾那一个空白
			}
			out = append(out, text[:n])
			text = text[n:]
		}
	}
	return out
}

func runeAt(s string) (rune, int) { return utf8.DecodeRuneInString(s) }

func isLetter(r rune) bool { return unicode.IsLetter(r) }
func isNumber(r rune) bool { return unicode.IsNumber(r) }
func isWS(r rune) bool     { return unicode.IsSpace(r) }

// wsRun 返回开头连续空白段的字节长度，以及最后一个空白 rune 的起始字节位置。
func wsRun(s string) (total, lastStart int) {
	lastStart = -1
	i := 0
	for i < len(s) {
		r, size := runeAt(s[i:])
		if !isWS(r) {
			break
		}
		lastStart = i
		i += size
	}
	return i, lastStart
}

// matchApostrophe：`(?i:'s|'t|'re|'ve|'m|'ll|'d)`
func matchApostrophe(s string) int {
	if len(s) == 0 || s[0] != '\'' {
		return 0
	}
	rest := s[1:]
	// 各后缀互为前缀关系（re/ve/ll/s/t/m/d），所以先试长的。
	for _, suf := range []string{"re", "ve", "ll", "s", "t", "m", "d"} {
		if len(rest) >= len(suf) && strings.EqualFold(rest[:len(suf)], suf) {
			return 1 + len(suf)
		}
	}
	return 0
}

// matchWord：`[^\r\n\p{L}\p{N}]?\p{L}+`
//
// 注意可选字符**排除** \r \n；若吃了可选字符却没有字母跟上，整个分支失败
// （与正则的“该分支不匹配”一致，不能把可选字符当已消耗）。
func matchWord(s string) int {
	i := 0
	if r, size := runeAt(s); r != '\r' && r != '\n' && !isLetter(r) && !isNumber(r) {
		i = size
	}
	r, size := runeAt(s[i:])
	if !isLetter(r) {
		return 0
	}
	i += size
	for i < len(s) {
		r, size := runeAt(s[i:])
		if !isLetter(r) {
			break
		}
		i += size
	}
	return i
}

// matchDigit：`\p{N}` —— 只吃**一个**数字。
func matchDigit(s string) int {
	if r, size := runeAt(s); isNumber(r) {
		return size
	}
	return 0
}

// matchPunct：` ?[^\s\p{L}\p{N}]+[\r\n]*`
//
// 开头是**字面空格**（不是 \s），所以只可能吃掉一个 U+0020。
func matchPunct(s string) int {
	i := 0
	if strings.HasPrefix(s, " ") {
		i = 1
	}
	n := 0
	for i+n < len(s) {
		r, size := runeAt(s[i+n:])
		if isWS(r) || isLetter(r) || isNumber(r) {
			break
		}
		n += size
	}
	if n == 0 {
		return 0
	}
	i += n
	for i < len(s) && (s[i] == '\r' || s[i] == '\n') {
		i++
	}
	return i
}

// matchNewline：`\s*[\r\n]+`
//
// 贪婪+回溯的真实语义：`\s*` 先吃完整段空白，`[\r\n]+` 无可匹配而回退，
// 最终停在段内**最后一个** \r 或 \n 之前，再把它之后的连续 \r\n 吃掉。
func matchNewline(s string) int {
	total, _ := wsRun(s)
	if total == 0 {
		return 0
	}
	last := -1
	for j := total - 1; j >= 0; j-- {
		if s[j] == '\r' || s[j] == '\n' {
			last = j
			break
		}
	}
	if last < 0 {
		return 0
	}
	end := last
	for end < len(s) && (s[end] == '\r' || s[end] == '\n') {
		end++
	}
	return end
}

// bpe 是标准字节级 BPE：反复合并 rank 最小的相邻对，直到无可合并。
func (t *Tokenizer) bpe(word string) []string {
	symbols := make([]string, 0, len(word))
	for _, r := range word {
		symbols = append(symbols, string(r))
	}
	if len(symbols) < 2 {
		return symbols
	}

	for {
		bestRank, bestIdx := -1, -1
		for i := 0; i+1 < len(symbols); i++ {
			r, ok := t.ranks[symbols[i]+" "+symbols[i+1]]
			if !ok {
				continue
			}
			if bestRank == -1 || r < bestRank {
				bestRank, bestIdx = r, i
			}
		}
		if bestIdx == -1 {
			return symbols
		}
		merged := symbols[bestIdx] + symbols[bestIdx+1]
		symbols = append(symbols[:bestIdx], append([]string{merged}, symbols[bestIdx+2:]...)...)
		if len(symbols) < 2 {
			return symbols
		}
	}
}

// bytesToUnicode 是 GPT-2 的字节↔unicode 映射表。
//
// 让每个字节都有一个「安全」的可见码点表示，避免原始控制字节混进 BPE 词表。
// 可打印 ASCII 与拉丁补充区保持原样，其余字节映射到 256 之后的码点。
func bytesToUnicode() map[byte]rune {
	bs := make([]int, 0, 256)
	for b := int('!'); b <= int('~'); b++ {
		bs = append(bs, b)
	}
	for b := 0xA1; b <= 0xAC; b++ {
		bs = append(bs, b)
	}
	for b := 0xAE; b <= 0xFF; b++ {
		bs = append(bs, b)
	}

	inBS := make(map[int]bool, len(bs))
	for _, b := range bs {
		inBS[b] = true
	}

	cs := make([]int, len(bs))
	copy(cs, bs)
	n := 0
	for b := 0; b < 256; b++ {
		if inBS[b] {
			continue
		}
		bs = append(bs, b)
		cs = append(cs, 256+n)
		n++
	}

	out := make(map[byte]rune, 256)
	for i, b := range bs {
		out[byte(b)] = rune(cs[i])
	}
	return out
}
