# HomeAgent 架构设计 v2

## 一、核心理念

24 小时陪伴用户、随时待命的智能管家。**单会话·单 Agent**（可分身/调用其他 Agent），身份不漂移。

### 设计原则
- 所有输入走 IO 抽象层（中断模式），不直调 agent 方法
- DeepSeek v4 flash 为默认 LLM，thinking 模式关闭
- 人格固定（personal.md），记忆分层管理防止性格突变
- 知识独立于记忆，agent 主动学习

---

## 二、人格内核（Personality Core）

```
personal.md ──→ 固定注入 system prompt
```

- 纯文本 Markdown 文件，定义 agent 的人格、行为准则
- 加载一次永不改变（不随对话漂移）
- 放在 prompt 最前面，优先级最高

---

## 三、记忆体系（Memory System）

三层分级架构，自顶向下逐渐持久化、抽象化：

```
             用户输入
                 │
                 ▼
     ┌─────────────────────┐
     │  Layer 1: 上下 文    │  RelevanceContext
     │  基于相关性保留      │  30 条活跃，TF-IDF 排序
     │  最不相关的→文档记忆 │  
     └────────┬────────────┘
              │ 相关性裁剪（每次响应后）
              ▼
     ┌─────────────────────┐
     │  Layer 2: 文档记忆   │  document.Store
     │  全文 + TF-IDF 向量  │  JSON 持久化
     │  冷文档→图数据库     │  72h 未访问→Graph
     └────────┬────────────┘
              │ 心跳蒸馏（30min）
              ▼
     ┌─────────────────────┐
     │  Layer 3: 图数据库   │  memory.GraphDB (SQLite)
     │  实体-关系三元组     │  向量索引（实体名）
     │  定期重整+同义合并   │  bigram Jaccard
     └─────────────────────┘
```

### Layer 1: 上下文（RelevanceContext）

**不再使用固定条数裁剪。改为：**

1. 每次用户输入后，计算**模型输出**与每条上下文的 TF-IDF 余弦相似度
2. 按相关性从高到低排序，保留 topK（默认 30）
3. 最不相关的上下文 → 归档到文档记忆（Layer 2），保留全文+向量
4. 活跃上下文中只保留最近且相关性高的内容

**相关文件：** `internal/agent/core/context.go`

### Layer 2: 文档记忆（Document Store）

**作用：**
- 存储被上下文裁剪下来的事件摘要、agent 主动提交的文档、图记忆同步的索引
- 每个文档包含：摘要、全文、标签、实体、来源、访问计数、最后访问时间
- TF-IDF 向量索引（字符 bigram），支持向量相似度搜索

**冷文档归化：**
- 心跳检测（每 30min）：找出 72h 未访问且访问次数 ≤ 2 的文档
- 转为图数据库三元组（文档→包含内容、提及实体、标签、来源）
- 清理已归化的冷文档

**相关文件：** `internal/memory/document/document.go`

### Layer 3: 图数据库（Graph Memory）

**存储：** SQLite 三元组（实体-关系-实体），每个关系带置信度、会话 ID、日期桶

**注入方式（区分于文档注入）：**
- 用户消息到达时，先对实体名做**向量相似度搜索**（TF-IDF）
- 找到相关实体名 → 查询图数据库
- 只注入**节点索引**（实体名+类型+提及次数+关系类型）到 prompt，不注入全文
- 需要更多细节时，agent 调用 `memory_recall` 工具查询

**定期重整（心跳触发，每 30min）：**
1. 同步实体名到向量索引（`Indexer.Sync()`）
2. 文档记忆向量索引重建
3. 冷文档→图归化
4. 实体同义合并（bigram Jaccard > 0.5）

**相关文件：** `internal/memory/graph.go`, `internal/memory/indexer.go`

### Agent 可用记忆工具

| 工具 | 作用 | 操作对象 |
|------|------|----------|
| memory_recall | 检索图记忆 | GraphDB |
| memory_commit | 写入三元组 | GraphDB |
| memory_introspect | 查看记忆统计 | GraphDB |
| doc_query | 向量查询文档记忆 | Document Store |
| doc_commit | 提交文档 | Document Store |

---

## 四、知识体系（Knowledge System）

独立于记忆系统，用于 agent 学习知识：

```
knowledge/
  smart_home/
    content.md     ← 原始知识文件
  cooking/
    content.md
  ...
       │
       ▼
  TF-IDF 向量索引 ← 知识目录扫描时自动构建
       │
       ▼
  knowledge_search(query) → 返回相关内容
```

**知识来源：**
1. **agent 主动学习**：调用 `knowledge_create` 工具，生成知识→写入目录+向量化
2. **用户上传**：HTTP 文件上传端点 → 写入目录+向量化
3. **预置知识**：`knowledge/` 目录下的 content.md

**相关文件：** `internal/knowledge/knowledge.go`

### Agent 可用知识工具

| 工具 | 作用 |
|------|------|
| knowledge_search | 向量搜索知识库 |
| knowledge_list | 列出知识分类 |
| knowledge_create | agent 主动创建知识 |

---

## 五、数据流总览

```
用户消息
  │
  ▼
IO 输入中断 (channel.go)
  │
  ▼
eventLoop → processTextInput
  │
  ├─ 1. 追加上下文 (RelevanceContext.Append)
  │     └─ 计算向量，缓存
  │
  ├─ 2. 构建 prompt：
  │     ├─ 人格设定 (personal.md)
  │     ├─ 图记忆索引 (Indexer.BuildContext → 向量搜索实体名 → Recall → 摘要注入)
  │     ├─ 文档记忆摘要 (DocStore.Query → 注入前3条摘要)
  │     ├─ 工作记忆上下文 (RelevanceContext.Format)
  │     ├─ 技能注入 (skills)
  │     └─ 工具说明 (indexer + doc + knowledge tools)
  │
  ├─ 3. 工具循环 (process)
  │     ├─ 调用 LLM (DeepSeek v4 flash)
  │     ├─ 解析 tool_calls
  │     ├─ 执行工具 (memory_*/knowledge_*/doc_*/设备工具)
  │     └─ 返回结果，循环直到无 tool_calls
  │
  ├─ 4. 追加响应到上下文
  │
  ├─ 5. 相关性裁剪 (RelevanceContext.Prune)
  │     └─ 最不相关的 → 文档记忆归档
  │
  ├─ 6. IO 输出响应
  │
  └─ 7. 记忆候选事件
        └─ → TextMemory (JSONL 持久化)
            └─ → Distiller → GraphMemory (三元组萃取)

心跳线程 (每 30min):
  ├─ distillContext: 安全裁剪兜底
  ├─ syncGraphToDocs: 图→文档索引同步
  ├─ reorgGraph:
  │   ├─ Indexer.Sync(): 实体名→向量索引
  │   ├─ DocStore.Reindex(): 文档向量重建
  │   ├─ 冷文档→图归化
  │   └─ 同义实体合并
  └─ (后续) GraphDB 向量索引生成/重整
```

---

## 六、关键文件

```
cmd/homed/main.go               — 入口：组装所有子系统
internal/agent/core/agent.go     — Agent 核心：事件循环、工具循环、心跳
internal/agent/core/context.go   — RelevanceContext：基于 TF-IDF 的上下文管理
internal/agent/personal.go       — Personality：personal.md 加载
internal/agent/api/provider.go   — LLM Provider：DeepSeek API 封装
internal/agent/io/channel.go     — IO 抽象层：中断输入、设备注册
internal/api/handler.go          — HTTP API：REST + OpenAI 兼容端点
internal/knowledge/knowledge.go  — 知识系统：目录扫描、向量索引、搜索
internal/memory/graph.go         — 图数据库：SQLite 三元组 CRUD
internal/memory/indexer.go       — 图索引器：向量搜索实体名、摘要注入
internal/memory/vector/store.go  — 向量存储：TF-IDF + 倒排索引 + 余弦相似度
internal/memory/document/doc.go  — 文档记忆：存储、查询、冷文档检测
internal/memory/text/text.go     — 文本记忆：JSONL 原始日志持久化
internal/memory/pipeline/        — 蒸馏器：原始日志→图记忆
internal/tracker/tracker.go      — Change Tracker：overlayfs 文件变更追踪
internal/plugin/plugin.go        — 插件平台：SKILL.md 加载
internal/supervisor/daemon.go    — 守护进程：健康检查、自动 rollback
internal/lua/vm.go               — Lua 适配器 VM
config/config.go                 — 配置加载
pkg/types/                       — 类型定义
```

---

## 七、配置示例

```yaml
daemon:
  listen_addr: ":8080"
  data_dir: "/var/lib/homeagent"
  heartbeat_interval: 15s
  check_interval: 30s

llm:
  provider: "openai"
  model: "deepseek-v4-flash"
  base_url: "https://api.deepseek.com"
  api_key: "${DEEPSEEK_API_KEY}"
  temperature: 0.7
  max_tokens: 4096
```

---

## 八、对比原设计

| 维度 | 旧设计 | 新设计 |
|------|--------|--------|
| 上下文裁剪 | 固定 20 条 FIFO | TF-IDF 相关性排序，保留最相关 |
| 上下文→文档 | 蒸馏器每周期 flush | 每次响应后按相关性裁剪归档 |
| 图记忆注入 | 关键词 LIKE 查询 | 向量搜索实体名，只注索引 |
| 文档→图 | 无 | 72h 冷文档自动归化 |
| 图重整 | 无 | 心跳：向量同步+同义合并 |
| 知识系统 | 无 | `knowledge/` 目录+向量索引+工具 |
| 人格 | 无 | `personal.md` 固定注入 |
| 向量引擎 | 无 | 自研 TF-IDF + 倒排索引 |
