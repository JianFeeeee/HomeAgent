# CI/CD 流水线手册（GitHub Actions）

> 2026-09-29 随仓库迁移 gitcode → GitHub 而建。此前 AtomGit 停止对普通用户
> 提供流水线，本仓无任何自动化验证。本文记录两条流水线的用法、机制与边界。
> 分支模型见 `docs/git-branching.md`（本文与其 §七「发版产物清单」衔接）。

## 0. 仓库与流水线总览

| 仓 | 位置 | 流水线 | 触发 |
| --- | --- | --- | --- |
| 主仓 HomeAgent | `github.com/JianFeeeee/HomeAgent`（公开） | `ci.yml` + `release.yml` | push / PR、`release/**` push |
| SDK 仓 homeagentsdk | `github.com/JianFeeeee/homeagentsdk`（公开） | `release.yml` | `release/**` push |
| gitcode 镜像 | 同名仓库 | 无（由主仓 Release 的 sync job 同步） | — |

**分支保护**（main，公开仓免费）：10 项必须检查（Go build/vet/test、Race、
cross×5、GUI、C gates、Docs）+ `strict`（必须与 main 同步）+ 禁 force push。
⇒ **feature 合入 main 前必须过流水线**（PR 的 checks 全绿才能 merge）。

**gitcode 的角色**：只读镜像（git push 同步）。Release 附件由 sync job 自动
补传，或手工跑 `deploy/scripts/upload_assets.py`（见 §3.4）。

## 1. CI（ci.yml）—— 每次推送到 main / release/** / PR 都跑

六个 job，**全部命令均本地实测过**（原则：不写"应该有用"的未验证步骤）：

| job | 内容 | 时长 |
| --- | --- | --- |
| Go build / vet / test | `go build/vet/test ./...` + `make check-client-versions` | ~12m |
| Race detector | `go test -race`（core + waiter） | ~8m |
| Cross-compile ×5 | waiter/initconfig/mock-server 五平台（CGO=0） | ~5m |
| GUI (node) | `cd cmd/gui && npm test`（纯 Node，零依赖，秒级） | <1m |
| C infrastructure gates | `make check-csrc`（告警/ABI/ASan/跨架构） | ~6m |
| Docs build | mkdocs.yml 可解析性检查 | <1m |

**明确不进 CI**（依赖真机/密钥/内网，跑了只会变 flaky 噪音）：
`deploy-*.sh`、waiter 真机（192.168.2.x）、`npm run test-live`（需真
Electron+Xvfb+真后端）、`scripts/kernel-stress/*`、需 `DEEPSEEK_API_KEY` 的
真实 LLM 测试（自带 t.Skip）。

### 已知边界（不是缺陷）

- **只有 waiter/initconfig/mock-server 能纯交叉编译**。homed/memgc/
  homed-kb-migrate 依赖 cgo（gojieba/onnx），必须原生构建（见 Makefile）。
- **`cmd/gui` 是纯 Electron 目录**（0 个 .go、无 go.mod）。`go test ./...`
  不会包含它（Go 的 `./...` 语义）；只有显式 `go test ./cmd/gui` 才报
  "no Go files"。前端测试走 `npm test`。
- **CGO 必须开**（CI 里 `CGO_ENABLED: 1`）：gojieba 需要。

## 2. 发布（release.yml）—— release/** 推送即发版

### 2.1 标准发版流程（以 1.3.14 为例）

```bash
# 1. 在发布线的 worktree（干净）里 bump 版本
git worktree add /tmp/rel-v1.3.14 release/v1.3.x
cd /tmp/rel-v1.3.14
sed -i 's/Version = "1.3.13"/Version = "1.3.14"/' internal/meta/meta.go
git commit -am "release: 1.3.14"
git push origin release/v1.3.x
# 2. 流水线自动：读版本 → go build/test 门 → 下载资产 → 打包
#    → 打 tag v1.3.14 → 建 GitHub release → 传附件 → 回读校验
#    → （配了 GITCODE_TOKEN 时）同步 gitcode
# 3. 在 GitHub Actions 页看进度；全绿即发版完成
```

**版本号唯一事实源是 `internal/meta/meta.go` 的 Version**（SDK 仓是
`meta/meta.go`）。改它并推送 = 发版指令。

### 2.2 幂等闸门：tag 已存在 ⇒ 整轮跳过

Prepare 先查 `refs/tags/v<version>` 是否存在。已存在（比如改文档的推送、
cherry-pick 维护提交）则 Build/Publish/Sync 全部 skipped。**不会重复发版**。

### 2.3 发版门：go build 硬门 + go test 可显式跳过

- `go build ./...` —— **硬门**，不可跳过。
- `go test ./...` —— 默认跑。历史维护线若存在**既存红测试**（如
  release/v1.3.x 的 deepsearch 测试，main 上已修），在**改动 meta 的那个
  提交**的正文里写 `[skip-release-tests]` 即可跳过：

  ```bash
  git commit -am "release: 1.3.14 [skip-release-tests]

  （正文里写明跳过理由——为什么本线的红是既存的、与本次发版无关）"
  ```

  跳过时 CI 输出 `::warning`，决定记录在发版 commit 里可审计。
  ★ 标记查在**改动 meta 的提交**上而非 HEAD：发版提交后常还会跟几个
  维护提交，只看 HEAD 会让标记被顶掉、静默失效。

### 2.4 产物与验证

产物（v1.3.13 实测）：

```text
homeagent-client_1.3.13_amd64.deb      83.5MB   客户端（不含模型）
homeagent-server_1.3.13_amd64.deb     730.1MB   服务端（含向量模型）
homeagent-full_1.3.13_amd64.deb       808.3MB   全量（模型+ORT+GUI）
homeagent_1.3.13_linux_amd64.tar.gz   839.2MB   内核+CLI+GUI 打包
SHA256SUMS                                       全量校验和
```

流水线内置的验证（不通过即不发版）：

- deb 元数据（Package/Version/Architecture）逐包核对；
- full/server 包内**必须真的含** `TextEncoder.onnx` 与 `libonnxruntime.so`
  （防"默认启用但装完不能用"的假包）；
- `sha256sum -c SHA256SUMS`（产物先平铺再验 —— 脚本把校验和写成平铺名）；
- 发布后**回读**：把附件下载回来再验一遍校验和。

### 2.5 构建资产（ci-assets-v1 release）

server/full 的打包需要 719MB 模型 + 24MB ONNX Runtime。它们**内容不随版本
变**，故作为 `ci-assets-v1` release 的附件一次性托管，每次发版由流水线下载
并 `sha256sum -c` 校验后使用：

```text
chinese-clip-vit-b16-onnx.tar         718.8MB   Chinese-CLIP ViT-B/16 ONNX
onnxruntime-linux-amd64-1.28.0.tar     23.5MB   ORT 1.28.0 + LICENSE + TPN
SHA256SUMS
```

模型或 ORT 升级时：改好本地 `CHINESECLIP_BUNDLE_DIR` /
`ONNXRUNTIME_ASSET_DIR` 指向的目录 → 重新打包 → 在 GitHub 上新建
`ci-assets-v2` release 并上传 → 同步更新 release.yml 里的 `ASSETS_TAG`。

## 3. 运维要点（都踩过坑）

### 3.1 workflow 文件必须存在于目标分支

GitHub 用**被推送 commit 里的** `.github/workflows/*.yml` 决定是否触发。
给旧发布线补流水线时，要把 workflow 文件本身 commit 到那条分支
（实测：只在 main 有时，推 release/** 什么都不触发）。

### 3.2 runner 上没有 electron 缓存

打包脚本从两处找 Electron：`~/.cache/electron` 的 zip，或
`cmd/gui/node_modules/electron/dist`。全新 runner 两处都没有 ⇒ release.yml
在打包前 `npm ci`（electron 已在 package-lock 锁定；**不能**
`npm install --production`，electron 是 devDependency 会被跳过）。

### 3.3 管道里的 `grep -q` 会因 SIGPIPE 误杀检测

`dpkg-deb -c <800M 包> | grep -q <目标>`：grep -q 匹配即退出、关闭读端 ⇒
tar 写 stdout 收到 EPIPE ⇒ pipefail 判失败。**包越大越易触发**
（80M 的 client 没事、800M 的 full 炸）。检测存在性一律
`grep <pat> >/dev/null`。

### 3.4 gh 在非 git 目录要显式 `--repo`

`gh release download` 在 `/tmp/back` 这类非 git 目录里会报
"not a git repository"（gh 从 cwd 的 git 上下文推断仓库）。必须
`gh release download "$TAG" --repo "$GITHUB_REPOSITORY"`。

### 3.5 gitcode 上传的路径语义

`upload_assets.py` 拼路径是 `os.path.join(ASSET_DIR, name)`：
要 `cd` 进资产目录、`ASSET_DIR=.`、传**裸文件名**。SDK 仓的产物多数无
扩展名，必须显式列名（自动扫描按后缀识别，会静默一个都不传）。

### 3.6 手工补发 gitcode 附件

CI 的 sync job 需要 secret `GITCODE_TOKEN`；未配置时该 job 显式跳过。
手工补发的完整流程（下载 GitHub 产物 → 建 release 条目 → 上传）：

```bash
# token 放 ~/.git-credentials（https://JianFeeeee:<token>@gitcode.com）
ASSET_DIR=<产物目录> GITCODE_REPO=JianFeeeee/HomeAgent \
  python3 deploy/scripts/upload_assets.py <tag> <token>
# release 条目必须先存在（脚本向 releases/<tag>/upload_url 取 OBS 签名 URL）
```

## 4. SDK 仓（third_party/homeagent-sdk）

与主仓同构，差异：

| | 主仓 | SDK |
| --- | --- | --- |
| 版本源 | `internal/meta/meta.go` | `meta/meta.go` |
| 产物 | 3 deb + 1 tar.gz（2.4GB） | `hmapdev_*` 5 平台 + SHA256SUMS（~140MB） |
| CGO | 必须（gojieba） | 不需要（CGO_ENABLED=0 纯交叉） |
| 测试 | `go test ./...` | **两处**：根模块 + `tools/hmapdev`（独立 module，根的 ./... 不含它） |
| 门 | 同一机制 | 同一机制（`[skip-release-tests]`、幂等闸门） |

发版：改 `meta/meta.go` 的 Version 推 `release/vX.Y.x`。SDK 版本随核心的
中版本走、patch 恒为 `.0`（见 git-branching.md §七.1）。

## 5. 通知

Actions 失败会给仓库 owner 发邮件（GitHub 默认）。若嫌吵：
github.com/settings/notifications → Actions 关闭，或仓库页 Watch → Custom
取消 Actions。注意失败邮件也可能是"验证步骤自身 bug"的假警报 ——
先看是哪个 job/step 红了再判断（对照 §3 的坑）。
