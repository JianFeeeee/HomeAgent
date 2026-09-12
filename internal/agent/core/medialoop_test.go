package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

// 媒体与记忆块的生命周期测试。
//
// 媒体没有独立生命周期管理（没有 GC、没有引用计数）：blob 是记忆块的内容，
// 块的创建/迁移/删除由记忆系统决定。图片也不靠文本描述索引。

func newMediaLoopAgent(t *testing.T) (*Agent, *media.Store) {
	t.Helper()
	dir := t.TempDir()
	ms, err := media.New(filepath.Join(dir, "media"))
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	a := &Agent{mediaStore: ms}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	t.Cleanup(a.cancel)
	return a, ms
}

// heldMediaDigests 汇总三层记忆持有的媒体：只有这些才可被召回。
func TestHeldMediaDigests_CollectsAcrossLayers(t *testing.T) {
	a, ms := newMediaLoopAgent(t)
	d1, _ := ms.Put([]byte("ctx-layer"), media.Item{MIME: "image/png"})
	d2, _ := ms.Put([]byte("doc-layer"), media.Item{MIME: "image/png"})
	d3, _ := ms.Put([]byte("graph-layer"), media.Item{MIME: "image/png"})
	d4, _ := ms.Put([]byte("orphan"), media.Item{MIME: "image/png"})

	a.context = NewRelevanceContext("", memory.NewStaticEmbedder(""))
	a.context.Append(ContextEvent{Input: "带图的一轮", Blocks: []memory.MemoryBlock{
		{ID: "blk_ctx", Modality: memory.BlockImage, PayloadDigest: d1},
	}})

	dir := t.TempDir()
	bo, ok := a.blockFromDigest(d2)
	if !ok {
		t.Fatal("blockFromDigest 失败")
	}
	ds := document.NewStore(filepath.Join(dir, "docs"), memory.TokenizeWords)
	if err := ds.Start(); err != nil {
		t.Fatal(err)
	}
	defer ds.Stop()
	if err := ds.Insert(&document.Doc{ID: "doc_1", Summary: "s", Blocks: []memory.MemoryBlock{bo}}); err != nil {
		t.Fatal(err)
	}
	a.docStore = ds

	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.PutMemoryBlocks([]memory.MemoryBlock{
		{ID: "blk_g", Modality: memory.BlockImage, PayloadDigest: d3},
	}); err != nil {
		t.Fatal(err)
	}
	a.memory = g

	held := a.heldMediaDigests()
	for _, want := range []string{d1, d2, d3} {
		if !held[want] {
			t.Errorf("层次持有 %s 却不在结果里: %v", shortDigest(want), held)
		}
	}
	if held[d4] {
		t.Errorf("无人持有的 %s 不该出现在结果里", shortDigest(d4))
	}
}

// fakeSpace 是一个只覆盖图像的假统一空间，用来验证「不在本空间」与
// 「本次失败」必须被区分对待。
type fakeSpace struct{}

func (fakeSpace) VectorizeDense(string) ([]float64, error) { return []float64{1, 0}, nil }

func (fakeSpace) EmbedImageDense(_ []byte, mime string) ([]float64, error) {
	if strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "video/") {
		return nil, fmt.Errorf("%w: %s", vector.ErrModalityUnsupported, mime)
	}
	return []float64{1, 0}, nil
}

func (fakeSpace) Fingerprint() string { return "fake-space" }
func (fakeSpace) Dim() int            { return 2 }
func (fakeSpace) Loaded() bool        { return true }
func (fakeSpace) Close()              {}

// TestReembedStaleMedia_SkipsUnsupportedWithoutFaking 验证向量迁移不会：
//   - 把音频当失败反复重试；
//   - 更不能拿另一个模型的向量顶替音频（那会污染统一空间且静默）。
func TestReembedStaleMedia_SkipsUnsupportedWithoutFaking(t *testing.T) {
	a, ms := newMediaLoopAgent(t)
	img, _ := ms.Put([]byte("img-bytes"), media.Item{MIME: "image/png"})
	aud, _ := ms.Put([]byte("aud-bytes"), media.Item{MIME: "audio/wav"})

	a.multimodalSpace = fakeSpace{}
	a.reembedStaleMedia()

	it, err := ms.Stat(img)
	if err != nil {
		t.Fatal(err)
	}
	if len(it.Vec) != 2 || it.VecModel != "fake-space" {
		t.Fatalf("图像应拿到本空间向量，实际 vec=%v model=%q", it.Vec, it.VecModel)
	}

	audIt, err := ms.Stat(aud)
	if err != nil {
		t.Fatal(err)
	}
	if len(audIt.Vec) != 0 || audIt.VecModel != "" {
		t.Fatalf("音频不得被写入任何向量（不能用别的模型顶替），实际 vec=%v model=%q",
			audIt.Vec, audIt.VecModel)
	}
}

// payloadHeld 是删除前的活查询。
func TestPayloadHeld(t *testing.T) {
	a, ms := newMediaLoopAgent(t)
	d, _ := ms.Put([]byte("held"), media.Item{MIME: "image/png"})
	if a.payloadHeld(d) {
		t.Fatal("尚无块持有时不该报已持有")
	}

	a.context = NewRelevanceContext("", memory.NewStaticEmbedder(""))
	a.context.Append(ContextEvent{Input: "x", Blocks: []memory.MemoryBlock{
		{ID: "blk_1", Modality: memory.BlockImage, PayloadDigest: d},
	}})
	if !a.payloadHeld(d) {
		t.Fatal("L0 持有却报未持有")
	}
	if a.payloadHeld("") {
		t.Fatal("空 digest 应为 false")
	}
}

// TestForgetPayloads_DeletesOnlyUnheldContent 验证删除语义：
// 块被删除后内容才被删；仍被其它记忆块共享的 digest 不会被误删。
func TestForgetPayloads_DeletesOnlyUnheldContent(t *testing.T) {
	dir := t.TempDir()
	ms, err := media.New(filepath.Join(dir, "media"))
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()

	d1, _ := ms.Put([]byte("held-by-graph"), media.Item{MIME: "image/png"})
	d2, _ := ms.Put([]byte("being-forgotten"), media.Item{MIME: "image/png"})

	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.PutMemoryBlocks([]memory.MemoryBlock{
		{ID: "blk_keep", Modality: memory.BlockImage, PayloadDigest: d1},
	}); err != nil {
		t.Fatal(err)
	}

	a := &Agent{mediaStore: ms, memory: g}
	a.forgetPayloads([]string{d1, d2})

	if _, err := ms.Stat(d1); err != nil {
		t.Fatalf("仍被 L3 块持有的内容不该被删: %v", err)
	}
	if _, err := ms.Stat(d2); err == nil {
		t.Fatal("无人持有的内容应被删除")
	}
}
