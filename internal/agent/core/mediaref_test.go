package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/document"
	"github.com/JianFeeeee/HomeAgent/internal/memory/media"
)

// 媒体记忆接线测试：验证媒体从对话进入 CAS、挂到 L0 事件、
// 随归档转到 L2 文档的完整链路。
//
// 核心断言不是"函数被调用了"，而是不变量：
//   1. 媒体存不进去时对话照常（它是记忆增强，不是对话必需品）
//   2. 引用转移期间内容始终可读（先挂后销，不留归零窗口）
//   3. mediaStore 为 nil 时全链路静默跳过，行为与本特性上线前一致

func newTestAgentWithMedia(t *testing.T) (*Agent, *media.Store) {
	t.Helper()
	dir := t.TempDir()
	ms, err := media.New(filepath.Join(dir, "media"))
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	emb := memory.NewStaticEmbedder()
	a := &Agent{
		mediaStore: ms,
		context:    NewRelevanceContext(filepath.Join(dir, "context.json"), emb),
	}
	return a, ms
}

// imageBlockURL 造一个带指定 URL 的图片块。
// 名字带 URL 后缀是为了不与 modalfallback_test.go 里固定用 testPNG 的
// imageBlock() 撞名——两者用途不同：那个验回退链，这个验入库。
func imageBlockURL(dataURL string) agentAPI.ContentBlock {
	return agentAPI.ContentBlock{
		Type:     "image_url",
		ImageURL: &agentAPI.ImageURL{URL: dataURL, Detail: "auto"},
	}
}

func TestCaptureBlockMedia_StoresDataURL(t *testing.T) {
	a, ms := newTestAgentWithMedia(t)

	raw := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	blocks := []agentAPI.ContentBlock{
		{Type: "text", Text: "看这张图"},
		imageBlockURL(media.DataURL("image/png", raw)),
	}

	digests := a.captureBlockMedia(blocks, "multimodal_see_picture")
	if len(digests) != 1 {
		t.Fatalf("应捕获 1 个媒体，实际 %d", len(digests))
	}

	got, err := ms.Get(digests[0])
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatal("内容不一致")
	}
	it, _ := ms.Stat(digests[0])
	if it.MIME != "image/png" || it.Tool != "multimodal_see_picture" || it.Kind != media.KindImage {
		t.Fatalf("元数据不对: %+v", it)
	}
}

func TestCaptureBlockMedia_SkipsHTTPURL(t *testing.T) {
	// http(s) URL 拿不到字节就无法内容寻址；"下载它再存"会把一次对话
	// 变成一次网络请求（超时、鉴权、SSRF 全来了），不在本层解决。
	a, _ := newTestAgentWithMedia(t)

	blocks := []agentAPI.ContentBlock{
		imageBlockURL("https://example.com/x.png"),
	}
	if d := a.captureBlockMedia(blocks, "t"); len(d) != 0 {
		t.Fatalf("http URL 不该被捕获，实际 %d 个", len(d))
	}
}

func TestCaptureBlockMedia_NilStoreIsNoop(t *testing.T) {
	// mediaStore 未启用时全链路静默跳过，不能 panic 也不能报错——
	// 行为必须与本特性上线前完全一致。
	a := &Agent{}
	blocks := []agentAPI.ContentBlock{imageBlockURL(media.DataURL("image/png", []byte("x")))}
	if d := a.captureBlockMedia(blocks, "t"); d != nil {
		t.Fatalf("nil store 应返回 nil，实际 %v", d)
	}
	a.stageMediaDigests("deadbeef")
	if got := a.drainMediaDigests(); len(got) != 1 {
		t.Fatal("stage/drain 不依赖 store，应正常工作")
	}
	// bindEventMedia 对 nil store 也必须安全
	evt := &ContextEvent{}
	a.bindEventMedia(evt, []string{"deadbeef"})
	if len(evt.Blocks) != 0 || evt.ID != "" {
		t.Fatalf("nil store 时不该改动事件: %+v", evt)
	}
	if s := mediaLabel(nil); s != "" {
		t.Fatalf("nil 媒体应产出空标签，得到 %q", s)
	}
}

func TestCaptureBlockMedia_AudioAndVideo(t *testing.T) {
	a, ms := newTestAgentWithMedia(t)

	blocks := []agentAPI.ContentBlock{
		imageBlockURL(media.DataURL("image/jpeg", []byte("frame"))),
		{Type: "audio_url", AudioURL: &agentAPI.AudioURL{URL: media.DataURL("audio/wav", []byte("sound"))}},
	}
	digests := a.captureBlockMedia(blocks, "multimodal_see_video")
	if len(digests) != 2 {
		t.Fatalf("应捕获 2 个，实际 %d", len(digests))
	}

	kinds := map[media.Kind]int{}
	for _, d := range digests {
		it, err := ms.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		kinds[it.Kind]++
	}
	if kinds[media.KindImage] != 1 || kinds[media.KindAudio] != 1 {
		t.Fatalf("大类归属不对: %v", kinds)
	}
}

func TestStageDrainMediaDigests(t *testing.T) {
	a, _ := newTestAgentWithMedia(t)

	a.stageMediaDigests("a", "b")
	a.stageMediaDigests("c")
	got := a.drainMediaDigests()
	if len(got) != 3 {
		t.Fatalf("应累积 3 个，实际 %d", len(got))
	}
	// drain 后必须清空——否则下一轮对话会把上一轮的媒体又挂一遍
	if again := a.drainMediaDigests(); again != nil {
		t.Fatalf("drain 后应为空，实际 %v", again)
	}
}

func TestBindEventMedia_CreatesBlocks(t *testing.T) {
	a, ms := newTestAgentWithMedia(t)

	d, err := ms.Put([]byte("img"), media.Item{MIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}

	evt := &ContextEvent{Timestamp: time.Now(), Source: "qq", Input: "看图"}
	a.bindEventMedia(evt, []string{d})

	if evt.ID == "" {
		t.Fatal("应懒生成事件 ID")
	}
	if len(evt.Blocks) != 1 || evt.Blocks[0].PayloadDigest != d {
		t.Fatalf("事件应持有一等记忆块: %+v", evt.Blocks)
	}
	if evt.Blocks[0].Modality != memory.BlockImage || evt.Blocks[0].MIME != "image/png" {
		t.Fatalf("块元数据不对: %+v", evt.Blocks[0])
	}
}

func TestBindEventMedia_LazyIDOnlyWhenNeeded(t *testing.T) {
	// 绝大多数对话没有媒体，不该为它们都生成 ID 塞进 context.json
	a, _ := newTestAgentWithMedia(t)
	evt := &ContextEvent{Input: "纯文本"}
	a.bindEventMedia(evt, nil)
	if evt.ID != "" {
		t.Fatalf("无媒体时不该生成 ID，得到 %q", evt.ID)
	}
}

func TestMediaLabel_NoGeneratedDescription(t *testing.T) {
	// 标签只用来告诉模型「这条记忆带着哪份媒体、可用该 digest 取回字节」。
	// 它不包含任何生成的描述：描述式索引是把就机制，已彻底废弃。
	a, ms := newTestAgentWithMedia(t)

	d, _ := ms.Put([]byte("img"), media.Item{MIME: "image/png"})
	it, err := ms.Stat(d)
	if err != nil {
		t.Fatal(err)
	}
	s := mediaLabel(it)
	if s == "" {
		t.Fatal("应产出标签")
	}
	if !strings.Contains(s, "image/png") {
		t.Fatalf("标签应含 MIME 标注: %q", s)
	}
	if !strings.Contains(s, shortDigest(d)) {
		t.Fatalf("标签应含短 digest 供反查: %q", s)
	}
	_ = a
}

func TestPrune_NilMediaStoreStillArchives(t *testing.T) {
	// 媒体存储未启用时归档链路必须照常工作
	dir := t.TempDir()
	emb := memory.NewStaticEmbedder()
	docStore := document.NewStore(filepath.Join(dir, "docs"), memory.TokenizeWords)
	if err := docStore.Start(); err != nil {
		t.Fatal(err)
	}
	rc := NewRelevanceContext(filepath.Join(dir, "context.json"), emb)

	for i := 0; i < 15; i++ {
		rc.Append(ContextEvent{
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Source:    "qq",
			Input:     "内容",
		})
	}
	if n := rc.Prune("查询", 5, docStore); n == 0 {
		t.Fatal("无媒体存储时归档也应正常")
	}
}

func TestContextEvent_BlocksFieldRoundTrip(t *testing.T) {
	// context.json 加字段必须向后兼容：存量文件读回来 Blocks 为空、ID 为空，
	// 不影响任何既有行为。
	dir := t.TempDir()
	path := filepath.Join(dir, "context.json")

	// 写一份"存量格式"（无 id / blocks 字段）
	legacy := `[{"timestamp":"2026-09-04T10:00:00Z","source":"qq","input":"老数据","response":"回复"}]`
	if err := os.WriteFile(path, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	emb := memory.NewStaticEmbedder()
	rc := NewRelevanceContext(path, emb)
	if rc.Len() != 1 {
		t.Fatalf("应读回 1 条，实际 %d", rc.Len())
	}

	// 新写入带记忆块的事件，再读回
	ms, err := media.New(filepath.Join(dir, "media"))
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()
	a := &Agent{mediaStore: ms, context: rc}
	d, _ := ms.Put([]byte("img"), media.Item{MIME: "image/png"})
	evt := ContextEvent{Timestamp: time.Now(), Source: "qq", Input: "新数据"}
	a.bindEventMedia(&evt, []string{d})
	rc.Append(evt)
	rc.flush()

	rc2 := NewRelevanceContext(path, emb)
	if rc2.Len() != 2 {
		t.Fatalf("应有 2 条，实际 %d", rc2.Len())
	}
	var persisted int
	for _, e := range rc2.Recent(10) {
		persisted += len(e.Blocks)
	}
	if persisted != 1 {
		t.Fatalf("块应随 context.json 持久化，实际 %d 个", persisted)
	}
}

func TestPruneMigratesBlocksToDocument(t *testing.T) {
	// 一等记忆块的 L0→L2 迁移：块随事件离开 Context、进入 Document，
	// 身份（ID/模态/digest/向量）原样保留；同一块不能同时留在两层。
	// 这条路径不依赖 media_refs/ref_count。
	dir := t.TempDir()
	emb := memory.NewStaticEmbedder()
	docStore := document.NewStore(filepath.Join(dir, "docs"), memory.TokenizeWords)
	if err := docStore.Start(); err != nil {
		t.Fatal(err)
	}
	rc := NewRelevanceContext(filepath.Join(dir, "context.json"), emb)

	block := memory.MemoryBlock{
		ID: "blk_migrate_1", Modality: memory.BlockImage,
		PayloadDigest: "deadbeef", MIME: "image/png", Size: 42,
		Vector: []float64{0.1, 0.2, 0.3}, Fingerprint: "qwen:test",
	}
	rc.Append(ContextEvent{
		Timestamp: time.Now().Add(-time.Hour),
		Source:    "qq", Input: "很久以前的一张图",
		Blocks: []memory.MemoryBlock{block},
	})
	for i := 0; i < 12; i++ {
		rc.Append(ContextEvent{
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Source:    "qq", Input: "无关内容",
		})
	}

	if n := rc.Prune("完全不相关的查询", 5, docStore); n == 0 {
		t.Fatal("应有事件被归档")
	}

	// 块应已到达 L2，且身份不变。
	var found *document.Doc
	for _, d := range docStore.RecentDocs(20) {
		if len(d.Blocks) > 0 {
			found = d
			break
		}
	}
	if found == nil {
		t.Fatal("归档文档应持有一等记忆块")
	}
	if len(found.Blocks) != 1 {
		t.Fatalf("文档应有 1 个块，实际 %d", len(found.Blocks))
	}
	got := found.Blocks[0]
	if got.ID != block.ID || got.Modality != block.Modality || got.PayloadDigest != block.PayloadDigest || got.Fingerprint != block.Fingerprint || len(got.Vector) != len(block.Vector) {
		t.Fatalf("块身份应原样迁移:\n got  %+v\n want %+v", got, block)
	}

	// 同一块不能同时留在 L0。
	for _, e := range rc.Recent(100) {
		if len(e.Blocks) > 0 {
			t.Fatalf("块仍留在 L0（同一块同时存在于两层）: %+v", e.Blocks)
		}
	}
}
