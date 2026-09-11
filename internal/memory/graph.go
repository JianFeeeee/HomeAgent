package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Entity struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	MentionCount int       `json:"mention_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Relation struct {
	ID           int64     `json:"id"`
	SourceID     int64     `json:"source_id"`
	TargetID     int64     `json:"target_id"`
	SourceName   string    `json:"source_name"`
	TargetName   string    `json:"target_name"`
	RelationType string    `json:"relation_type"`
	Confidence   float64   `json:"confidence"`
	Status       string    `json:"status"`
	SessionID    string    `json:"session_id"`
	TurnID       int       `json:"turn_id"`
	CreatedAt    time.Time `json:"created_at"`
	DateBucket   string    `json:"date_bucket"`
	SentenceID   int64     `json:"sentence_id,omitempty"`   // FK → sentences.id
	SentenceText string    `json:"sentence_text,omitempty"` // JOINed from sentences
}

type Triple struct {
	Subject      string  `json:"subject"`
	Relation     string  `json:"relation"`
	Object       string  `json:"object"`
	Confidence   float64 `json:"confidence,omitempty"`
	SubjectType  string  `json:"subject_type,omitempty"`
	ObjectType   string  `json:"object_type,omitempty"`
	SentenceText string  `json:"sentence_text,omitempty"` // 原始句子文本，Commit时写入sentences表
	// MediaDigests 是该三元组显式携带的媒体 digest（完整或前缀）。
	// 媒体不再靠正文 marker 反解：结构化字段直接给出归属，
	// 由调用方（core）把它变成 L3 一等块并与句子建立结构边。
	MediaDigests []string `json:"media_digests,omitempty"`
}

type GraphDB struct {
	db     *sql.DB
	mu     sync.RWMutex
	dbPath string
}

func NewGraphDB(dbPath string) (*GraphDB, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open graph db: %w", err)
	}

	g := &GraphDB{db: db, dbPath: dbPath}
	if err := g.initSchema(); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return g, nil
}

func (g *GraphDB) initSchema() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	schemas := []string{
		`CREATE TABLE IF NOT EXISTS entities (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			type TEXT DEFAULT 'Concept',
			mention_count INTEGER DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS sentences (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			text TEXT UNIQUE NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS relations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER NOT NULL,
			target_id INTEGER NOT NULL,
			relation_type TEXT NOT NULL,
			confidence REAL DEFAULT 1.0,
			status TEXT DEFAULT 'active',
			session_id TEXT,
			turn_id INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			date_bucket TEXT,
			sentence_id INTEGER DEFAULT 0,
			FOREIGN KEY (source_id) REFERENCES entities(id),
			FOREIGN KEY (target_id) REFERENCES entities(id),
			UNIQUE(source_id, target_id, relation_type, session_id)
		)`,
		`CREATE TABLE IF NOT EXISTS memory_blocks (
			id TEXT PRIMARY KEY,
			modality TEXT NOT NULL,
			text_content TEXT DEFAULT '',
			payload_digest TEXT DEFAULT '',
			mime TEXT DEFAULT '',
			size INTEGER DEFAULT 0,
			width INTEGER DEFAULT 0,
			height INTEGER DEFAULT 0,
			vector TEXT DEFAULT '',
			fingerprint TEXT DEFAULT '',
			source TEXT DEFAULT '',
			tool TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS memory_block_edges (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_kind TEXT NOT NULL,
			source_id TEXT NOT NULL,
			target_kind TEXT NOT NULL,
			target_id TEXT NOT NULL,
			edge_type TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(source_kind, source_id, target_kind, target_id, edge_type)
		)`,
		`CREATE TABLE IF NOT EXISTS documents (
			id         TEXT PRIMARY KEY,
			summary    TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_blocks_modality ON memory_blocks(modality)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_blocks_digest ON memory_blocks(payload_digest)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_block_edges_source ON memory_block_edges(source_kind, source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_block_edges_target ON memory_block_edges(target_kind, target_id)`,
		`CREATE INDEX IF NOT EXISTS idx_entity_name ON entities(name)`,
		`CREATE INDEX IF NOT EXISTS idx_entity_type ON entities(type)`,
		`CREATE INDEX IF NOT EXISTS idx_relation_source ON relations(source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_relation_target ON relations(target_id)`,
		`CREATE INDEX IF NOT EXISTS idx_relation_type ON relations(relation_type)`,
		`CREATE INDEX IF NOT EXISTS idx_relation_status ON relations(status)`,
		`CREATE INDEX IF NOT EXISTS idx_relation_session ON relations(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sentences_text ON sentences(text)`,
	}

	for _, s := range schemas {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("schema exec: %w", err)
		}
	}

	// 迁移1：兼容旧版 sentence_ref 列（已有表则忽略）
	tx.Exec(`ALTER TABLE relations ADD COLUMN sentence_ref TEXT DEFAULT ''`)
	// 迁移2：为新表添加 sentence_id 列（必须放在索引创建之前，否则旧表无此列导致索引创建失败）
	tx.Exec(`ALTER TABLE relations ADD COLUMN sentence_id INTEGER DEFAULT 0`)
	// 迁移3：将现有 sentence_ref 数据迁移到 sentences 表
	tx.Exec(`INSERT OR IGNORE INTO sentences (text) SELECT DISTINCT sentence_ref FROM relations WHERE sentence_ref != ''`)
	tx.Exec(`UPDATE relations SET sentence_id = (SELECT id FROM sentences WHERE text = relations.sentence_ref) WHERE sentence_ref != ''`)

	// sentence_id 索引在迁移后创建，避免旧表缺少该列时失败
	tx.Exec(`CREATE INDEX IF NOT EXISTS idx_relation_sentence ON relations(sentence_id)`)

	// 迁移4：为旧版 relations 表（无复合唯一约束）重建表以去重。
	// 旧表由 2026-07 之前的版本创建，缺少 UNIQUE(source_id, target_id, relation_type, session_id)，
	// 生产库累积了海量重复关系。这里检查 sqlite_master 中已建表的 DDL，
	// 若不含该约束则走"新建带约束表 → INSERT OR IGNORE 拷贝去重 → 换名"的官方 12 步迁移。
	if err := g.migrateRelationUnique(tx); err != nil {
		return fmt.Errorf("migrate relations unique: %w", err)
	}

	return tx.Commit()
}

// migrateRelationUnique 检测 relations 表是否带复合唯一约束，缺失则重建去重。
// 必须在 initSchema 的同一个事务内调用（外键/索引均已存在时需先禁用外键再换名）。
func (g *GraphDB) migrateRelationUnique(tx *sql.Tx) error {
	var ddl string
	err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'relations'`).Scan(&ddl)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil // 表都不存在，无从迁移
		}
		return err
	}
	if strings.Contains(ddl, "UNIQUE") {
		return nil // 已是新 schema
	}

	stmt := []string{
		`ALTER TABLE relations RENAME TO relations_old`,
		`CREATE TABLE relations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER NOT NULL,
			target_id INTEGER NOT NULL,
			relation_type TEXT NOT NULL,
			confidence REAL DEFAULT 1.0,
			status TEXT DEFAULT 'active',
			session_id TEXT,
			turn_id INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			date_bucket TEXT,
			sentence_id INTEGER DEFAULT 0,
			sentence_ref TEXT DEFAULT '',
			FOREIGN KEY (source_id) REFERENCES entities(id),
			FOREIGN KEY (target_id) REFERENCES entities(id),
			UNIQUE(source_id, target_id, relation_type, session_id)
		)`,
		`INSERT OR IGNORE INTO relations (id, source_id, target_id, relation_type, confidence, status, session_id, turn_id, created_at, updated_at, date_bucket, sentence_id, sentence_ref)
		 SELECT id, source_id, target_id, relation_type, confidence, status, session_id, turn_id, created_at, updated_at, date_bucket, sentence_id, sentence_ref FROM relations_old`,
		`DROP TABLE relations_old`,
	}
	for _, s := range stmt {
		if _, err := tx.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// Commit 把三元组写入图库，返回新建的实体数与关系数。
func (g *GraphDB) Commit(triples []Triple, sessionID string, turnID int) (int, int, error) {
	_, ec, rc, err := g.commit(triples, sessionID, turnID, false)
	return ec, rc, err
}

// CommitWithMedia 与 Commit 相同，但额外返回每条句子文本对应的 sentences.id。
//
// 为何单独开一个方法而不改 Commit 的签名：Commit 有十个非测试调用点
// 加二十多个测试调用点，为了一个多数调用方都不需要的返回值去改全部签名
// 不划算。这里让 Commit 内部转调，两者共享同一份落库逻辑。
//
// 返回的 map 只包含本次真正写入了 sentences 表的句子。调用方据此把媒体
// 变成 L3 一等块，并以 sentence --contains--> block 边与句子相连；
// 关系行本身不持有媒体。
func (g *GraphDB) CommitWithMedia(triples []Triple, sessionID string, turnID int) (map[string]int64, int, int, error) {
	return g.commit(triples, sessionID, turnID, true)
}

func (g *GraphDB) commit(triples []Triple, sessionID string, turnID int, trackSentences bool) (map[string]int64, int, int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var sentenceIDs map[string]int64
	if trackSentences {
		sentenceIDs = make(map[string]int64)
	}

	tx, err := g.db.Begin()
	if err != nil {
		return nil, 0, 0, err
	}
	defer tx.Rollback()

	entitiesCreated := 0
	relationsCreated := 0
	dateBucket := time.Now().Format("2006-01-02")

	for _, t := range triples {
		if t.Subject == "" || t.Relation == "" || t.Object == "" {
			continue
		}
		if !validEntityName(t.Subject) || !validEntityName(t.Object) {
			continue
		}

		subjType := t.SubjectType
		if subjType == "" {
			subjType = "Concept"
		}
		objType := t.ObjectType
		if objType == "" {
			objType = "Concept"
		}
		confidence := t.Confidence
		if confidence == 0 {
			confidence = 1.0
		}

		ec, err := g.upsertEntity(tx, t.Subject, subjType)
		if err != nil {
			return nil, 0, 0, err
		}
		entitiesCreated += ec

		ec, err = g.upsertEntity(tx, t.Object, objType)
		if err != nil {
			return nil, 0, 0, err
		}
		entitiesCreated += ec

		var sourceID, targetID int64
		err = tx.QueryRow("SELECT id FROM entities WHERE name = ?", t.Subject).Scan(&sourceID)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("subject %q: %w", t.Subject, err)
		}
		err = tx.QueryRow("SELECT id FROM entities WHERE name = ?", t.Object).Scan(&targetID)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("object %q: %w", t.Object, err)
		}

		// 写入/查找句子
		var sentenceID int64
		if t.SentenceText != "" {
			_, err = tx.Exec(
				`INSERT OR IGNORE INTO sentences (text) VALUES (?)`, t.SentenceText)
			if err != nil {
				return nil, 0, 0, fmt.Errorf("insert sentence: %w", err)
			}
			err = tx.QueryRow("SELECT id FROM sentences WHERE text = ?", t.SentenceText).Scan(&sentenceID)
			if err != nil {
				sentenceID = 0
			} else if sentenceIDs != nil {
				sentenceIDs[t.SentenceText] = sentenceID
			}
		}

		var existing int
		err = tx.QueryRow(
			`SELECT 1 FROM relations WHERE source_id = ? AND target_id = ? AND relation_type = ? AND session_id = ?`,
			sourceID, targetID, t.Relation, sessionID,
		).Scan(&existing)
		if err == sql.ErrNoRows {
			_, err = tx.Exec(
				`INSERT INTO relations (source_id, target_id, relation_type, confidence, session_id, turn_id, date_bucket, sentence_id)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				sourceID, targetID, t.Relation, confidence, sessionID, turnID, dateBucket, sentenceID,
			)
			if err != nil {
				return nil, 0, 0, err
			}
			relationsCreated++
		} else if err != nil {
			return nil, 0, 0, err
		} else {
			// 同一(会话内)三元组已存在：仅刷新置信度与时间戳，不重复计数
			_, err = tx.Exec(
				`UPDATE relations SET confidence = ?, updated_at = CURRENT_TIMESTAMP
				 WHERE source_id = ? AND target_id = ? AND relation_type = ? AND session_id = ?`,
				confidence, sourceID, targetID, t.Relation, sessionID,
			)
			if err != nil {
				return nil, 0, 0, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, 0, err
	}

	return sentenceIDs, entitiesCreated, relationsCreated, nil
}

func validEntityName(name string) bool {
	if name == "" {
		return false
	}
	r := []rune(name)
	if len(r) < 2 || len(r) > 50 {
		return false
	}
	hasLetter := false
	for _, ch := range r {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '\u4e00' && ch <= '\u9fff') || ch == '-' || ch == '_' {
			hasLetter = true
		}
	}
	return hasLetter
}

func (g *GraphDB) upsertEntity(tx *sql.Tx, name string, entityType string) (int, error) {
	if !validEntityName(name) {
		return 0, nil
	}
	result, err := tx.Exec(
		`INSERT INTO entities (name, type) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		 	mention_count = mention_count + 1,
		 	updated_at = CURRENT_TIMESTAMP`,
		name, entityType,
	)
	if err != nil {
		return 0, err
	}
	rows, _ := result.RowsAffected()
	if rows > 0 {
		return 1, nil
	}
	return 0, nil
}

type RecallResult struct {
	Entities  []Entity   `json:"entities"`
	Relations []Relation `json:"relations"`
}

func (g *GraphDB) Recall(keywords []string, seedEntities []string, depth int, sessionFilter string) (*RecallResult, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	result := &RecallResult{}

	if len(keywords) == 0 && len(seedEntities) == 0 {
		rows, err := g.db.Query(
			`SELECT id, name, type, mention_count, created_at, updated_at
			 FROM entities ORDER BY mention_count DESC`,
		)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		for rows.Next() {
			var e Entity
			if err := rows.Scan(&e.ID, &e.Name, &e.Type, &e.MentionCount, &e.CreatedAt, &e.UpdatedAt); err != nil {
				return nil, err
			}
			result.Entities = append(result.Entities, e)
		}

		relRows, err := g.db.Query(
			`SELECT r.id, r.source_id, r.target_id, e1.name, e2.name,
					r.relation_type, r.confidence, r.status, r.session_id,
					r.turn_id, r.created_at, COALESCE(r.date_bucket, ''),
					COALESCE(r.sentence_id, 0), COALESCE(s.text, '')
			 FROM relations r
			 JOIN entities e1 ON r.source_id = e1.id
			 JOIN entities e2 ON r.target_id = e2.id
			 LEFT JOIN sentences s ON r.sentence_id = s.id
			 WHERE r.status = 'active'
			 ORDER BY r.created_at DESC LIMIT 30`,
		)
		if err != nil {
			return nil, err
		}
		defer relRows.Close()
		for relRows.Next() {
			var rel Relation
			if err := relRows.Scan(&rel.ID, &rel.SourceID, &rel.TargetID,
				&rel.SourceName, &rel.TargetName, &rel.RelationType,
				&rel.Confidence, &rel.Status, &rel.SessionID,
				&rel.TurnID, &rel.CreatedAt, &rel.DateBucket, &rel.SentenceID, &rel.SentenceText); err != nil {
				return nil, err
			}
			result.Relations = append(result.Relations, rel)
		}

		return result, nil
	}

	entityIDs := make(map[int64]bool)

	for _, kw := range keywords {
		rows, err := g.db.Query(
			`SELECT id, name, type, mention_count, created_at, updated_at
			 FROM entities WHERE LOWER(name) LIKE ?`,
			"%"+kw+"%",
		)
		if err != nil {
			return nil, err
		}

		for rows.Next() {
			var e Entity
			if err := rows.Scan(&e.ID, &e.Name, &e.Type, &e.MentionCount, &e.CreatedAt, &e.UpdatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			if !entityIDs[e.ID] {
				entityIDs[e.ID] = true
				result.Entities = append(result.Entities, e)
			}
		}
		rows.Close()
	}

	for _, se := range seedEntities {
		row := g.db.QueryRow(
			`SELECT id, name, type, mention_count, created_at, updated_at
			 FROM entities WHERE name = ?`, se)
		var e Entity
		if err := row.Scan(&e.ID, &e.Name, &e.Type, &e.MentionCount, &e.CreatedAt, &e.UpdatedAt); err == nil {
			if !entityIDs[e.ID] {
				entityIDs[e.ID] = true
				result.Entities = append(result.Entities, e)
			}
		}
	}

	if len(entityIDs) == 0 {
		return result, nil
	}

	for depthLevel := 0; depthLevel < depth; depthLevel++ {
		ids := make([]interface{}, 0, len(entityIDs))
		for id := range entityIDs {
			ids = append(ids, id)
		}

		if len(ids) == 0 {
			break
		}

		query := fmt.Sprintf(
			`SELECT r.id, r.source_id, r.target_id, e1.name, e2.name,
					r.relation_type, r.confidence, r.status, r.session_id,
					r.turn_id, r.created_at, COALESCE(r.date_bucket, ''),
					COALESCE(r.sentence_id, 0), COALESCE(s.text, '')
			 FROM relations r
			 JOIN entities e1 ON r.source_id = e1.id
			 JOIN entities e2 ON r.target_id = e2.id
			 LEFT JOIN sentences s ON r.sentence_id = s.id
			 WHERE (r.source_id IN (%s) OR r.target_id IN (%s))
			   AND r.status = 'active'`,
			placeholders(len(ids)),
			placeholders(len(ids)),
		)
		allIDs := append(ids, ids...)

		if sessionFilter != "" {
			query += " AND r.session_id = ?"
			allIDs = append(allIDs, sessionFilter)
		}

		relRows, err := g.db.Query(query, allIDs...)
		if err != nil {
			return nil, err
		}

		newIDs := make(map[int64]bool)
		for relRows.Next() {
			var rel Relation
			if err := relRows.Scan(&rel.ID, &rel.SourceID, &rel.TargetID,
				&rel.SourceName, &rel.TargetName, &rel.RelationType,
				&rel.Confidence, &rel.Status, &rel.SessionID,
				&rel.TurnID, &rel.CreatedAt, &rel.DateBucket, &rel.SentenceID, &rel.SentenceText); err != nil {
				relRows.Close()
				return nil, err
			}
			result.Relations = append(result.Relations, rel)

			if !entityIDs[rel.SourceID] {
				newIDs[rel.SourceID] = true
			}
			if !entityIDs[rel.TargetID] {
				newIDs[rel.TargetID] = true
			}
		}
		relRows.Close()

		if len(newIDs) == 0 {
			break
		}

		ids2 := make([]interface{}, 0, len(newIDs))
		for id := range newIDs {
			ids2 = append(ids2, id)
		}

		eRows, err := g.db.Query(
			fmt.Sprintf(
				`SELECT id, name, type, mention_count, created_at, updated_at
				 FROM entities WHERE id IN (%s)`, placeholders(len(ids2))),
			ids2...,
		)
		if err != nil {
			return nil, err
		}

		for eRows.Next() {
			var e Entity
			if err := eRows.Scan(&e.ID, &e.Name, &e.Type, &e.MentionCount, &e.CreatedAt, &e.UpdatedAt); err != nil {
				eRows.Close()
				return nil, err
			}
			if !entityIDs[e.ID] {
				entityIDs[e.ID] = true
				result.Entities = append(result.Entities, e)
			}
		}
		eRows.Close()

		for id := range newIDs {
			entityIDs[id] = true
		}
	}

	return result, nil
}

func (g *GraphDB) Purge(criteria map[string]string, mode string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	conds := []string{"status = 'active'"}
	args := []interface{}{}

	if v, ok := criteria["subject_contains"]; ok {
		rows, err := g.db.Query("SELECT id FROM entities WHERE name LIKE ?", "%"+v+"%")
		if err != nil {
			return 0, err
		}
		var ids []interface{}
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) > 0 {
			conds = append(conds, fmt.Sprintf("source_id IN (%s)", placeholders(len(ids))))
			args = append(args, ids...)
		}
	}

	if v, ok := criteria["target_contains"]; ok {
		rows, err := g.db.Query("SELECT id FROM entities WHERE name LIKE ?", "%"+v+"%")
		if err != nil {
			return 0, err
		}
		var ids []interface{}
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) > 0 {
			conds = append(conds, fmt.Sprintf("target_id IN (%s)", placeholders(len(ids))))
			args = append(args, ids...)
		}
	}

	if v, ok := criteria["relation_type"]; ok {
		conds = append(conds, "relation_type = ?")
		args = append(args, v)
	}

	if v, ok := criteria["session_id"]; ok {
		conds = append(conds, "session_id = ?")
		args = append(args, v)
	}

	if len(conds) == 1 {
		return 0, fmt.Errorf("no criteria provided")
	}

	where := ""
	for i, c := range conds {
		if i == 0 {
			where = c
		} else {
			where += " AND " + c
		}
	}

	if mode == "hard" {
		result, err := g.db.Exec(
			fmt.Sprintf(`DELETE FROM relations WHERE %s`, where), args...)
		if err != nil {
			return 0, err
		}
		n, _ := result.RowsAffected()

		g.db.Exec(`DELETE FROM entities WHERE id NOT IN (
			SELECT DISTINCT source_id FROM relations
			UNION SELECT DISTINCT target_id FROM relations)`)

		return int(n), nil
	}

	result, err := g.db.Exec(
		fmt.Sprintf(`UPDATE relations SET status = 'deleted', updated_at = CURRENT_TIMESTAMP WHERE %s`, where),
		args...,
	)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

func (g *GraphDB) GraphData() (map[string]interface{}, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	rows, err := g.db.Query(`SELECT id, name, type, mention_count, created_at, updated_at FROM entities ORDER BY mention_count DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type graphEntity struct {
		ID           int64     `json:"id"`
		Name         string    `json:"name"`
		Type         string    `json:"type"`
		MentionCount int       `json:"mention_count"`
		CreatedAt    time.Time `json:"created_at"`
		UpdatedAt    time.Time `json:"updated_at"`
	}
	var entities []graphEntity
	for rows.Next() {
		var e graphEntity
		if err := rows.Scan(&e.ID, &e.Name, &e.Type, &e.MentionCount, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entities = append(entities, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rrows, err := g.db.Query(`SELECT r.id, r.source_id, r.target_id, r.relation_type, r.confidence, r.status, r.created_at, COALESCE(r.sentence_id, 0), COALESCE(s.text, '') FROM relations r LEFT JOIN sentences s ON r.sentence_id = s.id WHERE r.status = 'active' ORDER BY r.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rrows.Close()

	type graphRelation struct {
		ID           int64     `json:"id"`
		SourceID     int64     `json:"source_id"`
		TargetID     int64     `json:"target_id"`
		RelationType string    `json:"relation_type"`
		Confidence   float64   `json:"confidence"`
		Status       string    `json:"status"`
		CreatedAt    time.Time `json:"created_at"`
		SentenceID   int64     `json:"sentence_id,omitempty"`
		SentenceText string    `json:"sentence_text,omitempty"`
	}
	var relations []graphRelation
	for rrows.Next() {
		var r graphRelation
		if err := rrows.Scan(&r.ID, &r.SourceID, &r.TargetID, &r.RelationType, &r.Confidence, &r.Status, &r.CreatedAt, &r.SentenceID, &r.SentenceText); err != nil {
			return nil, err
		}
		relations = append(relations, r)
	}
	if err := rrows.Err(); err != nil {
		return nil, err
	}

	brows, err := g.db.Query(`SELECT id, modality, text_content, payload_digest, mime,
		size, width, height, vector, fingerprint, source, tool, created_at, updated_at
		FROM memory_blocks ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer brows.Close()
	var blocks []MemoryBlock
	for brows.Next() {
		var block MemoryBlock
		var vectorJSON string
		if err := brows.Scan(&block.ID, &block.Modality, &block.Text, &block.PayloadDigest,
			&block.MIME, &block.Size, &block.Width, &block.Height, &vectorJSON,
			&block.Fingerprint, &block.Source, &block.Tool, &block.CreatedAt,
			&block.UpdatedAt); err != nil {
			return nil, err
		}
		if vectorJSON != "" && vectorJSON != "null" {
			if err := json.Unmarshal([]byte(vectorJSON), &block.Vector); err != nil {
				return nil, fmt.Errorf("decode memory block %s vector: %w", block.ID, err)
			}
		}
		blocks = append(blocks, block)
	}
	if err := brows.Err(); err != nil {
		return nil, err
	}

	berows, err := g.db.Query(`SELECT id, source_kind, source_id, target_kind, target_id,
		edge_type, created_at FROM memory_block_edges ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer berows.Close()
	var blockEdges []MemoryBlockEdge
	for berows.Next() {
		var edge MemoryBlockEdge
		if err := berows.Scan(&edge.ID, &edge.SourceKind, &edge.SourceID,
			&edge.TargetKind, &edge.TargetID, &edge.Type, &edge.CreatedAt); err != nil {
			return nil, err
		}
		blockEdges = append(blockEdges, edge)
	}
	if err := berows.Err(); err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"nodes":              entities,
		"edges":              relations,
		"memory_blocks":      blocks,
		"memory_block_edges": blockEdges,
	}, nil
}

func (g *GraphDB) Introspect() (map[string]interface{}, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var entityCount, relationCount int
	g.db.QueryRow("SELECT COUNT(*) FROM entities").Scan(&entityCount)
	g.db.QueryRow("SELECT COUNT(*) FROM relations WHERE status = 'active'").Scan(&relationCount)

	hotspots := []map[string]interface{}{}
	rows, err := g.db.Query(
		`SELECT name, mention_count, type FROM entities ORDER BY mention_count DESC LIMIT 10`,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name, etype string
			var count int
			if err := rows.Scan(&name, &count, &etype); err == nil {
				hotspots = append(hotspots, map[string]interface{}{
					"name": name, "count": count, "type": etype,
				})
			}
		}
	}

	return map[string]interface{}{
		"entity_count":    entityCount,
		"relation_count":  relationCount,
		"memory_hotspots": hotspots,
	}, nil
}

// MergeEntities 合并两个实体：将 sourceName 的所有信息合并到 targetName
// 1. sourceName 的所有关系重新指向 targetName
// 2. targetName 的 mention_count 增加 sourceName 的计数
// 3. sourceName 彻底删除（不再残留 @merged_ 实体）
// 返回 (关系的重定向数, error)
func (g *GraphDB) MergeEntities(sourceName, targetName string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	tx, err := g.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var sourceID, targetID int64
	var sourceCount, targetCount int

	err = tx.QueryRow("SELECT id, mention_count FROM entities WHERE name = ?", sourceName).Scan(&sourceID, &sourceCount)
	if err != nil {
		return 0, fmt.Errorf("source entity '%s' not found: %w", sourceName, err)
	}
	err = tx.QueryRow("SELECT id, mention_count FROM entities WHERE name = ?", targetName).Scan(&targetID, &targetCount)
	if err != nil {
		return 0, fmt.Errorf("target entity '%s' not found: %w", targetName, err)
	}

	if sourceID == targetID {
		return 0, fmt.Errorf("cannot merge entity with itself")
	}

	// 重定向 source → target 的活跃关系（作为 source）
	res, err := tx.Exec(
		`UPDATE relations SET source_id = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE source_id = ? AND status = 'active'`,
		targetID, sourceID,
	)
	if err != nil {
		return 0, err
	}
	redirectedSource, _ := res.RowsAffected()

	// 重定向 source → target 的活跃关系（作为 target）
	res, err = tx.Exec(
		`UPDATE relations SET target_id = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE target_id = ? AND status = 'active'`,
		targetID, sourceID,
	)
	if err != nil {
		return 0, err
	}
	redirectedTarget, _ := res.RowsAffected()

	// 删除可能产生的自引用关系
	_, err = tx.Exec(
		`DELETE FROM relations
		 WHERE source_id = target_id AND source_id = ?`,
		targetID,
	)
	if err != nil {
		return 0, err
	}

	// 清理 source 残留的非活跃关系（archived/deleted），否则外键约束阻止删除实体
	_, err = tx.Exec(`DELETE FROM relations WHERE source_id = ? OR target_id = ?`, sourceID, sourceID)
	if err != nil {
		return 0, err
	}

	// 更新 target 的 mention_count
	_, err = tx.Exec(
		`UPDATE entities SET mention_count = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		targetCount+sourceCount, targetID,
	)
	if err != nil {
		return 0, err
	}

	// 彻底删除 source 实体（所有关系已重定向，自引用已删除）
	_, err = tx.Exec(`DELETE FROM entities WHERE id = ?`, sourceID)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	total := int(redirectedSource + redirectedTarget)
	return total, nil
}

// DeleteEntity 彻底删除一个实体及其所有关联关系。
func (g *GraphDB) DeleteEntity(name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRow("SELECT id FROM entities WHERE name = ?", name).Scan(&id)
	if err != nil {
		return fmt.Errorf("entity '%s' not found: %w", name, err)
	}

	_, err = tx.Exec(`DELETE FROM relations WHERE source_id = ? OR target_id = ?`, id, id)
	if err != nil {
		return err
	}

	_, err = tx.Exec(`DELETE FROM entities WHERE id = ?`, id)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (g *GraphDB) Archive(days int) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	result, err := g.db.Exec(
		`UPDATE relations SET status = 'archived', updated_at = CURRENT_TIMESTAMP
		 WHERE status = 'active' AND created_at < datetime('now', ?)`,
		fmt.Sprintf("-%d days", days),
	)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// ClearSentenceID 清除指定关系的 sentence_id（LLM复审后解除句子引用）
func (g *GraphDB) ClearSentenceID(relationID int64) error {
	_, err := g.db.Exec(
		`UPDATE relations SET sentence_id = 0, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		relationID,
	)
	return err
}

// CleanupOrphanedSentences 删除既无关系引用、也无媒体块边的句子，返回删除数。
//
// 两个条件都必须看：旧媒体实体被迁移成原生块后，那些句子可能只靠
// sentence --contains--> block 存活，若只看 relations 引用就会被误删，
// 连带把块边变成悬空引用。
func (g *GraphDB) CleanupOrphanedSentences() (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	tx, err := g.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// 先清掉指向将被删除句子的块边，避免留下悬空端点。
	if _, err := tx.Exec(`DELETE FROM memory_block_edges
		WHERE source_kind = 'sentence' AND source_id NOT IN (
			SELECT CAST(id AS TEXT) FROM sentences
			WHERE id IN (SELECT DISTINCT sentence_id FROM relations WHERE sentence_id != 0)
			   OR id IN (SELECT CAST(source_id AS INTEGER) FROM memory_block_edges WHERE source_kind = 'sentence')
		)`); err != nil {
		return 0, err
	}

	res, err := tx.Exec(`DELETE FROM sentences WHERE id NOT IN (
			SELECT DISTINCT sentence_id FROM relations WHERE sentence_id != 0
		) AND id NOT IN (
			SELECT CAST(source_id AS INTEGER) FROM memory_block_edges WHERE source_kind = 'sentence'
		)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

func (g *GraphDB) Close() error {
	return g.db.Close()
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	b := make([]byte, 0, n*2-1)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}
