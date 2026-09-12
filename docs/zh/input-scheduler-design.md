# 输入调度器设计（四级优先级 · 可抢占 · 现场保存）

> 分支：`feature/input-semantics`
> 状态：**设计稿 v1**（待确认项见 §12，未确认处按 §12 的「默认取值」推进）
> 影响面：`internal/agent/core`、`internal/agent/io`、`internal/sdk`（**仅内部**）
> 公开 SDK：**v1 不改**（`third_party/homeagent-sdk/sdk/` diff 必须保持为 0，理由见 §13）

---

## 1. 背景：现状与要解决的问题

现有两条输入语义（见审查结论），都建立在**串行 `eventLoop`** 之上：

| 语义 | 入口 | 路径 |
|---|---|---|
| 排队 | `InjectText` / `InjectInput*` | `io.inputCh` → `eventLoop` → `processInput` → `process()` |
| 中断 | `InjectInterruptText` / `InjectInterrupt*` | `io.interruptCh` → `interceptLoop` → `cancelLLM` + `a.interceptCh` |

已确认的具体问题（均为源码事实）：

1. **队头阻塞**：`eventLoop` 单 goroutine，`process()` 全程持 `a.mu`（`internal/agent/core/process.go:103-104`），一轮对话（含 N 轮工具）期间后续输入全部排队。
2. **中断只在一种时刻成立**：`interceptLoop` 有三条降级回排队的路径——当前无 LLM 在跑、当前轮是 `_consolidation_`、`a.interceptCh` 满（`internal/agent/core/eventloop.go:74-116`）。它不是独立管线，而是"抢占 + 三次降级"。
3. **回执误投风险**：`ResponseCh` 只由全局 `emitResponse` 写（`eventloop.go:475`），无任务归属。一旦引入抢占，中断的回执会写进被挂起任务的等待者。
4. **断链点静默**：`processInput` 有多条提前 `return` 而不 `emitResponse` 的路径（`resolveInput` 失败 `:309`、10s 去重命中 `:317`、`_consolidation_` `:326`），同步调用方（`cli`、`clawhubadapter` 无超时）永久挂起。
5. **背压策略分裂**：`inputCh` 满 → 阻塞发送方；`selfInputCh` 满 → 静默丢弃；`a.interceptCh` 满 → 降级回 `inputCh`。
6. **假取消**：工具超时只是放弃等待，内层继续执行且副作用照做（`internal/agent/core/toolcall.go:33-45`）。
7. **不可观测**：没有任何"当前在跑什么、谁被挂起、降级了多少次"的统一入口。

**目标**：把上述隐式行为收敛成一个**显式、可 dump、可单测、可断言**的调度器。

---

## 2. 术语与模型

```
Task = { id, level, origin, frame, state, enqueueAt, preemptCount, responseCh }
  state:  ready | running | suspended | done

TaskFrame = {
    msgs []Message            // 本任务自己的 LLM 消息序列
    step  Step                // 下一个要执行的 step（安全点游标）
    turn  int                 // 已完成的工具轮数
    toolIdx int               // 当前工具批内的下标
    toolResults []ToolResultItem
    toolsUsed []string
    lastBatchReplyOnly bool
    stageCtx *sdk.StageContext
    budget TokenBudget
    outputChannel string      // 该任务的输出通道
    responseCh chan<- *OutputEvent  // 任务级回执信道（可为 nil）
    startedAt time.Time
    input string              // 触发本任务的输入文本（COMMIT 时写记忆）
    noMemory bool
    originSource string
}

Step（枚举，顺序执行，步与步之间是安全点）：
  S_PREPARE     构建 msgs / 应用中断标记 / 合并 stage 上下文
  S_LLM         LLM 流式调用（**可抢占**：cancel 即丢弃）
  S_POST        StagePostAction
  S_LLM_JUDGE   无 tool_call → 去 S_BEFORE_OUTPUT；有 → 去 S_TOOL_BEGIN
  S_TOOL_BEGIN  replyOnly 判定 + 取当前 tc
  S_TOOL_BEFORE StageBeforeToolcall
  S_TOOL_EXEC   工具执行（**临界区，不可抢占**）
  S_TOOL_AFTER  StageAfterToolcall
  S_TOOL_NEXT   批内下一个 / 批结束 → S_LLM
  S_BEFORE_OUTPUT / S_AFTER_OUTPUT
  S_COMMIT      context.Append + emitMemoryCandidate（**原子，不可抢占**）
  S_FINISH      写 responseCh、发事件
```

三集合：

| 集合 | 内容 | 语义 |
|---|---|---|
| `readyQueue` | 排队输入形成的新任务 | 按 `(level desc, enqueueAt asc)` 排序 |
| `pendingInterrupts` | **因优先级不足或临界区而未能抢占**的中断请求 | 同上排序；被取出时以中断语义启动 |
| `suspendPool` | 被打断、保存了现场的任务 | 同上排序；被取出时从 `frame.step` 继续 |

> 注意用词：`suspendPool` **不是栈**。当恢复规则是"按优先级取"而非 LIFO 时，把它叫"中断栈"会误导实现（写死 pop 语义）。本设计统一称 `suspendPool`。

---

## 3. 优先级

### 3.1 四级（内核预定义）

| Level | 名称 | 语义 | 典型来源 |
|---|---|---|---|
| `L4` | CRITICAL | 紧急打断 | CLI/WebUI 的显式打断、系统告警、安全类中断 |
| `L3` | INTERACTIVE | 人机交互 | 用户在 CLI/WebUI 的直接对话 |
| `L2` | MESSAGE | 异步消息 | QQ/微信等入站消息、插件通知 |
| `L1` | BACKGROUND | 后台维护 | 心跳蒸馏/归档/合并/复审、`spawn_child` 子任务、consolidation、healthcheck |

- **默认级 = `L1`**：未显式声明一律最低级（"显式才是特权"，避免新插件默认获得抢占权）。
- **取值域仅这四档**，不引入任意整数，避免"9 级比 4 级大但没人知道怎么排"。

### 3.2 优先级从哪来（内核内部属性，**不做成配置项**）

优先级是**内核内部属性**：内核预定义四级，并按**内核自己的规则**为任务与中断定级。

- ❌ **不是运维可调项**。不引入 `core.agent.priority.<channel>` 这类配置键，
  也不把 `PriorityLookup` 做成可注入的策略表——那等于把内核的调度内部属性
  外化成配置，与“谁能打断谁”的内部语义相反。
- ❌ 也不暴露给插件声明（这个方向曾写入 v2 计划，已删除）。
- ✅ v1 的内部缺省规则（仅为实现缺省值，语义上是内核自己的事）：
  `cli`/`webui`/`http` → `L3`；`system` / `_consolidation_` → `L1`；其余 → `L1`（默认级）。
- 定级规则可随内核演进调整，但**始终不对外暴露**。

> 具体“哪类工作算哪一级”的完整规则由内核定义；本稿只固定四级语义与定级位置
> （`(*Agent).taskLevel`），不承诺配置面。

### 3.3 抢占判据

```
incoming.level > running.level   → 请求抢占
incoming.level == running.level  → 入 readyQueue（或 pendingInterrupts，见 §5），FIFO
incoming.level <  running.level  → 入 pendingInterrupts
```

**严格大于才抢占**；相等一律排队——这条保证确定性，也是"较低无法打断较高"的字面实现。

---

## 4. 调度规则

### 4.1 选择函数（统一三集合）

任务结束、或运行任务到达安全点且存在待处理抢占请求时，执行：

```
pick = argmin over (readyQueue ∪ pendingInterrupts ∪ suspendPool)
         by (-effectiveLevel(t), t.enqueueAt)
```

- 高有效级先；
- 同级**先到先服务**（`enqueueAt` 为首次入队时刻；挂起任务保留其原始入队时刻，因此倾向"先完成旧任务"，天然抑制饥饿）。

### 4.2 安全点（可切换点）

**只有 step 与 step 之间是安全点。** 明确：

- ✅ `S_LLM` 之后、`S_TOOL_BEFORE` 之后、`S_TOOL_EXEC` **之后**、`S_TOOL_AFTER` 之后……
- ❌ `S_TOOL_EXEC` **执行中不是安全点**：工具副作用不可回滚，无法"保存现场"。

### 4.3 临界区

```
CriticalSection：step 标记 nonPreemptible = true，或任务进入声明区间
```

- 实现方式：**调度器在临界区期间不求值抢占**（协作式单线程下即"不 yield"），不使用 `sync.Mutex`。
- 资源互斥不用锁，而是**调度器持有的资源表**（若某资源被 running 占用，则不会选出同样占用它的任务）——纯数据判定，天然无优先级反转。
- **v1 临界区清单**（显式列出，避免"隐式临界区"）：

| 临界区 | 理由 |
|---|---|
| `S_TOOL_EXEC`（单次工具执行全程） | 副作用不可回滚；插件 RPC 不可取消 |
| `S_COMMIT` | 上下文/记忆写入必须原子 |
| 需 ONNX 嵌入的 `S_PREPARE` 片段 | ONNX `Run` 不可取消 |
| 媒体 CAS 落盘 | 同上 |
| 显式声明的 `_consolidation_` 类任务 | 记忆一致性 |

- 临界区期间到达的抢占请求**不丢失**：进入 `pendingInterrupts`，在临界区结束后的第一个安全点重新求值。

### 4.4 背压（v1 统一为一种）

- `readyQueue` 有界（默认 256，可配）。
- 满时：**阻塞发送方**（与现状 `inputCh` 一致，避免静默丢用户输入），但必须**计数并打日志**。
- `pendingInterrupts` 有界（默认 64）；满时**丢弃最老的 pending 中断并计数**（中断是提示性输入，宁可丢旧保新）。
- `suspendPool` 深度上限默认 4（见 §7.3）。

---

## 5. 中断语义

### 5.1 中断产生线程的职责（钉死）

`interruptLoop` 只做三件事，**绝不触碰任何 TaskFrame**：

```
① 从 io.interruptCh 收中断 → 定级
② 决策：
     incoming.level > running.level 且 running 不在临界区
        → 置 preemptionRequest = {incoming, requestedAt}，并调用 running 当前 step 的 cancel（若可取消）
     incoming.level > running.level 但 running 在临界区
        → 入 pendingInterrupts
     incoming.level <= running.level
        → 入 pendingInterrupts
③ 唤醒调度器（向 schedulerInbox 投一个 wake 信号）
```

共享面仅两处：`preemptionRequest`（原子指针）与 `running.stepCancel`（原子读）。**帧的保存与恢复只能由调度器做。**

### 5.2 三种情形的统一

现状的三条降级路径在新模型里不再需要特殊分支：

| 情形 | 现状 | 新模型 |
|---|---|---|
| LLM 在跑，正常 | 真抢占（同轮 continue） | 真抢占：`S_LLM` 取消，任务 A 存 `suspendPool`，中断任务 B 从 `S_PREPARE` 启动 |
| LLM 没在跑 | 降级为排队 | B 入 `pendingInterrupts`（或直接成为 ready 任务），调度器立即选出 |
| `_consolidation_` 中 | 降级为排队 | `_consolidation_` 是后台临界区 → B 入 `pendingInterrupts`，临界区结束后求值 |
| `a.interceptCh` 满 | 降级为排队 | 不存在该队列；`pendingInterrupts` 有界丢弃 |

### 5.3 中断任务与被打断任务的关系（**已定：D1 = 方案 B**）

> 用户明确：
> *“中断打断时，上个任务到达以来的所有上下文现场被保护（含 toolcall），
> 然后中断在**上个任务前的那个完整状态**上开始运行。中断运行结束，再把被挂起的
> 任务与其上下文现场**加载回中断任务之上**，并继续运行。”*

因此语义是：

1. **被挂起任务的现场 = 它自到达以来累积的全部上下文（含 toolcall 结果）**，
   原样保存在 `TaskFrame` 里。
2. **中断任务从「上一个任务之前的完整状态」开始运行**——它**看不到**被打断
   任务的任何部分进展。等价于：中断任务就是一个普通新任务，正常走 `S_PREPARE`
   （重建 system prompt + timeline + 自己的输入）。
3. **中断结束后，把被挂起任务与其现场加载回「中断任务之上」再继续**：
   中断已提交的那段上下文留在**下面**（成为重建前缀的一部分），本任务自己的
   现场接回**其上**。

实现对应（`internal/agent/core/task.go`）：

- `TaskFrame.PrefixLen` 记录 prepare 段构建的**基础前缀**长度
  （system + timeline + 用户输入）；其后的 Stage 上下文与工具轮产物都是“自己的现场”。
- `rebaseFramePrefix(f)`：恢复时重建基础前缀（因中断结束已把它的输入/输出提交进
  `a.context`，重建出的 timeline 已含中断效果），再把 `f.Msgs[PrefixLen:]` 原样接回；
  并补回 prepare 段的両处尾部改写（`IsInterrupt` 的 `[中断消息]` 标记、输入多模态块）。
- 调用点：`resumeTask` 在 `runTaskSteps` **之前**调用它。

> 代价（已知且接受）：中断看不到“进行到哪一步”，所以“别搜了改成 X”这类指令
> 只能靠它自己重新理解；换来的是中断起点总是一个**一致的完整状态**。

---

## 6. 保存现场与恢复

### 6.1 保存

在安全点被抢占时：

```
suspendPool.push(Task{frame: running.frame, state: suspended,
                      step: running.frame.step, enqueueAt: running.enqueueAt})
running.state = done_for_now
```

- **只保存数据帧**，不保存 goroutine 栈（这正是"单调度 + 隐式状态机"优于"park goroutine"的地方）。
- `S_LLM` 被抢占时：**不完整的 LLM 请求直接丢弃**（LLM 调用幂等、无持久副作用）；恢复时从 `S_LLM` **重发**，`msgs` 与抢占前一致（即"请求前"的状态）。
- 已提交的副作用（已执行的工具、已 append 的 context）**不回滚**——帧里记录的 `toolResults` 会保留，恢复后继续。

### 6.2 恢复

从 `suspendPool` 取出后：

1. **重建基础前缀**（`rebaseFramePrefix`）—— 此时中断任务已结束并提交，
   重建出的 timeline 包含中断的输入/输出，即“现场加载回中断任务之上”；
2. 把本任务自己的尾部（Stage 上下文 + 工具轮产物 + 占位）原样接回；
3. 从 `frame.Step` 继续执行。

被丢弃的只有那次**不完整的 LLM 请求**（幂等），已执行的工具与已累积的
`toolResults` 全部保留。

### 6.3 嵌套

- 允许中断任务自身被更高优先级抢占（嵌套）。
- **`suspendPool` 深度上限 = 4**（与优先级档数一致，可配）。
- 超限策略：**不继续下潜**——新的抢占请求转为 `pendingInterrupts`。超限丢弃/拒绝必须计数。

---

## 7. 回执路由（任务级）

**必须改**：`ResponseCh` 从"全局 `emitResponse` 的对象"上升为 `TaskFrame.responseCh`。

```
emitResponse(task, ...)      // 写 task.frame.responseCh，而不是"当前全局通道"
```

- 抢占场景下，中断任务 B 的 `S_FINISH` 只可能写 `B.responseCh`，绝不会写进被挂起的 `A.responseCh`。
- **不变量**：**每个任务在 `S_FINISH` 必然产生且仅产生一个终态事件**（无论成功、失败、被跳过）。`processInput` 现有的三个提前 return（解析失败、去重、consolidation）在新模型里都必须转成"任务以 `skipped` 终态结束并回执"。
- 这顺带修掉现有缺陷：`cli`（`internal/plugins/cli/plugin.go:242`）与 `clawhubadapter`（`internal/plugins/clawhubadapter/plugin.go:1045`）的同步注入在断链时会永久挂起。

---

## 8. 并发结构（两个 goroutine）

```
schedulerLoop（唯一持有任务状态与帧）
   for {
     if 可切换 && preemptionRequest 有效 → 执行抢占（保存现场）
     if running == nil → pick from 三集合；无候选则等待 inbox
     runOneStep(running)          // 可能是阻塞调用（见 §8.2）
     处理 step 结果 → 推进或结束任务
   }

interruptLoop（不持有任何帧）
   收 interruptCh → 定级 → 决策 → 置 preemptionRequest + cancel + wake scheduler
```

### 8.1 不变量

| # | 不变量 |
|---|---|
| I1 | 任意时刻至多一个 `running` 任务（"一个 running"约束的是**副作用**，不只是 CPU） |
| I2 | 任务帧只由 `schedulerLoop` 读写；`interruptLoop` 只写 `preemptionRequest` / 读 `stepCancel` |
| I3 | 任何跨挂起点的状态都是纯数据，不持有锁 |
| I4 | 安全点只在 step 边界；工具执行中与 COMMIT 不是安全点 |
| I5 | 每个任务恰好一次终态事件（含 `responseCh` 写入） |
| I6 | 调度器本身永不退出（panic 只使当前任务失败） |

### 8.2 关于"调度器不被阻塞"（**待确认 D2**）

> **M3 拆分的理由**：真正的挂起要求帧跨越 `prepare → step… → finish` 全生命周期。
> 若只把 `process()` 改成可挂起，`processInput` 会在挂起返回后继续执行
> `context.Append` 与 `emitResponse`——造成重复提交。故 M3 分为 M3a（所有权重构，
> 行为等价）与 M3b（抢占语义）两步。

v1 采纳：**`S_TOOL_EXEC` / ONNX / CAS 属于临界区，调度器在这些 step 上会阻塞进插件 RPC / 原生调用。** 这是有意的取舍：

- 好处：与"两个 goroutine 就够"一致，实现简单，无临时 goroutine。
- 代价：这些临界区期间**中断只能排队，不能抢占**。换言之，**中断的有效窗口 = `S_LLM`**（与今天的实际行为相同，但现在是显式声明而非隐式结果）。
- 演进（v2）：把 `S_TOOL_EXEC` 改成异步 step（临时 goroutine + 完成事件），并给插件协议加 `tool.cancel`。此路径在文档保留，不在 v1 实现。

### 8.3 panic 隔离

- `runOneStep` 外包 `recover`：panic → 当前任务标记 `failed`，**调度器继续**。
- 取代现有 `eventLoop`/`interceptLoop` 的 `recover → sleep 1s → go loop()` 无退避重启（`eventloop.go:19-22,38-42`）。

---

## 9. 失效模式与防御

| 失效 | 防御 |
|---|---|
| 饥饿（高优先级流反复抢占） | `preemptCount` 提升有效级：`effectiveLevel = min(4, baseLevel + min(preemptCount, 2))`；被抢占 +1 |
| 无界下潜 | `suspendPool` 深度上限 4，超限转 `pendingInterrupts` |
| 中断请求堆积 | `pendingInterrupts` 有界 64，满则丢最老并计数 |
| 就绪队列满 | 阻塞发送方 + 计数（不静默丢） |
| 同一任务反复被打断 | `preemptCount` 达阈值后有效级提升；另设**抢占冷却**：刚被抢占的任务在 `cooldown` 内不再被同级/更低级抢占 |
| 任务永不结束 | 每任务 `maxTurns`（主循环目前缺失，见审查 P0）+ 每步超时 |
| 不可观测 | `Scheduler.Dump()` 原子快照 + 事件（切换原因、降级次数、丢弃次数） |

---

## 10. 非目标（v1 明确不做）

1. 工具级取消 / 可抢占工具（`tool.cancel`）。
2. 多 agent 并行（仍是单 agent 单调度器）。
3. 公开 SDK 接口变更。
4. 微抢占（任意指令级）。
5. 跨进程恢复（帧不落盘）。

---

## 11. 测试点、测试方式与预期结果

测试基础设施（先于 M1 落地）：

- **假时钟** `Clock` 接口（`Now()` / `AfterFunc`），生产用真实实现，测试注入可控时钟。
- **假 Provider**：实现 `agentAPI.Provider`，返回脚本化的 `tool_calls` 序列（支持"第 N 次调用时挂起直到放行"）。
- **假工具**：测试内 `StageHost.RegisterTool` 注册，可控制每次执行耗时、是否返回错误、是否触发中断注入。
- **同步栅栏**：测试通过 `scheduler.Inbox` 注入中断并用 `runtime.Gosched` + 显式 `waitFor(state)` 断言，不用 sleep 猜时序。
- **快照断言**：`scheduler.Dump()` 返回 `{running, readyQueue, pendingInterrupts, suspendPool, counters}`，测试对纯数据断言。

### 11.1 优先级与抢占

| 编号 | 测试点 | 方式 | 预期结果 |
|---|---|---|---|
| P1 | 高优先级抢占低优先级 | running=L2 在 `S_LLM`；注入 L3 中断 | L2 进 `suspendPool`（step=S_LLM）；L3 变 running；`preemptionCount==1` |
| P2 | 相等优先级不抢占 | running=L2 在 `S_LLM`；注入 L2 | 不抢占；请求入 `pendingInterrupts`（或 readyQueue，按 D3）；running 不变 |
| P3 | 低优先级不抢占 | running=L3；注入 L2 | 同上，不抢占 |
| P4 | 四级逐级抢占嵌套 | 依次注入 L4→L3→L2，均在 `S_LLM` | `suspendPool` 深度 3；running 为最新注入者；每层 step 均为 S_LLM |
| P5 | 抢占后在安全点才生效 | running=L1 在 `S_TOOL_EXEC`；注入 L4 | 抢占**不立即生效**；工具返回后才保存/切换；`deferredPreemptions==1` |
| P6 | 临界区不可抢占 | running=L1 声明临界区；注入 L4 | 同上；L4 请求留在 `pendingInterrupts`，临界区结束立即被选中 |

### 11.2 保存现场与恢复

| 编号 | 测试点 | 方式 | 预期结果 |
|---|---|---|---|
| R1 | 在 `S_LLM` 抢占后恢复 | 构造 A 在 `S_LLM` 被 B 抢占；B 结束 | A 恢复后**重新发起** LLM 请求；`msgs` 与 A 被抢占前**逐字节相同**；不重复执行已完成的工具 |
| R2 | 在 `S_TOOL_BEGIN` 抢占后恢复 | A 完成 1 个工具批后于 `S_TOOL_BEGIN` 被抢占 | A 恢复后继续**下一批**工具；`toolResults` 长度不变 |
| R3 | 恢复结果与不中断一致 | 同一脚本跑两次：一次中途注入中断，一次不注入 | 两次最终 `context` 事件序列**除"中断任务自身的事件"外一致**；A 的 `toolsUsed` 顺序相同 |
| R4 | 嵌套恢复顺序 | L4→L3→L2 依次抢占后依次结束 | 按有效级/到达序恢复；每个任务的 `frame.step` 与其被挂起时一致 |
| R5 | 不完整的 LLM 请求被丢弃 | 假 Provider 在流式途中触发中断 | 该次请求被 cancel；**不产生任何 `msgs` 追加、不产生 tool_call**；恢复后重发次数 = 1 |

### 11.3 回执路由

| 编号 | 测试点 | 方式 | 预期结果 |
|---|---|---|---|
| X1 | 任务级回执不误投 | A 为同步任务并已挂起；B 为同步中断 | B 的回执只到 `B.responseCh`；`A.responseCh` 在 A 恢复并结束后才收到自己的回执 |
| X2 | 断链路径必有终态 | 分别构造：解析失败、10s 去重命中、`_consolidation_` | 三种都产生 `skipped` 终态事件并回执；同步调用方**不挂起** |
| X3 | 每任务恰一次终态 | 统计 `S_FINISH` 次数 vs 任务数 | 相等（I5），无重复写入 |
| X4 | 无超时同步注入不再永久挂起 | `cli` 路径（无超时）注入一条会被去重的输入 | 返回 `skipped` 回执而非永久阻塞 |

### 11.4 队列与选择

| 编号 | 测试点 | 方式 | 预期结果 |
|---|---|---|---|
| Q1 | 选择函数排序 | 同时放入不同 level 与不同 `enqueueAt` 的三个集合成员 | 取值 = `(-effectiveLevel, enqueueAt)` 最小者；同级先到先服务 |
| Q2 | 挂起任务优先恢复（同优先级） | A 挂起（早入队）+ B 就绪（晚入队），同级 | A 先被选中 |
| Q3 | 三类集合联动 | 结束 running 时 `pendingInterrupts` 与 `suspendPool` 同时非空且优先级不同 | 取优先级高者（与 §4.1 一致） |
| Q4 | 就绪队列背压 | readyQueue 满（256）后注入 | 发送方阻塞（或按 D4 返回错误）；计数 +1；不静默丢弃 |
| Q5 | pending 队列溢出 | pendingInterrupts 满（64）后注入更多 | 丢最老的 + 计数；其余保持 |

### 11.5 深度、饥饿与并发

| 编号 | 测试点 | 方式 | 预期结果 |
|---|---|---|---|
| D1T | 下潜深度上限 | 连续注入 6 个逐级更高的中断 | `suspendPool` 深度 ≤ 4；超出部分在 `pendingInterrupts`；`depthRejections` 计数正确 |
| G1 | 饥饿防护（抢占提升） | 对同一 L1 任务连续抢占 5 次（同级/高级交替） | `effectiveLevel` 提升至 `min(4, 1+2)=3`；第 3 次后不再被 L1/L2 抢占 |
| G2 | 冷却生效 | 同一任务刚被抢占后立刻再注入同级中断 | 冷却期内不抢占，请求入 pending |
| K1 | panic 隔离 | 假工具 panic | 只有该任务变 `failed`；调度器存活；后续任务正常执行 |
| K2 | 竞态检查 | 全部调度用例加 `-race` | 无数据竞争报告 |
| O1 | 快照一致性 | 在任意 step 边界调 `Dump()` | 返回的 `running/ready/pending/suspend` 三集合互不重叠且总数守恒 |
| O2 | 切换可观测 | 每次抢占/恢复 | 产生一条事件（任务 id、原因、from→to、level） |

### 11.6 端到端

| 编号 | 测试点 | 方式 | 预期结果 |
|---|---|---|---|
| E1 | 真实 provider + 假长工具 | 启动内核，用一个会阻塞 5s 的假工具跑 L1 任务，途中经 `interceptCh` 注入 L4 中断 | 中断**在工具执行期间不被处理**；工具返回后立即抢占；中断任务先完成；原任务恢复并完成 |
| E2 | LLM 流式中断 | 假 Provider 慢速流式返回 | 中断后当前流被 cancel，任务挂起，中断任务完成，原任务恢复并重新请求 |
| E3 | 现有 e2e 回归 | 跑 `internal/plugins/integration_test.go`、`real_plugin_smoke_test.go` | 行为不变（除文档化的语义变化） |

---

## 12. 待确认决策（含默认取值）

> 未获异议时按"默认取值"实现；每项单独一个 commit，便于回退。

| 编号 | 问题 | 默认取值 |
|---|---|---|
| **D1** | 中断任务的上下文 | **方案 B（已定）**：中断从上一个任务之前的完整状态开始；恢复时把被挂起任务的现场加载回中断之上 |
| **D2** | 阻塞 step 处置：v1 全部声明为临界区（调度器可被阻塞）还是引入异步 step | **v1 = 临界区**；异步 step 留到 v2 |
| **D3** | `pendingInterrupts` 与 `readyQueue` 是否合一 | **保持分离**（中断请求带 interrupt 语义，取出时以中断语义启动）；但**共用同一个排序键** |
| **D4** | readyQueue 满时：阻塞发送方 or 返回错误 | **阻塞发送方 + 计数**（与现状一致，避免丢用户输入） |
| **D5** | 饥饿防护：抢占计数提升 or 时间老化 | **抢占计数提升**（确定性、易测）；时间老化留待需要时 |
| **D6** | 主循环 `max_tool_turns` 是否在本特性一并落地 | **是**（审查 P0，且调度器需要"任务可终止"这一前提） |

---

## 13. 与发布纪律的关系

- 本特性在 `feature/input-semantics` 上开发，完成后合回 `main`，**不碰 `release/v1.2.x`**。
- **公开 SDK 冻结**：v1 不改 `third_party/homeagent-sdk/sdk/`。验收命令：
  ```bash
  git diff main -- third_party/homeagent-sdk/sdk/ | wc -l    # 必须为 0
  ```
- 若 v2 需要 `ChannelDef.Priority`（纯追加），需：
  1. 同步更新 `docs/zh/plugin-interface-matrix.md`；
  2. 与 SDK 仓协同升 SDK 中版本；
  3. 遵守"只增不减、签名不改"边界。

---

## 14. 实现里程碑（逐个实现，每个 = 一个可独立验收的提交）

| 里程碑 | 内容 | 验收 |
|---|---|---|
| **M0** | 测试基础设施：`Clock` 接口、假 Provider、假工具、`waitFor`、`Dump()` 骨架 | 新测试可运行；`go vet` 干净 |
| **M1** | **纯重构**：把 `process()` 拆成显式 step 状态机 + `TaskFrame`；仍由现有 `eventLoop` 驱动，无优先级/无抢占 | R3、X3 通过；既有全部 agent 测试通过（行为等价） |
| **M2** | 调度器骨架：单 `schedulerLoop` + `readyQueue`，取代 `eventLoop` 的输入处理；无优先级（全部 L1，纯 FIFO） | Q1/Q4 通过；integration 测试通过 |
| **M3a** | **前置重构（本次拆分引入）**：把一轮对话的所有权从 `processInput` 移到调度器——帧覆盖 `prepare → step… → finish`；同时移除 `process()` 整轮持有的 `a.mu`（挂起不能持锁） | 既有全部 agent 测试 + 既有 e2e 通过（行为等价）；`-race` 干净 |
| **M3b** | `interruptLoop` 重写 + 四级优先级 + 严格大于抢占 + `suspendPool`；只支持 `S_LLM` 抢占 | P1–P4、R1、R5、K1–K2 通过 |
| **M4** | 临界区 + `S_TOOL_EXEC` 声明 + `pendingInterrupts` + 深度上限 | P5–P6、D1T、Q3、Q5 通过 |
| **M5** | 饥饿防护（抢占计数提升 + 冷却） | G1–G2 通过 |
| **M6** | 任务级 `responseCh` + 断链点统一为终态事件 | X1–X4 通过；`cli`/`clawhub` 不再挂起 |
| **M7** | 可观测性（`Dump()`/事件/状态页）+ 既有回归 | O1–O2、E1–E3 通过；`go test -race ./internal/agent/... ./internal/plugin/...` 全绿 |

每步收尾命令：

```bash
export GOCACHE=/tmp/gocache GOPATH=/tmp/gopath
gofmt -l internal/agent internal/plugin internal/sdk   # 本步新增文件必须为空
go build ./... && go vet ./...
go test -race -count=1 ./internal/agent/... ./internal/plugin/... ./internal/sdk/...
```

### 实现状态（2026-09-13 完成）

| 里程碑 | 提交 | 验收结果 |
|---|---|---|
| M1 | `9a58878` | ✅ agent 全量 + `-race`；新增 `task_test.go` 4 项 |
| M2 | `7082a50` | ✅ 新增 `scheduler_test.go` 6 组（含 O1/K1） |
| M3a+M3b | `c69a1f1` | ✅ 新增 `task_lifecycle_test.go` 5 项、`scheduler_preempt_test.go` 5 项 |
| M4 | `7565248` | ✅ 新增 `scheduler_critical_test.go` 3 项 |
| M5 | `a971fc8` | ✅ 新增 `scheduler_starvation_test.go` 4 项 |
| M6 | `4e4e0ad` | ✅ 新增 `task_terminal_test.go` 3 项 |
| M7 | `f11de37` | ✅ 新增 `scheduler_e2e_test.go` 3 项（压力/可观测/端到端） |

实现期与设计的差异（均已回写本文档）：

1. **M3 拆为 M3a/M3b**：真正挂起要求帧跨 `prepare→run→finish`，否则 `processInput`
   会在挂起返回后继续提交。
2. **`a.mu` 整体移除**：它原本只包住整轮 `process()`（同一 goroutine），
   移除后所有任务状态由 schedulerLoop 独占（不变量 I2/I3 可落地）。
3. **`interceptCh` 被删除**：M3b 起中断一律走 `pendingInterrupts`，旧的
   “同行注入 + 三处 drain + 批次放弃” 已无写入者，属死代码（M4 清理）。
4. **v1 未做 M0 的伪时钟**：所有抢占测试用“单次调用阻塞到 ctx 取消”的
   provider 达到确定性，无需注入时钟。时序型判据（老化式提升）留待需要时。
5. **工具执行中不可抢占是被结构保证的**：让位检查只在 step 之间；
   不需要在 step 内部再判一次。

---

## 15. 开放问题（后续版本）

1. 异步 step + `tool.cancel`（真正让工具可抢占）。
2. 帧落盘（跨进程/崩溃恢复）。
3. 多 agent 并行调度。
4. 与 `plan.md` §13.7 的 `RuntimeManager + 分组 worker` 合并（本设计是其前置）。

> 已删除：“`ChannelDef.Priority` / `InjectOptions.Priority` 进入公开 SDK”——
> 优先级是内核内部属性（§3.2），不应由插件声明。
