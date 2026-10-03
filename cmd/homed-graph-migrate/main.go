// Command homed-graph-migrate 把存量 entities 迁移成块节点（entities 退场第 3 步）。
//
// 背景
// ----
// entities 是旧形态：只有 name/type/mention_count，没有向量。块节点
// (memory_blocks) 带 vector/fingerprint/modality，是图节点的正统形态。
// MigrateLegacyMediaEntities 已经把媒体类实体迁过一次（internal/memory/migrate.go），
// 本命令处理剩下的文本类实体。
//
// 安全设计（与 homed-kb-migrate 同款）
// --------------------------------------
//  1. **默认只报告**（-apply 才真迁移）。迁移本身是单事务、全成功或全回滚，
//     但「迁完召回是否变好」只有跑过才知道。
//  2. **不删旧表**：迁移只加块与边，entities 原样保留。验证通过后由人工
//     另跑清理（下一步）。这样「效果不如预期」时的回滚就是什么都不做。
//  3. 迁移前自动快照 graph.db 到 <db>.bak-<时间戳>。
//  4. **向量是可选的**：-embed-provider 缺省则不带向量迁移（结构正确、
//     不参与向量召回）。可后续用 -backfill 单独回填，不必重跑迁移。
//
// 用法：
//
//	homed-graph-migrate -db /data/homeagent/memory/graph.db              # 报告
//	homed-graph-migrate -db ... -apply                                 # 迁移（不带向量）
//	homed-graph-migrate -db ... -apply -embed chineseclip -model-dir ...  # 带向量迁移
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/embedding"
	_ "gitcode.com/JianFeeeee/HomeAgent/providers/chineseclip"
	_ "gitcode.com/JianFeeeee/HomeAgent/providers/qwen3vl"
)

func main() {
	dbPath := flag.String("db", "", "graph.db 路径（必填）")
	apply := flag.Bool("apply", false, "真正迁移（缺省只报告）")
	provider := flag.String("embed-provider", "", "向量 provider 名（chineseclip / qwen3vl）；缺省则不带向量")
	modelDir := flag.String("model-dir", "", "provider 的模型目录")
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

	// ── 报告现状 ──
	before, err := legacyStats(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：读取现状失败: %v\n", err)
		os.Exit(1)
	}
	stats, err := db.BlockVectorStats()
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：读取块向量状态失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("迁移前：%s\n", before)
	fmt.Printf("        块 %d（其中带向量 %d），块边 %d\n",
		stats.Total, stats.WithVector, before.BlockEdges)
	if before.Entities == 0 {
		fmt.Println("\n库里没有 entities，无需迁移。")
		return
	}

	// ── 准备 embed（可选）──
	var embed memory.EntityEmbedder
	// embedFP 记下 provider 指纹：迁移后重建中心要用（见文件末尾说明）。
	var embedFP string
	if *provider != "" {
		adapter, err := buildEmbedder(*provider, *modelDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：打开 provider %q 失败: %v\n", *provider, err)
			fmt.Fprintln(os.Stderr, "（不带向量也能迁移：结构正确，向量可后续回填）")
			os.Exit(1)
		}
		defer adapter.Close()
		embed = func(name string) ([]float64, string) {
			vec, err := adapter.VectorizeDense(name)
			if err != nil {
				return nil, ""
			}
			return vec, adapter.Fingerprint()
		}
		embedFP = adapter.Fingerprint()
		fmt.Printf("向量 provider：%s（指纹 %s，维度 %d）\n",
			*provider, embedFP, adapter.Dim())
	} else {
		fmt.Println("向量 provider：未指定 → 块不带向量迁移（可后续回填）")
	}

	if !*apply {
		fmt.Printf("\n这是报告模式（缺省）。加 -apply 真正迁移。\n")
		fmt.Printf("预计产出：块 %d，块边 %d（实体名同时作为句子，句子--contains-->块）\n",
			before.Entities, before.Relations)
		// ★ 口径说明：块边数是**按 relations 全表**估计的，与迁移同口径。
		// 孤儿关系（两端实体已不存在）会被跳过，实际产出可能略少。
		fmt.Println("  （块边按 relations 全表计，孤儿关系会被跳过，实际可能略少）")
		fmt.Printf("旧表 entities/relations/sentences 保持不动，验证通过后再单独清理。\n")
		return
	}

	// ── 快照 ──
	backup := fmt.Sprintf("%s.bak-%s", *dbPath, time.Now().Format("20060102-150405"))
	if err := copyFile(*dbPath, backup); err != nil {
		fmt.Fprintf(os.Stderr, "错误：快照失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n已快照：%s\n", backup)

	// ── 执行 ──
	t0 := time.Now()
	res, err := db.MigrateLegacyTextEntities(embed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n迁移失败（已整体回滚）: %v\n", err)
		fmt.Fprintf(os.Stderr, "快照可用于人工核对：%s\n", backup)
		os.Exit(1)
	}

	after, err := legacyStats(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：读取迁移后状态失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n迁移完成（%.1fs）\n", time.Since(t0).Seconds())
	fmt.Printf("  句子 +%d，块 +%d，边 +%d\n", res.Sentences, res.Blocks, res.Edges)
	if res.SkippedOrphan > 0 {
		fmt.Printf("  跳过孤儿关系 %d 条（端点实体为空名；关系信息仍在句子里）\n", res.SkippedOrphan)
	}
	if res.SkippedNoVec > 0 {
		fmt.Printf("  无向量块 %d 个（可后续回填；本次迁移不影响它们按边查取）\n", res.SkippedNoVec)
	}
	fmt.Printf("\n迁移后：%s\n", after)

	// ★ 迁移后必须重建中心向量，否则召回没有区分度。
	//
	// 实测（生产快照 1294 实体 / 1391 块）：迁移后不建中心，
	// 召回分数挤成一团 —— 查询「本机 13010 端口」返回的 8 条
	// 全在 0.84~0.90，含精确串 13010 的目标块连 top2000 都进不去：
	//
	//	1. 0.9006  本机 443 按 SNI 透传到 192.168.2.106:3080
	//	2. 0.8712  ACP回环调用自身12001
	//	3. 0.8611  MAR/MDR与ALU不直通必须经CPU内部总线
	//
	// 而 RebuildCentroid 此前**只有测试在调用**，生产路径没有调用者
	// —— ha-c 的中心是手工测试时留下的，生产侧从来没有过。
	if embedFP != "" {
		if _, anomalous, err := db.RebuildCentroid(embedFP); err != nil {
			fmt.Fprintf(os.Stderr, "警告：重建中心失败（召回将无区分度）: %v\n", err)
		} else if anomalous {
			fmt.Println("已重建中心向量（检测到各向异性，已启用双边中心化）")
		} else {
			fmt.Println("已重建中心向量（各向异性不显著，仍保存以便后续块增多时复用）")
		}
	} else {
		fmt.Println("未指定向量 provider，跳过中心重建" +
			"（无向量时中心无意义；回填向量后请重跑本命令或手工 RebuildCentroid）")
	}

	fmt.Println("\n下一步：验证召回改善（memory_recall 的跨维度探针），确认后再清理旧表。")
}

// buildEmbedder 按名字打开向量 provider 并适配成 VectorizeDense 形态。
//
// 走 pkg/embedding 注册表（内核无感边界）：换模型只改 -embed-provider，
// 本命令不含任何 provider 名分支。
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

type legacyStat struct {
	Entities, Relations, Blocks, BlockEdges int
}

func (s legacyStat) String() string {
	return fmt.Sprintf("实体 %d，关系 %d，块 %d，块边 %d",
		s.Entities, s.Relations, s.Blocks, s.BlockEdges)
}

// legacyStats 汇总迁移前后的库规模。
//
// 复用既有的 Introspect（实体/关系统计）与 BlockVectorStats（块向量状态），
// 不新增统计 API：句数与块边数用只读查询补齐。
func legacyStats(db *memory.GraphDB) (legacyStat, error) {
	var s legacyStat
	intro, err := db.Introspect()
	if err != nil {
		return s, err
	}
	if v, ok := intro["entity_count"].(int); ok {
		s.Entities = v
	}
	if v, ok := intro["relation_count"].(int); ok {
		s.Relations = v
	}
	edges, err := db.MemoryBlockEdges()
	if err != nil {
		return s, err
	}
	s.BlockEdges = len(edges)
	bs, err := db.BlockVectorStats()
	if err != nil {
		return s, err
	}
	s.Blocks = bs.Total
	return s, nil
}

// countSentences 通过块边反推句子数不可靠（迁移前无块），
// 而 Introspect 不含句数 —— 迁移报告里句数不是关键指标，
// 因此不新增统计 API，只用实体/关系/块三个数（够判断迁移效果）。

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}
