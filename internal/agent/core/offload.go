package core

// 积压任务的**自动转投**：主 agent 长时间忙时，把排队中的任务改投给内核拉起的
// 驻留子，并在原队列位置留一条"已转投"提示。
//
// 为什么要这个（用户 2026-09-19 提出的实际需求）：
//   实测主 agent 被一条长任务占住时（当天现场：12 分 8 秒、69 次工具调用），
//   后来的 QQ 消息全部以 "level insufficient" 排进中断队列干等 —— 同级中断
//   不能抢占同级运行任务（scheduler.canPreempt），只能等前一个跑完。
//   而内核明明有驻留子（独立 agent + 独立调度器 + 共享输出通道视图）可以并行干活。
//
// 与设计文档 §7 的关系（**这是刻意的例外，必须显式记录**）：
//   设计原文写「创建/销毁/回收/查看/发送是父可调用的原语；**决策在父的模型手里**——
//   内核不替父决定」。本特性让**内核**主动创建并使用驻留子，属于对该原则的例外。
//   之所以可接受：父此刻正忙（无法做决策），而积压任务**本来就是空的**——
//   转投只是把"排队干等"换成"有人在做"，不改变任何已提交决策的语义。
//   若不做例外，这个能力就只能由父的模型发起，而它恰恰是忙不过来的那个。

import (
	"fmt"
	"log"
	"strings"
	"time"

	"runtime/debug"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// OffloadOptions 是自动转投的判定与执行参数。
type OffloadOptions struct {
	// BusyAfter：运行任务已持续多久算"长时间工作"（0 = 用默认）。
	BusyAfter time.Duration
	// MinPending：至少要积压多少条才值得拉起驻留子（0 = 用默认）。
	MinPending int
	// MaxResidents：为转投而拉起的驻留子上限（0 = 用默认）。
	MaxResidents int
	// Enabled 为 false 时完全关闭（默认关：见 DefaultOffloadOptions 的说明）。
	Enabled bool
}

// 默认参数。
//
// 为什么默认**关闭**：自动拉起是"内核替父做决策"，改变的是系统行为而非修 bug；
// 且它会让日志/账单里凭空多出一个 agent 在干活。默认关闭、由部署方显式打开，
// 与「显式才是特权」（scheduler.DefaultLevel 的同一条理由）一致。
const (
	defaultOffloadBusyAfter   = 5 * time.Minute
	defaultOffloadMinPending  = 3
	defaultOffloadMaxResident = 2
)

// DefaultOffloadOptions 返回默认参数（Enabled=false）。
func DefaultOffloadOptions() OffloadOptions {
	return OffloadOptions{
		BusyAfter:    defaultOffloadBusyAfter,
		MinPending:   defaultOffloadMinPending,
		MaxResidents: defaultOffloadMaxResident,
		Enabled:      false,
	}
}

func (o OffloadOptions) normalized() OffloadOptions {
	if o.BusyAfter <= 0 {
		o.BusyAfter = defaultOffloadBusyAfter
	}
	if o.MinPending <= 0 {
		o.MinPending = defaultOffloadMinPending
	}
	if o.MaxResidents <= 0 {
		o.MaxResidents = defaultOffloadMaxResident
	}
	return o
}

// offloadNotice 是替换被转投任务的那条提示的正文。
//
// 它必须**自己说清是系统做的**：用户看到队列里出现一条没人发过的消息时，
// 唯一能解释这件事的就是这句话本身。
func offloadNotice(count int, residentID string) string {
	return fmt.Sprintf(
		"[系统] %d 条积压任务已转投给驻留子 agent %s 处理（主 agent 正忙于长任务，"+
			"内核为它们拉起了独立 agent 并行执行）。它们的回复会由 %s 直接发到对应通道；"+
			"本提示仅用于说明「那几条消息不会再由你处理」，无需为它们采取任何行动。",
		count, residentID, residentID)
}

// offloadCandidate 是一条可被转投的排队任务。
//
// 只有**纯排队输入**（TaskQueued + KindInput）可转投：
//   - 中断任务带级别语义（可能正在等待抢占时机），转投会打乱中断阶梯；
//   - self 任务是内核内部记账（记忆整理等），与父的记忆面绑定，不能换 agent。
type offloadCandidate struct {
	Event *agentIO.InputEvent
}

// drainInboxToQueue 把 io 输入 channel 里**已经到达但尚未被搬运**的输入
// 搬进就绪队列（非阻塞；取空为止）。
//
// 它与 pumpInbox 做的事一样，但**不要求 hasRoom**：pumpInbox 在队列满时
// 会停下以保留背压，而转投场景恰恰是「队列空/不满、但输入堵在 channel 里」
// （因为调度器正忙于执行任务、根本回不到 pumpInbox）。
//
// 队列上限仍由 enqueue 把关：满了就停下，超出的输入留在 channel 里。
func (a *Agent) drainInboxToQueue() {
	for {
		select {
		case evt := <-a.io.InputChan():
			if !a.sched.enqueue(newInputTask(evt)) {
				// 队列满：放不进去。不能丢，也不能阻塞（我们是后台 goroutine，
				// 阻塞会把这个循环永远卡住）——给同步调用方一个终态后丢弃，
				// 与 pumpInbox 的 queue_full 处置一致。
				a.sched.noteBackpressure()
				a.emitSkippedReply(evt, "queue_full")
				return
			}
		default:
			return
		}
	}
}

// residentInputChannel 返回"父给某个子投递输入"用的 inputch 名。
//
// 与 SendToResident 用的是同一个（sub/<id>）：子是**不配插件 inputch** 的
// （opts.InputChs 为空），它的入站口就只有父给它的这一条，因此必须与
// SendToResident 保持一致，否则转投的消息会落到一个父不知道的通道名上。
func residentInputChannel(id string) string { return "sub/" + id }

// offloadPendingTasks 检查是否需要转投，需要则拉起/复用一个驻留子并搬运任务。
//
// 返回实际转投的任务条数（0 = 未触发/未转投）。
//
// ❗并发前提：本函数会被 offloadLoop 在**任务执行期间**调用（那正是它的意义），
// 因此它与 schedulerLoop 是并发跑的。所有对队列的读写都经 scheduler 的锁，
// 而"取走哪些任务"与"放回什么"都在同一次锁内完成，不存在丢任务的窗口。
func (a *Agent) offloadPendingTasks(opts OffloadOptions) int {
	opts = opts.normalized()
	if !opts.Enabled {
		return 0
	}

	// ① 判定：运行任务是否已忙够久。运行任务为空说明压根不忙，不做。
	running := a.sched.runningTask()
	if running == nil {
		return 0
	}
	busyFor := a.sched.runningFor()
	if busyFor < opts.BusyAfter {
		return 0
	}

	// ② 先把积压从 io 的输入 channel **搬进就绪队列**。
	//
	// ！！这是本特性最容易写错的一步（我第一版就错了，写完后线上实测永不触发）：
	// schedulerLoop 是**同步执行**任务的，所以「正忙」期间它根本不会回到循环顶部
	// 去调 pumpInbox —— 这时后到的输入全部堆在 io.inputCh（容量 256）里，
	// **压根没进 sched.queue**。只数 s.queue 会得到 0，转投就永远不触发。
	//
	// 仓库里早记过同一个坑：armStop 的注释写着「pending 是还没被 pumpInbox 搬进
	// 队列的那一段……只数 s.queue 会得到 0（实测），配额随之失效」。
	// 这里必须在同一层把这件事做对，而不是重犯。
	a.drainInboxToQueue()

	// ③ 收集可转投的排队任务；不够量就不值得拉起一个 agent。
	cands := a.sched.takeQueuedInputs(opts.MinPending)
	if len(cands) == 0 {
		return 0
	}

	// ③ 找或拉起一个"转投专用"驻留子。
	residentID, err := a.ensureOffloadResident(opts)
	if err != nil {
		// 拉不起来就把任务**放回队列**，绝不能丢：丢一条输入比多处理一条更糟
		// （与 routeInputByOwner 的兜底同一条理由）。
		a.sched.requeueFront(cands)
		log.Printf("[offload] 无法为 %d 条积压任务准备驻留子，已放回队列: %v", len(cands), err)
		return 0
	}

	// ④ 搬运：逐条投进子的 inputch。
	//
	// 注意这里**逐条转发原文**而不是打包成一条：任务本身带 Source/OutputChannel
	// 等路由信息，打包会让子无法把回复发回正确的通道（qq 私聊 vs 群聊不同）。
	var moved int
	for _, c := range cands {
		if c.Event == nil {
			continue
		}
		if err := a.forwardInputToResident(residentID, c.Event); err != nil {
			// 某条投不进去：放回原队列，其余继续（部分成功好过全部回滚）。
			a.sched.requeueFront([]offloadCandidate{c})
			log.Printf("[offload] 转发任务给驻留子 %s 失败，已放回队列: %v", residentID, err)
			continue
		}
		moved++
	}
	if moved == 0 {
		return 0
	}

	// ⑤ 在**原队列位置**留下提示（用户要求的那条说明）。
	//
	// 为什么留在队列里而不是只记日志：队列顺序就是主 agent 接下来要处理的事；
	// 用户看会话记录时，需要在这里就看到"那几条去哪儿了"，而不是去翻内核日志。
	a.sched.requeueFront([]offloadCandidate{
		{Event: a.syntheticEvent(offloadNotice(moved, residentID))},
	})

	log.Printf("[offload] 主 agent 已忙 %s，把 %d 条积压任务转投给驻留子 %s（队列留 1 条说明）",
		busyFor.Truncate(time.Second), moved, residentID)
	return moved
}

// forwardInputToResident 把一条输入原文投给指定驻留子的 inputch。
//
// 走 InjectInputTo（排队输入，非中断）：转投的是"待办工作"，不是"打断子"。
// 子的 io 上有 inputRouter（routeInputByOwner），但投递目标是**它自己的** inputch
// 且 Owner 就是它，因此不会被再次路由走。
func (a *Agent) forwardInputToResident(residentID string, evt *agentIO.InputEvent) error {
	a.residentMu.Lock()
	rc := a.residents[residentID]
	a.residentMu.Unlock()
	if rc == nil || rc.agent == nil || rc.agent.io == nil {
		return fmt.Errorf("驻留子 %s 不存在或不可用", residentID)
	}

	payload := map[string]interface{}{}
	for k, v := range evt.Payload {
		payload[k] = v
	}
	// 带上来源线索，让子知道这条不是父当前任务的续接，而是转投的独立请求。
	payload["offloaded_from"] = string(a.id)
	payload["offloaded_at"] = time.Now().Format(time.RFC3339)

	ch := residentInputChannel(residentID)
	rc.agent.io.InjectInputToOpts(evt.Source, ch, evt.Type, payload, agentIO.InjectOptions{})
	return nil
}

// ensureOffloadResident 返回一个可用于转投的驻留子 id，必要时拉起一个新的。
//
// 复用规则：优先复用"内核为转投而建"且仍 running、还没满的驻留子；
// 都不可用时（在 MaxResidents 内）新建一个。
func (a *Agent) ensureOffloadResident(opts OffloadOptions) (string, error) {
	a.residentMu.Lock()
	var reusable []string
	for id, rc := range a.residents {
		if rc == nil || !rc.offloadOwned {
			continue
		}
		rc.mu.Lock()
		state := rc.state
		rc.mu.Unlock()
		if state == "running" {
			reusable = append(reusable, id)
		}
	}
	a.residentMu.Unlock()

	// 复用：按 id 稳定排序后取第一个，避免每次挑到不同的子（可预测性）。
	if len(reusable) > 0 {
		sortStrings(reusable)
		return reusable[0], nil
	}

	// 计数：只为转投而建的子是否已达上限（人工建的子不计入）。
	a.residentMu.Lock()
	owned := 0
	for _, rc := range a.residents {
		if rc != nil && rc.offloadOwned {
			owned++
		}
	}
	a.residentMu.Unlock()
	if owned >= opts.MaxResidents {
		return "", fmt.Errorf("转投专用驻留子已达上限 %d", opts.MaxResidents)
	}

	id := fmt.Sprintf("offload-%d", time.Now().Unix())
	if a.dataDir == "" {
		return "", fmt.Errorf("未配置 DataDir，无法为驻留子分配 temp 图库路径")
	}
	info, err := a.SpawnResident(ResidentOptions{
		ID: id,
		// 不配 inputch：它是内核的**干活** agent，不接收任何插件的用户输入
		// （用户要求"不配输入通道"）。它只由父经 sub/<id> 投喂任务。
		InputChs: nil,
		// 全部输出通道：它要能把结果发回 qq/webui 等正确通道
		// （用户要求"持有全部输出通道"）。nil = 完整授权。
		AllowedOutputs: nil,
		TempPath:       a.residentTempPath(id),
		OffloadOwned:   true,
	})
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

// residentTempPath 计算某个驻留子的 temp 图记忆路径（与既有约定一致）。
func (a *Agent) residentTempPath(id string) string {
	return strings.TrimRight(a.dataDir, "/") + "/residents/" + id + "/graph.db"
}

// syntheticEvent 造一条"内核自己发的"输入事件（用于队列里的转投说明）。
//
// Source 取 kernel：这条消息不是任何用户发来的，日志与用户界面里都应看得出。
// 不带 ResponseCh：没有同步调用方在等它（它只是给主 agent 看的一句说明）。
func (a *Agent) syntheticEvent(text string) *agentIO.InputEvent {
	return &agentIO.InputEvent{
		Source:        "kernel",
		Type:          "text",
		OutputChannel: "kernel",
		Payload:       map[string]interface{}{"content": text},
	}
}

// sortStrings 是一个不引入 sort 依赖的小排序（候选集极小，插入排序足够）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// offloadLoop 周期性检查「主 agent 是否被长任务占住 + 是否有积压」。
//
// 为什么必须是**独立 goroutine**而不是 schedulerLoop 里的一步：
//
//	schedulerLoop 是**同步执行**任务的（executeNewTask 会一直阻塞到任务结束），
//	所以「正忙」期间它根本不会回到循环顶部 —— 把检查放在那里等于永不触发。
//	这正是本特性存在的理由（主 agent 忙时无人处理积压），不能在实现上重犯。
//
// 检查间隔取 BusyAfter 的 1/5（不低于 1 秒）：保证在跨过阈值后能在合理时间内
// 触发，又不至于空转打日志。
func (a *Agent) offloadLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] offloadLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.offloadLoop()
		}
	}()

	if !a.offload.Enabled {
		return // 未启用：不占 goroutine，也不打日志（默认关闭是常态）
	}
	// 只让**根 agent** 做转投：驻留子自己也可能忙，但让子再去拉孙子会形成
	// 无界增殖（每层都能拉 MAX 个），而积压的源头是根那一条调度链。
	if a.parentID != "" {
		return
	}

	interval := a.offload.BusyAfter / 5
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.offloadPendingTasks(a.offload)
		case <-a.ctx.Done():
			return
		}
	}
}
