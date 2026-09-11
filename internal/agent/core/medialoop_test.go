package core

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
)

// 媒体后台循环测试。
//
// 媒体没有独立生命周期管理（没有 GC、没有引用计数）：blob 是记忆块的内容，
// 块的创建/迁移/删除由记忆系统决定。这里只测描述循环与删除语义。

func newMediaLoopAgent(t *testing.T, describe bool) (*Agent, *media.Store) {
	t.Helper()
	dir := t.TempDir()
	ms, err := media.New(filepath.Join(dir, "media"))
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	a := &Agent{
		mediaStore:    ms,
		mediaDescribe: describe,
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	t.Cleanup(a.cancel)
	return a, ms
}

func TestMediaDescribeLoop_ExitsWhenDisabled(t *testing.T) {
	// describe 关闭时必须立即返回（默认就是关闭，绝大多数部署走这条路）
	a, _ := newMediaLoopAgent(t, false)
	done := make(chan struct{})
	go func() { a.mediaDescribeLoop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("describe 关闭时 mediaDescribeLoop 未立即返回")
	}
}

func TestDescribePendingMedia_NoProviderLeavesUndescribed(t *testing.T) {
	// 没有声明视觉能力的源时整轮跳过，且**不能**把项标记成已处理——
	// 配置好之后必须还能被捡起来。
	a, ms := newMediaLoopAgent(t, true)
	d, _ := ms.Put([]byte("img"), media.Item{MIME: "image/png"})

	// providerManager 为 nil → resolveModalFallback 返回 nil
	a.describePendingMedia()

	it, err := ms.Stat(d)
	if err != nil {
		t.Fatal(err)
	}
	if it.Description != "" || it.DescribedBy != "" {
		t.Fatalf("无可用源时不该写描述: %+v", it)
	}
	pending, _ := ms.Pending(10)
	if len(pending) != 1 {
		t.Fatalf("项应仍在待描述队列里，实际 %d 条", len(pending))
	}
}

func TestDescribePendingMedia_MarksUnsupportedKind(t *testing.T) {
	// video/other 大类没有可用的描述通道，必须标记掉，
	// 否则每轮 Pending 都把它取出来重试，永远卡住队列头部。
	a, ms := newMediaLoopAgent(t, true)

	other, _ := ms.Put([]byte("blob"), media.Item{MIME: "application/octet-stream"})
	a.describePendingMedia()

	it, err := ms.Stat(other)
	if err != nil {
		t.Fatal(err)
	}
	if it.DescribedBy != "unsupported" {
		t.Fatalf("不可描述的大类应被标记，实际 DescribedBy=%q", it.DescribedBy)
	}
	pending, _ := ms.Pending(10)
	if len(pending) != 0 {
		t.Fatalf("标记 unsupported 后应退出待描述队列，仍有 %d 条", len(pending))
	}
}

func TestDescribePendingMedia_EmptyQueueIsNoop(t *testing.T) {
	a, _ := newMediaLoopAgent(t, true)
	a.describePendingMedia() // 不该 panic
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
