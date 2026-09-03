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

- **Part 0** 脆弱基线先行（不依赖迁移，现网可直接受益）— 0.1 ✅ / 0.2 ✅ / 0.3 ⏭️ / 0.4 ⏭️
- **Part 1** 加载分派骨架（`entry` 双通道共存）— ✅ **已完成**
- **Part 2** 子进程通道原型（spawn / JSON-RPC / procPlugin）— ✅ **已完成**
- **Part 3** plugindev 工具链改造（`.bin` 产物）— ✅ **已完成**
- **Part 4** 共享内存数据面（StageContext 跨进程并发改写）— ✅ **已完成**（段/编解码/锁仲裁 + RunStage 接线）
- **Part 5** 通知面（事件环 + eventfd）— ✅ **核心已完成**
- **Part 6** 迁移与收尾（17 插件逐个 + 删 cabi + 权限显式化）
- 最终验收清单

> **进度快照（2026-09-02）**：分支 `feature/plugin-proc-migration`。
> 已交付：现网止血 2 项（11.1/11.3）、entry 双通道分派、共享内存 stage 并发、
> 子进程控制面（NDJSON RPC + 51 method 名平移）、plugindev `.bin` 构建、
> registry 接线、**事件环（§3.6）**。**外部插件已可端到端跑在子进程 + 共享内存上**，
> 且首次获得事件订阅能力（C ABI 下 case 23/24 一直是空实现）。
> 测试：内核 `internal/plugin/proc` 38 项 + `internal/plugin` 16 项（含 `-race`），
> SDK 仓 plugindev 16 项。
 > 下一步：Part 6 逐插件迁移 + 删 `internal/plugin/cabi/`。

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

### 0.2 stage lost update 补丁（11.3）— ✅ **已完成**（2026-08-31）

- 【M】✅ `templates.go`（**SDK 仓** update 分支 `5648519`）`go_invoke_stage` 改为 diff 回传：
  - 新增 `snapshotWritable(sc) map[string]string`——handler 前的**序列化**快照
  - 新增 `changedFieldsOnly(before, after)`——只回传变更字段，无变更零回传
  - ❗ **第一版踩坑并修正**：`stageContextWritable` 返回的切片字段与 `sc` **共享底层数组**，handler 原地改元素（`sc.ToolResults[0].Result = clean`）时 before 快照跟着变，diff 看不到变更 → 修复会静默失效。故 before 必须逐字段序列化成字符串。
- 【M】✅ `internal/plugin/cabi/loader.go` `applyStageResult` 配套（本仓 `9bb9cb3`）：`tool_calls`/`tool_results` 去掉 `len(v)>0` 拦截——改为键存在即应用，使插件「清空全部工具调用」的显式 `[]` 能被表达（旧插件仅 len>0 才带键，不会被误清空）。
- 【R】✅ `changedFieldsOnly` 无竞态（纯函数，无共享状态）；只读插件零回传（单测断言）。
- 【R】✅ 接口冻结：两仓 `git diff sdk/` 均为空（只改 bridge 模版 + 内核）。
- 【R】✅ bridge 模版可编译性：抽取 `tmplLinuxBridge` + 真实 `weather/plugin.go` 做 `go build -buildmode=c-shared` → exit 0。
- 【V】✅ SDK 仓 `tools/plugindev/stagediff_test.go` 6 用例全绿：
  - `_ReadOnlyPluginReturnsNothing`（只读插件零回传——修复核心）
  - `_WriterReturnsOnlyChanged`（原地改切片元素仅回传 tool_results）
  - `_ScalarChange` / `_NewResponseIsReturned` / `_ClearedSliceIsReturnedAsEmpty`
  - `_ProductionScenarioNoOverwrite`（**复刻实验 13 现网场景**：sanitizer 清洗 + weather 只读，清洗结果不再被覆盖）
- 【V】✅ 内核侧 `output_test.go` 新增 `TestApplyStageResult_ClearedSlicesAreApplied` / `_OnlyPresentKeysApplied` 全绿。
- 【V】✅ `go build ./...` exit 0；`go test ./internal/plugin/... ./internal/agent/...` 全绿。
- ⚠️ **待部署项**：需用新 plugindev 重编全部 17 个外部插件（bridge 模版变更），走 `plugin_install(overwrite=true)`。

### 0.3 reload 语义修正（11.6）— ⏭️ **已跳过**（2026-08-31 用户决策：直接进入进程化重构）

> 子进程模型下 `DF_1_NODELETE` 议题**整体消失**（§3.1）——同路径替换 `plugin.bin` 重启进程即生效。
> 在 cabi 路径上补 ELF 检测属于「给即将删除的代码打补丁」，性价比低。
> 现网仍受 reload 假成功影响，但 Part 1 的 entry 分派已为迁移铺路，迁移完成即根治。

- 【M】`dynamic_loader_unix.go`：ELF 检测 `DF_1_NODELETE` → 标记"不可热重载"。
- 【M】`registry.go` 的 `ReloadOne`：对此类插件返回"需重启 homed"。
- 【M】`pluginmgr/plugin.go` 的 `plugin_install`：返回 `restart_required` 替代 `reload_required`。
- 【R】确认 `.so` 插件重载不再"假成功"。
- 【V】单测：mock ELF 头带 NODELETE vs 不带 → 正确区分。

### 0.4 超时日志措辞修正 + 附带（11.2 短期项 + 11.4）— ⏭️ **已跳过**（同上）

> 11.2 的 cgo 超时不可中断在子进程模型下由 `Process.Kill()` 真正解决（§9.5）；
> 11.4 的 Lua 路径在迁移后统一走 RPC（三套 ABI 收敛），锁语义天然有边界。

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
#### ✅ **Part 1 已完成**（2026-08-31，commit `610e9d0`）

- 【M】✅ `dynamic.go`：新增 `binEntry`/`skillEntry` 常量 + `entryKind` 枚举 + `classifyEntry` / `detectEntryKind`
  - **manifest 的 entry 优先级最高**——把 entry 改回 `plugin.so` 即回退 cabi 通道（回退路径的保证）
  - 无 manifest 时按目录探测，`.bin` 优先于 `.so`（迁移期同目录两产物共存时走新通道）
- 【M】✅ `registry.go` `tryDynamic`：按 entry 分派 proc/cabi；entry 声明 `.bin` 但二进制缺失时**报明确错误，不静默回退**
- 【M】✅ `registry.go` `pluginEntryHash`：候选顺序与 `detectEntryKind` 对齐（`.bin` 优先），否则增量重载会用错文件算 hash
- 【M】✅ `manifest.go`：`Entry` 字段注释补 `plugin.bin`
- 【M】✅ `dynamic_proc_unix.go` / `dynamic_proc_windows.go`：`tryLoadProc` 桩位（存在性/类型/可执行权限校验已实现）
- 【R】✅ 内置插件（`hasFactory` 分支）完全未受影响——仍走进程内 `RegisterNative`
- 【R】✅ `.so` 路径行为与改动前一致（既有测试全绿，无回归）
- 【R】✅ 接口冻结：`git diff third_party/homeagent-sdk/sdk/` 为空
- 【V】✅ `entry_dispatch_test.go` 9 项全绿：
  - `TestClassifyEntry`（8 种 entry 分类）
  - `TestDetectEntryKind_ManifestWins` / `_ManifestCanForceRollback`（**回退路径验证**）
  - `TestDetectEntryKind_ProbeOrderPrefersBin` / `_ProbeFallbacks`（4 子例）
  - `TestTryLoadProc_MissingBinaryReturnsNil` / `_NonExecutableRejected`
  - `TestPluginEntryHash_PrefersBin` / `_EmptyForFactoryOnlyPlugin`
- 【V】✅ `go build ./...` exit 0；`go test -race ./internal/plugin/...` 全绿；全量 32 个包测试通过


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

#### ✅ **Part 2 已完成**（2026-09-01，commit `d62430a` + `82dcc86`）

- `proc/protocol.go`：NDJSON 帧、**51 个 method id 平移为 method 名**（编号扔掉）、握手/stage/tool/output 参数类型。
  `case 25`(CORE_FREE_STRING) 无对应 method（GC 接管）；`case 23/24`(事件订阅) 与 `io.setToolBlocks`
  明确返回未实现，**不静默成功**。
- `proc/process.go`：Spawn/readLoop/CallContext/Notify/Stop/Kill/markExited；单帧上限 1MB。
- `proc/corehandler.go`：51 case 平移 + `CoreSDK` 接口（**刻意排除**内核内部机制，见 Part 6 权限梯度）。
- `proc/host.go`：**全部插件共享同一 memfd**。最初写成每插件一块段，尝试后发现
  那等于**副本模型换壳**（各写各段、各自回读、最后回读者覆盖前者），已改正。
- `proc/stage.go`：RunStage 接线 + lockRegistry；`proc/plugin.go`：Plugin 实体。
- 共享段分配按平台拆分（`shmalloc_linux.go` memfd / `shmalloc_darwin.go` 立即 unlink 的临时文件 /
  `shmalloc_other.go` 明确报错）——不静默降级成「无共享段」，那会让 stage 静默失去数据面。
- registry 接线（commit `11c1bbc`）：`tryDynamic` → `Registry.loadProc`；Host 惰创建且全局唯一；
  `StopAll` **锁外**释放共享段（插件还持有映射时拆段 → SIGBUS；持锁调与 onProcCrash 有锁序风险）；
  `onProcCrash` 只发 EventSystem 事件，**不在回调里直接重载**（重载需 registry 锁）。
- `proc_core.go` —— 权限梯度的类型系统落点：`procCore` 用**命名字段**持有 `*isdk.PluginSDK`，
  不是嵌入。嵌入会提升全部方法，外部插件就能经类型断言拿到
  Supervisor/Tracker/Adapter/Indexer/Status/Selftest。
- 测试 36 项含 `-race`：`testdata/` 8 个假插件 + `e2e_template_test.go` 用**真实 plugindev 模板**
  编译插件跑全链路（验证「模板 ↔ 内核」协议/布局真的对齐，不只是内核自己跟自己对齐）。

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

#### ✅ **Part 3 已完成**（2026-09-02，SDK 仓 commit `09b64dc`）

**模板落地方式换了**：不是计划里的 `templates.go` 新增 `tmplProcMain` raw string，
而是真实 `.go` 源文件 `templates/proc_main.go.tmpl` + `//go:embed`（`proc_runtime.go`）。
原因：900+ 行代码塞在字符串里写错只能等生成插件时才炸，作为源文件可被
`go/parser`、`gofmt`、`go vet` 直接检查。这也是 `proc_runtime_test.go` 16 项
静态检查得以存在的前提。

- `templates/proc_main.go.tmpl`（1113 行）：51 个 method 的插件侧 RPC 实现
  （`procIO`/`procMemory`/`procSettings`/`procSocial`/`procLLM`/`procKnowledge`/
  `procDocMemory`/`procTextMemory`/`procPluginMgr`）、共享段访问（fd 3）与 16 字段
  StageContext 编解码、`handleStageInvoke`（拿锁 → 读段 → handler → **只写脏字段** → 放锁）。
- `cmd_build.go`：`resolveBuild(target, proc)` 分派；proc 走 `go build -trimpath` + `CGO_ENABLED=0`，
  **交叉编译不再需要目标平台 C 工具链**。bundle 模式各平台产物同名（进程边界即 ABI 边界，
  无平台扩展名），故 zip 内加平台后缀 `plugin.bin.linux.amd64`。
- `proc_runtime.go`：生成时清理残留 `z_bridge_gen.go`/`z_entry.c`——同目录两套 main 会编译冲突，
  这让 `.so` → `.bin` 切换无需人工清理。

**计划外补的一个真缺口**：`lifecycle.autoRestart` 没接线。公开 SDK 的 `SetAutoRestart`
是纯 setter（`s.autoRestart = enabled`，无回调 hook）。C ABI 下内核在 `Start` 返回后
直接读 `plgSDK.AutoRestart()`；子进程隔着进程边界读不到，插件调它只改自己进程内的副本。
修法：模板在 `plg.Start()` 返回后显式上报一次（内核侧 `corehandler.go:145` 早已就绪）。
**没有改公开 SDK 接口**。

验证（均已实测）：
```
$ plugindev build              # plg.json: entry = "plugin.bin"
  compiling linux/amd64 (子进程模式，CGO_ENABLED=0)...
  packaged weather_linux_amd64.hmap

build/plugin.bin  →  ELF 64-bit executable, statically linked   ← 零 cgo
dist/*.hmap       →  plugin.json + plugin.bin

$ diff example/weather/plugin.go <构建目录>/plugin.go
✅ 逐字节一致                    ← 业务代码零改动的硬证据

$ git diff third_party/homeagent-sdk/sdk/
(空)                            ← 接口冻结保持
```

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
#### ✅ **Part 4 核心已完成**（2026-08-31，commit `610e9d0`）—— 段 / 编解码 / 锁仲裁三件套

> 用户明确指出「基于共享内存的 stage 并发是最为关键的」，故先于 Part 2/3 落地数据面。
> `RunStage` 的跨进程接线（3.4）待 Part 2 的进程通道就绪后进行。

- 【M】✅ `proc/shm.go` 段布局与 arena 分配器（§3.3）
  - `Header(64B) + ShmStageCtx(描述符数组 + 标志位) + append-only arena`
  - **相对偏移**：各进程 mmap 到不同虚拟地址仍能正确解引用
  - `NewSegment` / `AttachSegment` 带魔数 + 版本校验（版本不匹配显式报错，不静默错读）
  - **arena 用尽显式报错**而非静默截断（§4.4 风险登记的硬要求）
  - `Compact()` 回收 append-only 垃圾，须在无插件持锁时调用
- 【M】✅ `proc/shmcodec.go` StageContext 16 字段跨进程编解码（§3.4）
  - **字段级描述符消除 lost update**：只改 `FinalText` 的插件完全不触碰 `ToolResults` 描述符
  - `WriteDirty` 只写脏字段——**只读插件零写入**，不可能覆盖他人改写
  - `Snapshot` 存**序列化字符串**（切片共享底层数组的坑，C ABI 侧修 11.3 时已踩过一次）
  - `Extra` 4 键提升为具名字段；`Response` 用标志位区分 nil 与空串（短路语义）
  - **全 16 字段可见**——今日经 C ABI 只有 10 个，`ContextMsgs`/`ReasoningContent`/`TokenUsage`/`Memory`/`Extra`/`Errors` 首次对外部插件可见
- 【M】✅ `proc/lock.go` 锁仲裁回归内核（§3.7 已裁定，**零 cgo**）
  - `ForceRelease` 实现实验 9 的崩溃自愈 → 排除 robust pthread_mutex 必要性
  - 重复加锁**显式拒绝**（否则死锁 30s，比挂死更难排查）
  - 等待超时有补偿 goroutine 防锁永久泄漏
- 【R】✅ 并发语义：`TestSegment_ConcurrentAppend_NoLostUpdate` 断言「各标记计数之和 == 最终长度 且 == 期望写入次数」，同时排除丢失与撕裂
- 【R】✅ arena 上限报错（非静默截断）：`TestSegment_ArenaExhaustionReturnsError`
- 【R】✅ `Extra` 维持 4 键具名字段，未引入通用 tagged union 成本
- 【R】✅ 接口冻结：`sdk/` 零 diff；`StageContext` 结构体未改
- 【R】✅ `go vet` 干净（含 copylocks 检查）
- 【V】✅ proc 包共享段部分 **16 项测试全绿（含 `-race`）**（全包现 36 项，含进程/端到端）：
  - 段：魔数/版本校验、全 16 字段往返、Response nil vs 空串
  - 脏字段：只读零写回、原地改切片被识别、压实不破坏字段
  - **现网场景复刻**：`TestSegment_ProductionScenario_SanitizerNotOverwrittenByWeather`（sanitizer 清洗 + weather 只读并发，清洗结果不被覆盖）
  - **并发零丢失**：5 插件 × 40 轮读-改-写同一字段，200 次写入全部保留
  - 锁：互斥、串扰拒绝、未持锁释放拒绝、重复加锁拒绝、**崩溃自愈**、定向强制释放、临界区串行化

#### ✅ **Part 4 RunStage 接线已完成**（2026-09-01～09-02）

- `proc/stage.go` 把内核 `RunStage` 的并发扇出接到共享段：
  `Host.beginStage`（首个到达者独占段并写入 StageContext）→ `stage.invoke` RPC →
  插件侧 `stage.lock` → 读段 → handler → 只写脏字段 → `stage.unlock` →
  `Host.endStage`（最后离开者回读 + 压实 arena）。
- **并发扇出保留**（§0.2 第 1 条：并发扇出是原始设计，不是缺陷）；
  `stageMu` 串行化整次 stage 对共享段的独占（内核可能在不同路径并发触发
  RunStage，而段只有一份）。
- 端到端验证（`e2e_template_test.go`，用**真实 plugindev 模板**编译的插件，
  而非 `testdata/` 手写假插件——后者只能验证内核自己跟自己对齐）：
  - `TestE2E_RealTemplatePluginFullLifecycle`：握手 → init/start → 反向注册 →
    工具调用 → stage 读改写；同时验证 `FinalText` 回传
    （**C ABI 下 after_toolcall 看不到此字段**，§8.3 10→16）
  - `TestE2E_RealTemplateReadOnlyPluginDoesNotOverwrite`：两插件共享同一 Host 并发，
    只读插件不覆盖改写插件的结果（若每插件一块段，此测试必然失败）

**Part 4 已整体完成**。

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

## Part 6：迁移与收尾（阶段 5.1~5.4，~2 周）— ✅ **已完成**（2026-09-03）

> 依据：迁移评估 §4.5 双通道共存、§5 权限梯度。
>
> ⚠️ **实际执行偏离计划的一处**：原计划「逐插件迁移，随时回退」。
> 用户决策改为**彻底舍弃 `.so` 能力，无回退通道**（不做 `--cabi` 开关），
> 本轮直接删 `internal/plugin/cabi/`，生产全量切换。代价是某插件出问题
> 只能紧急修复或 `git revert` 整批。因此下方【V】的「`.so` ↔ `.bin` 混跑」
> 不再适用——新内核根本不认 `.so`。

### 修改

- ✅【M】**6.1** 工具链 entry 语义收敛（SDK 仓 `9f84412`）：`isProcEntry` 删除，Go 插件一律产出 `plugin.bin` 不看 entry 值；`templates.go` 1296→516 行。
- ✅【M】**6.3** 17 插件全量重编（`1d7f011`）：16 个×3 平台 + qq×1；`git status example/` 无输出（业务代码零改动）。
- ✅【M】**6.5** 生产切换（`62bdfa2`）：经 `pluginmgr` 的 hmap 正规通道安装，17/17 成功且 `config_kept=true`。
- ✅【M】**6.6** 压测 + 版本 1.0.0 + 文档（`2572688`、`670efcd`、tag `v1.0.0`）。
- ✅【M】**6.2** 内核侧 Windows（`d027c96`）+ 删 C ABI（`b20121f`，-3198 行）：删 `internal/plugin/cabi/`(1156)、`dynamic_dll_windows.go`(272)、`dynamic_loader_unix.go`(79) + bridge 模板；新增 `shmalloc_windows.go` + `evtfd_windows.go` + `shmpass_{unix,windows}.go`；顺带修 macOS pipe 写端被 GC 回收的真 bug。
- ✅【M】**6.4** 权限梯度显式化（`2ebdb9a`）：54 个 method 划入 11 个 capability 组；`coreHandler.Handle` 入口强制；`withheldCapabilities` 表记录 10 项刻意不提供的内核机制及理由（`SelftestAPI`/`SupervisorAPI`/`TrackerAPI`/`StatusAPI`/`AdapterAPI`/`ConfigAPI`/`ToolAPI`/`IndexerAPI`/`OutputChanRaw`/`EventPublish`）。
- ⏭️【M】`lua_plugin.go`/`dynamic_lua.go` 统一走 RPC —— **留待后续**。Lua 走解释器不经 C ABI，不阻塞本轮目标（消除 C ABI 前提缺陷）。收敛第三套 ABI 是独立优化。
- ✅【M】文档：本文与 `plugin-interface-matrix.md` 更新；切换实录见下方。

### 审查

- ✅【R】每删一个 cabi 依赖项，`go build ./...` + `go vet ./...` 干净。
- ✅【R】权限梯度：被拒 API 在 RPC 边界返回**明确错误**（非忽略）。错误消息含四要素：哪个插件、哪个调用、缺什么能力、在哪声明。`TestCapability_DeniedErrorIsActionable` 守护。
- ✅【R】接口冻结：`git diff third_party/homeagent-sdk/sdk/` 全程为空。

### 验证（全量回归）

- ✅【V】17 插件经 `plugin_install(overwrite=true)` 加载，工具/设置/通道/阶段 e2e。
- ⏭️【V】~~`.so` ↔ `.bin` 混跑集群冒烟~~ —— 不适用（无回退通道，见上方偏离说明）。改为验证**新内核面对旧 `.so` 给可操作错误且不崩溃**，已在真实二进制上确认。
- ✅【V】`make test` 全量绿 + `go build ./...`。
- ⚠️【V】内存：**未达成计划目标**。15 个插件进程 RSS=88.0MB / PSS=87.9MB，远超「基线 +29MB」。根因是每插件静态链接整个 Go runtime，15 个不同二进制无共同物理页可映射（PSS/RSS 99.9% vs 基线 44%）。这是「每插件独立二进制」的固有代价，实际开销高于 §4.3 乐观估计。压缩方向：共享 launcher 二进制 + 各自业务模块。
- ✅【V】工具调用 RPC 延迟 24.1µs（实验 11 基线 19.6µs，同量级）。

**Part 6 出口条件**：全部外部插件 `.bin` 化 ✅，cabi 删除 ✅，接口零改动 ✅，权限显式化 ✅，无回归 ✅。

---

## 最终验收清单（对照接口不变矩阵 §7 检查点）

| # | 检查点 | 通过标准 | 结果 |
|---|---|---|---|
| 1 | 公开 SDK 接口冻结 | `git diff third_party/homeagent-sdk/sdk/` **为空**（全程） | ✅ 每次审查均确认 |
| 2 | 外部插件业务代码零改动 | 17 个 `example/*/plugin.go` 与基线逐字节可比 | ✅ `git status example/` 无输出 |
| 3 | 17 插件 `.bin` 化 | 全部经 `plugin_install` 加载，工具/设置/通道/阶段 e2e | ✅ 17/17，`config_kept=true` |
| 4 | cabi 删除 | `internal/plugin/cabi/` 与 bridge 模板不存在 | ✅ -3198 行（`b20121f`） |
| 5 | 崩溃隔离 | 插件 kill 只退出自身，homed 存活 | ✅ `TestRealPlugin_CrashDoesNotKillKernel` |
| 6 | 热重载 | 同路径换 `.bin` 即生效，无需重启 | ✅ 生产实测（`unloaded (config kept)` → 重载） |
| 7 | 并发改写 | 跨进程 stage 丢失率 0%（对照今天 35.8~36.8%） | ✅ `TestPlugin_FiveProcessesConcurrentAppendNoLostUpdate` |
| 8 | 事件订阅 | 外部插件 `Events().Subscribe` 可用 | ✅ 事件环已接线（当前零用户） |
| 9 | 多模态 | `SetToolBlocks` 非空实现 | ⚠️ method 已定义并划入 core 能力，内核侧仍返回未实现 |
| 10 | 超时取消 | 工具超时可 `Process.Kill()`，零泄漏 | ✅ 整套新架构零 cgo |
| 11 | output_send | 真实结果返回（非假成功） | ✅ 生产实测 `map[status:sent]` |
| 12 | 权限梯度 | 内部专属 API 在 RPC 边界拒绝 | ✅ 12 项测试（`2ebdb9a`） |
| 13 | 内存/延迟 | 常驻 +≤29MB，RPC p50 ≤20µs 量级 | ⚠️ 延迟 24.1µs 达标；内存 88MB **未达标** |

**两项未完全达标的说明**：

- **#9 SetToolBlocks**：`io.setToolBlocks` 已在 protocol 定义并划入 `CapCore`，
  但内核侧 handler 仍返回未实现。C ABI 时代它也是空实现（§1.4），
  故**不是回归**，但也没兑现 §3.8 的承诺。当前无插件使用。
- **#13 内存**：15 个进程 RSS=88.0MB，远超「基线 +29MB」。根因是每插件
  静态链接整个 Go runtime，15 个不同二进制无共同物理页（PSS/RSS 99.9%
  vs 基线 44%）。实验 5 的基线用的是 2.68MB 最小插件，而真实插件 3.1~14.8MB，
  绝对数字不可比。结构性指标（均摊线程 5.5 vs 4.9）同量级。

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

---

## Part 6.5 生产切换实录（2026-09-03）

### 执行顺序（先换二进制，再装包）

```
1. systemctl stop homeagent
2. 换 /usr/local/bin/homed
3. 起服务 —— 15 个 .so 插件报可操作错误被跳过，homed 与 16 个内置正常
4. 逐个 POST 装 17 个 hmap（overwrite=true）
5. 重启核对
```

**为何不能反过来**：若先装包，旧 homed 的 `StopAndUnload` 会停掉 qq
消息通道，而它又无法加载 `.bin`，会卡在「插件全挂」的状态。

第 3 步顺带在真实二进制上验证了 Part 6.2 的可操作错误：

```
[plugin] dynamic weather: plugin weather: 检测到旧 C ABI 产物（plugin.so/.dll/.dylib）。
外部插件已改为子进程模式，请用新版 plugindev 重编产出 plugin.bin（业务代码无需修改）
```

不崩溃，只跳过该插件。

### 走 hmap 正规通道，而非手工拷贝

第一版切换脚本是手工拷 `plugin.bin` + 手改 `plugin.json` 的 entry ——
那等于**重新实现了一遍 hmap 解包逻辑，且实现得更差**。漏掉的东西：

| | 手工拷贝 | hmap 正规通道 |
|---|---|---|
| `platforms` 字段 | 漏了 | 包内 manifest 本来就写对 |
| 平台二进制选择 | 硬编码 `_linux_amd64` | `platformBinary()` 按 runtime 选 |
| `overwrite` 语义 | 无 | `StopAndUnload` **保留配置表** |
| 失败回滚 | 无 | `os.Rename` 备份，解包失败自动恢复 |
| 校验 | 只查文件存在 | `validatePackage` 查 manifest + 各平台二进制齐全 |

配置保留那条尤其关键：生产 17 个插件都有配置（qq 账号、weather 默认城市、
browser profile 路径）。手工脚本恰好没碰配置表所以侥幸不丢，但那是运气不是设计。

最终实现：POST 到 `127.0.0.1:9876/plugins`，传 `{path, overwrite:true}`。
保留的一个设计是**先全部校验再动手**——任一插件缺 hmap 就整批中止，
因为新 homed 不认 `.so`，「一半装了一半没装」的中间态最难排查。

### 结果

```
17/17 成功，全部 config_kept=true
0 个残留 .so；17 个 plugin.bin 均有执行位
17 个 manifest 的 entry 均为 plugin.bin；无 .bak 残留
bundle 包正确挑了当前平台（weather 目录只留 8.7MB 的 linux/amd64 那份）
```

备份：`/home/newqqagent-migration-backup-20260902-214812`
（plugins 全目录 + homed.old + homeagent.service，162MB）。
**唯一回滚路径**是恢复该目录 + 回滚 homed 二进制。

### 生产端到端验证（真实 QQ 消息）

```
input from qq → response (83293ms, tools=[qq_get_message qq_get_history
                                          output_send__qq output_send__qq qq_mark_read])
```

逐环节：

- **输入**：qq 子进程收 webhook → 经 RPC 报给内核 → agent 主循环
- **工具调用**：5 次跨进程调用全部成功（内核反向调用进子进程执行）
- **stage 改写生效**（最关键的一条）：
  ```
  [sanitizer] cleanToolCallLeakage: 2 bytes removed
  [sanitizer] cleaned 2 bytes (before=13590 after=13588)
  [proc] sanitizer stage post_action 改写了 1 个字段
  ```
  sanitizer 在**另一个进程里**改了 StageContext，内核读到了改写结果。
  13590 字节文本经共享段传递、被改写、写回，全程未拷贝整个上下文。
- **输出真的送达**：`tool output_send__qq result: 已通过 [qq] 通道发送: map[status:sent]`
  —— 直接验证 Part 0.1 修的 output_send 假成功缺陷（§9.4）
- **arena 生命周期正常**：每次 stage 结束都压实回收（单次最高 15802 字节），无泄漏累积

这一次对话触发约 20 次 stage、5 次工具调用、2 次输出发送，跨越 15 个插件子进程。
旧架构下同样流程有三处会静默出问题：stage 并发写丢字段（§8.4 实测 35.8~36.8%
lost update）、output_send 假成功、cgo 超时泄漏 goroutine。现在这些在日志里可见且正确。

---

## Part 6.6 压测与延迟实测

基准与压测在代码里（`internal/plugin/proc/bench_test.go` + `streaming_test.go`），
非独立脚本——随代码演进自动跑，不会腐坏。

| 项目 | 实测 | 基线 | 判断 |
|---|---|---|---|
| 工具调用 RPC 往返 | 24.1 µs | 实验 11: 19.6 µs | 同量级 |
| 锁仲裁（内核侧） | 0.76 µs | — | 见下注 |
| 事件环写入 | 95 ns | — | 亚微秒 |
| 事件环并发写入 | 83 ns | — | 无锁竞争恶化 |
| 完整 stage 往返 | 132 µs | — | 含 3 次进程间往返 |
| 共享段编解码 | 3.7 µs | — | 占 stage 的 2.8% |

**锁仲裁 0.76µs 不可与实验 3 的 19.40µs 对照**——测的不是同一个东西：
实验 3 测插件经 RPC 请求锁的完整跨进程往返，本基准只测内核侧
`lockRegistry.acquire/release`。真实成本仍在 20µs 量级。基准原名
`BenchmarkStageLockRoundTrip` 有误导性，已改为 `BenchmarkStageLockArbitration`。

**stage 往返 132µs 的成本构成**：共享段编解码只占 3.7µs，其余是
**一次 stage 要走 3 次进程间往返**（`stage.invoke` + 插件侧反向的
`stage.lock` / `stage.unlock`）。相对 LLM 往返 2-8 秒可忽略；
要优化的方向是把 lock/unlock 合入 `stage.invoke` 的请求/应答。

### 流式压测（§4.3 标记「风险高」的那一项）

```
5000 次 Publish + 每条睡 20µs 的慢消费者
  实测 2.29ms，均摊 457 ns/token
  同步语义理论下限 100ms

订阅者 1 个：1.547ms（515 ns/次）
订阅者 8 个：1.518ms（506 ns/次）   ← 无线性恶化

环溢出（无消费者写 30000 次，cap=8192）：均摊 35 ns/次   ← 仍 O(1)
```

2.29ms 与实验 4 的数字完全一致（那次也是 2.29ms / 0.46µs per token），
post-and-forget 在实现中成立。第三项的意义：消费者完全停摆时写端覆盖
最旧 slot，这条路径仍是 O(1)，故「插件卡住」不会连带拖慢内核主循环。

---

## 版本号

v1.0.0（tag 已打）。公开 SDK 接口零改动，但产物形态从 `plugin.so` 变为
`plugin.bin`，0.9.x 内核不会识别——不可互操作的破坏性变化，故跃主版本号。

⚠️ **Makefile 陷阱**：`VERSION ?= $(shell git describe --tags --dirty)`
意味着实际注入值来自 git tag，`meta.go` 里的默认值只在不带 ldflags 时生效。
打 tag 前 `make build` 注入的是 `v0.9.1-56-g2572688-dirty`。

同时删掉 C ABI 时代的死常量（`ABIVersion`/`CABINum`/51 个 `Core<Method>`
整数 ID）——随 Part 6.2 删 `internal/plugin/cabi/` 就已无使用者，
留着会让人以为 C 层协商还在生效，或以为加 method 要同步维护那张整数表。
