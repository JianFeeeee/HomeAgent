package core

// 图记忆的**共同面**（设计 docs/zh/resident-subagent-design.md §16.0）。
//
// 只有两个方法 —— 因为按调用方实测分类后，`Agent.memory` 的 42 处使用里，
// 真正"根与子都要"的只有：
//
//	Recall  读（上下文检索 / memory_recall）
//	Commit  写（自动写入路径的图部分 / memory_commit）
//
// 其余 40 处全是**主 agent 整理记忆**与**记忆整理流水线**：
//   - 记忆整理流水线：distill.go 的 archive/review/merge 循环
//   - 记忆块 + 媒体桥：graphmedia.go / medialoop.go
//   - 记忆整理工具：memory_merge / memory_delete_entity / memory_block_merge /
//     memory_purge / memory_edit / memory_introspect
//
// 所以驻留子的轻量内核**不该有那些代码路径**：它用 `*memory.LightMemory` 接上 `graph`，
// 而 `a.memory`（整理面）保持 nil —— 既有的 22 处 `if a.memory != nil` 关卡
// 会自动把整理面全部禁掉，不需要写"每个方法都返回错误"的受限包装。

import "gitcode.com/JianFeeeee/HomeAgent/internal/memory"

// GraphMemory 是任意 agent 都能用的图记忆面。
//
// 实现者：
//   - 根 agent：直接就是 *memory.GraphDB
//   - 驻留子：*memory.LightMemory（读 temp∪main，只写 temp）
type GraphMemory interface {
	// Recall 按关键词/种子实体召回（子的实现是两空间并集）。
	Recall(keywords []string, seedEntities []string, depth int, sessionFilter string) (*memory.RecallResult, error)
	// Commit 写入三元组（子的实现只落自己的 temp 空间）。
	Commit(triples []memory.Triple, sessionID string, turnID int) (int, int, error)
}
