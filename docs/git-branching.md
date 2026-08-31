# Git 分支管理规范

> 生效：2026-08-31。适用：**本仓（TrueAgent/HomeAgent）与 third_party/homeagent-sdk（SDK 仓）**——两仓协作时分支策略必须一致，本规范两仓同用。
> 核心原则一句话：**main 唯一长命、永远可部署；一切新工作在特性分支；版本发布走 release 分支 + tag；hotfix 只进 released 分支并 cherry-pick 回 main。**

---

## 一、分支类型总览

| 分支 | 生命周期 | 来源 | 去向 | 部署性 |
|---|---|---|---|---|
| `main` | **唯一长命分支** | — | — | ✅ **永远可部署** |
| `feature/xxx` | 短命（本次特性完成即删） | main | 合回 main | ❌ 不部署 |
| `release/vX.Y.Z` | 中命（从切出到下个版本发布） | main | 打 tag → 构建发布 | ✅ **发布产物来源** |
| hotfix（直接提交 release 分支） | 随 release 分支 | release 分支 | **cherry-pick 回 main** | ✅ |

```
main ──────────────── E ──────────────── G ────────────────（永远可部署）
      │                                    ▲
      │ feature/xxx                         │ cherry-pick（hotfix 逐个 pick 回）
      ├── A ── B ──(合回)───────────────────┤
      │                                    │
      └── release/v1.2.0                release/v1.2.0
          ├─(tag v1.2.0)→ 构建发布         ├─(hotfix) F ← 版本特定严重 bug
          └─ 退役（可删可留）                └─ F 被 separately cherry-pick 到 main
```

---

## 二、分支职责

### 1. `main`（唯一长命分支）

- **唯一长期存在且永远可部署**。任何时刻 `git checkout main` 出来都是可构建、可上线的状态。
- 积攒**下一个版本**的功能：feature 分支完成即合回，main 持续向前。
- **main 上不直接开发**。所有改动经 feature 分支合入；hotfix 经 cherry-pick 注入。
- 合入门禁（**单人直推也遵守**，不强制 PR 但强制验证）：
  - `make test` 全绿
  - 涉及插件/工具链时：接口冻结检查 `git diff third_party/homeagent-sdk/sdk/` 为空
  - `go vet ./...` 无新增告警

### 2. `feature/xxx`（新特性/修复）

- 命名：`feature/<短横线描述>`，如 `feature/plugin-proc-migration`、`feature/webui-narrow-fix`。
- **从 main 开出**：`git checkout -b feature/xxx main`。
- 完成后合回 main：
  - 单人：直推（`git merge --no-ff` 保留特性边界，或 squash 成一个 commit，二选一在团队内固定）。
  - 多人：走 PR（review 后合入）。
- 合回后删除 feature 分支（避免累积）。

### 3. `release/vX.Y.Z`（发布）

- **从 main 的某个可部署点切出**：`git checkout -b release/v1.2.0 main`。
- 切出后**冻结功能**——release 分支上只做：版本号 bump、发布准备、bug 修复、文档。
- 打 tag → 构建发布安装包 → 上传（附件命名规范见历史记录）。
- **现网部署永远用 release tag 的构建产物**，不是 main 头部、更不是 feature。

### 4. hotfix（只属于此版本的严重 bug）

- **场景**：版本已发布后，发现只存在于该版本（或该发布线）的严重 bug。
- **动作**：直接把修复提交到 **release 分支**（不收进 main 的开发流）→ 该 release 分支重新构建、打 patch tag（如 `v1.2.1`）发布。
- **关键：hotfix 必须 cherry-pick 回 main**：

  ```bash
  # 在 release 分支上提交修复（代码部分与版本号 bump 分开提交）
  git commit -m "fix(x): ..."                    # ① 修复本身
  git commit -m "chore: bump v1.2.1"             # ② 版本号（此 commit 不 pick 回 main）

  # 回到 main，只挑修复本身
  git checkout main
  git cherry-pick <修复commit的sha>              # 只 pick ①，不 pick ②
  ```

  > **为什么 cherry-pick 而不是 merge**：release 分支只承载该版本特有的补丁，merge 会把 release 分支的版本号/发布相关改动一并带进 main 造成冲突。逐个 cherry-pick 修复 commit 让 main 精确地只获得修复本身。**版本号 bump 不要 pick 回 main**（main 的版本号应始终是下一个未发布版本）。

- **hotfix 已逐个 pick 回 main ⇒ main 已含全部修复 ⇒ 无需再合并 release 回 main**。这是本规范刻意为之——除非 release 分支上有 main 想要的**功能级**改动（罕见），否则 release 永不 merge 回 main。

### 5. release 分支退役

- **下个版本发布 = 此 release 分支生命周期结束**（不再维护）。
- 退役后可删可留：
  - 删除：保持仓库干净（tag 已保留全部历史，删分支不丢东西）。
  - 保留：便于追溯该发布线的历史构建（对 24/7 现网友好，推荐与本仓库一样保留已打 tag 的历史分支做对照）。
- 本仓对现网多代版本并行维护时，保留近期 release 分支是合理的。

---

## 三、当前分支对齐（2026-08-31 执行）

### 主仓（TrueAgent）

| 现存分支 | 状态 | 处理 |
|---|---|---|
| `main` | `48b5c24` [origin/main] | ✅ 保持不变（规范基线） |
| `feature/plugin-proc-migration` | 原 `update`，`69a138c`（领先 main 5：文档基线 + Part 0.1/0.2 + 本规范） | ✅ **已对齐重命名**（2026-08-31） |
| `backup-local`（SDK 仓） | `7092d15`（ahead 3, behind 14，含 `ignore example/recoverydiag` 敏感提交） | ⚠️ 遗留本地分支，功能已合入 main，**保留不删**（无远端，删除即永久丢失） |

### SDK 仓（homeagent-sdk）

| 现存分支 | 状态 | 处理 |
|---|---|---|
| `main` | `61f307b` v1.2.0 | ✅ 保持不变 |
| `update` | `5648519`（领先 main 1：Part 0.2 模板修复） | ⚠️ 与主仓 `update` 对齐重命名 |
| `backup-local` | `7092d15`（ahead 3, behind 14，遗留调试分支） | ⚠️ 可选清理 |

> `update` 整改工作分支按规范应为 `feature/plugin-proc-migration`（多进程插件化整改，8-9 周大特性）。
> 是否重命名由执行人确认；不重命名则视为偏离规范的既有分支，须在文档记录其存在。

---

## 四、现网部署与版本对应（运维纪律）

- **现网 homed 永远部署 `release/vX.Y.Z` 分支打出的 tag 构建**，路径见 `Makefile`（`make build` → `build/homed`）。
- systemd 服务（`/usr/local/bin/homed`）替换前：备份旧二进制 → 停服 → 替换 → 起服 → 健康检查（`scripts/verify_deploy.sh`）。
- **改造期间（update 整改）现网不得部署 main 或 feature 的中间态**——只有发版才用 release。
- 涉及 SDK 仓时：主仓 `go.mod` 的 `replace => ./third_party/homeagent-sdk` 指向本地 vendored 副本，
  发版前确认 vendored SDK 与 SDK 仓 release tag 一致（两仓版本对齐是第一优先级）。

---

## 五、快速参考命令

```bash
# 新特性
git checkout main && git pull
git checkout -b feature/xxx
# ... 开发 ...
git checkout main && git merge --no-ff feature/xxx   # 或 squash
git branch -d feature/xxx

# 发布
git checkout -b release/v1.2.0 main
git commit -am "chore: bump v1.2.0"                  # 版本号
git tag v1.2.0
# ... 构建发布 ...

# hotfix（发布后）
git checkout release/v1.2.0
git commit -am "fix(x): 严重 bug"                    # ① 修复
git commit -am "chore: bump v1.2.1"                  # ② 版本号
git tag v1.2.1
git checkout main
git cherry-pick <修复①的sha>                          # ③ 只挑修复

# release 退役（可选）
git branch -d release/v1.2.0                          # tag 已保存历史，删分支不丢东西
```

---

## 六、本规范与「接口冻结」约束的关系

- feature 分支合回 main 的门禁（`git diff sdk/` 为空）是本仓特有的硬约束，独立于 Git 流程本身。
- 插件多进程化整改（`feature/plugin-proc-migration` 或现 `update`）**不满足接口冻结不等于不能合并**——
  接口冻结约束的是「公开 SDK 不变」，整改若突破需走变更评审（见 `docs/zh/plugin-interface-matrix.md` §七）。