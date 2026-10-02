package memory

// 测试脚手架：重建被测试删掉的 memory_blocks 表。
// 刻意放在 _test.go 里 —— 它不是生产符号，只是让回滚测试能自造失败。

// ddlMemoryBlocks 是 graph.go 里同一段 DDL 的副本，仅供测试重建被删的表。
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
	created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
)`
