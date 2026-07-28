package memory

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yanyiwu/gojieba"
)

var (
	jiebaOnce sync.Once
	jiebaInst *gojieba.Jieba
)

func GetJieba() *gojieba.Jieba {
	jiebaOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[jieba] init panic recovered: %v", r)
			}
		}()
		d := jiebaDictDir()
		if d == "" {
			log.Printf("[jieba] no dictionary directory found, jieba disabled")
			return
		}
		jiebaInst = gojieba.NewJieba(
			filepath.Join(d, "jieba.dict.utf8"),
			filepath.Join(d, "hmm_model.utf8"),
			filepath.Join(d, "user.dict.utf8"),
			filepath.Join(d, "idf.utf8"),
			filepath.Join(d, "stop_words.utf8"),
		)
	})
	return jiebaInst
}

func jiebaDictDir() string {
	// GOMODCACHE is typically $GOPATH/pkg/mod. When set, Go writes modules
	// under <GOMODCACHE>/github.com/... . Look first at GOMODCACHE, then
	// derive from GOPATH, then try common locations.
	candidates := []string{
		os.Getenv("GOMODCACHE"),
	}
	if gp := os.Getenv("GOPATH"); gp != "" {
		candidates = append(candidates, filepath.Join(gp, "pkg", "mod"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, "go", "pkg", "mod"))
	}
	if h := os.Getenv("HOME"); h != "" {
		candidates = append(candidates, filepath.Join(h, "go", "pkg", "mod"))
	}

	suffix := filepath.Join("github.com", "yanyiwu", "gojieba@v1.4.7", "deps", "cppjieba", "dict")
	for _, base := range candidates {
		if base == "" {
			continue
		}
		d := filepath.Join(base, suffix)
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return d
		}
	}
	return ""
}

var stopWords = map[string]bool{
	"的": true, "了": true, "是": true, "在": true, "有": true,
	"和": true, "就": true, "不": true, "人": true, "都": true,
	"一": true, "一个": true, "上": true, "也": true, "很": true,
	"到": true, "说": true, "要": true, "去": true, "你": true,
	"会": true, "着": true, "没有": true, "看": true, "好": true,
	"自己": true, "这": true, "他": true, "她": true, "它": true,
	"我": true, "我们": true, "你们": true, "他们": true,
	"吗": true, "吧": true, "啊": true,
	"嗯": true, "哦": true, "哈": true, "呀": true, "嘛": true,
	"然后": true, "因为": true, "所以": true, "如果": true, "但是": true,
	"可能": true, "还是": true, "已经": true,
	"就是": true, "不是": true, "是的": true,
	"非常": true, "比较": true, "应该": true, "需要": true,
	"能够": true, "目前": true, "现在": true, "今天": true, "昨天": true,
	"明天": true, "知道": true, "觉得": true, "认为": true,
	"能": true, "没": true, "对": true,
	"the": true, "a": true, "an": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "do": true, "does": true,
	"did": true, "will": true, "would": true, "could": true, "should": true,
	"may": true, "might": true, "can": true, "shall": true, "this": true,
	"that": true, "these": true, "those": true, "it": true, "its": true,
	"and": true, "or": true, "but": true, "in": true, "on": true,
	"at": true, "to": true, "for": true, "of": true, "with": true,
	"what": true, "how": true, "why": true, "which": true, "where": true,
	"when": true, "who": true, "whom": true,
}

// TokenizeWords 使用 jieba 精确模式分词，返回去重后的所有词 token（不过滤停用词）
func TokenizeWords(text string) []string {
	text = CleanText(text)
	x := GetJieba()
	if x == nil {
		return nil
	}
	words := x.Cut(text, false)
	var result []string
	seen := make(map[string]bool)
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		result = append(result, w)
	}
	return result
}

func ExtractKeywords(text string) []string {
	text = CleanText(text)
	x := GetJieba()
	if x == nil {
		return nil
	}
	words := x.Cut(text, false)
	var keywords []string
	seen := make(map[string]bool)
	for _, w := range words {
		if stopWords[w] || seen[w] {
			continue
		}
		r := []rune(w)
		if len(r) < 2 {
			continue
		}
		seen[w] = true
		keywords = append(keywords, w)
	}
	if len(keywords) > 5 {
		keywords = keywords[:5]
	}
	return keywords
}

// CutExact 精确模式分词：返回去停用词后的所有有义项（不限数量），用于 doc→graph 蒸馏
func CutExact(text string) []string {
	text = CleanText(text)
	x := GetJieba()
	if x == nil {
		return nil
	}
	words := x.Cut(text, false)
	var result []string
	seen := make(map[string]bool)
	for _, w := range words {
		if stopWords[w] || seen[w] {
			continue
		}
		if !validEntityName(w) {
			continue
		}
		seen[w] = true
		result = append(result, w)
	}
	return result
}
