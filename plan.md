# HomeAgent 遗留问题清单

> **本文定位（2026-09-24 重写）**：本文**只写仍未完成、且经源码核实确属本仓库的问题**。
> 上一版 plan.md 是 1474 行的「历史工单 + 路线图」混合档，混入了大量已解决记录、
> 以及**根本不属于本仓库的跨项目工单**（见 §五），本次全部剔除。
>
> **每一条的判定方法**：不是继承旧档的复选框，而是回到源码逐项核实
> （`grep` 定符号存在性、`go build ./...`、`go test ./...`、`go test -race`）。
> 旧档里标记「未做」但代码已落地的项、以及引用别仓代码的项，统一进 §三/§五 并附证据。
>
> **本文不写**：已完成项的定位过程（那是 git log 和 commit message 的职责）、
> 无实测支撑的性能断言、任何凭据/生产路径。

---

## 一、结论速览

| 类别 | 数量 | 去向 |
|---|---|---|
| 真正待办（本仓库、代码层可动） | 10 项 | §二 |
| 生产部署后验证（代码已就绪，需现场跑） | 4 项 | §四 |
| 已关闭 / 已实现（旧档误标为 TODO） | 8 项 | §三 |
| 跨项目工单（不属本仓，已在别处解决或归档） | 2 项 | §五 |

当前健康度：`go build ./...` 通过；`go test ./...` 有 **3 个 FAIL**，全部是
测试自身缺陷（见 P2-10），非产品代码问题；`go test -race` 于 `internal/lua`、
`internal/plugin/**` 全绿。

---

## 二、真正待办

按「影响面 × 可验证性」排序。每条给出**证据**（file:line）与**验收**。

### P0-1　§13.7 RuntimeManager + 分组 worker（架构演进，唯一大件）

**现状**：`RuntimeManager` / `worker_group` / `WorkerGroup` 在全仓库
（含 .go / .json / .md，排除 plan.md 自身）**零引用** —— 该能力从未实现。
当前是**一插件一子进程**：`internal/plugin/proc/plugin.go:134` 的 `Spawn`
按插件各起一个进程。

**为什么要做**：每个外部插件一个进程 → N 个插件 = N 个常驻进程 + N 份
transport，进程数随插件线性涨。目标是一个 RuntimeManager + 少量 worker +
多插件共享 transport + 每插件独立 PluginContext（独立身份，共享管道）。

**证据**：
- `internal/plugin/proc/plugin.go:134`（`Spawn`，逐插件起进程）
- manifest 无 `worker_group` 字段：`third_party/homeagent-sdk/example/a2a/plugin.json`
  的键集合为 `author/description/entry/name/name_en/name_zh/platforms/tags/version`

**子任务**：
1. `RuntimeManager` 类型：worker 池 + 调度
2. `Worker` 类型：一个进程，承载多个插件，共享 `RuntimeClient`
3. `PluginContext` 类型：每插件独立身份（能力门、ownerID、工具命名空间）
   挂在同一 transport 上
4. manifest 增 `worker_group` 字段（缺省 = 全部归同一 worker，兼容迁移）
5. 高风险插件可声明独立 `worker_group`

**验收**：
- [ ] 缺省分组行为与现状逐字节等价（现有 proc 测试全绿，不改协议）
- [ ] 两个插件同 worker 时，各自 `ReclaimOwner` 只回收自己的 arena 块
- [ ] 一插件崩溃不带走同 worker 的另一插件（或明确：带走，并写进文档）
- [ ] manifest 无 `worker_group` 的旧插件可原样加载（向后兼容）

> 注：`.pi/subagents/missions/` 里有一份针对本项的只读架构评审产物，
> 可作为设计输入；其中结论尚未落地为代码。

---

### P0-2　§11.6 reload 语义仍在说谎

**现状**：`plugin_install` 依旧回 `reload_required`，而**不可热重载的插件
（Go `plugin.Open` 路径、`DF_1_NODELETE`）会被假装重载成功**。

**证据**：
- `internal/plugins/pluginmgr/plugin.go:528 / :548 / :755` —— 仍返回
  `reload_required`
- 全仓库无 `DF_1_NODELETE` / `NODELETE` 检测（`grep` 为空）
- `internal/plugin/registry.go:823-824` 注释已自认：`Go plugin.Open 路径
  （dynamicPlugin）不可卸载，跳过`

**子任务**：
1. ELF 检测 `DF_1_NODELETE` → 标记插件「不可热重载」
   （`internal/plugin/dynamic_loader_unix.go`）
2. `ReloadOne` 对这类插件返回「需重启 homed」，停止假装成功（`registry.go:785`）
3. `plugin_install` 返回 `restart_required` 替代误导性的 `reload_required`
   （`pluginmgr/plugin.go`）

**验收**：
- [ ] 装一个 `DF_1_NODELETE` 插件后，调用方拿到 `restart_required`，不是 `reload_required`
- [ ] `ReloadOne` 在该插件上返回明确错误，且**摘除旧注册面**（工具/stage/output）
      仍已完成，不留悬空闭包

---

### P1-3　§13.2 arena 容量固定，无 Grow/Shrink

**现状**：内核独占的变长分配器已落地（first-fit + 邻块合并 + owner 校验），
但容量**固定 4MB**，用尽即调用失败，不再退回内联。

**证据**：
- `internal/plugin/proc/arena.go:99`：`const arenaDefaultCapacity = 4 * 1024 * 1024`
- `arena.go:263 / :283`：超限直接报错

**为什么当时不做**：跨进程 `remap` 会让正在读的对端 SIGSEGV。所以这不是
「补个函数」，而是要先解决**对端可见的地址稳定性**。

**可选路径**（择一，需先定方案再动手）：
- a) 预映射大虚拟区间（未触碰页不占物理内存），容量「逻辑无限」
- b) 段表 + 多段拼接：新块分配在新段，不必 remap 旧段
- c) 维持固定容量，但**把超限错误做成可执行指引**（告诉插件该怎么分片）

**验收**（按所选路径定）：
- [ ] 超过当前 4MB 的单次 payload 仍能送达，或收到带指引的明确错误
- [ ] 任何情况下对端不 SIGSEGV

---

### P1-4　§12.3 事件环：机制完成，零真实负载检验

**现状**：事件环（区内 segment + eventfd）与 `events.subscribe` 能力已完成，
但**没有一个真实外部插件订阅 `stage` / `tool_call` 事件**，`dropped` 计数
在长跑下的行为也无人观察。

**证据**：
- `internal/plugin/proc/capability.go:149-150`（subscribe/unsubscribe 已授权）
- `internal/plugin/proc/corehandler.go:46-58`（`EvtRingSubscriber` 接口）
- 无对应 example 插件（`third_party/homeagent-sdk/example/` 无事件订阅样例）

**子任务**：
1. 写一个订阅 `stage` / `tool_call` 事件的 example 插件，跑真实负载
2. 长跑观察 `dropped` 是否异常增长（环 cap 溢出告警是否够用）

**验收**：
- [ ] example 插件能稳定收到事件并正确反序列化
- [ ] 压测下 `dropped` 有上界且可观测

---

### P1-5　§11.2 工具超时措辞误导 + browser 插件超时聚集

**现状**：内核侧工具超时文案仍写「已取消」，但**进程内 cgo 插件根本取消不了**
（OS 线程永久占用）——这是措辞与事实不符。

**证据**：
- `internal/agent/core/toolcall.go:42`：
  `fmt.Sprintf("工具 %s 执行超时（60秒），已取消", tc.Name)`
- `internal/plugin/proc/process.go:27 / :114 / :577`：注释自认 cgo 路径
  「超时后 OS 线程永久占用，现网已泄漏 26 次」

**子任务**：
1. 措辞改「已放弃等待（插件仍在后台运行，其占用的线程无法回收）」
2. 排查 `browser` 插件为何频繁 60s 超时（旧档记录 22/26 次集中于此）；
   真取消能力依赖子进程模型（与 P0-1 相关）

**验收**：
- [ ] 超时文案不再暗示「已取消」
- [ ] browser 超时率下降到可解释水平，或给出根因

> 与 P0-1 的关系：把内部 cgo 插件也迁到子进程后，这条的严重性自动消除。

---

### P1-6　§13.11 尾项：`handleAgentAction` 直接 501

**现状**：WebUI 的 agent 操作接口**所有 action 一律返回 `501 Not Implemented`**，
是明确的未接线桩。

**证据**：
- `internal/plugins/webui/handler_agents.go:239`：
  `writeJSON(w, http.StatusNotImplemented, ..."action not implemented by supervisor")`

**子任务**：决定该端点该做什么——要么接线到 supervisor 的真实动作
（stop/restart/snapshot 等），要么从 UI 撤掉入口，不要留一个必然失败的按钮。

**验收**：
- [ ] UI 上不存在「点了必 501」的入口，或该入口真的能动作

> 说明：旧档 §13.11 的「11 项」其余各项**已实现**（见 §三.6），仅此一项是真缺口。

---

### P2-7　§12.4 Lua 仍走独立 ABI（进程内解释器）

**现状**：Lua 插件在**内核进程内**跑 gopher-lua，不走 proc 通道，是三套
ABI 里唯一没收敛的。代价是：Lua 插件崩溃 = 内核崩溃，且无共享内存数据面。

**证据**：
- `internal/plugin/lua_plugin.go:947`（`makeStageHandler`，进程内）
- `internal/plugin/lua_plugin.go:128 / :329`（`register_stage` 直接注册到内核 SDK）

**子任务**：评估「Lua 走 proc 通道」的代价（解释器进程启动开销 vs 隔离收益），
据此决定收敛还是明确保留为独立 ABI 并写进文档。

**验收**：
- [ ] 有明确决策（收敛 / 永久保留），且文档与实际一致

---

### P2-8　§11.5 / §12.5 Windows 只有交叉编译，无真机验证

**现状**：Windows 侧共享内存（`CreateFileMappingW`）、事件通知
（`CreateEventW`）、命名对象传递均已实现（桩已合回平台中立文件），但
**从未在 Windows 真机端到端跑过**。

**证据**：
- `internal/plugin/dynamic_proc.go:17-27` 注释：平台差异已全部封装，
  桩已删除
- 无 Windows CI / 真机记录

**子任务**：
1. 找一台 Windows 机器跑端到端
2. **特别验证命名对象的撞名防护**（名字带 PID + 递增序号）

**验收**：
- [ ] Windows 下 `homed` 能加载外部插件并完成工具调用往返
- [ ] 并发起多个插件时命名对象不撞

---

### P2-9　§11.9 homed 主 heap 常驻未解释

**现状**：旧档记录 homed 主 heap 有约 **2.36GB 常驻**，来源未定位。
配置侧已有缓解手段（`embedding_model_path` 支持 `#topN` 限词向量数量），
但**未确认现状是否仍存在**。

**证据**：
- `internal/config/registry.go:856`：`embedding_model_path` 描述已写明
  `#topN` 可「控制常驻内存」
- 无 `GOMEMLIMIT` 相关设置（`grep` 为空）

**子任务**：
1. 现场 `pprof` 定位常驻来源（是否仍为双模型加载）
2. 若确认，评估是否加 `GOMEMLIMIT` 或默认 `#topN`

**验收**：
- [ ] 给出常驻内存的构成分解（哪块占多少）
- [ ] 有明确取舍结论（可接受 / 需优化 / 已优化）

---

### P2-10　仓库卫生：三个真实的测试缺陷（3 FAIL）

**现状**：`go test ./...` 有 **3 个 FAIL**，根因都是**测试自身缺陷**，
不是「环境玄学」，也都可修：

1. **PTY 用例要求可打开的 `/dev/ptmx`**：容器里节点存在但 `open` 被
   `EACCES` 拒（实测 `PermissionError: [Errno 13]`），而用例**没有环境探测、
   直接 `t.Fatal`** → 应用 `t.Skip` 或 build tag 隔离，而不是硬 FAIL
2. **测试端口硬编码 `127.0.0.1:9890`** → 并行/残留实例即 `bind: address
   already in use`，波及无关用例
3. **`TestRestoreFileFromBaseline` 写真实系统路径且忽略错误**：
   `internal/system/system_test.go:83` 的 target 是
   `"/etc/RestoreFileFromBaseline.test.tmp"`，而用例内
   `os.WriteFile(target, ...)` **不检查 err**（`os.WriteFile(target, []byte("v1"), 0644)`）；
   在 `/etc` 不可写的环境（实测本机 root 也被拒）里，前面的写全部静默失败，
   留档内容为空/不存在，到 `RestoreFileFromBaseline` 就报
   `expected restore to happen`。**根因是测试写死真实路径 + 吞错误**，
   不是 `RestoreFileFromBaseline` 实现有问题。

**证据**：
- `internal/plugins/integration_test.go:262 / :318 / :376`（PTY 三例）
- `internal/plugins/remotedevice/plugin.go:27`：`const defaultAddr = "127.0.0.1:9890"`
  （测试沿用固定端口）
- 实测输出：`open /dev/ptmx: permission denied`、
  `listen tcp 127.0.0.1:9890: bind: address already in use`

**子任务**：
1. PTY 用例：无 `/dev/ptmx` 时 `t.Skip`（环境能力探测，不静默）
2. remotedevice 相关测试改用 `:0` 让 OS 分配端口，或测试内随机端口
3. `TestRestoreFileFromBaseline`：改用可写的临时路径（并保留
   `IsProtectedPath` 语义所需的显式前缀，用 `IsProtectedPathExplicit`），
   且**每一步 WriteFile 都检查 err**；顺带审计同类「写真实系统路径」的测试

**验收**：
- [ ] 在无 PTY 权限、`/etc` 不可写的环境里，`go test ./...` 全绿
      （受限用例显式 skip，不是静默通过）
- [ ] 重复/并行跑不再端口冲突

---

## 三、已关闭 / 已实现（旧档误标，防复活）

逐条给出「旧档怎么说」与「源码实际怎样」，**不要再往待办里加**。

### 1. §13.12 L3 原生多模态 —— 已实现
- 旧档：标「未做」，要求 media 成为一等图节点 + contains/depicts/derived_from 原生边
- 实际：`internal/memory/graph.go:169` 起建 `memory_blocks`（含
  `modality/payload_digest/mime/vector/fingerprint/scene`）+
  `memory_block_edges`（`source_kind/target_kind/edge_type`），
  以及 `scenes` / `scene_features` / `scene_refs`；
  `internal/agent/core/graphmedia.go`（310 行）实现
  `migrateLegacyGraphMedia` / `attachBlocksToSentence` /
  `linkBlocksToDocument` / `commitTriplesWithMedia`；`graphmedia_test.go` 21 个测试
- 结论：**关闭**

### 2. §13.9 llmsproxy 上下文溢出感知 —— 不属本仓（见 §五.1）

### 3. §13.10 AgentMail 三个 bug —— 不属本仓（见 §五.2）

### 4. §11.4 Lua stage 快照缺读锁（DATA RACE）—— 已修
- 旧档：要求加 `sc.RLock()`/`sc.RUnlock()` 包裹快照构造
- 实际：`internal/plugin/proc/shmcodec.go:42 / :326 / :425` 已在
  `captureLocal` / `WriteDirty` 前后持读锁
- 实测：`go test -race ./internal/lua/... ./internal/plugin/...` 全绿
- 结论：**关闭**

### 5. §12.2 `io.setToolBlocks` 内核侧是桩 —— 已实现
- 旧档：要求实现内核侧 handler + 补 example
- 实际：`internal/plugin/proc/corehandler_inject.go:132` 实现
  `MethodIOSetToolBlocks`；模板 `putArena` → `blocks_ref`；
  `e2e_template_test.go:371` 用**真实 SDK 模板**验证
- 结论：**关闭**

### 6. §13.11 WebUI 修复清单 —— 主体已实现，仅剩 P1-6
逐项核实：`Last-Event-ID` 重放（`handler_chat.go:710`）、请求超时
（`handler_chat.go:592` 等 300s）、XSS 消毒（`dashboard.js:252` DOMPurify）、
`renderAll` 增量（`dashboard.js:34 / :452 / :1361` 增量游标 + 流式增量）、
`handleKnowledge` 不再吞错（`handler_memory.go:122` 起逐分支返回错误）、
CSS/DesignSystem（`dashboard.css:3` 起 sakura/frost 令牌）、
GUI 重构（`cmd/gui/renderer/app.js`）—— 上述均已落地。
**仅 `handleAgentAction` 501 是真缺口**（已列 P1-6）。

### 7. §12.5 Windows 桩未删 / 旧档称「交叉编译通过」 —— 已收敛
- 实际：`internal/plugin/dynamic_proc.go` 已合为平台中立单文件，
  平台差异封装在 `shmalloc_*` / `evtfd_*` / `shmpass_*` / `procattr_*`（各带
  构建标签）；旧桩已删。**真机验证仍缺**（已列 P2-8）

### 8. §12.7 `cmd/ohos/.../SettingsPage.ets` 有未提交改动 —— 已提交
- 实际：`git status --short` 于工作区全清（含 `cmd/ohos/`）

---

## 四、生产部署后验证（代码已就绪，本就无法在仓库内完成）

这些**不是待开发项**，是「必须落到生产实例才能确认」的验收。仓库内有
脚本 `scripts/verify_deploy.sh [data_dir]` 可一键检查前两条。

- [ ] **§0.1 healthcheck 隔离**：部署后 `knowledge/`、`memory/graph.db`、
      `memory/documents/` 不再出现 `_hc_*` 残留
- [ ] **图记忆去重**：`relations` 重复率归零，跑一周不新增重复
- [ ] **§13.5 / §13.6 QQ 端到端**：真实 QQ 消息注入与输出经共享内存通道正常
      （小 payload 内联、大 payload 走 `text_ref`/`frame`）
- [ ] **§0.2 agentcli 不泛滥**：QQ 消息在 agentcli 无自喂送风暴时能被正常响应

---

## 五、跨项目工单（**不属本仓**，旧档误并入）

旧 plan.md 把别仓的工单写成本仓 TODO，导致「查无此代码却挂着未完成」。
移出并说明归属：

### 1. §13.9「llmsproxy 上下文溢出感知」
- 旧档写：补 `OVERFLOW_PATTERNS`（"Context window is full"）、
  AUTO 截断宽度 `80→160`、`go test ./internal/ai/...`
- 事实：本仓**没有 `internal/ai/`**；`OVERFLOW_PATTERNS` /
  `clientUpstreamErr` / `OneLine(...,80)` 都在 **`/home/program/llmsproxy`**
  （`internal/gateway/chat.go`）
- 现状：该仓已把宽度改成 160（`chat.go:634` 注释「160 而不是 80」）
- 归属：**llmsproxy 仓**，与本仓无关

### 2. §13.10「AgentMail 三个 bug」
- 旧档写：提示词修正 / `InReplyTo` / `relay_key ≤ 64 字节`
- 事实：AgentMail 是独立仓 **`/home/program/agentmail`**
  （`relay_key` 见 `server/internal/handler/permission.go:32 / :90 / :382`）
- 现状：`relay_key` 上限已实现为 **160 字节**并带测试
  （`permission.go:90`、`relay_test.go:55`），`in_reply_to` 语义见
  `forward.go:225`
- 归属：**agentmail 仓**

> 若这两仓也要纳入统一管理，应各自建 plan，不要塞进本仓文档。

---

## 六、明确「不做」的决定（避免反复挂账）

- **§13.13 反向结果入共享内存**：原设想把 `doc.query` / `llm.chat` 的大结果
  也搬进段。核实后：**本仓根本不存在 `llm.chat`**（`llm.*` 只映射
  listSources/setSource/currentSource）；唯一可能返回大结果的 `doc.query`
  被 `CapDocMemory` 能力门挡着，且无任何外部插件使用。**不做**，
  而不是留成永久 TODO。

---

*重写时间：2026-09-24　·　核实方式：源码 grep / build / test / race*
*旧档备份于 `/tmp/plan.md.bak-*`（仅作对照，内容已判定过时）*
