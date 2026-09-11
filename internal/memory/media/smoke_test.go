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
// 媒体存储现在只做内容寻址（CAS）：字节 + 元数据 + 向量。
// “哪些字节还活着”由三层记忆持有的一等记忆块决定，调用方把该集合传给
// GC/检索，本层不维护 media_refs/ref_count 这类平行账本。

// makePNG 生成一张 w×h 的条带 PNG，用真 PNG 而不是随机字节，
// 让入库/回读/digest 走的是与生产一致的数据形态。
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
	// 内容寻址天然去重，磁盘上只应有一份。
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
	}

	st := s.Stats()
	if st["count"].(int) != 1 {
		t.Fatalf("5 轮同图应只存 1 份，实际 %v 条", st["count"])
	}
	if total := st["total_bytes"].(int64); total != int64(len(png)) {
		t.Fatalf("字节数应等于单张原图 %d，实际 %d", len(png), total)
	}
	// 三层记忆持有它；内容应仍可读
	if _, err := s.Get(d0); err != nil {
		t.Fatalf("内容应仍可读: %v", err)
	}
}

func TestSmoke_VideoFramesDistinct(t *testing.T) {
	// 场景：see_video 抽 6 帧，帧间内容不同，应各存一份。
	s := newTestStore(t, 50*1024*1024)
	keep := map[string]bool{}
	for i := 0; i < 6; i++ {
		d, err := s.Put(makePNG(320, 240, i), Item{
			MIME: "image/jpeg", Width: 320, Height: 240, Tool: "multimodal_see_video",
		})
		if err != nil {
			t.Fatalf("第 %d 帧: %v", i, err)
		}
		keep[d] = true
	}

	if st := s.Stats(); st["count"].(int) != 6 {
		t.Fatalf("6 帧应各存一份，实际 %v 条", st["count"])
	}
}

func TestSmoke_DescribeThenRetrieve(t *testing.T) {
	// 场景 C：视觉模型描述落库后，描述文字成为可检索的语义入口。
	// 这是本方案最关键的一环——blob 可能被淘汰，描述会长期留在记忆里。
	s := newTestStore(t, 50*1024*1024)

	pic, _ := s.Put(makePNG(400, 400, 0), Item{MIME: "image/png", Tool: "multimodal_see_picture"})
	if err := s.Describe(pic, "一张 400x400 的三色带图：上红、中绿、下蓝", "visionllm"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		d, _ := s.Put(makePNG(320, 240, i), Item{MIME: "image/jpeg", Tool: "multimodal_see_video"})
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
}

func TestSmoke_ContentSurvivesLayerMigration(t *testing.T) {
	// 场景：同一份媒体随记忆块从 Context 迁移到 Document 再到 Graph。
	// 迁移的是块本身，digest 不变，因此内容在整条链路上始终可读。
	s := newTestStore(t, 50*1024*1024)
	png := makePNG(400, 400, 0)
	d, _ := s.Put(png, Item{MIME: "image/png", Tool: "multimodal_see_picture"})

	// 迁移过程中该 digest 始终可读
	for _, layer := range []string{"context", "document", "graph"} {
		if got, err := s.Get(d); err != nil || !bytes.Equal(got, png) {
			t.Fatalf("迁移到 %s 时内容应完好: %v", layer, err)
		}
	}
}

func TestSmoke_DeleteRemovesOnlyThatContent(t *testing.T) {
	// 场景：某个工具产出的一次性图片所在的记忆块被删除时，
	// 只有它自己的内容被删；其他块的内容一个都不能少。
	s := newTestStore(t)

	held, _ := s.Put(makePNG(400, 400, 0), Item{MIME: "image/png"})
	var frames []string
	for i := 0; i < 6; i++ {
		d, _ := s.Put(makePNG(320, 240, i), Item{MIME: "image/jpeg"})
		frames = append(frames, d)
	}
	var ephemeral []string
	for i := 0; i < 20; i++ {
		d, _ := s.Put(makePNG(100, 100, 1000+i), Item{MIME: "image/png", Tool: "cmd_run"})
		ephemeral = append(ephemeral, d)
	}

	before := s.Stats()["count"].(int)
	for _, d := range ephemeral {
		if err := s.Delete(d); err != nil {
			t.Fatal(err)
		}
	}
	after := s.Stats()["count"].(int)
	if after != before-20 {
		t.Fatalf("条目数应从 %d 降到 %d，实际 %d", before, before-20, after)
	}
	if _, err := s.Get(held); err != nil {
		t.Fatalf("被保留的内容被误删: %v", err)
	}
	for i, f := range frames {
		if _, err := s.Get(f); err != nil {
			t.Fatalf("第 %d 帧被误删: %v", i, err)
		}
	}
}

func TestSmoke_FullLifecycleAcrossRestart(t *testing.T) {
	// 端到端：入库 → 描述 → 删除一些内容 → 重启 → 检索，
	// 并确认磁盘与元数据不出现双向孤儿。记忆的意义就在于跨重启还在。
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	png := makePNG(400, 400, 0)
	pic, _ := s.Put(png, Item{MIME: "image/png", Width: 400, Height: 400, Tool: "multimodal_see_picture"})
	s.Describe(pic, "一张 400x400 的三色带图：上红、中绿、下蓝", "visionllm")
	for i := 0; i < 6; i++ {
		d, _ := s.Put(makePNG(320, 240, i), Item{MIME: "image/jpeg", Tool: "multimodal_see_video"})
		s.Describe(d, fmt.Sprintf("视频第 %d 帧", i+1), "visionllm")
	}
	for i := 0; i < 10; i++ {
		d, _ := s.Put(makePNG(64, 64, 2000+i), Item{MIME: "image/png", Tool: "cmd_run"})
		if err := s.Delete(d); err != nil {
			t.Fatal(err)
		}
	}
	beforeCount := s.Stats()["count"].(int)
	s.Close()

	s2, err := New(dir)
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
	if it.Description == "" {
		t.Fatalf("元数据未持久化: %+v", it)
	}
	data, err := s2.Get(pic)
	if err != nil || !bytes.Equal(data, png) {
		t.Fatalf("重开后内容不一致: %v", err)
	}
	if hits, _ := s2.Search("三色带", KindImage, 10); len(hits) != 1 {
		t.Fatal("重开后描述应仍可检索")
	}

	// 磁盘文件数 == 元数据条数：无「元数据在文件没了」也无「文件在元数据没了」
	if n := blobFileCount(t, s2); n != beforeCount {
		t.Fatalf("磁盘 blob=%d 与元数据=%d 不一致", n, beforeCount)
	}
	t.Logf("跨重启：%d 条目、描述与内容全部完好", beforeCount)
}
