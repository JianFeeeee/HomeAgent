package media

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 压力测试与冒烟测试。
//
// 关注点不是吞吐数字，而是并发下的不变量是否被破坏：
//   1. GC 与读写并发时，被记忆块持有的内容绝不能被删
//   2. 同内容并发 Put 只落一份磁盘、digest 一致
//   3. SQLite 在多 goroutine 下不出现 "database is locked"
//
// 存活判定不再依赖 media_refs/ref_count：调用方把「三层记忆当前持有的
// digest 集合」传给 GC，本层只做 CAS。

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

// blobFileCount 统计 CAS 目录下的实际文件数（不含 .tmp）。
func blobFileCount(t *testing.T, s *Store) int {
	t.Helper()
	n := 0
	filepath.Walk(s.blobDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".tmp") {
			t.Errorf("残留临时文件: %s", path)
			return nil
		}
		n++
		return nil
	})
	return n
}

func TestStress_ConcurrentPutSameContent(t *testing.T) {
	// 同一内容被 N 个 goroutine 同时 Put：digest 必须一致，磁盘只一份。
	// 现实对应：see_video 抽出的相邻帧、用户连发同一张图。
	s := newTestStore(t, 0)
	data := randBytes(t, 64*1024)

	const workers = 32
	var wg sync.WaitGroup
	digests := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			d, err := s.Put(data, Item{MIME: "image/png", OriginPath: fmt.Sprintf("/tmp/%d.png", idx)})
			digests[idx] = d
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d Put 失败: %v", i, err)
		}
	}
	first := digests[0]
	for i, d := range digests {
		if d != first {
			t.Fatalf("worker %d digest 不一致: %s vs %s", i, d, first)
		}
	}
	if n := blobFileCount(t, s); n != 1 {
		t.Fatalf("同一内容应只落一份 blob，实际 %d 个文件", n)
	}
	if got, err := s.Get(first); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("内容应可完整读回: err=%v len=%d", err, len(got))
	}
}

func TestStress_ConcurrentPutDistinctContent(t *testing.T) {
	// 大量不同内容并发入库：不丢条目、不串内容。
	s := newTestStore(t, 0)
	const workers = 16
	const perWorker = 25

	var wg sync.WaitGroup
	var failed atomic.Int64
	type rec struct {
		digest string
		data   []byte
	}
	recCh := make(chan rec, workers*perWorker)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				data := []byte(fmt.Sprintf("w%d-i%d-", wid, i))
				data = append(data, randBytes(t, 512)...)
				d, err := s.Put(data, Item{MIME: "image/png"})
				if err != nil {
					failed.Add(1)
					continue
				}
				recCh <- rec{digest: d, data: data}
			}
		}(w)
	}
	wg.Wait()
	close(recCh)

	if n := failed.Load(); n > 0 {
		t.Fatalf("%d 次 Put 失败", n)
	}

	var records []rec
	for r := range recCh {
		records = append(records, r)
	}
	if len(records) != workers*perWorker {
		t.Fatalf("应有 %d 条记录，实际 %d", workers*perWorker, len(records))
	}

	// 逐条回读校验内容没串
	for _, r := range records {
		got, err := s.Get(r.digest)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", shortDigest(r.digest), err)
		}
		if !bytes.Equal(got, r.data) {
			t.Fatalf("内容串了: %s", shortDigest(r.digest))
		}
	}

	st := s.Stats()
	if st["count"].(int) != len(records) {
		t.Fatalf("库内条目应为 %d，实际 %v", len(records), st["count"])
	}
}

func TestStress_ConcurrentDeleteAndPut(t *testing.T) {
	// 删除与写入并发：核心断言是被保留的内容永远可读，
	// 删除只影响目标 digest，不误伤其他内容。
	s := newTestStore(t)

	const heldCount = 8
	held := make([]string, heldCount)
	for i := range held {
		d, err := s.Put([]byte(fmt.Sprintf("payload-%d", i)), Item{MIME: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		held[i] = d
	}

	const workers = 16
	const rounds = 30
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				d, err := s.Put([]byte(fmt.Sprintf("tmp-%d-%d", wid, r)), Item{MIME: "image/png"})
				if err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				if err := s.Delete(d); err != nil {
					t.Errorf("Delete: %v", err)
					return
				}
			}
		}(w)
	}
	// 并发读取被保留内容
	for rdr := 0; rdr < 4; rdr++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				for i, d := range held {
					if _, err := s.Get(d); err != nil {
						t.Errorf("内容 %d 被误删: %v", i, err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	for i, d := range held {
		if _, err := s.Get(d); err != nil {
			t.Fatalf("仍被保留的第 %d 项不可读: %v", i, err)
		}
	}
}

func TestStress_DeleteConcurrentWithReads(t *testing.T) {
	// 删除与读取并发。最重要的断言：被保留的内容在整个过程中始终可读。
	s := newTestStore(t)

	const protectedCount = 10
	protected := make([]string, protectedCount)
	protectedData := make([][]byte, protectedCount)
	for i := range protected {
		data := append([]byte(fmt.Sprintf("protected-%d-", i)), randBytes(t, 256)...)
		d, err := s.Put(data, Item{MIME: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		protected[i] = d
		protectedData[i] = data
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var readErr atomic.Int64
	var deleteCount atomic.Int64
	var putCount atomic.Int64

	// 写入者：持续 Put 一次性内容再删除（模拟块创建后又被遗忘）
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				data := append([]byte(fmt.Sprintf("ephemeral-%d-%d-", wid, i)), randBytes(t, 128)...)
				d, err := s.Put(data, Item{MIME: "image/png"})
				if err != nil {
					continue
				}
				putCount.Add(1)
				if err := s.Delete(d); err == nil {
					deleteCount.Add(1)
				}
				i++
			}
		}(w)
	}

	// 读取者：反复读受保护内容，任何一次失败都是致命的
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i, d := range protected {
					got, err := s.Get(d)
					if err != nil {
						readErr.Add(1)
						t.Errorf("受保护内容读失败 %s: %v", shortDigest(d), err)
						return
					}
					if !bytes.Equal(got, protectedData[i]) {
						readErr.Add(1)
						t.Errorf("受保护内容被改 %s", shortDigest(d))
						return
					}
				}
			}
		}()
	}

	time.Sleep(1500 * time.Millisecond)
	close(stop)
	wg.Wait()

	if n := readErr.Load(); n > 0 {
		t.Fatalf("受保护内容读取失败 %d 次", n)
	}
	t.Logf("并发窗口内: Put=%d Delete=%d", putCount.Load(), deleteCount.Load())

	// 收尾确认：受保护的一个都没少
	for i, d := range protected {
		got, err := s.Get(d)
		if err != nil || !bytes.Equal(got, protectedData[i]) {
			t.Fatalf("收尾检查失败 %s: %v", shortDigest(d), err)
		}
	}
}

func TestStress_DescribeConcurrentWithSearch(t *testing.T) {
	// 描述写入与检索并发。C 部分的后台描述任务会长期这样跑。
	s := newTestStore(t, 0)
	const n = 60
	digests := make([]string, n)
	for i := range digests {
		d, err := s.Put([]byte(fmt.Sprintf("img-%d", i)), Item{MIME: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		digests[i] = d
	}

	var wg sync.WaitGroup
	var descErr, searchErr atomic.Int64

	// 描述写入者
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := wid; i < n; i += 4 {
				desc := fmt.Sprintf("第 %d 张图，含蓝色图表与文字", i)
				if err := s.Describe(digests[i], desc, "vis-src"); err != nil {
					descErr.Add(1)
				}
			}
		}(w)
	}

	// 检索者 + Pending 消费者
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := s.Search("图表", KindImage, 20); err != nil {
					searchErr.Add(1)
				}
				if _, err := s.Pending(10); err != nil {
					searchErr.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if v := descErr.Load(); v > 0 {
		t.Fatalf("Describe 失败 %d 次", v)
	}
	if v := searchErr.Load(); v > 0 {
		t.Fatalf("Search/Pending 失败 %d 次", v)
	}

	// 全部应已描述完
	pending, err := s.Pending(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("应全部描述完，仍有 %d 条未描述", len(pending))
	}
	got, err := s.Search("图表", KindImage, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("应检索到 %d 条，实际 %d", n, len(got))
	}
}

func TestStress_DeleteUnderLoad(t *testing.T) {
	// 持续写入 + 删除下，被保留的项必须始终可读。
	s := newTestStore(t)

	const keepN = 3
	keepList := make([]string, keepN)
	for i := range keepList {
		data := append([]byte(fmt.Sprintf("keep-%d-", i)), randBytes(t, 32*1024)...)
		d, err := s.Put(data, Item{MIME: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		keepList[i] = d
	}

	for round := 0; round < 30; round++ {
		for i := 0; i < 3; i++ {
			data := append([]byte(fmt.Sprintf("tmp-%d-%d-", round, i)), randBytes(t, 16*1024)...)
			d, err := s.Put(data, Item{MIME: "image/png"})
			if err != nil {
				t.Fatalf("round %d Put: %v", round, err)
			}
			if err := s.Delete(d); err != nil {
				t.Fatalf("round %d Delete: %v", round, err)
			}
		}
	}

	// 被保留的项必须都在
	for _, d := range keepList {
		if _, err := s.Get(d); err != nil {
			t.Fatalf("被保留项被误删 %s: %v", shortDigest(d), err)
		}
	}
}

func TestStress_ReopenAfterHeavyChurn(t *testing.T) {
	// 大量写入 + 删除之后重开：元数据与磁盘不该出现互相不认的孤儿。
	dir := t.TempDir()
	s1, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	var kept []string
	for i := 0; i < 100; i++ {
		data := append([]byte(fmt.Sprintf("churn-%d-", i)), randBytes(t, 256)...)
		d, err := s1.Put(data, Item{MIME: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		if i%5 == 0 {
			kept = append(kept, d)
		} else if err := s1.Delete(d); err != nil {
			t.Fatal(err)
		}
	}
	beforeStats := s1.Stats()
	s1.Close()

	s2, err := New(dir)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	defer s2.Close()

	afterStats := s2.Stats()
	if beforeStats["count"] != afterStats["count"] {
		t.Fatalf("重开后条目数变了: %v → %v", beforeStats["count"], afterStats["count"])
	}

	// 每条元数据都应有对应磁盘文件（无「元数据在文件没了」的孤儿）
	rows, err := s2.db.Query(`SELECT digest FROM media`)
	if err != nil {
		t.Fatal(err)
	}
	var missing int
	for rows.Next() {
		var d string
		if rows.Scan(&d) != nil {
			continue
		}
		if _, err := os.Stat(s2.blobPath(d)); err != nil {
			missing++
			if missing <= 3 {
				t.Errorf("元数据存在但 blob 缺失: %s", shortDigest(d))
			}
		}
	}
	rows.Close()
	if missing > 0 {
		t.Fatalf("%d 条元数据没有对应文件", missing)
	}

	// 磁盘文件数应等于元数据条数（无「文件在元数据没了」的孤儿）
	if n := blobFileCount(t, s2); n != afterStats["count"].(int) {
		t.Fatalf("磁盘文件 %d 与元数据 %v 不一致", n, afterStats["count"])
	}

	for _, d := range kept {
		if _, err := s2.Get(d); err != nil {
			t.Fatalf("被持有项重开后读不到 %s: %v", shortDigest(d), err)
		}
	}
}

func TestStress_LargeBlob(t *testing.T) {
	// 单个大文件：see_video 10 帧 × 2MB 是现实上限附近。
	s := newTestStore(t, 0)
	data := randBytes(t, 4*1024*1024) // 4MB

	d, err := s.Put(data, Item{MIME: "image/jpeg", Width: 1920, Height: 1080})
	if err != nil {
		t.Fatalf("4MB Put 失败: %v", err)
	}
	got, err := s.Get(d)
	if err != nil {
		t.Fatalf("4MB Get 失败: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("4MB 内容回读不一致")
	}
	it, _ := s.Stat(d)
	if it.Size != int64(len(data)) {
		t.Fatalf("Size 记录错: %d vs %d", it.Size, len(data))
	}
}

func TestStress_DataURLRoundTripAtScale(t *testing.T) {
	// data URL 往返是插件注入的实际路径（SetToolBlocks 给的就是 data URL）。
	s := newTestStore(t, 0)
	for i := 0; i < 50; i++ {
		raw := randBytes(t, 2048)
		url := DataURL("image/png", raw)
		mime, decoded, ok := ParseDataURL(url)
		if !ok {
			t.Fatalf("第 %d 次解析失败", i)
		}
		if mime != "image/png" || !bytes.Equal(decoded, raw) {
			t.Fatalf("第 %d 次往返不一致", i)
		}
		d, err := s.Put(decoded, Item{MIME: mime})
		if err != nil {
			t.Fatal(err)
		}
		back, err := s.Get(d)
		if err != nil || !bytes.Equal(back, raw) {
			t.Fatalf("第 %d 次入库回读不一致: %v", i, err)
		}
	}
}
