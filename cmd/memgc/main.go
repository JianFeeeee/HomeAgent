// memgc 清理图记忆里已存在的「噪音实体」「孤立实体」及其关系。
//
// 为什么需要这个命令：噪音闸门（internal/memory.IsNoiseEntity）只能拦住
// **新写入**的噪音。旧库里那批（常用词 / 归档内部标记 / 模板摘要回声）是
// 闸门上线前攒下的存量，没人清就一直在——热实体被它们占着，召回预算被
// 同构垃圾边挤满。清理是一次性动作，但需要可重复执行、可先看不做。
//
// 两件事分开开关：-orphans 处理的是「零关系的空节点」（清理噪音后另一端
// 留下的壳），它们的名字本身可能没问题，但已经不在图里了。
//
// 用法（默认 dry-run，只列不删）：
//
//	memgc -db /home/newqqagent/memory/graph.db
//	memgc -db /home/newqqagent/memory/graph.db -orphans -apply
//
// 清理生产库前请先备份：sqlite3 graph.db ".backup 'graph.db.bak-<ts>'"
// 不要用 cp —— WAL 模式下会复制出主库与 -wal 不一致的快照。
package main

import (
	"flag"
	"fmt"
	"log"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
)

func main() {
	path := flag.String("db", "", "graph.db 路径（必填）")
	apply := flag.Bool("apply", false, "真正删除；不加则只 dry-run 打印")
	orphans := flag.Bool("orphans", false, "同时处理「零关系孤立实体」（先被清理的噪音在另一端留下的空节点）")
	flag.Parse()

	if *path == "" {
		flag.Usage()
		log.Fatal("memgc: 必须指定 -db")
	}

	g, err := memory.NewGraphDB(*path)
	if err != nil {
		log.Fatalf("memgc: open %s: %v", *path, err)
	}
	defer g.Close()

	junk, err := g.NoiseEntities()
	if err != nil {
		log.Fatalf("memgc: scan: %v", err)
	}

	fmt.Printf("噪音实体 %d 个：\n", len(junk))
	for _, e := range junk {
		fmt.Printf("  %-64s type=%-8s mentions=%d\n", e.Name, e.Type, e.MentionCount)
	}

	de, dr, err := g.PurgeNoise(!*apply)
	if err != nil {
		log.Fatalf("memgc: purge: %v", err)
	}
	if *apply {
		fmt.Printf("[APPLIED] 噪音：已删除 实体=%d 关系=%d\n", de, dr)
	} else {
		fmt.Printf("[DRY-RUN] 噪音：将删除 实体=%d 关系=%d（未写库，加 -apply 才落地）\n", de, dr)
	}

	if *orphans {
		list, err := g.OrphanEntities()
		if err != nil {
			log.Fatalf("memgc: orphans: %v", err)
		}
		fmt.Printf("孤立实体（零关系）%d 个：\n", len(list))
		for _, e := range list {
			fmt.Printf("  %-64s type=%-8s mentions=%d\n", e.Name, e.Type, e.MentionCount)
		}
		n, err := g.PurgeOrphans(!*apply)
		if err != nil {
			log.Fatalf("memgc: purge orphans: %v", err)
		}
		if *apply {
			fmt.Printf("[APPLIED] 孤立实体：已删除 %d 个\n", n)
		} else {
			fmt.Printf("[DRY-RUN] 孤立实体：将删除 %d 个\n", n)
		}
	}

	if *apply {
		fmt.Println("提示：运行中的进程会在下一个 archive 心跳（Indexer.Sync）重建实体名向量索引，无需重启。")
	}
}
