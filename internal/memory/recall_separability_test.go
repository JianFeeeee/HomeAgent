package memory

import (
	"fmt"
	"os"
	"testing"
)

// TestScore_真实查询与编造查询的分数可分性
//
// 决定「拒答能力」能否靠阈值实现：
//   - 若真实查询 top1 明显高于编造查询 top1 → 加阈值即可
//   - 若两者分布重叠 → 阈值必然误伤，必须靠符号路/融合
func TestScore_真实查询与编造查询的分数可分性(t *testing.T) {
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

	real := []string{
		"脚本路径改到哪个目录了",
		"从零开发 QQ 插件用什么工具链",
		"agentmail 公网访问地址是什么",
		"本机 13010 端口对应什么",
	}
	fake := []string{
		"grafana 监控面板的端口是多少",
		"谁负责数据库容灾演练",
		"kafka 消息队列的 broker 地址是什么",
		"redis 集群的主从复制配置在哪",
	}

	fmt.Println("  ── 真实查询（库里确有相关块）")
	realTop := make([]float64, 0, len(real))
	for _, q := range real {
		vec, err := ad.VectorizeDense(q)
		if err != nil {
			t.Fatal(err)
		}
		hits, err := g.RecallBlocks(BlockRecallQuery{Vector: vec, Fingerprint: fp, TopK: 8})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) == 0 {
			fmt.Printf("    %-32s → 无命中\n", q)
			continue
		}
		realTop = append(realTop, hits[0].Score)
		fmt.Printf("    %-32s top1=%.4f  第8=%.4f  跨度=%.4f\n",
			q, hits[0].Score, hits[len(hits)-1].Score,
			hits[0].Score-hits[len(hits)-1].Score)
	}

	fmt.Println("  ── 编造查询（库里没有）")
	fakeTop := make([]float64, 0, len(fake))
	for _, q := range fake {
		vec, err := ad.VectorizeDense(q)
		if err != nil {
			t.Fatal(err)
		}
		hits, err := g.RecallBlocks(BlockRecallQuery{Vector: vec, Fingerprint: fp, TopK: 8})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) == 0 {
			fmt.Printf("    %-32s → 无命中（正确拒答）\n", q)
			continue
		}
		fakeTop = append(fakeTop, hits[0].Score)
		fmt.Printf("    %-32s top1=%.4f  第8=%.4f  跨度=%.4f\n",
			q, hits[0].Score, hits[len(hits)-1].Score,
			hits[0].Score-hits[len(hits)-1].Score)
	}

	if len(realTop) == 0 || len(fakeTop) == 0 {
		t.Skip("样本不足")
	}
	minReal, maxFake := realTop[0], fakeTop[0]
	for _, v := range realTop {
		if v < minReal {
			minReal = v
		}
	}
	for _, v := range fakeTop {
		if v > maxFake {
			maxFake = v
		}
	}
	fmt.Println()
	fmt.Printf("  真实查询 top1 最低 = %.4f\n", minReal)
	fmt.Printf("  编造查询 top1 最高 = %.4f\n", maxFake)
	if minReal > maxFake {
		fmt.Printf("  ★ 可分！阈值取 %.4f 即可拒答编造\n", (minReal+maxFake)/2)
	} else {
		fmt.Printf("  ★ 不可分 —— 真实查询最低(%.4f) < 编造最高(%.4f)，\n", minReal, maxFake)
		fmt.Println("    任何单一阈值都会误伤：要么放过编造，要么拒掉真实。")
		fmt.Println("    ⇒ 拒答不能靠阈值，必须靠符号路（精确串命中与否）。")
	}
}
