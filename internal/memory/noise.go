package memory

import (
	"sort"
	"strings"
)

// ──────────────────────────────────────────────
// 图记忆「噪音实体」判定与清理
//
// 为什么需要这一层：doc→graph 蒸馏在 1cb3e87 从「CutExact 滑窗词链」换成
// NLP 依存提取器后，落库闸门只剩 validEntityName（长度 2–50、含字母/汉字）。
// validEntityName 挡的是「不像名字的字符串」，不挡「像名字的常用词」——
// 于是「结果 / 什么 / 哪个 / 待命 / 报告」这类词被反复写成实体，
// mention_count 冲到几百，度数却只有 1~2：它们占着热实体位、挤满召回预算，
// 却不带任何结构。CutExact（去停用词 + validEntityName + 去重）本可以做这层
// 过滤，但它当年只挂在 doc→graph 上，换提取器时被整条摘掉，如今只剩测试调用。
//
// 这里把判定收敛到一处，供三个地方共用：
//   - 自动填充路：docToTriples（冷文档归档）、extractKeyTriples（对话蒸馏）
//   - 历史数据清理：GraphDB.PurgeNoise（老库里的存量垃圾）
// ──────────────────────────────────────────────

// 模板摘要的判定片段：summarizeEntries 产出形如
// 「来自 N 个来源的 M 条对话 (src…) 涉及: kw…」。这类串只是「哪些词出现过」
// 的回声，没有独立信息量，写进图库会把「文档 --主题--> …」变成同构垃圾边。
// 判定与 agent/core.isTemplateSummary 保持一致。
const (
	templateSummaryPrefix = "来自 "
	templateSummaryMarker = "条对话"
)

// archivedContextSource 是归档上下文文档写死的 source 标记。
// 它是内部状态而非概念，不应以「来源」实体形式存在于图库中。
const archivedContextSource = "context_archived"

// IsNoiseEntity 判定一个实体名是否属于「不该进图记忆的噪音」。
//
// 判定刻意保守，只覆盖三类**客观**噪音，不做「这个词有没有信息量」的价值判断：
//  1. 停用词表命中的常用词（代词/助词/连词/介词/泛化名词/英文虚词）
//  2. 归档上下文内部标记（context_archived）
//  3. summarizeEntries 的模板摘要串
//
// 开放类词（「报告 / 对话 / 处理」这类真名词真动词）不在此列：它们在别的
// 语境下可能是有意义的实体，挡不挡属于领域决策，不该由这个函数替使用者拍板。
func IsNoiseEntity(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return true
	}
	if stopWords[n] {
		return true
	}
	if n == archivedContextSource {
		return true
	}
	return IsTemplateSummaryName(n)
}

// IsTemplateSummaryName 判断实体名是否为模板摘要串（供调用方单独使用）。
func IsTemplateSummaryName(name string) bool {
	n := strings.TrimSpace(name)
	return strings.HasPrefix(n, templateSummaryPrefix) && strings.Contains(n, templateSummaryMarker)
}

// FilterNoiseTriples 丢弃任一端为噪音实体的三元组，保持原顺序。
//
// 为什么在自动填充入口过滤、而不是在 Commit 里过滤：Commit 同时是
// memory_commit 工具（模型显式写入）的落库口。模型主动写「结果 --是--> X」
// 是它的自由（也可能它当时真的想记），而自动填充是无人在环的批量产出，
// 必须自己保证质量——闸门开在产生噪声的那一端。
func FilterNoiseTriples(triples []Triple) []Triple {
	if len(triples) == 0 {
		return triples
	}
	out := make([]Triple, 0, len(triples))
	for _, t := range triples {
		if IsNoiseEntity(t.Subject) || IsNoiseEntity(t.Object) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// NoiseEntities 列出库中现存的噪音实体，按 mention_count 降序。
func (g *GraphDB) NoiseEntities() ([]Entity, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.noiseEntitiesLocked()
}

func (g *GraphDB) noiseEntitiesLocked() ([]Entity, error) {
	rows, err := g.db.Query(
		`SELECT id, name, type, mention_count, created_at, updated_at FROM entities`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entity
	for rows.Next() {
		var e Entity
		if err := rows.Scan(&e.ID, &e.Name, &e.Type, &e.MentionCount, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		if IsNoiseEntity(e.Name) {
			out = append(out, e)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MentionCount != out[j].MentionCount {
			return out[i].MentionCount > out[j].MentionCount
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// PurgeNoise 清理库中已存在的噪音实体及其关系，返回删除的实体数与关系数。
//
// dryRun 为 true 时只统计、不写库——清理生产库前先看清楚要动什么。
// 关系按「任一端是噪音实体」删除；删完实体后顺带清理失去引用的孤儿句子
// （复用 cleanupOrphanedSentencesLocked，不在这里重写一遍判定）。
func (g *GraphDB) PurgeNoise(dryRun bool) (int, int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	junk, err := g.noiseEntitiesLocked()
	if err != nil {
		return 0, 0, err
	}
	if len(junk) == 0 {
		return 0, 0, nil
	}

	ids := make([]interface{}, 0, len(junk))
	for _, e := range junk {
		ids = append(ids, e.ID)
	}
	ph := placeholders(len(junk))
	args := append(append([]interface{}{}, ids...), ids...)

	// 关系数按 DISTINCT id 统计：两端都是噪音的关系不能被算两次。
	var relCount int
	if err := g.db.QueryRow(
		`SELECT COUNT(DISTINCT id) FROM relations
		 WHERE source_id IN (`+ph+`) OR target_id IN (`+ph+`)`,
		args...,
	).Scan(&relCount); err != nil {
		return 0, 0, err
	}

	if dryRun {
		return len(junk), relCount, nil
	}

	tx, err := g.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`DELETE FROM relations WHERE source_id IN (`+ph+`) OR target_id IN (`+ph+`)`,
		args...); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(
		`DELETE FROM entities WHERE id IN (`+ph+`)`, ids...); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}

	if _, err := g.cleanupOrphanedSentencesLocked(); err != nil {
		return len(junk), relCount, err
	}
	// 节点没了，场景引用必须跟着对齐：残留引用会让场景看着大、召回却是空的。
	if _, err := g.purgeStaleSceneRefsLocked(); err != nil {
		return len(junk), relCount, err
	}
	return len(junk), relCount, nil
}

// OrphanEntities 列出「没有任何关系的孤立实体」，按 mention_count 降序。
//
// 孤立实体在图里只剩一个名字：关系召回永远够不到它，唯一的副作用是
// 混进实体名向量索引、被自动注入当成相关实体。它们出现的典型路径是
// 噪音实体被清理后留下的另一端（边没了，节点还在），或提取器把关系写重了。
func (g *GraphDB) OrphanEntities() ([]Entity, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.orphanEntitiesLocked()
}

func (g *GraphDB) orphanEntitiesLocked() ([]Entity, error) {
	rows, err := g.db.Query(
		`SELECT id, name, type, mention_count, created_at, updated_at FROM entities e
		 WHERE NOT EXISTS (SELECT 1 FROM relations r WHERE r.source_id = e.id OR r.target_id = e.id)
		   -- 与媒体块有边的实体不算孤立：那是 sentence/document --contains--> block
		   -- 体系的一部分，删了会让块边悬空。
		   AND NOT EXISTS (SELECT 1 FROM memory_block_edges b
		        WHERE (b.source_kind = 'entity' AND b.source_id = CAST(e.id AS TEXT))
		           OR (b.target_kind = 'entity' AND b.target_id = CAST(e.id AS TEXT)))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entity
	for rows.Next() {
		var e Entity
		if err := rows.Scan(&e.ID, &e.Name, &e.Type, &e.MentionCount, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MentionCount != out[j].MentionCount {
			return out[i].MentionCount > out[j].MentionCount
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// PurgeOrphans 删除没有任何关系的孤立实体，返回删除数。
//
// dryRun 为 true 时只统计。与 PurgeNoise 分开：噪音是「这个名字本身不该在」，
// 孤立是「这个名字虽然可能合理，但它已经不在图里了」——两件事，别混在一个开关里。
func (g *GraphDB) PurgeOrphans(dryRun bool) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	orphans, err := g.orphanEntitiesLocked()
	if err != nil {
		return 0, err
	}
	if dryRun || len(orphans) == 0 {
		return len(orphans), nil
	}

	ids := make([]interface{}, 0, len(orphans))
	for _, e := range orphans {
		ids = append(ids, e.ID)
	}
	if _, err := g.db.Exec(
		`DELETE FROM entities WHERE id IN (`+placeholders(len(ids))+`)`, ids...); err != nil {
		return 0, err
	}
	if _, err := g.purgeStaleSceneRefsLocked(); err != nil {
		return 0, err
	}
	return len(orphans), nil
}
