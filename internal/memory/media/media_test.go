package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestStore 建一个临时媒体存储。
// 参数保留只为兼容旧调用点；媒体不再有容量上限（生命周期由记忆块决定）。
func newTestStore(t *testing.T, _ ...int64) *Store {
	t.Helper()
	s, err := New(t.TempDir())
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

func TestDelete_RemovesContentAndMetadata(t *testing.T) {
	// 删除块即删除内容：Delete 同时清掉 blob 与元数据。
	// 这不是 GC，也不看引用计数——调用方是记忆系统本身。
	s := newTestStore(t)
	d, _ := s.Put([]byte("held"), Item{MIME: "image/png"})
	other, _ := s.Put([]byte("orphaned"), Item{MIME: "image/png"})

	if err := s.Delete(other); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(other); err == nil {
		t.Fatal("删除后元数据应已移除")
	}
	if _, err := s.Get(other); err == nil {
		t.Fatal("删除后内容应已移除")
	}
	// 未被删除的项不受影响
	if _, err := s.Get(d); err != nil {
		t.Fatalf("未删除的内容不该受影响: %v", err)
	}
}

func TestDelete_UnknownDigestIsNoop(t *testing.T) {
	s := newTestStore(t)
	if err := s.Delete(""); err != nil {
		t.Fatalf("空 digest 应为无操作: %v", err)
	}
	if err := s.Delete("ffffffffffffffff"); err != nil {
		t.Fatalf("不存在的 digest 应为无操作: %v", err)
	}
}

func TestDescribe_Removed(t *testing.T) {
	// 媒体不再有文字描述：描述式索引是废弃的就机制。
	// 这里只保留一个编译期断言，确保 API 不会静默回归。
	s := newTestStore(t)
	if _, err := s.Put([]byte("img"), Item{MIME: "image/png"}); err != nil {
		t.Fatal(err)
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

func TestStats_CountsByKind(t *testing.T) {
	s := newTestStore(t)
	s.Put([]byte("i1"), Item{MIME: "image/png"})
	s.Put([]byte("i2"), Item{MIME: "image/jpeg"})
	s.Put([]byte("a1"), Item{MIME: "audio/wav"})

	st := s.Stats()
	if st["count"].(int) != 3 {
		t.Fatalf("count 应为 3，实际 %v", st["count"])
	}
	if _, ok := st["described"]; ok {
		t.Fatal("媒体已不再有描述计数")
	}
	byKind := st["by_kind"].(map[string]int)
	if byKind["image"] != 2 || byKind["audio"] != 1 {
		t.Fatalf("by_kind 不对: %v", byKind)
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

func TestPut_RejectsEmpty(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Put(nil, Item{MIME: "image/png"}); err == nil {
		t.Fatal("空内容应报错")
	}
}

func TestReopen_PersistsAcrossRestart(t *testing.T) {
	// 记忆的意义就在于跨重启还在。
	dir := t.TempDir()
	s1, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s1.Put([]byte("persistent-img"), Item{MIME: "image/png", OriginPath: "/tmp/x.png"})
	s1.SetVec(d, []float64{0.1, 0.2}, "test-space")
	s1.Close()

	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	it, err := s2.Stat(d)
	if err != nil {
		t.Fatalf("重开后应能查到: %v", err)
	}
	if it.OriginPath != "/tmp/x.png" || len(it.Vec) != 2 || it.VecModel != "test-space" {
		t.Fatalf("元数据与向量应持久化: %+v", it)
	}
	data, err := s2.Get(d)
	if err != nil || string(data) != "persistent-img" {
		t.Fatalf("内容应持久化: %v / %q", err, data)
	}
}

func TestStaleVecDigests_TracksModelSwitch(t *testing.T) {
	// 模型切换后旧向量必须被重算：StaleVecDigests 是启动迁移的入口。
	s := newTestStore(t)
	d1, _ := s.Put([]byte("a"), Item{MIME: "image/png"})
	d2, _ := s.Put([]byte("b"), Item{MIME: "image/png"})
	s.SetVec(d1, []float64{0.1}, "space-a")

	stale, err := s.StaleVecDigestsAll("space-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != d2 {
		t.Fatalf("只有未嵌入的 d2 需重算，实际 %v", stale)
	}

	stale, err = s.StaleVecDigestsAll("space-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 2 {
		t.Fatalf("换空间后两条都需重算，实际 %v", stale)
	}
}
