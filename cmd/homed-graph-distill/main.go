// Command homed-graph-distill 把存量实体跑字段拆分、落成块节点。
//
// 背景
// ------
// MigrateLegacyTextEntities 把 entities 迁成了块（a3e4c1c/09f31e2），但迁移
// 刻意**不做内容改写** —— 迁出来的块仍是整句：
//
//	「下周起值班室分机号改为 4324，旧号 4379 停用，值班轮换到 老周」
//
// 实测这种形态在值覆盖维度上答错（真实 chineseclip，真库 188 块）：
//
//	查询「值班室分机号是多少」→ top1 = 旧号 4379（score 0.8127）
//	                          新号 4324 那条进不了 top8
//
// 归因实验（改写形态重算向量）证实病根是**长复合句**把新旧值混在一句里：
//
//	现状·旧号（短句）    cos=0.8127
//	现状·新号（长复合句）cos=0.7327   ← 正确但排第二
//	改写·新号（短句）    cos=0.9284   ← 形态一变跃升 0.20
//
// 本命令做那一步改写：LLM 拆字段 → 「<主语>|<维度>=<值>」块 → 向量。
// 拆完的块才带得到仲裁（arbitration.go 需要 主语|维度=值 才解析得出）。
//
// 安全设计（与 homed-graph-migrate / homed-kb-migrate 同款）
// --------------------------------------------------------
//  1. **默认只报告**（-apply 才真写）。
//  2. 写入前自动快照 graph.db。
//  3. **不删任何东西**：整句块保留（它们是原始素材，回退路径）。
//     拆分块是新增的独立块（ID 由内容派生，重复跑幂等）。
//  4. 拆不动就记「零字段」并跳过，不编造。
//
// 用法：
//
//	homed-graph-distill -db /data/homeagent/memory/graph.db          # 报告
//	homed-graph-distill -db ... -apply                              # 拆分落块
//	homed-graph-distill -db ... -apply -limit 20                     # 先试 20 条
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/distill"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/embedding"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/generation"
	_ "gitcode.com/JianFeeeee/HomeAgent/providers/chineseclip"
	_ "gitcode.com/JianFeeeee/HomeAgent/providers/ollama"
)

func main() {
	dbPath := flag.String("db", "", "graph.db 路径（必填）")
	apply := flag.Bool("apply", false, "真正拆分落块（缺省只报告）")
	limit := flag.Int("limit", 0, "最多处理多少条实体，0 = 不限")
	// minLen 过滤掉太短的实体（标题类如「order-gw 运维进展」「每天」
	// 本来就没有可拆字段，跑它们只会浪费模型时间并让报告全是「零字段」）。
	//
	// ★ 实测（真库）：前 8 条实体全是这类短标题，拆分结果 8/8 零字段 ——
	// 那是**正确**结果，不是故障。不加这个过滤会误以为拆分坏了。
	minLen := flag.Int("min-len", 18, "实体名最小字符数，短于此的跳过")
	embedProvider := flag.String("embed-provider", "chineseclip", "向量 provider 名")
	modelDir := flag.String("model-dir", "", "向量 provider 的模型目录")
	genModel := flag.String("gen-model", "qwen3:1.7b", "拆分用的生成模型")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "错误：必须指定 -db <graph.db 路径>")
		os.Exit(2)
	}
	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "错误：打不开 %s: %v\n", *dbPath, err)
		os.Exit(2)
	}

	db, err := memory.NewGraphDB(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：打开图库失败: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	entities, err := loadEntities(db, *limit, *minLen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：读取实体失败: %v\n", err)
		os.Exit(1)
	}
	if len(entities) == 0 {
		fmt.Println("库里没有 entities，无需拆分。")
		return
	}

	stats, _ := db.BlockVectorStats()
	fmt.Printf("待处理实体 %d 条；现有块 %d（带向量 %d）\n", len(entities), stats.Total, stats.WithVector)

	// 打开两个 provider
	gen, err := generation.Open("ollama", generation.Config{
		Options: map[string]string{
			"model": *genModel, "num_thread": "10",
			"think": "false", "keep_alive": "30m",
		}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：打开生成 provider 失败: %v\n", err)
		os.Exit(1)
	}
	defer gen.Close()

	adapter, err := buildEmbedder(*embedProvider, *modelDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：打开向量 provider 失败: %v\n", err)
		os.Exit(1)
	}
	defer adapter.Close()
	embed := func(text string) ([]float64, string) {
		vec, err := adapter.VectorizeDense(text)
		if err != nil {
			return nil, ""
		}
		return vec, adapter.Fingerprint()
	}

	ex := distill.NewExtractor(gen, nil)

	// ── 干跑：先只统计，不写 ──
	type splitResult struct {
		fields int
		err    error
	}
	results := make([]splitResult, 0, len(entities))
	triples := 0
	zeroField := 0
	failed := 0
	for _, e := range entities {
		payload, err := ex.Blocks(context.Background(), e.name)
		if err != nil {
			results = append(results, splitResult{err: err})
			failed++
			continue
		}
		n := len(payload.Fields)
		results = append(results, splitResult{fields: n})
		if n == 0 {
			zeroField++
			continue
		}
		triples += n
		if len(entities) <= 8 || n != 0 {
			fmt.Printf("  %2d 字段  %.46s\n", n, e.name)
			for _, f := range payload.Fields {
				fmt.Printf("        %s|%s=%s\n", f.Subject, f.Dimension, f.Value)
			}
		}
	}

	fmt.Printf("\n拆解结果：%d 条实体 → %d 个字段块；零字段 %d 条，报错 %d 条\n",
		len(entities), triples, zeroField, failed)
	if !*apply {
		fmt.Println("\n这是报告模式（缺省）。加 -apply 真正落块。")
		fmt.Println("注意：整句块保留不动；拆分块是新增的独立块（内容派生 ID，重复跑幂等）。")
		return
	}

	// ── 快照 ──
	backup := fmt.Sprintf("%s.bak-distill-%s", *dbPath, time.Now().Format("20060102-150405"))
	if err := copyFile(*dbPath, backup); err != nil {
		fmt.Fprintf(os.Stderr, "错误：快照失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n已快照：%s\n", backup)

	// ── 落块 ──
	t0 := time.Now()
	written, zeroWritten, writeFailed := 0, 0, 0
	for i, e := range entities {
		r := results[i]
		if r.err != nil {
			writeFailed++
			continue
		}
		if r.fields == 0 {
			zeroWritten++
			continue
		}
		payload, err := ex.Blocks(context.Background(), e.name)
		if err != nil {
			writeFailed++
			continue
		}
		// ★ WritePayload 会保证「同一条原句 → 同 ID」的重试幂等
		n, err := distill.WritePayload(context.Background(), db, payload, embed)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  写入失败 %.40s: %v\n", e.name, err)
			writeFailed++
			continue
		}
		written += n
	}
	elapsed := time.Since(t0)

	after, _ := db.BlockVectorStats()
	fmt.Printf("\n落块完成（%.1fs）：写入字段块 %d，零字段跳过 %d，写入失败 %d\n",
		elapsed.Seconds(), written, zeroWritten, writeFailed)
	fmt.Printf("块总数 %d → %d（带向量 %d）\n", stats.Total, after.Total, after.WithVector)

	// 抽样展示落库后的块文本
	if b, err := db.MemoryBlocks(); err == nil && len(b) > 0 {
		fmt.Println("\n抽样块文本（应有 <主语>|<维度>=<值> 形态）：")
		shown := 0
		for _, x := range b {
			if x.Source == "distill" {
				fmt.Printf("  %s\n", x.Text)
				shown++
				if shown >= 5 {
					break
				}
			}
		}
	}
	fmt.Println("\n下一步：跑 memory_recall 端到端，对比仲裁前后的召回效果。")
}

type legacyEntity struct {
	id   int64
	name string
}

func loadEntities(db *memory.GraphDB, limit, minLen int) ([]legacyEntity, error) {
	out, err := db.LegacyEntities(limit, minLen)
	if err != nil {
		return nil, err
	}
	var res []legacyEntity
	for _, e := range out {
		res = append(res, legacyEntity{id: e.ID, name: e.Name})
	}
	return res, nil
}

func buildEmbedder(name, modelDir string) (*vector.ProviderAdapter, error) {
	opts := map[string]string{}
	if modelDir != "" {
		opts["model_dir"] = modelDir
	}
	p, err := embedding.Open(name, embedding.Config{Options: opts})
	if err != nil {
		return nil, err
	}
	adapted, err := vector.AdaptProvider(p)
	if err != nil {
		p.Close()
		return nil, err
	}
	return adapted, nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}
