package memory

import (
	"fmt"
	"regexp"
	"sort"
	"testing"
)

// cleanQQTemplate 模拟之前由 globalTextCleaner 执行的模板噪音清理，
// 用于 stress test 中生成 cleanedText。
func cleanQQTemplate(text string) string {
	reQQGroupSuffix := regexp.MustCompile(`，通过id\d+使用qq_get_message工具获取消息正文。获取内容后使用 output_send\(channel="qq"\) 回复该群聊，content 设为 JSON 字符串：\{[^}]*\}`)
	reQQPrivateSuffix := regexp.MustCompile(`，通过id\d+使用qq_get_message工具获取消息正文。获取内容后使用 output_send\(channel="qq"\) 回复对方，content 设为 JSON 字符串：\{[^}]*\}`)
	reQQOldReply := regexp.MustCompile(`通过id\d+使用qq_get_message工具获取消息正文。获取后必须使用[^。]+。`)
	reQQOldForbid := regexp.MustCompile(`你只能通过qq_get_message先看消息，然后直接用%!s\(MISSING\)send_private_msg回复对方，中间的思考过程禁止调用任何其他工具\s*→\s*`)
	reQQGeneral := regexp.MustCompile(`通过id\d+使用qq_get_message工具获取消息正文[。，][^。]*?(?:回复|发送消息)`)
	reTimestamp := regexp.MustCompile(`\[\d{2}:\d{2}\]\s*`)
	reMultiSpace := regexp.MustCompile(`\s+`)
	text = reQQGroupSuffix.ReplaceAllString(text, "")
	text = reQQPrivateSuffix.ReplaceAllString(text, "")
	text = reQQOldReply.ReplaceAllString(text, "")
	text = reQQOldForbid.ReplaceAllString(text, "")
	text = reQQGeneral.ReplaceAllString(text, "")
	text = reTimestamp.ReplaceAllString(text, "")
	text = reMultiSpace.ReplaceAllString(text, " ")
	return text
}

type cleanTestEvent struct {
	idx     int
	source  string
	input   string
	response string
	rawText string
	cleanedText string
	topic  string
}

func TestCleanStressPrecision(t *testing.T) {
	events := genStressEvents(200)
	topics := []string{"大学招生", "老大私聊", "前端开发", "服务器运维", "股票基金"}

	e := NewStaticEmbedder("/tmp/cc.zh.sample.vec")
	if !e.Loaded() {
		t.Skip("embedder not loaded")
	}

	for _, cleanMode := range []bool{true, false} {
		t.Run(fmt.Sprintf("去模版=%v", cleanMode), func(t *testing.T) {
			usedTopics := make([]string, 0)
			for _, tp := range topics {
				if hasTopicEvents(events, tp) {
					usedTopics = append(usedTopics, tp)
				}
			}
			if len(usedTopics) == 0 {
				t.Fatal("no events for any topic")
			}
			t.Logf("topics: %v, events: %d", usedTopics, len(events))

	for _, qTopic := range usedTopics {
		query := queryForTopic(qTopic)
		qVec := e.Vectorize(query)

		type scored struct {
			idx   int
			topic string
			text  string
			score float64
		}
		all := make([]scored, len(events))
		for i, ev := range events {
			text := ev.rawText
			if cleanMode {
				text = ev.cleanedText
			}
			vec := e.Vectorize(text)
			all[i] = scored{idx: i, topic: ev.topic, text: text, score: cosineSim(qVec, vec)}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })

		topK := len(usedTopics) * 2
		if topK > len(all) {
			topK = len(all)
		}

		intraHits := 0
		for _, s := range all[:topK] {
			if s.topic == qTopic {
				intraHits++
			}
		}
		expected := countTopicEvents(events, qTopic)
		if expected > topK {
			expected = topK
		}
		recall := float64(intraHits) / float64(expected)

		if recall < 0.3 {
			t.Logf("  [LOW] query=%q topK=%d intra=%d/%d recall=%.2f", qTopic, topK, intraHits, expected, recall)
			for _, s := range all[:8] {
				t.Logf("    [%.4f] %s", s.score, trimLen(s.text, 60))
			}
		} else {
			t.Logf("  [OK] query=%q topK=%d intra=%d/%d recall=%.2f", qTopic, topK, intraHits, expected, recall)
		}
	}
		})
	}
}

func TestCleanStressCrossTopic(t *testing.T) {
	events := genStressEvents(200)

	e := NewStaticEmbedder("/tmp/cc.zh.sample.vec")
	if !e.Loaded() {
		t.Skip("embedder not loaded")
	}

	queries := []string{
		"河南医药大学录取分数线",
		"老大发了什么私聊消息",
		"前端组件怎么封装布局",
		"服务器部署配置代理备份证书",
		"基金定投收益计算",
	}

	for _, q := range queries {
		qVec := e.Vectorize(q)
		t.Logf("query: %q", q)

		type scored struct {
			idx   int
			topic string
			score float64
		}
		all := make([]scored, len(events))
		for i, ev := range events {
			all[i] = scored{idx: i, topic: ev.topic, score: cosineSim(qVec, e.Vectorize(ev.cleanedText))}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })

		topScores := make(map[string]float64)
		for _, s := range all[:10] {
			if _, ok := topScores[s.topic]; !ok {
				topScores[s.topic] = s.score
			}
		}
		for tp, sc := range topScores {
			t.Logf("  [%.4f] %s", sc, tp)
		}
	}
}

func TestCleanTemplateNoiseSuppression(t *testing.T) {
	e := NewStaticEmbedder("/tmp/cc.zh.sample.vec")
	if !e.Loaded() {
		t.Skip("embedder not loaded")
	}

	noisyInput := `来自小王的（扶高升学咨询群）群聊消息，通过id99使用qq_get_message工具获取消息正文。获取内容后使用 output_send(channel="qq") 回复该群聊，content 设为 JSON 字符串：{"content":"你的回复","group_id":979911915}`
	cleanInput := `来自小王的（扶高升学咨询群）群聊消息`

	query := "扶高升学咨询群"
	qClear := e.Vectorize(query)
	qNoisy := e.Vectorize(noisyInput)
	qClean := e.Vectorize(cleanInput)

	n2c := cosineSim(qNoisy, qClean)
	n2q := cosineSim(qNoisy, qClear)
	c2q := cosineSim(qClean, qClear)

	t.Logf("noisy(%q) vs clean(%q) = %.4f", noisyInput[:30], cleanInput, n2c)
	t.Logf("noisy vs query(%q) = %.4f", query, n2q)
	t.Logf("clean vs query = %.4f", c2q)

	if c2q <= n2q {
		t.Log("NOTE: clean not better than noisy for this pattern (may have useful info in metadata)")
	}
}

func TestCleanVectorConsistency(t *testing.T) {
	e := NewStaticEmbedder("/tmp/cc.zh.sample.vec")
	if !e.Loaded() {
		t.Skip("embedder not loaded")
	}

	templates := []string{
		`通过id1使用qq_get_message工具获取消息正文。获取内容后使用 output_send(channel="qq") 回复该群聊，content 设为 JSON 字符串：{"content":"你的回复","group_id":1}`,
		`通过id2使用qq_get_message工具获取消息正文。获取内容后使用 output_send(channel="qq") 回复对方，content 设为 JSON 字符串：{"content":"你的回复","user_id":2}`,
		`通过id3使用qq_get_message工具获取消息正文。获取后必须使用qq_send_private_msg工具回复对方，不得使用其他非回复工具。`,
		`[12:00] agent: 处理完成`,
	}

	for _, tmpl := range templates {
		cleaned := CleanText(tmpl)
		t.Logf("template {%q} → {%q} (%d chars)", trimLen(tmpl, 60), cleaned, len(cleaned))
	}

	pairs := []struct {
		a, b   string
		reason string
	}{
		{cleanQQGroup("A", "群1"), cleanQQGroup("B", "群1"), "同群不同人"},
		{cleanQQGroup("A", "群1"), cleanQQGroup("A", "群2"), "同人不同群"},
		{cleanQQPrivate("老大"), cleanQQPrivate("老板"), "私聊不同人"},
		{cleanQQGroup("A", "高考群"), cleanQQPrivate("老大"), "群聊vs私聊"},
	}

	for _, p := range pairs {
		va := e.Vectorize(p.a)
		vb := e.Vectorize(p.b)
		sim := cosineSim(va, vb)
		t.Logf("sim(%q, %q) [%s] = %.4f", trimLen(p.a, 40), trimLen(p.b, 40), p.reason, sim)
	}
}

func BenchmarkCleanVectorize(b *testing.B) {
	e := NewStaticEmbedder("/tmp/cc.zh.sample.vec")
	if !e.Loaded() {
		b.Skip("embedder not loaded")
	}

	texts := make([]string, 100)
	for i := range texts {
		texts[i] = fmt.Sprintf(
			`【重要！老大消息】来自—/的私聊消息，通过id%d使用qq_get_message工具获取消息正文。获取内容后使用 output_send(channel="qq") 回复对方，content 设为 JSON 字符串：{"content":"你的回复","user_id":%d}`,
			i, 1000+i,
		)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.VectorizeClean(texts[i%len(texts)])
	}
}

// --- test data generators ---

func genStressEvents(n int) []cleanTestEvent {
	if n <= 0 {
		return nil
	}
	topics := []string{"大学招生", "老大私聊", "前端开发", "服务器运维", "股票基金"}
	events := make([]cleanTestEvent, 0, n)

	names := []string{"小明", "小红", "小张", "老王", "老大", "小李", "小王", "小赵"}
	groups := []string{"扶高升学咨询群", "前端技术交流", "服务器运维群", "基金定投群", "闲聊群"}

	topicContent := map[string]struct {
		keywords []string
		sources  []string
	}{
		"大学招生": {[]string{"河南医药大学", "录取分数线", "专业排名", "高考志愿", "招生简章"}, []string{"qq", "qq", "agent"}},
		"老大私聊": {[]string{"老大私聊消息", "回复老大", "任务安排", "汇报工作", "收到"}, []string{"qq", "agent", "agent"}},
		"前端开发": {[]string{"前端组件封装", "页面路由配置", "界面布局设计", "交互逻辑开发", "代码调试优化"}, []string{"cli", "cli", "agent"}},
		"服务器运维": {[]string{"反向代理配置", "容器部署方案", "证书续期", "数据库备份恢复", "监控告警处理"}, []string{"cli", "agent", "agent"}},
		"股票基金": {[]string{"基金定投策略", "股票涨跌分析", "理财收益计算", "市场行情分析", "投资风险管理"}, []string{"qq", "qq", "agent"}},
	}

	for i := 0; i < n; i++ {
		tp := topics[i%len(topics)]
		info := topicContent[tp]
		kw := info.keywords[i%len(info.keywords)]
		nm := names[i%len(names)]
		grp := groups[i%len(groups)]
		src := info.sources[i%len(info.sources)]

		var input, response string
		switch src {
		case "qq":
			if tp == "老大私聊" {
				input = fmt.Sprintf(`【重要！老大消息】来自%s的私聊消息，通过id%d使用qq_get_message工具获取消息正文。获取内容后使用 output_send(channel="qq") 回复对方，content 设为 JSON 字符串：{"content":"你的回复","user_id":%d}`, nm, i, 1000+i)
				if i%3 == 0 {
					response = fmt.Sprintf("已回复老大，关于%s", kw)
				}
			} else {
				input = fmt.Sprintf(`来自%s的（%s）群聊消息，通过id%d使用qq_get_message工具获取消息正文。获取内容后使用 output_send(channel="qq") 回复该群聊，content 设为 JSON 字符串：{"content":"你的回复","group_id":%d}`, nm, grp, i, 9000+i)
				if i%3 == 0 {
					response = fmt.Sprintf("已回复%s相关的问题", kw)
				}
			}
		case "agent":
			input = fmt.Sprintf(`来自%s的（%s）消息`, nm, grp)
			response = fmt.Sprintf("关于%s，我的建议是...已处理完成。", kw)
		case "cli":
			input = fmt.Sprintf("查询%s的相关信息", kw)
			response = fmt.Sprintf("查到了%s的结果", kw)
		}

		cleaned := cleanEventText(src, input, response)
		raw := rawEventText(src, input, response)
		events = append(events, cleanTestEvent{
			idx:     i,
			source:  src,
			input:   input,
			response: response,
			rawText: raw,
			cleanedText: cleaned,
			topic:  tp,
		})
	}
	return events
}

func cleanEventText(source, input, response string) string {
	// 先做基础 CleanText（去空格/逗号），再做模板噪音清理
	switch {
	case source == "agent" && response != "":
		return cleanQQTemplate(CleanText(response))
	case source == "cold_storage":
		return cleanQQTemplate(CleanText(input + " " + response))
	default:
		return cleanQQTemplate(CleanText(input))
	}
}

func rawEventText(source, input, response string) string {
	if response == "" {
		return input
	}
	return input + " " + response
}

func queryForTopic(topic string) string {
	switch topic {
	case "大学招生":
		return "河南医药大学录取分数线多少"
	case "老大私聊":
		return "老大刚才说了什么私聊消息"
	case "前端开发":
		return "前端组件怎么封装布局"
	case "服务器运维":
		return "服务器部署容器代理配置证书备份监控告警"
	case "股票基金":
		return "基金定投收益怎么样"
	default:
		return topic
	}
}

func hasTopicEvents(events []cleanTestEvent, topic string) bool {
	for _, ev := range events {
		if ev.topic == topic {
			return true
		}
	}
	return false
}

func countTopicEvents(events []cleanTestEvent, topic string) int {
	n := 0
	for _, ev := range events {
		if ev.topic == topic {
			n++
		}
	}
	return n
}

func cleanQQGroup(user, group string) string {
	return fmt.Sprintf("来自%s的（%s）群聊消息", user, group)
}

func cleanQQPrivate(user string) string {
	return fmt.Sprintf("【重要！老大消息】来自%s的私聊消息", user)
}
