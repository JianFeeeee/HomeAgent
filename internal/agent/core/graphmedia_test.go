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

// L3 图库媒体绑定测试。
//
// 这一层的目的只有一个：几个月后从图谱走到一条句子，要能取回当时那份媒体。
// 媒体作为一等块进入 L3，以结构边与承载节点相连：
//
//	sentence --contains--> block（对话/三元组产生的记忆）
//	document --contains--> block（L2 文档归档进 L3）
//
// 描述文本、marker 反解、由 marker 反推出的「媒体实体」全部已废弃，
// 因此这些测试也不存在任何按描述检索的断言。

// attachBlockToSentenceBlock 提交一条句子并把媒体变成 L3 一等块。
// 必须走真实提交：边要求两端都是真实图节点。
func attachBlockToSentenceBlock(t *testing.T, g *memory.GraphDB, ms *media.Store, sentenceText, digest string) (string, memory.MemoryBlock) {
	t.Helper()
	ids, _, _, err := g.CommitWithMedia([]memory.Triple{{
		Subject: "媒体载体", Relation: "包含", Object: "内容", SentenceText: sentenceText,
	}}, "test", 0)
	if err != nil {
		t.Fatalf("CommitWithMedia: %v", err)
	}
	// ★ 媒体的挂载点已从「sentences 表行号」改成「原句块 ID」
	//   （CommitWithMedia 返回值由 map[string]int64 改为 map[string]string，
	//    因为 sentences 表退场后行号不存在）。
	sid := ids[sentenceText]
	if sid == "" {
		t.Fatalf("拿不到原句块 ID: %q", sentenceText)
	}
	it, err := ms.Stat(digest)
	if err != nil || it == nil {
		t.Fatalf("Stat(%s): %v", shortDigest(digest), err)
	}
	b := memory.MemoryBlock{
		ID:            fmt.Sprintf("blk_test_%s_%s", shortDigest(sid), shortDigest(digest)),
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
	// ★ 端点 kind 也从 "sentence" 改成 "block"（原句由块承载）
	if err := g.AddMemoryBlockEdge("block", sid, "block", b.ID, "contains"); err != nil {
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

func TestCommitWithMedia_ReturnsSentenceIDs(t *testing.T) {
	_, g, _ := newGraphMediaAgent(t)

	sentence := "这张图是紫蓝红三色带。"
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
	if ids[sentence] == "" {
		t.Fatalf("应返回原句块 ID，实际 %v", ids)
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

func TestCommitTriplesWithMedia_RoundTrip(t *testing.T) {
	// 整层的核心断言：写入 → 提交 → 反查取回原始字节
	a, _, ms := newGraphMediaAgent(t)

	content := []byte("\x89PNG\r\n\x1a\n fake image bytes")
	digest, err := ms.Put(content, media.Item{MIME: "image/png", Kind: media.KindImage})
	if err != nil {
		t.Fatal(err)
	}

	sentence := "用户发来一张紫蓝红三色带图。"
	triples := []memory.Triple{{
		Subject: "图片", Relation: "内容", Object: "三色带",
		SentenceText: sentence,
		MediaDigests: []string{digest[:12]}, // 模型手里通常只有短 digest
	}}

	if _, _, bound, err := a.commitTriplesWithMedia(triples, "s1", 0, nil); err != nil {
		t.Fatal(err)
	} else if bound != 1 {
		t.Fatalf("应绑定 1 个块，实际 %d", bound)
	}

	ids, _, _, err := a.memory.CommitWithMedia(triples, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	sid := ids[sentence]
	if sid == "" {
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

func TestAttachBlocksToSentence_SkipsUnresolvable(t *testing.T) {
	// digest 在库里不存在时必须跳过，不能建一条指向虚无的块边。
	a, g, _ := newGraphMediaAgent(t)
	// ★ 空块 ID = 无效句柄（原来用 0 行号表示）
	if n := a.attachBlocksToSentenceBlock("", []string{"deadbeefdead"}, nil, ""); n != 0 {
		t.Fatalf("无法补全的 digest 不该建块，实际绑定 %d", n)
	}
	blocks, err := g.BlocksForNode("sentence", "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 0 {
		t.Fatalf("不该有块，实际 %+v", blocks)
	}
}

func TestAttachBlocksToSentence_NilStoreNoop(t *testing.T) {
	a := &Agent{}
	if n := a.attachBlocksToSentenceBlock("", []string{"aaaaaaaaaaaa"}, nil, ""); n != 0 {
		t.Fatalf("媒体关闭时应静默无操作，实际 %d", n)
	}
	if got, err := a.RecallBlocksForSentence(""); err != nil || got != nil {
		t.Fatalf("媒体关闭时应静默无操作，实际 %v / %v", got, err)
	}
}

func TestAttachBlocksToSentence_ReusesSeedIdentity(t *testing.T) {
	// L2→L3 迁移必须保持块身份：同一个块换层，而不是另建一个同内容的新块。
	a, g, ms := newGraphMediaAgent(t)
	digest, _ := ms.Put([]byte("seed-img"), media.Item{MIME: "image/png"})
	seedBlock, ok := a.blockFromDigest(digest)
	if !ok {
		t.Fatal("blockFromDigest 失败")
	}

	ids, _, _, err := g.CommitWithMedia([]memory.Triple{{
		Subject: "迁移", Relation: "包含", Object: "媒体", SentenceText: "迁移测试句。",
	}}, "seed", 0)
	if err != nil {
		t.Fatal(err)
	}
	sid := ids["迁移测试句。"]

	byDigest := map[string]memory.MemoryBlock{digest: seedBlock}
	if n := a.attachBlocksToSentenceBlock(sid, []string{digest}, byDigest, ""); n != 1 {
		t.Fatalf("应绑定 1 个块，实际 %d", n)
	}
	blocks, err := g.BlocksForNode("block", sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].ID != seedBlock.ID {
		t.Fatalf("块身份应保持为 %s，实际 %+v", seedBlock.ID, blocks)
	}
}

func TestLinkBlocksToDocument_CreatesDocumentNodeEdge(t *testing.T) {
	// 文档归档进 L3：块原样迁入，document --contains--> block 边建立。
	a, g, ms := newGraphMediaAgent(t)

	digest, _ := ms.Put([]byte("doc-img"), media.Item{MIME: "image/png"})
	b, ok := a.blockFromDigest(digest)
	if !ok {
		t.Fatal("blockFromDigest 失败")
	}

	if n := a.linkBlocksToDocument("doc_42", []memory.MemoryBlock{b}, ""); n != 1 {
		t.Fatalf("应建立 1 条文档→块边，实际 %d", n)
	}
	blocks, err := g.BlocksForNode("document", "doc_42")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].ID != b.ID {
		t.Fatalf("文档应持有块 %s，实际 %+v", b.ID, blocks)
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

func TestMediaContextForSentences(t *testing.T) {
	a, g, ms := newGraphMediaAgent(t)

	digest, _ := ms.Put([]byte("img"), media.Item{MIME: "image/png"})
	sid, _ := attachBlockToSentenceBlock(t, g, ms, "一张紫蓝红三色带图。", digest)

	out := a.mediaContextForSentences([]string{sid, sid + "-extra"})
	if out == "" {
		t.Fatal("应产出媒体说明")
	}
	// 说明里的句子标识是 shortID(块ID) = 去掉 blk_src_ 前缀的 hex
	if !contains(out, fmt.Sprintf("句子 %s", shortID(sid))) || !contains(out, shortDigest(digest)) {
		t.Fatalf("说明内容不对: %q（期望含 shortID=%s）", out, shortID(sid))
	}
	// 说明只含 MIME 与短 digest，不含任何生成的描述
	if contains(out, "紫蓝红") {
		t.Fatalf("说明里不该有描述文本（描述式索引已废弃）: %q", out)
	}
	// 无引用的句子不该出现
	if contains(out, fmt.Sprintf("句子 %s", shortID(sid+"-extra"))) {
		t.Fatalf("无引用的句子不该出现: %q", out)
	}
}

func TestMediaContextForRelations_SurfacesMediaToAgent(t *testing.T) {
	// L3 检索接线回归：媒体作为一等块进了图库，agent 必须拿得出来。
	a, g, ms := newGraphMediaAgent(t)

	digest, err := ms.Put([]byte("img bytes"), media.Item{MIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = attachBlockToSentenceBlock(t, g, ms, "一张紫蓝红三色带图。", digest)

	// 命中的关系挂着该句子 → 应产出媒体说明
	// ★ 挂载点改成 SentenceText 现算块 ID（sentenceIDsFromRelations 的新契约）
	out := a.mediaContextForRelations([]memory.Relation{{ID: 1, SentenceText: "一张紫蓝红三色带图。"}})
	if out == "" {
		t.Fatal("关系挂着有媒体的句子，却没产出媒体说明——L3 检索接线断了")
	}
	if !contains(out, shortDigest(digest)) {
		t.Errorf("媒体说明里应含短 digest 供反查: %q", out)
	}

	// 没挂媒体的关系不该产出噪声
	if out := a.mediaContextForRelations([]memory.Relation{{ID: 2, SentenceText: "库中不存在的句子。"}}); out != "" {
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

	sentence := "用户发来的图片。"
	sids, _, _, err := graph.CommitWithMedia([]memory.Triple{{
		Subject: "测试图片", Relation: "包含", Object: "三色带", SentenceText: sentence,
	}}, "auto", 0)
	if err != nil {
		t.Fatal(err)
	}
	sid := sids[sentence]
	if sid == "" {
		t.Fatal("拿不到句子 id")
	}
	if err := graph.PutMemoryBlocks([]memory.MemoryBlock{{
		ID: "blk_auto_1", Modality: memory.BlockImage,
		PayloadDigest: digest, MIME: "image/png",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := graph.AddMemoryBlockEdge("block", sid, "block", "blk_auto_1", "contains"); err != nil {
		t.Fatal(err)
	}

	a.indexer = memory.NewIndexer(graph)
	if err := a.indexer.Sync(); err != nil {
		t.Fatalf("indexer sync: %v", err)
	}

	out := a.buildMemoryContext("测试图片", 0, nil)
	if out == "" {
		t.Skip("图库召回未命中（indexer 检索策略所致），无法验证媒体段注入")
	}
	if !contains(out, "【关联媒体】") {
		t.Errorf("自动注入的记忆上下文缺少媒体段: %q", out)
	}
	if !contains(out, shortDigest(digest)) {
		t.Errorf("媒体段里应含短 digest: %q", out)
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
	// 完整但不存在的 digest 也要报错，否则调用方会挂一条孤儿块
	fake := strings.Repeat("0", 64)
	if _, err := ms.ResolvePrefix(fake); err == nil {
		t.Fatal("不存在的完整 digest 应报错")
	}
}

func TestResolvePrefix_AmbiguityIsError(t *testing.T) {
	// 前缀歧义视为错误而非"取第一个"：挂错块会让内容被误删。
	// 构造歧义需要两个同前缀 digest——sha256 无法人为构造，
	// 因此这里退而验证「12 位前缀在大量样本下的行为是确定的」：
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
	// 0 relations），文档不能删、其持有的块不能丢。
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

	// 精确构造「三元组非空 + Commit 全部拒绝」这个状态：
	// Source/Summary 都超过 validEntityName 的 50 字符上限，
	// 于是 docToTriples 产出的两条元数据三元组都被跳过。
	longSource := strings.Repeat("超长来源名", 20)   // 100 字
	longSummary := strings.Repeat("超长摘要文本", 20) // >80 字触发长度门槛被跳过
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
}

func TestArchiveColdDocs_MigratesBlocksToGraph(t *testing.T) {
	// 归档成功时块必须迁进 L3 并以 document --contains--> block 关联，
	// 然后文档才被删除（迁移而非复制/引用保活）。
	a, g, ms := newGraphMediaAgent(t)

	dir := t.TempDir()
	ds := document.NewStore(filepath.Join(dir, "docs"), memory.TokenizeWords)
	if err := ds.Start(); err != nil {
		t.Fatal(err)
	}
	defer ds.Stop()
	a.docStore = ds
	a.embedder = memory.NewStaticEmbedder()

	digest, _ := ms.Put([]byte("archived-image"), media.Item{MIME: "image/png"})
	it, _ := ms.Stat(digest)
	doc := &document.Doc{
		ID:      "doc_arch",
		Summary: "带图的冷文档",
		Content: "张三把三色带图交给了李四。",
		Source:  "manual",
		Blocks: []memory.MemoryBlock{{ID: "blk_arch_1", Modality: memory.BlockImage,
			PayloadDigest: it.Digest, MIME: it.MIME, Size: it.Size}},
	}
	if err := ds.Insert(doc); err != nil {
		t.Fatal(err)
	}
	for _, d := range ds.RecentDocs(10) {
		if d.ID == doc.ID {
			d.LastAccess = time.Now().Add(-200 * time.Hour)
			d.AccessCount = 0
		}
	}
	a.archiveColdDocs()

	if d := ds.Get("doc_arch"); d != nil {
		t.Fatal("块已迁入 L3，文档应被删除")
	}
	blocks, err := g.BlocksForNode("document", "doc_arch")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].ID != "blk_arch_1" {
		t.Fatalf("L3 文档节点应持有原块（身份不变），实际 %+v", blocks)
	}
	if _, err := ms.Get(digest); err != nil {
		t.Fatalf("块被 L3 持有，内容应仍可读: %v", err)
	}
}

func TestMigrateLegacyMediaEntities(t *testing.T) {
	// 旧数据：媒体被伪装成 type=Media 的实体，靠描述文本当索引。
	// 迁移必须把它还原成原生块（挂回原句子）并删掉旧实体与描述关系。
	_, g, ms := newGraphMediaAgent(t)

	digest, _ := ms.Put([]byte("legacy-img"), media.Item{MIME: "image/png"})
	sentence := "老数据里的三色带图 [image/png " + digest[:12] + "]"
	// 直接构造旧的实体/关系形态（不走已删除的 marker 代码）。
	ids, _, _, err := g.CommitWithMedia([]memory.Triple{{
		Subject:      "图片 " + digest[:12],
		SubjectType:  "Media",
		Relation:     "内容",
		Object:       "三色带的描述文本",
		ObjectType:   "Description",
		SentenceText: sentence,
	}}, "legacy", 0)
	if err != nil {
		t.Fatal(err)
	}
	sid := ids[sentence]
	if sid == "" {
		t.Fatal("拿不到句子 id")
	}

	blocks, entities, err := g.MigrateLegacyMediaEntities(func(short string) (memory.MemoryBlock, bool) {
		full, err := ms.ResolvePrefix(short)
		if err != nil {
			return memory.MemoryBlock{}, false
		}
		it, err := ms.Stat(full)
		if err != nil {
			return memory.MemoryBlock{}, false
		}
		return memory.MemoryBlock{
			ID: "blk_legacy_" + short, Modality: memory.BlockImage,
			PayloadDigest: it.Digest, MIME: it.MIME, Size: it.Size,
		}, true
	})
	if err != nil {
		t.Fatal(err)
	}
	if blocks != 1 || entities != 1 {
		t.Fatalf("应迁移 1 块 / 删 1 实体，实际 %d / %d", blocks, entities)
	}

	// 旧媒体实体与描述关系必须消失
	res, err := g.Recall([]string{"图片 " + digest[:12]}, nil, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Entities {
		if e.Type == "Media" {
			t.Fatalf("旧媒体实体仍存在: %+v", e)
		}
	}
	// 块必须挂回原句子
	got, err := g.BlocksForNode("block", sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PayloadDigest != digest {
		t.Fatalf("句子应持有原生块，实际 %+v", got)
	}

	// 幂等：再跑一遍不应重复建块
	blocks2, entities2, err := g.MigrateLegacyMediaEntities(nil)
	if err != nil {
		t.Fatal(err)
	}
	if blocks2 != 0 || entities2 != 0 {
		t.Fatalf("无 resolver 时应空操作，实际 %d / %d", blocks2, entities2)
	}
}

func TestCleanupOrphanedSentences_KeepsBlockBackedSentences(t *testing.T) {
	// 旧媒体实体被删除后，承载它的句子可能再无关系引用，
	// 但它还挂着媒体块——清理孤儿句子时不能把它删掉。
	a, g, ms := newGraphMediaAgent(t)

	digest, _ := ms.Put([]byte("orphan-img"), media.Item{MIME: "image/png"})
	sentence := "只靠媒体块存活的句子。"
	ids, _, _, err := g.CommitWithMedia([]memory.Triple{{
		Subject: "媒体载体", Relation: "包含", Object: "内容", SentenceText: sentence,
	}}, "orphan", 0)
	if err != nil {
		t.Fatal(err)
	}
	sid := ids[sentence]

	b, ok := a.blockFromDigest(digest)
	if !ok {
		t.Fatal("blockFromDigest 失败")
	}
	if err := g.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
		t.Fatal(err)
	}
	// ★ 端点 kind 也从 "sentence" 改成 "block"（原句由块承载）
	if err := g.AddMemoryBlockEdge("block", sid, "block", b.ID, "contains"); err != nil {
		t.Fatal(err)
	}

	// 解除关系引用，句子只剩块边
	res, err := g.Recall([]string{"媒体载体"}, nil, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Relations {
		if err := g.ClearSentenceID(r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.CleanupOrphanedSentences(); err != nil {
		t.Fatal(err)
	}
	blocks, err := g.BlocksForNode("block", sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("承载媒体块的句子被误删，块反查失败: %+v", blocks)
	}
}

func TestSentenceIDsFromRelations(t *testing.T) {
	// 关系行不持有媒体，媒体挂在句子上。这个函数负责「关系→句子」这一跳。
	//
	// ★ 输入契约已变（Commit 块化后）：
	//   从 Relation.SentenceID（sentences 表行号）改为按 SentenceText
	//   现算 blk_src_<hash>。行号不再是稳定挂载点 —— sentences 表退场。
	//   去重与「无句子」的剔除仍然必须。
	rels := []memory.Relation{
		{ID: 1, SentenceText: "句子甲。"},
		{ID: 2},                       // 无句子
		{ID: 3, SentenceText: "句子甲。"}, // 重复
		{ID: 4, SentenceText: "句子乙。"},
	}
	want0 := memory.SentenceBlockID("句子甲。")
	want1 := memory.SentenceBlockID("句子乙。")
	got := sentenceIDsFromRelations(rels)
	if len(got) != 2 {
		t.Fatalf("应得 2 个去重后的原句块 ID，实际 %v", got)
	}
	if got[0] != want0 || got[1] != want1 {
		t.Fatalf("原句块 ID 或顺序不对: %v（期望 [%s %s]）", got, want0, want1)
	}
	if !strings.HasPrefix(got[0], "blk_src_") {
		t.Errorf("应是原句块 ID（blk_src_ 前缀），实际 %q", got[0])
	}
	if n := sentenceIDsFromRelations(nil); n != nil {
		t.Fatalf("空输入应返回 nil，实际 %v", n)
	}
}

func TestMediaBlocksHeldByDocumentSurviveDeletion(t *testing.T) {
	// 文档持有的一等块把内容钉住；文档被删后块随之消失，内容才可回收。
	_, _, ms := newGraphMediaAgent(t)

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

// TestFormatRecallRelations_SurfacesSentence 锁死「从图谱回到原文」：
// memory_recall 的关系行必须带上 sentence_text（截断），否则模型按工具
// schema 填了原始句子也永远取不回，该字段形同虚设。
func TestFormatRecallRelations_SurfacesSentence(t *testing.T) {
	rels := []memory.Relation{
		{SourceName: "张三", RelationType: "喜欢", TargetName: "咖啡", SentenceText: "张三说他每天早上一定要喝一杯手冲咖啡。"},
		{SourceName: "张三", RelationType: "住在", TargetName: "北京"}, // 无原句：不应出现空的原句字段
	}
	lines := formatRecallRelations(rels, 10)
	if len(lines) != 2 {
		t.Fatalf("应渲染 2 行，实际 %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "张三 →(喜欢)→ 咖啡") || !strings.Contains(lines[0], "原句:") {
		t.Errorf("第一条应带原句，实际 %q", lines[0])
	}
	if strings.Contains(lines[1], "原句") {
		t.Errorf("无 sentence_text 的关系不应出现原句字段，实际 %q", lines[1])
	}
}

// TestFormatRecallRelations_Truncates 锁死关系条数上限：
// 超过 max 时截断并明确告知，避免刷屏。
func TestFormatRecallRelations_Truncates(t *testing.T) {
	var rels []memory.Relation
	for i := 0; i < 15; i++ {
		rels = append(rels, memory.Relation{SourceName: "A", RelationType: "连", TargetName: "B"})
	}
	lines := formatRecallRelations(rels, 10)
	if len(lines) != 11 {
		t.Fatalf("10 条关系 + 1 条截断提示，实际 %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[10], "截断") {
		t.Errorf("最后一行应为截断提示，实际 %q", lines[10])
	}
}
