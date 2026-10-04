package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// 存量实体迁移为块节点（entities 退场第 3 步）。
//
// 为什么需要
// --------
// entities 是旧形态：只有 name/type/mention_count，没有向量；而块节点
// (memory_blocks) 带 vector+fingerprint/modality，是节点的正统形态。
// MigrateLegacyMediaEntities 已经把「媒体类实体」迁过一次（见 migrate.go），
// 本函数处理剩下的**文本类实体**——生产库实测 1277 个。
//
// 迁成什么形态
// ------------
//	sentence(实体名作为一句「陈述」) --contains--> block(实体名, 带向量)
//
// 关系则转成 block 边：
//	block(主语) -[关系类型]-> block(宾语)
//
// 为什么不拆字段：拆（LLM 三元组化）是**提升召回**的动作，与「换存储格式」
// 是两件事。混在一起做会让这次迁移既不可回滚也不可验证 —— 迁完不知道
// 召回变好还是变坏。拆分留给迁移后单独跑（有独立的探针判据）。
//
// 回滚
// ----
// 全程单事务，任何一步失败整体回滚：半途中断会留下既没有实体也没有块的
// 关系，信息静默消失（与 MigrateLegacyMediaEntities 同款考量）。
// 另：调用方应在迁移前自行快照 graph.db —— 本函数**不删数据**，
// 只在全部成功后由调用方决定是否清理旧表。

// MigrateResult 是存量迁移的统计。
type MigrateResult struct {
	Sentences     int `json:"sentences"`
	Blocks        int `json:"blocks"`
	Edges         int `json:"edges"`
	SkippedOrphan int `json:"skipped_orphan"`
	// DedupedEdges 是**被去重**的边数：relations 表里有完全重复的
	// (source,target,type) 三元组，边表按唯一键存 ⇒ 实际写入少于遍历数。
	// 必须显式报出来，否则「报告数 ≠ 实际写入数」会被当成数据丢失。
	DedupedEdges int `json:"deduped_edges"`
	SkippedNoVec int `json:"skipped_no_vector"`
}

// EntityEmbedder 为一个实体名算向量与空间指纹。
//
// 由调用方注入（持有 embedding provider 的一方）。返回 nil 向量表示
// 「这个实体算不出向量」——迁移**继续但不计为成功块**，不编造零向量。
type EntityEmbedder func(entityName string) (vec []float64, fingerprint string)

// MigrateLegacyTextEntities 把文本类实体迁移成块节点。
//
// embed 为 nil 时仍迁移（块不带向量，只是不参与向量召回）—— 这是可接受的
// 中间态：结构对了，向量可以后续回填（RecallBlocks 对无向量的块直接跳过）。// MigrateLegacyTextEntities 把文本类实体迁移成块节点。
//
// embed 为 nil 时仍迁移（块不带向量，只是不参与向量召回）—— 这是可接受的
// 中间态：结构对了，向量可以后续回填（RecallBlocks 对无向量的块直接跳过）。
//
// ★ 三个阶段，慢操作不进事务
// ----------------------------
//
//	阶段一 读快照（RLock，短）    取出全部实体与关系到内存
//	阶段二 算向量（无锁）          embed 回调可能很慢
//	阶段三 写事务落库（Lock）      句子 + 块 + 边
//
// 分阶段的原因：embed 是 ONNX 前向，实测 0.3~1s/条，生产库 1277 实体
// = 6~21 分钟。若在写事务内调用 embed，graph.db 会被锁住那么久 ——
// 它是单文件、所有记忆操作共用一把 g.mu，期间召回与写入全部阻塞。
// 第一版就是这么写的，症状是测试直接卡死 600s（测试在 embed 回调里
// 反过来拿锁，构造出真实场景的等价死锁）。
func (g *GraphDB) MigrateLegacyTextEntities(embed EntityEmbedder) (MigrateResult, error) {
	var res MigrateResult

	ents, rels, err := g.readLegacySnapshot()
	if err != nil {
		return res, err
	}

	// ── 阶段二：锁外算向量 ──
	vectors := make(map[int64][]float64, len(ents))
	fps := make(map[int64]string, len(ents))
	if embed != nil {
		for _, e := range ents {
			if vec, fp := embed(e.name); len(vec) > 0 {
				vectors[e.id] = vec
				fps[e.id] = fp
			} else {
				res.SkippedNoVec++
			}
		}
	} else {
		res.SkippedNoVec = len(ents)
	}

	// ── 阶段三：写事务落库 ──
	g.mu.Lock()
	defer g.mu.Unlock()

	tx, err := g.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	entToBlock := make(map[int64]string, len(ents))
	for _, e := range ents {
		// 实体名同时充当「句子」（陈述）与「块文本」：迁移期 1:1 对应，
		// 不做内容改写 —— 改写属于拆分，不属于迁移。
		// ★ 不再建 sentences 行（方案 A 退场）。
		//
		// 原来的 sentence 节点**没有独立价值**：它的 text 就是实体名，
		// 而下面那个块的 Text 也是 e.name —— 两者完全重复，
		// 而 sentences 表一退场这个节点就悬空了。
		//
		// 现在：原句块（blk_src_<hash>）--contains--> 迁移块
		// 与蒸馏路径（0586f6b）用同一套原句块形态。
		blockID := legacyEntityBlockID(e.id, e.name)
		src := NewSentenceBlock(e.name, e.createdAt, e.updatedAt)
		srcBlockID := src.ID
		if err := putBlockTx(tx, src); err != nil {
			return res, fmt.Errorf("put sentence block for entity %d: %w", e.id, err)
		}
		if err := putBlockTx(tx, MemoryBlock{
			ID:          blockID,
			Modality:    BlockText,
			Text:        e.name,
			Vector:      vectors[e.id],
			Fingerprint: fps[e.id],
			Source:      "legacy-entity",
			CreatedAt:   e.createdAt,
			UpdatedAt:   e.updatedAt,
		}); err != nil {
			return res, fmt.Errorf("put block for entity %d: %w", e.id, err)
		}
		if err, _ := addBlockEdgeTx(tx, "block", srcBlockID,
			"block", blockID, "contains"); err != nil {
			return res, fmt.Errorf("link block %s: %w", blockID, err)
		}
		entToBlock[e.id] = blockID
		// 注意：res.Sentences 不再递增 —— 统计口径改为「原句块」
		res.Blocks++
	}

	for _, r := range rels {
		src, ok1 := entToBlock[r.src]
		tgt, ok2 := entToBlock[r.tgt]
		if !ok1 || !ok2 {
			res.SkippedOrphan++
			continue
		}
		edgeType := strings.TrimSpace(r.typ)
		if edgeType == "" {
			// 空类型会被 addBlockEdgeTx 拒绝（端点与类型都必填）。
			// 用明确占位名而不是跳过 —— 关系的**存在**本身是信息。
			edgeType = "related_to"
		}
		// ★ 只在**真的新增**时计数 —— 否则报告会多算被去重的那些。
		//   实测：980 行 relations（含 20 组重复三元组）报告 980 而实际 959。
		err, added := addBlockEdgeTx(tx, "block", src, "block", tgt, edgeType)
		if err != nil {
			return res, fmt.Errorf("migrate relation %d: %w", r.id, err)
		}
		if !added {
			res.DedupedEdges++
			continue
		}
		res.Edges++
	}

	// ★ 为 sentences 表的原文补建原句块。
	//
	// 实测（清理前置判据，生产快照）：sentences 66 条，原句块 **0 个**。
	// 而迁移只从 `entities` 读（readLegacySnapshot 里只有 entities 查询），
	// 从没为 sentences 建过载体 ⇒ 直接清理该表会**丢 66 条原句**。
	//
	// 而 sentences 表的价值就在原文本身（entities 是提炼后的名字），
	// 所以原句块是它唯一的迁移出口 —— 不补这一段，清理就是数据丢失。
	//
	// 形态与迁移块的父节点一致：blk_src_<sha256(Text[:12])>，无向量。
	// 已有原句块（entities 迁移建的）会被 putBlockTx 的 ON CONFLICT 跳过，
	// 所以重复跑是幂等的。
	sentRows, err := tx.Query(`SELECT text FROM sentences
		WHERE text IS NOT NULL AND TRIM(text) != ''`)
	if err != nil {
		return res, fmt.Errorf("read sentences for migration: %w", err)
	}
	for sentRows.Next() {
		var text string
		if err := sentRows.Scan(&text); err != nil {
			_ = sentRows.Close()
			return res, err
		}
		sb := NewSentenceBlock(text, time.Time{}, time.Time{})
		if err := putBlockTx(tx, sb); err != nil {
			_ = sentRows.Close()
			return res, fmt.Errorf("put sentence block: %w", err)
		}
		res.Sentences++
	}
	if err := sentRows.Err(); err != nil {
		_ = sentRows.Close()
		return res, err
	}
	_ = sentRows.Close()

	if err := tx.Commit(); err != nil {
		return res, err
	}
	log.Printf("[graph] 存量实体迁移完成: 句子 %d，块 %d，边 %d"+
		"（跳过孤儿关系 %d，去重边 %d，无向量 %d）",
		res.Sentences, res.Blocks, res.Edges,
		res.SkippedOrphan, res.DedupedEdges, res.SkippedNoVec)
	return res, nil
}

type legacyEnt struct {
	id                   int64
	name                 string
	createdAt, updatedAt time.Time
}

// MemoryBlock 的时间戳来自源实体 —— 值覆盖维度（同一属性先后给两个值）
// 靠的就是时序。迁移时若统一写 NOW()，就把 188 个块的先后关系抹平成
// 同一时刻，`overwrite` 检索就退化成「新旧并列，取谁全靠向量相似度」——
// 实测 chineseclip 对新旧号短句给 0.9298 vs 0.9284，数值上根本区分不了。
type legacyRel struct {
	id, src, tgt int64
	typ          string
	createdAt    time.Time
}

// readLegacySnapshot 在短读锁内取全部实体与关系。
func (g *GraphDB) readLegacySnapshot() ([]legacyEnt, []legacyRel, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	rows, err := g.db.Query(`SELECT id, name,
		COALESCE(created_at, ''),
		COALESCE(updated_at, '')
		FROM entities
		WHERE name IS NOT NULL AND TRIM(name) != '' ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	var ents []legacyEnt
	for rows.Next() {
		var e legacyEnt
		var created, updated string
		if err := rows.Scan(&e.id, &e.name, &created, &updated); err != nil {
			rows.Close()
			return nil, nil, err
		}
		e.createdAt = parseLegacyTime(created)
		e.updatedAt = parseLegacyTime(updated)
		ents = append(ents, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	relRows, err := g.db.Query(`SELECT id, source_id, target_id,
		COALESCE(relation_type, '') FROM relations ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	var rels []legacyRel
	for relRows.Next() {
		var r legacyRel
		if err := relRows.Scan(&r.id, &r.src, &r.tgt, &r.typ); err != nil {
			relRows.Close()
			return nil, nil, err
		}
		rels = append(rels, r)
	}
	relRows.Close()
	return ents, rels, relRows.Err()
}

// legacyEntityBlockID 由**实体 id**派生块 id。
//
// 用 id 而非内容：实体的 name 是 UNIQUE 的，但迁移期同一句话可能既是实体
// 又是句子（下面第 3 步会把关系句子也建成块），用内容派生会撞。
// 实体 id 保证块 id 稳定且可重复迁移（幂等）。
func legacyEntityBlockID(entityID int64, name string) string {
	return fmt.Sprintf("blk_ent_%d_%s", entityID, shortHash(name))
}

func shortHash(s string) string {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%08x", uint32(h))
}

// legacyTimeLayouts 是旧库里时间列出现过的格式。
//
// entities.created_at 声明为 TIMESTAMP 但 SQLite 是弱类型：经由
// COALESCE 或历史写入路径存进去的可能是裸字符串，driver 会把它作为
// string 返回（直接扫进 time.Time 会报 unsupported Scan）。实测就是
// 卡在这里 —— 所以扫到 string 自己解析，而不是赌 driver 的类型推断。
var legacyTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05-07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// parseLegacyTime 解析旧库时间列；空值或无法识别时返回零值，
// 由 putBlockTx 兜底成 NOW()（宁可丢时序，也不能编造一个错的时刻）。
func parseLegacyTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range legacyTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	log.Printf("[graph] 旧库时间 %q 格式无法识别，该块将落为当前时间（时序信息丢失）", s)
	return time.Time{}
}

// ── 事务内工具（与 block.go 的公开方法同款语义，但共用同一个 tx）──

func ensureSentenceTx(tx *sql.Tx, text string) (int64, error) {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO sentences (text) VALUES (?)`, text); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(`SELECT id FROM sentences WHERE text = ?`, text).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func putBlockTx(tx *sql.Tx, b MemoryBlock) error {
	vectorJSON := ""
	if len(b.Vector) > 0 {
		raw, err := json.Marshal(b.Vector)
		if err != nil {
			return err
		}
		vectorJSON = string(raw)
	}
	// 时间戳显式写入（COALESCE 兜底 NOW()）：值覆盖维度依赖块间先后，
	// 全部落成同一时刻就等于把时序抹平。
	//
	// ★★ 但「零值」要区分两种语义（实测 Commit 块化后踩过）
	//
	//	① 迁移/蒸馏显式指定时间  → 用它
	//	② 调用方未指定（零值）    → 填 now，但**绝不覆盖已有时间**
	//
	// 第一版对②也用 now + 无条件 ON CONFLICT SET created_at，于是
	// Commit 块化之后：
	//
	//	Commit 写 blk_ent_<hash>（无时序）→ 填 now
	//	  → 迁移处理同名实体 → ON CONFLICT 覆盖 created_at
	//	  → 迁移块继承的实体时序被抹平
	//
	// 实测迁移测试直接抓到：
	//
	//	blk_ent_edb5a71b11d814fdc36e8332 应继承实体时间 2026-01-01 10:00
	//	实际 2026-10-04 08:05（时序被抹平）
	//
	// 而时序是**仲裁的前提**（arbitration.go 靠 CreatedAt 判断谁取代谁）。
	// ⇒ 零值时 UPDATE 分支**不碰时间列**。
	created := b.CreatedAt
	updated := b.UpdatedAt
	explicitTime := !created.IsZero()
	if !explicitTime {
		created = time.Now()
	}
	if updated.IsZero() {
		if explicitTime {
			updated = created
		} else {
			updated = created
		}
	}
	// 只有显式指定时间时才在 UPDATE 分支写时间列
	tsUpdate := "created_at = excluded.created_at, updated_at = excluded.updated_at"
	if !explicitTime {
		tsUpdate = "updated_at = memory_blocks.updated_at"
	}
	_, err := tx.Exec(`INSERT INTO memory_blocks
		(id, modality, text_content, payload_digest, mime, size, width, height,
		 vector, fingerprint, source, tool, scene, semantic_type, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			modality = excluded.modality,
			text_content = excluded.text_content,
			vector = excluded.vector,
			fingerprint = excluded.fingerprint,
			source = excluded.source,
			semantic_type = CASE WHEN excluded.semantic_type != ''
				THEN excluded.semantic_type ELSE memory_blocks.semantic_type END,
			`+tsUpdate,
		b.ID, b.Modality, b.Text, b.PayloadDigest, b.MIME, b.Size, b.Width, b.Height,
		vectorJSON, b.Fingerprint, b.Source, b.Tool, b.Scene, b.SemanticType,
		created, updated)
	return err
}

// ★ 返回 (error, 是否真的新增了一行)。第二个返回值是给报告口径用的 ——
//
//	`INSERT OR IGNORE` 静默去重时它为 false。
func addBlockEdgeTx(tx *sql.Tx, sourceKind, sourceID, targetKind, targetID, edgeType string) (error, bool) {
	// 端点存在性校验：与 AddMemoryBlockEdge 同款，但共用当前事务 ——
	// 分开校验会在并发下出现「校验通过后节点被删」的窗口。
	for _, ep := range []struct{ kind, id string }{
		{sourceKind, sourceID}, {targetKind, targetID}} {
		var n int
		var err error
		switch ep.kind {
		case "block":
			err = tx.QueryRow(`SELECT COUNT(*) FROM memory_blocks WHERE id = ?`, ep.id).Scan(&n)
		case "sentence":
			err = tx.QueryRow(`SELECT COUNT(*) FROM sentences WHERE CAST(id AS TEXT) = ?`, ep.id).Scan(&n)
		default:
			err = fmt.Errorf("invalid graph node kind %q", ep.kind)
		}
		if err != nil {
			return err, false
		}
		if n == 0 {
			return fmt.Errorf("%s graph node %s does not exist", ep.kind, ep.id), false
		}
	}
	// ★ 显式查重：边表升格去掉了 UNIQUE 约束（关系边要能并存多条），
	// 而 INSERT OR IGNORE 的去重正是靠那个 UNIQUE 实现的。
	// 不查重的话迁移跑第二遍会插出重复的 contains 边
	// （实测 TestMigrateLegacyTextEntities_幂等 当场红了）。
	var existing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM memory_block_edges
		WHERE source_kind=? AND source_id=? AND target_kind=? AND target_id=?
		  AND edge_type=? AND COALESCE(session_id,'')=''`,
		sourceKind, sourceID, targetKind, targetID, edgeType).Scan(&existing); err != nil {
		return err, false
	}
	if existing > 0 {
		return nil, false // 已存在：结构边幂等（不算新增）
	}

	res, err := tx.Exec(`INSERT INTO memory_block_edges
		(source_kind, source_id, target_kind, target_id, edge_type)
		VALUES (?, ?, ?, ?, ?)`, sourceKind, sourceID, targetKind, targetID, edgeType)
	if err != nil {
		return err, false
	}
	// ★ 返回「是否真的新增了一行」。
	//
	// `INSERT OR IGNORE` 在冲突时**既不报错也不新增**，调用方若只看
	// error 就会把「被去重」当成「写入成功」——
	// 生产快照实测：relations 980 行（含 20 组完全重复的三元组），
	// 报告写「边 980」而边表实际 959。
	n, err := res.RowsAffected()
	if err != nil {
		return err, false
	}
	return nil, n > 0
}

// LegacyEntity 是存量实体的一条只读快照。
type LegacyEntity struct {
	ID   int64
	Name string
}

// LegacyEntities 读出存量实体，供拆分/迁移命令使用。
//
// 只读，不删任何东西 —— 方案 A 里旧表的清理由调用方在验证通过后
// 单独执行（见 cmd/homed-graph-migrate 的说明）。
//
// limit <= 0 表示不限。分批是为了让拆分能增量推进（CPU 上 1.7b 模型
// 每条约 1~2 秒，全量 188 条要 3~6 分钟）。
//
// minLen > 0 时跳过过短的实体名：标题类实体（「order-gw 运维进展」「每天」
// 「老大」）本来就没有可拆字段，实测对它们跑拆分 8/8 全零字段 ——
// 那是正确结果，但会让报告看起来像「拆分坏了」。按长度预筛能把注意力
// 放在真有字段的记录上。
func (g *GraphDB) LegacyEntities(limit, minLen int) ([]LegacyEntity, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	query := `SELECT id, name FROM entities
		WHERE name IS NOT NULL AND TRIM(name) != ''`
	args := []any{}
	if minLen > 0 {
		query += ` AND LENGTH(name) >= ?`
		args = append(args, minLen)
	}
	query += ` ORDER BY id`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := g.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LegacyEntity
	for rows.Next() {
		var e LegacyEntity
		if err := rows.Scan(&e.ID, &e.Name); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountLegacyEntitiesByType 统计旧表 entities 里某类型的行数。
//
// ★ 用途：判据「迁移后旧表不该残留 X」。
//
// 读侧切块之后，Recall 返回的东西既可能是块也可能是旧表行，
// 而两者 Name/Type 可能完全一样 —— 光看召回结果分不出来。
// 于是判据必须**直查旧表**，而那是本包的私有 db。
//
// ★ 只读，无副作用。minLen=0 表示不限名长。
//
// ★ 它存在的另一理由：`LegacyEntity` 只带 ID/Name（不含 Type），
//
//	所以拿它做「按类型核查」并不够用。
func (g *GraphDB) CountLegacyEntitiesByType(entityType string) (int, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var n int
	if err := g.db.QueryRow(
		`SELECT COUNT(*) FROM entities WHERE type = ?`, entityType).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
