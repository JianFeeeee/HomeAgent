package core

import (
	"path/filepath"
	"strconv"
	"testing"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
)

// L3 图库媒体引用测试。
//
// 这一层的目的只有一个：几个月后从图谱走到一条句子，要能取回当时那份字节。
// 因此测试的重点是「反查链路是否完整」以及「引用是否会悬空或误删」。

func newGraphMediaAgent(t *testing.T) (*Agent, *memory.GraphDB, *media.Store) {
	t.Helper()
	dir := t.TempDir()

	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	ms, err := media.New(filepath.Join(dir, "media"), 0)
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

	if _, _, err := a.commitTriplesWithMedia(triples, "s1", 0); err != nil {
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

	// 反查：从句子取回 digest，再取回字节
	digests, err := a.RecallMediaForSentence(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(digests) != 1 || digests[0] != digest {
		t.Fatalf("反查应得完整 digest %s，实际 %v", shortDigest(digest), digests)
	}
	got, err := ms.Get(digests[0])
	if err != nil {
		t.Fatalf("取回内容失败: %v", err)
	}
	if string(got) != string(content) {
		t.Fatal("取回的内容与写入不一致")
	}

	// 引用计数非零 → GC 不会清它
	if _, _, err := ms.GC(0); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Get(digest); err != nil {
		t.Fatalf("被图库句子引用的内容不该被 GC 清掉: %v", err)
	}
}

func TestBindSentenceMedia_SkipsUnresolvable(t *testing.T) {
	// 文本里的 digest 在库里不存在时必须跳过，不能挂一条对不上的引用——
	// 那条引用 DropOwner 永远匹配不到，会永久占着计数。
	a, _, ms := newGraphMediaAgent(t)

	sentence := "[image/png deadbeefdead] 一张不存在的图"
	ids := map[string]int64{sentence: 42}
	a.bindSentenceMedia(ids)

	refs, err := ms.Refs(media.OwnerGraphSentence, "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("无法补全的 digest 不该挂引用，实际 %v", refs)
	}
}

func TestBindSentenceMedia_NilStoreNoop(t *testing.T) {
	a := &Agent{}
	a.bindSentenceMedia(map[string]int64{"[image aaaaaaaaaaaa] x": 1})
	if got, err := a.RecallMediaForSentence(1); err != nil || got != nil {
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
	ec, rc, err := a.commitTriplesWithMedia([]memory.Triple{
		{Subject: "张三", Relation: "喜欢", Object: "咖啡"},
	}, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ec != 2 || rc != 1 {
		t.Fatalf("期望 2 实体 1 关系，实际 ec=%d rc=%d", ec, rc)
	}
}

func TestReleaseDocMedia_DropsRefsSoGCCanReclaim(t *testing.T) {
	// L2→L3 那一跳留下的泄漏：文档被 Remove 但引用没销，
	// 引用计数永不归零，blob 永远不会被 GC 回收。
	a, _, ms := newGraphMediaAgent(t)

	digest, err := ms.Put([]byte("doc image"), media.Item{MIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.AddRef(digest, media.OwnerDocument, "doc_1"); err != nil {
		t.Fatal(err)
	}

	// 释放前 GC 清不掉
	if _, _, err := ms.GC(0); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Stat(digest); err != nil {
		t.Fatal("有文档引用时不该被清")
	}

	a.releaseDocMedia("doc_1")

	if refs, _ := ms.Refs(media.OwnerDocument, "doc_1"); len(refs) != 0 {
		t.Fatalf("释放后不该还有文档引用，实际 %v", refs)
	}
	// 现在 GC 能回收了
	removed, _, err := ms.GC(0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("释放引用后 GC 应能回收，实际清理 %d 条", removed)
	}
}

func TestMediaContextForSentences(t *testing.T) {
	a, _, ms := newGraphMediaAgent(t)

	digest, _ := ms.Put([]byte("img"), media.Item{MIME: "image/png"})
	if err := ms.Describe(digest, "一张紫蓝红三色带图", "visionllm"); err != nil {
		t.Fatal(err)
	}
	if err := ms.AddRef(digest, media.OwnerGraphSentence, "7"); err != nil {
		t.Fatal(err)
	}

	out := a.mediaContextForSentences([]int64{7, 8})
	if out == "" {
		t.Fatal("应产出媒体说明")
	}
	if !contains(out, "句子 #7") || !contains(out, "一张紫蓝红三色带图") {
		t.Fatalf("说明内容不对: %q", out)
	}
	// 8 号句子没引用媒体，不该出现
	if contains(out, "句子 #8") {
		t.Fatalf("无引用的句子不该出现: %q", out)
	}
}

func TestResolvePrefix(t *testing.T) {
	dir := t.TempDir()
	ms, err := media.New(filepath.Join(dir, "m"), 0)
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
	ms, err := media.New(filepath.Join(dir, "m"), 0)
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
