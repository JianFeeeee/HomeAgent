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
//     相似）。按 sha256 寻址天然去重，同一份字节只存一遍。
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
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// digestHexLen 是 sha256 的十六进制串长度。
const digestHexLen = sha256.Size * 2

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
	// FirstSeen/LastSeen 是首末次入库时间。
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// --- 多模态嵌入（v1.2.0）---
	// Vec 是视觉嵌入向量的序列化（JSON []float64），nil 表示未嵌入。
	Vec []float64 `json:"vec,omitempty"`
	// VecModel 是产生 Vec 的模型标识（如 "clip-vit-b32"），
	// 用于模型切换后判断是否需要重算。
	VecModel string `json:"vec_model,omitempty"`
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
}

// New 打开（或初始化）媒体存储。
// dir 下会建 media.db 与 blobs/ 两个条目。
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0755); err != nil {
		return nil, fmt.Errorf("media: create blob dir: %w", err)
	}
	dbPath := filepath.Join(dir, "media.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("media: open db: %w", err)
	}
	s := &Store{db: db, blobDir: filepath.Join(dir, "blobs")}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initSchema() error {
	stmts := []string{
		// digest 作主键：内容即身份，重复 Put 同一内容不重复落盘。
		`CREATE TABLE IF NOT EXISTS media (
			digest       TEXT PRIMARY KEY,
			kind         TEXT NOT NULL,
			mime         TEXT NOT NULL,
			size         INTEGER NOT NULL,
			width        INTEGER DEFAULT 0,
			height       INTEGER DEFAULT 0,
			origin_path  TEXT,
			tool         TEXT,
			first_seen   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			last_seen    TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_media_kind ON media(kind)`,
		`CREATE INDEX IF NOT EXISTS idx_media_last_seen ON media(last_seen)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("media: schema %q: %w", truncate(q, 60), err)
		}
	}
	// v1.2.0 迁移：给 media 表加 vec（视觉嵌入向量 JSON）和 vec_model（模型标识）。
	migrations := []string{
		`ALTER TABLE media ADD COLUMN vec TEXT`,
		`ALTER TABLE media ADD COLUMN vec_model TEXT`,
	}
	for _, q := range migrations {
		_, _ = s.db.Exec(q) // 列已存在时返回 "duplicate column name"，可忽略
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
		                   origin_path, tool, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(digest) DO UPDATE SET
			last_seen    = excluded.last_seen,
			width        = CASE WHEN media.width  = 0 THEN excluded.width  ELSE media.width  END,
			height       = CASE WHEN media.height = 0 THEN excluded.height ELSE media.height END,
			tool         = CASE WHEN COALESCE(media.tool,'') = '' THEN excluded.tool ELSE media.tool END
	`, digest, string(meta.Kind), meta.MIME, int64(len(data)), meta.Width, meta.Height,
		meta.OriginPath, meta.Tool, now, now)
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
		       first_seen, last_seen,
		       vec, vec_model
		FROM media WHERE digest = ?`, digest))
}

// Delete 删除媒体内容与元数据。
//
// 这不是 GC，也不看引用计数：调用方是记忆系统本身——当它把一个记忆块
// 永久地从三层记忆中删掉（而非在层间迁移）时，媒体作为块的内容一并删除。
// 文本块就是这么管理的：删除块即删除内容。
func (s *Store) Delete(digest string) error {
	if digest == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.blobPath(digest)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("media: remove blob %s: %w", shortDigest(digest), err)
	}
	if _, err := s.db.Exec(`DELETE FROM media WHERE digest = ?`, digest); err != nil {
		return fmt.Errorf("media: delete meta %s: %w", shortDigest(digest), err)
	}
	return nil
}

// Stats 返回条目统计，供 WebUI / healthcheck 展示。
func (s *Store) Stats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := map[string]interface{}{"blob_dir": s.blobDir}
	var count int
	var total int64
	s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0) FROM media`).Scan(&count, &total)
	out["count"] = count
	out["total_bytes"] = total

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

// ---- 多模态嵌入（v1.2.0） ----

// SetVec 给一条已入库的媒体设置视觉嵌入向量。
//
// 设计选择：vec 是 TEXT（JSON 序列化的 []float64）而非 BLOB，
// 因为 Go 的 json.Marshal/Unmarshal 对 []float64 是自然的，
// 而 SQLite 的 BLOB 是 []byte，序列化多一层反而复杂。
// 量级：一条 vec 最多 1536 维 × ~15 字节 ≈ 23KB，TEXT 合适。
func (s *Store) SetVec(digest string, vec []float64, model string) error {
	vecJSON, err := json.Marshal(vec)
	if err != nil {
		return fmt.Errorf("media: marshal vec: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(`UPDATE media SET vec = ?, vec_model = ? WHERE digest = ?`,
		string(vecJSON), model, digest)
	return err
}

// StaleVecDigests 返回所有需要重新嵌入的图片 digest：
// vec_model 不等于 currentModel（模型切换）或 vec_model 为空（从未嵌入）。
// 调用方使用返回的 digest 列表调用 Get/EmbedImage/SetVec 完成重算。
func (s *Store) StaleVecDigests(currentModel string) ([]string, error) {
	return s.staleVecDigests(currentModel, "image")
}

// StaleVecDigestsAll 返回所有需要重新嵌入的媒体 digest（不限 kind），
// 供模型切换后全量迁移向量空间（image + audio + video 等）。
func (s *Store) StaleVecDigestsAll(currentModel string) ([]string, error) {
	return s.staleVecDigests(currentModel, "")
}

// staleVecDigests 是 StaleVecDigests 的核心实现，kind=” 时不按 kind 过滤。
// 废弃了"只迁移图片"的限定：模型切换后所有模态都应迁移到新向量空间。
func (s *Store) staleVecDigests(currentModel string, kind string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT digest FROM media
		WHERE (COALESCE(vec_model,'') = '' OR vec_model != ?)`
	if kind != "" {
		query += ` AND kind = ?`
	}
	query += ` ORDER BY last_seen`

	var args []interface{}
	args = append(args, currentModel)
	if kind != "" {
		args = append(args, kind)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var digests []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err == nil {
			digests = append(digests, d)
		}
	}
	return digests, rows.Err()
}

// QueryMedia 用查询向量对所有已嵌入媒体做余弦相似度检索，返回 topK 个最相似的 Item。
//
// 这是跨模态检索的关键：查询可以是图片也可以是文本（经文本向量化后调用此方法），
// 被查的媒体库里的每个 item 也有一个视觉向量。两者在同一空间比对，
// 谁的相似度更高就召回谁——不再区分「这是一张图的查询」还是「这是一段文字的查询」，
// 由向量空间的相似度自动判断。
func (s *Store) QueryMedia(queryVec []float64, model string, topK int) ([]*Item, error) {
	hits, err := s.QueryMediaScored(queryVec, model, topK)
	if err != nil {
		return nil, err
	}
	if hits == nil {
		return nil, nil
	}
	out := make([]*Item, len(hits))
	for i, h := range hits {
		out[i] = h.Item
	}
	return out, nil
}

// MediaHit 是一条媒体相似度候选及其分数。
// 跨模态融合需要原始分数做归一化，仅返回 Item 会丢掉尺度信息。
type MediaHit struct {
	Item  *Item
	Score float64
}

// QueryMediaScored 用查询向量对所有已嵌入媒体做余弦相似度检索，
// 返回 topK 个最相似的候选及其原始 cosine 分数（供跨模态归一化）。
//
// 分数只做排序，不在存储层设绝对阈值：多模态文本→图像的绝对 cosine 随模型、
// 语言与数据域漂移，真实标定中有效命中可以低至 0.015。相关性门控在融合器中
// 使用当前候选集合的相对分布完成。
func (s *Store) QueryMediaScored(queryVec []float64, model string, topK int) ([]MediaHit, error) {
	return s.queryMediaScored(queryVec, model, topK)
}

func (s *Store) queryMediaScored(queryVec []float64, model string, topK int) ([]MediaHit, error) {
	if topK <= 0 {
		topK = 20
	}
	if len(queryVec) == 0 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT digest, kind, mime, size, width, height,
		origin_path, tool, first_seen, last_seen,
		vec, vec_model
		FROM media WHERE vec IS NOT NULL AND vec != ''`
	var args []interface{}
	if model != "" {
		query += ` AND vec_model = ?`
		args = append(args, model)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type scored struct {
		item  *Item
		score float64
	}
	var candidates []scored
	for rows.Next() {
		var it Item
		var kind string
		var origin, tool, vecJSON, vecModel sql.NullString
		if err := rows.Scan(&it.Digest, &kind, &it.MIME, &it.Size, &it.Width, &it.Height,
			&origin, &tool, &it.FirstSeen, &it.LastSeen,
			&vecJSON, &vecModel); err != nil {
			continue
		}
		it.Kind = Kind(kind)
		it.OriginPath = origin.String
		it.Tool = tool.String
		if !vecJSON.Valid || vecJSON.String == "" {
			continue
		}
		var itemVec []float64
		if err := json.Unmarshal([]byte(vecJSON.String), &itemVec); err != nil || len(itemVec) == 0 {
			continue
		}
		if len(itemVec) != len(queryVec) {
			continue // 维度不一致，跳过
		}
		score := cosineSimilaritySlice(queryVec, itemVec)
		if score > 0.05 {
			candidates = append(candidates, scored{&it, score})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 按分数降序排序
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	if len(candidates) > topK {
		candidates = candidates[:topK]
	}
	out := make([]MediaHit, len(candidates))
	for i, c := range candidates {
		out[i] = MediaHit{Item: c.item, Score: c.score}
	}
	return out, nil
}

// cosineSimilaritySlice 计算两个 []float64 向量的余弦相似度。
func cosineSimilaritySlice(a, b []float64) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
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
	var origin, tool, vecJSON, vecModel sql.NullString
	if err := r.Scan(&it.Digest, &kind, &it.MIME, &it.Size, &it.Width, &it.Height,
		&origin, &tool, &it.FirstSeen, &it.LastSeen,
		&vecJSON, &vecModel); err != nil {
		return nil, err
	}
	it.Kind = Kind(kind)
	it.OriginPath = origin.String
	it.Tool = tool.String
	if vecJSON.Valid && vecJSON.String != "" {
		var v []float64
		if err := json.Unmarshal([]byte(vecJSON.String), &v); err == nil {
			it.Vec = v
		}
	}
	it.VecModel = vecModel.String
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

// ResolvePrefix 把 digest 前缀补全为完整 digest。
//
// 日志、事件摘要与图库句子里出现的都是 shortDigest（前 12 位），
// 因为完整的 64 位 sha256 会把一行文字撑爆、也无助于人眼辨认。
// 反查时需要这个补全，否则那些短标记只能看不能用。
//
// 前缀歧义视为错误而非"取第一个"：挂错引用会让 GC 删掉仍被引用的内容，
// 宁可这次绑定失败。12 位十六进制的碰撞概率极低，真撞上说明该用更长前缀。
func (s *Store) ResolvePrefix(prefix string) (string, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if len(prefix) < 8 {
		return "", fmt.Errorf("digest 前缀过短（至少 8 位）: %q", prefix)
	}
	if len(prefix) == digestHexLen {
		// 已是完整 digest：仍要确认存在，否则调用方会挂一条孤儿引用
		if _, err := s.Stat(prefix); err != nil {
			return "", err
		}
		return prefix, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(
		`SELECT digest FROM media WHERE digest LIKE ? || '%' LIMIT 2`, prefix)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var found []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return "", err
		}
		found = append(found, d)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	switch len(found) {
	case 0:
		return "", fmt.Errorf("digest 前缀 %q 未匹配到媒体", prefix)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("digest 前缀 %q 有歧义（至少匹配 %s 和 %s）",
			prefix, found[0][:16], found[1][:16])
	}
}
