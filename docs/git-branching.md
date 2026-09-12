# Git 分支管理规范

> 生效：2026-08-31，2026-09-04 修订（三级发布通道 + 单条发布分支），2026-09-06 修订（SDK 仓版本语义与发版联动，见 §七）。
> 适用：**本仓（TrueAgent/HomeAgent）与 third_party/homeagent-sdk（SDK 仓）**——两仓协作时分支策略必须一致，本规范两仓同用。
> 核心原则一句话：**main 唯一长命、永远可部署；一切新工作在特性分支；一个中版本一条发布分支，alpha/beta/正式由 tag 区分；hotfix 只进发布分支并 cherry-pick 回 main。**

---

## 一、分支类型总览

| 分支 | 生命周期 | 来源 | 去向 | 部署性 |
|---|---|---|---|---|
| `main` | **唯一长命分支** | — | — | ✅ **永远可部署** |
| `feature/xxx` | 短命（本次特性完成即删） | main | 合回 main | ❌ 不部署 |
| `release/vX.Y.x` | 中命（**整个中版本生命周期**） | main | 打 tag → 构建发布 | ✅ **发布产物来源** |
| hotfix（直接提交发布分支） | 随发布分支 | 发布分支 | **cherry-pick 回 main** | ✅ |

```
main ──────────────── E ──────────────── G ────────────────（永远可部署）
      │                                    ▲
      │ feature/xxx                         │ cherry-pick（修复逐个 pick 回）
      ├── A ── B ──(合回)───────────────────┤
      │                                    │
      └── release/v1.0.x ────────────────────────────────────────────────
          │                                │              │
          ├─(tag v1.0.0-alpha.1) 内部验证   │              │
          ├─(tag v1.0.0-beta.1)  小范围试用 │              │
          ├─(tag v1.0.0)         正式发布   │              │
          ├─(hotfix) F ─────────────────────┤              │
          ├─(tag v1.0.1)         patch 发布 │              │
          ├─(hotfix) H ────────────────────────────────────┤
          └─(tag v1.0.3)         patch 发布
```

---

## 二、分支职责

### 1. `main`（唯一长命分支）

- **唯一长期存在且永远可部署**。任何时刻 `git checkout main` 出来都是可构建、可上线的状态。
- 积攒**下一个中版本**的功能：feature 分支完成即合回，main 持续向前。
- **main 上不直接开发**。所有改动经 feature 分支合入；hotfix 经 cherry-pick 注入。
- **main 的 `internal/meta.Version` 始终是下一个未发布版本**，不随 patch 发布变动。
- 合入门禁（**单人直推也遵守**，不强制 PR 但强制验证）：
  - `make test` 全绿
  - 涉及插件/工具链时：接口冻结检查 `git diff third_party/homeagent-sdk/sdk/` 为空
  - `go vet ./...` 无新增告警

### 2. `feature/xxx`（新特性/修复）

- 命名：`feature/<短横线描述>`，如 `feature/plugin-proc-migration`、`feature/memory-media`。
- **从 main 开出**：`git checkout -b feature/xxx main`。
- 完成后合回 main：
  - 单人：直推（`git merge --no-ff` 保留特性边界，或 squash 成一个 commit，二选一在团队内固定）。
  - 多人：走 PR（review 后合入）。
- 合回后删除 feature 分支（避免累积）。

### 3. `release/vX.Y.x`（发布分支：一个中版本一条）

- **命名用 `x` 占位 patch 位**：`release/v1.0.x` 承载 1.0.0 → 1.0.1 → … → 1.0.N 全部发布，
  直到 `release/v1.1.x` 切出为止。**不要按 patch 号建分支**（`release/v1.0.1`、`release/v1.0.3` 各一条会把
  同一发布线切成互不相连的碎片，追溯时无法用一条分支看完整条线的演进）。
- **从 main 的某个可部署点切出**：`git checkout -b release/v1.0.x main`。
- 切出后**冻结功能**——发布分支上只做：版本号 bump、发布准备、bug 修复、文档。
- **现网部署永远用发布分支上 tag 的构建产物**，不是 main 头部、更不是 feature。

### 4. 三级发布通道（alpha / beta / 正式）

通道**由 tag 区分，不由分支区分**——三者共用同一条 `release/vX.Y.x`。

| 通道 | tag 形式 | 含义 | 受众 |
|---|---|---|---|
| alpha | `vX.Y.Z-alpha.N` | 功能齐了但未充分验证，可能有已知缺陷 | 仅内部/开发者自测 |
| beta | `vX.Y.Z-beta.N` | alpha 问题已修，等待真实环境暴露长尾问题 | 小范围试用、愿意承担风险的用户 |
| 正式 | `vX.Y.Z` | 通过验证，可上现网 | 所有用户 |

- **推进顺序**：alpha → beta → 正式，逐级向前，**每级都是同一条分支上的新 tag**。
  这也是 semver 的标准预发布语义（`1.1.0-alpha.1 < 1.1.0-beta.1 < 1.1.0`），
  包管理器与版本比较逻辑天然认得，无需额外约定。
- **允许跳级**：若改动小、验证充分（如仅一处已定位并有回归测试覆盖的内核修复），
  可直接打正式 tag。跳级要在发布说明里写明理由。
- alpha/beta 的构建产物**可以上传 release 附件**，但必须在 gitcode release 上勾选
  "预发布"标记，且发布说明首行标注通道与已知风险。
- **beta 未清零的严重问题不得进正式**：正式 tag 意味着"我们认为它能上 24/7 现网"。
- **发版动作只在发布分支上做**：版本号 bump、打 tag、构建产物、上传 release 附件，
  全部发生在 `release/vX.Y.x` 上。**main 永远不是发版分支**——即使某个改动刚刚合进 main、
  即使 main 此刻可部署，也不从 main 打 tag、不拿 main 的构建产物发布。
  main 的版本号是「下一个未发布中版本」的路牌，不是任何一次发布的版本号。

### 5. hotfix（发布后发现的严重 bug）

- **场景**：版本已发布后，发现只存在于该版本（或该发布线）的严重 bug。
- **动作**：直接把修复提交到**发布分支** → 该分支重新构建、打下一个 patch tag（如 `v1.0.4`）发布。
- **关键：hotfix 必须 cherry-pick 回 main**：

  ```bash
  # 在发布分支上提交修复（代码部分与版本号 bump 分开提交）
  git checkout release/v1.0.x
  git commit -m "fix(x): ..."                    # ① 修复本身
  git commit -m "chore(release): bump v1.0.4"    # ② 版本号（此 commit 不 pick 回 main）
  git tag -a v1.0.4 -m "..."

  # 回到 main，只挑修复本身
  git checkout main
  git cherry-pick <修复①的sha>                   # 只 pick ①，不 pick ②
  ```

  > **为什么 cherry-pick 而不是 merge**：发布分支只承载该版本特有的补丁，merge 会把
  > 版本号/发布相关改动一并带进 main 造成冲突，并让 main 的 `meta.Version` 变成
  > 已发布的旧版本号。逐个 cherry-pick 让 main 精确地只获得修复本身。
  > **版本号 bump 不要 pick 回 main。**

- **同时存在多个活跃 feature 分支时**：修复也要 pick 到那些分支，否则它们合回 main 时
  可能带回旧代码。实践做法是修复落地当天就 pick 到全部活跃分支
  （如 2026-09-04 的 stage 双重解锁修复同时 pick 到 `main` 与 `feature/memory-media`）。

- **hotfix 已逐个 pick 回 main ⇒ main 已含全部修复 ⇒ 无需再合并发布分支回 main**。
  这是本规范刻意为之——除非发布分支上有 main 想要的**功能级**改动（罕见），
  否则发布分支永不 merge 回 main。

### 6. 发布分支退役

- **下个中版本发布 = 上一条发布分支生命周期结束**（`release/v1.1.x` 出现即 `release/v1.0.x` 退役）。
- 退役后可删可留：
  - 删除：保持仓库干净（tag 已保留全部历史，删分支不丢东西）。
  - 保留：便于追溯该发布线的历史构建（对 24/7 现网友好）。
- **按 patch 号命名的历史发布分支应当合并/删除**：它们是本规范修订前的遗留形态，
  内容已被对应的 `release/vX.Y.x` 完全包含，保留只会让"哪条才是这条线"变得含糊。

---

## 三、当前分支对齐（2026-09-12 更新）

### 主仓（TrueAgent）

| 分支 | 状态 | 处理 |
|---|---|---|
| `main` | 含全部回流修复；`meta.Version` = 下一个未发布中版本（现为 `1.3.0`） | ✅ 保持 |
| `release/v1.2.x` | **本条发布线**，`meta.Version` = `1.2.0`，vendored SDK 定版 `1.2.0`；已载入两个发布前修复（GUI 输出目录、知识库同名覆盖） | 🆕 2026-09-12 从 main 切出；**尚无 tag** |
| `release/v1.1.x` | 承载 `v1.1.0-beta.1` / `v1.1.0` / `v1.1.1` | 📦 已退役（§2.6：下个中版本发布即退役），保留供追溯 |
| `release/v1.0.x` | 承载 1.0.x 全部 tag | 📦 保留 |
| `feature/multimodal-embedding` | 已合入 main（`eb4762a`，43 提交，`--no-ff`） | ⏳ 待删（删远端分支需用户确认，§执行守则 3） |

> `feature/memory-media`、`feature/plugin-proc-migration` 均已从远端删除（旧表里的待删项已处理）。

### SDK 仓（homeagent-sdk）

| 分支 | 状态 | 处理 |
|---|---|---|
| `main` | `meta.Version` = 下一个未发布中版本（现为 **`1.2.0`**）——SDK **不跟 beta 发版**（§七.2），1.2.0 要等核心的**正式** tag 才定版（§七.3），在那之前路牌不得越过它。此阶段与核心 main（`1.3.0`）**故意不对称**，详见 §七.4 | ✅ 保持 |
| `release/v1.1.x` | `meta.Version` = `1.1.0`，承载 tag `v1.1.0` | ✅ 与核心对应 |
| `release/v1.2.x` | **尚未创建** | ⏳ 随核心**正式** tag 一起建（§七.3：分支上把版本定为 `1.2.0` 再打 `v1.2.0`；beta 阶段不发 SDK） |
| `release/v1.0.0` | 旧 patch 号命名形态，内容已被 main 完全包含 | 📦 保留（供追溯 1.0 线构建） |

### 1.0.x 发布线 tag 历史

| tag | 提交 | 通道 | 说明 |
|---|---|---|---|
| `v1.0.0` | `9b92a04` | 正式 | 外部插件从 C ABI 迁移到子进程 + 共享内存 |
| `v1.0.1` | `e671a8c` | 正式 | 多模态 bugfix（假成功、能力声明与回退链、see_video 帧数语义） |
| `v1.0.3` | `26dc76f` | 正式 | 内核 stage 协调器双重解锁（直接跳正式：单点修复 + 反向验证 + 全类审计） |

> `v1.0.2` 未使用：该号从未发布也无 tag，留空以免与任何本地构建混淆。

### 1.1.x 发布线 tag 历史

| tag | 提交 | 通道 | SDK | 说明 |
|---|---|---|---|---|
| `v1.1.0` | `579d7db` | 正式 | 1.0.0 | 记忆系统支持二进制多媒体节点（CAS 媒体存储 + L0/L2/L3 贯通） |
| `v1.1.0-beta.1` | `7a57a14` | beta | 不发 | 打包链路验证（GUI 架构污染 + 空壳 node_modules）。按 §七.2，beta 不伴随 SDK 发版 |
| `v1.1.1` | 见发布说明 | 正式 | **1.1.0** | 多模态贯通插件边界；SDK 首次随核心正式版发布 |

> `v1.1.0-beta.1` 的提交序在 `v1.1.0` **之后**（它多含一个打包修复），
> 而 semver 预发布语义里 `1.1.0-beta.1 < 1.1.0`。这是「一条发布分支 + tag 区分通道」的
> 已知代价：beta 是为验证**打包链路**而补打的，不代表源码更旧。发布说明里已注明。

### 1.2.x 发布线 tag 历史

| tag | 提交 | 通道 | SDK | 说明 |
|---|---|---|---|---|
| `v1.2.0-beta.1` | `215804c` | beta | 不发 | 统一多模态向量空间 + 媒体升为图记忆一等节点 + 数据面全量迁到共享内存（RPC 协议 **2**，与 1.x 不兼容）。按 §七.2，beta 不伴随 SDK 发版 |
| （正式 tag 待打） | — | — | — | 试运行 beta 无回退问题后打 `v1.2.0`，并同步 SDK 仓 `release/v1.2.x` + `v1.2.0` |

> 1.2.x 与存量插件**不兼容**：RPC 协议升到 2（fd3 布局改变），存量外部插件必须用
> 新版 plugindev 重编为 `plugin.bin`——**不支持滚动升级**，内核与插件须同批重建、同批安装。
> 按 §2.4，跳级直发正式版需在发布说明里列明「单点修复 / 反向验证 / 全类审计」三项；
> 本次改动面大（统一多模态向量空间 + 协议 2 + 数据面全量迁移），不满足跳级条件。

---

## 四、现网部署与版本对应（运维纪律）

- **现网 homed 永远部署 `release/vX.Y.x` 分支上 tag 的构建产物**，路径见 `Makefile`（`make build` → `build/homed`）。
- systemd 服务（`/usr/local/bin/homed`）替换流程：
  1. 备份旧二进制（`homed.bak.pre<版本>.<时间戳>`）
  2. 备份配置库（**用 `sqlite3 .backup`，不用 `cp`**——WAL 模式下 cp 可能拿到不一致快照）
  3. 记录当前插件建链清单，供重启后逐项比对
  4. `install -m 0755` 替换（原子 rename，不会写坏正在运行的进程镜像）
  5. `systemctl restart homeagent`
  6. 健康检查：版本号、插件清单无缺失、`/api/v1/status`、一次真实对话、`fatal error` 计数为 0
- **改造期间现网不得部署 main 或 feature 的中间态**——只有发版才用发布分支的 tag。
- alpha/beta tag 的产物**不上现网**（现网是 24/7 服务，预发布通道的存在就是为了不拿它冒险）。
- 涉及 SDK 仓时：主仓 `go.mod` 的 `replace => ./third_party/homeagent-sdk` 指向本地 vendored 副本，
  发版前确认 vendored SDK 与 SDK 仓 release tag 一致（**两仓中版本对齐是第一优先级**，见 §七）。

---

## 五、快速参考命令

```bash
# 新特性
git checkout main && git pull
git checkout -b feature/xxx
# ... 开发 ...
git checkout main && git merge --no-ff feature/xxx   # 或 squash
git branch -d feature/xxx

# 开一条新中版本的发布线
git checkout -b release/v1.1.x main
git commit -am "chore(release): bump v1.1.0-alpha.1"
git tag -a v1.1.0-alpha.1 -m "..."                   # alpha：内部验证
# ... 修问题 ...
git commit -am "chore(release): bump v1.1.0-beta.1"
git tag -a v1.1.0-beta.1 -m "..."                    # beta：小范围试用
# ... 真实环境验证 ...
git commit -am "chore(release): bump v1.1.0"
git tag -a v1.1.0 -m "..."                           # 正式

# hotfix（发布后）——注意是同一条 release/v1.0.x，不新建分支
git checkout release/v1.0.x
git commit -am "fix(x): 严重 bug"                    # ① 修复
git commit -am "chore(release): bump v1.0.4"         # ② 版本号
git tag -a v1.0.4 -m "..."
git checkout main
git cherry-pick <修复①的sha>                          # ③ 只挑修复
# 若有活跃 feature 分支，也 pick 过去
git checkout feature/xxx && git cherry-pick <main 上那个 pick 的 sha>

# 公开 SDK 接口改动（feature，不是 hotfix）：先进 main，再 pick 到发布分支
git checkout -b feature/sdk-xxx main
# ... 改 third_party/homeagent-sdk/sdk/ 与内核桥接层 ...
git checkout main && git merge --no-ff feature/sdk-xxx
git checkout release/v1.1.x
git cherry-pick <feature 的各 sha>                    # 只挑改动，不挑 main 的版本号
git commit -am "chore(release): bump v1.1.1"          # 发布分支自己的版本号
git tag -a v1.1.1 -m "..."
# SDK 仓同步（仅在核心打正式 tag 时，见 §七.2/§七.3）
cd third_party/homeagent-sdk
git checkout -b release/v1.1.x main
git commit -am "chore(release): SDK 1.1.0（1.1.x 线全程共用）"
git tag -a v1.1.0 -m "..."

# 发布分支退役（下个中版本发布后，可选）
git branch -d release/v1.0.x                          # tag 已保存历史，删分支不丢东西
```

---

## 六、本规范与「接口冻结」约束的关系

- feature 分支合回 main 的门禁（`git diff third_party/homeagent-sdk/sdk/` 为空）是本仓特有的硬约束，独立于 Git 流程本身。
- `internal/sdk` **不受冻结约束**，可自由扩展；冻结只针对公开 SDK 接口（`third_party/homeagent-sdk/sdk/`）。
- 若整改确需突破公开接口，走变更评审（见 `docs/zh/plugin-interface-matrix.md` §七），
  并同步 `SDKCompatibleVersion` 与 SDK 仓的 release tag。
- **公开接口的改动本身是 feature，不是发布准备**：它必须走 `feature/xxx` → 合回 main 的路径，
  再 cherry-pick 到发布分支。不允许把接口新增当成"发布分支上的 bug 修复"直接提交进 release
  ——发布分支冻结功能（§2.3），接口是最典型的功能面。

---

## 七、SDK 仓的版本语义与发版联动

### 1. SDK 版本号跟随核心的中版本，patch 位恒为 `.0`

| 核心版本 | 对应 SDK 版本 |
|---|---|
| 1.1.0 / 1.1.1 / 1.1.2 / … / 1.1.N | **1.1.0**（全线共用，不随核心 patch 变动） |
| 1.2.0 起 | **1.2.0** |

- 核心的 patch 位（`x`）专用于 **bugfix 与漏洞修复**，这类改动不触碰公开 SDK 接口，
  因此 SDK 版本号没有理由跟着动。
- **为什么不逐位对齐**：SDK 版本号是插件开发者的依赖声明。若核心每发一个 bugfix 就把 SDK
  也推一个新号，开发者要么被迫跟版、要么怀疑自己版本过时，而接口其实一个字都没变。
  让 SDK 号只在**接口可能变化的中版本边界**上跳，开发者只需关心「我在为哪个中版本写插件」。
- 因此「两仓版本对齐」在本规范里指**中版本对齐**（核心 1.1.x ↔ SDK 1.1.0），
  不是三位全等。核心 1.1.1 配 SDK 1.1.0 就是对齐状态。

### 2. beta 阶段不发 SDK

- **核心的 alpha/beta tag 不伴随 SDK 仓发版**：SDK 仓在这一阶段**不打 tag、不建 release**。
- **为什么**：beta 是核心自己的测试阶段，此时 SDK 接口尚未固定。若此刻给 SDK 发版，
  插件开发者会照着一个还会变的接口写代码——**那是无效开发**。接口没定就没有可依赖的契约，
  发出去的版本号是一个假承诺。
- 这条约束的对象是 **SDK 仓的发版动作**，不是核心二进制里有没有 SDK 代码。
  主仓 `go.mod` 用 `replace => ./third_party/homeagent-sdk`，任何核心构建都必然含 vendored
  SDK 源码，这是构建机制决定的，不在本条约束范围内。

### 3. 正式发布时 SDK 随核心一起发

核心打**正式 tag**（`vX.Y.Z`，无预发布后缀）时，SDK 仓同步执行：

1. SDK 仓也有自己的 `release/vX.Y.x`（与核心同名，一个中版本一条）；
2. 在该分支上把 `meta.Version` 定为 `X.Y.0`；
3. 打 tag `vX.Y.0`（首次进入该中版本时），并建 gitcode release；
4. 上传 5 平台 plugindev 产物 + `SHA256SUMS`。

同一中版本内的后续核心 patch（1.1.1 → 1.1.2 …）**不重复发 SDK**——SDK 已经是 1.1.0，
没有新东西要发。只有接口再次变化并进入下一个中版本时，SDK 才发 1.2.0。

### 4. 版本号在两仓 main 上的含义

两仓的 `main` 都遵守 §2.1：`meta.Version` 是**下一个未发布中版本**。
所以在 1.1.x 线发布期间，两仓 main 上的值都是 `1.2.0`——它标记「main 正在积攒 1.2 的东西」，
而不是「1.2.0 已经存在」。已发布的版本号一律看对应 `release/vX.Y.x` 分支与 tag。

**但声两仓「同步推进」是有条件的**（这一点曾导致误判，现补写清楚）：
推进的前提是**该中版本已经正式发布过**。具体到当前：

- 核心：切出 `release/v1.2.x` 后，1.2.0 就归发布线所有，main 立即推进到 `1.3.0`；
  **即使 1.2.0 目前只有 beta tag**（beta 不上现网，但发布线已占住这个号）。
- SDK：因为 §七.2 **beta 不发 SDK**，SDK 1.2.0 要等核心的**正式** tag 才定版、
  建 `release/v1.2.x`、打 `v1.2.0`（§七.3）。在那之前，SDK 的「下一个未发布中版本」
  仍然是 `1.2.0`，其 main 不得越过它。

→ 因此在这一阶段，**核心 main = `1.3.0` 而 SDK main = `1.2.0` 是正确的**，
不是遗漏同步。（曾按本节的例子把 SDK main 也推到 1.3.0，等于宣称 SDK 1.2.0 已发布。）
