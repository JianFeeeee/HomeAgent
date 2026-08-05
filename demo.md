# HomeAgent 自愈 / Failback 架构设计与讨论全程记录 (demo.md)

> 本文档按讨论演进顺序记录"守护 / 保活 / 文件追踪 / 崩溃自愈 / failback"整个设计过程，
> 含代码勘查结论、现实日志记录模式分析，以及最终定稿的架构与尚未落地的接口清单。

---

## 0. 背景与目标

框架目标：**内核零 IO、插件承载所有 IO**（`homed 内核 ← PluginSDK → 插件`），三层记忆 + 常用块。

**Failback 的定位：所有错误的一层兜底（Safe-Mode 式），而非针对单一场景。**

- 主 agent（全量 LLM agent）是一切骚操作的执行者，可能把自己搞到无法自愈的任意状态：
  LLM 源改坏 / 系统网络（proxy、DNS、host）破坏 / 配置文件损坏 / OOM / panic / 崩溃循环……
  这些错误无法在**同一个被污染环境内**用自身操作自救。
- 故需要一层**脱离主 agent 坏环境的、最小厚度且独立可控的恢复层**——
  类似 Windows **安全模式 / 启动修复**：只带最小驱动集合 + 干净 LLM 源（锚定 IP），
  单一职责：**让主 agent 回到可用状态；若不可行，则做最后的系统级兜底（回滚快照 / 重启）。**
- 它不替代主 agent 的功能，只在主 agent 无法自愈时作为最后一道防线出现。能省则省、能判定就不推理、有底即主张。

---

## 1. 现状盘点（代码勘查结论）

### 1.1 守护进程（`internal/supervisor/daemon.go`）

- 主 agent in-process 常驻，`agent.Start()` 在 `cmd/homed/main.go:468` 直接启动，**无独立进程边界**。
- `healthLoop` → `checkAgent` 判 LLM 是否可达，判定**仅依赖 `network.Monitor` 的 `AggregateResult().LLMAPIReachable`**（`daemon.go:132`）。
- `failCount >= MaxRetries`（默认 3）→ `handleFailure`：
  - 有 tracker → `trk.Rollback()`；失败才降级 `restartAgent`。
  - `restartAgent`（`daemon.go:166`）**只改内存状态再重新 Register，不真重启任何进程**，几乎空转。

关键问题：
- 探测是系统级（HTTP/DNS/TCP），回滚只作用于 `<data>/agentfs` overlay 的 upper，**二者对象错位**。
- `AgregateResult` 在无 LLM endpoint 时恒 healthy，机制形同虚设。
- `lastHB` 每轮都置 `time.Now()`，`Uptime` 无意义。
- `RollbackPolicy` 的 `HealthThreshold/CooldownPeriod/AutoRollback` 都是死字段，只用 `MaxRetries`。

### 1.2 文件追踪（`internal/tracker`）

- overlayfs 三层：`lower/upper/work → merged`（`tracker.go:156`）。
- `captureFSState(upperDir)` 递归遍历 upper 并 sha256（`changeset.go:56`）；`PreAction/PostAction` 前后 diff（`toolcall.go:86-94`）。
- **lower 恒空**（`Init` 只 `MkdirAll`，从不填充）→ 无 canonical 基线可回滚。
- `Rollback()` = `RemoveAll(upper)` 清空全部 changesets；`FileChange.Content`（本应存回滚原文）**从未回填**。

结论：overlay/tracker 对"LLM 可达性"这主场景**错位**，只能作数据兜底。

### 1.3 通信插件真相（`third_party/homeagent-sdk/example/qq/plugin.go`）

- agent 对外通信全部由**插件设置**驱动，存于 **ConfigRegistry / SQLite config.db**：
  `qq.napcat_url`、`qq.listen`、`qq.files_dir`、`qq.remote_dir`、`dm/group_policy`（`plugin.go:120-130`）。
- 插件在 `Start()` 里 `getSetting(...)` 读设置（`plugin.go:134-143`）→ **改动配置需重载插件才生效**。
- `cfgmgr` 提供 `config_set / config_batch_set` 可运行时改任意 core/插件配置（`cfgmgr/plugin.go:54,103`）。

### 1.4 LLM 源与"恢复即生效"

`internal/sdk/llm_impl.go:103 ReloadFromConfig()` **已存在**：
- `cfg := cfgReg.ToConfig()` 从 config.db 重建（含 `core.llm.sources.*`，见 `registry.go:590`）
- `mgr.Reset()` → 逐源 `NewLuaAdaptedProvider` → 重设默认。

即：**LLM 源的"恢复即生效"钩子已经具备**，缺的是"快照 + 探测 + 触发"三件事。

---

## 2. 现实环境：日志记录模式内参

> 看真实 systemd 托管的 HomeAgent（`/home/newqqagent`）日志，**目的是弄清现有的日志模型**
> （写哪、什么格式、工具调用打在哪），为 failback / recoveryDiag 的 `diag_log_scan` 提供准确的解析依据。

### 2.1 systemd 托管现状（样例）

```
homeagent.service: Type=simple, ExecStart=/usr/local/bin/homed -data /home/newqqagent, Restart=always, RestartSec=10
llm-mock.service:   ExecStart=/usr/bin/python3 /opt/llm-mock/mock_server.py, Restart=always, RestartSec=3
```

- 实测数据区：`/home/newqqagent/` 下有 `log/`、`config.db`、`agentfs/`（overlay merged）、`snapshots/`、`changesets/`、`knowledge/`、`memory/`、`plugins/`、
  `cli.sock`、`adapters/`、`homed.log`、`memos.json` 等——**日志以独立子目录 `log/` 存放，与配置/快照/knowledge 分置**。
- 启动段确认：`[files] started, sandbox: /`（**files 沙箱=全主机 `/` 实锤**）；`main agent started, model=mock-model base=http://127.0.0.1:18080/v1 sources=3 adapters=8`（LLM 走本地 mock）。

### 2.2 日志目录与格式（核心）

- **目录配置**：`core.log.path`，默认 `<dataDir>/log`（`internal/config/registry.go:415,508`）。
- **单次运行文件**：每次启动新建 `homed_<YYYY-MM-DD_HH-MM-SS>.log`（`cmd/homed/main.go:75`），
  写入 `logDir` 下；`log.SetOutput(io.MultiWriter(os.Stderr, logFile))`（`main.go:80`）——
  **同时进 stderr（systemd 捕获到 journald/`journalctl -u`）与文件**。
- **格式**：标准 Go `log.Printf`，即 `YYYY/MM/DD HH:MM:SS file.go:line: [module] message`。
  用户可看文件，也可用 `journalctl -u homeagent.service` 看同一来源（同一行）。
- **层级压缩 + 保留**（`internal/log/manager.go:26-28` + `compressor.go`）：
  - 周度压缩 → `week_<year>-W<ww>.tar.gz`；月度 → `month_<yyyy-mm>.tar.gz`；年度 `year_*.tar.gz`;
    raw 文件正则 `^homed_(\d{4}-\d{2}-\d{2})_\d{2}-\d{2}-\d{2}\.log$`（`compressor.go:16`）。
  - 保留策略：`core.log.retention`（default forever）、`core.log.retention_months`（default 3），
    `applyRetention` 只留当前周 + 近 N 月（`retention.go`）。
  实测：`log/` 下即为 `homed_2026-08-03_08-03-38.log` + `month_2026-*.tar.gz` + `week_2026-W31.tar.gz`，与代码一致。

### 2.3 工具调用日志打在哪儿（进程主循环 `internal/agent/core/process.go`）

| 位置 | 日志行内容 | 备注 |
|---|---|---|
| `process.go:36` | `[agent] tool call loop start, max_ctx=… target=… fixed=… mem=… ctx=… N tools, M events, personality=X, docs=K` | 每轮循环开头上下文统计 |
| `process.go:196` | `[agent] executing tool: <name> (plugin=<p>, id=<id>)` | **只记工具名/插件/id，不记 args** |
| `process.go:226` | `[agent] tool <name> result: <截断100字符>` | 结果截断到 100 字符（`truncateStr`）|
| `process.go:218` | `[agent] skip tool <name>: plugin <name> unhealthy` | 插件崩溃态跳过 |
| `process.go:89/109/121/125` | LLM fallback：`trying provider %q (#%d)` / `switched active provider` / `provider %q marked unavailable (HTTP %d)` / `provider %q failed` | 主循环内 LLM 商可观测 |
| `toolcall.go:20` | `[agent] tool %s panic: %v` + `debug.Stack()` | 工具 panic + 完整栈 |
| `toolcall.go:41` | `[agent] tool %s timed out after 60s` | 60s 超时 |
| `toolcall.go:92` | `[agent] tool %s changed %d files (changeset: %s)` | overlay changeset 摘要 |
| 插件侧 | Lua 插件 `sdk.log` → `print("[lua-plugin] <level>: <msg>")` | 模板见 `cmd_debug.go:74`/`templates.go` |

- 完整的工具**入参/结果**在 EventBus 事件 `EventToolCall`（`{tool, plugin, args, result, status}`，`process.go:184/205`）而非文件日志——**文件日志只是执行/结果的摘要指针**（结果被截断）。
- 重要观察：日志里未见 shell/cmd 之类的操作系统执行类调用摘要落盘（`[cmd]` 只在工具结果里），
  需要的话由 `diag_log_scan` 对 `executing tool: cmd_*` 前缀做签名匹配即可。

### 2.4 崩溃 / 重启观察

- `NRestarts=0`；MainPID 自 08-03 起稳定 3330844。曾出现**真实重复 panic**（pid 3310036）：
  `[stage] handler panic: runtime error: invalid memory address or nil pointer dereference`（03:23 / 05:23 / 07:23，约每 2h），
  被 `stages.go:139` 的 `RunStage` recover 吞掉 → **进程未真崩**，systemd 未见重启。
- 08:03:38 有过一次干净 `[homed] stopped` → systemd `Started` → pid 3310036 → 3330844。
- 对 failback 的意义：现有崩溃防护全赖 **in-process recover**，真实进程级崩溃从未被监督；
  且当前 `Restart=always` 由 systemd **直绑 worker 且无 StartLimit**——一旦真崩并陷入循环，
  systemd 每 10s 反复拉起，没有独立 failback/取证层。→ guard 取代点在此。

---

## 3. 设计演进（讨论全过程）

### 3.0 起点：`internal/supervisor` + `internal/tracker` 我是"保活 + 文件追踪"

- 保活 = 网络健康感知 + 内存态重置；追踪 = overlayfs 变更集 + `Rollback` 清空。
- 意图：LLM 不可达 → 回滚 agent 文件改动 → 自愈。

### 3.1 第一次纠正：回滚对象错位

- 回滚只作用于 overlayfs upper；而真正能改坏网络的路径（files 默认 `/`，可写 `/etc/resolv.conf`、`/etc/hosts`、代理配置）与 LLM 配置（config.db、adapters 目录）**都不在 overlay 内**。
- → 检测命中但回滚删错对象，闭环在"回滚"这一环断掉。

### 3.2 第二次纠正：授权不能靠外部插件主动配合

- 阶段管道是被动通知（`RunStage` 收集 error，不主动拒绝），拒绝依赖各插件 handler。
- 插件是外部不可控对象 → "安全 = 插件主动授权放行"不成立。
- → 安全应做**默认拒绝**，由内核在 `executeToolCall` 分发点按 ToolDef 的 capability 裁决；配置快照 + replug 只兜底"可文件化"改动。
- 中途又修正：真正的祸首不是 IO 组件，而是 LLM 源/系统网络配置；IO 组件崩溃是正交偶发轴，不并进来。

### 3.3 第三次纠正：LLM 源才是核心（config.db + ReloadFromConfig 已具钩子）

- `llm_set_source` 只切内存默认；真正改坏 LLM 源靠 `cfgmgr.config_set` 写 config.db `core.llm.sources.*`。
- `ReloadFromConfig()` 已能"恢复即生效" → 只需补：**LLM 配置快照 + 真实 liveness 探测（QuickChat）+ 在 handleFailure 里触发 RFO 复检**。
- 但用户进一步点明：**重点是 agent 改了系统网络配置文件（proxy/DNS）** —— 这类连救援 LLM 都连不上。

### 3.4 定稿架构一：lastFailback（独立进程，第一道防线）

- 独立进程 + agent 碰不到的 root:0600 配置 + 最小插件集（文件读写 + cmd）+ 单一任务。
- 用**锚定 IP/干净 DNS** 的 LLM 源绕开坏掉的 DNS/proxy。
- 若 failback 也失败 → agentfs 回滚最近快照 + 重启计算机（应用快照前系统配置）。
- 兼顾 systemd 托管冲突（主 agent 崩溃循环被 systemd 反复拉起 → failback 无限增殖）。

### 3.5 定稿架构二：去掉强 systemd 依赖 → 独立 guard 守护进程

- systemd 只绑定**静态、极小、永驻的 guard**；guard 作为父进程拉起 worker（`homed` 全量 agent）。
- guard 通过 IPC（unix socket / 匿名 pipe）收 worker 心跳；
  父进程 `waitpid` 天然 reap 崩溃/OOM/信号退出。
- worker 可崩可换，systemd 完全看不见 → **failback 唯一性 + 无风暴天然成立**。
- guard 抗 OOM：`oom_score_adj` 拉低，优先杀 worker 而非 guard。

### 3.6 定稿架构三：更纯粹 —— 同一二进制的两个 role，非独立二进制

- **不新建二进制**。`homed` 拆两个入口：
  - `homed --role=guard`：父守护进程，先起，负责拉起/监测/探活/裁决/恢复。
  - `homed --role=agent`：主 agent（工作进程，guard 的子进程）。
  - `homed --role=agent --boot=failback`：恢复用 agent（受限 bootstrap）。
- guard 复用现有 homeagent 基础设置；检测到崩溃时拉起 failback agent，只加载
  **webfetch + 文件读写 + cmd** 三个插件，外加 **恢复知识库插件** 与 **常见错误检测插件**，
  复用 agent 核心以 `trigger_prompt` 初始化，要求其"尝试恢复主 agent"。
- guard 配置**独立 YAML**，不复用 config.db（逃生舱知识必须脱离 agent 可达区）。

### 3.7 recoveryDiag：崩溃取证 / 根因定位插件（省 token 关键）

- 铁律：**工具返回结论，不返回原文**（签名式/统计式/确定性排序）。
- 工具集：
  | 工具 | 作用 |
  |---|---|
  | `diag_triage` | exit code/信号+uptime+头部嫌疑，快速粗分"进程死亡 vs 配置类不可达" |
  | `diag_db` | config.db integrity_check + LLM 源解析校验，逐项 ok/fail |
  | `diag_log_scan` | 时间窗内命中已知错误签名（panic/provider failed/unreachable/sql/OOM）|
  | `diag_delta` | 崩溃前 config/agentfs 与 last-good 快照 diff（"改了什么"）|
  | `diag_loc` | 综合正交，输出按因果强度排序的定位结论 + 推荐动作 |

- 崩溃类别 → 恢复分支决策表：
  | 结论类 | 走分支 |
  |---|---|
  | 配置损坏类 | 还原 config 快照 + ReloadFromConfig + 拉活主 agent（无需 agent 推理）|
  | 系统网络类 | 还原 DNS/proxy → 重载主 agent（第一步小修命中即停）|
  | 进程失稳类（OOM/panic）| 不还原配置，检查内存/泄漏 → 重建 worker |
  | 未知/混合 | 放开 webfetch/知识库，用 rescue 源 + diag_loc 摘要最小推理 |

- 只有"未知/混合"消耗 token，前几类近乎 0 token。
- 结论落盘 `recovery_kb/diag_<ts>.json`，回流知识库，同类崩溃下次直接命中，越用越省。

### 3.8 三层防御总览（最终）

```
L0 平时：核心只读探活 + 写前快照（config_set 写 core.llm.* 前、files 写 /etc 前自动留档）
L1 failback（guard 拉起，Safe-Mode 式兜底）：按诊断分支逐类恢复——
   还原 DNS/proxy → 还原 config 快照 + ReloadFromConfig → QuickChat 复检 → 拉起主 agent
L2 最后手段：agentfs 回滚最近快照 + 重启（应用快照前系统配置）
```

- failback 是**所有错误（LLM 不可达 / 网络 / 配置损坏 / OOM / panic / 崩溃循环）的统一兜底层**，
  并非只针对某一条；`diag_*` 决定它走哪条恢复路径。

---

## 4. 最终架构（定稿）

### 4.1 进程拓扑（同一二进制，两个 role）

```
systemd ──▶ homed --role=guard          # 父守护进程，永驻、静态、极小
             ├─ exec ──▶ homed --role=agent              # 主 agent（可崩）
             └─ exec ──▶ homed --role=agent --boot=failback  # 恢复用 agent
```

- guard：先起，持有恢复知识（锚定源 / DNS/proxy 还原 / 配置快照 / failback 逻辑）。
- worker：guard 子进程，心跳经 IPC，崩溃由 guard reap + 判型。
- failback agent = 受限启动（webfetch+files+cmd + 恢复知识库 + recoveryDiag），单一任务"恢复主 agent"，N 轮有界。

### 4.2 guard 独立 YAML 示例

> 现状实现（§5 已完成）：`guard.yaml` 已落地为 `max_restarts / heartbeat_timeout / heartbeat_interval / llm_snapshot / failback_enabled / last_resort / restart_command / reboot_grace` 子集（`cmd/homed/guard.go`），恢复梯子=重试→LLM 基线恢复→failback 受限启动→last_resort。下表的 rescue 源 / trigger_prompt / N 轮 failback 推理是目标态，未实现。

```yaml
role: guard

llm:
  sources:
    - name: rescue
      base_url: http://1.2.3.4:8080      # 锚定 IP 直连，绕开被破坏的 DNS/代理
      api_key: ${GUARD_RESCUE_KEY}
  adapter: ...                            # 锚定/SNI 型适配器

recovery:
  max_attempts: 4                  # 可配置尝试轮次
  attempt_timeout: 120s
  knowledge_base: /opt/homeagent/recovery/
  trigger_prompt: "你是恢复 agent，唯一任务：让主 agent 恢复运行。优先还原 DNS/代理，再重载 LLM 源…"
  plugins: [webfetch, files, cmd]

last_resort:
  action: reboot                   # restart_app | reboot
  snapshot_before: true
```

### 4.3 guard 恢复状态机（N 轮有界）

```
guard 检测( exit≠0 | OOM | 心跳超时 | guard 锚定源探活失败 )
  1. 固化追溯：exit/信号、panic、journal、OOM 上下文 → 永久区
  2. 拉起 failback agent（受限插件 + rescue 源 + trigger_prompt + 知识库 + recoveryDiag）
     for attempt in 1..N:
         (可选先 diag_triage/diag_loc 判型)
         failback 尝试恢复
         guard 每轮复检主 agent 是否可达/存活
         ├─ 成功 → 结束，交回主 agent
         └─ 超时/失败 → kill 重建，进入下一轮
  3. N 轮未成 → 取消 failback agent
        → agentfs 回滚崩溃前最近快照
        → 依 yaml 执行最后手段：restart_app 或 reboot
```

### 4.4 systemd 绑定（极简，杜绝风暴）

```
[Unit]  # guard
OnFailure=...            # 备用，通常不触发（guard 稳定）

[Service]                # guard
Restart=always           # guard 静态稳定 → 几乎不重启
ExecStart=/usr/local/bin/homed --role=guard ...
# No StartLimit needed for loop 情况；guard 不崩
```

- 主 agent 崩 → 只触发 guard 内部 failback；systemd 仅看 guard，看不到 worker 崩溃循环。
- failback 唯一性 + 无启动风暴：由"guard 永驻、唯一裁决"天然保证。
- guard 抗 OOM：`oom_score_adj` 拉低。

---

## 5. 尚未落地的接口 / 下一步

**已完成**：

`recoverydiag` 快速检查插件（`third_party/homeagent-sdk/example/recoverydiag/`，外部插件）。
- 五件套全实现：`diag_triage`（退出码/信号/存活粗分）、`diag_db`（config.db integrity_check + LLM 源字段校验，sqlite3 CLI 优先、缺失回退内核 Settings）、`diag_log_scan`（日志签名按类计数）、`diag_delta`（baseline vs 现状 diff）、`diag_loc`（四项结论正交排序 + 推荐恢复动作）。
- 全部确定性、返回结论非原文、`NoMemory`；工具实际名带插件前缀 `recoverydiag_diag_*`。
- 已通过 go vet + 6 个单测（对真实 config.db/日志跑通：3 个 LLM 源全 ok、日志命中 228 行主导 provider/fatal），并用**仓库内重建的 plugindev** 打出 `dist/recovery_diagnostics_linux_amd64.hmap`，装进运行实例（`/home/newqqagent/plugins/recoverydiag/`）加载成功、注册 5 工具。
- **结论落盘 + 知识库回流**：`diag_loc` 增 `persist`（缺省 true）→ 写 `<data_dir>/recovery_kb/diag_<ts>.json`（可配 `recovery_kb_dir`），并经 `sdk.Knowledge().Add` 以 `diag:<cause>:<ts>` 回流知识库（同类崩溃下次直接命中，越用越省）；失败不阻塞工具。新增 `TestDiagLocPersist`。
- 顺带修复：仓库内 `plugindev` 需重编译（`/usr/local/bin/plugindev` 是旧版、桥模板缺 `InjectInputSync`）；重编译见 `third_party/homeagent-sdk/tools/plugindev`，`go build -o ... .`。
- 注意：本环境 `snapshots/`、`changesets/` 均为空（direct 模式无基线）→ `diag_delta` 需显式传入 baseline_dir；未来接 guard 时由快照解包目录提供。

`ConfigRegistry` 快照钩子（`internal/config/registry.go`）。
- `SnapshotCoreLLM()`：抓全部 `core.llm.*` 键值快照；`RestoreCoreLLM(snap)`：精确还原（快照内键回写、快照外当前键删除）。
- `SetLLMSnapshotFile(path)`：写前自动留档——此后任意写 `core.llm.*` 键先把当前 LLM 配置整体快照到该文件（guard 恢复的外部基线）；homed 启动即挂 `<data>/llm_snapshot.json`。
- 文件持久化对：`SaveLLMSnapshot/LoadLLMSnapshot`。新增 `TestSnapshotRestoreCoreLLM`、`TestLLMSnapshotFile`、`TestSetLLMSnapshotFile`。

`homed --role{guard,agent}` 入口拆分 + `--boot=failback` 受限插件集（`cmd/homed/`）。
- `--role=guard` 父守护（永驻）：读独立 `<data>/guard.yaml`（避开被改坏的 config.db），拉起 worker（`--role=agent`）、心跳探活 + waitpid 收割、信号转发停机。
- `--role=agent` 工作进程：默认启动全插件；`--boot=failback` 走插件白名单（`core.agent.failback_plugins`，缺省 `webui,pluginmgr,recoverydiag`），内核 webfetch/files/cmd 仍内置可用。
- guard 恢复梯子（已端到端实测）：连续 `max_restarts` 次 normal 崩溃 → `restoreLLMBaseline`（从 llm_snapshot.json 恢复 core.llm.*）→ failback 受限启动 → failback 也崩 → `last_resort`（restart_app / reboot）。
- 心跳：worker 每 5s 触碰 `<data>/heartbeat`（agent 角色 goroutine），guard 以 mtime 判定卡死（超 `heartbeat_timeout` 即 SIGKILL 计入崩溃）。
- 插件注册表加 `SetLoadAllowlist(names)`：白名单外插件（含已注册工厂）一律跳过，failback 40 工具 → 4 工具实测通过。

现状 bug 修复（supervisor/network/tracker）。

- **monitor 无 endpoint 恒 healthy**（`internal/network/monitor.go` + `pkg/types`）：`NetworkCheckResult` 增 `EndpointsConfigured`；无探活端点时不再谎报 `LLMAPIReachable=true`（置 false + Error），`NewMonitor` 初始化空切片消除启动竞态；daemon 仅在配置了端点时才据此判定降级。探活端点新增 `core.defaults.llm_endpoints`（逗号分隔，留空自动取 LLM 源 base_url），生产从此健康检查有真实目标。
- **lastHB 恒置 now**（`internal/supervisor/daemon.go`）：`checkAgent` 接入真实存活源 `SetHeartbeatSource`（homed 注册为 agent core `GetKernelStatus`），只在确认 agent 存活时更新 `lastHB`；无源置 `HealthUnknown`，存活源丢失置 `HealthDown` 且不再刷新 lastHB。
- **restartAgent 只改内存空转**：增 `SetRestartHandler`（homed 注册为"清理后以 `exitRestartRequested=42` 退出"），不再假装成功；guard 把 42 识别为"请求重建"（`workerRestartRequested`，不计失败轮次直接重建），无 guard 时 systemd `Restart=always` 兜底。无 handler 时仅内存复位并打日志。
- **tracker 三缺陷**（`internal/tracker/`）：`captureFSStateWithContent` 为 before 基线捕获原文（上限 8MB）→ `diffStates` 对 modified/deleted 回填 `FileChange.Content`（回滚用原文）；新增 `RollbackLatest()` 定向撤销最近一条 changeset；`Rollback()` 改为按时间逆序逐条逆应用（还原被改/被删文件原文、删除新增），无 changeset 时才退回整目录重置。新增 6 个测试覆盖。

剩余：

- guard ↔ agent 心跳 IPC 升级为带自诊断上报的 `PING/ACK`（当前为文件心跳 + 退出码）。
- supervisor 适配成 guard 的探测/裁决逻辑；`ReloadFromConfig()` 复用为"恢复即生效"。
- 生产实例迁移：编译新 homed、改 systemd 只托管 guard（`--role=guard`），确认 failback 插件（recoverydiag）就位。

---

## 附录：真实日志节选（systemd 托管示例，`/home/newqqagent`）

```
# systemd unit
homeagent.service: Type=simple, ExecStart=/usr/local/bin/homed -data /home/newqqagent, Restart=always, RestartSec=10
llm-mock.service:   ExecStart=/usr/bin/python3 /opt/llm-mock/mock_server.py, Restart=always, RestartSec=3

# 日志文件与格式（log.Printf 标准格式）
2026/08/03 08:03:38 main.go:80:  [homed] logging to /home/newqqagent/log/homed_2026-08-03_08-03-38.log
2026/08/03 08:03:38 daemon.go:61: [homed] daemon started successfully
2026/08/03 08:03:39 tracker.go:65: [tracker] initialized (work=/home/newqqagent/agentfs)

# 工具调用摘要（process.go）
2026/08/03 10:03:52 process.go:36:  [agent] tool call loop start, max_ctx=32768 target=26214 fixed=1306 mem=8302 ctx=16606 176 tools, 31 events, personality=true, docs=5589
... process.go:196: [agent] executing tool: <name> (plugin=<p>, id=<id>)
... process.go:226: [agent] tool <name> result: <截断100字符>

# 启动 & 沙箱
homed[3330844]: [files] started, sandbox: /
homed[3330844]: main agent started, model=mock-model base=http://127.0.0.1:18080/v1 sources=3 adapters=8

# 重复 in-process panic（被 RunStage recover 吞掉，未进程级崩溃）
homed[3310036]: [stage] handler panic: runtime error: invalid memory address or nil pointer dereference   # 03:23 / 05:23 / 07:23

# 一次性干净重启（systemd 手动/触发 Started，pid 3310036 → 3330844）
homed[3310036]: [homed] stopped
systemd[1]: Stopped homeagent.service - HomeAgent - 24/7 AI Butler.
systemd[1]: Started  homeagent.service - HomeAgent - 24/7 AI Butler.

# 运行状态
systemctl show homeagent.service -p NRestarts   → 0
systemctl show homeagent.service -p MainPID     → 3330844（自 08-03 起稳定）

# 归档
/home/newqqagent/log/: homed_2026-08-03_08-03-38.log + week_2026-W31.tar.gz + month_2026-*.tar.gz
```