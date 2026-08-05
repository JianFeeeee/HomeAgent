package memory

import (
	"sort"
	"strings"
	"testing"
	"time"
)

type realEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source"`
	Input     string    `json:"input"`
	Response  string    `json:"response"`
}

func TestCleanText(t *testing.T) {
	cases := []struct {
		input    string
		expected string
		contains string
	}{
		{
			input:    `来自A的（扶高升学咨询群）群聊消息，通过id36使用qq_get_message工具获取消息正文。获取内容后使用 output_send(channel="qq") 回复该群聊，content 设为 JSON 字符串：{"content":"你的回复","group_id":979911915}`,
			expected: "来自A的（扶高升学咨询群）群聊消息",
		},
		{
			input:    `【重要！老大消息】来自—/的私聊消息，通过id54使用qq_get_message工具获取消息正文。获取内容后使用 output_send(channel="qq") 回复对方，content 设为 JSON 字符串：{"content":"你的回复","user_id":2198972886}`,
			expected: "【重要！老大消息】来自—/的私聊消息",
		},
		{
			input:    `[12:05] agent: 已经回复老大啦～继续去搞 vanblog 换端口的事 😊`,
			contains: "已经回复老大啦",
		},
		{
			input:    `加载文档记忆: 扶高升学咨询群 河南医药大学 回复 招生 专业 录取`,
			expected: "加载文档记忆: 扶高升学咨询群 河南医药大学 回复 招生 专业 录取",
		},
		{
			input:    `通过id10使用qq_get_message工具获取消息正文。获取后必须使用qq_send_private_msg工具回复对方，不得使用其他非回复工具。你只能通过qq_get_message先看消息，然后直接用%!s(MISSING)send_private_msg回复，中间的思考过程禁止调用任何其他工具 → 已经回复老大啦～`,
			contains: "已经回复老大啦",
		},
		{
			input:    "",
			expected: "",
		},
		{
			input:    `处理错误: all 9 providers failed, last error: lua transform_request: adapter tesy not loaded`,
			contains: "处理错误",
		},
	}

	for i, c := range cases {
		got := cleanQQTemplate(CleanText(c.input))
		if c.expected != "" && got != c.expected {
			t.Errorf("case %d:\n  input:    %q\n  expected: %q\n  got:      %q", i, trimLen(c.input, 60), c.expected, got)
		}
		if c.contains != "" && !strings.Contains(got, c.contains) {
			t.Errorf("case %d: expected to contain %q, got %q", i, c.contains, got)
		}
		t.Logf("case %d: %q → %q", i, trimLen(c.input, 60), got)
	}
}

func TestRealContextPerSourceVector(t *testing.T) {
	e := newSynthEmbedder(t, 300)

	raw := genRealEvents()
	t.Logf("loaded %d synthetic events", len(raw))

	type scored struct {
		idx    int
		source string
		text   string
		score  float64
	}

	clean := func(ev realEvent) string {
		switch {
		case ev.Source == "agent" && ev.Response != "":
			return CleanText(ev.Response)
		case ev.Source == "cold_storage":
			return CleanText(ev.Input + " " + ev.Response)
		default:
			return CleanText(ev.Input)
		}
	}

	t.Run("医药大学_不同源向量", func(t *testing.T) {
		q := "河南医药大学招生分数录取排名"
		qVec := e.VectorizeClean(q)

		all := make([]scored, len(raw))
		for i, ev := range raw {
			text := clean(ev)
			all[i] = scored{idx: i, source: ev.Source, text: text[:min(len(text), 200)], score: cosineSim(qVec, e.Vectorize(text))}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })

		t.Log("top 5:")
		for _, s := range all[:5] {
			t.Logf("  [%.4f] [%-13s] %s", s.score, s.source, trimLen(s.text, 80))
		}

		var univHigh bool
		for _, s := range all[:8] {
			if strings.Contains(s.text, "医药大学") || strings.Contains(s.text, "升学") {
				univHigh = true
				break
			}
		}
		if !univHigh {
			t.Error("expected university-related events in top 8")
		}
	})

	t.Run("老大私聊_agent主用Response", func(t *testing.T) {
		q := "老大私聊说了什么"
		qVec := e.VectorizeClean(q)

		all := make([]scored, len(raw))
		for i, ev := range raw {
			text := clean(ev)
			all[i] = scored{idx: i, source: ev.Source, text: text[:min(len(text), 200)], score: cosineSim(qVec, e.Vectorize(text))}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })

		t.Log("top 5:")
		for _, s := range all[:5] {
			t.Logf("  [%.4f] [%-13s] %s", s.score, s.source, trimLen(s.text, 80))
		}

		var bossFound bool
		for _, s := range all[:8] {
			if strings.Contains(s.text, "老大") {
				bossFound = true
				break
			}
		}
		if !bossFound {
			t.Error("expected events mentioning 老大 in top 8")
		}

		var agentFound bool
		for _, s := range all[:5] {
			if s.source == "agent" {
				agentFound = true
				break
			}
		}
		t.Logf("agent in top5: %v (source strategy: agent events use Response for vector)", agentFound)
	})

	t.Run("图片转SVG_去模版后效果", func(t *testing.T) {
		q := "图片转换SVG工具"
		qVec := e.VectorizeClean(q)

		all := make([]scored, len(raw))
		for i, ev := range raw {
			text := clean(ev)
			all[i] = scored{idx: i, source: ev.Source, text: text[:min(len(text), 200)], score: cosineSim(qVec, e.Vectorize(text))}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })

		t.Log("top 5:")
		for _, s := range all[:5] {
			t.Logf("  [%.4f] [%-13s] %s", s.score, s.source, trimLen(s.text, 80))
		}

		var img bool
		for _, s := range all[:5] {
			if strings.Contains(s.text, "图片") || strings.Contains(s.text, "SVG") {
				img = true
				break
			}
		}
		if !img {
			t.Error("expected image-related events in top 5")
		}
	})

	t.Run("南航航空航天", func(t *testing.T) {
		q := "南航航空航天专业转电气"
		qVec := e.VectorizeClean(q)

		all := make([]scored, len(raw))
		for i, ev := range raw {
			text := clean(ev)
			all[i] = scored{idx: i, source: ev.Source, text: text[:min(len(text), 200)], score: cosineSim(qVec, e.Vectorize(text))}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })

		t.Log("top 5:")
		for _, s := range all[:5] {
			t.Logf("  [%.4f] [%-13s] %s", s.score, s.source, trimLen(s.text, 80))
		}

		var nau bool
		for _, s := range all[:5] {
			if strings.Contains(s.text, "南航") {
				nau = true
				break
			}
		}
		if !nau {
			t.Error("expected 南航 in top 5")
		}
	})

	t.Run("跨域区分度", func(t *testing.T) {
		pairs := []struct {
			a, b string
		}{
			{"老大私聊说了什么", "河南医药大学招生分数"},
			{"老大私聊说了什么", "图片转换SVG工具"},
			{"南航航空航天电气", "河南医药大学录取"},
			{"图片转换SVG工具", "老大私聊"},
		}
		for _, p := range pairs {
			va := e.VectorizeClean(p.a)
			vb := e.VectorizeClean(p.b)
			s := cosineSim(va, vb)
			t.Logf("  sim(%q, %q) = %.4f", p.a, p.b, s)
		}

		univVec := e.VectorizeClean("河南医药大学招生")
		bossVec := e.VectorizeClean("老大私聊说了什么")
		t.Logf("cross-domain sim(医药大学, 老大私聊) = %.4f", cosineSim(univVec, bossVec))
	})

	t.Run("去模版节省量", func(t *testing.T) {
		var savedTotal int
		for i, ev := range raw {
			orig := len(ev.Input + " " + ev.Response)
			after := len(clean(ev))
			saved := orig - after
			savedTotal += saved
			if saved > 100 {
				t.Logf("  [%2d] [%-13s] 节省 %d 字符 (raw=%d clean=%d)", i, ev.Source, saved, orig, after)
			}
		}
		t.Logf("总计节省 %d 字符", savedTotal)
	})
}

func TestRealContextEmbedderStats(t *testing.T) {
	e := newSynthEmbedder(t, 300)

	raw := genRealEvents()

	for _, ev := range raw[:5] {
		var text string
		switch {
		case ev.Source == "agent" && ev.Response != "":
			text = CleanText(ev.Response)
		case ev.Source == "cold_storage":
			text = CleanText(ev.Input + " " + ev.Response)
		default:
			text = CleanText(ev.Input)
		}
		vec := e.Vectorize(text)
		origLen := len(ev.Input + ev.Response)
		t.Logf("[%-13s] raw=%d cleaned=%d dims=%d", ev.Source, origLen, len(text), len(vec))
		for k := range vec {
			if !isNumericKey(k) {
				t.Errorf("non-numeric key %q — should be dense space", k)
			}
		}
	}
}

// genRealEvents 生成跨领域的合成事件夹具（替代曾依赖的 /tmp/context.json）。
func genRealEvents() []realEvent {
	now := time.Now()
	var out []realEvent
	add := func(src, input, response string) {
		out = append(out, realEvent{Timestamp: now, Source: src, Input: input, Response: response})
	}

	for _, s := range []string{
		"河南医药大学招生分数线出来了",
		"扶高升学咨询群讨论专业排名",
		"医药大学录取结果查询方法",
		"河南高考志愿填报咨询",
		"大学招生简章发布了",
	} {
		add("qq", s, "")
	}
	add("agent", "来自升学群的（医药大学咨询）消息", "已回复关于河南医药大学录取分数线的咨询")

	for _, s := range []string{
		"老大私聊消息安排了一个任务",
		"回复老大关于服务器配置的问题",
		"老大要求检查容器运行状态",
		"老大说了关于组件封装的事情",
		"给老大汇报工作进展",
	} {
		add("qq", s, "")
	}
	add("agent", "收到老大的（私聊）消息", "已回复老大关于任务安排的消息")

	for _, s := range []string{
		"用户要求图片转换SVG工具",
		"图片转SVG后尺寸优化完成",
		"转换图片格式为SVG",
		"生成SVG工具使用说明",
	} {
		add("cli", "查询"+s, "查到了"+s+"的结果")
	}

	for _, s := range []string{
		"南航航空航天专业介绍",
		"电气专业转南航的录取咨询",
		"南航院校分数线讨论",
		"航空航天方向的就业前景",
	} {
		add("qq", s, "")
	}

	for _, s := range []string{
		"今天天气怎么样",
		"晚上吃什么",
		"推荐一部电影",
		"股票基金收益行情如何",
		"查询快递送达状态",
		"设置一个明早的闹钟",
	} {
		add("cold_storage", s, "已处理，结果记录完成")
	}

	return out
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func trimLen(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
