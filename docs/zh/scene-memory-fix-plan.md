# 场景式记忆修复 plan

> 起点：2026-09-26 生产库实测（`/home/newqqagent/memory/graph.db`）。
> 现象：65 个场景中 6 组是同一场面的双胞胎键；主力场景 `auto:chan:qq+part:morning`
> strength=270、6 个 features、**0 条记忆**，日志里被"命中"179 次。
> 分支：`feature/scene-writeback`（从 main 拉出，工作树干净）。

---

## 根因（三条，逐条修）

### R1 建键不过归一化（病因）

`internal/memory/scene_emerge.go:329`

```go
base := "auto:" + sig.Label(2)     // Label 用 "+" 拼接，未归一化
```

而同一层的另外三条路**都**过了归一化：

| 位置 | 是否归一化 |
|---|---|
| `EnsureScene`（声明建键）`scene_emerge.go:555` | ✅ |
| `effectiveScenes`（写侧挂 ref）`graph.go:1397` | ✅ |
| `RecallByScene`（读侧召回）`scene.go:328` | ✅ |
| **`createSceneLocked`（涌现建键）`scene_emerge.go:329`** | ❌ |

`normalizeSceneSegment` 把 `+` 归一成 `_`，所以涌现键天生带 `+`、其余三方说 `_`。
`scenes.key` 虽有 `UNIQUE` 约束，但**两个不同字符串都合法**，拦不住。

实测 6 组双胞胎（`REPLACE(key,'+','_')` 后重名）：

```
auto:chan:qq+part:morning            ↔ auto:chan:qq_part:morning      (270/0 refs)
auto:chan:mc:event+topic:mc          ↔ auto:chan:mc:event_topic:mc
auto:chan:mc:system+topic:mc         ↔ auto:chan:mc:system_topic:mc
auto:chan:mc-resident+topic:mc       ↔ auto:chan:mc-resident_topic:mc
auto:chan:homeagent-mail-bridge+...  ↔ auto:chan:homeagent-mail-bridge_...
auto:chan:system+topic:任务           ↔ auto:chan:system_topic:任务
```

### R2 `part`（时段，权重 0.2）污染主键与门槛

`Label(2)` 取权重最高的 2 个特征。特征顺序是
`chan(1.0) → peer(1.0) → tool(0.8) → topic(0.4) → part(0.2)`，
只有当前 2 个强特征不足时 `part` 才会进键——于是出现
`auto:chan:qq+part:morning` 这种"时段成了场景身份"的名字。

**更糟的是 `recordSituationEvidenceLocked` 也用 `Label(2)` 做桶**（`scene_emerge.go:380`），
所以 `part` 不只影响名字，还决定 `minSceneEvidence=2` 这个门槛在哪个桶里计数。

实测证据（键名与实际指纹自相矛盾）：

```
场景键: auto:chan:qq+part:morning   strength=270
实际指纹: [chan:qq part:evening]    ← 19:15 命中，evening 却并入 morning
```

这不是 bug 触发，是加权 Jaccard 正常工作：共享 `chan:qq`(1.0)、
并集含 `part`(0.2×2)，相似度 1.0/1.4 = 0.714 > `joinSceneThreshold` 0.5。

### R3 图整理心跳漏扫 `scenes`

`detectEntityMerge` 唯一遍历入口 `Recall(nil,nil,1,"")`，其全量路径只查
`entities`(`graph.go:609`) + `relations`(`graph.go:631`)，`scenes` 不在其中。
所以 R1/R2 造成的双胞胎从 9-15 起无人发现，23 号空转到 strength=270。

**R3 是"没被发现"的原因，R1/R2 是病因。** 顺序不能反。

---

## 步骤

### 步骤 1：修 R1 + R2（源头，不碰存量）

- [ ] `Label` 改名语义：**只取权重 ≥ 阈值的主导特征**，且结果过 `NormalizeSceneKey`
- [ ] `createSceneLocked` 的 `base` 用归一化后的 label
- [ ] `recordSituationEvidenceLocked` 的桶键用同一个归一化 label（R2 的另一半）
- [ ] 判据：先写**红**测试——同一指纹两次建键必须落同一行（现状会落两行）

### 步骤 2：修 R3（让图整理覆盖全库）

- [ ] 给 `mergeLoop` 加**独立的场景去重路径**，不塞进实体那个 O(n²) 双重循环
      （理由：实体 1 万行 × bigram + LLM 裁决，实测 5000 万次配对/轮；
        场景表小且**已有现成的 `situationSimilarity` 加权 Jaccard**，语义更准）
- [ ] 键归一化后相同 ⇒ 合并 refs/features/strength，**不经 LLM**（键相同已证明同一场面）
- [ ] 判据：构造两个 `+`/`_` 孪生键，跑一次 mergeLoop 后期望合成一个

### 步骤 3：清理现网垃圾场景，让它重新生成

用户明确要求：**直接清理，重新生成**（不做保守迁移）。

- [ ] `sqlite3 .backup` 备份（**禁用 cp**，WAL 模式会拷出不一致快照）
- [ ] 删 `origin='emergent'` 的全部场景 + 其 `scene_features`/`scene_refs`
      （**保留 `origin='declared'`**：那些是插件声明的，不是垃圾）
- [ ] 同步清 `situation_evidence`
- [ ] 重启 homed，等 ≥2 次同类交互让场景重新涌现
- [ ] 复验：新场景键**不含 `+`**、有 features **且**有 refs

### 步骤 4：验证与收口

- [ ] `go test ./internal/memory/... ./internal/agent/core/...` 全绿
- [ ] `go build ./...` + 全仓 `go test ./...`
- [ ] 现网观察：日志中场景命中后能查到 refs（非 0）
- [ ] `git_release_check.sh` 无新增红项

---

## 不做的事（防反复挂账）

- **不**把 `scenes` 塞进 `memory_merge` 工具：该工具语义是"实体删除 + 关系重定向"，
  场景合并需要"特征并集 + refs 重定向 + strength 相加"，是另一套操作，硬塞会让工具语义变危险。
- **不**改 `validGraphNodeKind`：它只管 `memory_block_edges` 端点校验，与 `scenes` 无关。
- **不**给实体那个 O(n²) 循环做优化：属独立问题（已实测：1 万实体→~224GB 瞬时分配/轮），
  混进本次修复会让 diff 失焦。单独开条目。
