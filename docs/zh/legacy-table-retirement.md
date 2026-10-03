# 旧表清理可行性评估（2026-10-04）

> 结论：**现在不能清理**。55 处生产调用点仍在读写 `entities` / `relations` / `sentences`。
> 这份评估回答「能不能删」以及「删之前要先做什么」。

## 一、数据侧已就绪

清理的**数据前置**已全部通过（`TestRetire_清理前置`，生产快照实测）：

| 判据 | 结果 |
|---|---|
| ① 每个 entity 都有对应的块 | ✅ 1294 → 1294，缺块 0 个 |
| ② 每条 relation 都有对应的边 | ✅ 980（去重 959）→ 959 |
| ③ 每条 sentence 都有原句块 | ✅ 66 → 66（`55db22c` 修复后） |
| ④ 悬空的 sentence→块 边 | ✅ 0 |
| ⑤ 端点不存在的块边 | ✅ 0 |

## 二、代码侧远未就绪

### 调用点分布

```
internal/memory/social/social.go            7 处
internal/memory/graph.go                    6 处（Commit/Recall 实现）
internal/memory/scene.go                    4 处
internal/memory/light_memory.go             4 处
internal/plugins/webui/handler_memory.go    2 处
internal/plugins/healthcheck/plugin.go       2 处
internal/plugin/proc/corehandler_memory.go  2 处
internal/plugin/lua_plugin.go                2 处
internal/sdk/memory_impl.go                 2 处（Recall + CommitWithMedia）
────────────────────────────────────────
合计 55 处
```

### 三张表各自还被谁依赖

**`entities`**

```go
graph.go:505  Commit 里 SELECT id FROM entities WHERE name = ?   （写入路径）
graph.go:681  hotspots ORDER BY mention_count DESC
graph.go:756  SELECT ... FROM entities WHERE LOWER(name) LIKE ?  （Recall）
```

**`relations`**

```go
graph.go:532  Commit 里查重（source+target+type+session）
graph.go:705  Recall 的关系遍历
graph.go:748  SELECT MAX(r.sentence_id) FROM relations r
graph.go:362  从 sentence_ref 回填 sentences
```

**`sentences`**

```go
graph.go:362  INSERT OR IGNORE INTO sentences ... FROM relations.sentence_ref
graph.go:522  CommitWithMedia 挂接媒体块的落点
block_recall.go:316  EnsureSentence（唯一残留写入点）
```

## 三、为什么这三条链路必须一起换

它们不是三张独立的表，而是**一条写入链 + 一条读取链**：

```
写入  Commit ──> entities(name 唯一) + relations + sentences(挂媒体块)
读取  Recall ──> entities(LIKE) + relations(遍历) + sentence_id
```

**只删表不改代码 ⇒ 写入报 "no such table"，读取返回空。**
而 `Commit` 是**在线主路径**（每轮对话都可能调用），
不是可以延后的离线工具。

## 四、清理前必须完成的工作

按依赖顺序：

| # | 工作 | 说明 |
|---|---|---|
| 1 | `Commit` 改写为写块 | 不能再 `INSERT entities`；三元组应直接落 `memory_blocks` + 块边 |
| 2 | `Recall` 改写为读块 | 现在读 entities/relations；应读块 + 块边 |
| 3 | 媒体挂接点迁移 | `CommitWithMedia` 的 `sentence_id` 要换成原句块 ID |
| 4 | social / scene / light_memory 适配 | 它们的调用要改到新接口 |
| 5 | WebUI / healthcheck / proc / lua / SDK 适配 | 对外契约不变，实现换掉 |
| 6 | **回滚演练** | 在快照上跑完 1-5，验证召回不退化 |

★ 第 6 步不是可选项：`Commit` 在线上，改错了**当场影响对话记忆**。

## 五、当前建议

**不要现在清理。** 理由不是"风险高"，而是：

1. 数据侧就绪 ≠ 代码侧就绪（当前 55 处调用点）
2. `Commit`/`Recall` 在**在线主路径**上，不是一次性迁移
3. 相比之下，**清理旧表的收益只是"少三张空表"**，
   而代价是重写一条在线链路

⇒ 真正该做的顺序是：**先让写入与读取都走块**（1-3），
  旧表自然失去写入方，最后再谈删除。

## 附：数据侧的两个真实缺口（已修）

- **66 条 sentence 没有原句块**（`55db22c` 修复）：
  迁移只从 entities 读，从不为 sentences 建载体 ⇒ 直接清理会丢原文
- **关系边 959 vs relations 980**：
  20 组 `(source,target,type)` 完全重复被边表去重（`dd2c996` 已显式报出 `DedupedEdges`）


## 六、⚠️ 一个操作错误：**`cp` 拿不到一致的快照**

验证 `55db22c`（sentences 原句块）时，我用
`cp /home/newqqagent/memory/graph.db /var/tmp/ha-probe/fix.db` 取副本，
结果：

```
生产库     entities=1294  sentences=66
cp 出来的   entities=1293  sentences=67      ← 差 1，且方向相反
```

第一反应是「迁移改动了源表」，差点去查迁移的写入逻辑。
**实际是 `cp` 的问题** —— 生产库在 WAL 模式下，已提交但未
checkpoint 的数据还在 `-wal` 文件里，`cp` 只拿到主库文件 ⇒ 不一致快照。

用 `sqlite3 .backup` 重取后：

```
.backup 快照  entities=1294  sentences=66  blocks=98   ← 与生产库一致
```

★ 本文档与 `production-recall-validation.md` 里都写过
  「库在写，`cp` 拿不到一致快照，要用 `.backup`」，
  **我自己写下的规则自己违反了。**

⇒ 判据也该加一条：**快照来源必须可验证**。
  最省事的做法是取完快照立刻与源库比对关键计数，不一致就重取
  —— 差 1 就会暴露。
