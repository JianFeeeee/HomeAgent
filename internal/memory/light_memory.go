package memory

// 轻量内核的**图记忆装配**（设计 §5.6）：
//
//	temp 实例（独立存储，读写）  ← 子的一切图记忆写入落这里，与子同生共死
//	主库**受限句柄**（只读）      ← 子只能读；写入被 SQLite 结构性拒绝
//
// 子的查询 = 两个实例各查一次，**应用层合并**（并集）：
//   - 实体：按名字去重（同名视为同一实体，保留 mention_count 较大的一条）
//   - 关系：按 (源名, 关系, 目标名) 去重
//
// 为什么合并放在应用层而不是给共享记忆层加 space 列：**独立存储实例**让隔离成为
// 结构性的（不同库），不依赖 where 条件；代价就是这个合并函数。
//
// 回收（N2d）由父 agent 读 `Temp()` 并选出要保留的记录写进主库。

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// LightMemory 是驻留子的图记忆装配。
type LightMemory struct {
	mu sync.RWMutex

	// temp 是子自己的图记忆实例（独立存储）。当不允许子写图记忆时为 nil。
	temp *GraphDB
	// main 是主图记忆的受限句柄（只读）。
	main *GraphDB

	// allowWrite 报告是否允许写 temp（false ⇒ 子对图记忆完全只读）。
	allowWrite bool
}

// NewLightMemory 构造子的图记忆装配。
//
//   - main: 主库句柄。为 nil 时表示"只能用自己的 temp"（一般不该发生）。
//   - tempPath: 子 temp 实例的存储路径（独立文件）。allowWrite=false 时**不会**打开它。
//   - allowWrite: 是否允许子写图记忆（用户给的备选开关，默认建议 true）。
func NewLightMemory(main *GraphDB, tempPath string, allowWrite bool) (*LightMemory, error) {
	m := &LightMemory{main: main, allowWrite: allowWrite}
	if !allowWrite {
		return m, nil
	}
	if tempPath == "" {
		return nil, fmt.Errorf("允许写图记忆时必须给出 temp 存储路径")
	}
	temp, err := NewGraphDB(tempPath) // temp 是子自己的库：建表/迁移都正常
	if err != nil {
		return nil, fmt.Errorf("open temp graph db: %w", err)
	}
	m.temp = temp
	return m, nil
}

// AllowWrite 报告子能否写图记忆。
func (m *LightMemory) AllowWrite() bool { return m.allowWrite }

// Temp 返回子的 temp 实例（可能为 nil）。回收时父 agent 用它读取/收割。
func (m *LightMemory) Temp() *GraphDB {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.temp
}

// Main 返回主库的受限句柄。
func (m *LightMemory) Main() *GraphDB { return m.main }

// Commit 写入图记忆：**只落 temp**（设计 §5：写目标收窄到自己的空间）。
func (m *LightMemory) Commit(triples []Triple, sessionID string, turnID int) (int, int, error) {
	if !m.allowWrite {
		return 0, 0, fmt.Errorf("本 agent 不允许写图记忆（allowTempGraphWrite=false）")
	}
	m.mu.RLock()
	temp := m.temp
	m.mu.RUnlock()
	if temp == nil {
		return 0, 0, fmt.Errorf("temp 图记忆实例不可用")
	}
	return temp.Commit(triples, sessionID, turnID)
}

// Recall 在 temp ∪ main 上做召回（并集），按上述规则合并去重。
//
// 任一侧出错都不影响另一侧的结果：单侧失败只在两侧都失败时返回错误
// （主库是只读句柄，任何"查询即失败"都说明是真实故障）。
func (m *LightMemory) Recall(keywords []string, seedEntities []string, depth int, sessionFilter string) (*RecallResult, error) {
	m.mu.RLock()
	temp := m.temp
	m.mu.RUnlock()

	var (
		parts   []*RecallResult
		lastErr error
		okAny   bool
	)
	if temp != nil {
		r, err := temp.Recall(keywords, seedEntities, depth, sessionFilter)
		if err != nil {
			lastErr = err
		} else {
			okAny = true
			parts = append(parts, r)
		}
	}
	if m.main != nil {
		r, err := m.main.Recall(keywords, seedEntities, depth, sessionFilter)
		if err != nil {
			lastErr = err
		} else {
			okAny = true
			parts = append(parts, r)
		}
	}
	if !okAny {
		if lastErr == nil {
			lastErr = fmt.Errorf("没有可用的图记忆实例")
		}
		return nil, lastErr
	}
	return mergeRecall(parts...), nil
}

// mergeRecall 把多个来源的召回结果并成一份（实体按名字、关系按三元组去重）。
//
// 顺序确定（先 entities/relations 各自排序），便于测试与展示稳定。
func mergeRecall(parts ...*RecallResult) *RecallResult {
	out := &RecallResult{}
	seenEntity := map[string]int{} // 小写名 → out.Entities 下标
	for _, p := range parts {
		if p == nil {
			continue
		}
		for _, e := range p.Entities {
			k := strings.ToLower(strings.TrimSpace(e.Name))
			if k == "" {
				continue
			}
			if i, dup := seenEntity[k]; dup {
				// 同名实体：保留 mention_count 较大的一条（更新的那个）。
				if e.MentionCount > out.Entities[i].MentionCount {
					out.Entities[i] = e
				}
				continue
			}
			seenEntity[k] = len(out.Entities)
			out.Entities = append(out.Entities, e)
		}
	}
	seenRel := map[string]struct{}{}
	for _, p := range parts {
		if p == nil {
			continue
		}
		for _, r := range p.Relations {
			k := strings.ToLower(strings.TrimSpace(r.SourceName)) + "\x00" +
				strings.ToLower(strings.TrimSpace(r.RelationType)) + "\x00" +
				strings.ToLower(strings.TrimSpace(r.TargetName))
			if _, dup := seenRel[k]; dup {
				continue
			}
			seenRel[k] = struct{}{}
			out.Relations = append(out.Relations, r)
		}
	}
	sort.Slice(out.Entities, func(i, j int) bool { return out.Entities[i].Name < out.Entities[j].Name })
	sort.Slice(out.Relations, func(i, j int) bool {
		if out.Relations[i].SourceName != out.Relations[j].SourceName {
			return out.Relations[i].SourceName < out.Relations[j].SourceName
		}
		if out.Relations[i].RelationType != out.Relations[j].RelationType {
			return out.Relations[i].RelationType < out.Relations[j].RelationType
		}
		return out.Relations[i].TargetName < out.Relations[j].TargetName
	})
	return out
}

// Close 关闭 temp 实例（主库句柄由父 agent 拥有，不在这里关）。
func (m *LightMemory) Close() error {
	m.mu.Lock()
	temp := m.temp
	m.temp = nil
	m.mu.Unlock()
	if temp != nil {
		return temp.Close()
	}
	return nil
}
