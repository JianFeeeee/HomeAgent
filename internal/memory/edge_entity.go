package memory

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// ★★★ 关系边：独立单位
//
// ============================================================
// 为什么边必须是独立实体，而不是「两点 + 一个类型字符串」
// ============================================================
// 原 schema 有：
//
//	UNIQUE(source_kind, source_id, target_kind, target_id, edge_type)
//
// ⇒ 同一对节点间只能存**一条**同类型边。后果实测：
//
//	写 A --属于--> B (session=1, conf=0.9)
//	写 A --属于--> B (session=2, conf=0.5)
//	⇒ 第二条被 INSERT OR IGNORE 静默丢弃，session/confidence 全丢
//
//	而这正是「报告边数 980、实际写入 959」的根因（dd2c996）——
//	当时我把它当成「重复数据」报出来就完事了，
//	**真正的问题是边表从设计上就存不下多条同类边**。
//
// ============================================================
// 边作为独立单位的三条要求
// ============================================================
//	① 同类边可并存    —— 不同 session / turn / confidence 是不同的事实
//	② 边承载属性      —— confidence / session_id / status 挂在边上而非关系上
//	③ 边有自己的身份  —— 可以被引用（merged_into 指向它）而不只是被遍历
//
// 对应旧 relations 表的列：confidence / status / session_id / turn_id /
// date_bucket。date_bucket 是索引优化而非语义，先不入。

// 边状态。
const (
	// EdgeActive 是有效边。软删除/仲裁靠它判断旧值是否作废
	// （与旧 relations.status='active' 语义一致）。
	EdgeActive = "active"
	// EdgeDeleted 是被取代/被删除的边。**保留而非物理删除** ——
	// 「旧号 4379 停用」本身是有信息量的（它解释了为什么现在打不通），
	// 与 arbitration.go 的 Superseded 同一套语义。
	EdgeDeleted = "deleted"
	// EdgeMerged 是源节点被合并后，其边重定向到目标块；
	// 源块记 merged_into 指向目标块，历史边留在源块上。
	EdgeMerged = "merged"
)

// RelationEdgeData 是关系边的属性集。
type RelationEdgeData struct {
	// Confidence 是置信度（0~1）。旧 relations 表同名列。
	Confidence float64
	// SessionID / TurnID 记录边的来源会话与轮次。
	// Recall 的 sessionFilter 依赖它。
	SessionID string
	TurnID    int
	// Status 是 EdgeActive / EdgeDeleted / EdgeMerged。
	Status string
	// MergedInto 在 EdgeMerged 时指向目标块 ID。
	MergedInto string
}

// AddRelationBlockEdge 写一条**带属性的关系边**。
//
// ★ 与 AddMemoryBlockEdge 的区别（后者保留给结构边）
//
//	AddMemoryBlockEdge      结构边（contains / 迁移关系）
//	                       —— 不并存多条，无属性，用 UNIQUE 保证幂等
//	AddRelationBlockEdge    关系边（主语--关系-->宾语）
//	                       —— ★ 可并存多条、承载属性
//
// 两者共用一张表（memory_block_edges），靠属性列是否为空区分，
// 但**唯一性约束不同**：结构边受 UNIQUE 保护，关系边不受。
func (g *GraphDB) AddRelationBlockEdge(sourceID, targetID, edgeType string,
	data RelationEdgeData) error {

	g.mu.Lock()
	defer g.mu.Unlock()

	if sourceID == "" || targetID == "" || edgeType == "" {
		return fmt.Errorf("edge: endpoints and type required")
	}
	// 端点必须存在 —— 沿用 AddMemoryBlockEdge 的校验，
	// 避免建出指向虚无节点的边。
	for _, id := range []string{sourceID, targetID} {
		var n int
		if err := g.db.QueryRow(
			`SELECT COUNT(*) FROM memory_blocks WHERE id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("edge: block %s does not exist", id)
		}
	}

	status := data.Status
	if status == "" {
		status = EdgeActive
	}

	// ★ 不带 OR IGNORE / 不受 UNIQUE 约束 ——
	//   这正是「边是独立单位」的落点：同一对节点可并存多条同类边。
	_, err := g.db.Exec(`INSERT INTO memory_block_edges
		(source_kind, source_id, target_kind, target_id, edge_type,
		 confidence, session_id, turn_id, status, merged_into)
		VALUES ('block', ?, 'block', ?, ?, ?, ?, ?, ?, ?)`,
		sourceID, targetID, edgeType, data.Confidence,
		nullIfEmpty(data.SessionID), data.TurnID, status,
		nullIfEmpty(data.MergedInto))
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// RelationEdgesBetween 取两个块之间的全部关系边（**不合并同类**）。
//
// 这是「边是独立单位」的直接体现：返回切片长度可以 > 1。
func (g *GraphDB) RelationEdgesBetween(sourceID, targetID string) ([]MemoryBlockEdge, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.relationEdgesTx(sourceID, targetID)
}

func (g *GraphDB) relationEdgesTx(sourceID, targetID string) ([]MemoryBlockEdge, error) {
	rows, err := g.db.Query(`SELECT id, source_kind, source_id, target_kind, target_id,
			edge_type, confidence, session_id, turn_id, status, merged_into
		FROM memory_block_edges
		WHERE source_kind='block' AND source_id=? AND target_kind='block' AND target_id=?
		ORDER BY confidence DESC, id ASC`, sourceID, targetID)
	if err != nil {
		return nil, err
	}
	return scanRelationEdges(rows)
}

// AllRelationEdges 返回全部关系边（供遍历/合并用）。
func (g *GraphDB) AllRelationEdges() ([]MemoryBlockEdge, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT id, source_kind, source_id, target_kind, target_id,
			edge_type, confidence, session_id, turn_id, status, merged_into
		FROM memory_block_edges ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanRelationEdges(rows)
}

func scanRelationEdges(rows *sql.Rows) ([]MemoryBlockEdge, error) {
	defer func() { _ = rows.Close() }()
	var out []MemoryBlockEdge
	for rows.Next() {
		var e MemoryBlockEdge
		var sess, merged sql.NullString
		var conf sql.NullFloat64
		var status sql.NullString
		var turn sql.NullInt64
		if err := rows.Scan(&e.ID, &e.SourceKind, &e.SourceID, &e.TargetKind, &e.TargetID,
			&e.Type, &conf, &sess, &turn, &status, &merged); err != nil {
			return nil, err
		}
		e.Confidence = conf.Float64
		e.SessionID = sess.String
		e.Status = status.String
		e.MergedInto = merged.String
		if turn.Valid {
			e.TurnID = int(turn.Int64)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ensureRelationEdgeUniqueness 处理**既有表**上的 UNIQUE 约束。
//
// ★ 为什么必须重建表
// ------------------
// SQLite 的 UNIQUE 约束是**表定义的一部分**，没有 ALTER TABLE ... DROP CONSTRAINT。
// 而旧库（生产 98 块 / 590 边、测试库 448 块）建表时都带了这个约束，
// 它让「边是独立单位」在既有库上无法生效 —— 同一对节点写第二条同类边会被丢。
//
// 做法：读出既有边 → 建新表（无 UNIQUE）→ 搬回去 → 删旧表 → 改名。
// 全程在一个事务里；任何一步失败整体回滚。
//
// ★ 为什么不能在 detect 阶段默默跳过
//
// 跳过会让「既有库不支持并存同类边、新库支持」成为事实，
// 而这种不一致极难排查（本地能测、生产静默丢边）。
// ⇒ 这里宁可重建表也要保证语义一致，并打日志说明发生了什么。
func ensureRelationEdgeUniqueness(tx *sql.Tx) {
	// 检测既有表是否带 UNIQUE
	rows, err := tx.Query(`SELECT sql FROM sqlite_master
		WHERE type='table' AND name='memory_block_edges'`)
	if err != nil {
		return
	}
	var ddl string
	if rows.Next() {
		_ = rows.Scan(&ddl)
	}
	_ = rows.Close()
	if ddl == "" || !strings.Contains(ddl, "UNIQUE") {
		return // 新表或已升格
	}

	log.Printf("[graph] 边表升格：重建 memory_block_edges（移除 UNIQUE 以支持同类边并存）")
	if _, err := tx.Exec(`CREATE TABLE memory_block_edges_new (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_kind TEXT NOT NULL,
		source_id TEXT NOT NULL,
		target_kind TEXT NOT NULL,
		target_id TEXT NOT NULL,
		edge_type TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		confidence REAL DEFAULT 0,
		session_id TEXT DEFAULT '',
		turn_id INTEGER DEFAULT 0,
		status TEXT DEFAULT '',
		merged_into TEXT DEFAULT ''
	)`); err != nil {
		log.Printf("[graph] 边表升格失败（保留原约束）: %v", err)
		return
	}
	// 搬数据（只搬旧表已有的 6 列，新列留默认值）
	if _, err := tx.Exec(`INSERT INTO memory_block_edges_new
		(id, source_kind, source_id, target_kind, target_id, edge_type, created_at)
		SELECT id, source_kind, source_id, target_kind, target_id, edge_type, created_at
		FROM memory_block_edges`); err != nil {
		log.Printf("[graph] 边表数据迁移失败，回滚: %v", err)
		if _, derr := tx.Exec(`DROP TABLE memory_block_edges_new`); derr != nil {
			log.Printf("[graph] 边表升格：回滚临时表失败: %v", derr)
		}
		return
	}
	if _, err := tx.Exec(`DROP TABLE memory_block_edges`); err != nil {
		log.Printf("[graph] 边表删除失败，回滚: %v", err)
		if _, derr := tx.Exec(`DROP TABLE memory_block_edges_new`); derr != nil {
			log.Printf("[graph] 边表升格：回滚临时表失败: %v", derr)
		}
		return
	}
	if _, err := tx.Exec(`ALTER TABLE memory_block_edges_new RENAME TO memory_block_edges`); err != nil {
		log.Printf("[graph] 边表改名失败: %v", err)
		return
	}
	// 索引要重建（DROP TABLE 会带走）
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_memory_block_edges_source
			ON memory_block_edges(source_kind, source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_block_edges_target
			ON memory_block_edges(target_kind, target_id)`,
	} {
		_, _ = tx.Exec(idx)
	}
	log.Printf("[graph] 边表升格完成")
}

// BFSBlocks 从 startID 出发，沿**关系边**展开 depth 层，返回全部可达节点。
//
// ★★★ 这就是「联想」的实现（docs/zh/recall-as-association.md）
//
// 人听到「值班室分机号是多少」时：想起主语 → 想起值 → 想起旧值 → 想起人。
// 节点是召回对象，N 层 BFS 是把「自动附带的上下文」取回来。
//
// ★ 为什么必须是「节点 + BFS」而不是「只召回主语块」
//
//	宾语块 "4324" 孤立召回时不知道自己来自哪里。
//	若只召回主语块 ⇒ 要么丢它（漏掉「用户直接问值」的场景），
//	要么加「来源标注」特判（联想本来就不需要标注）。
//
// ★ 双向遍历
//
//	人联想到「老周值班」是从「老周 --值班分机--> 4324」**反向**走来的。
//	所以正反向都要展开，否则网会退化成有向森林。
//
// ★ depth=0 只返回自身（不是空）
//
//	命中节点本身就是要返回的内容之一。
func (g *GraphDB) BFSBlocks(startID string, depth int) ([]MemoryBlock, error) {
	if depth < 0 {
		depth = 0
	}
	if startID == "" {
		return nil, fmt.Errorf("bfs: empty start id")
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	// 起点必须存在
	var exists int
	if err := g.db.QueryRow(
		`SELECT COUNT(*) FROM memory_blocks WHERE id = ?`, startID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, fmt.Errorf("bfs: start block %s does not exist", startID)
	}

	// visited 同时充当去重集合：网会织成"部分连通"，
	// 不去重会在环上无限展开。
	visited := map[string]bool{startID: true}
	frontier := []string{startID}

	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []string
		for _, cur := range frontier {
			// ★ 正向 + 反向：前驱（谁指向我）与后继（我指向谁）都是邻居。
			//
			// ★★ 这里**不能**加 COALESCE(session_id,'')='' 的过滤 ——
			//   那个条件是给结构边去重用的（结构边 session_id 恒空），
			//   而关系边的 session_id 恰恰**非空**（它是边的属性）。
			//   加上它会把所有关系边滤掉 ⇒ BFS 一个邻居都走不到。
			//   实测：边写对了（session=s1 status=active）但 BFS 只返回起点。
			rows, err := g.db.Query(`SELECT target_id FROM memory_block_edges
				WHERE source_kind='block' AND source_id=?
				UNION
				SELECT source_id FROM memory_block_edges
				WHERE target_kind='block' AND target_id=?`,
				cur, cur)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var nb string
				if err := rows.Scan(&nb); err != nil {
					rows.Close()
					return nil, err
				}
				if nb != "" && !visited[nb] {
					visited[nb] = true
					next = append(next, nb)
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
		}
		frontier = next
	}

	// 取块内容（按 visited 顺序返回：起点在前，邻居按发现顺序）
	ids := make([]string, 0, len(visited))
	for id := range visited {
		ids = append(ids, id)
	}
	return g.blocksByIDsLocked(ids)
}

func (g *GraphDB) blocksByIDsLocked(ids []string) ([]MemoryBlock, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := g.db.Query(`SELECT id, modality, text_content, payload_digest,
		mime, size, width, height, vector, fingerprint, source, tool, scene,
		created_at, updated_at
		FROM memory_blocks WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryBlock
	for rows.Next() {
		b, err := scanBlockRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BlockNeighbour 是一条关系边连同它的对端块 —— 「谁和谁有什么关系」的完整读法。
//
// ★ 为什么不复用 RelationEdgeData：那个类型只是**属性载荷**
//
//	（confidence/session/turn/status/merged_into），刻意不含 ID 与端点，
//	因为 AddRelationBlockEdge 的入参就是「端点已知、只传属性」。
//
//	读的方向相反：要端点、要 ID。所以另立类型而不是硬塞进去。
type BlockNeighbour struct {
	// EdgeID 是 memory_block_edges.id。场景引用 kind='edge' 指向它。
	EdgeID int64
	// SourceID / TargetID 是两端块 ID。
	SourceID string
	TargetID string
	// EdgeType 是关系名（「偏好」「使用」…）。
	EdgeType string
	// Peer 是**对端**块：SourceID == 查询块时为 Target，反之为 Source。
	// 查询块自己是起点，不需要重复带出。
	Peer MemoryBlock
	// IsOutgoing 标明方向：true 表示 Peer 是这条关系的宾语。
	// social 层区分「A 认识 B」与「B 认识 A」时必须看它。
	IsOutgoing bool

	RelationEdgeData
}
