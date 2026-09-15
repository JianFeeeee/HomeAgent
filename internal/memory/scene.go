package memory

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// ──────────────────────────────────────────────
// 场景式关联召回
//
// 词法与向量召回都建立在「字面/语义相似」上，而带**条件**的记忆天生不吃这一套：
//
//	QQ回复禁用Markdown格式 --规定--> 纯文本不用Markdown
//	老大 --偏好--> QQ回复禁用Markdown格式
//
// 这些规则约束的是「回 QQ 消息这个场面」，而不是某个话题。用户措辞里没出现
// 「QQ」「Markdown」时它们召不回来；而用户只说了「QQ」时，词法路又会把上百个
// 含 QQ 的实体按建表顺序排前面，把规则本体挤出注入预算（实测：输入「QQ回复格式」
// 命中 148 个实体，规则排第 32，注入只取前 5 —— 规则根本没进去）。
//
// 场景引用把「触发条件」变成一等索引：节点记住自己属于哪个场面，
// 场面重现时按场景直接取回，与措辞无关。
//
// 场景键的形态是**分层字符串**，用 `/` 分隔，由宽到窄：
//
//	chan:qq                          通道级（在 QQ 上收发消息）
//	chan:qq/peer:group_1027993713    再窄一层（具体群）
//	tool:qq_get_message              工具级（取回消息正文这一步）
//	chan:doc/src:qq                  文档归档的来源
//
// 召回按**前缀**匹配：当前场景 `chan:qq` 会取回它自己以及所有更窄的场景
// （`chan:qq/...`）——越窄的场景越具体，不该被漏掉；反向不成立。
// ──────────────────────────────────────────────

// maxSceneKeyLen 是场景键的长度上限。场景键要进索引、要参与前缀比较，
// 过长只说明有人把正文塞进了键里（那就该用句子/实体，而不是场景）。
const maxSceneKeyLen = 96

// NormalizeSceneKey 规范化场景键：按 `/` 分层、每层小写、层内空白与非法字符
// 归一成 `_`（连续多个只留一个）。
//
// 为什么要归一：场景键是**索引键**，`chan:QQ` 与 `chan:qq` 必须是同一个场景，
// 否则同一条规则会因为写入时大小写不同而分裂成两个召不齐的场景。
// 为什么空白不算层级分隔符：来源名里天然带空格（如「老大2026-09-04 12:27
// QQ私聊图片」），把它当层级会把一个平面名字拆成三层伪层级。
// 归一结果为空（全是非法字符）时返回空串，调用方应视为「没有场景」。
func NormalizeSceneKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	key = strings.ToLower(key)

	parts := strings.Split(key, "/")
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		seg := normalizeSceneSegment(p)
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	out := strings.Join(segs, "/")
	if len(out) > maxSceneKeyLen {
		out = strings.TrimRight(out[:maxSceneKeyLen], "/")
	}
	return out
}

// normalizeSceneSegment 归一单层场景：小写、空白与非法字符 → `_`（压缩连续）。
func normalizeSceneSegment(seg string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r >= '\u4e00' && r <= '\u9fff',
			r == ':' || r == '-' || r == '.':
			b.WriteRune(r)
			lastUnderscore = false
		case r == '_' || unicode.IsSpace(r):
			if !lastUnderscore && b.Len() > 0 {
				b.WriteRune('_')
				lastUnderscore = true
			}
		default:
			// 其它字符（标点、表情）归一成 `_` 而不是静默丢弃：
			// 「A/B」与「A_B」是两个不同的来源，不能塌成一个场景。
			if !lastUnderscore && b.Len() > 0 {
				b.WriteRune('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// ChannelScene 由输入/输出通道名构造场景键（`qq` → `chan:qq`）。
func ChannelScene(source string) string {
	s := NormalizeSceneKey(source)
	if s == "" {
		return ""
	}
	return "chan:" + s
}

// ToolScene 由工具名构造场景键（`qq_get_message` → `tool:qq_get_message`）。
func ToolScene(tool string) string {
	s := NormalizeSceneKey(tool)
	if s == "" {
		return ""
	}
	return "tool:" + s
}

// SceneStat 是单个场景的规模摘要（供 introspection / 运维观察）。
type SceneStat struct {
	Key       string    `json:"key"`
	Refs      int       `json:"refs"`
	Relations int       `json:"relations"`
	Entities  int       `json:"entities"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SceneRecall 是一次场景召回的产物。
type SceneRecall struct {
	Scenes    []string   `json:"scenes"`
	Relations []Relation `json:"relations"`
	Entities  []Entity   `json:"entities"`
}

// tagSceneTx 在事务内把「关系 + 实体」挂到场景上（幂等 upsert）。
//
// weight 取关系的置信度：场景内的记忆也要能排序，置信度是目前唯一现成的
// 质量信号。重复写入同一节点只刷新 weight 与时间，不产生重复引用。
func tagSceneTx(tx *sql.Tx, sceneKey string, relationID int64, entityIDs []int64, weight float64) error {
	key := NormalizeSceneKey(sceneKey)
	if key == "" {
		return nil
	}
	if _, err := tx.Exec(
		`INSERT INTO scenes (key) VALUES (?)
		 ON CONFLICT(key) DO UPDATE SET updated_at = CURRENT_TIMESTAMP`, key); err != nil {
		return fmt.Errorf("upsert scene %q: %w", key, err)
	}
	var sceneID int64
	if err := tx.QueryRow(`SELECT id FROM scenes WHERE key = ?`, key).Scan(&sceneID); err != nil {
		return fmt.Errorf("select scene %q: %w", key, err)
	}
	if weight <= 0 {
		weight = 1.0
	}

	refs := make([]struct {
		kind string
		id   int64
	}, 0, len(entityIDs)+1)
	if relationID != 0 {
		refs = append(refs, struct {
			kind string
			id   int64
		}{"relation", relationID})
	}
	for _, eid := range entityIDs {
		if eid != 0 {
			refs = append(refs, struct {
				kind string
				id   int64
			}{"entity", eid})
		}
	}

	for _, r := range refs {
		if _, err := tx.Exec(
			`INSERT INTO scene_refs (scene_id, kind, ref_id, weight) VALUES (?, ?, ?, ?)
			 ON CONFLICT(scene_id, kind, ref_id)
			 DO UPDATE SET weight = MAX(weight, excluded.weight)`,
			sceneID, r.kind, r.id, weight); err != nil {
			return fmt.Errorf("upsert scene ref %s/%d: %w", r.kind, r.id, err)
		}
	}
	return nil
}

// TagScene 给一批已有的关系补挂场景（存量记忆的场景标注入口）。
//
// 为什么需要「事后标注」：场景是后引入的维度，此前写下的规则（那批 QQ 规则
// 就是典型）没有任何场景引用，不补挂就永远吃不到场景召回。
func (g *GraphDB) TagScene(sceneKey string, relationIDs []int64) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	key := NormalizeSceneKey(sceneKey)
	if key == "" || len(relationIDs) == 0 {
		return 0, nil
	}
	tx, err := g.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	n := 0
	for _, rid := range relationIDs {
		var sourceID, targetID int64
		var confidence float64
		if err := tx.QueryRow(
			`SELECT source_id, target_id, confidence FROM relations WHERE id = ?`, rid,
		).Scan(&sourceID, &targetID, &confidence); err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			return n, err
		}
		if err := tagSceneTx(tx, key, rid, []int64{sourceID, targetID}, confidence); err != nil {
			return n, err
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// TagSceneByEntityGlob 把「任一端实体名匹配 GLOB pattern」的活跃关系标进场景，
// 返回标注的关系数。dryRun 时只统计。
//
// 这是**存量引导**用的窄口子：pattern 由调用方显式给出，不做任何自动猜测
// ——猜错的代价是把无关记忆钉死在某个场景上，之后每次进入该场景都会被注入，
// 比漏标更难发现。
//
// 为什么用 GLOB 而不是 LIKE：LIKE 对 ASCII **不区分大小写**，于是 `%QQ%`
// 会把对象里带 `/home/newqqagent` 的路径类记忆（生产数据目录、email-mcp、
// dify-ops技能路径…实测 7 条）一起卷进「QQ 场景」。GLOB 区分大小写，
// `*QQ*` 只命中真正写作 QQ 的那些名字。
func (g *GraphDB) TagSceneByEntityGlob(sceneKey, pattern string, dryRun bool) (int, error) {
	key := NormalizeSceneKey(sceneKey)
	if key == "" || pattern == "" {
		return 0, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	rows, err := g.db.Query(
		`SELECT r.id, r.source_id, r.target_id, r.confidence
		 FROM relations r
		 JOIN entities e1 ON r.source_id = e1.id
		 JOIN entities e2 ON r.target_id = e2.id
		 WHERE r.status = 'active' AND (e1.name GLOB ? OR e2.name GLOB ?)`,
		pattern, pattern)
	if err != nil {
		return 0, err
	}
	type cand struct {
		relID            int64
		sourceID, target int64
		confidence       float64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.relID, &c.sourceID, &c.target, &c.confidence); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if dryRun {
		return len(cands), nil
	}

	tx, err := g.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, c := range cands {
		if err := tagSceneTx(tx, key, c.relID, []int64{c.sourceID, c.target}, c.confidence); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(cands), nil
}

// RecallByScene 按当前场景取回被钉在该场景上的记忆（前缀匹配，越窄越算命中）。
//
// 排序：weight（=写入时置信度）降序 → 关系时间降序。取回的是**关系全文**
// （含 relation_type 与 JOIN 出的原句），不只是实体名——带条件的规则本体
// 长在关系上，只给名字等于没召回。
//
// limit 同时约束关系数与实体数，避免一个场景把注入预算吃光。
func (g *GraphDB) RecallByScene(scenes []string, limit int) (*SceneRecall, error) {
	out := &SceneRecall{}
	var keys []string
	seen := make(map[string]bool)
	for _, s := range scenes {
		k := NormalizeSceneKey(s)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return out, nil
	}
	out.Scenes = keys
	if limit <= 0 {
		limit = 8
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	// 前缀条件：(key = ? OR key LIKE ? || '/%')，用 '/' 兜底防止
	// `chan:qq` 误吞 `chan:qq2` 这种同前缀但不同层的场景。
	conds := make([]string, 0, len(keys))
	args := make([]interface{}, 0, len(keys)*2)
	for _, k := range keys {
		conds = append(conds, `(s.key = ? OR s.key LIKE ? || '/%')`)
		args = append(args, k, k)
	}
	where := "(" + strings.Join(conds, " OR ") + ")"

	relQuery := `SELECT r.id, r.source_id, r.target_id, e1.name, e2.name,
			r.relation_type, r.confidence, r.status, r.session_id,
			r.turn_id, r.created_at, COALESCE(r.date_bucket, ''),
			COALESCE(r.sentence_id, 0), COALESCE(sn.text, ''),
			MAX(sr.weight) AS w
		FROM scene_refs sr
		JOIN scenes s ON sr.scene_id = s.id
		JOIN relations r ON sr.kind = 'relation' AND sr.ref_id = r.id
		JOIN entities e1 ON r.source_id = e1.id
		JOIN entities e2 ON r.target_id = e2.id
		LEFT JOIN sentences sn ON r.sentence_id = sn.id
		WHERE ` + where + ` AND r.status = 'active'
		GROUP BY r.id
		ORDER BY w DESC, r.updated_at DESC, r.id DESC
		LIMIT ?`
	relArgs := append(append([]interface{}{}, args...), limit)
	rows, err := g.db.Query(relQuery, relArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rel Relation
		var w float64
		if err := rows.Scan(&rel.ID, &rel.SourceID, &rel.TargetID, &rel.SourceName, &rel.TargetName,
			&rel.RelationType, &rel.Confidence, &rel.Status, &rel.SessionID,
			&rel.TurnID, &rel.CreatedAt, &rel.DateBucket, &rel.SentenceID, &rel.SentenceText, &w); err != nil {
			rows.Close()
			return nil, err
		}
		out.Relations = append(out.Relations, rel)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	entQuery := `SELECT e.id, e.name, e.type, e.mention_count, e.created_at, e.updated_at, MAX(sr.weight) AS w
		FROM scene_refs sr
		JOIN scenes s ON sr.scene_id = s.id
		JOIN entities e ON sr.kind = 'entity' AND sr.ref_id = e.id
		WHERE ` + where + `
		GROUP BY e.id
		ORDER BY w DESC, e.mention_count DESC, e.id DESC
		LIMIT ?`
	entArgs := append(append([]interface{}{}, args...), limit)
	erows, err := g.db.Query(entQuery, entArgs...)
	if err != nil {
		return nil, err
	}
	for erows.Next() {
		var e Entity
		var w float64
		if err := erows.Scan(&e.ID, &e.Name, &e.Type, &e.MentionCount, &e.CreatedAt, &e.UpdatedAt, &w); err != nil {
			erows.Close()
			return nil, err
		}
		out.Entities = append(out.Entities, e)
	}
	erows.Close()
	return out, erows.Err()
}

// SceneStats 返回各场景的规模，按引用数降序。
func (g *GraphDB) SceneStats() ([]SceneStat, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	rows, err := g.db.Query(
		`SELECT s.key,
		        COUNT(sr.id),
		        SUM(CASE WHEN sr.kind = 'relation' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN sr.kind = 'entity' THEN 1 ELSE 0 END),
		        s.updated_at
		 FROM scenes s LEFT JOIN scene_refs sr ON sr.scene_id = s.id
		 GROUP BY s.id ORDER BY COUNT(sr.id) DESC, s.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SceneStat
	for rows.Next() {
		var st SceneStat
		var rels, ents sql.NullInt64
		if err := rows.Scan(&st.Key, &st.Refs, &rels, &ents, &st.UpdatedAt); err != nil {
			return nil, err
		}
		st.Relations = int(rels.Int64)
		st.Entities = int(ents.Int64)
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Refs > out[j].Refs })
	return out, nil
}

// PurgeStaleSceneRefs 清理指向已不存在节点的场景引用，返回删除数。
//
// 节点被清理（PurgeNoise / PurgeOrphans / memory_delete_entity）时不会级联
// 删 scene_refs（见建表注释），残留引用会让场景看起来很大却召回出空结果，
// 也会让 SceneStats 说谎。这个函数把它们对齐。
func (g *GraphDB) PurgeStaleSceneRefs() (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.purgeStaleSceneRefsLocked()
}

func (g *GraphDB) purgeStaleSceneRefsLocked() (int, error) {
	res, err := g.db.Exec(`DELETE FROM scene_refs WHERE
		(kind = 'relation' AND ref_id NOT IN (SELECT id FROM relations))
		OR (kind = 'entity' AND ref_id NOT IN (SELECT id FROM entities))`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
