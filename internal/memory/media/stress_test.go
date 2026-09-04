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
//   1. ref_count 与 media_refs 表的行数必须始终一致（错位会让 GC 误删或永不清）
//   2. GC 与读写并发时，有引用的内容绝不能被删
//   3. 同内容并发 Put 只落一份磁盘、digest 一致
//   4. SQLite 在多 goroutine 下不出现 "database is locked"

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

// checkRefIntegrity 校验核心不变量：每个 digest 的 ref_count 等于
// media_refs 里指向它的行数。这条对不上就意味着 GC 的判断依据是错的。
func checkRefIntegrity(t *testing.T, s *Store) {
	t.Helper()
	rows, err := s.db.Query(`
		SELECT m.digest, m.ref_count, COUNT(r.digest)
		FROM media m LEFT JOIN media_refs r ON m.digest = r.digest
		GROUP BY m.digest, m.ref_count`)
	if err != nil {
		t.Fatalf("integrity query: %v", err)
	}
	defer rows.Close()
	var bad int
	for rows.Next() {
		var d string
		var stored, actual int
		if err := rows.Scan(&d, &stored, &actual); err != nil {
			continue
		}
		if stored != actual {
			bad++
			if bad <= 5 {
				t.Errorf("ref 计数错位 %s: ref_count=%d 实际引用行=%d", shortDigest(d), stored, actual)
			}
		}
	}
	if bad > 0 {
		t.Fatalf("共 %d 条 digest 的 ref_count 与 media_refs 不一致", bad)
	}
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
	checkRefIntegrity(t, s)
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
	checkRefIntegrity(t, s)
}

func TestStress_ConcurrentRefChurn(t *testing.T) {
	// 引用增删风暴：多 owner 对少量 digest 反复 AddRef/DropRef。
	// 核心断言是最终 ref_count 与 media_refs 行数一致——错位就意味着
	// GC 会误删（计数偏低）或永不清（计数虚高）。
	s := newTestStore(t, 0)

	const digestCount = 8
	digests := make([]string, digestCount)
	for i := range digests {
		d, err := s.Put([]byte(fmt.Sprintf("payload-%d", i)), Item{MIME: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		digests[i] = d
	}

	const workers = 24
	const rounds = 40
	var wg sync.WaitGroup
	var addErr, dropErr atomic.Int64

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			owner := fmt.Sprintf("evt-%d", wid)
			for r := 0; r < rounds; r++ {
				d := digests[(wid+r)%digestCount]
				if err := s.AddRef(d, "context", owner); err != nil {
					addErr.Add(1)
				}
				// 故意重复 AddRef：幂等性在并发下也必须成立
				if err := s.AddRef(d, "context", owner); err != nil {
					addErr.Add(1)
				}
				if r%2 == 0 {
					if err := s.DropRef(d, "context", owner); err != nil {
						dropErr.Add(1)
					}
				}
			}
		}(w)
	}
	wg.Wait()

	if n := addErr.Load(); n > 0 {
		t.Fatalf("AddRef 失败 %d 次", n)
	}
	if n := dropErr.Load(); n > 0 {
		t.Fatalf("DropRef 失败 %d 次", n)
	}
	checkRefIntegrity(t, s)
}

func TestStress_GCConcurrentWithWrites(t *testing.T) {
	// GC 与读写并发。最重要的断言：有引用的内容在整个过程中始终可读。
	// 这条一旦破，记忆里的 digest 就成了悬空指针。
	s := newTestStore(t, 0)

	// 一批"受保护"的内容，全程持有引用
	const protectedCount = 10
	protected := make([]string, protectedCount)
	protectedData := make([][]byte, protectedCount)
	for i := range protected {
		data := append([]byte(fmt.Sprintf("protected-%d-", i)), randBytes(t, 256)...)
		d, err := s.Put(data, Item{MIME: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddRef(d, "document", fmt.Sprintf("doc-%d", i)); err != nil {
			t.Fatal(err)
		}
		protected[i] = d
		protectedData[i] = data
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var readErr atomic.Int64
	var gcRuns atomic.Int64
	var putCount atomic.Int64

	// 写入者：持续 Put 一次性内容（不加引用，是 GC 的正常目标）
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
				if _, err := s.Put(data, Item{MIME: "image/png"}); err == nil {
					putCount.Add(1)
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

	// GC 者：minAge=0 让所有无引用项立刻可清，最大化与写入的冲突
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, _, err := s.GC(0); err != nil {
				t.Errorf("GC 报错: %v", err)
				return
			}
			gcRuns.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()

	time.Sleep(1500 * time.Millisecond)
	close(stop)
	wg.Wait()

	if n := readErr.Load(); n > 0 {
		t.Fatalf("受保护内容读取失败 %d 次——GC 误删了有引用的项", n)
	}
	t.Logf("并发窗口内: Put=%d GC=%d 轮", putCount.Load(), gcRuns.Load())

	// 收尾确认：受保护的一个都没少
	for i, d := range protected {
		got, err := s.Get(d)
		if err != nil || !bytes.Equal(got, protectedData[i]) {
			t.Fatalf("收尾检查失败 %s: %v", shortDigest(d), err)
		}
		it, err := s.Stat(d)
		if err != nil || it.RefCount != 1 {
			t.Fatalf("受保护项引用计数应为 1: %+v err=%v", it, err)
		}
	}
	checkRefIntegrity(t, s)
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

func TestStress_CapacityGCUnderLoad(t *testing.T) {
	// 容量上限在持续写入下必须真正生效，且不碰有引用的项。
	const cap = 256 * 1024 // 256KB
	s := newTestStore(t, cap)

	// 先放 3 个有引用的大项（合计约 96KB），它们永不可删
	const keepN = 3
	keep := make([]string, keepN)
	for i := range keep {
		data := append([]byte(fmt.Sprintf("keep-%d-", i)), randBytes(t, 32*1024)...)
		d, err := s.Put(data, Item{MIME: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddRef(d, "graph_sentence", fmt.Sprintf("sent-%d", i)); err != nil {
			t.Fatal(err)
		}
		keep[i] = d
	}

	// 持续写入无引用内容，交替 GC
	for round := 0; round < 30; round++ {
		for i := 0; i < 3; i++ {
			data := append([]byte(fmt.Sprintf("tmp-%d-%d-", round, i)), randBytes(t, 16*1024)...)
			if _, err := s.Put(data, Item{MIME: "image/png"}); err != nil {
				t.Fatalf("round %d Put: %v", round, err)
			}
		}
		if _, _, err := s.GC(0); err != nil {
			t.Fatalf("round %d GC: %v", round, err)
		}
	}

	st := s.Stats()
	total := st["total_bytes"].(int64)
	t.Logf("上限 %d，收尾总量 %d，条目 %v", cap, total, st["count"])

	// 有引用的项必须都在
	for _, d := range keep {
		if _, err := s.Get(d); err != nil {
			t.Fatalf("有引用项被容量 GC 删了 %s: %v", shortDigest(d), err)
		}
	}
	// 无引用项应被压到上限附近：允许略超（有引用项本身可能就占了大头），
	// 但不该无界增长——30 轮 × 3 × 16KB = 1.4MB 若全留下就是失控。
	if total > cap*2 {
		t.Fatalf("容量 GC 未生效：总量 %d 远超上限 %d", total, cap)
	}
	checkRefIntegrity(t, s)
}

func TestStress_ReopenAfterHeavyChurn(t *testing.T) {
	// 大量写入 + GC 之后重开：元数据与磁盘不该出现互相不认的孤儿。
	dir := t.TempDir()
	s1, err := New(dir, 0)
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
			if err := s1.AddRef(d, "context", fmt.Sprintf("e-%d", i)); err != nil {
				t.Fatal(err)
			}
			kept = append(kept, d)
		}
	}
	if _, _, err := s1.GC(0); err != nil {
		t.Fatal(err)
	}
	beforeStats := s1.Stats()
	s1.Close()

	s2, err := New(dir, 0)
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
			t.Fatalf("有引用项重开后读不到 %s: %v", shortDigest(d), err)
		}
	}
	checkRefIntegrity(t, s2)
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
	checkRefIntegrity(t, s)
}
