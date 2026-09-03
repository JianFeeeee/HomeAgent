# 外部插件接口不变矩阵（多进程化整改基线）

> 状态：**完成 v2**（2026-09-03）——迁移已落地并上生产，内核 v1.0.0。
> 目的：钉死「暴露给外部插件的接口不变」这一约束的**合同面**——迁移前、迁移后外部插件看到/调用的 SDK 接口完全一致；
> 所有改造落在**核心（homed 侧）+ 工具链（plugindev）**，外部插件业务代码零改动，只需用新 plugindev 重编。
>
> **结果（已验证）**：`git diff third_party/homeagent-sdk/sdk/` 全程为空；17 个 `example/*/plugin.go` 逐字节未改
> （`git status example/` 无输出）；生产 17 插件全部经子进程通道运行。
>
> 维护规则：每次改动公开 SDK 接口面 `third_party/homeagent-sdk/sdk/` 或模板 `tools/plugindev/templates/` 后，
> 必须同步更新本矩阵。
>
> 权威编号：plan.md 第 11 节（11.1~11.9）。本文档只做接口面盘点，不做实现。

---

## 一、迁移的形状（一句话）

```
今天：  外部插件 = example/*/plugin.go（纯 Go） ──plugindev c-shared──> plugin.so
        homed ──dlopen──> plugin.so（C ABI bridge：51 个整数 method id）
之后：  外部插件 = example/*/plugin.go（纯 Go，一行不改） ──plugindev go build──> plugin.bin
        homed ──spawn──> plugin.bin（stdio JSON-RPC + shm + eventfd）
```

**为什么接口可以不变**（已代码核实）：

| 层 | 含 cgo？ | 迁移后动作 |
|---|---|---|
| 公开 SDK `third_party/homeagent-sdk/sdk/*.go` | ❌ 纯 Go | **不动**（接口面 = 合同） |
| 外部插件业务代码 `example/*/plugin.go` | ❌ 纯 Go（只 import 公开 SDK） | **不动**（只重编） |
| bridge 模板 `tools/plugindev/templates.go` 的 `tmplLinuxBridge`/`tmplBridge` | ✅ cgo | **删除/替换**为 `tmplProcMain` |
| `plugindev` 构建命令 | c-shared | 改普通 `go build` |
| homed `internal/plugin/cabi/`（1096 行） | cgo | 删（已归入 plan 迁移收尾 5.2） |
| homed `internal/plugin/registry.go` 加载分派 | — | 改：按 `entry` 分派 `.so`/`.bin` |

---

## 二、合同面 A：公开 SDK 类型与接口（迁移前后必完全一致）

文件：`third_party/homeagent-sdk/sdk/{plugin.go,memory.go,knowledge.go,llm.go,settings.go}`

### A1. 插件入口契约（Plugin 接口）

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
// 外部插件实现 NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error)
```

### A2. 插件可注册的 5 类组件（PluginSDK 方法）

| PluginSDK 方法 | 签名 | 外部插件使用量（example 实测） |
|---|---|---|
| `RegisterTool` | `(name string, def ToolDef, handler ToolHandler) error` | **86** |
| `RegisterStage` | `(stage Stage, handler StageHandler, scope ...StageScope)` | 6 |
| `RegisterOutputChannel` | `(name string, caps int, desc string, def ChannelDef, handler ToolHandler) error` | 4 |
| `RegisterInputChannel` | `(name string, def ChannelDef) error` | 2 |
| `RegisterPluginAPI` | `(name string) error` | 0（定义存在，可用） |

### A3. 插件可调用的能力访问器（PluginSDK 方法）

| 访问器 | 返回 | 外部插件使用量 |
|---|---|---|
| `Settings()` | `SettingsAPI` | **17 插件全部使用**（Get/Set/List/GetCore/SetCore/ListCore/DataDir/GetPlugin/SetPlugin/ListPlugin/RegisterDef/Defs/Dump/Plugins） |
| `Memory()` | `MemoryAPI`（Recall/Commit/Introspect/MergeEntities/Purge） | 低（controllable） |
| `DocMemory()` | `DocMemoryAPI`（Query/Insert/Remove/Stats） | 低 |
| `TextMemory()` | `TextMemoryAPI`（Append） | 0 当前 |
| `Knowledge()` | `KnowledgeAPI`（Search/Add/List） | 2 |
| `LLM()` | `LLMAPI`（ListSources/SetSource/CurrentSource） | 0 当前 |
| `Social()` | `SocialAPI`（**只读**：GetPerson/GetTrait/GetRelations/GetNetwork/ListPersons） | 0 当前 |
| `Events()` | `EventSubscriber`（Subscribe） | 0 当前（**C ABI 空实现**，迁移后可获得） |
| `PluginMgr()` | `PluginMgrAPI`（ReloadOne/ListLoadedPlugins/IsPluginDisabled） | 0 当前 |
| `AutoRestart()` | `bool` | 配套 SetAutoRestart 用 |

### A4. 生命周期 / 工具注入（PluginSDK 方法）

| 方法 | 签名 | 备注 |
|---|---|---|
| `SetAutoRestart` / `AutoRestart` | `(bool)` / `() bool` | example 使用 16 次 |
| `InjectText` | `(source, channel, text string)` | → C ABI case 5 |
| `InjectInterruptText` | `(source, channel, text string)` | example 使用 6 次 → case 6 |
| `InjectTextNoMemory` | `(source, channel, text string)` | → case 7 |
| `InjectInputSync` | `(source, channel, text string) string` | → case 47（例：qq 闭环） |
| `SetToolBlocks` | `(blocks []ContentBlock)` | **当前空实现**（C ABI 无对应），迁移后经 arena 二进制注入可实现 |
| `RegisterStopHandler` / `RunStopHandlers` | `(func())` / `()` | 已有（qq 等 1 次） |
| `RegisterOnRemoveHandler` / `RunOnRemoveHandlers` | `(func())` / `()` | example 使用 3 次 |
| `Set*`（SetIOInjector/SetMemoryAPI/.../SetPluginMgrAPI） | — | 供 bridge/核心启动时接线，插件不直接调 |

### A5. 核心数据类型（迁移前后结构体字段/JSON tag 不变）

| 类型 | 关键字段 | 备注 |
|---|---|---|
| `StageContext` | 16 字段：RawMessage/UserID/GroupID/ContextMsgs/LLMText/ReasoningContent/TokenUsage/ToolCalls/ToolResults/FinalText/Response/Phase/Memory/NoMemory/Extra/Errors + Lock/RLock/Unlock/RUnlock/IsResponded | **注意**：外部插件经 C ABI 只能看到 10 个字段（见 C3），迁移到共享内存后可看到全部 16 个 |
| `ToolDef` | Name/Plugin/Description/Parameters/NoMemory/Cleaner(func) | `Cleaner` 是函数，**无法过 C ABI**（迁移后经 RPC/进程内保留） |
| `ChannelDef` | NoMemory/Cleaner(func) | 同上 |
| `ToolCall` / `ToolResult` / `MemItem` | ID/Name/Plugin/Arguments；CallID/Name/Plugin/Success/Result；Role/Content/Score | 全部纯 JSON 可序列化 |
| `ContentBlock` / `ImageURL` / `AudioURL` | Type/Text/ImageURL/AudioURL；URL/Detail；URL | 全部可偏移化（迁移评估 3.3 已核实） |
| `Event` / `EventHandler` / `EventSubscriber` | Type/Source/Payload/Timestamp | 迁移后才对外部插件真正可用 |
| `Triple` / `Entity` / `Relation` / `Doc` / `TextEvent` / `PersonProfile` / `SocialRelation` / `Knowledge` / `ConfigDef` | — | 全部 JSON 可序列化 |

**函数类型字段盘点（唯一无法跨进程序列化的东西）**：
- `ToolDef.Cleaner func(string) string`
- `ChannelDef.Cleaner func(string) string`
- `StageContext.mu sync.RWMutex`（~~锁~~ → 迁移后映射到跨进程锁仲裁）
- 各种 `ToolHandler`/`StageHandler`/`EventHandler`/`func()`（回调 → RPC 反向注册）

→ 这些正是共享内存 + RPC 要保的「留在进程内的回调型资源」（迁移评估 3.5）。

---

## 三、合同面 B：bridge 51 个 method id ↔ SDK 方法映射（改造基线）

> ⏹️ **已完成（2026-09-03）**：整数 method id 已全部平移为 RPC method 名字符串，
> 定义在 `internal/plugin/proc/protocol.go` 的 `Method*` 常量（共 60 个，含内核→插件方向）。
> 原 `tmplLinuxBridge` 与 `meta.Core<Method>` 整数表**均已删除**。
>
> 两个遗留点：`case 25`（`CoreFreeString`）无对应 method（内存管理是 C 层特有问题）；
> `io.setToolBlocks` 已定义但内核侧仍返回未实现（C ABI 时代也是空实现，非回归）。
>
> 下表保留作为历史对照。

| # | method id（今天 C ABI） | SDK 背的方法 | 迁移后 RPC method 名（建议） |
|---|---|---|---|
| 1 | CORE_REGISTER_TOOL | RegisterTool | `tool.register` |
| 2 | CORE_REGISTER_STAGE | RegisterStage | `stage.register` |
| 3 | CORE_REGISTER_OUTPUT_CH | RegisterOutputChannel | `output.register` |
| 4 | CORE_REGISTER_PLUGIN_API | RegisterPluginAPI | `api.register` |
| 5 | CORE_INJECT_TEXT | InjectText | `io.injectText` |
| 6 | CORE_INJECT_INTERRUPT_TEXT | InjectInterruptText | `io.injectInterrupt` |
| 7 | CORE_INJECT_TEXT_NO_MEMORY | InjectTextNoMemory | `io.injectTextNoMem` |
| 47 | CORE_INJECT_INPUT_SYNC | InjectInputSync | `io.injectInputSync` |
| 8 | CORE_SET_AUTO_RESTART | SetAutoRestart | `lifecycle.autoRestart` |
| 9 | CORE_MEMORY_RECALL | Memory().Recall | `memory.recall` |
| 10 | CORE_MEMORY_COMMIT | Memory().Commit | `memory.commit` |
| 11 | CORE_MEMORY_INTROSPECT | Memory().Introspect | `memory.introspect` |
| 12 | CORE_MEMORY_MERGE | Memory().MergeEntities | `memory.merge` |
| 13 | CORE_MEMORY_PURGE | Memory().Purge | `memory.purge` |
| 14 | CORE_DOC_QUERY | DocMemory().Query | `doc.query` |
| 15 | CORE_KNOWLEDGE_SEARCH | Knowledge().Search | `knowledge.search` |
| 16 | CORE_SETTINGS_GET | Settings().Get | `settings.get` |
| 17 | CORE_SETTINGS_SET | Settings().Set | `settings.set` |
| 18 | CORE_SETTINGS_REGISTER_DEF | Settings().RegisterDef | `settings.registerDef` |
| 19 | CORE_LLM_LIST_SOURCES | LLM().ListSources | `llm.listSources` |
| 20 | CORE_LLM_SET_SOURCE | LLM().SetSource | `llm.setSource` |
| 21 | CORE_SOCIAL_GET_PERSON | Social().GetPerson | `social.getPerson` |
| 22 | CORE_SOCIAL_GET_NETWORK | Social().GetNetwork | `social.getNetwork` |
| 23 | CORE_SUBSCRIBE | Events().Subscribe | `events.subscribe`（**今天空实现**） |
| 24 | CORE_UNSUBSCRIBE | （退订闭包） | `events.unsubscribe`（**今天空实现**） |
| 25 | CORE_FREE_STRING | （内存释放） | 删除（RPC 无此概念） |
| 26 | CORE_SETTINGS_GET_CORE | Settings().GetCore | `settings.getCore` |
| 27 | CORE_SETTINGS_SET_CORE | Settings().SetCore | `settings.setCore` |
| 28 | CORE_SETTINGS_LIST_CORE | Settings().ListCore | `settings.listCore` |
| 29 | CORE_SETTINGS_GET_PLUGIN | Settings().GetPlugin | `settings.getPlugin` |
| 30 | CORE_SETTINGS_SET_PLUGIN | Settings().SetPlugin | `settings.setPlugin` |
| 31 | CORE_SETTINGS_LIST_PLUGIN | Settings().ListPlugin | `settings.listPlugin` |
| 32 | CORE_DOC_INSERT | DocMemory().Insert | `doc.insert` |
| 33 | CORE_DOC_REMOVE | DocMemory().Remove | `doc.remove` |
| 34 | CORE_DOC_STATS | DocMemory().Stats | `doc.stats` |
| 35 | CORE_KNOWLEDGE_ADD | Knowledge().Add | `knowledge.add` |
| 36 | CORE_KNOWLEDGE_LIST | Knowledge().List | `knowledge.list` |
| 37 | CORE_LLM_CURRENT_SOURCE | LLM().CurrentSource | `llm.currentSource` |
| 38 | CORE_SOCIAL_GET_TRAIT | Social().GetTrait | `social.getTrait` |
| 39 | CORE_SOCIAL_GET_RELATIONS | Social().GetRelations | `social.getRelations` |
| 40 | CORE_SOCIAL_LIST_PERSONS | Social().ListPersons | `social.listPersons` |
| 41 | CORE_TEXT_MEMORY_APPEND | TextMemory().Append | `textmemory.append` |
| 42 | CORE_SETTINGS_LIST | Settings().List | `settings.list` |
| 43 | CORE_SETTINGS_DEFS | Settings().Defs | `settings.defs` |
| 44 | CORE_SETTINGS_DUMP | Settings().Dump | `settings.dump` |
| 45 | CORE_SETTINGS_PLUGINS | Settings().Plugins | `settings.plugins` |
| 51 | CORE_SETTINGS_DATA_DIR | Settings().DataDir | `settings.dataDir` |
| 46 | CORE_REGISTER_INPUT_CH | RegisterInputChannel | `input.register` |
| 48 | CORE_PLUGIN_RELOAD_ONE | PluginMgr().ReloadOne | `plugin.reloadOne` |
| 49 | CORE_PLUGIN_LIST_LOADED | PluginMgr().ListLoadedPlugins | `plugin.listLoaded` |
| 50 | CORE_PLUGIN_IS_DISABLED | PluginMgr().IsPluginDisabled | `plugin.isDisabled` |

**bridge 侧反向调用（内核 → 插件，RPC 的另一半）**：

| 今天 | 迁移后 |
|---|---|
| `go_invoke_tool(name, argsJSON)` | `tool.invoke`（homed → pinvoke） |
| `go_invoke_stage(stage, ctxJSON, resultOut)` | `stage.invoke`（homed → pinvoke，共享内存数据面） |
| `go_invoke_output(channel, type, payloadJSON)` | `output.invoke`（homed → pinvoke） |
| `go_free_string` | 删除 |

---

## 四、合同面 C：StageContext 跨 ABI 现状 → 共享内存目标

> ✅ **已达成（2026-09-03）**：子进程插件现在看到全部 18 个字段（枚举见
> `internal/plugin/proc/shm.go`），且可写回。生产实测：sanitizer 在另一个进程里
> 改写 13590 字节文本，内核读到改写结果（`stage post_action 改写了 1 个字段`）。

### C1. 迁移前（C ABI 副本模型）：插件只看到 10 个字段

`stageContextWritable`（templates.go:762）下发/回传的字段：

```
raw_message  user_id  group_id  phase  llm_text  final_text  no_memory
+ response（可选）  + tool_calls（有才传）  + tool_results（有才传）
```

**看不到的 6 个字段**：`ContextMsgs` / `ReasoningContent` / `TokenUsage` / `Memory` / `Extra` / `Errors`

### C2. 迁移后（共享内存 + 锁仲裁）：插件可看到/改写全部字段 — ✅ 已实现

字段级 `Slice{Off,Len}` 描述符 + 内核仲裁锁。插件进程内保留原生 `StageContext`，
handler 照常读写，`Lock/RLock` 映射到跨进程锁仲裁 RPC（`stage.lock`/`stage.unlock`），
handler 返回时脏字段写回共享段。

**关键设计决定**：全部子进程插件共享**同一块 memfd**。第一版设计是每插件一段，
那会退化成副本模型，复现 §8.4 的 35.8~36.8% lost update。

→ **接口形式不变，能力变强**（能力断层消除：外部插件拿回 ContextMsgs 等）。

Windows 同步受益：从「只下发 3 字段、无写回」升到全字段可见 + 写回，
与 Unix 共用同一套 RPC 实现与共享段布局。

### C3. lost update 的合同面定义 — ✅ 已消除

C ABI 时代 `stageContextWritable` **无条件回传 10 个字段的当前快照**——两个插件
（sanitizer 改 ToolResults + weather 只读）并行时，weather 的回传会覆盖 sanitizer
的清洗结果（实测 1.6~4.3%，高并发下 35.8~36.8%）。

Part 0.2 先做了过渡补丁（只回传真正变更的字段）；Part 4 的共享内存模型从根上解决
（字段级描述符 + 锁仲裁，并发改写同一对象）。

回归基线：`TestPlugin_FiveProcessesConcurrentAppendNoLostUpdate`、
`TestSegment_ProductionScenario_SanitizerNotOverwrittenByWeather`。

---

## 五、外部插件实际触达面（example 18 插件实测汇总）

> 这是「17 个存量插件业务代码零改动」的直接依据——它们**只用**下表这些 API，全部在公开 SDK 合同面内。

| 插件 | 用到的 SDK 触达 |
|---|---|
| qq（最复杂） | SetAutoRestart / RegisterDef×11 / RegisterOutputChannel(qq, 4 caps) / RegisterInputChannel(qq, NoMemory+Cleaner) / RegisterStage(BeforeToolcall, OwnTools) / RegisterTool×N / InjectInterruptText×2 / getSetting(p.sdk.Settings()) |
| weather / rss / bili / ocr / files / memo / music / a2a / acp / ai_image / browser / calendar / editdoc / recoverydiag / sanitizer / vanblog / luademo | RegisterTool / Settings / SetAutoRestart / (部分) RegisterStage / RegisterOutputChannel / InjectInputSync / Knowledge / RegisterStopHandler / RegisterOnRemoveHandler |

**结论**：外部插件触达面 ⊆ 公开 SDK 合同面；无任何插件直接使用方法 id 或 bridge 内部符号。
→ 只要公开 SDK 签名不变 + bridge 语义平移，接口不变约束成立。

---

## 六、迁移后外部插件「新获得」的能力（合同面扩展——只增不减）

| 能力 | 迁移前 | 迁移后 | 实际结果 |
|---|---|---|---|
| 事件订阅 `Events().Subscribe`（case 23/24） | ❌ 空实现 | ✅ 事件环（EvtRing + eventfd + 独立游标） | ✅ 已接线（当前零用户） |
| `SetToolBlocks` 多模态注入 | ❌ 空实现 | ✅ 二进制落 arena，Slice 描述符回传 | ⚠️ method 已定义，内核侧仍未实现 |
| `ContextMsgs`/`ReasoningContent`/`TokenUsage`/`Memory`/`Extra`/`Errors` | ❌ 看不到 | ✅ 共享内存全字段 | ✅ 18 字段全可见可写 |
| 插件崩溃隔离 | ❌ panic 带崩 homed | ✅ 子进程独立崩溃 | ✅ 测试 + 生产验证 |
| 热重载 `.so` | ❌ `DF_1_NODELETE` no-op | ✅ 同路径替换 `.bin` 即生效 | ✅ 生产实测 |
| 工具超时取消 | ❌ cgo 不可中断（泄漏线程） | ✅ `Process.Kill()` 真取消 | ✅ 整套新架构零 cgo |
| `output_send` 结果 | ❌ 永远假成功 | ✅ 可同步等真实结果 | ✅ 生产实测 `map[status:sent]` |
| Lua/Windows DLL 路径 | ❌ 三套 ABI 分裂 | ✅ 收敛为单一 RPC 实现 | ⚠️ Windows 已收敛；Lua 仍独立（留待后续） |

**三项未完全兼得的说明**：

- `SetToolBlocks`：`io.setToolBlocks` 已在 protocol 定义并划入 `CapCore`，但内核侧 handler
  仍返回未实现。C ABI 时代它也是空实现（§1.4），故**不是回归**，但也没兑现承诺。
- Lua：`lua_plugin.go`/`dynamic_lua.go` 仍走自己的路径。Lua 经解释器不经 C ABI，
  不属于本轮要消除的 6 类缺陷，因此不阻塞。收敛第三套 ABI 是独立优化。
- 事件订阅：机制已完成（内核侧 `EvtRing` + 模板侧 `evtConsumerLoop`），
  但**无任何现有插件使用 `Events().Subscribe`**，所以生产上未经真实负载检验。

**刻意不给**（权限梯度显式化，非技术限制）：`SelftestAPI`/`SupervisorAPI`/`TrackerAPI`/
`StatusAPI`/`AdapterAPI`/`ConfigAPI`/`ToolAPI`/`IndexerAPI`/`OutputChanRaw`/`EventPublish`
（内核内部机制）。清单与理由记在 `internal/plugin/proc/capability.go` 的
`withheldCapabilities`，`TestCapability_WithheldListIsDocumented` 守护。

这一项从「C ABI 表达能力的意外产物」变成**显式策略**：以前拿不到是因为
C 结构体不好传函数指针（那是运气，任何人给 dispatch 加个 case 就能捅穿）；
现在是三道闸：类型层（`procCore` 命名字段不嵌入）+ 能力集（manifest 声明）
+ RPC 边界（返回明确错误而非静默忽略）。

---

## 七、接口冻结检查点（全部已通过）

1. ✅ **阶段 2（子进程通道原型）**：`plugindev` 重编 weather → `plugin.bin` → 端到端跑通。
   验收：weather 业务代码逐字节未改（`git status example/` 无输出）。
2. ✅ **阶段 3（共享内存）**：子进程并发改写 StageContext 丢失率 = 0%
   （`TestPlugin_FiveProcessesConcurrentAppendNoLostUpdate` 与
   `TestSegment_ProductionScenario_SanitizerNotOverwrittenByWeather`）。
3. ✅ **阶段 5**：17 个外部插件全部 `.bin` 化、cabi 删除（-3198 行）；
   `go build ./...` 与全仓 `go test ./...` 均通过。
4. ✅ **全程**：`git diff third_party/homeagent-sdk/sdk/` 为零——接口冻结的硬证据。

生产端到端（2026-09-03，真实 QQ 消息）：

```
input from qq → response (83293ms, tools=[qq_get_message qq_get_history
                                          output_send__qq output_send__qq qq_mark_read])
[sanitizer] cleaned 2 bytes (before=13590 after=13588)
[proc] sanitizer stage post_action 改写了 1 个字段
tool output_send__qq result: 已通过 [qq] 通道发送: map[status:sent]
```

---

## 八、关联文档

- `docs/zh/架构迁移评估.md` — 完整论证（§3.2 method id 平移、§3.3 数据面、§3.4 SDK 封装、§3.5 回调型资源、§3.8 能力对齐）
- `docs/zh/plugin-migration-plan.md` — Part 0~6 执行计划与完成实录（含 Part 6.5 生产切换、Part 6.6 压测）
- `plan.md` §11 — 11.1~11.9 修复清单（唯一权威编号）
- `third_party/homeagent-sdk/sdk/` — 合同面 A 的代码实现（全程零 diff）
- `internal/plugin/proc/protocol.go` — 合同面 B 的代码实现（`Method*` 常量，取代已删的 bridge 模板）
- `internal/plugin/proc/shm.go` — 合同面 C 的代码实现（共享段布局与 18 字段枚举）
- `internal/plugin/proc/capability.go` — 权限梯度（capability 组 + `withheldCapabilities`）
- `third_party/homeagent-sdk/tools/plugindev/templates/` — 子进程运行时模板（三文件）
- `docs/zh/experiments/plugin-arch/` — 18 项可行性实验 + `19-migration-verify/` 迁移执行期工具