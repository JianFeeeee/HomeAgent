package seq

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// maxCallDepth 是**嵌套调用的结构上界**，不是配置项。
//
// 沿用内核 MaxInterruptFrames 的做法（见 core/scheduler.go:271「是中断栈帧数的
// 结构上界，不是配置项」）：上界一旦可配，总有人会把它调大到栈溢出。
//
// 取 4 与内核的 4 级中断一致。
const maxCallDepth = 4

// Store 负责序列的持久化：**存 AST，不存文本**。
//
// 为什么不存原始文本：执行期若重新解析文本，一次格式改动就会改变已保存
// 序列的行为；存 AST 则解析只发生在创建时，注释/空白/引号形式在 AST
// 层面已消失，不引入执行期差异。
type Store struct {
	dir string
	mu  sync.RWMutex

	// graph 是**已解析的调用边**缓存：序列名 → 它调用的目标（裸名，无 #）。
	//
	// ★ 为什么需要它：CheckGraph 原来每次都 s.List() + 逐条 s.Load(n)，
	//   把**全部**序列重新读盘并反序列化（1000 条各 250KB ⇒ 每次创建都重读
	//   250MB）。实测创建 200 条要 27s、平均 135ms/条且**随序列数线性增长**
	//   —— O(n²)。
	//
	//   正确修法不是"挪到运行期检查"：store.go:163 明确写了
	//   "都必须在建序列/保存时做，而不是等运行"，因为目标不存在要等到
	//   运行才发现会浪费一整轮。校验时机是**语义**，不能为了性能挪。
	//   该做的是让保存时的全图检查不必重读盘。
	graph map[string][]string
}

// edgesOf 返回某序列的调用边（已解析）。
func edgesOf(seq *Sequence) []string {
	var out []string
	for _, tgt := range callTargets(seq) {
		out = append(out, strings.TrimPrefix(tgt, "#"))
	}
	return out
}

// graphOf 返回调用图快照（读时加锁）。
func (s *Store) graphOf() map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]string, len(s.graph))
	for k, v := range s.graph {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// invalidateGraph 丢弃缓存，下次访问时从磁盘重建。
func (s *Store) invalidateGraph() {
	s.mu.Lock()
	s.graph = nil
	s.mu.Unlock()
}

// NewStore 在 dir 下管理序列文件（不创建目录，由 Save 惰性创建）。
func NewStore(dir string) *Store { return &Store{dir: dir} }

// fileOf 返回某序列的落盘路径。
func (s *Store) fileOf(name string) string {
	return filepath.Join(s.dir, name+".json")
}

// Save 落盘一条序列的 AST。
func (s *Store) Save(seq *Sequence) error {
	if seq == nil || strings.TrimSpace(seq.Name) == "" {
		return fmt.Errorf("序列缺少 name")
	}
	if err := s.checkName(seq.Name); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return fmt.Errorf("创建序列目录失败: %w", err)
	}
	b, err := json.MarshalIndent(seq, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化序列 %q 失败: %w", seq.Name, err)
	}
	// 静态校验：同序列内的 group 引用必须存在、不得自调用。
	// 跨序列目标的存在性由 CheckGraph 统一查（此时新序列还没落盘）。
	if err := s.CheckNew(seq); err != nil {
		return err
	}
	// 先写临时文件再 rename：避免写一半被读（与内核原子替换同一思路）
	tmp := s.fileOf(seq.Name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return fmt.Errorf("写序列 %q 失败: %w", seq.Name, err)
	}
	if err := os.Rename(tmp, s.fileOf(seq.Name)); err != nil {
		// 清理失败**有意忽略**：rename 已失败，再报一个清理错误只会
		// 盖掉真正的失败原因（这正是 rename 失败要暴露的那条）。
		// 残留的 .tmp 由下次 Save 覆盖。
		_ = os.Remove(tmp)
		return fmt.Errorf("替换序列 %q 失败: %w", seq.Name, err)
	}
	// 增量维护调用图：只更新**这一条**的边，不重读全量。
	//
	// 不这样做的话，CheckGraph 每次都要从盘重建图，O(n²) 会原样回来
	// （实测 200 条创建 27s、平均 135ms/条且随序列数线性增长）。
	s.mu.Lock()
	if s.graph == nil {
		s.graph = make(map[string][]string)
	}
	s.graph[seq.Name] = edgesOf(seq)
	s.mu.Unlock()
	return nil
}

// Load 读回一条序列的 AST。
func (s *Store) Load(name string) (*Sequence, error) {
	if err := s.checkName(name); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.fileOf(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("序列 %q 不存在（用 seq_list 看可用序列）", name)
		}
		return nil, fmt.Errorf("读序列 %q 失败: %w", name, err)
	}
	var seq Sequence
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&seq); err != nil {
		return nil, fmt.Errorf("序列 %q 的存档损坏: %w", name, err)
	}
	return &seq, nil
}

// Delete 删除一条序列。不存在时报错（不静默成功 —— 模型会以为删掉了）。
func (s *Store) Delete(name string) error {
	if err := s.checkName(name); err != nil {
		return err
	}
	if err := os.Remove(s.fileOf(name)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("序列 %q 不存在（用 seq_list 看可用序列）", name)
		}
		return fmt.Errorf("删除序列 %q 失败: %w", name, err)
	}
	// 该序列的边已从图里移除，否则 CheckGraph 会报"调用了不存在的序列"。
	s.mu.Lock()
	delete(s.graph, name)
	s.mu.Unlock()
	return nil
}

// List 列出全部序列名（升序）。
func (s *Store) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(out)
	return out
}

// checkName 校验序列名：必须能安全用作文件名。
//
// ⚠️ 名字来自模型，且会被拼进路径（Load/Save/Delete 都用）⇒ 必须挡住
// 路径穿越（`../`）与分隔符，否则 `seq_load` 能读到任意文件。
func (s *Store) checkName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("序列名不能为空")
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("序列名 %q 非法：不能包含路径分隔符或 ..", name)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("序列名 %q 非法：不能以 . 开头", name)
	}
	return nil
}

// callTargets 返回某序列内所有 seq_call 的跨序列目标。
func callTargets(seq *Sequence) []string {
	var out []string
	for _, g := range seq.Groups {
		for _, t := range g.Tools {
			if t.Tool != "seq_call" && t.Tool != "seq_when_call" {
				continue
			}
			if tgt, ok := t.Args["target"].(string); ok && strings.HasPrefix(tgt, "#") {
				out = append(out, tgt)
			}
		}
	}
	return out
}

// CheckGraph 校验跨序列调用图。
//
// 两条检查（都必须在**建序列/保存**时做，而不是等运行）：
//  1. 每个 `seq_call` 的目标必须存在（不存在会在运行期才发现，浪费一整轮）
//  2. 不得有环（否则无限嵌套，每层都真的在调工具）
func (s *Store) CheckGraph() error {
	// 用**缓存的调用边**，不重读全部序列。
	//
	// 校验语义与原来完全一致（同样在建序列时做、同样报同样的错），
	// 只是不再为拿边信息把每条序列反序列化一遍。
	graph := s.loadGraph()
	// 目标存在性
	for name, targets := range graph {
		for _, bare := range targets {
			if _, ok := graph[bare]; !ok {
				return fmt.Errorf("序列 %q 调用了不存在的序列 %q（用 seq_list 看可用序列）", name, "#"+bare)
			}
		}
	}
	// 环检测（三色 DFS），错误里带**环路径**便于定位
	const (
		white = 0 // 未访问
		gray  = 1 // 在栈上
		black = 2 // 已完成
	)
	color := make(map[string]int, len(graph))
	var path []string
	var dfs func(n string) error
	dfs = func(n string) error {
		color[n] = gray
		path = append(path, "#"+n)
		for _, bare := range graph[n] {
			switch color[bare] {
			case gray:
				// 找到环：从 path 里第一次出现 bare 处截断，给出完整环
				// path 里存的是带 # 前缀的显示名，graph 的键是裸名
				ring := path
				for i, p := range path {
					if p == "#"+bare {
						ring = path[i:]
						break
					}
				}
				return fmt.Errorf("跨序列调用成环: %s → #%s",
					strings.Join(ring, " → "), bare)
			case white:
				if err := dfs(bare); err != nil {
					return err
				}
			}
		}
		path = path[:len(path)-1]
		color[n] = black
		return nil
	}
	for n := range graph {
		if color[n] == white {
			if err := dfs(n); err != nil {
				return err
			}
		}
	}
	return nil
}

// CheckNew 在**保存前**校验一条新序列：组内/跨序列引用是否存在。
//
// 分两步：先查**同序列内**的 seq_call 目标（组名）是否存在，再查
// **跨序列**目标是否已存在（存盘之后才能查全图，故由 Save 后的
// CheckGraph 负责）。
func (s *Store) CheckNew(seq *Sequence) error {
	groupNames := map[string]bool{}
	for _, g := range seq.Groups {
		groupNames[g.Name] = true
	}
	for _, g := range seq.Groups {
		for i, t := range g.Tools {
			if t.Tool != "seq_call" && t.Tool != "seq_when_call" {
				continue
			}
			tgt, _ := t.Args["target"].(string)
			if strings.TrimSpace(tgt) == "" {
				return fmt.Errorf("group %q 第 %d 个工具的 seq_call 缺少 target", g.Name, i+1)
			}
			if strings.HasPrefix(tgt, "#") {
				bare := strings.TrimPrefix(tgt, "#")
				if bare == seq.Name {
					return fmt.Errorf("序列 %q 调用了自身（会造成无限递归）", seq.Name)
				}
				// ⚠️ 跨序列目标**不在这里**要求存在：互调的两条序列
				// 谁也存不下来（A 要 B 先在、B 要 A 先在），是设计上死锁。
				// 存在性与环统一由 CheckGraph 在保存后兜底。
				continue
			}
			if !groupNames[tgt] {
				return fmt.Errorf("group %q 第 %d 个工具调用了不存在的 group %q"+
					"（本序列现有：%s）", g.Name, i+1, tgt, joinNames(groupNames))
			}
		}
	}
	return nil
}

func joinNames(m map[string]bool) string {
	if len(m) == 0 {
		return "（无）"
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// loadGraph 返回调用图，必要时从磁盘重建。
//
// 只在**缓存未建立**时重建；之后由 Save 增量维护。
// 外部直接改文件（删了序列文件、改了内容）会让缓存过期 ——
// Delete 已显式失效，跨进程改动不属于本 Store 的职责范围。
func (s *Store) loadGraph() map[string][]string {
	s.mu.RLock()
	g := s.graph
	s.mu.RUnlock()
	if g != nil {
		return g
	}

	names := s.List()
	g2 := make(map[string][]string, len(names))
	for _, n := range names {
		seq, err := s.Load(n)
		if err != nil {
			// 读不出来的序列（并发删除/损坏）不参与图检查，
			// 但不能因此让整次检查失败 —— 真正的错误会在 Load 时报。
			continue
		}
		g2[n] = edgesOf(seq)
	}
	s.mu.Lock()
	s.graph = g2
	s.mu.Unlock()
	return g2
}
