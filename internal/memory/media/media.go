// Package media 是记忆系统的内容寻址媒体存储（CAS）。
//
// 为何需要它：此前四层记忆全是纯文本载体——L0 `ContextEvent`、L1 `text.Event`、
// L2 `document.Doc`、L3 图库的 `sentences.text TEXT UNIQUE`——没有任何一层能
// 存二进制。multimodal 插件注入的图片在本轮对话内可见（走 message 数组，不经
// 记忆），下一轮起就只剩 `ToolResultItem.Output` 里那句
// "[已将图片注入后续对话] /tmp/x.png"，即一条路径字符串。那个文件被删或被
// 覆盖之后连线索都断了。
//
// 为何是内容寻址而不是存路径：
//   - 路径会失效。/tmp 下的探针图、下载缓存、其他进程的临时产物，记忆里留个
//     路径等于留个悬空指针。
//   - 同一张图往往被反复注入（用户连问几轮同一张截图、see_video 相邻帧高度
//     相似）。按 sha256 寻址天然去重，引用计数记住被引了几次。
//   - 内容即身份，跟 L3 图库 `sentences.text UNIQUE` 的思路一致：文本节点用
//     文本本身做身份，媒体节点用内容摘要做身份。
package media

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// OwnerKind 是 media_refs.owner_kind 的取值，对应引用媒体的记忆层。
//
// 定义为常量而不是让调用方写字符串：owner_kind 进了主键，
// 拼错一个字符就是一条永远对不上的孤立引用（AddRef 不会报错，
// DropOwner 也永远匹配不到）。
const (
	// OwnerContext 是 L0 对话上下文事件（ContextEvent.ID）。
	OwnerContext = "context"
	// OwnerDocument 是 L2 文档记忆（Doc.ID）。
	OwnerDocument = "document"
	// OwnerGraphSentence 是 L3 图库句子节点（sentences.id）。
	OwnerGraphSentence = "graph_sentence"
)

// Kind 是媒体大类。刻意只分三类而不细分具体格式：
// 记忆检索关心的是“这是张图还是段音频”，具体编码交给 MIME 字段。
type Kind string

const (
	KindImage Kind = "image"
	KindAudio Kind = "audio"
	KindVideo Kind = "video"
	KindOther Kind = "other"
)

// Item 是一条媒体记录。
//
// Digest 既是主键也是磁盘文件名，所以没有单独的 Path 字段——路径可由
// Store 根据 Digest 推导，不落库（落了就又是个会失效的引用）。
type Item struct {
	// Digest 是内容 sha256 的十六进制串（64 字符），媒体的唯一身份。
	Digest string `json:"digest"`
	// Kind 是大类，供检索时按模态筛选。
	Kind Kind `json:"kind"`
	// MIME 是原始 MIME 类型，如 image/png。
	MIME string `json:"mime"`
	// Size 是字节数。
	Size int64 `json:"size"`
	// Width/Height 是像素尺寸，未知或不适用时为 0。
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// OriginPath 是首次入库时的来源路径，仅供人类溯源与调试。
	// **不可用于读取内容**——它随时可能失效，这正是本包存在的理由。
	OriginPath string `json:"origin_path,omitempty"`
	// Tool 是注入这条媒体的工具名（如 multimodal_see_picture）。
	Tool string `json:"tool,omitempty"`
	// Description 是视觉/音频模型生成的文字描述，供 L2/L3 检索。
	// 空表示未描述（未开启描述、模型不可用或描述失败）。
	Description string `json:"description,omitempty"`
	// DescribedBy 记录描述来自哪个源，让后续读者能判断可靠性。
	DescribedBy string `json:"described_by,omitempty"`
	// RefCount 是引用计数。GC 只清理归零的项。
	RefCount int `json:"ref_count"`
	// FirstSeen/LastSeen 是首末次入库时间。
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// Store 管理媒体的元数据（SQLite）与内容（磁盘 CAS 目录）。
//
// 元数据与内容分离而不是把 blob 塞进 SQLite：单张图动辄几 MB，塞进库会让
// 每次 VACUUM/备份都拖着几百 MB 走，也让 WAL 迅速膨胀。CAS 目录用两级
// 前缀分桶（ab/cdef...）避免单目录几万文件。
type Store struct {
	mu      sync.RWMutex
	db      *sql.DB
	blobDir string

	// maxBytes 是内容目录的容量上限，0 表示不限。
	// 超限时 GC 按 LastSeen 从旧到新淘汰 RefCount=0 的项。
	maxBytes int64
}

// New 打开（或初始化）媒体存储。
// dir 下会建 media.db 与 blobs/ 两个条目。
func New(dir string, maxBytes int64) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0755); err != nil {
		return nil, fmt.Errorf("media: create blob dir: %w", err)
	}
	dbPath := filepath.Join(dir, "media.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("media: open db: %w", err)
	}
	s := &Store{db: db, blobDir: filepath.Join(dir, "blobs"), maxBytes: maxBytes}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initSchema() error {
	stmts := []string{
		// digest 作主键：内容即身份，重复 Put 同一内容只递增 ref_count。
		`CREATE TABLE IF NOT EXISTS media (
			digest       TEXT PRIMARY KEY,
			kind         TEXT NOT NULL,
			mime         TEXT NOT NULL,
			size         INTEGER NOT NULL,
			width        INTEGER DEFAULT 0,
			height       INTEGER DEFAULT 0,
			origin_path  TEXT,
			tool         TEXT,
			description  TEXT,
			described_by TEXT,
			ref_count    INTEGER DEFAULT 0,
			first_seen   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			last_seen    TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_media_kind ON media(kind)`,
		`CREATE INDEX IF NOT EXISTS idx_media_refcount ON media(ref_count)`,
		`CREATE INDEX IF NOT EXISTS idx_media_last_seen ON media(last_seen)`,
		// 反向索引：哪条记忆引用了哪个媒体。
		// owner_kind 取 context / document / graph_sentence，owner_id 是各层自己的标识。
		// 主键含三列，同一 owner 重复挂同一媒体是幂等的。
		`CREATE TABLE IF NOT EXISTS media_refs (
			digest     TEXT NOT NULL,
			owner_kind TEXT NOT NULL,
			owner_id   TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (digest, owner_kind, owner_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_refs_owner ON media_refs(owner_kind, owner_id)`,
		`CREATE INDEX IF NOT EXISTS idx_refs_digest ON media_refs(digest)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("media: schema %q: %w", truncate(q, 60), err)
		}
	}
	return nil
}

// blobPath 按两级前缀分桶推导内容路径。
func (s *Store) blobPath(digest string) string {
	if len(digest) < 4 {
		return filepath.Join(s.blobDir, digest)
	}
	return filepath.Join(s.blobDir, digest[:2], digest[2:])
}

// Put 落盘并登记一段媒体内容，返回其 digest。
//
// 幂等：同一内容重复 Put 不重复落盘，只更新 last_seen 与可选的新元数据
// （描述、尺寸等——后来者可能带着前一次没有的信息）。
func (s *Store) Put(data []byte, meta Item) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("media: empty content")
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.blobPath(digest)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return "", fmt.Errorf("media: mkdir: %w", err)
		}
		// 先写临时文件再 rename：中途崩溃不会留下半个 blob 被后续
		// 当成完整内容读走（digest 校验能发现，但那时已经把坏数据喂给模型了）。
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0644); err != nil {
			return "", fmt.Errorf("media: write blob: %w", err)
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return "", fmt.Errorf("media: commit blob: %w", err)
		}
	}

	now := time.Now()
	if meta.Kind == "" {
		meta.Kind = KindFromMIME(meta.MIME)
	}
	_, err := s.db.Exec(`
		INSERT INTO media (digest, kind, mime, size, width, height,
		                   origin_path, tool, description, described_by,
		                   ref_count, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
		ON CONFLICT(digest) DO UPDATE SET
			last_seen    = excluded.last_seen,
			-- 只在原值为空时补写：先到的描述可能来自更强的模型，
			-- 后到的空值不该把它冲掉。
			description  = CASE WHEN COALESCE(media.description,'')  = '' THEN excluded.description  ELSE media.description  END,
			described_by = CASE WHEN COALESCE(media.described_by,'') = '' THEN excluded.described_by ELSE media.described_by END,
			width        = CASE WHEN media.width  = 0 THEN excluded.width  ELSE media.width  END,
			height       = CASE WHEN media.height = 0 THEN excluded.height ELSE media.height END,
			tool         = CASE WHEN COALESCE(media.tool,'') = '' THEN excluded.tool ELSE media.tool END
	`, digest, string(meta.Kind), meta.MIME, int64(len(data)), meta.Width, meta.Height,
		meta.OriginPath, meta.Tool, meta.Description, meta.DescribedBy, now, now)
	if err != nil {
		return "", fmt.Errorf("media: upsert meta: %w", err)
	}
	return digest, nil
}

// Get 读取内容并校验 digest。
//
// 校验不是多余的：CAS 的全部保证建立在"文件名 == 内容摘要"上，磁盘位翻转
// 或外部误改会让这条保证失效，而把损坏的图喂给模型只会得到无从追溯的幻觉。
func (s *Store) Get(digest string) ([]byte, error) {
	s.mu.RLock()
	path := s.blobPath(digest)
	s.mu.RUnlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("media: read %s: %w", shortDigest(digest), err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != digest {
		return nil, fmt.Errorf("media: digest mismatch for %s (content corrupted)", shortDigest(digest))
	}
	return data, nil
}

// Stat 返回元数据，不读内容。
func (s *Store) Stat(digest string) (*Item, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.scanOne(s.db.QueryRow(`
		SELECT digest, kind, mime, size, width, height, origin_path, tool,
		       description, described_by, ref_count, first_seen, last_seen
		FROM media WHERE digest = ?`, digest))
}

// Describe 写入（或覆盖）文字描述。
//
// 与 Put 的"只在空时补写"不同：Describe 是显式操作，调用方明确想要这份
// 描述生效（例如换了更强的视觉模型重新描述）。
func (s *Store) Describe(digest, description, describedBy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE media SET description = ?, described_by = ? WHERE digest = ?`,
		description, describedBy, digest)
	if err != nil {
		return fmt.Errorf("media: describe: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("media: describe: unknown digest %s", shortDigest(digest))
	}
	return nil
}

// AddRef 登记一条引用并递增计数。幂等：同一 (digest, owner) 重复调用不重复计数。
func (s *Store) AddRef(digest, ownerKind, ownerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT OR IGNORE INTO media_refs (digest, owner_kind, owner_id) VALUES (?, ?, ?)`,
		digest, ownerKind, ownerID)
	if err != nil {
		return fmt.Errorf("media: add ref: %w", err)
	}
	// 只有真的插进去才递增：否则重复调用会让计数虚高，GC 永远不敢清。
	if n, _ := res.RowsAffected(); n > 0 {
		if _, err := tx.Exec(`UPDATE media SET ref_count = ref_count + 1 WHERE digest = ?`, digest); err != nil {
			return fmt.Errorf("media: bump refcount: %w", err)
		}
	}
	return tx.Commit()
}

// DropRef 注销一条引用并递减计数。内容不立即删除，留给 GC。
func (s *Store) DropRef(digest, ownerKind, ownerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`DELETE FROM media_refs WHERE digest = ? AND owner_kind = ? AND owner_id = ?`,
		digest, ownerKind, ownerID)
	if err != nil {
		return fmt.Errorf("media: drop ref: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		// MAX(0, ...) 兜底：历史数据或并发意外让计数与 refs 表不一致时，
		// 不让它掉成负数（负数会让容量 GC 的排序失去意义）。
		if _, err := tx.Exec(`UPDATE media SET ref_count = MAX(0, ref_count - 1) WHERE digest = ?`, digest); err != nil {
			return fmt.Errorf("media: lower refcount: %w", err)
		}
	}
	return tx.Commit()
}

// DropOwner 注销某个 owner 的全部引用（该条记忆被删/被归档替换时用）。
func (s *Store) DropOwner(ownerKind, ownerID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT digest FROM media_refs WHERE owner_kind = ? AND owner_id = ?`,
		ownerKind, ownerID)
	if err != nil {
		return 0, err
	}
	var digests []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err == nil {
			digests = append(digests, d)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(digests) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM media_refs WHERE owner_kind = ? AND owner_id = ?`, ownerKind, ownerID); err != nil {
		return 0, err
	}
	for _, d := range digests {
		if _, err := tx.Exec(`UPDATE media SET ref_count = MAX(0, ref_count - 1) WHERE digest = ?`, d); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(digests), nil
}

// Refs 返回某个 owner 引用的全部 digest。
func (s *Store) Refs(ownerKind, ownerID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT digest FROM media_refs WHERE owner_kind = ? AND owner_id = ? ORDER BY created_at`,
		ownerKind, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err == nil {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

// Search 按描述文本做 LIKE 匹配，返回最近的若干条。
//
// 刻意不在这里做向量检索：媒体的语义检索走 L2 文档层的既有索引
// （描述文字随记忆条目一起进 Doc.Content，复用那套 TF-IDF/embedding），
// 本方法只是"按关键词直接翻媒体库"的补充入口。
func (s *Store) Search(query string, kind Kind, limit int) ([]*Item, error) {
	if limit <= 0 {
		limit = 20
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := `SELECT digest, kind, mime, size, width, height, origin_path, tool,
	             description, described_by, ref_count, first_seen, last_seen
	      FROM media WHERE COALESCE(description,'') != ''`
	args := []interface{}{}
	if strings.TrimSpace(query) != "" {
		q += ` AND description LIKE ?`
		args = append(args, "%"+query+"%")
	}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, string(kind))
	}
	q += ` ORDER BY last_seen DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := s.scanRows(rows)
		if err != nil {
			continue
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Pending 返回尚无描述的媒体，供后台描述任务消费。
// Pending 返回尚无描述的媒体，供后台描述任务消费。
//
// 不只看 description 为空，还要求 described_by 也为空。
// 因为“已尝试但无法描述”的项（如 kind=other 的二进制、blob 已丢失）
// 会被标记为 described_by=unsupported/content-missing 而 description 仍为空——
// 若只看 description，这些项每轮都会被取出来重试，永远卡在队列头部，
// 真正需要描述的新项永远轮不到（LIMIT 只取前 N 条）。
func (s *Store) Pending(limit int) ([]*Item, error) {
	if limit <= 0 {
		limit = 10
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`
		SELECT digest, kind, mime, size, width, height, origin_path, tool,
		       description, described_by, ref_count, first_seen, last_seen
		FROM media
		WHERE COALESCE(description,'') = '' AND COALESCE(described_by,'') = ''
		ORDER BY last_seen DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := s.scanRows(rows)
		if err != nil {
			continue
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GC 清理无人引用的内容。
//
// 两段策略：
//  1. ref_count=0 且 last_seen 早于 minAge 的一律清理。刚 Put 还没来得及
//     AddRef 的项 refcount 也是 0，minAge 保护它们不被立刻清掉。
//  2. 清完仍超 maxBytes 时，继续按 last_seen 从旧到新淘汰 ref_count=0 的项。
//
// 有引用的项永不删除——那会让记忆里的 digest 变成悬空指针，正是本包要避免的。
func (s *Store) GC(minAge time.Duration) (removed int, freed int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-minAge)
	rows, err := s.db.Query(`
		SELECT digest, size FROM media
		WHERE ref_count <= 0 AND last_seen < ?
		ORDER BY last_seen`, cutoff)
	if err != nil {
		return 0, 0, err
	}
	type cand struct {
		digest string
		size   int64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.digest, &c.size); err == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()

	for _, c := range cands {
		if e := os.Remove(s.blobPath(c.digest)); e != nil && !os.IsNotExist(e) {
			continue // 删不掉就留着元数据，下轮再试；不制造"元数据没了文件还在"的孤儿
		}
		if _, e := s.db.Exec(`DELETE FROM media WHERE digest = ?`, c.digest); e != nil {
			continue
		}
		removed++
		freed += c.size
	}

	if s.maxBytes > 0 {
		r2, f2 := s.enforceCapacityLocked()
		removed += r2
		freed += f2
	}
	return removed, freed, nil
}

// enforceCapacityLocked 在超出 maxBytes 时继续淘汰无引用项（调用方已持锁）。
func (s *Store) enforceCapacityLocked() (removed int, freed int64) {
	var total int64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM media`).Scan(&total); err != nil {
		return 0, 0
	}
	if total <= s.maxBytes {
		return 0, 0
	}
	need := total - s.maxBytes

	rows, err := s.db.Query(`SELECT digest, size FROM media WHERE ref_count <= 0 ORDER BY last_seen`)
	if err != nil {
		return 0, 0
	}
	type cand struct {
		digest string
		size   int64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.digest, &c.size); err == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()

	for _, c := range cands {
		if freed >= need {
			break
		}
		if e := os.Remove(s.blobPath(c.digest)); e != nil && !os.IsNotExist(e) {
			continue
		}
		if _, e := s.db.Exec(`DELETE FROM media WHERE digest = ?`, c.digest); e != nil {
			continue
		}
		removed++
		freed += c.size
	}
	return removed, freed
}

// Stats 返回容量与条目统计，供 WebUI / healthcheck 展示。
func (s *Store) Stats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := map[string]interface{}{"blob_dir": s.blobDir, "max_bytes": s.maxBytes}
	var count, described, orphan int
	var total int64
	s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0) FROM media`).Scan(&count, &total)
	s.db.QueryRow(`SELECT COUNT(*) FROM media WHERE COALESCE(description,'') != ''`).Scan(&described)
	s.db.QueryRow(`SELECT COUNT(*) FROM media WHERE ref_count <= 0`).Scan(&orphan)
	out["count"] = count
	out["total_bytes"] = total
	out["described"] = described
	out["unreferenced"] = orphan

	byKind := map[string]int{}
	rows, err := s.db.Query(`SELECT kind, COUNT(*) FROM media GROUP BY kind`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int
			if rows.Scan(&k, &n) == nil {
				byKind[k] = n
			}
		}
	}
	out["by_kind"] = byKind
	return out
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

// ---- 扫描辅助 ----

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func (s *Store) scanOne(r rowScanner) (*Item, error) {
	it, err := scanItem(r)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("media: unknown digest")
	}
	return it, err
}

func (s *Store) scanRows(r rowScanner) (*Item, error) { return scanItem(r) }

func scanItem(r rowScanner) (*Item, error) {
	var it Item
	var kind string
	var origin, tool, desc, by sql.NullString
	if err := r.Scan(&it.Digest, &kind, &it.MIME, &it.Size, &it.Width, &it.Height,
		&origin, &tool, &desc, &by, &it.RefCount, &it.FirstSeen, &it.LastSeen); err != nil {
		return nil, err
	}
	it.Kind = Kind(kind)
	it.OriginPath = origin.String
	it.Tool = tool.String
	it.Description = desc.String
	it.DescribedBy = by.String
	return &it, nil
}

// ---- 工具函数 ----

// KindFromMIME 把 MIME 归到大类。
func KindFromMIME(mime string) Kind {
	m := strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.HasPrefix(m, "image/"):
		return KindImage
	case strings.HasPrefix(m, "audio/"):
		return KindAudio
	case strings.HasPrefix(m, "video/"):
		return KindVideo
	default:
		return KindOther
	}
}

// ParseDataURL 从 data:<mime>;base64,<data> 提取 MIME 与原始字节。
//
// 与 agent/api 里的 parseAudioDataURL 分开实现：那个只认音频且只回 base64
// 串（它要把串塞回 OpenAI 的 input_audio 字段），这里要的是解码后的字节。
func ParseDataURL(url string) (mime string, data []byte, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return "", nil, false
	}
	rest := url[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", nil, false
	}
	head := rest[:comma]
	payload := rest[comma+1:]
	if !strings.HasSuffix(strings.ToLower(head), ";base64") {
		return "", nil, false
	}
	mime = head[:len(head)-len(";base64")]
	if mime == "" || payload == "" {
		return "", nil, false
	}
	decoded, err := base64Decode(payload)
	if err != nil {
		return "", nil, false
	}
	return mime, decoded, true
}

// DataURL 把内容编回 data URL，供重新注入模型对话。
func DataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64Encode(data)
}

// CopyFrom 从 reader 读全部内容后 Put，用于大文件不便一次性构造 []byte 的场合。
func (s *Store) CopyFrom(r io.Reader, meta Item) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("media: read source: %w", err)
	}
	return s.Put(data, meta)
}

// MarshalItems 序列化条目列表，供工具返回给模型。
func MarshalItems(items []*Item) string {
	b, err := json.Marshal(items)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
