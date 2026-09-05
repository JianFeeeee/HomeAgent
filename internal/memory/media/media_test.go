package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, maxBytes int64) *Store {
	t.Helper()
	s, err := New(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPut_ContentAddressedDedup(t *testing.T) {
	s := newTestStore(t, 0)
	data := []byte("fake-png-bytes")

	d1, err := s.Put(data, Item{MIME: "image/png", OriginPath: "/tmp/a.png"})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.Put(data, Item{MIME: "image/png", OriginPath: "/tmp/b.png"})
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("同一内容应得同一 digest：%s vs %s", d1, d2)
	}

	// 去重的意义：同一张图反复注入（连问几轮同一截图、see_video 相邻帧）
	// 只占一份磁盘。
	st := s.Stats()
	if st["count"].(int) != 1 {
		t.Fatalf("同一内容应只有 1 条记录，实际 %v", st["count"])
	}
}

func TestPut_KindInferredFromMIME(t *testing.T) {
	s := newTestStore(t, 0)
	cases := map[string]Kind{
		"image/png":  KindImage,
		"image/jpeg": KindImage,
		"audio/wav":  KindAudio,
		"video/mp4":  KindVideo,
		"text/plain": KindOther,
	}
	for mime, want := range cases {
		d, err := s.Put([]byte("payload-"+mime), Item{MIME: mime})
		if err != nil {
			t.Fatal(err)
		}
		it, err := s.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if it.Kind != want {
			t.Fatalf("%s 应归为 %s，实际 %s", mime, want, it.Kind)
		}
	}
}

func TestGet_DetectsCorruption(t *testing.T) {
	// CAS 的全部保证建立在「文件名 == 内容摘要」上。外部误改或位翻转必须
	// 被发现——把损坏的图喂给模型只会得到无从追溯的幻觉。
	s := newTestStore(t, 0)
	d, err := s.Put([]byte("original-content"), Item{MIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(d); err != nil {
		t.Fatalf("正常读取应成功: %v", err)
	}

	if err := os.WriteFile(s.blobPath(d), []byte("tampered!"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = s.Get(d)
	if err == nil {
		t.Fatal("内容被改后应报 digest 不匹配，却读成功了")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("错误应指明 digest 不匹配，得到: %v", err)
	}
}

func TestPut_NoPartialBlobOnDisk(t *testing.T) {
	// 先写 .tmp 再 rename：确认落地后目录里不留临时文件。
	s := newTestStore(t, 0)
	d, err := s.Put([]byte("some-bytes"), Item{MIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(s.blobPath(d))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("落地后不该留临时文件: %s", e.Name())
		}
	}
}

func TestRefCount_AddIsIdempotent(t *testing.T) {
	s := newTestStore(t, 0)
	d, _ := s.Put([]byte("img"), Item{MIME: "image/png"})

	for i := 0; i < 3; i++ {
		if err := s.AddRef(d, "context", "evt-1"); err != nil {
			t.Fatal(err)
		}
	}
	it, _ := s.Stat(d)
	// 重复 AddRef 若都递增，计数会虚高，GC 永远不敢清。
	if it.RefCount != 1 {
		t.Fatalf("同一 owner 重复 AddRef 应只计 1，实际 %d", it.RefCount)
	}

	if err := s.AddRef(d, "document", "doc-9"); err != nil {
		t.Fatal(err)
	}
	it, _ = s.Stat(d)
	if it.RefCount != 2 {
		t.Fatalf("不同 owner 应各计一次，实际 %d", it.RefCount)
	}
}

func TestRefCount_DropAndNeverNegative(t *testing.T) {
	s := newTestStore(t, 0)
	d, _ := s.Put([]byte("img"), Item{MIME: "image/png"})
	s.AddRef(d, "context", "e1")

	if err := s.DropRef(d, "context", "e1"); err != nil {
		t.Fatal(err)
	}
	it, _ := s.Stat(d)
	if it.RefCount != 0 {
		t.Fatalf("应归零，实际 %d", it.RefCount)
	}

	// 多余的 DropRef 不该把计数压成负数（负数会让容量 GC 的排序失去意义）
	for i := 0; i < 3; i++ {
		s.DropRef(d, "context", "e1")
	}
	it, _ = s.Stat(d)
	if it.RefCount != 0 {
		t.Fatalf("重复 DropRef 后仍应为 0，实际 %d", it.RefCount)
	}
}

func TestDropOwner_RemovesAllItsRefs(t *testing.T) {
	s := newTestStore(t, 0)
	d1, _ := s.Put([]byte("frame1"), Item{MIME: "image/jpeg"})
	d2, _ := s.Put([]byte("frame2"), Item{MIME: "image/jpeg"})
	s.AddRef(d1, "context", "evt-x")
	s.AddRef(d2, "context", "evt-x")
	s.AddRef(d1, "document", "doc-y") // 别的 owner 也引了 d1

	n, err := s.DropOwner("context", "evt-x")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应注销 2 条引用，实际 %d", n)
	}

	it1, _ := s.Stat(d1)
	it2, _ := s.Stat(d2)
	if it1.RefCount != 1 {
		t.Fatalf("d1 仍被 document 引用，应剩 1，实际 %d", it1.RefCount)
	}
	if it2.RefCount != 0 {
		t.Fatalf("d2 应归零，实际 %d", it2.RefCount)
	}
}

func TestRefs_ListsOwnerDigests(t *testing.T) {
	s := newTestStore(t, 0)
	d1, _ := s.Put([]byte("a"), Item{MIME: "image/png"})
	d2, _ := s.Put([]byte("b"), Item{MIME: "image/png"})
	s.AddRef(d1, "context", "e1")
	s.AddRef(d2, "context", "e1")

	got, err := s.Refs("context", "e1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应返回 2 个 digest，实际 %d", len(got))
	}
}

func TestGC_KeepsReferencedContent(t *testing.T) {
	// 有引用的项永不删除——那会让记忆里的 digest 变成悬空指针，
	// 正是本包要避免的。
	s := newTestStore(t, 0)
	kept, _ := s.Put([]byte("referenced"), Item{MIME: "image/png"})
	orphan, _ := s.Put([]byte("orphaned"), Item{MIME: "image/png"})
	s.AddRef(kept, "context", "e1")

	// minAge=0 让刚 Put 的都算超龄
	removed, _, err := s.GC(0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("应只清 1 条无引用项，实际 %d", removed)
	}
	if _, err := s.Get(kept); err != nil {
		t.Fatalf("被引用的内容不该被清: %v", err)
	}
	if _, err := s.Stat(orphan); err == nil {
		t.Fatal("无引用项的元数据应已删除")
	}
}

func TestGC_MinAgeProtectsFreshUnreferenced(t *testing.T) {
	// 刚 Put 还没来得及 AddRef 的项 refcount 也是 0；
	// minAge 必须保护它们，否则「Put 完还没挂上就被 GC 清掉」。
	s := newTestStore(t, 0)
	d, _ := s.Put([]byte("just-arrived"), Item{MIME: "image/png"})

	removed, _, err := s.GC(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("新入库项应被 minAge 保护，却清掉了 %d 条", removed)
	}
	if _, err := s.Get(d); err != nil {
		t.Fatalf("内容应还在: %v", err)
	}
}

func TestGC_EnforcesCapacity(t *testing.T) {
	// 容量上限：清完超龄项后仍超限，继续按 last_seen 从旧到新淘汰无引用项。
	blob := make([]byte, 1024)
	s := newTestStore(t, 2048) // 只容 2KB

	var digests []string
	for i := 0; i < 4; i++ {
		b := append([]byte{byte(i)}, blob...) // 内容各异，避免去重
		d, err := s.Put(b, Item{MIME: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, d)
		time.Sleep(2 * time.Millisecond) // 拉开 last_seen
	}

	// 保护最后一个，确认容量 GC 也不碰有引用的
	s.AddRef(digests[3], "context", "e1")

	removed, freed, err := s.GC(0)
	if err != nil {
		t.Fatal(err)
	}
	if removed == 0 {
		t.Fatal("超限应触发淘汰")
	}
	if _, err := s.Get(digests[3]); err != nil {
		t.Fatalf("有引用项即使超限也不该删: %v", err)
	}
	t.Logf("removed=%d freed=%d", removed, freed)

	st := s.Stats()
	if total := st["total_bytes"].(int64); total > 2048 {
		// 有引用项可能让总量降不到线下，这是刻意的（宁可超限也不断引用）
		t.Logf("总量 %d 仍超 2048，因有引用项不可删（预期行为）", total)
	}
}

func TestDescribe_OverwritesExplicitly(t *testing.T) {
	s := newTestStore(t, 0)
	d, _ := s.Put([]byte("img"), Item{MIME: "image/png"})

	if err := s.Describe(d, "一只橘猫", "vis-a"); err != nil {
		t.Fatal(err)
	}
	it, _ := s.Stat(d)
	if it.Description != "一只橘猫" || it.DescribedBy != "vis-a" {
		t.Fatalf("描述未写入: %+v", it)
	}

	// Describe 是显式操作，允许覆盖（换更强模型重描述）
	if err := s.Describe(d, "一只橘色虎斑猫坐在窗台", "vis-b"); err != nil {
		t.Fatal(err)
	}
	it, _ = s.Stat(d)
	if !strings.Contains(it.Description, "虎斑") || it.DescribedBy != "vis-b" {
		t.Fatalf("Describe 应覆盖旧描述: %+v", it)
	}
}

func TestDescribe_UnknownDigestErrors(t *testing.T) {
	s := newTestStore(t, 0)
	err := s.Describe("deadbeef", "x", "y")
	if err == nil {
		t.Fatal("未知 digest 应报错而非静默成功")
	}
}

func TestPut_DoesNotClobberExistingDescription(t *testing.T) {
	// 先到的描述可能来自更强的模型；后到的空值不该把它冲掉。
	s := newTestStore(t, 0)
	data := []byte("img")
	d, _ := s.Put(data, Item{MIME: "image/png", Description: "详细描述", DescribedBy: "strong-model"})

	// 第二次 Put 同内容但不带描述
	if _, err := s.Put(data, Item{MIME: "image/png"}); err != nil {
		t.Fatal(err)
	}
	it, _ := s.Stat(d)
	if it.Description != "详细描述" || it.DescribedBy != "strong-model" {
		t.Fatalf("重复 Put 的空描述不该冲掉已有描述: %+v", it)
	}
}

func TestPut_BackfillsMissingDimensions(t *testing.T) {
	// 后来者可能带着前一次没有的信息（尺寸、工具名）
	s := newTestStore(t, 0)
	data := []byte("img")
	d, _ := s.Put(data, Item{MIME: "image/png"})

	if _, err := s.Put(data, Item{MIME: "image/png", Width: 640, Height: 480, Tool: "multimodal_see_picture"}); err != nil {
		t.Fatal(err)
	}
	it, _ := s.Stat(d)
	if it.Width != 640 || it.Height != 480 {
		t.Fatalf("尺寸应被补写: %dx%d", it.Width, it.Height)
	}
	if it.Tool != "multimodal_see_picture" {
		t.Fatalf("工具名应被补写: %q", it.Tool)
	}
}

func TestSearch_FiltersByDescriptionAndKind(t *testing.T) {
	s := newTestStore(t, 0)
	di, _ := s.Put([]byte("chart-img"), Item{MIME: "image/png"})
	da, _ := s.Put([]byte("speech-aud"), Item{MIME: "audio/wav"})
	dn, _ := s.Put([]byte("no-desc"), Item{MIME: "image/png"})
	s.Describe(di, "一张蓝色的柱状图表", "vis")
	s.Describe(da, "一段关于图表的讲解录音", "aud")

	all, err := s.Search("图表", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("两条描述都含「图表」，应返回 2，实际 %d", len(all))
	}

	imgs, _ := s.Search("图表", KindImage, 10)
	if len(imgs) != 1 || imgs[0].Digest != di {
		t.Fatalf("按 image 过滤应只剩图片，实际 %d 条", len(imgs))
	}

	// 无描述的项不该出现在语义检索结果里
	for _, it := range all {
		if it.Digest == dn {
			t.Fatal("无描述的项不该被 Search 返回")
		}
	}
}

func TestPending_ReturnsUndescribed(t *testing.T) {
	s := newTestStore(t, 0)
	described, _ := s.Put([]byte("has-desc"), Item{MIME: "image/png"})
	undescribed, _ := s.Put([]byte("needs-desc"), Item{MIME: "image/png"})
	s.Describe(described, "已有描述", "vis")

	pending, err := s.Pending(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Digest != undescribed {
		t.Fatalf("应只返回未描述项，实际 %d 条", len(pending))
	}
}

func TestParseDataURL(t *testing.T) {
	// 与 agent/api 的 parseAudioDataURL 不同：这里要解码后的字节。
	raw := []byte{0x89, 'P', 'N', 'G'}
	url := DataURL("image/png", raw)

	mime, data, ok := ParseDataURL(url)
	if !ok {
		t.Fatal("应解析成功")
	}
	if mime != "image/png" {
		t.Fatalf("MIME 应为 image/png，得到 %q", mime)
	}
	if string(data) != string(raw) {
		t.Fatalf("字节应还原，得到 %v", data)
	}

	for _, bad := range []string{
		"http://example.com/x.png",  // 不是 data URL
		"data:image/png,notbase64",  // 缺 ;base64
		"data:;base64,",             // 空 MIME 与空载荷
		"data:image/png;base64,!!!", // 非法 base64
	} {
		if _, _, ok := ParseDataURL(bad); ok {
			t.Fatalf("%q 应解析失败", bad)
		}
	}
}

func TestStats_CountsByKindAndDescription(t *testing.T) {
	s := newTestStore(t, 4096)
	d1, _ := s.Put([]byte("i1"), Item{MIME: "image/png"})
	s.Put([]byte("i2"), Item{MIME: "image/jpeg"})
	s.Put([]byte("a1"), Item{MIME: "audio/wav"})
	s.Describe(d1, "描述", "vis")
	s.AddRef(d1, "context", "e1")

	st := s.Stats()
	if st["count"].(int) != 3 {
		t.Fatalf("count 应为 3，实际 %v", st["count"])
	}
	if st["described"].(int) != 1 {
		t.Fatalf("described 应为 1，实际 %v", st["described"])
	}
	if st["unreferenced"].(int) != 2 {
		t.Fatalf("unreferenced 应为 2，实际 %v", st["unreferenced"])
	}
	byKind := st["by_kind"].(map[string]int)
	if byKind["image"] != 2 || byKind["audio"] != 1 {
		t.Fatalf("by_kind 不对: %v", byKind)
	}
}

func TestPut_RejectsEmpty(t *testing.T) {
	s := newTestStore(t, 0)
	if _, err := s.Put(nil, Item{MIME: "image/png"}); err == nil {
		t.Fatal("空内容应报错")
	}
}

func TestReopen_PersistsAcrossRestart(t *testing.T) {
	// 记忆的意义就在于跨重启还在。
	dir := t.TempDir()
	s1, err := New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s1.Put([]byte("persistent-img"), Item{MIME: "image/png", OriginPath: "/tmp/x.png"})
	s1.Describe(d, "跨重启的描述", "vis")
	s1.AddRef(d, "context", "e1")
	s1.Close()

	s2, err := New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	it, err := s2.Stat(d)
	if err != nil {
		t.Fatalf("重开后应能查到: %v", err)
	}
	if it.Description != "跨重启的描述" || it.RefCount != 1 {
		t.Fatalf("元数据应持久化: %+v", it)
	}
	data, err := s2.Get(d)
	if err != nil || string(data) != "persistent-img" {
		t.Fatalf("内容应持久化: %v / %q", err, data)
	}
}

func TestPending_ExcludesAttemptedButUndescribable(t *testing.T) {
	// 「已尝试但无法描述」的项必须退出待描述队列。
	//
	// 这些项被标记为 described_by=unsupported/content-missing 而 description
	// 仍为空。若 Pending 只看 description，它们每轮都会被取出来重试、
	// 永久占着 LIMIT 的名额，真正需要描述的新项永远轮不到。
	s := newTestStore(t, 0)

	fresh, _ := s.Put([]byte("needs-describe"), Item{MIME: "image/png"})
	unsupported, _ := s.Put([]byte("cannot-describe"), Item{MIME: "application/octet-stream"})
	described, _ := s.Put([]byte("已描述"), Item{MIME: "image/png"})

	// 标记「尝试过但不支持」：description 空，described_by 非空
	if err := s.Describe(unsupported, "", "unsupported"); err != nil {
		t.Fatal(err)
	}
	if err := s.Describe(described, "一张图", "visionllm"); err != nil {
		t.Fatal(err)
	}

	pending, err := s.Pending(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		var names []string
		for _, p := range pending {
			names = append(names, shortDigest(p.Digest))
		}
		t.Fatalf("应只剩 1 条待描述，实际 %d 条: %v", len(pending), names)
	}
	if pending[0].Digest != fresh {
		t.Fatalf("待描述的应是未处理项，实际 %s", shortDigest(pending[0].Digest))
	}
}
