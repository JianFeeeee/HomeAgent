package proc

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// 锁仲裁回归内核（§3.7 已裁定）的行为验证，含实验 9 的崩溃自愈机制。

func TestStageLock_MutualExclusion(t *testing.T) {
	l := newStageLock()

	if err := l.Acquire("A"); err != nil {
		t.Fatalf("A 应能获得锁: %v", err)
	}
	if l.Owner() != "A" {
		t.Errorf("Owner 应为 A，实际 %q", l.Owner())
	}

	// B 在 A 持锁期间不得进入
	entered := make(chan struct{})
	go func() {
		_ = l.Acquire("B")
		close(entered)
	}()
	select {
	case <-entered:
		t.Fatal("A 持锁期间 B 不应获得锁")
	case <-time.After(50 * time.Millisecond):
	}

	if err := l.Release("A"); err != nil {
		t.Fatalf("A 释放失败: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("A 释放后 B 应获得锁")
	}
	if l.Owner() != "B" {
		t.Errorf("Owner 应为 B，实际 %q", l.Owner())
	}
	_ = l.Release("B")
}

// 非持锁者不得释放他人的锁（防止串扰导致并发正确性被破坏）。
func TestStageLock_ReleaseByNonOwnerRejected(t *testing.T) {
	l := newStageLock()
	if err := l.Acquire("A"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release("A")

	err := l.Release("B")
	if err == nil {
		t.Fatal("非持锁者释放应被拒绝")
	}
	if !strings.Contains(err.Error(), "试图释放") {
		t.Errorf("错误信息应说明串扰，实际: %v", err)
	}
	if l.Owner() != "A" {
		t.Errorf("A 应仍持锁，实际 owner=%q", l.Owner())
	}
}

func TestStageLock_ReleaseWithoutHoldRejected(t *testing.T) {
	l := newStageLock()
	if err := l.Release("A"); err == nil {
		t.Fatal("未持锁时释放应报错")
	}
}

// 同插件重入：**直接通过**，不报错也不死锁（2026-10-07 改判据）。
//
// ## 这条判据原来要求什么，以及为什么改了
//
// 原断言：「同一插件重复加锁应被拒绝（否则死锁 30s）」。
// 它在 2026-09 的实验 9 里是对的 —— 那时把 Acquire 做成等待，
// 而同进程重入就是自己等自己。
//
// 生产实测（本机 homed 10-07，93 条）证明「拒绝」本身是错的：
//
//	[stage] before_toolcall handler error: proc: qq.stage.invoke:
//	  申请 stage 锁: proc: 插件 qq 重复申请 stage 锁（handler 内不应嵌套加锁）
//
// qq 的 before_toolcall 是它的**权限门**；每次工具调用都跑。
// 被拒绝 ⇒ handler 永远跑不到 ⇒ 权限门失效。
//
// 也不能改成「等待」：我先实现了那版，跑判据直接挂 30s
// （同进程重入 = 自己等自己）。⇒ 唯一正解是**可重入**。
//
// 断言仍要守住的那件事：**别的插件必须仍在门外互斥**
// （见 TestStageLock_DifferentPluginsStillMutuallyExclusive）。
func TestStageLock_ReentrantAcquirePassesThrough(t *testing.T) {
	l := newStageLock()
	if err := l.Acquire("A"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release("A")

	done := make(chan error, 1)
	go func() { done <- l.Acquire("A") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("★ 同插件重入不应报错（会让第二个 handler 永不执行）: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("★ 同插件重入死锁了（自己等自己）—— 比报错更糟，会把整个 stage 卡住")
	}
	if l.Owner() != "A" {
		t.Errorf("重入后 Owner 应仍为 A，实际 %q", l.Owner())
	}
}

// 实验 9 的核心：持锁进程崩溃后内核代为释放，后续插件不死锁。
// 这条彻底排除了 robust pthread_mutex 的必要性 —— 整个架构零 cgo。
func TestStageLock_ForceReleaseOnPluginCrash(t *testing.T) {
	l := newStageLock()

	// 插件 X 拿锁后"崩溃"（不调用 Release）
	if err := l.Acquire("X"); err != nil {
		t.Fatalf("X Acquire: %v", err)
	}
	if !l.ForceRelease("X") {
		t.Fatal("内核应能强制释放崩溃插件持有的锁")
	}
	if l.Owner() != "" {
		t.Errorf("强制释放后应无持有者，实际 %q", l.Owner())
	}

	// 插件 Y 随后必须能正常拿到锁（无死锁）
	done := make(chan error, 1)
	go func() { done <- l.Acquire("Y") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Y 应能获得锁: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("X 崩溃后 Y 无法获得锁 —— 出现死锁")
	}
	if err := l.Release("Y"); err != nil {
		t.Fatalf("Y 释放失败: %v", err)
	}
}

// ForceRelease 对非持有者/未持锁应为 no-op，不能误放他人的锁。
func TestStageLock_ForceReleaseIsTargeted(t *testing.T) {
	l := newStageLock()
	if err := l.Acquire("A"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release("A")

	if l.ForceRelease("B") {
		t.Error("强制释放不该动 A 持有的锁")
	}
	if l.Owner() != "A" {
		t.Errorf("A 应仍持锁，实际 %q", l.Owner())
	}
}

// 高并发下锁的串行化保证：临界区不重叠。
func TestStageLock_SerializesCriticalSection(t *testing.T) {
	l := newStageLock()
	var (
		mu      sync.Mutex
		inside  int
		maxSeen int
	)
	const workers = 8
	const iters = 50

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			name := string(rune('A' + id))
			for j := 0; j < iters; j++ {
				if err := l.Acquire(name); err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				mu.Lock()
				inside++
				if inside > maxSeen {
					maxSeen = inside
				}
				mu.Unlock()

				mu.Lock()
				inside--
				mu.Unlock()
				if err := l.Release(name); err != nil {
					t.Errorf("Release: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	if maxSeen > 1 {
		t.Fatalf("临界区出现并发：同时 %d 个持有者", maxSeen)
	}
}
