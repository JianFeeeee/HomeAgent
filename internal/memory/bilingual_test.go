package memory

import (
	"fmt"
	"sort"
	"testing"
)

type bilingualEvent struct {
	idx     int
	source  string
	topic   string
	text    string // cleaned text for vectorization
	label   string // short description
}

func TestBilingualPruningAccuracy(t *testing.T) {
	zhPath := "/tmp/cc.zh.top200k.vec"
	enPath := "/tmp/cc.en.top200k.vec"

	// Test with Chinese-only vs Chinese+English
	type modelConfig struct {
		name  string
		paths []string
	}

	configs := []modelConfig{
		{"中文仅", []string{zhPath}},
		{"中文+英文", []string{zhPath, enPath}},
	}

	events := genBilingualEvents()

	for _, cfg := range configs {
		t.Run(cfg.name, func(t *testing.T) {
			e := NewStaticEmbedder(cfg.paths...)
			if !e.Loaded() {
				t.Skipf("%s: embedder not loaded", cfg.name)
			}
			t.Logf("%s: %d words", cfg.name, len(e.words))

			type scored struct {
				idx    int
				topic  string
				label  string
				score  float64
			}

			queries := []struct {
				q       string
				qTopic  string
				desc    string
			}{
				{"老大说了关于 React 组件的事情", "老大私聊", "中英混合:老大+React"},
				{"帮我查一下 Nginx 反向代理配置", "服务器运维", "中英混合:Nginx+反向代理"},
				{"河南医药大学 Docker 部署", "大学招生", "中英混合:大学+Docker"},
				{"JavaScript 基金定投收益计算", "股票基金", "中英混合:JS+基金"},
				{"Server 前端组件封装 layout", "前端开发", "中英混合:Server+layout"},
				{"河南医药大学录取分数线", "大学招生", "纯中文:大学"},
				{"nginx reverse proxy config", "服务器运维", "纯英文:nginx"},
			}

			for _, q := range queries {
				qVec := e.Vectorize(q.q)
				t.Logf("\n  query: %q (%s)", q.q, q.desc)

				all := make([]scored, len(events))
				for i, ev := range events {
					text := textForBilingual(ev, cfg.paths)
					vec := e.Vectorize(text)
					all[i] = scored{idx: i, topic: ev.topic, label: ev.label, score: cosineSim(qVec, vec)}
				}
				sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })

				// Check top 5 for same-topic presence
				var intraHits int
				for _, s := range all[:5] {
					if s.topic == q.qTopic {
						intraHits++
					}
				}

				topScore := all[0]
				topIsCorrect := topScore.topic == q.qTopic

				t.Logf("    top5 intra=%d/5, top1=%q(%s) score=%.4f %s",
					intraHits, topScore.topic, topScore.label, topScore.score,
					map[bool]string{true: "✅", false: "❌"}[topIsCorrect])
				for _, s := range all[:5] {
					mark := ""
					if s.topic == q.qTopic {
						mark = " ✓"
					}
					t.Logf("      [%.4f] [%-12s] %s%s", s.score, s.topic, trimLen(s.label, 50), mark)
				}

				if !topIsCorrect {
					t.Logf("    [WARN] top1 mismatch for %q", q.desc)
				}
			}
		})
	}
}

func TestBilingualCrossLingualSimilarity(t *testing.T) {
	zhPath := "/tmp/cc.zh.top200k.vec"
	enPath := "/tmp/cc.en.top200k.vec"
	e := NewStaticEmbedder(zhPath, enPath)
	if !e.Loaded() {
		t.Skip("embedder not loaded")
	}

	pairs := []struct {
		a, b string
		desc string
	}{
		{"server", "服务器", "英中同义"},
		{"computer", "电脑", "英中同义"},
		{"老大", "boss", "中英同义"},
		{"大学", "university", "中英同义"},
		{"Nginx", "服务器", "专名+普通"},
		{"股票", "stock", "中英同义"},
		{"React", "前端", "专名+概念"},
		{"JavaScript", "编程", "专名+概念"},
		{"老大私聊", "boss private chat", "中英短语"},
		{"河南医药大学录取", "Henan Medical University admission", "中英专名"},
		{"nginx config", "Nginx 配置", "英中技术"},
	}

	t.Log("=== 跨语言相似度 ===")
	for _, p := range pairs {
		va := e.Vectorize(p.a)
		vb := e.Vectorize(p.b)
		sim := cosineSim(va, vb)
		t.Logf("  %.4f  %q  ↔  %q  [%s]", sim, trimLen(p.a, 30), trimLen(p.b, 30), p.desc)
	}
}

func TestBilingualEdgeCases(t *testing.T) {
	zhPath := "/tmp/cc.zh.top200k.vec"
	e := NewStaticEmbedder(zhPath)
	if !e.Loaded() {
		t.Skip("embedder not loaded")
	}

	cases := []string{
		"纯英文文本 nginx react docker javascript",
		"纯中文 服务器 配置 反向代理 部署",
		"中英混合 nginx 反向代理 配置",
		"代码片段 const foo = 'bar'; function test()",
		"URL路径 /api/v1/users/123",
		"中文含标点！@#￥%……&*（）",
		"空字符串",
	}

	t.Log("=== 边缘情况向量化 ===")
	for _, c := range cases {
		v := e.Vectorize(c)
		var dims int
		for range v {
			dims++
		}
		t.Logf("  dims=%d  %q", dims, trimLen(c, 60))
	}
}

func TestBilingualVectorizeClean(t *testing.T) {
	zhPath := "/tmp/cc.zh.top200k.vec"
	e := NewStaticEmbedder(zhPath)

	inputs := []string{
		`来自小王的（扶高升学咨询群）群聊消息，通过id99使用qq_get_message工具获取消息正文。获取内容后使用 output_send(channel="qq") 回复该群聊，content 设为 JSON 字符串：{"content":"你的回复","group_id":979911915}`,
		`【重要！老大消息】来自—/的私聊消息，通过id54使用qq_get_message工具获取消息正文。获取内容后使用 output_send(channel="qq") 回复对方，content 设为 JSON 字符串：{"content":"你的回复","user_id":2198972886}`,
		`The nginx server is configured with reverse proxy. 帮我查一下 Docker 容器状态。`,
		`老大你好，React 组件已经封装好了，Nginx 配置也改完了，Docker 部署没问题。`,
	}

	for i, inp := range inputs {
		rawVec := e.Vectorize(inp)
		cleanVec := e.VectorizeClean(inp)
		sim := cosineSim(rawVec, cleanVec)
		rawTokens := len(e.tokenize(inp))
		cleanTokens := len(e.tokenize(CleanText(inp)))
		t.Logf("[%d] sim(raw,clean)=%.4f  tokens: raw=%d clean=%d", i, sim, rawTokens, cleanTokens)
	}
}

// --- bilingual test data ---

func genBilingualEvents() []bilingualEvent {
	entries := []struct {
		topic   string
		zh      string // Chinese description
		en      string // English terms mixed in
		source  string
	}{
		{"大学招生", "河南医药大学录取分数线", "", "qq"},
		{"大学招生", "医学院专业排名", "medical university ranking", "agent"},
		{"大学招生", "高考志愿填报咨询", "college application consultation", "qq"},
		{"大学招生", "河南医药大学 Docker 部署项目", "docker deployment project", "agent"},
		{"老大私聊", "老大私聊消息回复", "boss private chat reply", "qq"},
		{"老大私聊", "老大说了关于 React 组件的事情", "boss talked about React components", "agent"},
		{"老大私聊", "回复老大关于服务器配置问题", "reply boss about nginx config", "agent"},
		{"老大私聊", "老大要求检查 Docker 容器状态", "boss asked to check docker status", "qq"},
		{"前端开发", "前端组件封装", "React component encapsulation", "cli"},
		{"前端开发", "页面路由配置 layout 设计", "page route config layout design", "cli"},
		{"前端开发", "JavaScript 交互逻辑开发", "javascript interaction logic", "agent"},
		{"前端开发", "TypeScript 代码调试优化", "typescript code debug optimization", "agent"},
		{"服务器运维", "Nginx 反向代理配置", "nginx reverse proxy config", "cli"},
		{"服务器运维", "Docker 容器部署方案", "docker container deployment", "cli"},
		{"服务器运维", "数据库备份恢复", "database backup recovery", "agent"},
		{"服务器运维", "SSL 证书续期配置", "ssl certificate renewal", "agent"},
		{"股票基金", "基金定投策略配置", "fund investment strategy", "qq"},
		{"股票基金", "股票涨跌分析", "stock market analysis", "agent"},
		{"股票基金", "理财收益 JavaScript 计算", "investment return javascript calculation", "agent"},
		{"股票基金", "市场行情 API 数据获取", "market data api fetch", "qq"},
	}

	var events []bilingualEvent
	for i, entry := range entries {
		text := entry.zh
		if entry.en != "" {
			text += " " + entry.en
		}
		events = append(events, bilingualEvent{
			idx:    i,
			source: entry.source,
			topic:  entry.topic,
			text:   text,
			label:  fmt.Sprintf("%s (%s)", trimLen(entry.zh, 30), trimLen(entry.en, 30)),
		})
	}
	return events
}

func textForBilingual(ev bilingualEvent, modelPaths []string) string {
	switch {
	case ev.source == "agent" && ev.text != "":
		return CleanText(ev.text)
	default:
		return CleanText(ev.text)
	}
}
