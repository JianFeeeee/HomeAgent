package memory

import (
	"log"
	"os"
	"path/filepath"
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
	candidates := []string{
		os.Getenv("GOMODCACHE"),
		os.Getenv("GOPATH"),
		filepath.Join(os.Getenv("HOME"), "go"),
		"/root/go",
		"/go",
		"/home/program/go",
	}
	for _, base := range candidates {
		if base == "" {
			continue
		}
		d := filepath.Join(base, "pkg", "mod", "github.com", "yanyiwu", "gojieba@v1.4.7", "deps", "cppjieba", "dict")
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return d
		}
		// also try without "pkg/mod" (in case GOPATH is already the mod cache)
		d2 := filepath.Join(base, "github.com", "yanyiwu", "gojieba@v1.4.7", "deps", "cppjieba", "dict")
		if info, err := os.Stat(d2); err == nil && info.IsDir() {
			return d2
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

func ExtractKeywords(text string) []string {
	text = CleanTemplateText(text)
	x := GetJieba()
	if x == nil {
		return nil
	}
	words := x.Cut(text, true)
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
