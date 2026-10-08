package memory

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 钩子在并发读写下的竞态与死锁判据。
//
// ★ 为何必须专测这个：钩子用**独立锁 hookMu**（用 g.mu 会自死锁，因为
// commit/Recall 已持该锁），而订阅者回调又可能反过来访问图库。
// 这是典型的两把锁交叉场景，-race 是唯一可靠的判据。
func TestAccessHookConcurrentNoRace(t *testing.T) {
	g, err := NewGraphDB(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	var mu sync.Mutex
	var commits, recalls int
	// counting 钩子：**所有**装上去的钩子都必须计数。
	//
	// ★ 为什么不能只在第一个钩子里计数：本测试有一个 goroutine 在并发
	//   替换钩子（验 SetAccessHook 与读写并发安全）。若替换进去的是 no-op，
	//   计数会被清零，断言「事件数 > 0」就永远失败、而断言「==0」又等于没验。
	//   ⇒ 让每个钩子都计数，才能既测替换安全、又保证计数有意义。
	counting := func(ev AccessEvent) {
		mu.Lock()
		switch ev.Op {
		case "commit":
			commits++
		case "recall":
			recalls++
		}
		mu.Unlock()
		if ev.Op == "recall" {
			_ = g.Path() // 只读、无锁调用：验证钩子内不持 g.mu
		}
	}
	g.SetAccessHook(counting)

	// ★ 先**播种**确定存在的数据，再开始并发读。
	//
	// 为什么必须播种：recall 的钩子在**没有匹配块**时不报事件（空事件不上报，
	// 否则星图收到空数组只会白重算一次）。若读者与写者同时起跑，
	// 读者很可能在写者落库前查询 ⇒ 全部空结果 ⇒ recall 事件为 0，
	// 断言「recall > 0」随机失败。实测正是如此（同一份代码，一次 recall=3、
	// 一次 recall=0）——那是 flaky 测试，不是产品缺陷。
	for i := 0; i < 20; i++ {
		if _, _, err := g.Commit([]Triple{{
			Subject:      fmt.Sprintf("seed_实体_%d", i),
			Relation:     "关联",
			Object:       fmt.Sprintf("seed_目标_%d", i),
			SentenceText: fmt.Sprintf("seed 句子 %d", i),
		}}, "seed", i); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	// 写者
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_, _, err := g.Commit([]Triple{{
					Subject:      fmt.Sprintf("w%d_实体_%d", id, i),
					Relation:     "关联",
					Object:       fmt.Sprintf("w%d_目标_%d", id, i),
					SentenceText: fmt.Sprintf("w%d 的句子 %d", id, i),
				}}, fmt.Sprintf("s%d", id), i)
				if err != nil {
					t.Errorf("commit: %v", err)
					return
				}
			}
		}(w)
	}
	// 读者
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				// 查播种数据：保证有匹配 ⇒ recall 事件必然上报，断言不再依赖时序。
				if _, err := g.Recall([]string{fmt.Sprintf("seed_实体_%d", i%20)}, nil, 2, ""); err != nil {
					t.Errorf("recall: %v", err)
					return
				}
			}
		}(r)
	}
	// 钩子的更换（SetAccessHook 与读写并发）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			g.SetAccessHook(counting)
			time.Sleep(2 * time.Millisecond)
		}
	}()

	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("60s 未完成 —— 钩子/读写路径疑似死锁")
	}
	mu.Lock()
	c, r := commits, recalls
	mu.Unlock()
	t.Logf("并发完成：commit 事件=%d recall 事件=%d", c, r)
	// ★ 必须断言事件真的上报过，否则「钩子从未触发」也会让本测试通过。
	//   这正是「判据失效」的典型：测了「不崩」，没测「真的工作」。
	if c == 0 || r == 0 {
		t.Fatalf("钩子未上报事件（commit=%d recall=%d）——说明埋点没触发或钩子被覆盖", c, r)
	}
}
