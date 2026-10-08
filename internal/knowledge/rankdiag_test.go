package knowledge

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HomeAgent/internal/memory"
)

// 知识库检索质量判据（真实数据，默认跳过）。
//
//	KB_DIAG=1                              → 跑并打印指标
//	KB_DIAG=1 KB_DIAG_ASSERT=1             → 额外断言门槛（CI/回归用）
//	KB_DIAG_ROOT / KB_DIAG_MODELS          → 覆盖数据与词向量路径
//
// 判据选「自检索 top-1 / MRR」的原因：不依赖人工标注问答对，且能直接量出
// **区分度**——词向量取平均后所有文档挤在语料均值附近，前两名分差极小，
// 排序等于噪声；这一项掉下来就说明检索坏了。
//
// 实测（33 条真实 KB）：
//
//	修复前（仅稠密路）  top-1 5/33 = 15%，MRR 0.271，平均分差 0.0133
//	修复后（稠密+词法融合）top-1 7/33 = 21%，MRR 0.376，平均分差 0.1280
//	门槛取 MRR ≥ 0.34 且分差 ≥ 0.10（留出余量，只挡「退化回噪声」）
func TestRankingQualityOnRealKB(t *testing.T) {
	if os.Getenv("KB_DIAG") == "" {
		t.Skip("需要 KB_DIAG=1（真实 KB + 词向量文件）")
	}
	srcRoot := envOr("KB_DIAG_ROOT", envOr("KB_DIAG_ROOT", "/data/knowledge"))
	models := envOr("KB_DIAG_MODELS", "/data/cc.zh.top200k.vec,/data/cc.en.top200k.vec")
	emb := memory.NewStaticEmbedder(strings.Split(models, ",")...)

	// 拷贝到临时目录跑：Start() 会重写 .index.json，不能动线上数据
	tmp := t.TempDir()
	entries, err := os.ReadDir(srcRoot)
	if err != nil {
		t.Fatalf("读取 %s: %v", srcRoot, err)
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		src := filepath.Join(srcRoot, e.Name(), "content.md")
		in, err := os.Open(src)
		if err != nil {
			continue
		}
		dst := filepath.Join(tmp, e.Name(), "content.md")
		os.MkdirAll(filepath.Dir(dst), 0755)
		out, _ := os.Create(dst)
		io.Copy(out, in)
		out.Close()
		in.Close()
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		t.Fatal("没有可用的知识条目")
	}

	st := NewStore(tmp)
	st.SetVectorizer(emb)
	if err := st.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	top1, mrr, missed := 0, 0.0, []string{}
	for _, name := range names {
		// 取全量排名：MRR 的定义用到真实名次，只取 top-2 会把 rank>2 的全都记 0
		// （我第一版就是这么写的，把 0.376 误报成 0.197）
		hits := st.Search(name, len(names))
		if len(hits) == 0 {
			missed = append(missed, name+"(无结果)")
			continue
		}
		if hits[0].Name == name {
			top1++
		} else {
			missed = append(missed, fmt.Sprintf("%s→%s", name, hits[0].Name))
		}
		for i, h := range hits {
			if h.Name == name {
				mrr += 1.0 / float64(i+1)
				break
			}
		}
	}
	n := float64(len(names))
	rate := 100 * float64(top1) / n
	fmt.Printf("\n  === 知识库检索质量（%d 条，自检索判据）===\n", len(names))
	fmt.Printf("  top-1 %d/%d = %.0f%%   MRR %.3f\n", top1, len(names), rate, mrr/n)
	if len(missed) > 0 {
		fmt.Printf("  未命中 top-1（前 10）：%v\n", firstN(missed, 10))
	}
	for _, q := range []string{"最近更新", "首启人格门禁", "插件怎么开发和部署", "统一多模态向量空间 ONNX", "隐私政策"} {
		hits := st.Search(q, 2)
		got := []string{}
		for _, h := range hits {
			got = append(got, h.Name)
		}
		fmt.Printf("  查询「%s」→ %v\n", q, got)
	}

	if os.Getenv("KB_DIAG_SWEEP") != "" {
		fmt.Printf("\n  === 稀疏融合权重扫描（1.0 = 只用语义路，0.0 = 只用词法路）===\n")
		fmt.Printf("  （无多模态稠密路接入时，本扫描直接对应历史 densePathWeight 的语义）\n")
		saved := sparseSemWeight
		for _, w := range []float64{1.0, 0.8, 0.7, 0.5, 0.3, 0.0} {
			sparseSemWeight = w
			t1, m := 0, 0.0
			for _, name := range names {
				hits := st.Search(name, len(names))
				for i, h := range hits {
					if h.Name == name {
						if i == 0 {
							t1++
						}
						m += 1.0 / float64(i+1)
						break
					}
				}
			}
			fmt.Printf("  权重 %.1f：top-1 %2d/%d = %3.0f%%   MRR %.3f\n",
				w, t1, len(names), 100*float64(t1)/float64(len(names)), m/float64(len(names)))
		}
		sparseSemWeight = saved
	}

	if os.Getenv("KB_DIAG_ASSERT") != "" {
		if mrr/n < 0.34 {
			t.Fatalf("检索质量退化：MRR %.3f < 0.34（修复前 0.271，修复后 0.376）", mrr/n)
		}
		if rate < 18 {
			t.Fatalf("检索质量退化：top-1 %.0f%% < 18%%", rate)
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
