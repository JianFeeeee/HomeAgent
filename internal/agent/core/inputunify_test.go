package core

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// 统一输入主干（processInput / resolveInput / injectedBlocks）与
// 模型可调用工具的媒体接线测试。
//
// 这一层此前的结构性缺陷：text 与 image/audio 各有一个 process 函数，
// 媒体那条缺了去重、no_memory、通道 Cleaner、中断语义、EventRawInput 五项。
// 归一成一条主干后，这些行为对所有模态一致——下面的断言就是这个不变量。

func newInputTestAgent(t *testing.T) (*Agent, *media.Store) {
	t.Helper()
	dir := t.TempDir()

	ms, err := media.New(filepath.Join(dir, "media"), 0)
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	return &Agent{mediaStore: ms}, ms
}

// ---------- injectedBlocks ----------

// 内核内部注入直接给 []agentAPI.ContentBlock；经公共 SDK 的 IOInjector 过来的是
// []pubsdk.ContentBlock。两者字段一致但 Go 不会自动转换，只认一种的后果是
// 另一种被静默丢弃——插件注入的图到 payload 就断了，且不报错。
func TestInjectedBlocks_AcceptsBothStaticTypes(t *testing.T) {
	t.Run("内核类型", func(t *testing.T) {
		blocks, kind := injectedBlocks(map[string]interface{}{
			"media_blocks": []agentAPI.ContentBlock{
				{Type: "text", Text: "看图"},
				{Type: "image_url", ImageURL: &agentAPI.ImageURL{URL: "data:image/png;base64,AAA"}},
			},
		})
		if len(blocks) != 2 {
			t.Fatalf("blocks = %d，期望 2", len(blocks))
		}
		if kind != "image" {
			t.Errorf("mediaType = %q，期望 image", kind)
		}
	})

	t.Run("公共SDK类型", func(t *testing.T) {
		blocks, kind := injectedBlocks(map[string]interface{}{
			"media_blocks": []pubsdk.ContentBlock{
				{Type: "text", Text: "听音频"},
				{Type: "audio_url", AudioURL: &pubsdk.AudioURL{URL: "data:audio/wav;base64,BBB"}},
			},
		})
		if len(blocks) != 2 {
			t.Fatalf("blocks = %d，期望 2（公共 SDK 类型被静默丢弃）", len(blocks))
		}
		if kind != "audio" {
			t.Errorf("mediaType = %q，期望 audio", kind)
		}
		// 转换必须保留 URL，否则块到了模型手上是空的
		if blocks[1].AudioURL == nil || blocks[1].AudioURL.URL != "data:audio/wav;base64,BBB" {
			t.Errorf("AudioURL 转换丢失: %+v", blocks[1].AudioURL)
		}
	})

	t.Run("图优先于音频", func(t *testing.T) {
		_, kind := injectedBlocks(map[string]interface{}{
			"media_blocks": []agentAPI.ContentBlock{
				{Type: "audio_url", AudioURL: &agentAPI.AudioURL{URL: "a"}},
				{Type: "image_url", ImageURL: &agentAPI.ImageURL{URL: "b"}},
			},
		})
		if kind != "image" {
			t.Errorf("mediaType = %q，期望 image", kind)
		}
	})

	t.Run("无媒体块", func(t *testing.T) {
		blocks, kind := injectedBlocks(map[string]interface{}{"content": "纯文本"})
		if blocks != nil || kind != "" {
			t.Errorf("无 media_blocks 时应返回 (nil,\"\")，实际 (%v,%q)", blocks, kind)
		}
	})

	t.Run("ImageURL 的 Detail 透传", func(t *testing.T) {
		blocks, _ := injectedBlocks(map[string]interface{}{
			"media_blocks": []pubsdk.ContentBlock{
				{Type: "image_url", ImageURL: &pubsdk.ImageURL{URL: "u", Detail: "high"}},
			},
		})
		if len(blocks) != 1 || blocks[0].ImageURL.Detail != "high" {
			t.Errorf("Detail 未透传: %+v", blocks)
		}
	})
}

// ---------- resolveInput ----------

func TestResolveInput_UnifiesAllModalities(t *testing.T) {
	a, _ := newInputTestAgent(t)

	t.Run("用户上传图片", func(t *testing.T) {
		in, ok := a.resolveInput(&agentIO.InputEvent{
			Source:  "qq",
			Type:    "image",
			Payload: map[string]interface{}{"data": "AAAA", "mime": "image/png"},
		})
		if !ok {
			t.Fatal("图片输入被判为无效")
		}
		if in.mediaType != "image" || in.captureTool != "input_image" {
			t.Errorf("mediaType=%q captureTool=%q", in.mediaType, in.captureTool)
		}
		if in.text == "" {
			t.Error("纯媒体输入应有 alt 文案作为文本落点")
		}
		if len(in.blocks) == 0 {
			t.Error("图片应转成内容块")
		}
	})

	t.Run("插件注入的媒体", func(t *testing.T) {
		in, ok := a.resolveInput(&agentIO.InputEvent{
			Source: "myplugin",
			Type:   "text",
			Payload: map[string]interface{}{
				"content": "帮我看看这张图",
				"media_blocks": []pubsdk.ContentBlock{
					{Type: "image_url", ImageURL: &pubsdk.ImageURL{URL: "data:image/png;base64,AAA"}},
				},
			},
		})
		if !ok {
			t.Fatal("带媒体的文本输入被判为无效")
		}
		if in.text != "帮我看看这张图" {
			t.Errorf("text = %q", in.text)
		}
		if len(in.blocks) != 1 || in.mediaType != "image" {
			t.Errorf("blocks=%d mediaType=%q —— 插件注入的媒体到 payload 就断了", len(in.blocks), in.mediaType)
		}
		if in.captureTool != "inject_myplugin" {
			t.Errorf("captureTool = %q，期望带来源便于溯源", in.captureTool)
		}
	})

	t.Run("只带图不带字也合法", func(t *testing.T) {
		_, ok := a.resolveInput(&agentIO.InputEvent{
			Source: "myplugin",
			Type:   "text",
			Payload: map[string]interface{}{
				"media_blocks": []agentAPI.ContentBlock{
					{Type: "image_url", ImageURL: &agentAPI.ImageURL{URL: "u"}},
				},
			},
		})
		if !ok {
			t.Error("只带媒体不带文本应视为有效输入（插件注入常这样）")
		}
	})

	t.Run("文本与媒体都空才无效", func(t *testing.T) {
		if _, ok := a.resolveInput(&agentIO.InputEvent{
			Source:  "cli",
			Type:    "text",
			Payload: map[string]interface{}{"content": ""},
		}); ok {
			t.Error("空输入应被拒")
		}
	})

	t.Run("纯文本", func(t *testing.T) {
		in, ok := a.resolveInput(&agentIO.InputEvent{
			Source:  "cli",
			Type:    "text",
			Payload: map[string]interface{}{"content": "你好"},
		})
		if !ok || in.text != "你好" || len(in.blocks) != 0 || in.mediaType != "" {
			t.Errorf("纯文本路径异常: ok=%v in=%+v", ok, in)
		}
	})
}

// ---------- 模型工具侧：sentenceWithMediaMarkers ----------

// 模型只知道 digest（从对话或 memory_recall 的「关联媒体」读到），
// 不该要求它自己按内核格式拼标记——格式写错的后果是引用静默挂不上。
func TestSentenceWithMediaMarkers(t *testing.T) {
	a, ms := newInputTestAgent(t)
	digest, err := ms.Put([]byte("marker-bytes"), media.Item{
		MIME: "image/png", Description: "一张紫蓝红三色带图",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	t.Run("短digest补全并生成标记", func(t *testing.T) {
		got := a.sentenceWithMediaMarkers("用户发来一张图。", []string{digest[:12]})
		if !strings.Contains(got, "三色带图") {
			t.Errorf("描述未并入句子: %q", got)
		}
		if !strings.Contains(got, digest[:12]) {
			t.Errorf("digest 未并入句子（反查会失效）: %q", got)
		}
		// 反解必须成功，否则 bindSentenceMedia 挂不上引用
		if got := extractMediaDigests(got); len(got) != 1 {
			t.Errorf("生成的标记无法被 extractMediaDigests 反解: %v", got)
		}
	})

	t.Run("模型已写标记时不重复追加", func(t *testing.T) {
		sentence := "看这个 [image/png " + digest[:12] + "] 三色带图"
		got := a.sentenceWithMediaMarkers(sentence, []string{digest[:12]})
		if n := strings.Count(got, digest[:12]); n != 1 {
			t.Errorf("digest 出现 %d 次，期望 1 次: %q", n, got)
		}
	})

	t.Run("空句子时标记本身充当句子", func(t *testing.T) {
		got := a.sentenceWithMediaMarkers("", []string{digest})
		if got == "" {
			t.Error("媒体必须有句子落点，否则 media_refs 无从挂起")
		}
	})

	t.Run("无法解析的digest被跳过", func(t *testing.T) {
		got := a.sentenceWithMediaMarkers("原句。", []string{"ffffffffffff"})
		if got != "原句。" {
			t.Errorf("不存在的 digest 不该造出标记: %q", got)
		}
	})

	t.Run("无媒体存储时原样返回", func(t *testing.T) {
		bare := &Agent{}
		if got := bare.sentenceWithMediaMarkers("原句。", []string{digest}); got != "原句。" {
			t.Errorf("无媒体存储时应原样返回: %q", got)
		}
	})
}

// ---------- resolveMediaDigests ----------

func TestResolveMediaDigests(t *testing.T) {
	a, ms := newInputTestAgent(t)
	d1, _ := ms.Put([]byte("one"), media.Item{MIME: "image/png"})
	d2, _ := ms.Put([]byte("two"), media.Item{MIME: "image/png"})

	got := a.resolveMediaDigests([]string{d1[:10], d2, d1, "ffffffffffff"})
	if len(got) != 2 {
		t.Fatalf("got = %v，期望 2 条（去重 + 丢弃无法解析的）", got)
	}
	for _, d := range got {
		if len(d) != 64 {
			t.Errorf("应返回完整 digest，实际 %q", d)
		}
	}

	if a.resolveMediaDigests(nil) != nil {
		t.Error("空输入应返回 nil")
	}
	bare := &Agent{}
	if bare.resolveMediaDigests([]string{d1}) != nil {
		t.Error("无媒体存储时应返回 nil")
	}
}

// ---------- bindDocMedia ----------

func TestBindDocMedia(t *testing.T) {
	a, ms := newInputTestAgent(t)
	d1, _ := ms.Put([]byte("doc-one"), media.Item{MIME: "image/png"})
	d2, _ := ms.Put([]byte("doc-two"), media.Item{MIME: "image/png"})

	if n := a.bindDocMedia("doc_x", []string{d1, d2}); n != 2 {
		t.Fatalf("绑定 %d 条，期望 2", n)
	}
	refs, err := ms.Refs(media.OwnerDocument, "doc_x")
	if err != nil {
		t.Fatalf("Refs: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("引用 = %v，期望 2 条", refs)
	}

	if n := a.bindDocMedia("", []string{d1}); n != 0 {
		t.Error("空 docID 不该绑定")
	}
	bare := &Agent{}
	if n := bare.bindDocMedia("doc_y", []string{d1}); n != 0 {
		t.Error("无媒体存储时不该绑定")
	}
}

// ---------- docMediaContext ----------

func TestDocMediaContext(t *testing.T) {
	a, ms := newInputTestAgent(t)
	digest, _ := ms.Put([]byte("ctx-bytes"), media.Item{
		MIME: "image/png", Description: "文档里的配图",
	})

	t.Run("优先用media_refs", func(t *testing.T) {
		if err := ms.AddRef(digest, media.OwnerDocument, "doc_refs"); err != nil {
			t.Fatalf("AddRef: %v", err)
		}
		got := a.docMediaContext("doc_refs", "正文里没有任何标记")
		if !strings.Contains(got, "文档里的配图") {
			t.Errorf("未从 media_refs 取到媒体说明: %q", got)
		}
	})

	t.Run("无引用时回退解析正文标记", func(t *testing.T) {
		content := "旧正文 [image/png " + digest[:12] + "] 文档里的配图"
		got := a.docMediaContext("doc_legacy", content)
		if !strings.Contains(got, "文档里的配图") {
			t.Errorf("历史文档只有标记时应回退解析: %q", got)
		}
	})

	t.Run("既无引用也无标记", func(t *testing.T) {
		if got := a.docMediaContext("doc_empty", "普通正文"); got != "" {
			t.Errorf("应返回空串，实际 %q", got)
		}
	})

	t.Run("无媒体存储", func(t *testing.T) {
		bare := &Agent{}
		if got := bare.docMediaContext("doc_x", "任意"); got != "" {
			t.Errorf("无媒体存储时应返回空串，实际 %q", got)
		}
	})
}

// ---------- mediaMarkerLine ----------

// 标记格式的唯一生成处。此前 mediaSummaryForEvent 与 mediaContextForSentences
// 各拼一份，改动截断长度或分隔符时只改一处，另一处写出的标记就再也解析不回来。
func TestMediaMarkerLine(t *testing.T) {
	a, ms := newInputTestAgent(t)

	described, _ := ms.Put([]byte("with-desc"), media.Item{
		MIME: "image/png", Description: "已描述的图",
	})
	if got := a.mediaMarkerLine(described); !strings.Contains(got, "已描述的图") {
		t.Errorf("有描述时应带描述: %q", got)
	}

	// 「已入库但还没描述」与「压根没有媒体」必须可区分
	bare, _ := ms.Put([]byte("no-desc"), media.Item{MIME: "image/png"})
	got := a.mediaMarkerLine(bare)
	if !strings.Contains(got, "(未描述)") {
		t.Errorf("无描述时应有占位符: %q", got)
	}
	if !strings.Contains(got, shortDigest(bare)) {
		t.Errorf("必须带短 digest 供反查: %q", got)
	}

	// 查不到返回空串：媒体可能已被容量 GC 淘汰，此时不该造出指向虚无的标记
	if got := a.mediaMarkerLine("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"); got != "" {
		t.Errorf("查不到的 digest 应返回空串，实际 %q", got)
	}
}

// ---------- 模型工具端到端：memory_commit / doc_commit / doc_query ----------

func newToolTestAgent(t *testing.T) (*Agent, *media.Store) {
	t.Helper()
	dir := t.TempDir()

	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	ds := document.NewStore(filepath.Join(dir, "documents"))
	if err := ds.Start(); err != nil {
		t.Fatalf("doc store: %v", err)
	}
	t.Cleanup(func() { ds.Stop() })

	ms, err := media.New(filepath.Join(dir, "media"), 0)
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	emb := memory.NewStaticEmbedder("")
	a := &Agent{
		id:         "tester",
		memory:     g,
		docStore:   ds,
		mediaStore: ms,
		context:    NewRelevanceContext("", emb),
	}
	return a, ms
}

// memory_commit 带 media_digests：三元组入库后必须能从句子反查回那份字节。
func TestToolMemoryCommit_BindsMedia(t *testing.T) {
	a, ms := newToolTestAgent(t)
	digest, _ := ms.Put([]byte("commit-bytes"), media.Item{
		MIME: "image/png", Description: "提交时关联的图",
	})

	out := a.executeMemoryTool(agentAPI.ToolCall{
		Name: "memory_commit",
		Arguments: map[string]interface{}{
			"triples": []interface{}{
				map[string]interface{}{
					"subject":       "配色方案",
					"relation":      "参考",
					"object":        "三色带图",
					"media_digests": []interface{}{digest[:12]},
				},
			},
		},
	})
	if !strings.Contains(out, "关联") {
		t.Errorf("返回值应告知模型媒体已关联: %q", out)
	}

	res, err := a.memory.Recall([]string{"配色方案"}, nil, 2, "")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Relations) == 0 || res.Relations[0].SentenceID == 0 {
		t.Fatal("没有句子落点 —— 媒体引用无从挂起")
	}
	refs, _ := ms.Refs(media.OwnerGraphSentence, strconv.FormatInt(res.Relations[0].SentenceID, 10))
	if len(refs) != 1 || refs[0] != digest {
		t.Errorf("句子引用 = %v，期望 [%s]", refs, digest)
	}
}

// 不带 media_digests 时行为与本特性上线前一致（不多写句子、不报错）。
func TestToolMemoryCommit_WithoutMedia(t *testing.T) {
	a, _ := newToolTestAgent(t)
	out := a.executeMemoryTool(agentAPI.ToolCall{
		Name: "memory_commit",
		Arguments: map[string]interface{}{
			"triples": []interface{}{
				map[string]interface{}{"subject": "甲方", "relation": "签署", "object": "合同"},
			},
		},
	})
	if strings.Contains(out, "失败") {
		t.Errorf("普通提交不该失败: %q", out)
	}
	if strings.Contains(out, "关联") {
		t.Errorf("无媒体时不该提媒体: %q", out)
	}
}

// sentence_text 必须透传：丢了它，图谱就回不到原文。
func TestToolMemoryCommit_CarriesSentenceText(t *testing.T) {
	a, _ := newToolTestAgent(t)
	a.executeMemoryTool(agentAPI.ToolCall{
		Name: "memory_commit",
		Arguments: map[string]interface{}{
			"triples": []interface{}{
				map[string]interface{}{
					"subject":       "李四",
					"relation":      "住在",
					"object":        "杭州",
					"sentence_text": "李四搬到杭州已经三年了。",
				},
			},
		},
	})
	res, _ := a.memory.Recall([]string{"李四"}, nil, 2, "")
	if len(res.Relations) == 0 {
		t.Fatal("召回为空")
	}
	if res.Relations[0].SentenceText != "李四搬到杭州已经三年了。" {
		t.Errorf("SentenceText = %q", res.Relations[0].SentenceText)
	}
}

// doc_commit 带 media_digests：标记进正文（否则检索不到）+ 引用挂文档 owner（否则 GC 会清）。
func TestToolDocCommit_BindsMedia(t *testing.T) {
	a, ms := newToolTestAgent(t)
	digest, _ := ms.Put([]byte("doc-commit-bytes"), media.Item{
		MIME: "image/png", Description: "笔记里的插图",
	})

	out := a.executeDocTool(agentAPI.ToolCall{
		Name: "doc_commit",
		Arguments: map[string]interface{}{
			"content":       "这是一篇带图的笔记正文。",
			"summary":       "带图笔记",
			"media_digests": []interface{}{digest[:12]},
		},
	})
	if !strings.Contains(out, "关联") {
		t.Errorf("返回值应告知模型媒体已关联: %q", out)
	}

	docs := a.docStore.RecentDocs(5)
	if len(docs) == 0 {
		t.Fatal("文档未写入")
	}
	d := docs[0]
	if !strings.Contains(d.Content, "笔记里的插图") {
		t.Errorf("标记未进正文（向量索引看不到这份媒体）: %q", d.Content)
	}
	refs, _ := ms.Refs(media.OwnerDocument, d.ID)
	if len(refs) != 1 || refs[0] != digest {
		t.Errorf("文档引用 = %v，期望 [%s]", refs, digest)
	}
}

// doc_query 必须把媒体说明附在返回值里，否则模型检索到带图文档也不知道有图。
func TestToolDocQuery_ShowsMedia(t *testing.T) {
	a, ms := newToolTestAgent(t)
	digest, _ := ms.Put([]byte("query-bytes"), media.Item{
		MIME: "image/png", Description: "检索命中的配图",
	})

	a.executeDocTool(agentAPI.ToolCall{
		Name: "doc_commit",
		Arguments: map[string]interface{}{
			"content":       "紫蓝红三色带配色说明正文",
			"summary":       "紫蓝红三色带",
			"media_digests": []interface{}{digest},
		},
	})

	a.executeDocTool(agentAPI.ToolCall{
		Name:      "doc_query",
		Arguments: map[string]interface{}{"query": "紫蓝红三色带 配色说明", "top_k": float64(3)},
	})

	// 正文进的是 cold_storage 事件（工具返回值只给引用编号），媒体说明也在那里。
	var found bool
	for _, e := range a.context.Recent(10) {
		if strings.Contains(e.Response, "检索命中的配图") {
			found = true
		}
	}
	if !found {
		t.Error("doc_query 未把媒体说明带进上下文 —— 模型不知道这篇文档带过图")
	}
}

// 无媒体存储时三个工具的行为与本特性上线前完全一致。
func TestTools_NilMediaStoreDegrades(t *testing.T) {
	a, _ := newToolTestAgent(t)
	a.mediaStore = nil

	out := a.executeMemoryTool(agentAPI.ToolCall{
		Name: "memory_commit",
		Arguments: map[string]interface{}{
			"triples": []interface{}{
				map[string]interface{}{
					"subject": "无存储", "relation": "仍可", "object": "提交",
					"media_digests": []interface{}{"aabbccddeeff"},
				},
			},
		},
	})
	if strings.Contains(out, "失败") {
		t.Errorf("无媒体存储时提交不该失败: %q", out)
	}

	out = a.executeDocTool(agentAPI.ToolCall{
		Name: "doc_commit",
		Arguments: map[string]interface{}{
			"content":       "无媒体存储的文档",
			"media_digests": []interface{}{"aabbccddeeff"},
		},
	})
	if strings.Contains(out, "失败") {
		t.Errorf("无媒体存储时文档写入不该失败: %q", out)
	}
}
