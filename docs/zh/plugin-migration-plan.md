# 外部插件多进程化适配计划（修改→审查→验证三步微循环）

> 分支：`update`
> 基线：`docs/zh/plugin-interface-matrix.md`（合同面 A/B/C）+ `plan.md` §11 + `docs/zh/架构迁移评估.md`
> 每部分 = 一个「修改 → 审查 → 验证」三步微循环。所有验证在 **update 分支**完成，可独立交付、可回退。
>
> **循环的铁律**（每部分适用）：
> - **修改**：只动核心侧 + 工具链，`third_party/homeagent-sdk/sdk/`（合同面 A）**零 diff**。
> - **审查**：接口冻结检查（`git diff` 公开 SDK 为空）+ 代码 review + `go vet`。
> - **验证**：`make test` + 针对性单测 + 端到端冒烟，产物 `.bin` 端到端可用。
>
> 标 `【M】`=修改部分、`【R】`=审查部分、`【V】`=验证部分。依赖前置部分完成后才可开始。

---

## 目录

- **Part 0** 脆弱基线先行（不依赖迁移，现网可直接受益）
- **Part 1** 加载分派骨架（`entry` 双通道共存）
- **Part 2** 子进程通道原型（spawn / JSON-RPC / procPlugin）
- **Part 3** plugindev 工具链改造（`.bin` 产物）
- **Part 4** 共享内存数据面（StageContext 跨进程并发改写）
- **Part 5** 通知面（事件环 + eventfd）
- **Part 6** 迁移与收尾（17 插件逐个 + 删 cabi + 权限显式化）
- 最终验收清单

---

## Part 0：脆弱基线先行（阶段 0，~1 人日）

> 依据：plan.md §11.1/11.3/11.6。不依赖任何新架构，独立交付，现网直接受益。
> 目的：在副本模型内部打补丁，止血，为后续迁移争取时间。

### 0.1 output_send 假成功修复（11.1）— ✅ **已完成**（2026-08-31）

- 【M】✅ `internal/plugin/cabi/loader.go`——`CORE_REGISTER_OUTPUT_CH`（:454）的异步 output 从「goroutine 直接返回 queued」改为「goroutine + 带超时 channel 等真实结果」。
  新增 `awaitOutputResult`（:276）+ 可注入版 `awaitOutputResultWith`（:281）+ 常量 `outputSendTimeout = 10s`：
  ```go
  resCh := make(chan error, 1)
  go func() { resCh <- invoke(pid, channel, argsJSON) }()
  select {
  case err := <-resCh:
      if err != nil { return nil, err }               // 真实失败上报
      return map[string]interface{}{"status": "sent"}, nil
  case <-time.After(timeout):
      return map[string]interface{}{"status": "unconfirmed", "note": "..."}, nil
  }
  ```
  关键：`dev.Execute` 由 `executeOutputSendTool` 从 Go 侧调起（不在 cgo 栈内），goroutine 内的 `pluginInvokeOutput` 才是 cgo，**不构成嵌套**。
- 【M】✅ `internal/agent/core/output.go` `executeOutputSendTool`：识别 `status=unconfirmed|queued` → 返回「发送结果未确认：<note>」而非「已发送」，把未确认状态透传给模型。
- 【R】✅ 无 cgo 嵌套（`awaitOutputResult` 只在 `RegisterOutputChannel` 的 handler 内被调用，该 handler 从 Go 侧调起）；
  「超时未确认」措辞与 11.2 的"已取消"谎言区分——用 `unconfirmed` + 显式 note，不谎报成功也不谎报失败。
- 【R】✅ 接口冻结：`git diff third_party/homeagent-sdk/sdk/` 为空。
- 【V】✅ 新增 `internal/plugin/cabi/output_test.go` 三用例全绿：
  - `TestAwaitOutputResult_Success` → `status=sent`
  - `TestAwaitOutputResult_Failure`（模拟 meta 缺 user_id）→ **返回 error**（旧实现会谎报成功）
  - `TestAwaitOutputResult_Timeout` → `status=unconfirmed` 且不返回 error
- 【V】✅ `go build ./...` exit 0；`go test ./internal/plugin/... ./internal/agent/...` 全绿。

### 0.2 stage lost update 补丁（11.3）

- 【M】`templates.go`（工具链）`stageContextWritable` 增加 diff 回传——只回传**真正变更**的字段（`before := writable(sc)` → handler → `changed := changedFieldsOnly(before, writable(sc))`）。
- 【R】确认 `changedFieldsOnly` 不引入竞态、对只读插件零回传。
- 【V】weather 调用后 tool_results 保持 sanitizer 已清洗状态（复刻实验 13 场景，丢失率 → 0）。
  ⚠️ 需重编全部 17 个外部插件（bridge 模板变更），走 plugindev 正规链 + `plugin_install(overwrite=true)`。

### 0.3 reload 语义修正（11.6）

- 【M】`dynamic_loader_unix.go`：ELF 检测 `DF_1_NODELETE` → 标记"不可热重载"。
- 【M】`registry.go` 的 `ReloadOne`：对此类插件返回"需重启 homed"。
- 【M】`pluginmgr/plugin.go` 的 `plugin_install`：返回 `restart_required` 替代 `reload_required`。
- 【R】确认 `.so` 插件重载不再"假成功"。
- 【V】单测：mock ELF 头带 NODELETE vs 不带 → 正确区分。

### 0.4 超时日志措辞修正 + 附带（11.2 短期项 + 11.4）

- 【M】`internal/agent/core/toolcall.go:41`：日志从"已取消"改为"已放弃等待（插件仍在后台运行，其占用的线程无法回收）"。
- 【M】`internal/plugin/lua_plugin.go:726`：stage 快照加 `sc.RLock()`/`RUnlock()`（11.4）。
- 【R】措辞语义诚实；Lua 快照持锁。
- 【V】`make test` 全绿；超时日志不再撒谎。

**Part 0 出口条件**：11.1/11.3/11.6 全部落地并有针对性测试；生产可先部署（现网止血）。

---

## Part 1：加载分派骨架（阶段 2.4，S）

> 依据：迁移评估 §2.4 / 3.2；plan.md 11.7。目标：让 registry 能按 entry 把插件分派到 `.so`（cabi）或 `.bin`（proc）两条通道——**双通道共存是整个计划可回退的前提**。

### 修改（核心）

- 【M】`internal/plugin/manifest.go`：`PluginManifest.Entry` 注释与 `IsPluginDir` 支持 `plugin.bin`。
- 【M】`internal/plugin/dynamic.go`：新增 `binEntry = "plugin.bin"` 常量；`readManifest` 读取 entry。
- 【M】`internal/plugin/registry.go` `loadOne`（~:376）：把「无工厂 → `tryDynamic`」的分支改为按 entry 分派：
  ```go
  switch entry {
  case soEntry, dllEntry:  p, err = r.tryLoadSO(...)   // 现有 cabi
  case binEntry:           p, err = r.tryLoadProc(...) // 新增（Part 2 填充）
  default:                 p, err = r.tryOther(...)    // lua / skill
  }
  ```
  先保留一个 `tryLoadProc` 桩（返回"未实现"错误），保证分派骨架先成立、可测。
- 【M】`internal/plugin/dynamic_loader_unix.go`：把 `tryLoadSO` 从 `tryDynamic` 拆出成 registry 可独立调用的函数。

### 审查

- 【R】确认内置插件（`hasFactory` 分支）完全不受影响——仍走 `RegisterNative` 进程内路径。
- 【R】确认 `.so` 路径行为与今天逐字节一致（无回归）。
- 【R】接口冻结：`git diff` 公开 SDK 为空。

### 验证

- 【V】单元测试：mock 三种 manifest（so/dll/bin/lua）→ 分派到正确通道；`.bin` 桩返回明确错误而非 panic。
- 【V】既有 `.so` 插件加载 e2e 不回归（带一个真实 .so 冒烟）。

**Part 1 出口条件**：分派骨架在，`.bin` 有明确桩位，`.so` 全回归。

---

## Part 2：子进程通道原型（阶段 2.1~2.3/2.5/2.9，~3 周，核心风险点）

> 依据：迁移评估 §4.1 阶段 2；迁移评估指明可大幅参考 `clawhubadapter/sidecar.go:54-350`（已有 stdin/stdout + pending map + notifyCh）。
> 目标：把单个外部插件（weather）以 `plugin.bin` 端到端跑通，验证"接口不变"假设。

### 修改（核心）

- 【M】新建 `internal/plugin/proc/`：
  - `process.go`——`procPlugin` 实现 `sdk.Plugin` 接口；`spawn`/健康检查/优雅停止/`Close()`=真 kill+wait。
  - **可参考** `clawhubadapter/sidecarProcess`：`exec.Cmd` + `stdin *bufio.Writer` + `readLoop`（scanner 大 buffer 64KB）+ `pending map[int]chan<- []byte` + `notifyCh chan OCNotification` + readerStop/readerWg。
  - `rpc.go`——双向 JSON-RPC 编解码：7 个 kernel→plugin 调用（`tool.invoke`/`stage.invoke`/`output.invoke`）+ 51 个 plugin→kernel 回调（平移自合同面 B 映射表）。
- 【M】`internal/plugin/dynamic_loader_unix.go`：实现 `tryLoadProc`（spawn `.bin`，回连 stdio RPC）。
- 【M】`internal/plugin/registry.go` `closePlugin`/卸载路径：对 proc 插件 `Close()` 真 kill。
- 【M】`internal/agent/core/plugin_health.go` 调用侧：插件**退出码/EOF** → `recordCrash`（**逻辑完全复用**，仅把"panic 捕获"换成"进程退出检测"，见迁移评估 §2.3）。

### 审查

- 【R】`readLoop` 鉴权：只接受来自本进程 spawn 的 stdout（防注入）。
- 【R】JSON-RPC 帧边界处理（`bufio.Scanner` 长行截断风险——沿用 sidecar 64KB buffer）。
- 【R】pending map 泄漏：超时清 map、退出时清 map。
- 【R】崩溃重启：`SetAutoRestart(true)` 语义保留；`plugin_health` 冷却/自愈复用。
- 【R】接口冻结：公开 SDK 零 diff。

### 验证

- 【V】单测：spawn→握手→工具调用往返→正常 Stop→kill 崩溃→退出码捕获。
- 【V】weather `.bin` 端到端：`RegisterTool`/`Settings`/`InjectInputSync` 全部经 stdio RPC 打通。
- 【V】与 Part 1 的 entry 分派联动：同目录 `.so` 与 `.bin` 共存互不干扰。

**Part 2 出口条件**：一个真实外部插件 `.bin` 全链路可用，崩溃隔离生效，接口零改动。

---

## Part 3：plugindev 工具链改造（阶段 2.6/2.7/2.8，M，SDK 仓）

> 依据：合同面 B；迁移评估 §4.1。此部分在**独立 SDK 仓**维护（用户决策 sdk_repo_only）。
> 目标：让外部插件能用普通 `go build` 产出 `.bin`，业务代码零改动。

### 修改（工具链）

- 【M】`tools/plugindev/templates.go`：新增 `tmplProcMain`——把 bridge 从「7 个 `//export` + `-buildmode=c-shared`」改为「`main()` + stdio JSON-RPC loop」；注册逻辑（`buildPluginSDK` 的 registar 闭包）从 `callVoid(id,...)` 改为 `sendRPC(methodName,...)`（合同面 B 的平移）。
- 【M】`tools/plugindev/cmd_build.go`：
  - 新增目标 `plugin.bin`：`go build`（去 `-buildmode=c-shared`、`CGO_ENABLED=0`）→ `plugin.bin`。
  - bundle 平台表：`{"linux/amd64","plugin.bin"}`（替代 `.so`）。
  - `resolveBuild`：bin 分支不再需 C 编译器。
- 【M】`tools/plugindev/cmd_build.go` `validBinaries`/打包：`.hmap` 内条目支持 `plugin.bin`（`plugin.json` entry 写 `plugin.bin`）。
- 【M】`plg.json` 模板（`tmplPlgJSON`）：`entry` 默认改为 `plugin.bin`（保留 `.so` 兼容）。

### 审查

- 【R】生成的 `tmplProcMain` 与旧 bridge 的 SDK 方法一一对应（对照合同面 B 51 行映射表逐行核对）。
- 【R】业务代码**零改动**证据：同一 `plugin.go`，仅入口文件/构建命令不同。
- 【R】交叉编译简化确认：`.bin` 无需 cgo 工具链，跨 GOOS 仅需目标 toolchain。

### 验证

- 【V】用新 plugindev 重编 `example/weather` → 产出 `plugin.bin`。
- 【V】`.hmap` 打包/解包校验：`plugin.bin` 条目正确登记。
- 【V】（与 Part 2 集成）weather.bin 被 homed proc 通道正确加载运行。

**Part 3 出口条件**：plugindev 一条命令产出 `.bin` + 正确 `.hmap`，外部插件源码零改动。

---

## Part 4：共享内存数据面（阶段 3.1~3.5，~3 周，最高风险）

> 依据：迁移评估 §3.3 数据面 / 3.4 SDK 封装 / 3.7 锁仲裁；合同面 C。
> 目标：多插件并发改写同一 `StageContext` 语义与今天一致（丢失率 → 0），外部插件看到全部 16 字段。

### 修改

- 【M】`internal/plugin/proc/` 新增 `shm.go`：
  - 共享段 schema：`ShmStageCtx` + `Slice{off,len}` 偏移描述符 + arena（append-only + 压实）。
  - arena 分配器：插件把 `FinalText` 从 10B 改 10KB 时分配新区域、旧区域留垃圾、stage 结束后压实。
  - 4 个 `Extra` 键（media_blocks/media_type/input_source/output_channel）提升为具名字段（迁移评估 §3.3 已核实全部使用点）。
  - 段生命周期：创建/挂载/插件崩溃后清理。
- 【M】`internal/plugin/proc/shmcodec.go`：`StageContext` ↔ 共享段编解码（偏移↔Go 值转换）。
- 【M】`internal/plugin/proc/lock.go`：**锁仲裁 RPC**——插件 `Lock/RLock` → `stage.lock`/`stage.unlock` → 内核 `sync.Mutex` 排队（迁移评估 §3.7 已裁定，实验 3+9 支撑）。
- 【M】`internal/agent/core/stages.go` `RunStage`：改造为跨进程并发扇出（**保留并发语义，最难一环**）——内置插件仍进程内 `go func`，外部插件走共享段 + 锁仲裁。
- 【M】SDK 侧（插件进程内）封装全部复杂度（迁移评估 §3.4）：插件保留原生 `StageContext`，handler 照常读写，脏字段写回共享段。

### 审查（最高优先级 review）

- 【R】**并发语义一致性**：内置（0% 丢失）与外置（迁移前 35.8~36.8%）在共享内存下都收敛到 0% 丢失。
- 【R】锁仲裁死锁：持锁进程崩溃自愈（实验 9 已证无需 robust mutex）。
- 【R】arena 单 stage 写入上限：大写入在 SDK 层**报错**而非静默截断（迁移评估 §4.4）。
- 【R】`Extra` 不引入通用 tagged union 成本（维持 4 键具名字段）。
- 【R】接口冻结：`sdk/` 零 diff；`StageContext` 结构体字段序不变。

### 验证

- 【V】复刻实验 8：5 子进程 × 300 轮并发改写 → **零丢失零撕裂**。
- 【V】复刻实验 13 现网场景：sanitizer（改 ToolResults）+ weather（只读）并发 → 清洗结果不再被覆盖。
- 【V】改写型插件行为基线测试：`sanitizer`/`multimodal` 迁移前后行为对拍（迁移评估 §4.4 风险缓解）。

**Part 4 出口条件**：跨进程并发改写零丢失，内置/外置语义一致，16 字段全可见。

---

## Part 5：通知面（阶段 4.1~4.5，~1.5 周）

> 依据：迁移评估 §3.6 事件环 / §2.4 约束 B / §3.8。目标：外部插件首次获得事件订阅能力，且不阻塞流式输出。

### 修改

- 【M】`internal/plugin/proc/eventring.go`：`EvtRing` + `Subscriber` schema（write_seq/read_seq/dropped/type_mask/last_seen），溢出计数、允许丢但让消费者知道丢了。
- 【M】eventfd 通知 + Go netpoller 消费：`unix.Eventfd(EFD_NONBLOCK|EFD_CLOEXEC)` + `os.NewFile` 注册 netpoller（**不占 OS 线程**——实验 1 已证 200 goroutine 仅 +1 线程）。
- 【M】`internal/events/bus.go` `Publish`：加事件环投递（**post-and-forget，绝不等待消费者**，满足约束 B）。
- 【M】实现 `case 23/24`（今天空实现）——`Events().Subscribe` 对外部插件真正可用。
- 【M】订阅者活性检测：`last_seen` 超时 → `recordCrash`。

### 审查

- 【R】`Bus.Publish` 路径**禁用任何锁/阻塞**——流式输出逐 token 发布，任何等待都会卡顿（迁移评估 §4.3 风险高）。
- 【R】溢出语义：drops 计数暴露，不静默丢。
- 【R】eventfd 计数合并：1000 token 事件只唤醒几次。

### 验证

- 【V】流式压测：长回复下 Publish 单次耗时不随订阅者数线性恶化。
- 【V】复刻实验 4：post-and-forget 解耦（5s → 2.3ms 量级）。
- 【V】外部插件订阅事件端到端（原空实现 case 23/24 现在可用）。

**Part 5 出口条件**：事件订阅对外可用，流式输出无卡顿。

---

## Part 6：迁移与收尾（阶段 5.1~5.4，~2 周）

> 依据：迁移评估 §4.5 双通道共存、§5 权限梯度。逐插件迁移，随时回退。

### 修改

- 【M】17 个外部插件逐个用新 plugindev 重编为 `.bin`（`plugin_install(overwrite=true)`），每个回归验证。
- 【M】`plugins/` 目录逐个把 `entry` 从 `plugin.so` 改为 `plugin.bin`。
- 【M】删除 `internal/plugin/cabi/`（1096 行）+ bridge 模板 `tmplLinuxBridge`/`tmplBridge`（385 行）+ `dynamic_dll_*`/`dynamic_loader_windows.go`。
- 【M】权限梯度显式化：manifest 声明 caps + 内核侧白名单（`Selftest`/`Supervisor`/`Tracker`/`Status`/`Adapter`/`Config`/`Tool`/`Indexer`/`OutputChan`/`Publish` 确认不给）。
- 【M】`lua_plugin.go`/`dynamic_lua.go`：统一走 RPC（收敛三套 ABI 为单一 RPC）。
- 【M】文档：`PLUGIN_DEV.md` 更新、迁移说明。

### 审查

- 【R】每删一个 cabi 依赖项，`go build ./...` + `go vet ./...` 干净。
- 【R】权限梯度：外部插件无权访问的 API 在 RPC 边界被**拒绝**（非忽略）。
- 【R】接口冻结：`sdk/` 零 diff。

### 验证（全量回归）

- 【V】17 插件每个 `.bin` 独立回归（工具/设置/通道/阶段）。
- 【V】`.so` ↔ `.bin` 混跑集群冒烟（Part 1 分派 + 双通道共存）。
- 【V】`make test` 全量绿 + `go build ./...`。
- 【V】内存/RSS 对比：迁移后常驻 ≤ 基线 +29MB（实验 5 量级）。
- 【V】工具调用 RPC 延迟 p50 ≤ 20µs 量级（实验 11）。

**Part 6 出口条件**：全部外部插件 `.bin` 化，cabi 删除，接口零改动，权限显式化，无回归。

---

## 最终验收清单（对照接口不变矩阵 §7 检查点）

| # | 检查点 | 通过标准 |
|---|---|---|
| 1 | 公开 SDK 接口冻结 | `git diff third_party/homeagent-sdk/sdk/` **为空**（全程） |
| 2 | 外部插件业务代码零改动 | 17 个 `example/*/plugin.go` 与基线逐字节可比 |
| 3 | 17 插件 `.bin` 化 | 全部经 `plugin_install` 加载，工具/设置/通道/阶段 e2e |
| 4 | cabi 删除 | `internal/plugin/cabi/` 与 bridge 模板不存在 |
| 5 | 崩溃隔离 | 插件 kill 只退出自身，homed 存活 |
| 6 | 热重载 | 同路径换 `.bin` 即生效，无需重启 |
| 7 | 并发改写 | 跨进程 stage 丢失率 0%（对照今天 35.8~36.8%） |
| 8 | 事件订阅 | 外部插件 `Events().Subscribe` 可用 |
| 9 | 多模态 | `SetToolBlocks` 非空实现 |
| 10 | 超时取消 | 工具超时可 `Process.Kill()`，零泄漏 |
| 11 | output_send | 真实结果返回（非假成功） |
| 12 | 权限梯度 | 内部专属 API 在 RPC 边界拒绝 |
| 13 | 内存/延迟 | 常驻 +≤29MB，RPC p50 ≤20µs 量级 |

---

## 风险与回退

| 风险 | 缓解 | 回退 |
|---|---|---|
| Part 2/4 `RunStage` 并发语义漂移 | 复刻实验 8/13 + sanitizer/multimodal 对拍（Part 4 review） | entry 分派切回 `.so`（Part 1 双通道） |
| Part 4 `Bus.Publish` 阻塞卡顿 | 专项流式压测（Part 5） | 事件环投递后置，先降级进程内 |
| Part 3 工具链 `.bin` 产物问题 | 单插件 weather 先行验证 | 保留 `.so` 构建分支 |
| Part 6 17 插件回归 | 逐个迁移 + `plugin_install(overwrite)` | 任意一个失败立即回退该插件 entry |
| 接口意外漂移 | 每部分【R】强制 `git diff sdk/` 检查 | 立即 revert，暴露合同面违约 |

---

*规划：2026-08-31，update 分支。Part 编号与其依赖的 plan.md/迁移评估阶段对应。*
