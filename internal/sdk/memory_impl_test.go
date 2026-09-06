package sdk

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	doc "gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
)

// 插件边界的媒体透传测试。
//
// 断言的都是不变量，而不是"函数被调过"：
//  1. 插件填的字段一个都不许丢（旧实现静默裁掉 Confidence/类型/SentenceText）；
//  2. 插件不必知道媒体标记格式，内核负责补；
//  3. 媒体引用挂到正确的 owner 上，删除时释放；
//  4. mediaStore 为 nil 时整条链路退化成纯文本，不 panic 不报错。

func newTestStores(t *testing.T) (*memory.GraphDB, *doc.Store, *text.Memory, *media.Store) {
	t.Helper()
	dir := t.TempDir()

	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	ds := doc.NewStore(filepath.Join(dir, "documents"))
	if err := ds.Start(); err != nil {
		t.Fatalf("doc store start: %v", err)
	}
	t.Cleanup(func() { ds.Stop() })

	tm := text.New(filepath.Join(dir, "text"))
	if err := tm.Start(); err != nil {
		t.Fatalf("text memory start: %v", err)
	}
	t.Cleanup(func() { tm.Stop() })

	ms, err := media.New(filepath.Join(dir, "media"), 0)
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	return g, ds, tm, ms
}

// putDescribed 存一份带描述的媒体，返回完整 digest。
func putDescribed(t *testing.T, ms *media.Store, payload, desc string) string {
	t.Helper()
	d, err := ms.Put([]byte(payload), media.Item{MIME: "image/png", Description: desc})
	if err != nil {
		t.Fatalf("media.Put: %v", err)
	}
	return d
}

// ---------- 图记忆 ----------

// 旧实现只搬 Subject/Relation/Object，其余字段静默丢弃：
// 插件标注的类型全部落成默认 Concept，置信度全成 1.0，SentenceText 直接消失。
func TestGraphCommit_CarriesAllFields(t *testing.T) {
	g, _, _, ms := newTestStores(t)
	m := NewGraphMemoryWithMedia("tester", g, ms)

	err := m.Commit([]Triple{{
		Subject:      "张三",
		Relation:     "养",
		Object:       "橘猫",
		Confidence:   0.75,
		SubjectType:  "Person",
		ObjectType:   "Animal",
		SentenceText: "张三养了一只橘猫。",
	}})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, err := g.Recall([]string{"张三"}, nil, 2, "")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Relations) == 0 {
		t.Fatal("召回不到刚提交的关系")
	}
	r := res.Relations[0]
	if r.Confidence != 0.75 {
		t.Errorf("Confidence = %v，期望 0.75（插件标注的置信度被丢弃）", r.Confidence)
	}
	if r.SentenceText != "张三养了一只橘猫。" {
		t.Errorf("SentenceText = %q，期望原句（丢了它媒体就没有落点）", r.SentenceText)
	}

	var subjType, objType string
	for _, e := range res.Entities {
		switch e.Name {
		case "张三":
			subjType = e.Type
		case "橘猫":
			objType = e.Type
		}
	}
	if subjType != "Person" || objType != "Animal" {
		t.Errorf("实体类型 = (%q,%q)，期望 (Person,Animal)", subjType, objType)
	}
}

// 插件只给 digest，标记与句子由内核合成；引用必须挂到 graph_sentence owner 上。
func TestGraphCommit_BindsMediaFromDigests(t *testing.T) {
	g, _, _, ms := newTestStores(t)
	digest := putDescribed(t, ms, "png-bytes", "一张紫蓝红三色带图")

	m := NewGraphMemoryWithMedia("tester", g, ms)
	if err := m.Commit([]Triple{{
		Subject:      "配色图",
		Relation:     "包含",
		Object:       "三色带",
		MediaDigests: []string{digest[:12]}, // 插件手里通常只有短 digest
	}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, err := g.Recall([]string{"配色图"}, nil, 2, "")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Relations) == 0 || res.Relations[0].SentenceID == 0 {
		t.Fatal("没有句子落点 —— 媒体引用无从挂起")
	}
	sid := res.Relations[0].SentenceID

	// 描述必须进句子：描述文本才是持久语义记忆，检索靠它。
	if !strings.Contains(res.Relations[0].SentenceText, "三色带图") {
		t.Errorf("句子里没有媒体描述: %q", res.Relations[0].SentenceText)
	}

	refs, err := ms.Refs(media.OwnerGraphSentence, strconv.FormatInt(sid, 10))
	if err != nil {
		t.Fatalf("Refs: %v", err)
	}
	if len(refs) != 1 || refs[0] != digest {
		t.Errorf("句子 #%d 的媒体引用 = %v，期望 [%s]", sid, refs, digest)
	}
}

// 插件自己按格式写了标记又同时填了 MediaDigests，不能产生两条重复引用/两份标记。
func TestGraphCommit_NoDuplicateMarker(t *testing.T) {
	g, _, _, ms := newTestStores(t)
	digest := putDescribed(t, ms, "dup-bytes", "重复标记测试图")
	short := digest[:12]

	m := NewGraphMemoryWithMedia("tester", g, ms)
	if err := m.Commit([]Triple{{
		Subject:      "重复图",
		Relation:     "标记",
		Object:       "一次",
		SentenceText: "看这个 [image/png " + short + "] 重复标记测试图",
		MediaDigests: []string{short},
	}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, _ := g.Recall([]string{"重复图"}, nil, 2, "")
	if len(res.Relations) == 0 {
		t.Fatal("召回不到关系")
	}
	if n := strings.Count(res.Relations[0].SentenceText, short); n != 1 {
		t.Errorf("句子里出现 %d 次 digest，期望 1 次: %q", n, res.Relations[0].SentenceText)
	}
}

// mediaStore 为 nil 时仍要能提交（媒体是增强，不是必需品），digest 留在文本里备查。
func TestGraphCommit_NilMediaStoreDegrades(t *testing.T) {
	g, _, _, _ := newTestStores(t)
	m := NewGraphMemory(g)

	if err := m.Commit([]Triple{{
		Subject:      "无存储",
		Relation:     "仍可",
		Object:       "提交",
		MediaDigests: []string{"aabbccddeeff"},
	}}); err != nil {
		t.Fatalf("Commit 在无媒体存储时不该失败: %v", err)
	}

	res, _ := g.Recall([]string{"无存储"}, nil, 2, "")
	if len(res.Relations) == 0 {
		t.Fatal("召回不到关系")
	}
	if !strings.Contains(res.Relations[0].SentenceText, "aabbccddeeff") {
		t.Errorf("digest 应留在句子里以备将来反查: %q", res.Relations[0].SentenceText)
	}
}

// Recall 必须把置信度带回插件：拿不到它，插件只能把所有召回结果等同看待。
func TestGraphRecall_CarriesConfidence(t *testing.T) {
	g, _, _, ms := newTestStores(t)
	m := NewGraphMemoryWithMedia("tester", g, ms)

	// 实体名至少两个字符：validEntityName 会静默跳过单字实体，
	// 那样 Commit 返回 nil 但什么都没写，测试会退化成假阳性。
	if err := m.Commit([]Triple{{
		Subject: "甲方", Relation: "疑似", Object: "乙方", Confidence: 0.3,
	}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	_, rels, err := m.Recall([]string{"甲方"}, 2)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(rels) == 0 {
		t.Fatal("召回为空")
	}
	if rels[0].Confidence != 0.3 {
		t.Errorf("Confidence = %v，期望 0.3", rels[0].Confidence)
	}
}

// ---------- 文档记忆（知识库） ----------

// 附件带 Data → 落进 CAS、标记补进正文、引用挂到文档 owner。
func TestDocInsertWithMedia_StoresAndBinds(t *testing.T) {
	_, ds, _, ms := newTestStores(t)
	dm := NewDocMemoryWithMedia("tester", ds, ms)

	d := &Doc{Title: "带图笔记", Content: "这是正文。"}
	err := dm.InsertWithMedia(d, []MediaAttachment{{
		MIME:        "image/png",
		Data:        []byte("attachment-bytes"),
		Name:        "chart.png",
		Description: "一张柱状图",
	}})
	if err != nil {
		t.Fatalf("InsertWithMedia: %v", err)
	}
	if d.ID == "" {
		t.Fatal("ID 未回填 —— 插件拿不到刚写入文档的 id")
	}

	// 标记必须进正文：向量索引用 Summary+Content 计算，
	// 标记进不去正文就永远检索不到这份媒体。
	if !strings.Contains(d.Content, "柱状图") {
		t.Errorf("正文里没有媒体标记: %q", d.Content)
	}

	refs, err := ms.Refs(media.OwnerDocument, d.ID)
	if err != nil {
		t.Fatalf("Refs: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("文档媒体引用 = %v，期望 1 条", refs)
	}
	// 内容可读，说明真的落盘了而不只是记了个 digest。
	got, err := ms.Get(refs[0])
	if err != nil || string(got) != "attachment-bytes" {
		t.Errorf("媒体内容读回失败: %v / %q", err, got)
	}
}

// 只给 Digest 的附件是「引用已有内容」，不该报错也不该重复落盘。
func TestDocInsertWithMedia_DigestOnlyReference(t *testing.T) {
	_, ds, _, ms := newTestStores(t)
	digest := putDescribed(t, ms, "existing", "已有的图")
	before := ms.Stats()["count"]

	dm := NewDocMemoryWithMedia("tester", ds, ms)
	d := &Doc{Title: "引用已有", Content: "正文"}
	if err := dm.InsertWithMedia(d, []MediaAttachment{{Digest: digest[:10]}}); err != nil {
		t.Fatalf("InsertWithMedia: %v", err)
	}

	if after := ms.Stats()["count"]; after != before {
		t.Errorf("媒体条数从 %v 变成 %v —— 引用已有内容不该新增", before, after)
	}
	refs, _ := ms.Refs(media.OwnerDocument, d.ID)
	if len(refs) != 1 || refs[0] != digest {
		t.Errorf("引用 = %v，期望 [%s]", refs, digest)
	}
}

// Query 必须回媒体元数据但**不回字节**：一次检索可能命中几十份媒体，
// 全塞回插件会把跨进程消息撑爆。
func TestDocQuery_FillsMediaMetadataWithoutBytes(t *testing.T) {
	_, ds, _, ms := newTestStores(t)
	dm := NewDocMemoryWithMedia("tester", ds, ms)

	d := &Doc{Title: "紫蓝红三色带", Content: "配色说明"}
	if err := dm.InsertWithMedia(d, []MediaAttachment{{
		MIME: "image/png", Data: []byte("query-bytes"), Description: "三色带图",
	}}); err != nil {
		t.Fatalf("InsertWithMedia: %v", err)
	}

	got := dm.Query("紫蓝红三色带 配色说明", 3)
	if len(got) == 0 {
		t.Fatal("检索不到刚写入的文档")
	}
	var hit *Doc
	for _, g := range got {
		if g.ID == d.ID {
			hit = g
		}
	}
	if hit == nil {
		t.Fatalf("检索结果里没有目标文档: %+v", got)
	}
	if len(hit.MediaDigests) != 1 {
		t.Errorf("MediaDigests = %v，期望 1 条", hit.MediaDigests)
	}
	if len(hit.Attachments) != 1 {
		t.Fatalf("Attachments = %v，期望 1 条", hit.Attachments)
	}
	att := hit.Attachments[0]
	if att.MIME != "image/png" || att.Description != "三色带图" {
		t.Errorf("附件元数据 = %+v，期望 mime=image/png desc=三色带图", att)
	}
	if len(att.Data) != 0 {
		t.Errorf("Attachments 不该带字节（%d 字节）—— 需要时按 digest 单取", len(att.Data))
	}
}

// 历史文档只有标记、没有 media_refs（旧版插件写入的）。
// 此时要能从正文标记反解出附件，否则那些文档的媒体对插件永远不可见。
func TestDocQuery_FallsBackToMarkers(t *testing.T) {
	_, ds, _, ms := newTestStores(t)
	digest := putDescribed(t, ms, "legacy", "历史图片")

	// 直接写底层 store，绕过 SDK 的绑定逻辑，模拟历史数据。
	if err := ds.Insert(&doc.Doc{
		Summary: "历史文档",
		Content: "旧正文 [image/png " + digest[:12] + "] 历史图片",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	dm := NewDocMemoryWithMedia("tester", ds, ms)
	got := dm.Query("历史文档 旧正文", 3)
	if len(got) == 0 {
		t.Fatal("检索不到历史文档")
	}
	if len(got[0].MediaDigests) != 1 || got[0].MediaDigests[0] != digest {
		t.Errorf("MediaDigests = %v，期望从标记反解出 [%s]", got[0].MediaDigests, digest)
	}
}

// 旧实现删文档不解引用 → 媒体永久"被引用"，GC 收不掉，磁盘只增不减。
func TestDocRemove_ReleasesMediaRefs(t *testing.T) {
	_, ds, _, ms := newTestStores(t)
	dm := NewDocMemoryWithMedia("tester", ds, ms)

	d := &Doc{Title: "待删除", Content: "正文"}
	if err := dm.InsertWithMedia(d, []MediaAttachment{{
		MIME: "image/png", Data: []byte("to-be-freed"), Description: "会被释放的图",
	}}); err != nil {
		t.Fatalf("InsertWithMedia: %v", err)
	}
	if refs, _ := ms.Refs(media.OwnerDocument, d.ID); len(refs) != 1 {
		t.Fatalf("前置条件不成立，引用 = %v", refs)
	}

	dm.Remove(d.ID)

	if refs, _ := ms.Refs(media.OwnerDocument, d.ID); len(refs) != 0 {
		t.Errorf("删除文档后仍有 %v 条引用 —— GC 永远收不掉这份媒体", refs)
	}
}

// mediaStore 为 nil 时 Insert/Query/Remove 必须与本特性上线前完全一致。
func TestDocMemory_NilMediaStoreDegrades(t *testing.T) {
	_, ds, _, _ := newTestStores(t)
	dm := NewDocMemory(ds)

	d := &Doc{Title: "无媒体存储", Content: "正文照常写入"}
	if err := dm.InsertWithMedia(d, []MediaAttachment{{
		MIME: "image/png", Data: []byte("ignored"),
	}}); err != nil {
		t.Fatalf("无媒体存储时写入不该失败: %v", err)
	}
	if d.ID == "" {
		t.Error("ID 仍应回填")
	}
	got := dm.Query("无媒体存储 正文照常写入", 3)
	if len(got) == 0 {
		t.Fatal("检索不到文档")
	}
	if len(got[0].Attachments) != 0 {
		t.Errorf("无媒体存储时不该有附件: %+v", got[0].Attachments)
	}
	dm.Remove(d.ID) // 不该 panic
}

// ---------- 文本记忆 ----------

// 文本记忆是追加写 JSONL，没有稳定 owner_id 可挂引用，
// 媒体只能以标记形式留在正文里；读回时要能反解成结构化附件。
func TestTextMemory_AttachmentRoundTrip(t *testing.T) {
	_, _, tm, ms := newTestStores(t)
	m := NewTextMemoryWithMedia("tester", tm, ms)

	if err := m.Append(TextEvent{
		Role:    "user",
		Content: "看这张图",
		Attachments: []MediaAttachment{{
			MIME: "image/png", Data: []byte("text-mem-bytes"), Description: "文本记忆里的图",
		}},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := m.RecentEvents(5)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("读不到刚追加的事件")
	}
	last := got[len(got)-1]
	if !strings.Contains(last.Content, "文本记忆里的图") {
		t.Errorf("正文里没有媒体标记: %q", last.Content)
	}
	if len(last.Attachments) != 1 {
		t.Fatalf("Attachments = %+v，期望 1 条（标记应能反解）", last.Attachments)
	}
	if last.Attachments[0].Description != "文本记忆里的图" {
		t.Errorf("附件描述 = %q", last.Attachments[0].Description)
	}
}
