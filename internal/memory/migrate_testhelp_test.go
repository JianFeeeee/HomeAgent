package memory

// 测试脚手架：重建被测试删掉的 memory_blocks 表。
// 刻意放在 _test.go 里 —— 它不是生产符号，只是让回滚测试能自造失败。

// ddlMemoryBlocks 是 graph.go 里同一段 DDL 的副本，仅供测试重建被删的表。
//
// ★★ 它是**重复的真相源** —— 加列时必须两边都改。
//   2026-10-04 加 semantic_type 时就漏了这里，报
//   「no such column: semantic_type」。抽公共常量是正解，
//   但 graph.go 的 DDL 是一整段 schema 初始化（多张表），
//   拆出来会改动面更大 —— 暂时保留副本，改列时记得同步。
// 刻意不导出成生产符号：它是测试脚手架，不是 API。
const ddlMemoryBlocks = `CREATE TABLE IF NOT EXISTS memory_blocks (
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
	scene TEXT DEFAULT '',
	-- ★ semantic_type 必须与 graph.go 的 DDL 同步（2026-10-04 加列时漏过一次，
	--    报 'no such column: semantic_type'）。改那边记得改这里。
	semantic_type TEXT DEFAULT '',
	created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
)`
