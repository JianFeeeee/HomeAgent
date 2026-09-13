# 内核二进制压力测试（kernel-stress）

对本仓库**编译出来的真实内核**做压力测试 —— 与 `go test` 的区别是：它跑真二进制、
真插件加载、真 unix socket 协议，因此能抓到只在集成面上出现的问题
（已有战绩：根 agent `DataDir` 漏接线、插件通道没登记为 inputch、create 后子不开工）。

## 为什么必须放在私有 netns 里

生产实例占着 `*:8080 / *:9890 / *:9876`，而插件的监听都设了 `SO_REUSEADDR`：
同机再起一个实例会**在 127.0.0.1 上与之并存绑定**（实测抢到过 `127.0.0.1:9890` 约 1 分钟）。
`unshare -n` 后实例只有 `lo`，结构上不可能碰到生产端口。
unix socket 是文件系统对象，跨 netns 仍可驱动，所以驱动脚本在 netns 外也能用。

## 前置

```bash
go build -o /tmp/homed-stress ./cmd/homed      # 被压的内核
export GOCACHE=/tmp/gocache GOPATH=/tmp/gopath TMPDIR=/var/tmp/gotmp
```

## 用法

```bash
# 1) 准备数据目录 + 把 LLM 指向本地 mock（无外网也能跑，且快、可控）
DATA=/var/tmp/kstress
mkdir -p $DATA
#  先跑一次实例建出 config.db，再写入下面这些键（也可直接复用现成目录）：
#    core.llm.provider=mock
#    core.llm.sources.mock.base_url=http://127.0.0.1:9099/v1
#    core.llm.sources.mock.model=mock   api_key=mock   adapter=openai
#    core.llm.sources.mock.adapter_path=adapters/openai.lua   priority=100
#    core.defaults.llm_endpoints=http://127.0.0.1:9099/v1/models   # 探活端点（探活用 HEAD！）
#    core.defaults.rollback.max_retries=100000   auto_rollback=false
#  并把 core.llm.sources.deepseek* 删掉（netns 里它不可达，会让 agent 被判 degraded → rollback 循环）

# 2) 起 mock LLM + 内核（都在同一个私有 netns 里）
MOCK_DELAY_MS=300 MOCK_CHUNKS=8 ./launch.sh /tmp/homed-stress

# 3) 取认证密钥（cli 插件回落到 webui.api_key）
export KCLI_KEY=$(sqlite3 $DATA/config.db "select value from config_webui where key='api_key';")

# 4) 压
./kcli.py $DATA/cli.sock stats full            # 看内核状态（含 scheduler 计数）
./stress.py $DATA/cli.sock "$KCLI_KEY" 16 12 4 20 dense 0.05   # 16 连接×12 输入 + 4 线程×20 中断
./stress.py $DATA/cli.sock "$KCLI_KEY" 12 12 1 25 0.0 3.0      # 稀疏中断 ⇒ 压抢占/挂起/恢复
./stress.py $DATA/cli.sock "$KCLI_KEY" 1 1 0 0 resident 0.0    # 驻留子全链路（mock 见 !resident 标记）
```

## 读结果

| 指标 | 含义 |
|---|---|
| `executed` / `rejected` | 任务执行数 / 被拒数（压力下应为 0） |
| 峰值 `峰值_队列` / `峰值_待处理中断` | 采样到的最大排队深度 / 待处理中断数 |
| `suspended` / `resumed` / `preempted` | 抢占三件套。**中断要放稀**才会打在高优先级任务上：密集中断会互相同级（L4 vs L4）不抢占，数值会很低 |
| `max_suspend_depth` | 中断栈结构上界（= 中断级数 4） |

## 已知坑

- 探活用 **HEAD**（`internal/network/monitor.go`）：mock 必须实现 `do_HEAD`，否则 501 → 判不可达 → agent degraded → rollback 循环（会 reset 合并目录）。
- CLI 协议**每条连接串行**处理（`handleChat` 阻塞到本次响应产出）：单连接狂发只会排成一条线，造不出队列压力 —— 必须多连接。
- 认证：连接后先发 `/auth <key>`，否则一切命令返回 `unauthorized`。
- `mockllm.py` 的 `!resident` / `!notify` 标记用来让 mock 回工具调用，从而在真内核里驱动 `resident_agents` / `notify_parent`。
