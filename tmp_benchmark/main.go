package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

type benchDoc struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	Content string `json:"content"`
}

type queryCase struct {
	Name  string
	Query string
	Seeds []string
}

type hit struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

type result struct {
	Name       string  `json:"name"`
	Query      string  `json:"query"`
	Relevant   int     `json:"relevant"`
	Rank       int     `json:"rank"`
	Reciprocal float64 `json:"reciprocal_rank"`
	HitAt1     bool    `json:"hit_at_1"`
	HitAt5     bool    `json:"hit_at_5"`
	LatencyMS  float64 `json:"latency_ms"`
	Top        []hit   `json:"top"`
}

type report struct {
	Documents int                 `json:"documents"`
	Queries   []queryCase         `json:"queries"`
	Methods   map[string][]result `json:"methods"`
}

var cases = []queryCase{
	{Name: "mail-semantic", Query: "邮件代理是否已经成功接入", Seeds: []string{"AgentMail 接入验证"}},
	{Name: "fox-cross-language", Query: "生成一张雪地红狐狸的图片", Seeds: []string{"red fox in snowy forest"}},
	{Name: "plugin-semantic", Query: "升级安装 QQ 插件包", Seeds: []string{"plugin_install"}},
	{Name: "weather-paraphrase", Query: "我所在城市的天气预报", Seeds: []string{"河南新乡"}},
	{Name: "textarea-paraphrase", Query: "聊天输入区域文字多了会不会自动增高", Seeds: []string{"输入框在内容超过一行"}},
	{Name: "devices-paraphrase", Query: "检查当前接入了哪些终端设备", Seeds: []string{"你看看现在你都有哪些设备"}},
	{Name: "memory-health", Query: "长期文档记忆功能是否健康", Seeds: []string{"文档记忆系统是否正常工作"}},
	{Name: "reload-plugins", Query: "重新加载全部扩展组件", Seeds: []string{"热重载所有插件", "plgreload"}},
	{Name: "exact-agentmail", Query: "AgentMail 接入验证", Seeds: []string{"AgentMail 接入验证"}},
	{Name: "exact-plugin", Query: "plugin_install", Seeds: []string{"plugin_install"}},
}

func main() {
	docs, err := loadDocs("/data/homeagent/memory/documents")
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(os.Stderr, "loaded %d production documents\n", len(docs))

	rel := relevantSets(docs)
	for i, c := range cases {
		fmt.Fprintf(os.Stderr, "case %-20s relevant=%d query=%q\n", c.Name, len(rel[i]), c.Query)
	}

	r := report{Documents: len(docs), Queries: cases, Methods: make(map[string][]result)}

	// 方案 A：纯 TF-IDF。完整训练在生产文档上，保留 IDF 高频抑制与倒排候选剪枝。
	tfidf := vector.NewTFIDFVectorizer(memory.TokenizeWords)
	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Summary + "\n" + d.Content
	}
	tfidf.Train(texts)
	tfStore := buildStore(docs, tfidf)
	r.Methods["tfidf"] = runCases(tfStore, tfidf, rel)

	// 方案 B：当前生产 fastText（中英各 20/37 万词，300 维平均词向量）。
	fast := memory.NewStaticEmbedder("/data/cc.zh.top200k.vec", "/data/cc.en.top200k.vec")
	fastStore := buildStore(docs, fast)
	r.Methods["fasttext"] = runCases(fastStore, fast, rel)

	// 方案 C：旧通道混合。RRF 不要求两种分数处于同一标尺，避免拍脑袋设绝对权重。
	r.Methods["tfidf_fasttext_rrf"] = runHybrid(tfStore, tfidf, fastStore, fast, rel)

	out, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile("/tmp/homeagent-old-retrieval.json", out, 0644); err != nil {
		panic(err)
	}
	printSummary(r)
}

func loadDocs(dir string) ([]benchDoc, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var docs []benchDoc
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "doc_") || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var d document.Doc
		if json.Unmarshal(b, &d) != nil || d.ID == "" {
			continue
		}
		docs = append(docs, benchDoc{ID: d.ID, Summary: d.Summary, Content: d.Content})
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	return docs, nil
}

func relevantSets(docs []benchDoc) []map[string]bool {
	sets := make([]map[string]bool, len(cases))
	for i, c := range cases {
		sets[i] = make(map[string]bool)
		for _, d := range docs {
			text := strings.ToLower(d.Summary + "\n" + d.Content)
			for _, seed := range c.Seeds {
				if strings.Contains(text, strings.ToLower(seed)) {
					sets[i][d.ID] = true
					break
				}
			}
		}
	}
	return sets
}

type textVectorizer interface {
	Vectorize(text string) vector.Vector
}

func buildStore(docs []benchDoc, v textVectorizer) *vector.Store {
	s := vector.NewStore()
	for _, d := range docs {
		s.Insert(d.ID, d.Summary, v.Vectorize(d.Summary+"\n"+d.Content), nil)
	}
	return s
}

func runCases(s *vector.Store, v textVectorizer, rel []map[string]bool) []result {
	out := make([]result, 0, len(cases))
	for i, c := range cases {
		start := time.Now()
		hs := s.SearchScored(v.Vectorize(c.Query), s.Size())
		lat := time.Since(start)
		ids := make([]hit, len(hs))
		for j, h := range hs {
			ids[j] = hit{ID: h.Doc.ID, Score: h.Score}
		}
		out = append(out, measure(c, ids, rel[i], lat))
	}
	return out
}

func runHybrid(a *vector.Store, av textVectorizer, b *vector.Store, bv textVectorizer, rel []map[string]bool) []result {
	out := make([]result, 0, len(cases))
	for i, c := range cases {
		start := time.Now()
		ah := a.SearchScored(av.Vectorize(c.Query), a.Size())
		bh := b.SearchScored(bv.Vectorize(c.Query), b.Size())
		scores := make(map[string]float64)
		const k = 60.0
		for rank, h := range ah {
			scores[h.Doc.ID] += 1 / (k + float64(rank+1))
		}
		for rank, h := range bh {
			scores[h.Doc.ID] += 1 / (k + float64(rank+1))
		}
		ids := make([]hit, 0, len(scores))
		for id, score := range scores {
			ids = append(ids, hit{ID: id, Score: score})
		}
		sort.Slice(ids, func(i, j int) bool {
			if ids[i].Score == ids[j].Score {
				return ids[i].ID < ids[j].ID
			}
			return ids[i].Score > ids[j].Score
		})
		out = append(out, measure(c, ids, rel[i], time.Since(start)))
	}
	return out
}

func measure(c queryCase, ranked []hit, relevant map[string]bool, latency time.Duration) result {
	rank := 0
	for i, h := range ranked {
		if relevant[h.ID] {
			rank = i + 1
			break
		}
	}
	topN := 5
	if len(ranked) < topN {
		topN = len(ranked)
	}
	r := result{Name: c.Name, Query: c.Query, Relevant: len(relevant), Rank: rank, LatencyMS: float64(latency.Microseconds()) / 1000, Top: append([]hit(nil), ranked[:topN]...)}
	if rank > 0 {
		r.Reciprocal = 1 / float64(rank)
		r.HitAt1 = rank <= 1
		r.HitAt5 = rank <= 5
	}
	return r
}

func printSummary(r report) {
	fmt.Printf("documents=%d queries=%d\n", r.Documents, len(r.Queries))
	names := make([]string, 0, len(r.Methods))
	for name := range r.Methods {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rs := r.Methods[name]
		var h1, h5 int
		var mrr, lat float64
		for _, x := range rs {
			if x.HitAt1 {
				h1++
			}
			if x.HitAt5 {
				h5++
			}
			mrr += x.Reciprocal
			lat += x.LatencyMS
		}
		fmt.Printf("%-24s Hit@1=%d/%d Hit@5=%d/%d MRR=%.4f avg-query=%.3fms\n", name, h1, len(rs), h5, len(rs), mrr/float64(len(rs)), lat/float64(len(rs)))
		for _, x := range rs {
			fmt.Printf("  %-20s rank=%-4d latency=%7.3fms", x.Name, x.Rank, x.LatencyMS)
			if len(x.Top) > 0 {
				fmt.Printf(" top=%s score=%.4g", x.Top[0].ID, x.Top[0].Score)
			}
			fmt.Println()
		}
	}
	_ = math.MaxFloat64
}
