package memory

import (
	"fmt"
	"os"
	"testing"
)

func TestScore_生产规模分数分布(t *testing.T) {
	db := os.Getenv("PROD_SNAPSHOT")
	if db == "" {
		t.Skip("需要 PROD_SNAPSHOT")
	}
	g, err := NewGraphDB(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	ad, fp := probeEmbedder(t, "/var/tmp/ha-c/models/chinese-clip-vit-b16-onnx")

	for _, q := range []string{"本机 13010 端口对应什么", "agentmail 公网访问地址是什么"} {
		vec, err := ad.VectorizeDense(q)
		if err != nil {
			t.Fatal(err)
		}
		hits, err := g.RecallBlocks(BlockRecallQuery{Vector: vec, Fingerprint: fp, TopK: 8})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("  查询 %q → %d 命中\n", q, len(hits))
		for i, h := range hits {
			fmt.Printf("    %d. %.4f  %s\n", i+1, h.Score, truncStr(h.Block.Text, 46))
		}
		// 那条目标块在全库里的排名是多少
		target := "13010/13011 而非 12011"
		if q[:4] == "本机 " {
			all, _ := g.RecallBlocks(BlockRecallQuery{Vector: vec, Fingerprint: fp, TopK: 2000})
			for i, h := range all {
				if h.Block.Text == target {
					fmt.Printf("    ★ 目标块排名 = %d / %d（分数 %.4f）\n", i+1, len(all), h.Score)
					break
				}
			}
		}
	}
}

func truncStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
