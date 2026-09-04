package core

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
)

// 媒体后台循环测试。
//
// 两条循环都要能在「未启用」时干净退出——它们随 Agent.Start() 无条件启动，
// 若不早退就会在每个没配媒体存储的部署上空转一个 goroutine。

func newMediaLoopAgent(t *testing.T, gcInterval, minAge time.Duration, describe bool) (*Agent, *media.Store) {
	t.Helper()
	dir := t.TempDir()
	ms, err := media.New(filepath.Join(dir, "media"), 0)
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	a := &Agent{
		mediaStore:      ms,
		mediaGCInterval: gcInterval,
		mediaGCMinAge:   minAge,
		mediaDescribe:   describe,
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	t.Cleanup(a.cancel)
	return a, ms
}

func TestMediaGCLoop_ExitsWhenDisabled(t *testing.T) {
	// 两种禁用形态都必须立刻返回，不留空转 goroutine：
	//   1. mediaStore 为 nil（媒体记忆整体关闭）
	//   2. gcInterval 为 0（显式不自动清理）
	cases := []struct {
		name  string
		agent *Agent
	}{
		{"nil store", func() *Agent {
			a := &Agent{mediaGCInterval: time.Hour}
			a.ctx, a.cancel = context.WithCancel(context.Background())
			return a
		}()},
		{"zero interval", func() *Agent {
			dir := t.TempDir()
			ms, _ := media.New(filepath.Join(dir, "m"), 0)
			t.Cleanup(func() { ms.Close() })
			a := &Agent{mediaStore: ms, mediaGCInterval: 0}
			a.ctx, a.cancel = context.WithCancel(context.Background())
			return a
		}()},
	}

	for _, c := range cases {
		done := make(chan struct{})
		go func(a *Agent) { a.mediaGCLoop(); close(done) }(c.agent)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: mediaGCLoop 未立即返回（会空转 goroutine）", c.name)
		}
		c.agent.cancel()
	}
}

func TestMediaGCLoop_ClearsOrphansKeepsReferenced(t *testing.T) {
	a, ms := newMediaLoopAgent(t, 50*time.Millisecond, 0, false)

	kept, _ := ms.Put([]byte("referenced"), media.Item{MIME: "image/png"})
	if err := ms.AddRef(kept, media.OwnerContext, "evt-1"); err != nil {
		t.Fatal(err)
	}
	orphan, _ := ms.Put([]byte("orphaned"), media.Item{MIME: "image/png"})

	go a.mediaGCLoop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := ms.Stat(orphan); err != nil {
			break // 孤儿已被清
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.cancel()

	if _, err := ms.Stat(orphan); err == nil {
		t.Fatal("无引用项应被 GC 清理")
	}
	// 关键不变量：有引用的内容永不被删，否则记忆里的 digest 成悬空指针
	if _, err := ms.Get(kept); err != nil {
		t.Fatalf("被引用的内容不该被清: %v", err)
	}
}

func TestMediaGCLoop_MinAgeProtectsFresh(t *testing.T) {
	// minAge 保护刚 Put 还没来得及 AddRef 的项——它们 refcount 也是 0
	a, ms := newMediaLoopAgent(t, 30*time.Millisecond, time.Hour, false)

	d, _ := ms.Put([]byte("just-arrived"), media.Item{MIME: "image/png"})

	go a.mediaGCLoop()
	time.Sleep(400 * time.Millisecond) // 足够跑十几轮 GC
	a.cancel()

	if _, err := ms.Get(d); err != nil {
		t.Fatalf("minAge 内的新项不该被清: %v", err)
	}
}

func TestMediaDescribeLoop_ExitsWhenDisabled(t *testing.T) {
	// describe 关闭时必须立即返回（默认就是关闭，绝大多数部署走这条路）
	a, _ := newMediaLoopAgent(t, 0, 0, false)
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
	a, ms := newMediaLoopAgent(t, 0, 0, true)
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
	a, ms := newMediaLoopAgent(t, 0, 0, true)

	other, _ := ms.Put([]byte("blob"), media.Item{MIME: "application/octet-stream"})
	a.describePendingMedia()

	it, err := ms.Stat(other)
	if err != nil {
		t.Fatal(err)
	}
	if it.DescribedBy != "unsupported" {
		t.Fatalf("不可描述的大类应被标记，实际 DescribedBy=%q", it.DescribedBy)
	}
	// 标记后必须退出待描述队列，否则每轮都被取出来重试、永久占着
	// LIMIT 的名额，真正需要描述的新项永远轮不到。
	pending, _ := ms.Pending(10)
	if len(pending) != 0 {
		t.Fatalf("标记 unsupported 后应退出待描述队列，仍有 %d 条", len(pending))
	}
}

func TestDescribePendingMedia_EmptyQueueIsNoop(t *testing.T) {
	a, _ := newMediaLoopAgent(t, 0, 0, true)
	a.describePendingMedia() // 不该 panic
}
