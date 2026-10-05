# kbtree 本机部署交接（交给部署方执行）

我负责把 kbtree 的**代码**合入 main，并把 **skill + 调用方法**在本机装好。
**二进制替换与重启由你做** —— 我不碰运行中的生产守护进程。

## 现状（交接时的事实）

| 项 | 状态 |
|---|---|
| kbtree 代码 | ✅ 已在 `main`（`41d7543`），已推送 |
| skill | ✅ 已装 `${HA_DATA}/skills/knowledge-base/` |
| token | ✅ 已写入 `config_kbtree` 表（固定值，重启不变） |
| 配置库备份 | ✅ `config.db.bak-20260926-143643` |
| **服务** | ❌ **未上线** —— 运行中的二进制里没有 kbtree |
| 我构建的候选二进制 | `/tmp/homed-new`（84.7MB，与 main 同源，`-tags onnxruntime`） |

**为何未上线**：14:35 有人替换了 `/usr/local/bin/homed`（84,985,720 字节）并重启了
服务（现 PID 1450152）。那个二进制与 main 构建**不是同一份**，比我的大 2.4MB。
直接部署会覆盖它 —— 所以交给你决定何时、以及以哪个版本为准。

实测确认现役二进制不含 kbtree：

```bash
strings /usr/local/bin/homed | grep -c kbtree   # → 0
ss -ltn | grep 9892                              # → 无监听
```

## 上线步骤

**必须按顺序，且第 1 步不能用 `cp`**：

```bash
DATA=${HA_DATA}

# 1. 备份配置库（WAL 模式下 cp 会拿到不一致快照，必须用 .backup）
sqlite3 $DATA/config.db ".backup '$DATA/config.db.bak-$(date +%Y%m%d-%H%M%S)'"

# 2. 确认要部署的二进制
strings /tmp/homed-new | grep -c kbtree    # 期望 ≥ 1；为 0 说明候选不对

# 3. 原子替换（install 内部是 rename，不会写坏运行中进程的映像）
install -m 0755 /tmp/homed-new /usr/local/bin/homed

# 4. 重启
systemctl restart homeagent

# 5. 验证
systemctl is-active homeagent
journalctl -u homeagent --since "-2min" | grep kbtree
ss -ltn | grep 9892
```

### 第 5 步期望看到

```
[kbtree] 已启动 http://127.0.0.1:9892
[kbtree] 外部 agent 可用：GET /tree、/categories、/counts、/search?q=&category=
```

## 部署后的冒烟测试

```bash
cd ${HA_DATA}/skills/knowledge-base

./scripts/kb_tree.sh -h                 # 帮助（不需要 token）
./scripts/kb_tree.sh categories         # 分类列表
./scripts/kb_tree.sh counts             # 各分类条目数
./scripts/kb_tree.sh tree -d 1 -i 0     # 只看第一层结构
./scripts/kb_tree.sh search -q 知识库 -c tech -l 3
```

token 自动从 `config/config.json` 读（权限 600），无需手动 export。
覆盖方式：`KB_TOKEN=xxx ./scripts/kb_tree.sh ...` 或改那个 json。

## 端口与 token

- 监听：`kbtree.listen_addr` = `127.0.0.1:9892`（**仅本机**）
- token：`kbtree.token` 已在 `config_kbtree` 表里设为固定值。
  若要改成随机（每次重启变），把该行 value 置空即可 ——
  但那样每次重启都要重新把 token 告诉所有使用者。
- 改 token 时**两处一起改**：`config_kbtree` 表 + `skills/knowledge-base/config/config.json`。

要对外（别的机器）时改 `listen_addr` 为 `0.0.0.0:9892`，
但**先想清楚 token 怎么分发** —— 它是唯一的屏障。

## 退出码约定（脚本）

| 码 | 含义 |
|---|---|
| 0 | 成功 |
| 2 | 参数错误 / 未知命令 |
| 3 | 令牌无效（401） |
| 4 | 分类不存在（404，stderr 会列出现有分类） |
| 5 | 其它 HTTP 错误 |
| 7 | 连不上（**通常是服务未上线**） |
| 8 | 请求发送失败 |

## 回滚

```bash
install -m 0755 <旧二进制> /usr/local/bin/homed
systemctl restart homeagent
```

配置与 skill 都不影响回滚（kbtree 未启动时它们只是闲置文件）。
