# HomeAgent 架构设计 v3

## 一、核心理念

24 小时陪伴用户、随时待命的智能管家。**单会话·单 Agent**，身份不漂移。

### 设计原则
- **所有输入走 IO 抽象层（中断模式）**，不直调 agent 方法
- **所有 LLM 调用走 Provider 接口**，不直连 API
- **DeepSeek v4 flash** 为默认 LLM，thinking 模式关闭
- **人格固定**（personal.md），记忆分层管理防止性格突变
- **知识独立于记忆**，agent 主动学习
- **插件 = 容器**，IO 通道是插件的内嵌组件

---

## 二、核心抽象

### IO 抽象层（唯一输入路径）

```
外部设备 (Mic/Camera/OneBot/GPIO/HTTP)
        │
        ▼
IOManager
  ├─ Device 接口（每个设备实现）
  │   ├─ Name() / Type() / Description()
  │   ├─ Tools() → 给 LLM 的 Function Calling 工具
  │   ├─ Execute() → 工具调用分发
  │   ├─ Start() / Stop()
  │   └─ OutputCapabilities() → 输出能力声明
  ├─ InputEvent{Source, Type, Payload} → inputCh
  ├─ OutputEvent{Target, Type, Payload} → outputCh
  └─ 路由表: 输入源 → 默认输出通道
```

**输出通道能力声明**（`OutputCapability` 位掩码）：
| 能力 | 说明 |
|------|------|
| CapText | 文本输出 |
| CapFile | 文件传输 |
| CapImage | 图片输出 |
| CapAudio | 音频输出 |
| CapStructured | 结构化数据（JSON/卡片） |

**Agent 可调用通道工具**：
- `output_list_channels` — 查看所有可用通道及其能力
- `output_set_channel` — 切换当前回复的输出通道
- `output_send` — 通过指定通道异步发送消息（校验能力）

### API 抽象层（唯一输出路径）

```
Agent Core → Provider.Chat() → LLM API
                │
        ┌───────┴───────┐
        ▼               ▼
  OpenAIProvider    LuaAdapter
  (DeepSeek API)    (格式转换)
```

### 插件系统（OpenClaw 兼容）

```
Plugin（容器）
  ├─ 元数据: name, version, author, description
  ├─ IOConfig（可选）: 声明 IO 端口
  ├─ Device（可选）: 原生 Go IO 设备实现
  └─ Tools: 给 LLM 的工具定义
```

插件来源：
- `plugins/` 目录热加载，agent 通过 `plgreload` 工具显式控制重载
- 有原生工厂注册的（如 QQ/OneBot）→ 构造原生设备
- 纯 SKILL.md → 用 PluginDevice 包装为 IO 设备
- 原子化替换：新设备 Start → 原子换路由 → 旧设备 Stop

---

## 三、记忆体系（三层）

```
用户输入 → Context（内存, 30条, TF-IDF排序）
              │ 每次响应后裁剪最不相关的
              ▼
          Document（JSON + TF-IDF向量, 冷72h→图）
              │ 心跳蒸馏（30min）
              ▼
          Graph（SQLite三元组, 定期重整+同义合并）
```

| 层 | 存储 | 容量 | 裁剪策略 |
|---|---|---|---|
| Layer 1: 上下文 | `RelevanceContext`（内存环形缓冲） | 30 条 | TF-IDF 余弦相似度排序，低分→文档 |
| Layer 2: 文档 | `document.Store`（JSON 文件 + 向量索引） | 无上限 | 72h 未访问 + ≤2 次命中→图 |
| Layer 3: 图 | `memory.GraphDB`（SQLite 三元组） | 无上限 | 定期重整 + bigram Jaccard 同义合并 |

**注入策略**：
- 只注入**图索引**（实体名+类型+提及数）到 prompt，不注入全文
- agent 通过 `memory_recall` 主动查询详情
- 文档摘要按相关性注入前 3 条

---

## 四、知识体系

独立于记忆，agent 主动学习。

```
knowledge/
  smart_home/content.md
  cooking/content.md
       │
       ▼
  TF-IDF 向量索引（字符 bigram）
       │
       ▼
  knowledge_search / knowledge_create / knowledge_list
```

---

## 五、人格内核

```personal.md``` → 加载一次 → 固定在 system prompt 最前 → 永不漂移。

---

## 六、OneBot QQ 通道

```
go-cqhttp / Lagrange（OneBot 前端）
        │  Reverse WebSocket
        ▼
OneBot Client（internal/onebot/）
  ├─ 事件循环 → Event → IO InputEvent
  ├─ Action 调用 → send_private_msg / send_group_msg / ...
  └─ 自动重连 + 心跳检测
        │
        ▼
Device 注册 → IOManager → Agent
```

---

## 七、数据流

```
用户消息 → IO InputEvent → eventLoop
  │
  ├─ 1. 追加到 RelevanceContext
  ├─ 2. 构建 prompt:
  │     personal.md + 图索引 + 文档摘要 + 上下文 + 工具
  ├─ 3. 工具循环（最多 10 轮）
  │     LLM → tool_calls → Execute → 结果 → LLM → ...
  ├─ 4. 追加响应到上下文
  ├─ 5. 相关性裁剪（Prune → 不相关的归档到文档）
  ├─ 6. OutputEvent → 输出通道
  └─ 7. 记忆候选 → TextMemory(JSONL) + Distiller → GraphDB

心跳（30min）:
  ├─ Indexer.Sync() — 图→向量
  ├─ DocStore.Reindex() — 文档向量重建
  ├─ 冷文档→图归化
  └─ 图同义合并

插件重载（plgreload）:
  ├─ 扫描 plugins/ 目录
  ├─ 加载新插件，Start 新设备
  ├─ 原子替换 IOManager 设备表 + 路由表
  └─ Stop 旧设备
```

---

## 八、配置

```yaml
daemon:
  listen_addr: ":8080"
  data_dir: "/var/lib/homeagent"
  heartbeat_interval: 15s
llm:
  model: "deepseek-v4-flash"
  base_url: "https://api.deepseek.com/v1"
  api_key: "${DEEPSEEK_API_KEY}"
  temperature: 0.7
  max_tokens: 4096
```

---

## 九、关键文件

```
cmd/homed/main.go                — 入口：组装所有子系统
internal/agent/core/agent.go     — Agent 核心：事件循环、工具循环、心跳
internal/agent/core/context.go   — RelevanceContext：TF-IDF 上下文管理
internal/agent/personal.go       — 人格加载
internal/agent/api/provider.go   — Provider 接口 + DeepSeek 实现
internal/agent/io/channel.go     — IO 抽象层：Device/InputEvent/OutputEvent/路由
internal/api/handler.go          — HTTP API 端点
internal/knowledge/knowledge.go  — 知识系统
internal/memory/graph.go         — SQLite 图数据库
internal/memory/indexer.go       — 图索引器
internal/memory/vector/store.go  — TF-IDF 向量存储
internal/memory/document/doc.go  — 文档记忆
internal/memory/text/text.go     — 文本记忆（JSONL）
internal/memory/pipeline/        — 蒸馏器
internal/onebot/                 — OneBot V11 协议实现
internal/plugin/plugin.go        — 插件系统
internal/tracker/tracker.go      — 变更追踪
internal/supervisor/daemon.go    — 守护进程
internal/lua/vm.go               — Lua 适配器 VM
plugins/                         — 插件目录
  qq/SKILL.md                    — QQ 插件 SKILL.md
  qq/skill.json                  — QQ 插件 JSON 元数据
config/config.go                 — 配置加载
pkg/types/                       — 类型定义
DESIGN.md                        — 本架构文档
```

---

## 十、与旧设计的核心区别

| 维度 | 旧设计 (v1) | 当前设计 (v3) |
|------|-------------|---------------|
| 上下文裁剪 | 固定 FIFO 20 条 | TF-IDF 相关性排序 top-K |
| 图记忆注入 | 关键词 LIKE 查询 | 向量搜索实体名，只注索引 |
| 文档→图 | 无 | 72h 冷文档自动归化 |
| 图重整 | 无 | 向量同步 + bigram Jaccard 同义合并 |
| 知识系统 | 无 | `knowledge/` 目录 + TF-IDF 向量 |
| 人格 | 无 | `personal.md` 固定注入 |
| 向量引擎 | 无 | 自研 TF-IDF + 倒排索引（字符 bigram） |
| 部署 | Docker 容器 + 快照 | 单二进制 + overlayfs 追踪 |
| Agent 模型 | 多 Agent 编排 | 单 Agent + 工具循环 |
| 输出通道 | 无 | 能力声明 + 路由 + 校验 |
| 插件系统 | 无 | OpenClaw SKILL.md + 原生工厂 |
| QQ 通道 | 无 | OneBot V11 Reverse WS |
