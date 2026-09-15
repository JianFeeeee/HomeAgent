package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// BlockModality 是一等记忆块的原生模态。
type BlockModality string

const (
	BlockText  BlockModality = "text"
	BlockImage BlockModality = "image"
	BlockVideo BlockModality = "video"
	BlockAudio BlockModality = "audio"
)

// MemoryBlock 是 Context、Document、Graph 三层共同使用的记忆块值。
//
// 它不携带 Layer、Owner 或 RefCount：块当前由哪个层的容器持有，哪个层就是
// 唯一事实源。Context→Document→Graph 迁移的是这个值本身，不建立平行保活账本。
// PayloadDigest 仅用于定位内容寻址的原始字节，不表示另一条逻辑记忆。
type MemoryBlock struct {
	ID            string        `json:"id"`
	Modality      BlockModality `json:"modality"`
	Text          string        `json:"text,omitempty"`
	PayloadDigest string        `json:"payload_digest,omitempty"`
	MIME          string        `json:"mime,omitempty"`
	Size          int64         `json:"size,omitempty"`
	Width         int           `json:"width,omitempty"`
	Height        int           `json:"height,omitempty"`
	Vector        []float64     `json:"vector,omitempty"`
	Fingerprint   string        `json:"fingerprint,omitempty"`
	Source        string        `json:"source,omitempty"`
	Tool          string        `json:"tool,omitempty"`
	// Scene 是这个块所属的场景键（可空）。
	//
	// 块是记忆流水线里最细的「子项目」：一段转写、一张图的描述、一份附件。
	// 场景要贯穿到流水线底，就得从块开始——否则「QQ 那场对话里发过来的那张图」
	// 在场面重现时永远拿不回来。块进 L3 时按 Scene 挂 scene_refs(kind='block')，
	// 场景召回即可把它取回（见 GraphDB.RecallByScene）。
	Scene     string    `json:"scene,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MemoryBlockEdge 是 L3 中连接一等记忆节点的结构化语义边。
// source/target kind 当前允许 block、entity、sentence、document。
type MemoryBlockEdge struct {
	ID         int64     `json:"id"`
	SourceKind string    `json:"source_kind"`
	SourceID   string    `json:"source_id"`
	TargetKind string    `json:"target_kind"`
	TargetID   string    `json:"target_id"`
	Type       string    `json:"type"`
	CreatedAt  time.Time `json:"created_at"`
}

func validBlockModality(modality BlockModality) bool {
	return modality == BlockText || modality == BlockImage || modality == BlockVideo || modality == BlockAudio
}

// PutMemoryBlocks 将完成 L2→L3 迁移的块写成 GraphDB 原生节点。
// 调用方只有在本事务成功后才能从 Document 删除这些块。
func (g *GraphDB) PutMemoryBlocks(blocks []MemoryBlock) error {
	if len(blocks) == 0 {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, block := range blocks {
		if block.ID == "" {
			return fmt.Errorf("memory block id is required")
		}
		if !validBlockModality(block.Modality) {
			return fmt.Errorf("memory block %s has invalid modality %q", block.ID, block.Modality)
		}
		vectorJSON, err := json.Marshal(block.Vector)
		if err != nil {
			return fmt.Errorf("marshal memory block %s vector: %w", block.ID, err)
		}
		now := time.Now()
		if block.CreatedAt.IsZero() {
			block.CreatedAt = now
		}
		_, err = tx.Exec(`INSERT INTO memory_blocks (
			id, modality, text_content, payload_digest, mime, size, width, height,
			vector, fingerprint, source, tool, scene, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			modality = excluded.modality,
			text_content = excluded.text_content,
			payload_digest = excluded.payload_digest,
			mime = excluded.mime,
			size = excluded.size,
			width = excluded.width,
			height = excluded.height,
			vector = excluded.vector,
			fingerprint = excluded.fingerprint,
			source = excluded.source,
			tool = excluded.tool,
			-- 场景只在本次给了值时才覆盖：块可能先被写入、后被归档路径补挂场景，
			-- 反过来「已挂场景的块被一次无场景的重写抹掉」是不可接受的静默降级。
			scene = CASE WHEN excluded.scene != '' THEN excluded.scene ELSE memory_blocks.scene END,
			updated_at = excluded.updated_at`,
			block.ID, block.Modality, block.Text, block.PayloadDigest, block.MIME,
			block.Size, block.Width, block.Height, string(vectorJSON), block.Fingerprint,
			block.Source, block.Tool, block.Scene, block.CreatedAt, now)
		if err != nil {
			return fmt.Errorf("put memory block %s: %w", block.ID, err)
		}
		// 场景引用与块同事务：块写进去了、引用丢了，这个块在场景里就永远取不回。
		if block.Scene != "" {
			if err := tagSceneRefTx(tx, block.Scene, "block", 0, block.ID, 1.0); err != nil {
				return fmt.Errorf("tag scene for block %s: %w", block.ID, err)
			}
		}
	}
	return tx.Commit()
}

// PutDocumentNode 在 L3 登记一个文档节点，作为 document --contains--> block
// 结构边的端点。文档正文已蒸馏为实体/关系，这里只保留身份与摘要。
func (g *GraphDB) PutDocumentNode(id, summary string) error {
	if id == "" {
		return fmt.Errorf("document node id is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, err := g.db.Exec(`INSERT INTO documents (id, summary) VALUES (?, ?)
		ON CONFLICT(id) DO UPDATE SET summary = excluded.summary`, id, summary)
	return err
}

// MemoryBlocks 查询 Graph 层实际持有的一等记忆节点。
func (g *GraphDB) MemoryBlocks() ([]MemoryBlock, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT id, modality, text_content, payload_digest, mime,
		size, width, height, vector, fingerprint, source, tool, created_at, updated_at
		FROM memory_blocks ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blocks []MemoryBlock
	for rows.Next() {
		var block MemoryBlock
		var vectorJSON string
		if err := rows.Scan(&block.ID, &block.Modality, &block.Text, &block.PayloadDigest,
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
	return blocks, rows.Err()
}

func validGraphNodeKind(kind string) bool {
	return kind == "block" || kind == "entity" || kind == "sentence" || kind == "document"
}

func graphNodeExists(tx *sql.Tx, kind, id string) (bool, error) {
	var n int
	var err error
	switch kind {
	case "block":
		err = tx.QueryRow(`SELECT COUNT(*) FROM memory_blocks WHERE id = ?`, id).Scan(&n)
	case "entity":
		err = tx.QueryRow(`SELECT COUNT(*) FROM entities WHERE CAST(id AS TEXT) = ?`, id).Scan(&n)
	case "sentence":
		err = tx.QueryRow(`SELECT COUNT(*) FROM sentences WHERE CAST(id AS TEXT) = ?`, id).Scan(&n)
	case "document":
		err = tx.QueryRow(`SELECT COUNT(*) FROM documents WHERE id = ?`, id).Scan(&n)
	default:
		return false, fmt.Errorf("invalid graph node kind %q", kind)
	}
	return n == 1, err
}

// AddMemoryBlockEdge 建立 contains、depicts、derived_from 等原生图边。
// 端点必须是真实 Graph 节点，不能用 owner 字符串伪装关系。
func (g *GraphDB) AddMemoryBlockEdge(sourceKind, sourceID, targetKind, targetID, edgeType string) error {
	if !validGraphNodeKind(sourceKind) || !validGraphNodeKind(targetKind) {
		return fmt.Errorf("invalid memory block edge kinds %q -> %q", sourceKind, targetKind)
	}
	if sourceID == "" || targetID == "" || edgeType == "" {
		return fmt.Errorf("memory block edge endpoints and type are required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, endpoint := range []struct{ kind, id string }{{sourceKind, sourceID}, {targetKind, targetID}} {
		exists, err := graphNodeExists(tx, endpoint.kind, endpoint.id)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%s graph node %s does not exist", endpoint.kind, endpoint.id)
		}
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO memory_block_edges
		(source_kind, source_id, target_kind, target_id, edge_type)
		VALUES (?, ?, ?, ?, ?)`, sourceKind, sourceID, targetKind, targetID, edgeType)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (g *GraphDB) MemoryBlockEdges() ([]MemoryBlockEdge, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT id, source_kind, source_id, target_kind, target_id,
		edge_type, created_at FROM memory_block_edges ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edges []MemoryBlockEdge
	for rows.Next() {
		var edge MemoryBlockEdge
		if err := rows.Scan(&edge.ID, &edge.SourceKind, &edge.SourceID, &edge.TargetKind,
			&edge.TargetID, &edge.Type, &edge.CreatedAt); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	return edges, rows.Err()
}

// BlocksForNode 返回与某个图节点通过任意边相连的一等记忆块。
// 例：sentence --contains--> block；entity --depicts--> block。
func (g *GraphDB) BlocksForNode(nodeKind, nodeID string) ([]MemoryBlock, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT b.id, b.modality, b.text_content, b.payload_digest,
		b.mime, b.size, b.width, b.height, b.vector, b.fingerprint, b.source, b.tool,
		b.created_at, b.updated_at
		FROM memory_block_edges e
		JOIN memory_blocks b ON (
			(e.source_kind = 'block' AND e.source_id = b.id AND e.target_kind = ? AND e.target_id = ?)
			OR (e.target_kind = 'block' AND e.target_id = b.id AND e.source_kind = ? AND e.source_id = ?))
		ORDER BY b.created_at, b.id`, nodeKind, nodeID, nodeKind, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blocks []MemoryBlock
	for rows.Next() {
		var block MemoryBlock
		var vectorJSON string
		if err := rows.Scan(&block.ID, &block.Modality, &block.Text, &block.PayloadDigest,
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
	return blocks, rows.Err()
}
