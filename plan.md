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
|------|------|------|----------|
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
- [ ] 部署验证：编译新 `homed` 部署后，`knowledge/`、`memory/graph.db`、`memory/documents/` 不再出现 `_hc_*` 残留。

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
|---|------|------|------|
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
- [ ] 生产部署后确认：graph.db 36→ 去重（2 组 `like/plugin` 重复消失），跑 1 周不再新增重复。

> 注：entities 已有 `UNIQUE(name)` 保护，仅 relations 缺失。

---

### Phase 2：归档三元组模板清理（治本）✅ **计划中**

**目标**：`docToTriples` 不再把 `context_archived`/`Topic` 摘要当成实体写入图库。

- [ ] 重构 `docToTriples`：仅当 `doc.Source` 非 `context_archived` 且非空时写 `文档→来源`；`文档→主题` 仅当 summary 长度合理（<80 字）且非模板化时写，否则跳过。
- [ ] 引入 `doc.Meta["is_archived_context"]` 标记上下文归档文档，供 `docToTriples` 识别并跳过。
- [ ] 单测验证：构造冷文档 → `archiveColdDocs` → 无模板垃圾产出。

---

### Phase 3：配置持久化解析修复（防配置失效）✅ **计划中**

**目标**：支持 `2d`/`1w` 等人类可读时长单位，配置即时生效。

- [x] 实现 `parseDurationExtended(string) time.Duration`：正则识别 `\d+[dhw]` → 换算为 `time.Hour*24` 等，再调 `time.ParseDuration`。
- [x] 替换 `GetDuration` 加载点（`registry.go` `GetDuration` 共用），`main.go:423-426` 的四个间隔配置自动受益。
- [x] 单测：`TestParseDurationExtended`（`"2d"==48h`、`"1w"==168h`、`2d12h`、复合/无单位、错误输入）+ `GetDuration` 集成用例全绿。
- [x] 生产核实：当前生产 `core.agent.distill_interval=30m`（可解析，非失效态）；修复为防御性，未来 `2d`/`1w` 写入即可生效。

---

### Phase 4：Pipeline Distiller 行为对齐文档（可选，低优）✅ **计划中**

- [ ] 改为真正的增量蒸馏：每 tick 取最近 `RetentionDays` 内、未蒸馏记录 → `extractKeyTriples` → `Commit`，标记 `Distilled=true`。
- [ ] 移除 `CreatedAt.Before(cutoff)` 的 7 天门槛，改为"每 tick 处理前 N 条"，保持文档所述 10min 频率。
- [ ] 单测验证启动即蒸馏 + 不重复蒸馏。

---

### Phase 5：嵌入模型内存优化（运维侧）✅ **计划中**

- [ ] 提供 **量化/裁剪** 选项：`embedding_model_path` 支持 `top50k` 等规格，或运行时 `mmap` 只加载词表需求词。
- [ ] 文档补充内存预算说明：双模 300 维 ≈ 1.5G RAM/模型。
- [ ] 生产可选降级：仅保留中文模型（主语言）。

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
- [ ] 单测：终端持续吐进度时，消息注入频率显著低于 500ms/条；进程结束/出错/提示符时立即通知。
- [ ] 运维止血：杀掉残留 `term_3` bash（PID 3716282），验证 QQ 消息恢复响应。

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
|------|------|------|----------|
| Graph relations 重复率 | ~90% (5613/6225) | 0% | `SELECT count(*), count(DISTINCT source_id||target_id||relation_type) FROM relations` |
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
|------|-------------|-------------------|
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
|------|-----------------------------|
| 单页面堆砌所有功能 | **左侧可折叠侧边栏**（16rem 固定）→ 导航分组：概览/记忆/工具/插件/配置/日志 |
| 无面包屑、无状态反馈 | 顶部面包屑 + 悬停微动效；关键操作 Toast 反馈（右上角） |
| 表格/列表无视觉分组 | 卡片网格布局：每卡片 = 一个功能模块（记忆统计/插件状态/工具调用/系统资源） |
| 无数据可视化 | Canvas 2D 绘制：记忆增长趋势图、CPU/内存环图、工具调用热力图 |

### 交互体验对标
| 场景 | NapCat 做法 | HomeAgent 改进 |
|------|-------------|----------------|
| 卡片悬停 | 3D 透视倾斜 + 光标跟随渐变光斑 | CSS `transform: perspective(1000px) rotateX/Y(±5deg)` + 伪元素光斑跟随鼠标 |
| 按钮点击 | 弹簧 scale + loading 态 | `:active { transform: scale(0.97) }` + 内置 spinner |
| 页面切换 | Framer Motion fade-up+scale stagger | CSS `@keyframes fadeUpScale` + JS 交错延迟 50ms |
| 空状态 | 插画 + 友好文案 + 引导 | 每模块空状态统一组件：图标 + 说明 + 主操作按钮 |
| 错误处理 | Toast 右上 + 破坏性操作确认弹窗 | 统一 `showToast(type, msg)` + `confirmDialog(action, onConfirm)` |

### 实施路线（非阻塞，Phase 8+）
```
Phase 8.1: CSS 变量系统 + Glassmorphism 基础样式（浅/深色）
Phase 8.2: 布局重构 — 侧边栏 + 面包屑 + 卡片网格响应式
Phase 8.3: 核心页面卡片化 — 概览/记忆/插件/工具/配置/日志
Phase 8.4: 交互微动效 — 3D 倾斜卡片、弹簧按钮、Toast、Loading
Phase 8.5: 数据可视化 — Canvas 记忆趋势/资源环图/工具热力图
Phase 8.6: 空状态/错误/确认弹窗统一组件库
Phase 8.7: 无障碍/键盘导航/移动端适配
```

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