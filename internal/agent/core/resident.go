package core

// 驻留式子 agent 的**生命周期与控制面**（设计 docs/zh/resident-subagent-design.md §7/§9/§10）。
//
//	父 ──创建/发送消息/查看/压缩/回收/销毁──► 驻留子
//	子 ──主动消息(L3) / contextfull(L4)──► 父
//
// 层级关系：
//   - 父持**登记表**（residents），它是查看·发送·压缩·回收·销毁的寻址依据；
//   - 父 `Stop()` ⇒ 销毁全部子（**不留孤儿**）；
//   - 子的 `KernelSource` = 父；父则把子的 contextfull 当**内核级事件**（raiseKernelInterrupt）上报给自己；
//   - 子持有自己的 **inputch 处理表**（父 pull，不打断子）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

// InputchRecord 是 inputch 处理表的一条记录（子持有，父 pull）。
type InputchRecord struct {
	InputCh   string    `json:"inputch"`
	At        time.Time `json:"at"`
	Proactive bool      `json:"proactive"` // true = 子主动写入；false = 系统自动写
	Text      string    `json:"text"`
}

// ResidentOptions 是创建一个驻留子的参数（父的"创建"动作）。
type ResidentOptions struct {
	// ID 是子 agent 的 id（同时是登记表的键、跨 agent 寻址的依据）。
	ID string
	// TaskPrompt 是在固定提示词之上注入的**任务提示词**。
	TaskPrompt string
	// InputChs 是**划入**给这个子的 inputch（单位 = inputch；可来自同一插件的多个）。
	InputChs []string
	// AllowedOutputs 是授权给它的输出通道集合（nil/空 = 完整授权）。
	AllowedOutputs []string
	// Capacity 是划入 inputch 的队列容量（0 = 内核默认）。
	Capacity int
	// TempPath 是它 temp 图记忆的存储路径（必填；与子同生共死）。
	TempPath string
}

// ResidentInfo 是父对某个驻留子的可查询状态（登记表条目 + 状态面摘要）。
type ResidentInfo struct {
	ID             string          `json:"id"`
	State          string          `json:"state"`
	InputChs       []string        `json:"inputchs"`
	AllowedOutputs []string        `json:"allowed_outputs"`
	Rounds         int             `json:"rounds"`
	ContextFull    bool            `json:"context_full"`
	CreatedAt      time.Time       `json:"created_at"`
	TableSize      int             `json:"table_size"`
	Table          []InputchRecord `json:"table,omitempty"`
}

type residentChild struct {
	id        string
	agent     *Agent
	light     *memory.LightMemory
	mainRO    *memory.GraphDB
	tempPath  string
	dir       string
	inputChs  []string
	allowed   []string
	createdAt time.Time

	mu    sync.Mutex
	state string // running | contextfull | stopped
}

// mainGraph 返回父自己的完整图记忆库（驻留子的受限句柄由它派生）。
func (a *Agent) mainGraph() *memory.GraphDB { return a.memory }

// SpawnResident 创建一个驻留子（父的"创建"动作）。
func (a *Agent) SpawnResident(opts ResidentOptions) (ResidentInfo, error) {
	if strings.TrimSpace(opts.ID) == "" {
		return ResidentInfo{}, fmt.Errorf("驻留子必须有 id")
	}
	if strings.TrimSpace(opts.TempPath) == "" {
		return ResidentInfo{}, fmt.Errorf("驻留子必须给出 temp 图记忆路径")
	}
	main := a.mainGraph()
	if main == nil {
		return ResidentInfo{}, fmt.Errorf("父没有图记忆，无法为驻留子提供主库只读视图")
	}

	a.residentMu.Lock()
	if a.residents == nil {
		a.residents = map[string]*residentChild{}
	}
	if _, dup := a.residents[opts.ID]; dup {
		a.residentMu.Unlock()
		return ResidentInfo{}, fmt.Errorf("驻留子 %s 已存在", opts.ID)
	}
	a.residentMu.Unlock()

	// ① 轻量内核的记忆装配：主库**受限句柄**（结构上写不进）+ 自己的 temp 实例。
	// temp 目录由内核创建（调用方只给路径）——与子同生共死，销毁时整目录丢弃。
	if dir := filepath.Dir(opts.TempPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return ResidentInfo{}, fmt.Errorf("创建 temp 目录: %w", err)
		}
	}
	mainRO, err := memory.OpenGraphDBReadOnly(main.Path())
	if err != nil {
		return ResidentInfo{}, fmt.Errorf("打开主库受限句柄: %w", err)
	}
	light, err := memory.NewLightMemory(mainRO, opts.TempPath, true)
	if err != nil {
		_ = mainRO.Close()
		return ResidentInfo{}, err
	}

	// ② 划入 inputch（登记表里记归属；一个插件的多个 inputch 可分别划给不同子）。
	if reg := a.io.ChannelRegistry(); reg != nil {
		for _, ch := range opts.InputChs {
			if err := reg.Assign(ch, opts.ID, opts.Capacity); err != nil {
				_ = light.Close()
				_ = mainRO.Close()
				return ResidentInfo{}, fmt.Errorf("划入 inputch %s: %w", ch, err)
			}
		}
	}

	// ③ 子的 io：**独立**的 IOManager（自己的输入通道入口），但共享通道登记表，
	// 并把父的 io 挂成"上级"——**输出通道（io 里的 Device）由插件登记在父的 io 上**，
	// 子若不继承这张视图，`output_send__<通道>` 一律被判"通道不存在或不可用"、
	// `output_list_channels` 为空、连 `output_send__*` 工具都不会生成
	// （现场联调：父侧通道装载完整、子侧 childIO 空壳）。
	// 回退是实时的（设备随资源生灭），授权仍由 opts.AllowedOutputs 白名单把关。
	childIO := agentIO.NewIOManager()
	if reg := a.io.ChannelRegistry(); reg != nil {
		childIO.SetChannelRegistry(reg)
	}
	childIO.SetParentIO(a.io)

	parentID := string(a.id)
	child := New(AgentConfig{
		ID:              types.AgentID(opts.ID),
		Provider:        a.provider,
		ProviderManager: a.providerManager,
		IO:              childIO,
		StageHost:       a.stageHost,
		LightMemory:     light, // 轻量内核：只有图记忆共同面
		AllowedOutputs:  opts.AllowedOutputs,
		KernelSource:    parentID, // 子的 L4 只属于父
		ParentID:        parentID,
		TaskPrompt:      opts.TaskPrompt,
	})

	rc := &residentChild{
		id: opts.ID, agent: child, light: light, mainRO: mainRO,
		tempPath: opts.TempPath, dir: filepath.Dir(opts.TempPath),
		inputChs:  append([]string(nil), opts.InputChs...),
		allowed:   append([]string(nil), opts.AllowedOutputs...),
		createdAt: time.Now(), state: "running",
	}

	// ④ 子 → 父的主动消息（**L3 中断**，带子标识）：投进父的 inputch。
	parentInCh := a.residentInboundChannel(opts.ID)
	child.notifyParent = func(text string) {
		a.io.InjectInterruptTextOpts(opts.ID, parentInCh, text,
			agentIO.InjectOptions{Priority: "L3"})
	}
	// ⑤ 子的 contextfull → 父侧的**内核级事件**（L4，带子标识）。
	child.onContextFull = func() { a.handleChildContextFull(rc) }

	a.residentMu.Lock()
	a.residents[opts.ID] = rc
	a.residentMu.Unlock()

	child.Start()

	// ⑥ create 即开工：把任务提示词作为**第一条排队输入**投给子。
	//
	// 为什么必须在这里投：TaskPrompt 只进子的系统提示词（"你是谁、要做什么"），
	// 而**不会**让子跑起来 —— 实测现象是子启动后 rounds=0、永远待机
	// （日志 `[agent] r1 started, waiting for IO interrupts` 之后无事发生）。
	// 走排队输入（非中断）：创建是"安排工作"，不是"打断它正在做的事"。
	if strings.TrimSpace(opts.TaskPrompt) != "" {
		child.io.InjectInputTo(a.residentParentSource(), parentInCh, "text",
			map[string]interface{}{"content": opts.TaskPrompt})
	}
	return rc.info(), nil
}

// residentParentSource 是"父给子投递"的输入来源名（子的视角里能看出是谁发的）。
func (a *Agent) residentParentSource() string { return "parent/" + string(a.id) }

// residentInboundChannel 是"父接收某个子的消息"的 inputch 名（登记进登记表可见）。
func (a *Agent) residentInboundChannel(childID string) string {
	ch := "child/" + childID
	if reg := a.io.ChannelRegistry(); reg != nil {
		// 归属父自己：它是父的入站 inputch。
		_ = reg.Register(agentIO.InputChannel{Name: ch, Plugin: "resident", Owner: string(a.id)})
	}
	return ch
}

// DestroyResident 立刻销毁一个驻留子并从登记表移除（父的"销毁"动作；不收割）。
//
// 销毁是父**随时**可做的；父退出时由 StopResidents 对全部子执行。
func (a *Agent) DestroyResident(id string) error {
	a.residentMu.Lock()
	rc, ok := a.residents[id]
	if ok {
		delete(a.residents, id)
	}
	a.residentMu.Unlock()
	if !ok {
		return fmt.Errorf("驻留子 %s 不存在", id)
	}
	a.teardownResident(rc)
	return nil
}

// teardownResident 停内核、放通道、丢 temp（销毁与回收共用）。
func (a *Agent) teardownResident(rc *residentChild) {
	rc.mu.Lock()
	rc.state = "stopped"
	rc.mu.Unlock()

	rc.agent.Stop() // 停子的调度器（取消其运行中的任务）
	if rc.light != nil {
		_ = rc.light.Close() // 关掉 temp 实例
	}
	if rc.mainRO != nil {
		_ = rc.mainRO.Close()
	}
	// temp 与子同生共死：连同目录一起丢弃（回收/销毁都不保留）。
	if rc.dir != "" && strings.Contains(rc.tempPath, rc.id) {
		_ = os.RemoveAll(rc.dir)
	}
	// 归还划入的 inputch（归属清空 ⇒ 回到"未分配"，可再分配）。
	if reg := a.io.ChannelRegistry(); reg != nil {
		for _, ch := range rc.inputChs {
			_ = reg.Assign(ch, "", 0)
		}
	}
}

// StopResidents 销毁全部驻留子（父退出时必须；不留孤儿）。
func (a *Agent) StopResidents() int {
	a.residentMu.Lock()
	all := make([]*residentChild, 0, len(a.residents))
	for _, rc := range a.residents {
		all = append(all, rc)
	}
	a.residents = map[string]*residentChild{}
	a.residentMu.Unlock()
	for _, rc := range all {
		a.teardownResident(rc)
	}
	return len(all)
}

// Residents 返回登记表的快照（按 id 排序，便于断言与展示稳定）。
func (a *Agent) Residents() []ResidentInfo {
	a.residentMu.Lock()
	all := make([]*residentChild, 0, len(a.residents))
	for _, rc := range a.residents {
		all = append(all, rc)
	}
	a.residentMu.Unlock()
	out := make([]ResidentInfo, 0, len(all))
	for _, rc := range all {
		out = append(out, rc.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ResidentTable 是父**查看**子的 inputch 处理表（pull，不打断子）。
func (a *Agent) ResidentTable(id string) ([]InputchRecord, error) {
	a.residentMu.Lock()
	rc, ok := a.residents[id]
	a.residentMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("驻留子 %s 不存在", id)
	}
	return rc.agent.inputchTableSnapshot(), nil
}

// SendToResident 是父的"发送消息"：经输出通道寻址到该子的 inputch，
// 对子而言是 **L4 中断**（取消当前状态 + 插入新消息）。
func (a *Agent) SendToResident(id, text string) error {
	a.residentMu.Lock()
	rc, ok := a.residents[id]
	a.residentMu.Unlock()
	if !ok {
		return fmt.Errorf("驻留子 %s 不存在", id)
	}
	ch := "sub/" + id
	if len(rc.inputChs) > 0 {
		ch = rc.inputChs[0]
	}
	// 来源 = 父；子的 KernelSource 是父 ⇒ 在子的阶梯上这是合法的 L4。
	rc.agent.io.InjectInterruptTextOpts(string(a.id), ch, text,
		agentIO.InjectOptions{Priority: "L4"})
	return nil
}

// CompressResident 是父的"压缩"（**保留语义**）：压上下文 + **清理处理表**，子继续存在。
func (a *Agent) CompressResident(id string) (int, error) {
	a.residentMu.Lock()
	rc, ok := a.residents[id]
	a.residentMu.Unlock()
	if !ok {
		return 0, fmt.Errorf("驻留子 %s 不存在", id)
	}
	dropped := rc.agent.context.TrimKeepRecent(residentKeepRecent)
	rc.agent.clearInputchTable() // 处理表记的是被压掉那段窗口的逐轮处理 ⇒ 必须清
	rc.agent.resetContextFull()
	rc.mu.Lock()
	rc.state = "running"
	rc.mu.Unlock()
	return dropped, nil
}

// ReclaimResident 是父的"回收"（**取消语义**）：父读子的 temp → 选记录 → 合入 main
// → 丢弃 temp → **取消**该驻留子。
//
// keep 由父决定"哪些纳入记忆"；为 nil 时表示全部合入。
func (a *Agent) ReclaimResident(id string, keep func([]InputchRecord, []memory.Triple) []memory.Triple) (ResidentInfo, error) {
	a.residentMu.Lock()
	rc, ok := a.residents[id]
	a.residentMu.Unlock()
	if !ok {
		return ResidentInfo{}, fmt.Errorf("驻留子 %s 不存在", id)
	}
	info := rc.info()

	var promoted int
	// 收割：读 temp 的全部活跃三元组（比通过图记录选择更直接）。
	if rc.light != nil && rc.light.Temp() != nil && a.mainGraph() != nil {
		exported, err := rc.light.Temp().ExportTriples(0)
		if err != nil {
			return info, fmt.Errorf("读取子 temp 失败: %w", err)
		}
		selected := exported
		if keep != nil {
			selected = keep(info.Table, exported)
		}
		if len(selected) > 0 {
			if _, _, err := a.mainGraph().Commit(selected, "reclaim/"+id, 0); err != nil {
				return info, fmt.Errorf("合入主记忆失败: %w", err)
			}
			promoted = len(selected)
		}
	}

	// 收割完成 ⇒ 取消该驻留子（回收是取消语义，≠ 压缩）。
	if err := a.DestroyResident(id); err != nil {
		return info, err
	}
	info.State = fmt.Sprintf("reclaimed(promoted=%d)", promoted)
	return info, nil
}

// handleChildContextFull 把子的 contextfull 当**内核级事件**上报给父自己：
// 父侧 L4 中断（带子标识）—— 只推信号，细节靠"查看"拉状态面。
func (a *Agent) handleChildContextFull(rc *residentChild) {
	rc.mu.Lock()
	rc.state = "contextfull"
	rc.mu.Unlock()
	a.raiseKernelInterrupt("child/"+rc.id, "kernel",
		fmt.Sprintf("内核事件：驻留子 %s 上下文已满（contextfull）。请【查看】其状态面后决定【压缩】/【回收】/【销毁】。", rc.id))
}

// checkContextFull 判断本 agent 的**积累上下文是否已经装不下窗口**，是则触发一次 contextfull。
//
// ❗判据为什么不能写成"估算拼好的 f.Msgs"：`buildMessages` 拿到的 `budget.ContextTokens`
// 本身就是按 `targetUsage = 0.8 × 窗口` 算出来的，时间线**在拼进消息之前就被预算裁过**了。
// 于是 `f.Msgs` 的规模结构上封顶在 ~80% 窗口 —— 对 90% 阈值而言是**永远不成立**的判据
// （写测试时我用一个比系统提示词还小的窗口才勉强越过线，那等于什么都没测）。
//
// 正确的事是"**要被裁了**"：拿**未裁剪**的积累上下文（`a.context` 的全部事件）估算，
// 它超过窗口阈值就说明下一轮必须丢事件 ⇒ 这就是 contextfull。
//
// 只对**驻留子**生效（只有它们设了 onContextFull）。
func (a *Agent) checkContextFull(f *TaskFrame) {
	if a.onContextFull == nil || a.ctxFullSignaled {
		return
	}
	max := 0
	if a.provider != nil {
		max = a.provider.MaxContextTokens()
	}
	if max <= 0 {
		max = defaultMaxContextTokens
	}
	if a.context == nil {
		return
	}
	// 未裁剪的积累上下文规模。
	acc := 0
	for _, e := range a.context.Recent(0) {
		acc += EstimateTokens(e.Input) + EstimateTokens(e.Response)
	}
	if float64(acc) < float64(max)*contextFullRatio {
		return
	}
	a.ctxFullSignaled = true
	a.onContextFull()
}

func (a *Agent) resetContextFull() { a.ctxFullSignaled = false }

// ---- 子侧：inputch 处理表（子持有，父 pull） ----

// recordInputchNote 是子**主动写入**本轮 inputch 的处理信息（工具 inputch_note）。
func (a *Agent) recordInputchNote(text string) {
	a.tableMu.Lock()
	defer a.tableMu.Unlock()
	a.inputchPending = &InputchRecord{
		InputCh: a.currentInputch, At: time.Now(), Proactive: true, Text: text,
	}
}

// autoRecordInputch 是**系统自动写**兜底：本轮未主动写时，把该轮 inputch 的处理信息写入。
// 保证每一轮必有记录，父不会看到空洞。
func (a *Agent) autoRecordInputch(f *TaskFrame) {
	if f == nil {
		return
	}
	a.tableMu.Lock()
	defer a.tableMu.Unlock()
	if a.inputchPending != nil {
		rec := *a.inputchPending
		a.inputchPending = nil
		rec.InputCh = firstNonEmpty(rec.InputCh, a.currentInputch)
		a.inputchTable = append(a.inputchTable, rec)
		return
	}
	text := fmt.Sprintf("轮次完成：输入=%s", truncateStr(f.Input, 80))
	if f.Response != "" {
		text += "；产出=" + truncateStr(f.Response, 120)
	}
	a.inputchTable = append(a.inputchTable, InputchRecord{
		InputCh: a.currentInputch, At: time.Now(), Proactive: false, Text: text,
	})
}

func (a *Agent) inputchTableSnapshot() []InputchRecord {
	a.tableMu.Lock()
	defer a.tableMu.Unlock()
	return append([]InputchRecord(nil), a.inputchTable...)
}

func (a *Agent) clearInputchTable() {
	a.tableMu.Lock()
	defer a.tableMu.Unlock()
	a.inputchTable = nil
	a.inputchPending = nil
}

// ---- 子侧：主动向父发消息（L3） ----

func (a *Agent) notifyParentFrom(text string) string {
	if a.notifyParent == nil {
		return "本 agent 没有上级，无法发送消息"
	}
	if strings.TrimSpace(text) == "" {
		return "消息内容不能为空"
	}
	a.notifyParent(text)
	return "已发送给主 agent"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// residentKeepRecent 是压缩时保留的最近事件条数。
const residentKeepRecent = 20

// contextFullRatio 是触发 contextfull 的占比：积累上下文超过窗口的这个比例就报。
// 取 0.9 而不是 1.0：留一点余量，让父 agent 在"下一次必须丢事件"之前就能决策
// （压缩 / 回收 / 销毁），而不是等已经丢了再报。
const contextFullRatio = 0.9

// defaultMaxContextTokens 是 provider 未报告窗口时的兜底（与 ComputeTokenBudget 一致）。
const defaultMaxContextTokens = 32768

func (rc *residentChild) info() ResidentInfo {
	rc.mu.Lock()
	state, full := rc.state, rc.state == "contextfull"
	rc.mu.Unlock()
	table := rc.agent.inputchTableSnapshot()
	info := ResidentInfo{
		ID: rc.id, State: state, InputChs: append([]string(nil), rc.inputChs...),
		AllowedOutputs: append([]string(nil), rc.allowed...),
		ContextFull:    full, CreatedAt: rc.createdAt, TableSize: len(table),
	}
	if len(table) > 0 {
		info.Table = table
	}
	return info
}

// MarshalResidentInfo 便于工具输出（单工具多视图用 JSON 视图）。
func MarshalResidentInfo(info ResidentInfo) string {
	b, err := json.Marshal(info)
	if err != nil {
		return fmt.Sprintf("%+v", info)
	}
	return string(b)
}
