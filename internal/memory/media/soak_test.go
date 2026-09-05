package media

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSoak_SustainedMixedLoad 长稳测试：持续混合负载下不变量不破。
// 用 -run TestSoak -timeout 300s 单独跑，默认 short 模式跳过。
func TestSoak_SustainedMixedLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("long soak test; run with -run TestSoak")
	}
	dur := 60 * time.Second
	s := newTestStore(t, 8*1024*1024) // 8MB 上限，逼 GC 频繁工作

	// 常驻受保护集
	const keepN = 20
	keep := make([]string, keepN)
	keepData := make([][]byte, keepN)
	for i := range keep {
		d := make([]byte, 4096)
		rand.Read(d)
		d = append([]byte(fmt.Sprintf("keep-%d-", i)), d...)
		dg, err := s.Put(d, Item{MIME: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddRef(dg, "graph_sentence", fmt.Sprintf("s-%d", i)); err != nil {
			t.Fatal(err)
		}
		keep[i] = dg
		keepData[i] = d
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var puts, gets, gcs, describes, searches, refOps atomic.Int64
	var fatal atomic.Int64

	worker := func(name string, fn func(iter int) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := fn(i); err != nil {
					fatal.Add(1)
					t.Errorf("%s 第 %d 次失败: %v", name, i, err)
					return
				}
			}
		}()
	}

	// 写入者 ×3
	for w := 0; w < 3; w++ {
		wid := w
		worker(fmt.Sprintf("put-%d", wid), func(i int) error {
			b := make([]byte, 2048)
			rand.Read(b)
			b = append([]byte(fmt.Sprintf("eph-%d-%d-", wid, i)), b...)
			d, err := s.Put(b, Item{MIME: "image/png", Tool: "cmd_run"})
			if err != nil {
				return err
			}
			puts.Add(1)
			// 三分之一挂上引用再立刻注销，模拟短命引用
			if i%3 == 0 {
				own := fmt.Sprintf("tmp-%d-%d", wid, i)
				if err := s.AddRef(d, "context", own); err != nil {
					return err
				}
				if err := s.DropRef(d, "context", own); err != nil {
					return err
				}
				refOps.Add(2)
			}
			return nil
		})
	}

	// 读取者 ×3：受保护集必须始终完好
	for r := 0; r < 3; r++ {
		worker("get", func(i int) error {
			idx := i % keepN
			got, err := s.Get(keep[idx])
			if err != nil {
				return err
			}
			if !bytes.Equal(got, keepData[idx]) {
				return fmt.Errorf("内容被改 %s", shortDigest(keep[idx]))
			}
			gets.Add(1)
			return nil
		})
	}

	// GC 者
	worker("gc", func(i int) error {
		if _, _, err := s.GC(0); err != nil {
			return err
		}
		gcs.Add(1)
		time.Sleep(5 * time.Millisecond)
		return nil
	})

	// 描述者
	worker("describe", func(i int) error {
		pend, err := s.Pending(5)
		if err != nil {
			return err
		}
		for _, it := range pend {
			// 忽略 unknown digest：GC 可能在 Pending 与 Describe 之间清掉它，
			// 这是正常竞态而非缺陷。
			_ = s.Describe(it.Digest, fmt.Sprintf("描述 %d 含图表与文字", i), "vis")
			describes.Add(1)
		}
		time.Sleep(2 * time.Millisecond)
		return nil
	})

	// 检索者
	worker("search", func(i int) error {
		if _, err := s.Search("图表", KindImage, 20); err != nil {
			return err
		}
		if _, err := s.Stat(keep[i%keepN]); err != nil {
			return err
		}
		searches.Add(1)
		time.Sleep(2 * time.Millisecond)
		return nil
	})

	time.Sleep(dur)
	close(stop)
	wg.Wait()

	if n := fatal.Load(); n > 0 {
		t.Fatalf("%d 个 worker 报致命错误", n)
	}

	t.Logf("%v 内: put=%d get=%d gc=%d describe=%d search=%d refOps=%d",
		dur, puts.Load(), gets.Load(), gcs.Load(), describes.Load(), searches.Load(), refOps.Load())

	// 收尾断言
	for i, d := range keep {
		got, err := s.Get(d)
		if err != nil {
			t.Fatalf("受保护项丢失 %s: %v", shortDigest(d), err)
		}
		if !bytes.Equal(got, keepData[i]) {
			t.Fatalf("受保护项内容变了 %s", shortDigest(d))
		}
		it, err := s.Stat(d)
		if err != nil || it.RefCount != 1 {
			t.Fatalf("受保护项引用计数应为 1: %+v", it)
		}
	}
	checkRefIntegrity(t, s)

	st := s.Stats()
	t.Logf("收尾: 条目=%v 字节=%v 未引用=%v 已描述=%v",
		st["count"], st["total_bytes"], st["unreferenced"], st["described"])
	if total := st["total_bytes"].(int64); total > 8*1024*1024*3 {
		t.Fatalf("容量失控: %d 远超上限", total)
	}
}
