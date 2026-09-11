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
//
// 媒体没有独立生命周期管理：blob 是记忆块的内容，块被删除时内容随之删除。
func TestSoak_SustainedMixedLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("long soak test; run with -run TestSoak")
	}
	dur := 60 * time.Second
	s := newTestStore(t)

	// 常驻受保护区：全程被记忆块持有，模拟 Graph L3 中的块
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
		keep[i] = dg
		keepData[i] = d
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var puts, gets, deletes, describes, searches atomic.Int64
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

	// 写入者 ×3：持续写入一次性内容（无人持有）
	for w := 0; w < 3; w++ {
		wid := w
		worker(fmt.Sprintf("put-%d", wid), func(i int) error {
			b := make([]byte, 2048)
			rand.Read(b)
			b = append([]byte(fmt.Sprintf("eph-%d-%d-", wid, i)), b...)
			if _, err := s.Put(b, Item{MIME: "image/png", Tool: "cmd_run"}); err != nil {
				return err
			}
			puts.Add(1)
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

	// 删除者：持续删除一次性内容（模拟块创建后又被遗忘）
	worker("delete", func(i int) error {
		b := make([]byte, 2048)
		rand.Read(b)
		b = append([]byte(fmt.Sprintf("del-%d-", i)), b...)
		d, err := s.Put(b, Item{MIME: "image/png", Tool: "cmd_run"})
		if err != nil {
			return err
		}
		if err := s.Delete(d); err != nil {
			return err
		}
		deletes.Add(1)
		time.Sleep(time.Millisecond)
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

	t.Logf("%v 内: put=%d get=%d delete=%d describe=%d search=%d",
		dur, puts.Load(), gets.Load(), deletes.Load(), describes.Load(), searches.Load())

	// 收尾断言
	for i, d := range keep {
		got, err := s.Get(d)
		if err != nil {
			t.Fatalf("受保护项丢失 %s: %v", shortDigest(d), err)
		}
		if !bytes.Equal(got, keepData[i]) {
			t.Fatalf("受保护项内容变了 %s", shortDigest(d))
		}
	}

	st := s.Stats()
	t.Logf("收尾: 条目=%v 字节=%v 已描述=%v",
		st["count"], st["total_bytes"], st["described"])
}
