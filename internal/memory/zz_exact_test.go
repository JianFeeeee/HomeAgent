package memory

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// ★ 「形态 1 带精确串 → 强」需要多类型验证，不能只靠 13010 一条。
func TestExact_多类型精确串都能置顶(t *testing.T) {
	db := os.Getenv("PROD_SNAPSHOT")
	if db == "" {
		t.Skip("需要 PROD_SNAPSHOT")
	}
	g, _ := NewGraphDB(db)
	defer func() { _ = g.Close() }()
	ad, fp := probeEmbedder(t, "/var/tmp/ha-c/models/chinese-clip-vit-b16-onnx")

	cases := []struct{ name, query, want string }{
		{"端口", "本机 13010 端口对应什么", "13010"},
		{"IP", "127.0.0.1 这个地址对应什么", "127.0.0.1"},
		{"路径", "/api/sources 这个接口做什么用", "/api/sources"},
		{"commit", "f91b27a 这个提交做了什么", "f91b27a"},
		{"日期", "2026-09-04 那次记录是什么", "2026-09-04"},
	}
	blocks, _ := g.MemoryBlocks()
	textBlocks := make([]MemoryBlock, 0, len(blocks))
	for _, b := range blocks {
		if b.Text != "" {
			textBlocks = append(textBlocks, b)
		}
	}

	pass := 0
	for _, c := range cases {
		vec, err := ad.VectorizeDense(c.query)
		if err != nil {
			t.Fatal(err)
		}
		vecHits, err := g.RecallBlocks(BlockRecallQuery{Vector: vec, Fingerprint: fp, TopK: 8})
		if err != nil {
			t.Fatal(err)
		}
		// 融合（生产路径）
		fused := fuseCandidates(vecHits, textBlocks, c.query, defaultWeights())

		pos := -1
		for i, f := range fused {
			if strings.Contains(f.Text, c.want) {
				pos = i
				break
			}
		}
		mark := "✘"
		if pos == 0 {
			mark = "✓"
			pass++
		}
		desc := ""
		if len(fused) > 0 && pos > 0 {
			desc = fmt.Sprintf("（第一是 %.2f %s）", fused[0].Score, truncStr(fused[0].Text, 30))
		}
		fmt.Printf("  [%s] %-8s %-26s 目标位置=%d %s\n", mark, c.name, c.query, pos, desc)
	}
	fmt.Printf("  合计 %d/%d 置顶\n", pass, len(cases))
	if pass < len(cases) {
		t.Errorf("并非所有精确串类型都能置顶（%d/%d）—— 文档里的「强」是过度概括",
			pass, len(cases))
	}
}
