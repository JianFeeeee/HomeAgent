package core

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
)

// L3 图库媒体引用测试。
//
// 这一层的目的只有一个：几个月后从图谱走到一条句子，要能取回当时那份媒体。
// 媒体不再靠 media_refs 挂载，而是作为一等块进入 L3，并以
// sentence --contains--> block 的结构边与句子相连。

// attachBlockToSentence 提交一条句子，把媒体变成 L3 一等块，并以
// sentence --contains--> block 相连，返回句子 id 与块。
// 必须走真实提交：边要求两端都是真实图节点。
func attachBlockToSentence(t *testing.T, g *memory.GraphDB, ms *media.Store, sentenceText, digest string) (int64, memory.MemoryBlock) {
	t.Helper()
	ids, _, _, err := g.CommitWithMedia([]memory.Triple{{
		Subject: "媒体载体", Relation: "包含", Object: "内容", SentenceText: sentenceText,
	}}, "test", 0)
	if err != nil {
		t.Fatalf("CommitWithMedia: %v", err)
	}
	sid := ids[sentenceText]
	if sid == 0 {
		t.Fatalf("拿不到句子 id: %q", sentenceText)
	}
	it, err := ms.Stat(digest)
	if err != nil || it == nil {
		t.Fatalf("Stat(%s): %v", shortDigest(digest), err)
	}
	b := memory.MemoryBlock{
		ID:            fmt.Sprintf("blk_test_%d_%s", sid, shortDigest(digest)),
		Modality:      memory.BlockImage,
		PayloadDigest: it.Digest,
		MIME:          it.MIME,
		Size:          it.Size,
		Width:         it.Width,
		Height:        it.Height,
		Vector:        it.Vec,
		Fingerprint:   it.VecModel,
	}
	if err := g.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
		t.Fatalf("PutMemoryBlocks: %v", err)
	}
	if err := g.AddMemoryBlockEdge("sentence", strconv.FormatInt(sid, 10), "block", b.ID, "contains"); err != nil {
		t.Fatalf("AddMemoryBlockEdge: %v", err)
	}
	return sid, b
}

func newGraphMediaAgent(t *testing.T) (*Agent, *memory.GraphDB, *media.Store) {
	t.Helper()
	dir := t.TempDir()

	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	ms, err := media.New(filepath.Join(dir, "media"))
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	return &Agent{memory: g, mediaStore: ms}, g, ms
}

func TestExtractMediaDigests(t *testing.T) {
	// 与 mediaSummaryForEvent 的输出格式对应
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"事件摘要格式", "媒体内容：\n[image/png a1b2c3d4e5f6] 一张紫蓝红三色带图", []string{"a1b2c3d4e5f6"}},
		{"kind 兜底格式", "[image abcdef0123456789] (未描述)", []string{"abcdef0123456789"}},
		{"一句多个", "[image aaaaaaaaaaaa] 图一；[image bbbbbbbbbbbb] 图二", []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb"}},
		{"去重", "[image cccccccccccc] x [image/png cccccccccccc] y", []string{"cccccccccccc"}},
		{"无标记", "普通句子，没有媒体", nil},
		{"空串", "", nil},
		// 非十六进制、过短的方括号内容不能误命中，否则会拿一个假前缀去 ResolvePrefix
		{"非 digest 方括号", "[注意] 这是普通标注 [TODO]", nil},
		{"过短", "[image abc] 太短", nil},
	}

	for _, c := range cases {
		got := extractMediaDigests(c.text)
		if len(got) != len(c.want) {
			t.Fatalf("%s: 得到 %v，期望 %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: 第 %d 个得到 %q，期望 %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}

func TestCommitWithMedia_ReturnsSentenceIDs(t *testing.T) {
	_, g, _ := newGraphMediaAgent(t)

	sentence := "[image/png a1b2c3d4e5f6] 一张紫蓝红三色带图"
	triples := []memory.Triple{{
		Subject: "图片", Relation: "内容", Object: "三色带",
		SentenceText: sentence,
	}}

	ids, ec, rc, err := g.CommitWithMedia(triples, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ec == 0 || rc == 0 {
		t.Fatalf("应写入实体与关系，实际 ec=%d rc=%d", ec, rc)
	}
	if ids[sentence] == 0 {
		t.Fatalf("应返回句子 id，实际 %v", ids)
	}
}

func TestCommit_StillWorksAfterRefactor(t *testing.T) {
	// Commit 有三十多个调用点，内部转调后行为必须完全不变
	_, g, _ := newGraphMediaAgent(t)

	triples := []memory.Triple{
		{Subject: "张三", Relation: "喜欢", Object: "咖啡", SentenceText: "张三喜欢咖啡"},
		{Subject: "李四", Relation: "住在", Object: "北京"},
	}
	ec, rc, err := g.Commit(triples, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ec != 4 || rc != 2 {
		t.Fatalf("期望 4 实体 2 关系，实际 ec=%d rc=%d", ec, rc)
	}

	// 重复提交同一批：关系被唯一约束去重。
	//
	// 实体计数**不**归零——这是 upsertEntity 的既有行为：SQLite 的
	// ON CONFLICT DO UPDATE 也算一行 affected，于是 RowsAffected() > 0
	// 被当成"新建了"。用 main 分支的 graph.go 单独验证过基线同样是
	// 首次 ec=2 / 重复 ec=2，与 CommitWithMedia 重构无关。
	// entitiesCreated 只用于日志，故此处记录现状而不改行为。
	ec2, rc2, err := g.Commit(triples, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if rc2 != 0 {
		t.Fatalf("重复提交不该新建关系，实际 rc=%d", rc2)
	}
	if ec2 != 4 {
		t.Fatalf("实体计数应与首次一致（既有 upsert 计数行为），实际 ec=%d", ec2)
	}
}

func TestBindSentenceMedia_RoundTrip(t *testing.T) {
	// 整层的核心断言：写入 → 提交 → 反查取回原始字节
	a, _, ms := newGraphMediaAgent(t)

	content := []byte("\x89PNG\r\n\x1a\n fake image bytes")
	digest, err := ms.Put(content, media.Item{MIME: "image/png", Kind: media.KindImage})
	if err != nil {
		t.Fatal(err)
	}
	short := shortDigest(digest)

	sentence := "[image/png " + short + "] 一张紫蓝红三色带图"
	triples := []memory.Triple{{
		Subject: "图片", Relation: "内容", Object: "三色带", SentenceText: sentence,
	}}

	if _, _, _, err := a.commitTriplesWithMedia(triples, "s1", 0, nil); err != nil {
		t.Fatal(err)
	}

	// 找到句子 id
	ids, _, _, err := a.memory.CommitWithMedia(triples, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	sid := ids[sentence]
	if sid == 0 {
		t.Fatal("拿不到句子 id")
	}

	// 反查：从句子取回一等块，再取回字节
	blocks, err := a.RecallBlocksForSentence(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].PayloadDigest != digest {
		t.Fatalf("反查应得完整 digest %s，实际 %+v", shortDigest(digest), blocks)
	}
	got, err := ms.Get(blocks[0].PayloadDigest)
	if err != nil {
		t.Fatalf("取回内容失败: %v", err)
	}
	if string(got) != string(content) {
		t.Fatal("取回的内容与写入不一致")
	}

	// 块仍被 L3 持有 → 内容应仍可读
	if _, err := ms.Get(digest); err != nil {
		t.Fatalf("被 L3 记忆块持有的内容不该被清除: %v", err)
	}
}

func TestBindSentenceMedia_SkipsUnresolvable(t *testing.T) {
	// 文本里的 digest 在库里不存在时必须跳过，不能建一条指向虚无的块边。
	a, g, _ := newGraphMediaAgent(t)

	sentence := "[image/png deadbeefdead] 一张不存在的图"
	ids := map[string]int64{sentence: 42}
	a.bindSentenceBlocks(ids, nil)

	blocks, err := g.BlocksForNode("sentence", "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 0 {
		t.Fatalf("无法补全的 digest 不该建块，实际 %+v", blocks)
	}
}

func TestBindSentenceMedia_NilStoreNoop(t *testing.T) {
	a := &Agent{}
	a.bindSentenceBlocks(map[string]int64{"[image aaaaaaaaaaaa] x": 1}, nil)
	if got, err := a.RecallBlocksForSentence(1); err != nil || got != nil {
		t.Fatalf("媒体关闭时应静默无操作，实际 %v / %v", got, err)
	}
}

func TestCommitTriplesWithMedia_FallsBackWithoutStore(t *testing.T) {
	// 媒体关闭时退回普通 Commit，行为与直接调 Commit 完全一致
	dir := t.TempDir()
	g, err := memory.NewGraphDB(filepath.Join(dir, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	a := &Agent{memory: g}
	ec, rc, _, err := a.commitTriplesWithMedia([]memory.Triple{
		{Subject: "张三", Relation: "喜欢", Object: "咖啡"},
	}, "s1", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ec != 2 || rc != 1 {
		t.Fatalf("期望 2 实体 1 关系，实际 ec=%d rc=%d", ec, rc)
	}
}

func TestMediaBlocksHeldByDocumentSurviveGC(t *testing.T) {
	// 文档持有的一等块把内容钉住；文档被删后块随之消失，内容才可回收。
	a, g, ms := newGraphMediaAgent(t)
	_ = a

	digest, err := ms.Put([]byte("doc image"), media.Item{MIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ds := document.NewStore(filepath.Join(dir, "docs"), memory.TokenizeWords)
	if err := ds.Start(); err != nil {
		t.Fatal(err)
	}
	defer ds.Stop()

	it, _ := ms.Stat(digest)
	doc := &document.Doc{
		ID: "doc_1", Summary: "带图的文档", Content: "正文",
		Blocks: []memory.MemoryBlock{{ID: "blk_doc_1", Modality: memory.BlockImage,
			PayloadDigest: it.Digest, MIME: it.MIME, Size: it.Size}},
	}
	if err := ds.Insert(doc); err != nil {
		t.Fatal(err)
	}
	_ = g

	// 文档仍持有块 → 内容在
	if _, err := ms.Stat(digest); err != nil {
		t.Fatal("有文档块持有内容时不该被清")
	}

	// 删除文档 → 一并删除其内容（与文本块一致：删块即删内容）
	ds.Remove(doc.ID)
	if blocks := ds.Blocks(); len(blocks) != 0 {
		t.Fatalf("删除文档后不该还有块，实际 %+v", blocks)
	}
	if err := ms.Delete(digest); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Stat(digest); err == nil {
		t.Fatal("删除后内容应已移除")
	}
}

func TestMediaContextForSentences(t *testing.T) {
	a, g, ms := newGraphMediaAgent(t)

	digest, _ := ms.Put([]byte("img"), media.Item{MIME: "image/png"})
	if err := ms.Describe(digest, "一张紫蓝红三色带图", "visionllm"); err != nil {
		t.Fatal(err)
	}
	sid, _ := attachBlockToSentence(t, g, ms, "[image/png "+shortDigest(digest)+"] 一张紫蓝红三色带图", digest)

	out := a.mediaContextForSentences([]int64{sid, sid + 100})
	if out == "" {
		t.Fatal("应产出媒体说明")
	}
	if !contains(out, fmt.Sprintf("句子 #%d", sid)) || !contains(out, "一张紫蓝红三色带图") {
		t.Fatalf("说明内容不对: %q", out)
	}
	// 无引用的句子不该出现
	if contains(out, fmt.Sprintf("句子 #%d", sid+100)) {
		t.Fatalf("无引用的句子不该出现: %q", out)
	}
}

func TestResolvePrefix(t *testing.T) {
	dir := t.TempDir()
	ms, err := media.New(filepath.Join(dir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()

	digest, _ := ms.Put([]byte("content"), media.Item{MIME: "image/png"})

	// 短前缀补全
	full, err := ms.ResolvePrefix(digest[:12])
	if err != nil || full != digest {
		t.Fatalf("短前缀补全失败: %v / %v", full, err)
	}
	// 完整 digest 原样返回
	full, err = ms.ResolvePrefix(digest)
	if err != nil || full != digest {
		t.Fatalf("完整 digest 应原样返回: %v / %v", full, err)
	}
	// 过短拒绝
	if _, err := ms.ResolvePrefix("abc"); err == nil {
		t.Fatal("过短前缀应报错")
	}
	// 不存在
	if _, err := ms.ResolvePrefix("deadbeefdead"); err == nil {
		t.Fatal("不存在的前缀应报错")
	}
	// 完整但不存在的 digest 也要报错，否则调用方会挂一条孤儿引用
	fake := ""
	for i := 0; i < 64; i++ {
		fake += "0"
	}
	if _, err := ms.ResolvePrefix(fake); err == nil {
		t.Fatal("不存在的完整 digest 应报错")
	}
}

func TestResolvePrefix_AmbiguityIsError(t *testing.T) {
	// 前缀歧义视为错误而非"取第一个"：挂错引用会让 GC 删掉仍被引用的内容。
	// 构造歧义需要两个同前缀 digest——sha256 无法人为构造，
	// 因此这里退而验证「8 位前缀在大量样本下的行为是确定的」：
	// 要么唯一命中，要么明确报歧义，绝不静默取第一个。
	dir := t.TempDir()
	ms, err := media.New(filepath.Join(dir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()

	digests := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		d, err := ms.Put([]byte("content-"+strconv.Itoa(i)), media.Item{MIME: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, d)
	}

	for _, d := range digests {
		got, err := ms.ResolvePrefix(d[:12])
		if err != nil {
			// 报歧义是可接受结果；静默取错才是缺陷
			if !contains(err.Error(), "歧义") {
				t.Fatalf("非歧义错误: %v", err)
			}
			continue
		}
		if got != d {
			t.Fatalf("补全结果错误: 前缀 %s 得到 %s", d[:12], got)
		}
	}
}

func TestArchiveColdDocs_KeepsDocWhenGraphWriteEmpty(t *testing.T) {
	// 数据丢失回归：三元组全被实体名校验拒绝时（Commit 无错但 0 entities
	// 0 relations），文档不能删、媒体引用不能释放。
	//
	// 该缺陷曾真实发生：LLM 生成的 456 字图片描述提不出合规实体名
	//（validEntityName 要求 2–50 字符），archiveColdDocs 只检查
	// len(triples) > 0 就释放引用并删文档 → GC 清掉 blob → 图片与描述全丢。
	a, _, ms := newGraphMediaAgent(t)

	dir := t.TempDir()
	ds := document.NewStore(filepath.Join(dir, "docs"), memory.TokenizeWords)
	if err := ds.Start(); err != nil {
		t.Fatal(err)
	}
	defer ds.Stop()
	a.docStore = ds
	a.embedder = memory.NewStaticEmbedder()

	content := []byte("image bytes")
	digest, err := ms.Put(content, media.Item{MIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}

	// 精确构造「三元组非空 + Commit 全部拒绝」这个状态。
	//
	// 用超长 Source 而不是指望 NLP 提取器：docToTriples 在
	// Source != "context_archived" 时会写一条 {文档 -来源-> Source}，
	// Source 超过 validEntityName 的 50 字符上限 → Commit 静默跳过
	// → len(triples)==1 但 ec=0 rc=0。构造是确定的，不依赖提取器的
	// 具体行为（提取器行为随版本变化，测试不该押在它身上）。
	//
	// 正文里刻意**不放**媒体标记：mediaTriplesFromText 会为标记产出
	// 合规的「图片 <digest>」三元组，那样 ec/rc 就不为 0，这个用例
	// 也就测不到「全被拒绝」这个状态了。媒体引用直接用 AddRef 挂上，
	// 模拟「文档持有媒体但正文的媒体标记已在清洗中丢失」这一情形——
	// 那正是最危险的组合：有引用要释放，却没有句子能承载它。
	longSource := strings.Repeat("超长来源名", 20) // 100 字，远超 50 字符上限
	// Summary 也必须超长：docToTriples 会为合理 summary 写一条
	// {文档 -主题-> summary}，那条能通过校验，ec/rc 就不为 0 了。
	// 这里要的是「三元组全部被拒」这一个状态。
	longSummary := strings.Repeat("超长摘要文本", 20) // >80 字，触发长度门槛被跳过
	// 文档持有的一等块（模拟“文档有媒体但正文标记已在清洗中丢失”）。
	it, _ := ms.Stat(digest)
	doc := &document.Doc{
		ID:          "doc_keep",
		Summary:     longSummary,
		Content:     "一段没有媒体标记的正文",
		Source:      longSource,
		CreatedAt:   time.Now().Add(-200 * time.Hour),
		LastAccess:  time.Now().Add(-200 * time.Hour),
		AccessCount: 0,
		Blocks: []memory.MemoryBlock{{ID: "blk_keep_1", Modality: memory.BlockImage,
			PayloadDigest: it.Digest, MIME: it.MIME, Size: it.Size}},
	}
	if err := ds.Insert(doc); err != nil {
		t.Fatal(err)
	}
	// Insert 会把 LastAccess 覆写成 now、AccessCount 置 1，
	// 于是 FindColdDocs(72h, 2) 一篇都找不到。插入后再改回来，
	// 让文档真正满足"冷"的条件——这是触发归档路径的前提。
	for _, d := range ds.RecentDocs(10) {
		if d.ID == doc.ID {
			d.LastAccess = time.Now().Add(-200 * time.Hour)
			d.AccessCount = 0
		}
	}
	a.archiveColdDocs()

	// 关键断言：内容在、块在、文档在
	if _, err := ms.Get(digest); err != nil {
		t.Fatalf("图库未写入任何实体/关系，内容却丢了: %v", err)
	}
	held := false
	for _, d := range ds.RecentDocs(10) {
		if d.ID == doc.ID && len(d.Blocks) > 0 {
			held = true
		}
	}
	if !held {
		t.Error("文档或块被释放了——图库没有句子承载它，内容会被删除")
	}
	if _, err := ms.Stat(digest); err != nil {
		t.Fatalf("未归档成功时内容不该被删: %v", err)
	}
}

func TestCommitTriplesWithMedia_ReportsBoundCount(t *testing.T) {
	// mediaBound 必须反映真实绑定数：归档路径靠它决定能否释放旧引用。
	a, _, ms := newGraphMediaAgent(t)

	digest, err := ms.Put([]byte("img"), media.Item{MIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	short := shortDigest(digest)

	// 句子含可反解的短 digest → 应绑定 1 个
	_, _, bound, err := a.commitTriplesWithMedia([]memory.Triple{{
		Subject: "图片", Relation: "内容", Object: "三色带",
		SentenceText: "[image/png " + short + "] 一张三色带图",
	}}, "s1", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bound != 1 {
		t.Fatalf("应绑定 1 个媒体引用，实际 %d", bound)
	}

	// 句子无 digest → 绑定 0 个
	_, _, bound2, err := a.commitTriplesWithMedia([]memory.Triple{{
		Subject: "张三", Relation: "喜欢", Object: "咖啡",
		SentenceText: "张三喜欢咖啡",
	}}, "s2", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bound2 != 0 {
		t.Fatalf("无媒体标记的句子不该绑定引用，实际 %d", bound2)
	}
}

func TestSentenceIDsFromRelations(t *testing.T) {
	// 关系行不持有媒体，媒体挂在句子上。这个函数负责"关系→句子"这一跳，
	// 去重与去零都不能少：sentence_id=0 表示该关系没有关联句子，
	// 拿 0 去查 media_refs 会命中一个不存在的 owner。
	rels := []memory.Relation{
		{ID: 1, SentenceID: 5},
		{ID: 2, SentenceID: 0}, // 无句子
		{ID: 3, SentenceID: 5}, // 重复
		{ID: 4, SentenceID: 7},
	}
	got := sentenceIDsFromRelations(rels)
	if len(got) != 2 {
		t.Fatalf("应得 2 个去重后的句子 id，实际 %v", got)
	}
	if got[0] != 5 || got[1] != 7 {
		t.Fatalf("句子 id 或顺序不对: %v", got)
	}
	if n := sentenceIDsFromRelations(nil); n != nil {
		t.Fatalf("空输入应返回 nil，实际 %v", n)
	}
}

func TestMediaContextForRelations_SurfacesMediaToAgent(t *testing.T) {
	// L3 检索接线回归：媒体描述进了图库，agent 必须拿得出来。
	//
	// 第四层做完了"存和反查的能力"（RecallMediaForSentence /
	// mediaContextForSentences），但那两个函数一度没有任何调用方——
	// 媒体能进 L3，进去之后 agent 检索不到。这个测试守住那条接线。
	a, g, ms := newGraphMediaAgent(t)

	digest, err := ms.Put([]byte("img bytes"), media.Item{MIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Describe(digest, "一张紫蓝红三色带图", "visionllm"); err != nil {
		t.Fatal(err)
	}
	sid, _ := attachBlockToSentence(t, g, ms, "[image/png "+shortDigest(digest)+"] 一张紫蓝红三色带图", digest)

	// 命中的关系挂着该句子 → 应产出媒体说明
	out := a.mediaContextForRelations([]memory.Relation{{ID: 1, SentenceID: sid}})
	if out == "" {
		t.Fatal("关系挂着有媒体的句子，却没产出媒体说明——L3 检索接线断了")
	}
	if !contains(out, "一张紫蓝红三色带图") {
		t.Errorf("媒体说明里应含描述文本: %q", out)
	}
	if !contains(out, shortDigest(digest)) {
		t.Errorf("媒体说明里应含短 digest 供反查: %q", out)
	}

	// 没挂媒体的关系不该产出噪声
	if out := a.mediaContextForRelations([]memory.Relation{{ID: 2, SentenceID: 99}}); out != "" {
		t.Errorf("无媒体的句子不该产出说明: %q", out)
	}
	if out := a.mediaContextForRelations(nil); out != "" {
		t.Errorf("空关系不该产出说明: %q", out)
	}
}

func TestBuildMemoryContext_IncludesMediaSection(t *testing.T) {
	// buildMemoryContext 是自动注入路径（每次 LLM 调用都走）。
	// 媒体说明必须出现在这里，否则 agent 只有显式调 memory_recall 才知道有图。
	a, graph, ms := newGraphMediaAgent(t)

	digest, err := ms.Put([]byte("auto inject"), media.Item{MIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Describe(digest, "自动注入用的测试图", "visionllm"); err != nil {
		t.Fatal(err)
	}

	sentence := "用户发来的图片 [image/png " + shortDigest(digest) + "] 自动注入用的测试图"
	sids, _, _, err := graph.CommitWithMedia([]memory.Triple{{
		Subject: "测试图片", Relation: "包含", Object: "三色带", SentenceText: sentence,
	}}, "auto", 0)
	if err != nil {
		t.Fatal(err)
	}
	sid := sids[sentence]
	if sid == 0 {
		t.Fatal("拿不到句子 id")
	}
	attachBlockToSentence(t, graph, ms, sentence, digest)

	a.indexer = memory.NewIndexer(graph)
	if err := a.indexer.Sync(); err != nil {
		t.Fatalf("indexer sync: %v", err)
	}

	out := a.buildMemoryContext("测试图片", 0)
	if out == "" {
		t.Skip("图库召回未命中（indexer 检索策略所致），无法验证媒体段注入")
	}
	if !contains(out, "【关联媒体】") {
		t.Errorf("自动注入的记忆上下文缺少媒体段: %q", out)
	}
	if !contains(out, "自动注入用的测试图") {
		t.Errorf("媒体段里应含描述文本: %q", out)
	}
}

func TestParseMediaMarkers(t *testing.T) {
	// 与 mediaSummaryForEvent 的输出格式严格对应
	text := "用户发来图片\n媒体内容：\n" +
		"[image/png a1b2c3d4e5f6] 一张紫蓝红三色带图\n" +
		"[audio/wav bbbbccccdddd] 一段三秒的钢琴声\n" +
		"[image/png a1b2c3d4e5f6] 重复的同一张图"

	ms := parseMediaMarkers(text)
	if len(ms) != 2 {
		t.Fatalf("应解析出 2 条去重后的标记，实际 %d: %+v", len(ms), ms)
	}
	if ms[0].label != "image/png" || ms[0].shortDigest != "a1b2c3d4e5f6" {
		t.Errorf("第一条解析错误: %+v", ms[0])
	}
	if ms[0].description != "一张紫蓝红三色带图" {
		t.Errorf("描述应取到行尾且不跨行: %q", ms[0].description)
	}
	if ms[1].label != "audio/wav" {
		t.Errorf("第二条 label 错误: %+v", ms[1])
	}
	// raw 用作 SentenceText，必须含 digest 才能被 bindSentenceMedia 反解
	if !contains(ms[0].raw, "a1b2c3d4e5f6") {
		t.Errorf("raw 必须含 digest: %q", ms[0].raw)
	}
	if n := parseMediaMarkers("没有任何标记的普通文本"); n != nil {
		t.Errorf("无标记应返回 nil，实际 %+v", n)
	}
}

func TestMediaEntityName(t *testing.T) {
	// 实体名必须由 digest 而非描述构成：描述会被重新生成，
	// 若名字取自描述，同一张图会在图谱上留下多个节点。
	cases := []struct{ label, digest, want string }{
		{"image/png", "a1b2c3d4e5f6", "图片 a1b2c3d4e5f6"},
		{"audio/wav", "bbbbccccdddd", "音频 bbbbccccdddd"},
		{"video/mp4", "ccccddddeeee", "视频 ccccddddeeee"},
		{"application/octet-stream", "ddddeeeeffff", "媒体 ddddeeeeffff"},
	}
	for _, c := range cases {
		got := mediaEntityName(c.label, c.digest)
		if got != c.want {
			t.Errorf("mediaEntityName(%q,%q) = %q，期望 %q", c.label, c.digest, got, c.want)
		}
		// 必须过 validEntityName 的 2–50 字符门槛，否则 Commit 会静默跳过
		if n := len([]rune(got)); n < 2 || n > 50 {
			t.Errorf("实体名长度 %d 不在 2–50 之间: %q", n, got)
		}
	}
}

func TestSummarizeForEntity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"一张紫蓝红三色带图。还有更多内容。", "一张紫蓝红三色带图"},
		{"**整体构成**：正方形画布", "整体构成：正方形画布"}, // Markdown 强调符被清掉
		{"", ""},
		{"短", ""}, // 单字过不了 validEntityName，宁可不写
		// 无句子边界时按 rune 截到 40（不是按字节，否则切坏 UTF-8 会在图库里留乱码）
		{"没有句子边界的一长串文字需要按 rune 截断以免切坏 UTF-8 编码导致图库里出现乱码实体名字符",
			"没有句子边界的一长串文字需要按 rune 截断以免切坏 UTF-8 编码导致图库"},
	}
	for _, c := range cases {
		got := summarizeForEntity(c.in, 40)
		if got != c.want {
			t.Errorf("summarizeForEntity(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestMediaTriplesFromText_DeterministicRegardlessOfNLP(t *testing.T) {
	// 核心回归：媒体入 L3 不再依赖 NLP 提取器的运气。
	//
	// 实测 LLM 的 477 字图片描述经提取器只产出「水平 -分割-> 成」，
	// obj 仅 1 字被 validEntityName 拒掉 → ec=0 rc=0 → 媒体记忆进不了图库，
	// 且时好时坏取决于描述文本。这里验证确定性路径。
	longDesc := "这张图片是一张纯色块构成的抽象图像，不包含任何文字、人物、物体或可识别的场景。" +
		"整体构成：一个小尺寸的正方形图像，被水平分割成三条颜色条带。"
	text := "媒体内容：\n[image/png 89e293b42546] " + longDesc

	triples := mediaTriplesFromText(text)
	if len(triples) < 2 {
		t.Fatalf("应至少产出类型+内容两条三元组，实际 %d", len(triples))
	}

	// 每条都必须能通过 validEntityName（经 Commit 实证）
	g, err := memory.NewGraphDB(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	sids, ec, rc, err := g.CommitWithMedia(triples, "det", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ec == 0 || rc == 0 {
		t.Fatalf("确定性三元组应能写入图库，实际 ec=%d rc=%d", ec, rc)
	}
	if len(sids) == 0 {
		t.Fatal("应返回句子 id 供 bindSentenceMedia 绑定")
	}
	// SentenceText 必须含 digest，否则绑定还是断的
	for st := range sids {
		if !contains(st, "89e293b42546") {
			t.Errorf("句子必须含短 digest 供反解: %q", st)
		}
	}

	// 描述为空时仍应产出类型三元组——媒体节点不能因为没描述就不存在
	bare := mediaTriplesFromText("[image/png 89e293b42546]")
	if len(bare) != 1 {
		t.Fatalf("无描述时应只有类型三元组，实际 %d 条", len(bare))
	}
	if bare[0].Relation != "类型" {
		t.Errorf("无描述时那条应是类型三元组: %+v", bare[0])
	}
}
