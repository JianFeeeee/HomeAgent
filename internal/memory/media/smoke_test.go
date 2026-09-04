package media

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"testing"
)

// 冒烟测试：走真实数据路径的端到端场景，而非孤立的 API 单测。
//
// 之前这套场景是 internal/memory/media/smoke/ 下一个带 //go:build smoke 的
// 独立 main，得记着加 -tags smoke 才跑得到——那种早晚会被忘掉。搬成普通
// 测试后它随 go test ./... 一起跑，冒烟的意义（每次改动都过一遍真实链路）
// 才真正成立。

// makePNG 生成一张 w×h 的条带 PNG，用真 PNG 而不是随机字节，
// 让入库/回读/digest 走的是与生产一致的数据形态。
//
// variant 注入到像素而不只用于选色：最初写的是
// palette[(variant+y*3/h)%5]，调色盘只 5 色，于是 variant=0 与 5 产出
// 逐字节相同的 PNG——冒烟跑出「6 帧只得 5 条」，看着像存储丢了一帧，
// 实际是 CAS 正确去重了两张真同图。冒烟要验的是「不同帧各存一份」，
// 夹具就必须保证帧间真的不同。
func makePNG(w, h, variant int) []byte {
	palette := [][3]byte{
		{255, 0, 0}, {0, 192, 0}, {0, 0, 255}, {255, 220, 0}, {160, 0, 200},
	}
	var raw bytes.Buffer
	for y := 0; y < h; y++ {
		raw.WriteByte(0) // 每行的滤波器字节
		c := palette[(variant+y*3/h)%len(palette)]
		for x := 0; x < w; x++ {
			raw.Write(c[:])
		}
	}
	// 把 variant 写进首行头几个像素，确保不同 variant 字节必然不同。
	b := raw.Bytes()
	if len(b) > 8 {
		b[1] = byte(variant)
		b[2] = byte(variant >> 8)
	}

	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(b)
	zw.Close()

	chunk := func(typ string, data []byte) []byte {
		var out bytes.Buffer
		binary.Write(&out, binary.BigEndian, uint32(len(data)))
		out.WriteString(typ)
		out.Write(data)
		binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
		return out.Bytes()
	}
	var ihdr bytes.Buffer
	binary.Write(&ihdr, binary.BigEndian, uint32(w))
	binary.Write(&ihdr, binary.BigEndian, uint32(h))
	ihdr.Write([]byte{8, 2, 0, 0, 0}) // 8bit 深度、truecolor

	var out bytes.Buffer
	out.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	out.Write(chunk("IHDR", ihdr.Bytes()))
	out.Write(chunk("IDAT", z.Bytes()))
	out.Write(chunk("IEND", nil))
	return out.Bytes()
}

func TestSmoke_SamePictureAcrossTurns(t *testing.T) {
	// 场景：用户连问几轮同一张截图。multimodal 每轮都会重新注入，
	// 磁盘上应该只有一份，但每轮的 context 事件各持一个引用。
	s := newTestStore(t, 50*1024*1024)
	png := makePNG(400, 400, 0)

	var d0 string
	for turn := 1; turn <= 5; turn++ {
		// 走 data URL：这是 SetToolBlocks 实际给出的形态
		url := DataURL("image/png", png)
		mime, data, ok := ParseDataURL(url)
		if !ok {
			t.Fatalf("第 %d 轮 data URL 解析失败", turn)
		}
		d, err := s.Put(data, Item{
			MIME: mime, Width: 400, Height: 400,
			OriginPath: fmt.Sprintf("/tmp/probe_%d.png", turn),
			Tool:       "multimodal_see_picture",
		})
		if err != nil {
			t.Fatalf("第 %d 轮 Put: %v", turn, err)
		}
		if d0 == "" {
			d0 = d
		} else if d != d0 {
			t.Fatalf("同一张图第 %d 轮 digest 变了", turn)
		}
		if err := s.AddRef(d, "context", fmt.Sprintf("evt-%d", turn)); err != nil {
			t.Fatalf("第 %d 轮 AddRef: %v", turn, err)
		}
	}

	st := s.Stats()
	if st["count"].(int) != 1 {
		t.Fatalf("5 轮同图应只存 1 份，实际 %v 条", st["count"])
	}
	if total := st["total_bytes"].(int64); total != int64(len(png)) {
		t.Fatalf("字节数应等于单张原图 %d，实际 %d", len(png), total)
	}
	it, _ := s.Stat(d0)
	if it.RefCount != 5 {
		t.Fatalf("应有 5 个引用，实际 %d", it.RefCount)
	}
	t.Logf("同图 5 轮：条目=1 字节=%d refcount=%d", len(png), it.RefCount)
	checkRefIntegrity(t, s)
}

func TestSmoke_VideoFramesDistinct(t *testing.T) {
	// 场景：see_video 抽 6 帧，帧间内容不同，应各存一份并共享一个 owner。
	s := newTestStore(t, 50*1024*1024)
	var frames []string
	for i := 0; i < 6; i++ {
		d, err := s.Put(makePNG(320, 240, i), Item{
			MIME: "image/jpeg", Width: 320, Height: 240, Tool: "multimodal_see_video",
		})
		if err != nil {
			t.Fatalf("第 %d 帧: %v", i, err)
		}
		frames = append(frames, d)
		if err := s.AddRef(d, "context", "evt-video"); err != nil {
			t.Fatal(err)
		}
	}

	st := s.Stats()
	if st["count"].(int) != 6 {
		t.Fatalf("6 帧应各存一份，实际 %v 条", st["count"])
	}
	refs, err := s.Refs("context", "evt-video")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 6 {
		t.Fatalf("evt-video 应引用 6 帧，实际 %d", len(refs))
	}
	checkRefIntegrity(t, s)
}

func TestSmoke_DescribeThenRetrieve(t *testing.T) {
	// 场景 C：视觉模型描述落库后，描述文字成为可检索的语义入口。
	// 这是本方案最关键的一环——blob 可能被淘汰，描述会长期留在记忆里。
	s := newTestStore(t, 50*1024*1024)

	pic, _ := s.Put(makePNG(400, 400, 0), Item{MIME: "image/png", Tool: "multimodal_see_picture"})
	if err := s.Describe(pic, "一张 400x400 的三色带图：上红、中绿、下蓝", "visionllm"); err != nil {
		t.Fatal(err)
	}
	var frames []string
	for i := 0; i < 6; i++ {
		d, _ := s.Put(makePNG(320, 240, i), Item{MIME: "image/jpeg", Tool: "multimodal_see_video"})
		frames = append(frames, d)
		if err := s.Describe(d, fmt.Sprintf("视频第 %d 帧：测试图卡，含彩条与计数器", i+1), "visionllm"); err != nil {
			t.Fatal(err)
		}
	}

	if hits, _ := s.Search("三色带", KindImage, 10); len(hits) != 1 {
		t.Fatalf("搜「三色带」应命中 1 条，实际 %d", len(hits))
	}
	if hits, _ := s.Search("计数器", KindImage, 10); len(hits) != 6 {
		t.Fatalf("搜「计数器」应命中 6 帧，实际 %d", len(hits))
	}
	pend, _ := s.Pending(100)
	if len(pend) != 0 {
		t.Fatalf("应全部已描述，仍有 %d 条待描述", len(pend))
	}
	_ = frames
}

func TestSmoke_ArchiveTransfersOwnership(t *testing.T) {
	// 场景：L0 的 context 事件被 Prune 归档进 L2 文档，
	// 媒体引用需从 context owner 转到 document owner，期间内容不能被 GC 掉。
	s := newTestStore(t, 50*1024*1024)
	png := makePNG(400, 400, 0)
	d, _ := s.Put(png, Item{MIME: "image/png", Tool: "multimodal_see_picture"})
	for turn := 1; turn <= 5; turn++ {
		s.AddRef(d, "context", fmt.Sprintf("evt-%d", turn))
	}

	// evt-1 被淘汰，其内容归档为一篇文档
	n, err := s.DropOwner("context", "evt-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应注销 1 条引用，实际 %d", n)
	}
	if err := s.AddRef(d, "document", "doc_archived_001"); err != nil {
		t.Fatal(err)
	}

	it, _ := s.Stat(d)
	if it.RefCount != 5 {
		t.Fatalf("引用转移后总数应仍为 5（4 context + 1 document），实际 %d", it.RefCount)
	}
	// 归档过程中内容必须始终可读
	if got, err := s.Get(d); err != nil || !bytes.Equal(got, png) {
		t.Fatalf("归档后内容应完好: %v", err)
	}
	checkRefIntegrity(t, s)
}

func TestSmoke_GCSweepsToolLeftovers(t *testing.T) {
	// 场景：别的工具（cmd_run 之类）产出的一次性图片没人引用，
	// 应被 GC 清掉；而被记忆引用的媒体一个都不能少。
	s := newTestStore(t, 50*1024*1024)

	keep, _ := s.Put(makePNG(400, 400, 0), Item{MIME: "image/png"})
	s.AddRef(keep, "document", "doc-1")
	var frames []string
	for i := 0; i < 6; i++ {
		d, _ := s.Put(makePNG(320, 240, i), Item{MIME: "image/jpeg"})
		s.AddRef(d, "context", "evt-video")
		frames = append(frames, d)
	}
	// 1000+i 保证与上面的帧、以及彼此都不重复
	for i := 0; i < 20; i++ {
		s.Put(makePNG(100, 100, 1000+i), Item{MIME: "image/png", Tool: "cmd_run"})
	}

	before := s.Stats()["count"].(int)
	removed, freed, err := s.GC(0)
	if err != nil {
		t.Fatal(err)
	}
	after := s.Stats()["count"].(int)
	if removed != 20 {
		t.Fatalf("应清 20 条孤儿，实际 %d", removed)
	}
	if after != before-20 {
		t.Fatalf("条目数应从 %d 降到 %d，实际 %d", before, before-20, after)
	}
	if _, err := s.Get(keep); err != nil {
		t.Fatalf("被文档引用的图被误删: %v", err)
	}
	for i, f := range frames {
		if _, err := s.Get(f); err != nil {
			t.Fatalf("第 %d 帧被误删: %v", i, err)
		}
	}
	t.Logf("GC: %d 条 → 清 %d 条（%d 字节）→ %d 条", before, removed, freed, after)
	checkRefIntegrity(t, s)
}

func TestSmoke_FullLifecycleAcrossRestart(t *testing.T) {
	// 端到端：入库 → 描述 → 引用 → GC → 重启 → 检索，
	// 并确认磁盘与元数据不出现双向孤儿。记忆的意义就在于跨重启还在。
	dir := t.TempDir()
	s, err := New(dir, 50*1024*1024)
	if err != nil {
		t.Fatal(err)
	}

	png := makePNG(400, 400, 0)
	pic, _ := s.Put(png, Item{MIME: "image/png", Width: 400, Height: 400, Tool: "multimodal_see_picture"})
	s.Describe(pic, "一张 400x400 的三色带图：上红、中绿、下蓝", "visionllm")
	s.AddRef(pic, "graph_sentence", "sent-42")
	for i := 0; i < 6; i++ {
		d, _ := s.Put(makePNG(320, 240, i), Item{MIME: "image/jpeg", Tool: "multimodal_see_video"})
		s.Describe(d, fmt.Sprintf("视频第 %d 帧", i+1), "visionllm")
		s.AddRef(d, "context", "evt-video")
	}
	for i := 0; i < 10; i++ {
		s.Put(makePNG(64, 64, 2000+i), Item{MIME: "image/png", Tool: "cmd_run"})
	}
	if _, _, err := s.GC(0); err != nil {
		t.Fatal(err)
	}
	beforeCount := s.Stats()["count"].(int)
	s.Close()

	s2, err := New(dir, 50*1024*1024)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	defer s2.Close()

	if got := s2.Stats()["count"].(int); got != beforeCount {
		t.Fatalf("重开后条目数变了: %d → %d", beforeCount, got)
	}
	it, err := s2.Stat(pic)
	if err != nil {
		t.Fatalf("重开后查不到: %v", err)
	}
	if it.Description == "" || it.RefCount != 1 {
		t.Fatalf("元数据未持久化: %+v", it)
	}
	data, err := s2.Get(pic)
	if err != nil || !bytes.Equal(data, png) {
		t.Fatalf("重开后内容不一致: %v", err)
	}
	if refs, _ := s2.Refs("context", "evt-video"); len(refs) != 6 {
		t.Fatalf("重开后视频帧引用应为 6，实际 %d", len(refs))
	}
	if hits, _ := s2.Search("三色带", KindImage, 10); len(hits) != 1 {
		t.Fatal("重开后描述应仍可检索")
	}

	// 磁盘文件数 == 元数据条数：无「元数据在文件没了」也无「文件在元数据没了」
	if n := blobFileCount(t, s2); n != beforeCount {
		t.Fatalf("磁盘 blob=%d 与元数据=%d 不一致", n, beforeCount)
	}
	checkRefIntegrity(t, s2)
	t.Logf("跨重启：%d 条目、描述与引用全部完好", beforeCount)
}
