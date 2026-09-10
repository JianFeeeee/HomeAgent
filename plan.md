# HomeAgent 生产问题修复计划

## 设计意图备忘（核心架构原则）

本框架的两大核心设计意图，贯穿所有插件/记忆/工具设计，**所有改动必须符合**：

1. **插件即 Agent 的 App** —— Agent 像人用 App 一样用插件。
   - QQ 插件应像 QQ 客户端：通知到来 → 看到预览/上下文 → 一键回复。
   - **认知负荷最小**：中断/通知只给「发送者昵称 + 消息预览」，**元数据（user_id/group_id/message_id）完全走工具不进 prompt**，防提示词注入、防昵称欺诈、低认知负荷。
   - **工具语义自解释**：`qq_get_message`、`output_send__qq`、`qq_get_history` 的 Description 要让 Agent「读完即知怎么用」，无需额外指令。

2. **基于相关性的分层记忆架构** —— L1 文本流 / L2 图谱 / L3 文档向量，按相关性蒸馏、检索、归档。
   - GraphDB 去重、docToTriples 模板清理、Pipeline 增量蒸馏、嵌入模型内存优化，均服务于此。

---

## 0.1 紧急：healthcheck 健康检查污染真实存储 ⚠️ 正在持续污染

**现象**（2026-08-11 22:02 起，每 30 分钟一次）：日志反复出现
`[knowledge] added: _hc_knowledge_test_<ts>`，且知识库出现 `gotest`、`luatest`、`_hc_knowledge_test_*` 等测试残留。

**根因**：`internal/plugins/healthcheck/plugin.go` 的三个"写入通道"自检**全部在真实生产存储上写入再删除**：

| 函数 | 写入 | 清理 | 固化问题 |
| ------ | ------ | ------ | ---------- |
| `testMemoryRaw` (:460) | `Memory().Commit(_hc_<ts> triple)` | `Purge(hard)` | GraphDB 空实体/AUTOINCREMENT id 膨胀 |
| `testKnowledgeRaw` (:485) | `Knowledge().Add(_hc_knowledge_test_<ts>)` | `Remove(marker)` | Add 异步 `go writeIndex` vs Remove 异步重写**竞态**；`dirName`(用了 `/` 解析)与 `Remove` 的 `id=sanitize(name)` 计算**不一致**→ 删除可能失效 → **残留目录固化成文件** |
| `testDocStoreRaw` (:521) | `DocMemory().Insert(...)` | Query 后 Remove | 真实 docStore 写文件再删，抖动 |

**原则**：健康检查验证的是"写入通道是否可用"，**结果不应固化进生产记忆**。改为**独立虚拟/影子空间**或**不落盘验证**。

**实现**：

- [x] **核心实现**：SDK 新增 `VirtualInstance`（`internal/sdk/selftest.go`），healthcheck 的三个 raw 自检改为在完全隔离的虚拟空间（`os.MkdirTemp` 独立图库/知识库/文档/文本）上做真实"写→查→删"，绝不碰生产存储。
- [x] `PluginSDK.Selftest()/SelftestReset()` 暴露隔离实例（含 mutex 防并发），每轮自检前 `SelftestReset` 重建清空上轮数据。
- [x] **LLM 驱动自检防护**：`collectToolDefsForLLM` 改为只收集**只读白名单**工具（`isSafeReadonlyTool`），写/删/改生产数据及外部副作用工具（memory_commit/doc_commit/knowledge_create/cmd_run/files_write/terminal_*/output_send/spawn_child 等）一律不交给 LLM 自检，防止 LLM 乱调污染生产。
- [x] 单测：healthcheck 自检后注入的"生产"实例内容不变（快照对比 + `_hc_` 无残留）；读/写工具白名单过滤测试通过。
- [x] 存量清理：删除生产残留的 `gotest/`、`luatest/`、`_hc_knowledge_test_*/` 目录（保留真实知识库）；备份留于 `/tmp/opencode/knowledge_garbage_backup_20260812_122322`。
- [x] 代码复核：healthcheck 三自检（memory/knowledge/doc）全部经 `s.Selftest("hc")` 隔离虚拟实例写→查→删，生产实例零接触（plugin.go:339/466-473/476-549）。
- [ ] 部署验证：编译新 `homed` 部署后，`knowledge/`、`memory/graph.db`、`memory/documents/` 不再出现 `_hc_*` 残留。—— **验证脚本已就绪：`scripts/verify_deploy.sh [data_dir]`，部署后一键检查 0.1 残留 / 1 去重与 UNIQUE 迁移 / 2 archived 残留 / 5 嵌入规格**

---

## 0.2 紧急：QQ 消息被无视（agentcli 幽灵终端自喂送风暴）⚠️ 优先处理

**现象**（2026-08-11 20:4x）：用户发 QQ 私聊消息，agent 不回应。日志显示 agent 被 `agentcli` 终端 echo 洪水完全阻塞。

**根因（两个耦合缺陷，非记忆层）**：

1. **`agentcli` 幽灵终端自喂送风暴（直接原因）**
   - `internal/plugins/agentcli/plugin.go:602` — 终端一旦有输出就 `s.InjectText("agentcli","agentcli","[终端 X 有新输出]\n...")` 注入 agent 事件循环。
   - 残留 `term_3`（bash 的 `git sparse clone` 进度）持续吐数据 → 插件每 `NotifyOutputDelay`（几秒）注入一次新输入 → agent 每 6-14 秒跑完整 LLM 工具循环去 `terminal_read`/`terminal_list` → 读到新进度 → 再注入 → **无限自循环，独占整个 eventLoop**。
   - 日志：2026-08-11 20:48–20:52 期间约 20+ 次 `input from agentcli`，无一例外。

2. **QQ 消息无真正优先级（放大原因）**
   - QQ 走 `interceptLoop` → `cancelLLM` + 塞入 `interceptCh`（`eventloop.go:84-85`）。
   - 但 `process()` 打断后 `continue` 回到**同一回合**（`process.go:139`），QQ 的 `[打断消息]` 只被追加到幽灵回合对话里夹带响应，**拿不到独立处理回合**。
   - 面对 agentcli 持续自喂送，QQ 永远排不到前面 → 20:51 之后的 QQ 消息一条未回。

### 止血（运维，立即执行）

- [x] 杀掉残留 `term_3` bash（本机 PID 3716282）→ 幽灵回声立停，QQ 事件循环恢复。
- [x] **QQ 身份注入增强**：`third_party/homeagent-sdk/example/qq/plugin.go` 中断模板新增 QQ号/群号（`evt.UserID`/`evt.GroupID`），agent 无需先调 `get_message` 即可知发送者身份。
- 后续：`agentcli` 终端用后应 `terminal_close`，避免残留。

### 根治（改代码，入本计划）

- **Phase 6**：agentcli 终端通知节流/去重，同源同 tag 的"有新输出"合并；避免对长输出逐段注入。

---

# HomeAgent 记忆层修复计划

> 基于生产实例诊断（2026-08-11），清理完成：`graph.db` 从 6225 条 relations（99.4% 垃圾） → **13 条真实关系**；entities 从 29 → **17**。备份文件：`memory/graph.db.backup.20260811_163301`。

## 核心问题清单

| # | 问题 | 影响 | 位置 |
| --- | ------ | ------ | ------ |
| 1 | GraphDB.Commit 对 relations **裸 INSERT 无去重** | 同一三元组每次归档无限重复，生产 5613 条 `文档→来源→context_archived` 重复垃圾 | `internal/memory/graph.go:209` |
| 2 | `docToTriples()` **对每篇冷文档永远生成固定模板三元组**（`文档→来源→context_archived`、`文档→主题→{summary}`） | 归档即写垃圾，配合问题1指数级累积 | `internal/agent/core/distill.go:390-437` |
| 3 | `core.agent.distill_interval=2d` 配置 **静默失效**（Go `time.ParseDuration` 不支持 `d` 单位），回退 30 分钟默认值 | archiveColdDocs **每小时跑**而非每 2 天，放大问题 1/2 | `internal/config/registry.go:698` |
| 4 | `Pipeline Distiller` 标称"10min 心跳"，**实为 7 天一批回放** | 文档与行为严重不符，虽未直接制造垃圾但不可信 | `internal/memory/pipeline/pipeline.go:194` |
| 5 | 嵌入模型双模加载（中 200k + 英 378k，300 维）直接导致 **2.4G 常驻** | OOM 风险、启动慢，文档未提内存代价 | 部署配置 `/data/cc.*.vec` |

---

## 修复计划

### Phase 1：图记忆去重（最小改动、最高收益）✅ **进行中**

**目标**：`Commit` 对 relations 加唯一约束 + 冲突即跳过，彻底阻断重复累积。

- [x] **Schema 迁移**：`initSchema` 新增 `migrateRelationUnique`——检测旧 relations 表无复合唯一约束（旧 DD表），自动重建为带 `UNIQUE(source_id, target_id, relation_type, session_id)` 的新表并 `INSERT OR IGNORE` 去重（官方 12 步迁移），无需人工干预。
- [x] **Commit 逻辑**：改为"查存在 → 不存在才 INSERT 并计数；已存在则仅刷新 confidence/updated_at"，重复提交不新增、不重计。
- [x] **验证**：`TestCommitDedupSameSession`（同会话重复 commit 不增行）、`TestCommitDedupDifferentSession`（跨会话允许重复）、`TestMigrateRelationUniqueDedupsOldTable`（旧表重建去重）全绿；`go build ./...` 通过。
- [ ] 生产部署后确认：graph.db 36→ 去重（2 组 `like/plugin` 重复消失），跑 1 周不再新增重复。—— **`scripts/verify_deploy.sh` 已含 relations 重复率 + UNIQUE 索引检查**

> 注：entities 已有 `UNIQUE(name)` 保护，仅 relations 缺失。

---

### Phase 2：归档三元组模板清理（治本）✅ **已完成**

**目标**：`docToTriples` 不再把 `context_archived`/`Topic` 摘要当成实体写入图库。

- [x] 重构 `docToTriples`：仅当 `doc.Source` 非 `context_archived` 且非空时写 `文档→来源`；`文档→主题` 仅当 summary 长度合理（<80 字）且非模板化时写，否则跳过。
- [x] 引入 `doc.Meta["is_archived_context"]` 标记上下文归档文档，供 `docToTriples` 识别并跳过（`ContextToDoc` 在 source=`context_archived` 时自动打标）。
- [x] 单测验证：构造冷文档 → `archiveColdDocs` → 无模板垃圾产出（`TestDocToTriplesArchivedContext`/`TestDocToTriplesTemplateSummary`/`TestDocToTriplesLongSummary`/`TestIsTemplateSummary` 全绿，既有 4 个 docToTriples 用例回归通过）。

---

### Phase 3：配置持久化解析修复（防配置失效）✅ **计划中**

**目标**：支持 `2d`/`1w` 等人类可读时长单位，配置即时生效。

- [x] 实现 `parseDurationExtended(string) time.Duration`：正则识别 `\d+[dhw]` → 换算为 `time.Hour*24` 等，再调 `time.ParseDuration`。
- [x] 替换 `GetDuration` 加载点（`registry.go` `GetDuration` 共用），`main.go:423-426` 的四个间隔配置自动受益。
- [x] 单测：`TestParseDurationExtended`（`"2d"==48h`、`"1w"==168h`、`2d12h`、复合/无单位、错误输入）+ `GetDuration` 集成用例全绿。
- [x] 生产核实：当前生产 `core.agent.distill_interval=30m`（可解析，非失效态）；修复为防御性，未来 `2d`/`1w` 写入即可生效。

---

### Phase 4：Pipeline Distiller 行为对齐文档（可选，低优）✅ **已完成**

- [x] 改为真正的增量蒸馏：每 tick 取前 N 条（`BatchSize`，默认 50）未蒸馏记录 → `extractKeyTriples` → `Commit`，**移除 RetentionDays 时间门槛**——新记录下个 tick 即蒸馏（文档所述 10min 频率），不再等 7 天。
- [x] 蒸馏失败重试：`distillBatch` 返回成功标志，Commit 失败时记录写回队头，下个 tick 重试（原实现无论成败都移除，会丢数据）。
- [x] 单测验证启动即蒸馏 + 不重复蒸馏 + batch 分批消化（`TestDistillOnceFreshRecords`/`TestDistillOnceBatchLimit` 新增，既有用例回归通过）。

---

### Phase 5：嵌入模型内存优化（运维侧）✅ **已完成（代码层）**

- [x] 提供 **量化/裁剪** 选项：`embedding_model_path` 支持 `#topN` 规格（如 `/data/cc.zh.300.vec#top50000`），只加载前 N 个词向量（fastText 词频降序，前 N 词覆盖绝大多数文本命中）；`parseModelSpec` 解析规格，`ensureModelFile`/`load` 均按裁剪路径处理，未命中词走 `unkVec` 兜底。无规格行为不变。
- [x] 文档补充：配置项 Description 已写明 `#topN` 用法与内存预算建议（双模 300 维全量 ≈ 1.5G RAM/模型，`top50000` 级裁剪可显著降低）。
- [ ] 生产可选降级：仅保留中文模型（主语言）——部署时在 `embedding_model_path` 只填中文模型或加 `#topN` 即可，无需改代码。
- [x] 单测：`TestStaticEmbedderTopNSpec`（规格解析 + topN 裁剪加载词数）+ `TestStaticEmbedderTopNVectorize`（裁剪后 unkVec 兜底不空向量）全绿。

---

### Phase 6：agentcli 终端通知频率策略（QQ 被无视的直接原因）✅ **进行中**

**背景**：`readLoop`（`internal/plugins/agentcli/plugin.go:556-608`）用写死常量 `NotifyOutputDelay = 500ms`（`plugin.go:24`）做**定时节流**：只要终端持续输出（如 `git sparse clone` 进度），就每 500ms 注入一条 `[终端 X 有新输出]` 到 agent，造成无限自喂送、占满 eventLoop，使 QQ 消息永远只能塞进回环通道且被 echo 上下文淹没。

**原则（明确保留通知，不改中断机制）**：

- **通知机制必须保留**——agent 需要感知"终端仍在运行、可能有待读取的输出"，否则会忘记终端的存在、不知何时去 `terminal_read`。
- 真正要改的是**写死的 500ms 定时节流**，改为**基于输出语义/任务生命周期的通知策略**，"多少输出一通知 / 命令执行结束再通知"由可配策略决定，而非插件写死。

**实现**：

- [x] 通知从"按 500ms 定时"改为**按终端生命周期事件触发**：
  - **进程结束 / 超时 / 读取错误 → 立即通知**（已存在 + 增强 EOF 即时触发）。
  - **持续运行、仅产出进度 → 低频"有新输出"通知**（累计 `notify_bytes` 默认 2KB 未读字节，或距上次通知 `notify_interval` 默认 2s，两条件满足任一即触发），而非每 500ms。
  - **首次创建 → 立即通知"**已启动**"**，确保 agent 感知终端存在。
- [x] 通知频率可配置（per-plugin settings，`notify_bytes`、`notify_interval`），把控制权交还 agent，不写死。
- [x] 纯进度输出仍吸入 `t.buf`，agent 需要时用现有 `terminal_read` 主动拉全量（保持 agent 可感知存在、可自主决策取量）。
- [x] 单测：mock ptyTerm + capture IOInjector 三用例——`TestReadLoopNotifyThrottle`（3s 持续 2KB/s 吐进度仅 ≤3 条通知，远低于 500ms/条的 6 条）、`TestReadLoopNotifyOnExit`（进程退出立即通知）、`TestReadLoopNotifyOnReadError`（读取错误立即通知）全绿。
- [ ] 运维止血：杀掉残留 `term_3` bash（PID 3716282），验证 QQ 消息恢复响应。（生产侧，代码已就绪）

---

### Phase 7：中断机制核验（确认不需改动，仅作记录）✅ **已确认**

**结论**：中断机制本身正确，**无需改动**。文档（ARCHITECTURE.md:9,368-384）明确：

> QQ 等外部插件提示走 `InjectInterrupt → interruptCh → interceptLoop → 塞 interceptCh（内部回环通道）→ process() 每轮前 drainInterrupts() 以 [打断消息] 注入当前对话流` —— **同一段 LLM 记忆内连贯处理**，不割裂、不另开新回合。

- [x] 验证回环通道存在：`interceptCh`（`eventloop.go:85`）+ `drainInterrupts()`（`eventloop.go:404`）在 `process()` 每轮 LLM call 前非阻塞排空注入。
- [x] 确认设计约束："不能开新回合，保证 LLM 记忆连贯" —— 中断注入当前对话流，QQ 在该语境下 `qq_get_message` 看消息、回复、接着干。
- [ ] 仅作回归验证：修复 Phase 6 后，QQ 消息在 agentcli 不泛滥时能正常经回环通道被响应（20:49 已证明机制可达）。

---

## 验收标准

| 指标 | 当前 | 目标 | 验收方式 |
| ------ | ------ | ------ | ---------- |
| Graph relations 重复率 | ~90% (5613/6225) | 0% | `SELECT count(*), count(DISTINCT source_id | | target_id | | relation_type) FROM relations` |
| `context_archived` 关系残留 | 5613 | 0 | `grep` 关系表 |
| `distill_interval` 配置生效 | 失效(30m) | 2d | 日志 `heartbeat distill tick` 间隔 = 48h |
| Pipeline distiller 频率 | 7天一批 | 10min | 日志 `distilled N records` 每 10min |
| 启动内存占用 | 2.4G | <1.5G（单模）或可配 | `systemd` MemoryCurrent |
| agentcli 通知注入频率 | 每 500ms/条 | 仅在生命周期事件/低频里程碑 | 日志 `input from agentcli` 密度显著下降 |
| healthcheck 污染生产存储 | 每 30min 写 `_hc_*` | 0（不落生产存储/虚拟空间） | `knowledge/`、`memory/` 无 `_hc_*`、`gotest`、`luatest` 残留；快照对比 |

---

## 实施顺序建议

1. **立即**：Phase 0.1（healthcheck 污染隔离）——正在持续污染生产，最高优先；顺手清理存量 `_hc_*`/`gotest`/`luatest`。
2. **立即**：Phase 1（去重）+ Phase 3（配置解析）—— 互不依赖，风险最低，收益最大。
3. **次日**：Phase 2（模板清理）—— 需确认 Phase 1 生效后，防止旧垃圾再次写入。
4. **终端洪水紧急项**：Phase 6（agentcli 通知频率策略）——解决 QQ 被无视的直接原因；先运维止血杀残留终端，再上代码。
5. **后续**：Phase 4/5 按需求排期。Phase 7 确认中断机制无需改动。

---

## 相关文件清单

```
internal/plugins/healthcheck/plugin.go  # Phase 0.1：自检写入改为隔离空间/不落盘
internal/plugins/agentcli/plugin.go     # Phase 6：终端通知频率策略
internal/memory/graph.go           # Commit 去重 + Schema 迁移
internal/agent/core/distill.go     # docToTriples 重构
internal/config/registry.go        # parseDurationExtended
internal/memory/pipeline/pipeline.go # distillOnce 增量化
internal/memory/static_embedder.go # 量化/裁剪入口（可选）
```

---

## 已知遗留问题（非阻塞，记录待后续排期）

- **内置 WebUI 插件 (`internal/plugins/webui/`) 设计低劣、Bug 多**：前端交互廉价、API 不稳定、与核心插件机制契合度差。属于技术债，**不在当前核心修复路径**（Phase 0-7）内，待核心记忆层/通知层稳定后统一重写或剥离。

---

## WebUI 改进计划（参考 NapCat WebUI Design DNA）

> NapCat WebUI 设计特征：ACG 樱花/霜蓝配色 + Glassmorphism 玻璃态 + HeroUI 组件 + Framer Motion 弹簧动效 + Canvas 数据可视化。
> HomeAgent WebUI 为 Go 嵌入式 HTML/JS（非 React），改进方向：**在原有技术栈内尽可能逼近设计原则**，重点提升信息架构、交互反馈、视觉层次。

### 设计系统适配（Go 模板 + 原生 CSS/JS）

| 维度 | NapCat 参考 | HomeAgent 适配策略 |
| ------ | ------------- | ------------------- |
| **配色** | 樱花粉 `#FF7FAC` / 霜蓝 `#88C0D0` / 玫瑰红 `#F33B7C`；粉色调中性色 | CSS 变量定义同色系；浅/深色模式切换；主色用于 CTA/聚焦态 |
| **字体** | Quicksand/Nunito 圆润无衬线 + JetBrains Mono | 引入 Google Fonts Quicksand + JetBrains Mono；标题 -0.02em tracking |
| **间距** | 4px 基准单位；卡片 p-4~6；区块 gap-4/6 | CSS Grid/Flex 统一 4px 节奏；卡片内边距 16/20/24px |
| **圆角** | 6/8/12px + 胶囊全圆角 | `--radius-sm:6px --radius-md:8px --radius-lg:12px --radius-full:9999px` |
| **玻璃态** | `backdrop-blur-sm~xl` + 半透明白/黑 + 微边框 | CSS `backdrop-filter: blur(8px)` + `rgba(255,255,255,0.6)` / `rgba(0,0,0,0.4)` + `border:1px solid rgba(255,255,255,0.25)` |
| **阴影/层级** | 软扩散 + backdrop-blur 分层 | `box-shadow: 0 1px 2px rgba(0,0,0,.05)` 低层 / `0 4px 6px rgba(0,0,0,.07)` 中层 / `0 20px 25px rgba(0,0,0,.1)` 高层 |
| **动效** | 弹簧物理 120-150 stiffness；150-400ms | CSS `transition: 200ms cubic-bezier(.34,1.56,.64,1)` 模拟弹簧；页面切换 fade-up+scale |
| **图标** | Lucide 2px stroke | 引入 Lucide 静态 SVG（内联）或同风格 iconfont |

### 信息架构重构（核心痛点）

| 现状 | 目标（对标 NapCat Dashboard） |
| ------ | ----------------------------- |
| 单页面堆砌所有功能 | **左侧可折叠侧边栏**（16rem 固定）→ 导航分组：概览/记忆/工具/插件/配置/日志 |
| 无面包屑、无状态反馈 | 顶部面包屑 + 悬停微动效；关键操作 Toast 反馈（右上角） |
| 表格/列表无视觉分组 | 卡片网格布局：每卡片 = 一个功能模块（记忆统计/插件状态/工具调用/系统资源） |
| 无数据可视化 | Canvas 2D 绘制：记忆增长趋势图、CPU/内存环图、工具调用热力图 |

### 交互体验对标

| 场景 | NapCat 做法 | HomeAgent 改进 |
| ------ | ------------- | ---------------- |
| 卡片悬停 | 3D 透视倾斜 + 光标跟随渐变光斑 | CSS `transform: perspective(1000px) rotateX/Y(±5deg)` + 伪元素光斑跟随鼠标 |
| 按钮点击 | 弹簧 scale + loading 态 | `:active { transform: scale(0.97) }` + 内置 spinner |
| 页面切换 | Framer Motion fade-up+scale stagger | CSS `@keyframes fadeUpScale` + JS 交错延迟 50ms |
| 空状态 | 插画 + 友好文案 + 引导 | 每模块空状态统一组件：图标 + 说明 + 主操作按钮 |
| 错误处理 | Toast 右上 + 破坏性操作确认弹窗 | 统一 `showToast(type, msg)` + `confirmDialog(action, onConfirm)` |

### 实施路线（非阻塞，Phase 8+）

```
Phase 8.1: ✅ CSS 变量系统 + Glassmorphism 基础样式（浅/深色）— dashboard.html :root 重写 NapCat DNA tokens（sakura/frost 色板、玻璃变量、阴影、圆角、字体、动效）
Phase 8.2: ✅ 布局重构 — 左侧 16rem 可折叠侧边栏 + 顶部面包屑 topbar + 卡片网格响应式（toggleSidebar/switchTab 联动）
Phase 8.3: ✅ 核心页面卡片化 — 全部 .card 玻璃态 + hover 抬升 + 语义色 badge/dot/按钮；星图/终端/设置面板统一换肤
Phase 8.4: ✅ 交互微动效 — 3D 透视倾斜 + 光标光斑（事件委托 .card.tilt）、弹簧按钮 scale、Toast 滑入动画、Loading、tab 切换 fade-up
Phase 8.5: ✅ 数据可视化 — Canvas 记忆分布环图（drawDonut 扫掠动画）+ 运行时资源条形图（延迟生长动画）
Phase 8.6: ✅ 空状态/错误/确认弹窗统一组件库 — showToast(type,msg) + confirmDialog(action,onConfirm)（Esc/Enter/遮罩关闭）
Phase 8.7: ✅ 无障碍/键盘导航/移动端适配 — :focus-visible ring、prefers-reduced-motion 全停动效、主题滚动条、移动端自动折叠侧边栏
Phase 8.8: ✅ 用户反馈迭代（8.x 收尾）— ①健康检查 UI 去冗余（系统操作卡仅保留重载插件，健康检查卡自带右上角「运行」按钮+空态文案）；②主题跟随系统（无手动偏好时用 prefers-color-scheme，并监听系统实时切换）；③设置页内容列 max-width 860px 居中；④emoji 清理（☰/☀️/🌙/⛔/🔧/🧠 → 内联 SVG/纯文本，聊天工具调用状态用语义色图标）；⑤总览页重构为插件页式全宽单列卡片（移除看板娘大照片卡、移除不准确的记忆分布环图+运行时资源条形图，runtime/memory 改 kv-row 精确数字展示），kernel 页同化
```

> 实现均在 `internal/plugins/webui/dashboard.html`（纯 CSS + Vanilla JS，无构建链）；`handler_test.go:641` 修复上游遗留断言失配（`api('/settings'` → `api("/settings"`）。

### 技术约束

- **保持 Go `html/template` + 内嵌静态资源** —— 不引入 Node/构建链
- 静态资源（CSS/JS/字体/图标）以 `embed.FS` 内嵌二进制
- 复杂动效用纯 CSS + 极简 Vanilla JS（无框架依赖）
- 优先修复现有 Bug（API 500、WebSocket 断连、表单提交无反馈）再做视觉

---

## 回滚预案

- Phase 1/2 修改数据库 Schema/写入逻辑：保留 `graph.db.backup.*`，出问题 `systemctl stop homeagent && cp backup graph.db && systemctl start`。
- Phase 3 仅改配置解析，回滚即改回 `time.ParseDuration`。
- 所有改动需先跑 `make test`（内存/图/文档/配置全绿）再部署。

---

*更新时间：2026-08-11*
*生产实例：`/home/newqqagent`，systemd 托管，二进制 `/usr/local/bin/homed` (v0.8.0, 2026-07-28 build)*
---

## 9. device_ctl 设备接入网关：WebUI 监听 + devicedetect 扫描

### 设计意图备忘（对齐核心架构原则）

1. **插件即 Agent 的 App，IO 全在插件层** —— 设备接入是 IO 能力，**必须全部收敛到 webui 插件**（内核零 IO 原则），
   设备状态归 webui 插件管理，agent 只经工具访问。**不触碰 CLI/waiter**（CLI 本机自执行命令已有 `cmd` 插件，反向操控 CLI 属重复造轮子，不在本计划范围）。
2. **认知负荷最小 / 反提示词注入** —— 设备元数据（dev_id/ip/status）只出现在工具参数与返回值，**绝不进 system prompt**；
   agent 只通过 `devicedetect` / `device_ctl_*` 工具按需查询，避免设备名/IP 污染对话上下文。
3. **工具语义自解释** —— `devicedetect` / `device_ctl_*` 的 Description 让 agent「读完即知怎么用」，无需额外指令。
4. **显式授权为唯一信任源** —— 任何 device_ctl 执行必须 **先授权、后执行**；高危操作（cmdrun/open）需**每次二次确认**（授权确认响应中的 accept 字段）。授权状态持久化，重启不丢。

### 目标（需求澄清）

用户明确收敛为：**只做 webui**（webui 开监听端口作为设备接入网关），提供 `devicedetect` 工具供 agent 扫描设备连接状态。
CLI 本机自执行命令已有 cmd 插件，不做反向操控 CLI。

### 一、协议与接入（webui 充当“设备注册网关”）

**背景**：当前 webui 是纯 HTTP/SSE 服务（`internal/plugins/webui`）：`/api/v1/*` REST（webui API key 认证）+ 聊天 SSE。设备接入需要一条**独立、受控、可被反向推送**的通道。

**设计**：新增 **JSON-over-WebSocket 监听**（agent→设备反向推送 + 设备→agent 上报，双向长连接），复用 webui 插件。

| # | 内容 | 位置 | 风险 |
| --- | ------ | ------ | ------ |
| A1 | 新增 WebSocket 端点 `/api/v1/device/ws`（API key 认证，仅连接期有效），JSON 消息帧 | `internal/plugins/webui/handler.go` | 中（WS 依赖库 `github.com/gorilla/websocket`，判断是否已引入；若无则用 `golang.org/x/net/websocket` 或静态协议） |
| A2 | 新增 REST：`GET /api/v1/device/online`（在线设备列表/状态）、`POST /api/v1/device/push`（向已连接设备推 JSON） | 同上 | 低 |
| A3 | **设备能力声明**：连接时设备上传 `{op:"hello", device:{name,kind,caps:[...]}}`，webui 记录并登记在线 | 同上 | 低 |
| A4 | **设备侧授权绑定**：首次连接需 `{op:"bind", token}`；token 由 webui 设置页生成（`device_gateway.token` 配置），绑定成功后该设备进入“已授权”集合 | `plugin.go`(设置) + handler | 中 |

### 二、Agent 工具（devicedetect 等）

**设计**：注册一个 `devicectl` 设备（实现 `agentIO.Device` 接口，`internal/plugins/webui` 内定义），
`Tools()` 返回三工具，`Execute()` 检查授权 + 路由到 WebSocket 在线设备。

| 工具 | 语义 | 参数 | 返回值 |
| ------ | ------ | ------ | -------- |
| `devicedetect` | 扫描/列出已连接且已授权的设备 | `kind`(可选) | `[{device_id,name,kind,caps,online}]` |
| `device_ctl_status` | 查询单设备实时状态 | `device_id` | `{device,status,last_seen}` |
| `device_ctl_cmdrun` | 向设备发送命令执行请求（**高危，需授权+二次确认**） | `device_id, command` | `{accepted:true, request_id}` 或拒绝 |

**注意**：webui 插件属于内置插件，注册 `devicectl` 设备走 `s.RegisterChannel("devicectl", dev)`（IOManager 自动并入工具集，`ExecuteTool` 自动可达）。设备 Execute 是**同步阻塞**的，但 cmdrun 是异步的——需维护 **pending 请求表（request_id → chan）**，WS 收到设备结果后写回，工具循环内超时返回。

### 三、webui 前端：设备管理页

| # | 内容 | 位置 | 风险 |
| --- | ------ | ------ | ------ |
| F1 | 侧栏新增「设备」入口 + 页面：设备列表（在线/离线/已授权/未授权）、连接状态徽标 | `dashboard.html` | 低（纯前端） |
| F2 | 设备详情：基本信息（名称/种类/caps）、状态/历史、授权/取消授权按钮、`device_ctl_cmdrun` 命令输入+结果展示 | 同上 | 低 |
| F3 | 设备接入引导：显示 `device_gateway.token` + 接入方式说明（WS URL + 绑定 token），供外部设备复制 | 同上 | 中（token 明文展示，需「显示/隐藏」） |

### 四、安全与授权

| # | 内容 | 位置 | 风险 |
| --- | ------ | ------ | ------ |
| S1 | `device_gateway.token`：启动时生成并持久化（复用 `Settings()` 机制，webui 插件设置页可「显示/重置」） | `plugin.go` / handler | 低 |
| S2 | **授权级别**：只读（status/detect）无需确认；**执行类（cmdrun）需二次确认**——工具返回 `{accepted:false, require_confirm:true}`，agent 需再调 `device_ctl_confirm` 或经 webui 前端用户点击「允许」 | handler + 工具 | 中 |
| S3 | **授权状态持久化**：已授权设备集合存 webui 插件配置（SQLite config.db，复用 Settings），重启不丢 | 同上 | 低 |
| S4 | **WS 连接安全**：连接即要求 API key（`Sec-WebSocket-Protocol` 或 query token）；每消息帧校验；超时/断开自动清理在线表 | handler | 中 |

### 五、实施顺序（本计划按此逐步 push，每步可独立验证）

| Phase | 内容 | 验证 |
| ------- | ------ | ------ |
| **P1** | 协议与接入：新增 WS 端点 + hello/bind 握手 + 在线设备登记（内存） | curl/WS 客户端连上 → `GET /api/v1/device/online` 可见 |
| **P2** | REST 面：`/api/v1/device/online`、`/api/v1/device/push`、（device_gateway.token 设置注册） | token 生成/持久化；push 到在线设备收到 JSON |
| **P3** | Agent 工具：`devicectl` Device + `devicedetect` / `device_ctl_status` / `device_ctl_cmdrun` | agent 工具面板可见三工具；devicedetect 返回在线设备 |
| **P4** | 异步 cmdrun：pending 请求表 + WS 结果回写 + 超时 | 模拟设备返回 cmdrun 结果，agent 拿到 |
| **P5** | 授权：bind 绑定 + token 校验 + 持久化已授权集合 + cmdrun 二次确认流 | 未绑定拒绝；已绑定可查询；cmdrun 需确认 |
| **P6** | webui 前端设备管理页（F1–F3） | 页面可见在线/授权/执行状态 |
| **P7** | 单测 + 文档：handler/工具/授权单测；README/架构文档补充 | `make test` 全绿 |

### 六、里程碑外延（明确不做）

- **不做 CLI/waiter 反向操控**（本机命令已由 cmd 插件承担；预期语义是“设备本来就是远程的”）。
- 不做 QQ/微信等具体设备插件——设备按通用 WS 协议接入即可。
- 单设备回调/事件推送的复杂路由（多设备扇出、订阅过滤）留给后续迭代。

### 七、回滚预案

- P1–P5 均为新增代码/路由，不触碰现有 webui 路由与 CLI socket；既有功能完全不受影响。
- 若 WS 依赖库引入失败：回退为**独立 TCP 监听**（复用 cli 插件逐行 JSON 模式，协议一致）——同样满足“设备接入网关”。
- 所有改动先 `make build build-cli` + `make test` 后再部署；生产回滚即替换旧 `homed` 二进制。

> **更新（2026-08-16）**：用户定案——device 接入独立为 `remotedevice` 插件（webui 只做前端反代，如 `proxyToPluginmgr` 先例；不把 WS 监听直接长在 webui 插件上，避免 webui 过重）。本插件的所有实现细节沿用上述设计，落点全部移到 `internal/plugins/remotedevice/`：
>
> - 设备网关（WS 监听 + hello/bind/在线登记）→ remotedevice 自持监听端口（默认 127.0.0.1:9890，配置 `listen_addr`）
> - REST（online/push）→ remotedevice 自带 HTTP mux（地址同上）
> - Agent 工具（devicectl Device + devicedetect/device_ctl_*）→ remotedevice 内 `s.RegisterChannel("devicectl", dev)`
> - 设置项（listen_addr / ws token / 已授权集合）→ remotedevice 插件 Settings（config_remotedevice 表）
> - webui 前端设备页 → webui `/api/v1/device/*` **可选反代**到 remotedevice（参照 `proxyToPluginmgr` 模式）：**默认禁用**，用户配置 `device_gateway_enabled` / `device_gateway_addr` / `device_gateway_token` 后才挂载，避免硬耦合
> - 设备网关鉴权：webui 层 API key（`requireAPI`）+ 网关层 remotedevice token（`X-API-Key`）双鉴权

## 10. 实施：remotedevice 独立插件（逐步推进）

### Phase 0：插件骨架 + 设备注册表 + WS 网关（本轮）

### Phase 0：插件骨架 + 设备注册表 + WS 网关 + devicectl 工具（已实施）

**已交付**：

- `internal/plugins/remotedevice/` 独立插件：`registry.go`（设备注册表 + 标准库 WebSocket 网关 + push/await/结果留档）、`plugin.go`（设置 + REST 面 + HTTP 服务）、`device.go`（devicectl Device + 四工具）
- Agent 工具：`devicedetect` / `device_ctl_status` / `device_ctl_cmdrun` / `device_ctl_cmdresult`（经 `s.RegisterChannel("devicectl", dev)` 并入 IOManager 工具集）
- 设备接入：WS 端点 `/api/v1/device/ws`（token 认证，hello/bind/status/cmd_result 协议），默认监听 127.0.0.1:9890
- REST 管理面：`/api/v1/device`（列表）、`/api/v1/device/online`、`/api/v1/device/{id}`、`/api/v1/device/push`、`/api/v1/device/auth`（全部 token 鉴权）
- 授权：token 校验（`ws_token`，启动生成持久化）+ 已授权集合持久化（`authorized_devices`，逗号分隔）+ `RestoreAuthorized` 重启恢复
- webui 可配置反代：新增 `device_gateway_enabled`(默认 false)/`device_gateway_addr`/`device_gateway_token` 设置；仅启用时挂 `/api/v1/device/` 反代路由（webui API key 鉴权 → 转发带 remotedevice token）
- 装配：`internal/plugins/all.go` 注册 remotedevice

**验证**：`gofmt -w` + `go build ./...` exit:0 ✅

**待办（后续 Phase）**：

- [] 单测：注册表/WS 握手/push/cmdrun/授权
- [] webui 前端设备管理页（非必选，可经 REST/CLI 使用）
- [] 心跳/离线自动清理的周期 goroutine（当前断开即清理）

### Phase 0b：GUI 连接 remotedevice（已实施）

**需求**：本机有 GUI（Electron），为其添加连接 remotedevice 网关的逻辑，可查看/授权/控制设备。

**已交付**（cmd/gui/）：

- **连接类型**：表单新增 `device`（设备网关 remotedevice）类型；`saveConnForm` 加 device 分支（url+apiKey=ws_token）；连接测试加 device 探活（GET /api/v1/device，带 X-API-Key）
- **normalize 修复**：`main.js normalizeConnections` 允许 `type==="device"`（原来会强制改回 webui）
- **侧栏「设备」入口** + `view-devices` 容器（index.html）
- **renderDevices()**：设备列表（名称/种类/在线/授权/能力）、空态引导、"执行命令"（prompt 输入→POST /device/push）、"授权/取消授权"（POST /device/auth）、"刷新"
- `renderAll`/`refreshAll` 挂载 renderDevices；state.devices

**验证**：`node --check renderer/app.js` 与 `node --check main.js` 全通过；`go test ./internal/plugins/... ./internal/sdk/... ./internal/agent/...` 全绿；remotedevice 端到端（WS 设备接入 → REST 列表/online/鉴权/push）验证通过。

**设备模拟全链路**（验证实录）：

- WS 连 /api/v1/device/ws?token=... → hello_ack → bind_ack → 服务端 push {op:cmd, command} → 设备回 cmd_result
- GET /api/v1/device 返回全部设备（合并授权态）；GET /api/v1/device/online 只返回在线；无 token 401
- GUI 的 "执行命令" 走 /device/push 已验证下发到达设备

### Phase 0c：端到端验收（已完成）

**环境**：临时 homed（-data /tmp/ha-dev）+ Python WS 设备模拟 + Electron GUI 冒烟。

**验证结果**：

- ✅ `go build ./...` exit:0；`go test ./internal/plugins/... ./internal/sdk/... ./internal/agent/...` 全绿
- ✅ remotedevice 网关监听 127.0.0.1:9890（config_remotedevice 表：listen_addr/ws_token/authorized_devices 持久化）
- ✅ WS 设备接入全链路：hello_ack → bind_ack → 服务端 push {op:cmd} → 设备回 cmd_result
- ✅ REST：GET /api/v1/device（含授权态合并）、/online（仅在线）、无 token 401
- ✅ GUI：`node --check renderer/app.js` + `node --check main.js` 通过；Electron 冒烟（--disable-gpu）稳定运行无 JS 错误（headless 容器需禁 GPU）
- ✅ 说明：webui 反代默认禁用（device_gateway_enabled=false），GUI 直连 remotedevice 网关不受影响

**遗留/后续**：

- [ ] remotedevice 单测（registry/WS 握手/push/授权）
- [ ] agent 真实工具调用验证（需 LLM key，本环境无）
- [ ] webui 设备管理页（可选，GUI 已覆盖）
- [ ] cmdrun 二次确认流（当前为已授权即下发）

### Phase 0d：真实 LLM 驱动工具调用测试（完成 ✅）

**环境**：本机 LLM 网关 `http://127.0.0.1:8080/v1`（deepseek-v4-flash-free，Bearer key）+ 临时 homed + Python WS 模拟设备（living-light 客厅灯，已授权在线）。

**配置**：`core.llm.base_url/api_key/model=AUTO/adapter=openai` + `core.llm.sources.default.*`；重启后 `model=AUTO base=http://127.0.0.1:8080/v1 sources=2` ✓

**测试1：devicedetect（扫描设备）**
> 用户指令："请扫描一下当前有哪些设备在线，用devicedetect工具"
> Agent 真实调用 devicedetect → 返回客厅智能灯（living-light）类型/状态/授权/能力/最近在线时间 ✓（7.8s）

**测试2：device_ctl_cmdrun（反向操控设备）—— 决定性验证**
> 用户指令："用device_ctl_cmdrun让客厅灯living-light执行 turn_on 命令打开灯"
> 设备模拟日志：`PUSHED: {command: turn_on, op: cmd, req_id: f00dd0596ffeee2d}` → 设备回 `cmd_result`
> Agent 回复："开灯指令已成功下发执行（status: ok，请求 ID f00dd0596ffeee2d）" ✓（16.6s）

**结论**：HomeAgent → LLM 决策 → devicectl 工具 → WS push → 设备执行 → cmd_result 回执 → agent 汇报 全链路真实跑通，即"agent 反向操控设备"核心能力已验证。

**清理**：已终止临时 homed / 模拟设备 / 临时文件；代码改动与 plan.md 保留。

### Phase 0e：GUI 作为设备接入 + 托盘驻留 + 偏好设置 + 聊天卡顿修复（已实施）

**设备桥（GUI 作为设备）**

- `cmd/gui/main.js` 新增设备桥：GUI 以 `device_id: gui-<hostname>` 接入 remotedevice WS（hello/bind），收到 `{op:cmd}` 用 `spawn` 本机执行（白名单命令 + 15s 限时 + 8KB 截断）回 `cmd_result`
- whenReady 时若存在 `type="device"` 连接则自动启动设备桥；before-quit 停设备桥

**托盘驻留**

- `initTray()`：nativeImage icon + 菜单（显示主界面/退出）+ 双击显示；whenReady 调用
- `window-all-closed` 依 `exitToTray` 偏好：true 则隐藏驻留（后台维持设备桥），false 则 quit
- `window:close` handler 改为 exitToTray 时 hide（修复了托盘块重复注册崩溃）

**偏好设置（prefs:get/set + 设置页 UI）**

- `gui-prefs.json`（userData）存 `autoLaunch`（开机自启，Electron `app.setLoginItemSettings` + openAsHidden）/ `silentStart`（静默启动，createWindow 后 hide）/ `exitToTray`（退出进托盘，默认 true）
- preload 暴露 `homeagent.prefs`；设置页「客户端偏好」卡片三个开关

**聊天卡顿/输入框卡死修复**

- 根因：webui 连接 sendChat POST `/chat`（同步等 60s 完整结果）与 SSE 流式（agent_output 增量）**双通道重复**，回复长时反复全量 innerHTML 重建 + marked.parse 占满主线程 → UI 卡死、输入冻结
- 修复：webui 走**触发式 POST**（15s 短超时确认受理，回复靠 SSE 流式增量渲染）；cli/device 无 SSE 保持同步等完整结果
- `connectSSE` 跳过 device（设备网关无 chat/events）；sendChat 对 device 连接提示不支持聊天
- 流式增量渲染（`renderChatStreamChunk` 90ms 防抖 + 200字符/300ms 节流 parse）保留，输入框 DOM 不被重建

**验证**：node --check main.js/preload.js/app.js 全通过；Electron 冒烟（--disable-gpu --in-process-gpu）稳定运行无 JS 错误；go build ./... exit:0；go test plugins 全绿

**待后续**：screenuse（GUI 拉起窗口显示信息，规划中）；cmdrun 二次确认流；remotedevice 单测

### Phase 0f：deviceinfo 工具 + GUI 被控端能力声明（已实施并真实验证）

**deviceinfo 工具（agent 探查设备详情+能力）**

- `internal/plugins/remotedevice/device.go`：新增第五个工具 `deviceinfo`（参数 device_id），返回设备接入时声明的 info（hostname/platform/arch/cpus/mem/版本）+ caps 能力列表；需已授权
- `registry.go`：`DeviceMeta` 新增 `Info map[string]interface{}`；`metaFromMsg` 解析 hello 的 `device.info`；`publicDevices` 输出带 info

**GUI 被控端能力**

- `cmd/gui/main.js` 设备桥 hello 声明 `caps: ["status","cmdrun","deviceinfo","cmdresult"]` + `info`（本机 hostname/platform/arch/cpus/totalmem/node/electron 版本）

**真实验证（模拟 GUI 设备 + 本机 LLM 网关）**
> 用户指令："用deviceinfo探查 gui-testhost 这台设备的信息和它支持什么能力"
> Agent 调 deviceinfo → 返回基本信息（device_id/名称/类型/平台/主机名/在线授权）、硬件（8核/16G/Node v22/Electron 33）、能力（status/cmdrun/deviceinfo）✓（11.7s）

**遗留**：screenuse（GUI 拉起窗口显示，规划中）；cmdrun 二次确认流；remotedevice 单测

### Phase 0g：waiter CLI 设备桥（已实施并端到端验证）

**实现**（cmd/waiter/）：

- `device.go`：`deviceBridge`（纯 Go 标准库 WS 客户端）——握手/帧/hello/bind/readLoop/execCommand，白名单命令 + 15s 超时 + 8KB 截断；cap 声明 status/cmdrun/deviceinfo；info 上报 hostname/platform/arch/cpus/mem_mb
- `config.go`：Config 加 `device_gateway` + `device_token`（waiter.yaml）
- `main.go`：`--device` / `--device-token` 启动设备桥；或读配置

**端到端验证（真实执行）**：

- `waiter -device 127.0.0.1:9890 -device-token ...` → `device bridge active: waiter-jianf-Station`
- remotedevice 登记：waiter-jianf-Station / HomeAgent CLI (waiter) / computer / caps[status,cmdrun,deviceinfo] / info{arch:amd64,cpus:32,mem_mb:23129,hostname:jianf-Station} authorized ✓ online ✓
- **device_ctl_cmdrun**："用device_ctl_cmdrun让 waiter-jianf-Station 执行 uname -a" → agent 调工具 → waiter 本机 exec → 返回 `Linux jianf-Station 6.12.101+deb13-amd64 ... x86_64 GNU/Linux` ✓（9.2s）
- **deviceinfo**："用deviceinfo查看 waiter-jianf-Station" → 32 核 / 22.6GB / Linux amd64 / 三能力 ✓（11.3s）

**修复的关键 Bug**：9890 被残留进程占用导致新 homed remotedevice 监听失败（acceptBind 用旧 token → 401）；杀残留后恢复正常。

**验证环境**：临时 homed（LLM 网关）+ 新编译 waiter；全量 go build/test 通过（仅剩 2 个与改动无关的既有 system 失败）。

### Phase 0h：设备默认不授权，用户手动授权（已实施并真实验证）

**需求**：GUI 与 waiter 设备默认不授权，必须用户手动授权。

**修复（registry.go bind）**：

- bind 原无条件 `SetAuthorized(id, true)`（每次重连/心跳都自动授权，撤销被覆盖）→ 改为**仅验证 token + 登记设备，绝不自动授权**
- 授权完全由用户手动控制：GUI 本机卡片/设备页「授权本机」按钮 → REST `/api/v1/device/auth`，或 waiter 用户手动操作
- 新增设备接入即 `authorized:false`，agent 的 device_ctl_* 默认拒绝

**配套修复（cmd/gui/main.js）**：

- 设备桥选连接：优先 `currentId`，否则最后一个 device 连接（避免列表里旧连接旧 token 抢先 → 401）
- 清理 connections.json 残留的旧 device 连接

**真实验证（GUI 被控端 + 本机 LLM 网关）**：

- GUI 接入后 `gui-jianf-Station authorized: **False**` ✅（默认不授权）
- **手动授权后** → agent `device_ctl_cmdrun echo AUTH_OK` → 返回 AUTH_OK ✅
- **撤销授权后** → agent `device_ctl_cmdrun` 返回 "device gui-jianf-Station 未授权，无法执行命令" ❌；本地 cmd_run 兜底成功（本机能力，合理）
- agent 智能：未授权时自动改用本地 cmd_run 并提示"去设备管理页授权"

**结论**：GUI/waiter 被控设备默认不可被 agent 远程操控，用户手动授权后才可；撤销即时生效。

### Phase 0i：GUI 视觉修复（圆角 / 字体方框 / 托盘图标 / 授权开关融合）

**托盘图标**（cmd/gui/）：

- 生成 `icon-tray.png`(22x22) + `icon-tray@2x.png`(44x44)（ImageMagick 从 icon.svg 转换）
- `initTray` 平台化：Linux 用 PNG（ico/svg 在 Linux 托盘不受支持）、Windows 用 ico、Linux resize 22x22 兜底

**窗口圆角**（cmd/gui/）：

- `createWindow` 加 `transparent: true` + `roundedCorners`（透明帧圆角窗口）
- CSS：`body` 透明 + 18px 圆角；`#app` 18px 圆角 + 背景（背景移入容器内裁切）

**设备页字体/方框修复**（renderer/style.css）：

- table 10px 圆角 + collapse + 行 hover；th 加粗；kv-row .val flex 对齐
- `.switch input` `-webkit-appearance:none` + 背景透明（消除 checkbox 浅蓝方框）

**授权开关融合**（renderer/app.js）：

- 本机授权改为 **kv-row 信息行内嵌滑动开关**（key=授权，val=开关+状态圆点），与设备ID/网关/状态行同构，消除割裂
- 设备列表"执行命令"按钮替换为**授权开关**（checked=已授权）
- 修复 onchange `JSON.stringify(id)` 双引号冲突 → 单引号转义 `deviceToggleAuth('id',this.checked)`

**验证**（CDP 计算样式）：body/app 18px 圆角可见、card 14px、table 10px、switch input 背景透明；CDP 模拟点击授权开关 → authorized true 持久化；agent device_ctl_cmdrun 操控 GUI 成功。

### Phase 0j：白屏根因定位 + 逐步安全加回（已完成）

**白屏根因（二分定位确定）**：新版 main.js 顶层 `const { Tray, Menu: ElectronMenu, nativeImage } = require("electron")`（在 asar/无托盘环境加载异常）→ 导致 renderer 合成卡死、窗口全灰白无绘制。设备桥/新版 renderer 本身安全（混合测试证明）。

**修复**：托盘改**函数内惰性 require + try/catch 安全降级**（失败不动托盘、不影响窗口）。

**逐步加回验证**（每步实测窗口字节 70KB 正常）：

- F3：旧 main 主体 + 设备桥 + handlers + 新版 renderer（可用基线）
- Step1：安全托盘（惰性 require）— 显示正常 ✅
- Step2：完整 prefs（GUI_PREFS_FILE 持久化 / loadGuiPrefs / applyAutoLaunch / silentStart / 开机自启）— 显示正常 ✅
- Step3：生命周期（window-all-closed 退出进托盘依偏好、before-quit 清理托盘/设备桥）— 显示正常 ✅

**最终版已安装**：/opt/HomeAgent（md5 047f2f1bbe，备份 /tmp/ha-app-step3-final.asar），含：设备桥 / 设备页+授权开关 / 惰性托盘 / prefs 持久化 / 退出进托盘 / 字体/圆角（renderer）。

---

## 11. 插件架构缺陷修复 + 子进程化迁移评估

> 完整评估文档：[`docs/zh/架构迁移评估.md`](docs/zh/架构迁移评估.md)
> —— **先读其第零章「给接手者的阅读指引」**，该文档是增量写成的，前六章部分结论已被后续推翻。
> 可复跑实验：[`docs/zh/experiments/plugin-arch/`](docs/zh/experiments/plugin-arch/)（18 项，`./run.sh`）
>
> **本节 11.1~11.6 是修复项的唯一权威编号。** 评估文档中出现的 `0.x` / `A-F`
> 仅为历史分组，勿用于实施。本节只列可执行项与决策状态；论证与数据见评估文档。

### 11.0-pre 三个易被误解的前提（动手前必读）

1. **stage 的并发扇出是原始设计，不是缺陷。**
   `stages.go:124` 的 `go func` + `wg.Wait()` 是刻意的，`StageContext` 的 `RWMutex`
   与公开 `Lock/RLock` 就是为它准备的。**问题是 C ABI 把外部插件降级成副本模型**，
   使那把锁在 ABI 边界外变成空转（内置 0% 丢失 vs 副本 35.8~36.8%）。
   → 不要试图"取消并发"来修 11.3。

2. **内置插件的高权限是刻意设计，不是"自己人所以安全"。**
   但当前实现混淆了「应有的权限梯度」与「C ABI 表达能力天花板」：
   外部插件拿不到 `OutputChan`/`Subscribe` 是技术限制（`case 23/24` 是空实现，
   属"给不了"），而非权限决定。迁移目标是让梯度**显式化并强制**，不是消除梯度。

3. **副本模型是"为方便插件加载的无奈之举"。**
   C ABI 用于绕开 Go 原生 `plugin` 包的同版本限制，副本模型是其必然代价。
   问题在于该代价未被记录、后果未被发现——不是当初的选择错了。

### 11.0 起因

更换 `plugin.so` 后 `plgreload` 报成功但运行旧代码。根因是 Go c-shared 的
ELF `DF_1_NODELETE` 标记使 `dlclose` 成为 no-op，**换 `.so` 必须重启 homed**。
排查该问题时连带发现 6 类此前未知的缺陷，其中 **2 项正在生产环境造成故障**。

### 11.1 紧急：output_send 永远返回成功 ⚠️ 现网已发生

**现象**：模型调用 `output_send__qq` 收到「已发送」，但消息实际未送达，模型不知道也不重试。

**根因**（`internal/plugin/cabi/loader.go:458-470`）——注释自己写明了原因：

```go
// Output is async: return immediately, send in background
// to avoid nested cgo calls (cgo within cgo can crash)
go func() {
    if err := pluginInvokeOutput(pid, chName, argsJSON); err != nil {
        log.Printf("[dispatch] async output %s/%s failed: %v", ...)  // ← 仅日志
    }
}()
return map[string]interface{}{"status": "queued"}, nil   // ← 立即返回"成功"
```

`output.go:65-70` 拿到 `{status:queued}` + `err=nil`，返回给模型「已通过 [qq] 通道发送」。

**现网证据**（近 7 天）：成功 44 次，失败 2 次。

```
Aug 30 15:10:51 [dispatch] async output qq/qq failed:
  invoke_output qq: meta 中需要 group_id 或 user_id 字段
```

**与此前排查的关系**：之前诊断「qq 渠道回复丢失」时修复了系统提示词
（`a3a5cd4`，强调 qq 是异步通道、必须用 `output_send`），
但**未发现 `output_send` 本身永远报成功**——模型即使正确调用也无法感知失败。

**修复方案**（保留 goroutine + 带超时 channel 等待，避免 cgo 嵌套）：

```go
resCh := make(chan error, 1)
go func() { resCh <- pluginInvokeOutput(pid, chName, argsJSON) }()
select {
case err := <-resCh:
    if err != nil { return nil, err }                      // 真实失败上报
    return map[string]interface{}{"status": "sent"}, nil
case <-time.After(10 * time.Second):
    return map[string]interface{}{"status": "queued", "note": "发送超时未确认"}, nil
}
```

`dev.Execute` 由 `executeOutputSendTool` 从 Go 侧调起（不在 cgo 栈内），
goroutine 里的 `pluginInvokeOutput` 才是 cgo 调用，**不构成嵌套**。

- [x] 实现修复 —— `loader.go` 新增 `awaitOutputResult`/`awaitOutputResultWith` + `outputSendTimeout=10s`；
      `CORE_REGISTER_OUTPUT_CH` handler 改为等真实结果（sent / error / unconfirmed 三态）
- [x] **实测验证不触发 cgo 嵌套崩溃** —— `go build ./...` exit 0 + `go test ./internal/plugin/... ./internal/agent/...` 全绿；
      `awaitOutputResult` 只在 `RegisterOutputChannel` handler 内被调用，该 handler 由 `executeOutputSendTool` 从 Go 侧调起，非 cgo 栈
- [x] 构造 meta 缺 `user_id` 的失败场景，确认模型收到错误而非"已发送" —— `TestAwaitOutputResult_Failure` 断言返回 error；
      `output.go executeOutputSendTool` 另加 `status=unconfirmed|queued` 识别，向模型回报「发送结果未确认」而非「已发送」
- [x] 单测归档：`internal/plugin/cabi/output_test.go`（Success/Failure/Timeout 三例）

### 11.2 紧急：cgo 工具超时不可中断，线性泄漏 ⚠️ 现网已发生 26 次

**现象**：`toolcall.go:41` 日志称「已取消」，实际什么都没取消。

**根因**：`select` 超时只让调用方返回，goroutine 仍卡在 `C.call_invoke_tool` 里。
**cgo 调用不可被 Go runtime 抢占或取消**，该 OS 线程永久占用。

**实验 14 实测**（纯 C 死循环 `.so`，20 次卡死调用）：

```
 5 次后: goroutines= 6 threads= 9 (+3)
10 次后: goroutines=11 threads=14 (+8)
20 次后: goroutines=21 threads=24 (+18)
线性泄漏，永不回收
```

对照：子进程模型 `Process.Kill()` 后 OS 回收全部资源，**零泄漏**。

**现网统计**（近 14 天 26 次超时）：

```
  9  browser_screenshot      2  browser_type     1  cmd_run
  5  browser_render          2  browser_start    1  browser_html
  2  output_send__webui      2  browser_click    1  browser_fetch
```

`browser` 插件占 22/26。历史进程（`homed[1063615]`、`homed[2609279]`）必然已累积泄漏。

- [ ] **短期**：日志措辞改为「已放弃等待（插件仍在后台运行，其占用的线程无法回收）」
      —— 消除语义谎言，1 行改动
- [ ] **短期**：排查 `browser` 插件为何频繁 60s 超时（22/26 集中于它）
- [ ] 真正的取消能力需子进程模型（见 11.7）

### 11.3 stage 副本模型的 lost update ⚠️ 现网数据污染（量级百分之几）

**核心事实更正**：外部 `.so` 插件**从未共享过 `StageContext`**，一直是「快照-副本-写回」：

```
内核 sc.RLock() → 快照 10 字段为 JSON → 跨 ABI
  → go_invoke_stage: sc := &sdk.StageContext{}   ← 插件进程内全新对象
  → handler 改副本（其 ctx.Lock() 是空操作，无跨插件互斥）
  → stageContextWritable → Marshal 回传
  → applyStageResult: sc.Lock() 逐字段写回
```

这是**为方便插件加载的无奈之举**（C ABI 无法传 Go 对象引用），但带来三个后果：

1. **`ctx.Lock()` 是空操作** —— 插件按文档正确加锁，锁语义在 ABI 边界静默失效
2. **字段被裁剪** —— 16 个字段只下发 10 个，`ContextMsgs`/`ReasoningContent`/`TokenUsage`/`Memory`/`Extra`/`Errors` 外部插件永远看不到
3. **lost update** —— read-modify-write 非原子，实验 12 实测丢失率 **36.8%**（内置模型 0%）

**现网触发点**：`AfterToolcall` 上有两个外部插件

| Stage | 注册者 | 风险 |
| --- | --- | --- |
| `AfterToolcall` | **sanitizer**(Global,改写 ToolResults) + **weather**(own_tools,只读) | ⚠️ 真实冲突 |
| `PreAction` | memo(外部) + webui(内置) | ⚡ |
| `BeforeToolcall` | qq(外部) + webui(内置) + cmd(内置) | ⚡ |

根因在 `templates.go:762` —— **无条件回传未修改字段**：

```go
if len(sc.ToolResults) > 0 {
    m["tool_results"] = sc.ToolResults    // weather 没改也回传它收到的旧快照
}
```

**实验 13 复刻现网场景**（模型调用 `weather_query`，3000 轮）：

```
47 轮 sanitizer 的清洗结果被 weather 的旧快照覆盖 (1.6%)
→ 脏数据（ANSI 转义）进入 LLM 上下文
```

⚠️ **该比率不是常数**：三次复跑得 1.6% / 2.1% / 4.3%，取决于两插件 handler 的
实际耗时比。**应表述为「量级百分之几」**，不要把 1.6% 当精确值写进代码注释或对外说明。

⚠️ **修复方向的红线**：不要通过"把 `RunStage` 改成串行"来消除冲突。
并发扇出是原始设计（见 11.0-pre 第 1 条），串行化会改变所有 stage 插件的时序语义，
且掩盖真正的根因（副本模型 + 无条件回传）。正确做法是让回传只带真正变更的字段。

现网条件已确认：sanitizer v0.1.0 / weather v1.0.0 均 8/15 部署、`disabled_plugins` 为空、
日志有 `[sanitizer] stage OnInput/AfterToolcall/PostAction registered`。

**修复**：`stageContextWritable` 只回传**真正变更**的字段

```go
before := stageContextWritable(sc)
if err := h(sc); err != nil { ... }
diff := changedFieldsOnly(before, stageContextWritable(sc))
```

- [x] 实现 diff 回传 —— SDK 仓 `templates.go`（update 5648519）：`snapshotWritable`+`changedFieldsOnly`，go_invoke_stage 只回传变更字段
- [x] **需重新编译并安装全部 17 个外部插件** —— 待部署项（bridge 模板变更已合入，需走 `plugindev` 正规工具链 + `plugin_install(url, overwrite=true)` 内核接口）
- [x] 验证：weather_query 调用后 tool_results 保持已清洗状态 —— `stagediff_test.go::TestChangedFieldsOnly_ProductionScenarioNoOverwrite`（复刻实验 13 现网场景：sanitizer 清洗 + weather 只读，清洗结果不再被覆盖）；内核配套 `TestApplyStageResult_*`

### 11.4 Lua stage 快照缺读锁（DATA RACE）

`lua_plugin.go:726` 直接读 `sc.RawMessage` 等字段，**未持 `sc.RLock()`**：

| 路径 | 快照时是否持锁 |
| --- | --- |
| cabi（`loader.go:412`） | ✅ `sc.RLock()` |
| Lua（`lua_plugin.go:726`） | ❌ 无锁 |

`RunStage` 是并发扇出，这与其他 handler 的 `sc.Lock()` 构成数据竞争。

**现网未触发**（无 Lua 插件部署，`find` 无 `main.lua`），但缺陷已存在。

- [ ] 加 `sc.RLock()`/`sc.RUnlock()` 包裹快照构造（约 3 行）

### 11.5 Windows DLL 路径能力严重退化

`dynamic_dll_windows.go:227-245`：

```go
ctxJSON, _ := json.Marshal(map[string]interface{}{
    "raw_message": sc.RawMessage, "user_id": sc.UserID, "phase": string(sc.Phase),
})   // ← 只有 3 个字段
syscall.SyscallN(p.invokeStage, p.handle, ...)
return nil       // ← 无 resultOut，无 applyStageResult
```

| 路径 | 下发字段 | 写回 |
| --- | --- | --- |
| Linux cabi | 10 | ✅ |
| Lua | 10 | ✅ |
| **Windows DLL** | **3** | ❌ **完全没有** |

**后果**：`sanitizer` 类改写型插件在 Windows 上**静默失效**——handler 正常执行、
日志正常打印，修改全部丢弃；且看不到 `llm_text`/`tool_calls`/`tool_results`。

- [ ] 补齐字段下发 + 写回（无 Windows 环境，需借测试机验证）

### 11.6 reload 语义谎言（原始起因）

`plgreload` 对 `.so` 插件报成功但运行旧代码。已实验确证 `dlclose` 对
`DF_1_NODELETE` 是 no-op，且**套任何层数的 C 中间件都绕不过去**
（NODELETE 属于被卸载对象自身的 ELF 属性）。

已评估并**否决**版本化路径方案：技术上可行，但每次重载永久泄漏
**5.8 个线程 + 1.5MB**（30 次实测 +168 线程 / +46MB），对 24/7 常驻进程不可接受。

- [ ] ELF 检测 `DF_1_NODELETE` → 标记插件"不可热重载"（`dynamic_loader_unix.go`）
- [ ] `ReloadOne` 对此类插件返回"需重启 homed"，停止假装成功（`registry.go`）
- [ ] `plugin_install` 返回 `restart_required` 替代误导性的 `reload_required`（`pluginmgr/plugin.go`）

### 11.7 子进程 + 共享内存架构迁移（待决策）

**目标架构**：

```
今天： homed ──dlopen──> plugin.so（cgo bridge 385 行 + 51 个整数 method id）
                         ↑ C 层唯一目的：绕开 Go plugin 包同版本限制

之后： homed ──spawn──> plugin（纯 Go 二进制，零 cgo）
         ├── stdio JSON-RPC   控制面：51 个 case 平移为 method 名
         ├── shm + 偏移        数据面：StageContext 并发改写、二进制零拷贝
         └── eventfd          通知面：事件环 post-and-forget
```

**关键洞察**：C 中间层存在的唯一理由是绕开 Go 原生 `plugin` 包的版本枷锁。
子进程模型下**进程边界本身就是 ABI 边界**，C 层解决的问题消失，C 层自己也就该消失。
可删除 `cabi/` 1096 行 + 每插件 385 行 bridge 模板。

**11 项可行性实验全部通过**（详见评估文档第七章）：

| 验证项 | 结果 |
| --- | --- |
| eventfd 走 netpoller | ✅ 200 等待者仅 +1 线程 |
| 跨进程偏移解引用 | ✅ 父子 mmap 不同基址仍正确 |
| 锁仲裁 RPC 成本 | ✅ 19.4 µs/次 |
| post-and-forget 解耦流式 | ✅ 2218x 加速 |
| 17 子进程常驻开销 | ✅ 29MB RSS（原估 50-70MB） |
| 崩溃隔离 + 退出码信号 | ✅ 退出码 2，EOF 2.5ms 感知 |
| 子进程热重载 | ✅ 同路径替换即生效 |
| **跨进程并发改写 StageContext** | ✅ 5 插件×300 轮零丢失 |
| 持锁进程崩溃自愈 | ✅ 无需 robust mutex，**零 cgo** |
| 二进制零拷贝 | ✅ 18-22x，体积省 100% |
| 工具调用 RPC 延迟 | ✅ p50 19.5 µs |

**工作量约 8-9 周**（6 阶段，详见评估文档第四章）。
双通道共存（按 manifest `entry` 分派 `.so`/`.bin`）使迁移可逐插件推进、随时回退。

**迁移正当性 6 条**：① 热重载 ② 崩溃隔离 ③ 能力断层消除
④ 内置插件解耦 ⑤ 修复 stage lost update ⑥ 修复超时泄漏/output 假成功/Windows 退化

其中 ②③④⑥ 全是 C ABI 前提的直接产物（三套 ABI 实现、cgo 不可抢占、cgo 不可嵌套），
在进程边界下自动消失。

**待决策**：

- [ ] **是否全量迁移？** 若只为热重载，11.6（1 人日）即够；8-9 周投入的理由必须是 ②-⑥
- [x] 跨进程锁选型 → **已裁定**：锁仲裁回内核，零 cgo（实验 3+9）
- [x] `Extra` 处置 → **维持**：4 键提升为具名字段，`Extra` 留 RPC 副本
- [ ] 权限梯度显式形式（manifest 声明 caps？内核白名单？）
      —— 注：内置插件的高权限是**刻意设计**，迁移目标是让梯度从"C ABI 表达能力的
      意外产物"变成"显式声明并强制的策略"，而非消除梯度

### 11.8 实施顺序建议

按「影响 × 成本」排序，前 4 项不依赖迁移决策：

| 序 | 项 | 规模 | 现网影响 |
| --- | --- | --- | --- |
| 1 | **11.1** output_send 同步等结果 | M | ❗ 用户收不到消息且模型以为成功 |
| 2 | **11.3** stageContextWritable diff 回传 | S | ❗ 脏数据进 LLM（量级百分之几） |
| 3 | **11.6** reload 语义修正（3 项） | S | 误导模型白跑重载 |
| 4 | **11.2** 超时日志措辞 + browser 排查 | S | 已泄漏 26 次 |
| 5 | 11.4 Lua 读锁 | S | 潜在 |
| 6 | 11.5 Windows 补齐 | M | 无部署 |
| 7 | 11.7 迁移（待决策） | 8-9 周 | — |

### 11.9 附带发现（独立问题，非本节范围）

测量对照数据时发现 homed 内存异常：

```
homed RSS = 2.34 GB   RssAnon = 2.35 GB（真实驻留）
  2420 MB × 1   ← 主 homed 的 Go heap
   512 MB × 15  ← 15 个插件各自的 heap arena（虚拟预留，不占物理内存）
```

512MB×15 说明当前架构下**插件间内存无法协同回收**（各自独立 Go runtime）。
但 **2.36 GB 真实驻留在主 homed heap 上，与插件无关**，疑似 chat history /
context 累积导致的内存增长。

- [ ] 单独排查 homed 主 heap 的 2.36GB 驻留来源

---

## 12. 子进程化迁移收尾：剩余工作与目标效果

> 状态锚点（2026-09-03）：迁移主体已完成并上生产。内核 **v1.0.0**，
> 生产 17 个外部插件全部经子进程通道运行，15 个子进程稳定存活。
> 分支：主仓 `feature/plugin-proc-migration` @ `12259ed`（领先 main 26），
> SDK 仓 `feature/plugin-proc-migration` @ `5ed8d65`（领先 main 4）。
>
> 三项合入门禁**已全部通过**：`make test` 零失败、`go vet ./...` 无告警、
> `git diff main -- third_party/homeagent-sdk/sdk/` 为空（接口冻结不变量）。
>
> 详细执行记录见 `docs/zh/plugin-migration-plan.md`（Part 0~6 全部标记完成）。

### 12.1 ✅ 已完成：合并到 main + 发布分支（2026-09-03 ~ 09-06）

四个决策点均已落定并执行：

| # | 决策点 | 最终选择 |
| --- | --- | --- |
| 1 | merge 方式 | **`--no-ff`** —— commit message 记录了「为何共享同一块 memfd」「为何 procCore 不能嵌入」等踩坑过程，压成一条就没了 |
| 2 | 合回后是否删 feature 分支 | **删**（`feature/plugin-proc-migration`、`feature/memory-media` 均已删，本地 + 远端） |
| 3 | release 构建是否再替换生产二进制 | **换**，且此后每个正式版都走同一流程（备份二进制 + `sqlite3 .backup` 配置库 + 记插件清单 → `install -m 0755` → restart → 健康检查） |
| 4 | SDK 仓是否同步 main + release | **同步**，且已升级为规范条款（`docs/git-branching.md` §七） |

**tag 归属问题已修**：`v1.0.0` 曾指向 feature 分支中间点 `670efcd`，已删除重打在 `release/v1.0.x` 上（`9b92a04`）。

**实际演进已超出本节当初的设想**，后续发生的事写进了 `docs/git-branching.md`：

- 发布分支改为**一个中版本一条**（`release/v1.0.x` 承载 1.0.0/1.0.1/1.0.3/1.0.4，而非按 patch 号各开一条）；
- 三级发布通道 alpha/beta/正式**由 tag 区分而非分支**；
- SDK 版本号**跟随核心的中版本、patch 位恒为 `.0`**（整条核心 1.1.x 线共用 SDK 1.1.0）——
  所以「两仓版本对齐」指**中版本对齐**，不是三位全等；
- **beta 阶段不发 SDK**：接口未固定时发版会让插件开发者照着会变的接口写代码；
- **main 永不作发版分支**，版本号 bump / 打 tag / 构建产物只在发布分支上做。

已发布：`v1.0.0` / `v1.0.1` / `v1.0.3` / `v1.0.4`（1.0.x 线）、`v1.1.0` / `v1.1.0-beta.1` / `v1.1.1`（1.1.x 线），
SDK 仓 `v1.0.0` / `v1.1.0`。main 的版本路牌现为 `1.2.0`（尚无 tag）。

```

---

### 12.2 验收清单里两项**未达成**的目标

这两项在 `docs/zh/plugin-migration-plan.md` 的最终验收清单里如实标了 ⚠️，
不是遗漏而是明确的未兑现承诺。

#### 12.2.1 `SetToolBlocks` 仍是未实现（承诺未兑现）

- **现状**：`io.setToolBlocks` 已在 `proc/protocol.go` 定义、已划入 `CapCore`
  能力组，但 `corehandler.go` 的 handler 仍返回未实现。
- **为何不算回归**：C ABI 时代它也是空实现（§1.4 / `case` 无对应逻辑），
  能力从「给不了」变成「暂未接」，没变差。
- **但 §3.8 承诺过**：迁移评估明确写「`SetToolBlocks` → 二进制写入 arena，
  返回 `Slice` 描述符 ✅」。这条没做到。
- **目标效果**：插件调用 `SetToolBlocks(blocks)` 后，多模态内容块经共享段
  arena 传给内核，内核把它并入工具返回值；`Slice` 描述符回传避免拷贝。
- **当前无用户**：17 个外部插件均未调用，故不阻塞发布。
- [ ] 实现 `io.setToolBlocks` 的内核侧 handler（arena 写入 + Slice 回传）
- [ ] 补一个真实使用它的 example 插件，否则无法验证

#### 12.2.2 内存开销超出计划目标（结构性问题）

- **计划目标**：迁移后常驻 ≤ 基线 +29MB（实验 5 量级）。
- **实测**：15 个插件进程 `RSS=88.0MB` / `PSS=87.9MB`，均摊 5.87MB。
- **根因**：每插件静态链接整个 Go runtime。15 个**不同**二进制之间无共同
  物理页可映射，`PSS/RSS = 99.9%`（基线是 44%——那次用同一个 2.68MB 最小
  插件复制 17 份，页可共享）。
- **绝对数字不可比**：基线插件 2.68MB，真实插件 3.1~14.8MB（browser 最大）。
  结构性指标（均摊线程 5.5 vs 4.9）同量级。
- **实际开销高于 §4.3 乐观估计**，这是「每插件独立二进制」的固有代价。
- **目标效果（若要压）**：共享一个 launcher 二进制 + 各插件只提供业务模块，
  让 15 个进程映射同一份 runtime 物理页，把 PSS 压回 RSS 的一半以下。
  代价是插件不再是自包含可执行文件，分发与版本管理都变复杂。
- [ ] 决定是否值得为此改变分发模型（当前倾向：不改，88MB 可接受）

---

### 12.3 事件环：机制完成但**零真实负载检验**

- **已完成**：内核侧 `proc/evtring.go`（写端 + 消费端 + 事件类型位编码）、
  `internal/plugin/evtring.go`（Bus ↔ EvtRing 适配）、模板侧 `evtConsumerLoop`、
  三平台通知机制（Linux eventfd / macOS pipe / Windows Event）。
- **压测通过**：5000 次 Publish + 20µs 慢消费者 = 2.29ms（与实验 4 一致）；
  订阅者 1→8 耗时不变；环溢出仍 O(1)。
- **但**：`grep` 确认**无任何外部插件使用 `Events().Subscribe`**。
  压测是我构造的负载，生产上这条路径从未被真实插件走过。
- **目标效果**：至少一个真实插件订阅内核事件并正确处理，
  验证「独立游标 + 溢出跳过 + dropped 计数」在真实时序下的行为。
- [ ] 写一个订阅 `stage`/`tool_call` 事件的 example 插件做真实验证
- [ ] 观察长时间运行下 `dropped` 计数是否异常增长

---

### 12.4 三套 ABI 只收敛了两套：Lua 仍独立

- **已收敛**：C ABI（删除）+ Windows DLL（改走同一 RPC）。
- **未收敛**：`internal/plugin/lua_plugin.go` / `dynamic_lua.go` 仍走
  gopher-lua 解释器的独立路径。
- **为何不阻塞本轮**：Lua 经解释器不经 C ABI，不属于本轮要消除的 6 类缺陷
  （热重载失效、崩溃隔离缺失、stage lost update、cgo 超时泄漏、
  output_send 假成功、能力断层）。§9.2 的「三套 ABI 收敛为单一 RPC」
  这句话本轮只兑现了 2/3。
- **目标效果**：Lua 插件也走 `proc` 通道（launcher 进程内嵌解释器），
  内核侧只有一套加载逻辑与一套权限检查。
- **收益**：Lua 插件获得崩溃隔离与共享内存 stage 全字段可见；
  内核侧删掉 `lua_plugin.go` 的平行实现。
- [ ] 评估 Lua 走 proc 通道的代价（解释器进程启动开销 vs 隔离收益）

---

### 12.5 Windows 只做了交叉编译，无真机验证

- **已完成**：`shmalloc_windows.go`（`CreateFileMappingW` + `MapViewOfFile`）、
  `evtfd_windows.go`（`CreateEventW` + `SetEvent`）、`shmpass_windows.go`
  （名字经环境变量传递）、插件侧 `proc_shm_windows.go.tmpl`
  （`syscall.NewLazyDLL` 绑定 `OpenFileMappingW`/`OpenEventW`）。
- **验证程度**：仅 `GOOS=windows GOARCH=amd64 go build` 通过 + 单元测试。
  **无 Windows 测试机，从未真机跑过**。
  v1.0.0 起 Windows NSIS 安装器（`HomeAgent_v*_{Full,Server,Client}_win64.exe`）已作为
  release 资产随每个正式版发布 —— 但那只证明**能打出包**，不证明包装出来的
  共享内存/事件对象在真机上能跑通。这两件事不要混为一谈。
- **已知的语义差异**（代码注释里记了，但未实测）：
  Windows Event 是二元信号而非计数器，多次 `SetEvent` 只唤醒一次。
  推理上不影响正确性（消费者按 `readSeq` 追 `writeSeq` 批量 drain），
  但没在真机确认过。
- **目标效果**：Windows 真机上完成一次完整的插件加载 → 工具调用 →
  stage 改写 → 事件消费闭环，确认 16 字段全可见且写回生效
  （这是 §9.2 声称 Windows「从受害者变受益方」的实证）。
- [ ] 找一台 Windows 机器跑端到端验证
- [ ] 特别验证命名对象的撞名防护（名字带 PID + 递增序号）

---

### 12.6 性能优化候选：stage 往返省两次 IPC

- **实测**：完整 stage 往返 132µs，其中共享段编解码只占 3.7µs（2.8%）。
- **成本构成**：一次 stage 要走 **3 次进程间往返**——`stage.invoke`
  加上插件侧反向的 `stage.lock` / `stage.unlock`。
- **相对 LLM 往返 2-8 秒可忽略**，故非紧急。
- **目标效果**：把 lock/unlock 合入 `stage.invoke` 的请求/应答
  （内核在下发 invoke 前就代插件持锁，应答时释放），
  stage 往返从 3 次 IPC 降到 1 次，预期 132µs → ~30µs。
- **风险**：改变锁的持有时机。当前是插件主动请求，
  改后内核代持——插件若在 handler 里再次请求锁会死锁，需要额外防护。
- [ ] 评估锁语义变化的影响面（哪些插件依赖显式 lock 时机）

---

### 12.7 无关本次迁移的遗留项

- [ ] 单独排查 homed 主 heap 的 2.36GB 驻留来源（见 §11.9，与插件无关）
- [ ] `cmd/ohos/.../SettingsPage.ets` 有 80 行未提交的鸿蒙端改动
      （非本次迁移内容，一直未碰）

---

### 12.8 本次迁移**已达成**的目标（对照 §11.0 起因）

留档备查——6 类 C ABI 前提缺陷的消除状态：

| 缺陷 | 原状 | 现状 | 证据 |
|---|---|---|---|
| 热重载失效（11.6） | `DF_1_NODELETE` 让 `dlclose` 成 no-op | ✅ 换 `plugin.bin` 即生效 | 生产实测 `unloaded (config kept)` → 重载 |
| 崩溃隔离缺失 | 插件 panic 带崩 homed | ✅ 子进程独立崩溃 | `TestRealPlugin_CrashDoesNotKillKernel` |
| stage lost update（11.3） | 副本模型互相覆盖 35.8~36.8% | ✅ 0% | `TestPlugin_FiveProcessesConcurrentAppendNoLostUpdate` |
| cgo 超时泄漏（11.2） | 现网泄漏 26 次 | ✅ 整套新架构零 cgo | `Process.Kill()` 真取消 |
| output_send 假成功（11.1） | 永远返回成功 | ✅ 真实结果 | 生产实测 `map[status:sent]` |
| 能力断层（11.5 + §3.8） | Windows 只见 3 字段、无写回 | ✅ 18 字段全可见可写回 | 生产实测 sanitizer 跨进程改写 13590 字节 |

额外收益：权限梯度从「C ABI 表达能力的意外产物」变成**显式三道闸**
（类型层 `procCore` 命名字段 + manifest 能力声明 + RPC 边界明确拒绝）。

---

## 13. 子进程化后遗留改造（逐步推进）

每个步骤独立提交、独立验证，遵循「小步 → 测试 → 提交」循环。

### 步骤分组

| 步骤 | 内容 | 依赖 | 产出 |
|---|---|---|---|
| 13.1 | 统一共享内存布局 | 无 | 单一 memfd，StageContext+EvtRing 成为区内 segment |
| 13.2 | 共享内存分配器 | 13.1 | 内核独占的变长块分配器，插件经 RPC 申请/归还 |
| 13.3 | 工具调用调用帧 | 13.1/13.2 | payload 始终走共享内存，内核标定帧，插件按需扩容 |
| 13.4 | Cleaner 迁移至 SharedRef | 13.1 | cleaner.invoke 参数/结果走 SharedRef |
| 13.5 | InputChannel lane | 13.1 | 输入通道消息走共享内存 |
| 13.6 | OutputChannel lane | 13.1 | 输出通道消息走共享内存 |
| 13.7 | RuntimeManager + 分组 worker | 13.1~13.6 | 一个 RuntimeManager，少量 worker，多插件共享 transport |
| 13.8 | ContextPolicy tool 上下文策略 | 无 | ToolDef.ContextPolicy = none/prune |
| 13.9 | llmsproxy 上下文溢出感知 | 无 | 溢出错误归一化 + AUTO 截断放宽 |
| 13.10 | AgentMail 三个 bug | 无 | 提示词修正 / 回信标识 / relay_key 限长 |
| 13.11 | WebUI 修复全清单 | 无 | 11 项逐步推进 |
| 13.12 | L3 原生多模态 | 13.1~13.7 | 媒体作为图节点/边，L2→L3 引用迁移 |

### 13.1 统一共享内存布局

**现状**：两块独立 memfd（StageContext 256KB + EvtRing ~320KB），fd 3/4，eventfd 占 fd 5。
**目标**：合并为单一 memfd 占 fd 3；eventfd 占 fd 4。段内偏移表定位各 segment。

```text
┌─────────────────────────────────────────┐
│ Unified Shared Region（单 memfd）       │
├─────────────────────────────────────────┤
│ [SuperBlock 64B] magic/version/cap/gen  │
│ [StageContext segment]  布局不变         │
│ [EvtRing segment]      布局不变         │
│ [Reserved: ToolCall]   后续步骤填充     │
│ [Reserved: InputCh]    后续步骤填充     │
│ [Reserved: OutputCh]   后续步骤填充     │
│ [Dynamic Arena]        自由分配区       │
└─────────────────────────────────────────┘
```

**实施**：

1. 定义 SuperBlock 布局（magic/version/segment 偏移表）
2. NewHost() 一次 allocShm，内含两段
3. Segment + EvtRing 从 SuperBlock 读偏移
4. fd 传递从 3 个降为 2 个
5. procExtraFilesForShm 返回 2 个 fd
6. 插件侧模板解析 SuperBlock，自行定位两段
7. 所有现有测试不变
8. go test -race ./internal/plugin/proc/

**验证**：

- [x] SuperBlock 写入读回一致
- [x] StageContext 并发改写 0 lost update
- [x] 事件环 post-and-forget 仍工作
- [x] TestPlugin_ConcurrentWriterAndReaderNoLostUpdate 通过
- [x] fd 数从 3 降到 2（`procExtraFilesForShm` 只返回 memfd + evtfd）
- [x] git commit -m "feat(shm): unified shared memory region"（ad016e4）

**顺手清理**：删除 §13.1 后遗留的死代码 `allocEvtRing`（从未被调用，
统一区域后只有操作区内切片的 `NewEvtRing` 仍在使用），并修正
`plugin.go` 里仍写着旧 3-fd 布局（3=StageContext, 4=事件环, 5=通知）
的过时注释。

### 13.2 内核独占的共享内存分配器

**设计约束（用户明确）**：

> 内核应当全权管理共享内存，插件需要共享内存要向内核申请，内核给插件返回偏移与大小，使用完成后插件通知内核回收。
> 内核暴露类似 syscall 的 RPC 接口；共享内存是内部实现，不对插件开发者暴露。

**为什么不是跨进程分配器**（前几版都被推翻）：

- v1：SuperBlock 放 `arenaUsed` 游标，内核 CAS bump。但**插件模板里的 `arenaUsed` 是进程本地变量**，两个进程各自 bump，必然写到同一段内存；`arenaReset` 还会重置共享游标覆盖对方数据。
- v2：把位图 CAS 下沉到插件模板。虽然正确，但把分配器实现细节泄漏进了插件运行时，且插件必须与内核保持位图布局同步。
- v3：定长槽 + 共享位图 CAS。正确，但定长槽唯一的理由是“跨进程没法安全做变长分配”。
- v4（当前）：分配器收回内核进程后那个约束消失，改成**变长块分配器**（first-fit + 邻块合并）。内核可以按需**标定**每块大小，大 payload 不再受固定槽容量限制。

**实施**：

1. `arena.go`：块头 16B（`size/state/owner/prevSize`），`Alloc/Put/Read/Free/ReclaimOwner`，内核独占一把 `sync.Mutex`。`prevSize` 让 `Free` 能 O(1) 找到前驱做向后合并。
2. 块头记录 `owner`；`Free` 校验归属 + 走块链确认 offset 是已分配块的数据起点，**伪造引用不能改动分配器状态**。
3. `Read` 允许块内偏移（调用帧的结果区就在帧块中间），但要求不跨越块边界。
4. 协议新增 `arena.alloc` / `arena.free`（`CapCore`，属基础能力）。
5. `Plugin` 在 `Start` 领取 ownerID；`handleExit` 调 `ReclaimOwner` 回收残留块，防崩溃把 arena 耗尽。
6. 插件侧不写任何分配器状态：模板只通过 RPC 申请/归还；SDK 公开 API 仍是普通字符串/Map，开发者无感。
7. arena 容量 4MB，但底层是 memfd：**未触碰的页不占物理内存**，所以开大无成本。

**验证**：

- [x] `TestArena_*`：分配/归还/归属校验/回收/并发唯一/耗尽/超限/非法引用/伪造 offset/相邻合并/对齐/布局校验
- [x] `TestPlugin_ArenaAllocFreeAcrossProcess`：真进程申请→写入→随业务 RPC 回传→归还，内核读回内容一致且 arena 归零
- [ ] **Grow/Shrink 未实现**：跨进程 remap 会让正在读的对端 SIGSEGV。当前容量固定，用尽时调用失败（不再退回内联）。
- [x] git commit -m "refactor(shm): kernel-owned variable-size arena"

### 13.3 工具调用走 funccall 调用帧

**目标**：工具调用的 payload **始终**在共享内存里；内核作为 caller 标定内存块交给插件。

**模型（用户明确）**：

> 工具调用的 payload 应当始终在共享内存中。因为工具调用是内核发出的，按 funccall 方式，内存块应当由内核标定后交给子进程。当内核提前给的不够用时，插件侧才请求扩容。

调用帧布局：

```text
[0, ArgsLen)              参数 JSON
[ArgsLen, Frame.Length)   结果区（内核预留的预算）
```

**为什么不用 ring 状态机**：初版计划用 FREE→WRITING→READY→READING→DONE 的 ring 把 `tool.invoke` 整个搬出 RPC。写完发现是错的方向——RPC 已经提供请求 ID 关联、错误传递、ctx 取消、崩溃唤醒（`Process.CallContext`），ring 只是把它们重新实现一遍，且从未接线（死代码，已删）。

**实施**：

1. 内核 `invokeTool`：序列化参数 → `Alloc(len(args)+toolResultBudget)` → 参数写帧前段 → 发 `ToolInvokeParams{Name, Frame, ArgsLen}`。
2. 插件：从帧读参数；结果优先写帧的结果区。
3. 结果超出预算 → 插件 `arena.alloc` 扩容块，引用上打 `sharedRefFlagExpand`；内核据此单独归还。
4. **没有按大小切换内联的分支**：小 payload 同样走帧。
5. Cleaner 复用同一帧模型（`Frame` + `InputLen`）。
6. `ToolInvokeParams.Args` / `ToolInvokeResult.Result` 仅剩给**直连 RPC 的测试**（process/bench 不建 Host，拿不到共享内存）；生产路径永远走帧。

**验证**：

- [x] `TestPlugin_ToolInvokeArgsResultViaArena`：大/小 payload 都经帧往返，结果内容一致且 arena 归零
- [x] `TestE2E_RealTemplatePluginFullLifecycle`：真实 SDK 模板编译的插件跑通
- [x] `TestProcTemplate_ToolInvokeUsesSharedRef`：模板必须处理 frame/args_len/result_ref（防漂移）
- [x] Bench: ToolInvoke 延迟对比（inline vs frame，见 `bench_test.go`）

### 13.4 Cleaner 迁移至 SharedRef

**目标**：cleaner.invoke 参数/结果走 SharedRef。

**实施**：

1. CleanerInvokeParams 的 Text 改为 SharedRef
2. 插件从 SharedRef 读文本、写结果
3. 内核从 SharedRef 读结果
4. RPC 帧从 ~200B 降到 ~16B

**验证**：

- [x] 三类 Cleaner 结果正确（testdata/stageplugin.go 覆盖 tool/input/output 三类 scope）
- [x] git commit -m "feat(shm): cleaner invoke via shared refs"（5608abd）

**顺带**：§13.4 的第六项（工具调用帧）同时把每批工具调用的性质写入
`lastBatchReplyOnly`，供工具循环选择补位文案（见 `process.go`）。

### 13.5 InputChannel lane

**目标**：输入通道消息走共享内存。

**现状（核实）**：基础设施就位，但内核侧未接线——

- `injectParams.TextRef SharedRef` 与 `resolveText`（corehandler.go:585）
  在插件→内核方向可用，但内核→插件方向的 `procCore.InjectText` 仍直接传
  字符串给 `sdk.InjectText`，从不 `Alloc/Put` 构造 `TextRef`。
- 即只做了半边（插件回传文本），内核注入还没走共享内存。

**实施**：

1. InputChannel Slot：source/channel/text/blocks 写入 arena
2. 插件消费后设 DONE
3. 同步注入仍走 RPC + SharedRef  ← 需要接线内核侧
4. 异步注入改写 arena + eventfd

**验证**：

- [ ] 真实 QQ 消息注入测试
- [ ] git commit -m "feat(shm): input channel lane"  ← 未提交，内核侧待接线

### 13.6 OutputChannel lane

**目标**：输出通道消息走共享内存。

**实施**（已完成）：

1. `OutputInvokeParams` 加 `Frame SharedRef` + `ArgsLen`（`Args` 仅留给直连
   RPC 的测试），与 `ToolInvokeParams` 同一 funccall 帧模型
2. `invokeOutput` Alloc 帧、写 payload JSON、只传偏移描述符；插件退出/调用完
   后内核归还整帧（`defer arena.Free`）
3. 帧尾不预留结果区：output 应答很小（`"ok"` / status map），直接走 RPC
   应答字段；若插件把大结果写回帧（`OutputInvokeResult.ResultRef`）也能读回，
   并识别 `sharedRefFlagExpand` 单独归还扩容块
4. 模板 `output.invoke` 从帧读参数（`frameInput`），无帧才回退内联 `Args`

**验证**：

- [x] `TestPlugin_OutputPayloadViaArena`：9000 字节 payload 经帧完整送达（插件
  回报实收长度）+ 调用后 arena 归零
- [x] `TestE2E_RealTemplateOutputPayloadViaFrame`：同上但用**真实 SDK 模板**
  编译的插件（生产插件走的就是模板，模板不读帧该改动就等于没做）
- [x] `TestPlugin_OutputChannelReportsRealFailure` 仍绿：同步等真实结果、
  失败必须上报（§9.4）不被破坏
- [ ] QQ 输出正常（需生产部署后验证）
- [x] git commit -m "feat(shm): output channel lane"

### 13.7 RuntimeManager + 分组 worker

**目标**：一个 RuntimeManager + 少量 worker + 多插件共享 transport + 每插件独立 PluginContext。

**实施**：

1. RuntimeManager 类型：管理 worker 池 + 调度
2. Worker 类型：一个进程，共享 RuntimeClient
3. PluginContext 类型：独立身份，共享 transport
4. manifest 新增 worker_group 字段
5. 默认所有 proc 插件归同一 worker（兼容迁移）
6. 高风险插件可声明独立 worker_group

**验证**：

- [ ] 默认分组 = 现有行为
- [ ] git commit -m "feat(runtime): RuntimeManager with grouped workers"

### 13.8 ContextPolicy tool 上下文策略

**目标**：ToolDef.ContextPolicy = none/prune。

**实施**：

1. SDK ToolDef 加 ContextPolicy string 字段
2. StageAfterToolcall 检查当前 tool 的 ContextPolicy
3. prune 时执行 RelevanceContext.Prune
4. 默认 none

**验证**：

- [x] qq_get_message 加 prune 后上下文精简（QQ 插件已声明 `ContextPolicy: "prune"`）
- [x] git commit -m "feat(ctx): context policy for tool results"（772a494）

### 13.9 llmsproxy 上下文溢出感知

**实施**：

1. OVERFLOW_PATTERNS 补充 "Context window is full"
2. AUTO 截断宽度 80→160

**验证**：

- [ ] go test ./internal/ai/...
- [ ] git commit -m "fix(ai): context overflow pattern + truncation width"

### 13.10 AgentMail 三个 bug

**实施**：

1. 提示词修正
2. InReplyTo 字段
3. relay_key ≤ 64 字节

**验证**：

- [ ] Agent→Agent 不再套话 6 轮
- [ ] git commit -m "fix(agentmail): prompt / in-reply-to / relay key"

### 13.11 WebUI 修复全清单

11 项，每项独立提交：
SSE Last-Event-ID → 超时 → api 状态码 → renderAll 增量 → XSS 消毒 → CSS → DesignSystem → handleAgents 持久化 → handleKnowledge 吞错 → GUI 重构

### 13.12 L3 原生多模态

**实施**：

1. L3 node_type 新增 media/media_block
2. L3 edge_type 新增 depicts/contains
3. L2→L3 迁移时保留 media 引用边

**验证**：

- [ ] 图查询返回媒体节点
- [ ] git commit -m "feat(l3): native multimodal nodes/edges"
