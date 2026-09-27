# 工具调用并行化改造：执行计划

> **设计依据**：`docs/zh/toolcall-contract-and-sequence-design.md`（本文只讲**怎么一步步做**，
> 设计取舍与实证依据在那份文档里，不重复）。
>
> **状态**：阶段 0 已完成。阶段 0.5 起为待办。
> **纪律**：每个阶段的「判据」必须**先写且必须能失败**，再改实现；
> 判据通过前不进入下一阶段（见文末「阶段纪律」）。

---

## 阶段总览与依赖

| 阶段 | 内容 | 依赖 | 状态 |
| --- | --- | --- | --- |
| **0** | 修 map 迭代顺序（flush 乱序） | — | ✅ **已完成** |
| **0.2** | **工具错误类型化（已完成）** | — | ✅ **已完成** |
| **0.5** | 补多 tool_call 批内路径判据 | — | ✅ **已完成** |
| **1** | 结果契约 + 参数预校验 | — | ⬜ 待办 |
| **2** | 并行执行层 | 1 | ⬜ 待办 |
| **2.5** | 提示词改为「默认并行」 | 2 | ⬜ 待办 |
| **3** | 序列内核 | 2 | ⬜ 待办（设计已定，实现另议） |
| **4** | `seq_*` 插件 | 3 | ⬜ 待办（设计已定，实现另议） |

**主线是 0.5 → 1 → 2 → 2.5**。3/4 属序列特性，主线完成后另立。

### ⚠️ 顺序不可调换的两处

- **2.5 必须在 2 之后**：先改提示词说「默认并行」而内核仍串行 = 提示词对模型说谎。
- **1 必须在 2 之前**：并行化需要「诚实的 Success」与「结构化值」来判断结果与做条件；
  先并行后补契约，等于把两处改动叠在同一段逻辑上，回归时无法定位是哪一处引起。

---

## 阶段 0 ✅ 已完成

**问题**：`flushToolCall` 由 `for idx := range accs` 驱动，Go map 迭代顺序随机化
⇒ 同一批并行 tool_call 进入 `resp.ToolCalls` 的顺序**每次运行都可能不同**。
对 `output_send__` 这类用户可见通道 ⇒ 分段消息到达顺序不可复现。

**改动**：`process.go` —— 抽出闭包 `flushAll`，收集 index 后 `sort.Ints` 再 flush。
两个调用点（`ch` 关闭、`ctx.Done()`）统一走它。

**判据**：`stream_flush_order_test.go` —— 8 工具 × 200 轮，断言输出严格按投递顺序。
**已做变异验证**：退回 map 遍历后判据于 round 0 即 FAIL（实际顺序 `[tool_02…tool_00 tool_01]`）。
修复后 core 包全绿。

---

## 阶段 0.2 ✅ 工具错误类型化（已完成）

**问题**（两条，第二个是静默 bug）：

1. 内核用 `strings.Contains(err, "not found in any plugin")` 判别「工具不存在」
   （原 `toolcall.go:104`）——**约定**不是契约。插件错误文案若含该子串即被误判。
2. ⚠️ `IOManager` 向父兜底时**吞掉父的执行失败**（`channel.go:655-660`），
   误报为「工具不存在」。后果放大：设备离线这类**本该 retry** 的失败被判为
   「工具没了」⇒ 整组被跳过，与「插件真没加载」无法区分。

**改动**：
- `io/channel.go`：新增哨兵 `ErrToolNotFound` + `ToolNotFound(name)` + `IsToolNotFound(err)`
  （沿用仓内 `ErrInputChannelUnknown` 的先例）。`IOManager.ExecuteTool` 的父兜底
  改为**只传递「确实不存在」**，其余错误如实上抛。
- `core/stages.go`：`StageHost` 的 not-found 改用 `agentIO.ToolNotFound`。
- `core/toolcall.go`：字符串匹配 → `agentIO.IsToolNotFound`；「不存在」时给出
  **可执行**文案（提示 `get_plugin_tools` / `output_list_channels`），
  而非含糊的「执行失败」——后者会让模型反复重试同一个不存在的名字。

**判据**：`toolcall_error_test.go`（类型化 vs 诱饵子串、%w 穿透、执行期文案）+
`channel_error_test.go`（父失败不吞、真的不存在仍可判别）。

**变异验证**：退回父兜底吞噬后，`TestExecuteTool_DoesNotSwallowParentFailureAsNotFound`
FAIL（`父的执行失败被误报为『工具不存在』`）；修复后全绿。

⚠️ **过程中的一次自伤**：我先改了 `channel.go` 却漏了 `stages.go` 的 import，
导致整包 build 失败。已修。另：第一次写判据时只覆盖了类型化，**没覆盖父兜底吞噬**，
变异后仍绿 —— 说明「判据通过」不等于「修的东西被测到」。补了 `channel_error_test.go` 才闭合。

---

## 阶段 0.5 ✅ 补批内路径判据（已完成）

**前提更正**（此前我写「无任何测试直接驱动批内路径」——**不准确**）：
`scheduler_critical_test.go:125` 的 `TestBatch_NotAbandonedWithoutPreemption`
**已经**驱动了「同批两个工具按序执行」。漏查是因为我只 grep 了
`PendingTools` / `ToolIdx` 这两个**字段名**，没查断言**内容**。

**真正缺的是**该测试**未覆盖**的三条（已核实全仓无对应断言）：

| 缺口 | 风险 | 新增判据 |
| --- | --- | --- |
| `tool_call_id` 配对完整性 | 阶段 2 改消息落法时，配对一旦断裂上游直接报错 | `TestBatchToolCallIDsAllPaired` |
| `ContentOnce` 批内语义 | 同一段 assistant 文本在批内重复 N 次，撑爆上下文 | `TestBatchAssistantTextAppearsOnce` |
| denied 后**继续**批内 | 改成 abort 会丢掉本可执行的后续调用 | `TestBatchContinuesAfterDeniedTool` |

另加 `TestBatchExecutesEveryToolCall`（批内全序列 + 顺序 + ToolResults 完整性）。

**判据**：`toolbatch_test.go`（4 条）+ 复用既有 `TestBatch_NotAbandonedWithoutPreemption`。
全部**确定性**断言（阶段 0 已消除 map 随机性）。

**变异验证**：令 `stepToolBegin` 跳过批内最后一个工具后，**4 条判据同时 FAIL**
（既有那条也 FAIL），报错直指 `ToolsUsed = [tool_alpha]`、`tool_call_id "c2" 被声明 0 次`。

⚠️ **过程中三次自伤**（都靠"判据先写"暴露）：
1. 臆造了不存在的 helper（`sdkToolDef` / `sdkStageCtx`）⇒ build 失败
2. 把 `a.stageHost = nil` 后又用它注册 stage handler
3. 给 `newTaskFrame` 传 `nil` ⇒ `stepPrepare` 于 `task.go:519` **nil 解引用 panic**
   ⇒ 改用仓内既有的 `a.stageCtxFromInput(...)`（与 `scheduler_preempt_test` 一致）
   ★ 顺带记录：生产路径两处 `newTaskFrame` 调用（`task.go:228/422`）都传真实 ctx，
     但 `stepPrepare` 对 `f.StageCtx` **无 nil 兜底**。本次不修（无生产触发路径），
     记为潜在健壮性缺口。

## 阶段 2 ⬜ 并行执行层（进行中：2a/2b/2c 已落地，2d 待做）

| 子项 | 内容 | 状态 |
| --- | --- | --- |
| **2a** | 消息落法：一条 assistant 带全部 tool_calls | ✅ 已提交 |
| **2b** | `ConsumeToolBlocks` 改 per-call 归档 | ✅ 已提交 |
| **2c** | `StageContext` 拆 per-tool | ✅ 已提交（**判据待 2d 补强**） |
| **2d** | 批次调度与保序（`ParallelSafe` + 同通道保序） | ⬜ 待做 |

**改动要点**（细节见设计文档 §6）：

- **2a** 现状每工具一对消息；改为一条 assistant 带全部 tool_calls。
  这本身是协议上更正确的形态（现状不表达「这是一批」）。
- **2b** `ConsumeToolBlocks` 原本是 IOManager 级单队列，并发下会互相抢媒体 ⇒
  破坏「媒体必须紧跟自己 toolMsg」那条三轮实测的结论。改按 `call_id` 归档。
- **2c** `f.StageCtx` 原本是单槽；改为每工具一份，`Extra` 逐份浅拷贝
  （`output_channel` 被 `stage.go:18` 依赖）。
- **2d** 全批 `ParallelSafe` 才并发，否则**整批**降级串行（不做部分并发——
  收益不抵不可预测性）；同 `output_send__<通道>` 多次发送**保序**。

### 遇到的一处**既有**测试竞态（非本次引入，但会污染回归信号）

`TestResidualKeepReturnsTasksToParent` / `TestResidualDropNotifiesSyncCaller`
偶发失败，报「应处置 2 条，实际 1」。

**根因**（已核实）：`offload_test.go:473` 的 `SpawnResident` 会启动**子 agent 的
调度器 goroutine**，而测试随后 `child.sched.enqueue(...)` 两条任务、
立刻 `ApplyResidual` 去读同一队列——**全程无任何同步**。调度器与测试读并发
同一份 `sched.queue`，条数可能已被取走。

**为什么以前没暴露**：干净基线（阶段 2 之前）连跑 3 次恰好全绿，是**运气**，
不是确定性。`-race` 单跑该用例也过（无并发源）。本次改动让 core 包耗时略增、
调度时序变化，才把它翻出来。

**处置**：属测试侧缺陷，不在本阶段范围内，**记为待修**（修法：测试里改用
不启动调度器的子 agent，或给 enqueue/读取加同步）。记录在此以免后续误判为
「并行化引入的回归」。

### 2c 的诚实记录：判据在串行下测不出差别

`toolCtxFor` 退回单槽后，两条 2c 判据**仍然全绿**。原因是：
**串行路径下「单槽」与「per-tool」行为完全一致**——每个工具跑完才进下一个，
不存在交错。差别只在**并发**下显现（互相覆写 / after 读到别人的结果）。

⇒ 因此 2c 的真正判据**必须与 2d 一起写**：并发执行批内多工具时，
断言每个工具的 ctx 只带自己的 ToolCalls、after_toolcall 读到自己结果，
并以 `go test -race` 确认无数据竞争。
**在 2d 落地前，2c 只能算"实现已就位、判据未闭合"**，不得记为已验证。

---
## 阶段 1 ⬜ 结果契约 + 参数预校验

对应设计文档 §4。**动机**：`Success` 硬编码 `true`（`task.go:757` 唯一赋值点）、
`required` 无消费方、结果被降级为 `string`——这三项让阶段 2/3 都缺地基。

### 1a. SDK 侧新增（纯新增，无签名变更）

`third_party/homeagent-sdk/sdk/plugin.go`：

```go
// ToolError 描述失败原因。存在的理由：失败若只表达为文本，模型无法定位到字段，
// 只能原样重试（实测 cmd_run 失败率 34%~48% 源于同一成因）。
type ToolError struct {
    Field  string `json:"field,omitempty"`
    Reason string `json:"reason"`   // required/type/unauthorized/timeout/not_found
    Detail string `json:"detail,omitempty"`
    Hint   string `json:"hint,omitempty"`
}
```

- `ToolDef` 增 `ParallelSafe bool`（零值 `false` = 不可并行 = 现状行为，
  刻意让存量插件升级后得到**保守**行为）。
- 仓内 `internal/sdk/plugin.go` 补对应别名再导出。

### 1b. 诚实化 Success

`Success = (err == nil) && !isToolError(result)`。

**判据必须同时覆盖新旧两种形态**：存量插件返回 `map[string]interface{}{"error": ...}`
（如 `files/plugin.go:228`）要判为失败，而返回普通 map/string 仍算成功。
**漏了后者 = 升级会把存量插件的成功误判成失败**，这是本阶段最大的回归风险。

### 1c. 参数预校验

在 `executeToolCallInner`（`toolcall.go:47`）分派**之前**执行：逐项查 `required`、
逐项查 `properties.type`。失败返回结构化 `ToolError`，不进分派。

⚠️ **`getBool`（`utils.go:26`）的注释记载 bool/string/float 三种都出现过** ⇒
校验**不能**把合法的 `"true"` 判为非法。判据必须覆盖这一点。

### 1d. 保留结构化值

`executeToolCall` 内部保留 `rawResult interface{}`（不降级为 string），
渲染交给统一函数：**紧凑 JSON，禁止 `fmt.Sprintf("%v")`**（那会产出
`map[status:sent id:123]` 这种模型读不懂的 Go 语法）。

**顺带修一个静默失效**：`task.go:771` 的 `ToolResults[0].Result.(string)` 断言
几乎恒失败（插件返回的多是 map）⇒ `after_toolcall` 阶段插件对结构化结果的改写当前无效。

### 阶段 1 判据

- `toolerror_test.go`：`isToolError` 对新旧两种形态的判定（各 3 例：失败 map /
  成功 map / 成功 string）
- `argvalidate_test.go`：缺 required、类型不符、`"true"` 宽松放行
- 回归：core 包全绿；**存量插件 smoke**（`internal/plugins` 不得新增 FAIL）

---

## 阶段 2.5 ⬜ 提示词改为「默认并行」

⚠️ **必须在阶段 2 落地之后**（见「顺序不可调换的两处」）。

对应设计文档 §6.5。`buildSystemPrompt`（`tooldefs.go`）新增一段，表达四件事：
默认并行 / 不可依赖顺序 / 例外：同通道输出保序 / 例外：写类工具不并发。

**同时必改**：`spawn_child` 描述里那句「应并行 spawn 多个子 Agent，不要自己串行逐个执行」
（`tooldefs.go:466`）——它在改造前是**落空**的（模型照做，内核仍串行）；
改造后应改为机制性表述。

**判据**：提示词内容断言（`prompt_contract_test.go`）——四要点各自在文本中命中；
且**负判据**：串行阶段的提示词**不含**「默认并行」字样，防止再次跑反顺序。

---

## 阶段 3 / 4 ⬜ 序列（设计已定，实现另议）

设计见 `docs/zh/toolcall-contract-and-sequence-design.md` §7/§8。
依赖阶段 2（组内并行）与阶段 1（结构化条件 + 诚实 Success）。
**待定项 D2**（`parallel: false` 是否构成闸门后门）需先拍板。

---

## 阶段纪律 ［沿用本仓既有教训］

1. **判据先写，且必须能失败**。写完先跑一次确认 FAIL，再改实现。
2. **变异验证**：改完把修复回退一次，确认判据重新 FAIL。
   「判据全绿」不等于「判据有效」——本仓 T23 教训（fixture 照臆测字段编，
   判据全绿而实现全错）就是跳过这一步。
3. **判据只断言代码真实产出的字段**。我本次就栽过一次：先写了
   `assert tc.StreamIndex == i`，而 `flushToolCall` 根本不给 `ToolCall` 赋 `StreamIndex`
   ⇒ 恒假信号。判据必须对着**实际输出**写。
4. **期望值由独立来源算出**，不引用被测代码。
5. **每阶段结束跑全包** `go test ./internal/agent/core/ -count=1`，
   并检查存量插件 smoke 无新增 FAIL。
