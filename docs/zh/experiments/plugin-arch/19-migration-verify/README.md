# 实验 19：迁移验证工具（Part 6.3）

外部插件从 C ABI 动态库迁移到子进程后的批量重编与开销实测工具。
与 01~18 的性质不同：那些是**决策前**的可行性验证，这两个是**迁移执行期**
反复使用的操作脚本。

## rebuild-plugins.sh

批量把 `example/` 下的插件重编为子进程模式（`plugin.bin`）。

```bash
PLUGINDEV=/tmp/plugindev ./rebuild-plugins.sh weather sanitizer qq
```

关键性质：**不修改任何插件源码**。`plg.json` 的 `entry` 仍写着 `"plugin.so"`
也无妨——工具链已不看这个字段（Part 6.1）。

两个实现细节值得记：

- **成功判定看产物而非退出码**。plugindev 对部分错误只 `fmt.Printf` 不
  `os.Exit`，单看 `$?` 会把失败当成功。
- 构建前清 `build/`+`dist/`。残留的 `.so` 不影响构建，但会让人误以为
  还在用旧通道。

已知环境依赖：`rss` 插件需要 `github.com/mmcdole/gofeed`，
`proxy.golang.org` 不通时用 `GOPROXY=https://goproxy.cn,direct`。

## measure-plugin-overhead.sh

实测 homed + 插件子进程的常驻开销。

```bash
./measure-plugin-overhead.sh $(pgrep -f 'homed -data' | head -1)
```

### 一个统计口径的坑

第一版混用了两个来源：RSS 读 `/proc/pid/status` 的 `VmRSS`，
PSS 读 `smaps_rollup` 的 `Pss`。结果输出 `PSS=87.9MB > RSS=69.1MB`——
物理上不可能。

原因是两者对**共享内存段**的计入方式不同：`smaps_rollup` 的 `Rss` 含
`Pss_Shmem`（共享段的按比例份额），`VmRSS` 不含。现已统一从
`smaps_rollup` 读，保证 PSS ≤ RSS。

### 实测结果（2026-09-02，15 个真实插件）

```
15 个插件进程   RSS=88.0 MB   PSS=87.9 MB   线程=82
均摊            5.87 MB       5.86 MB       5.5 线程
homed 本体      RSS=182 MB    线程=15
```

**与实验 5 基线（17 进程 RSS=29.1MB / PSS=12.9MB / 线程=84）的偏差解释**：

实验 5 用的是 2.68MB 的最小插件，真实插件 3.1~14.8MB（browser 依赖最多）。
RSS 随二进制体积线性增长，故绝对数字不可比。可比的是结构性指标：

| 指标 | 基线 | 实测 | 判断 |
|---|---|---|---|
| 均摊线程 | 4.9 | 5.5 | 同量级，无线程膨胀 |
| PSS/RSS | 44% | 99.9% | **明显差于基线** |

第二项是真实发现：基线里 PSS 远低于 RSS，说明 Go runtime 只读代码页在
进程间共享。实测几乎不共享，因为 15 个插件是 15 个**不同**的二进制，
没有共同的物理页可映射。

这是「每插件独立二进制」的固有代价，不是缺陷，但意味着实际内存开销
高于评估文档（§4.3）的乐观估计。若日后需要压这一项，方向是让插件共享
一个 launcher 二进制 + 各自的业务 plugin，而非各自静态链接整个 runtime。

## 冒烟测试

自动化部分在 `internal/plugins/real_plugin_smoke_test.go`（4 项）：

- `ToolInvokeRoundTrip`：工具真实调用往返（不只是注册）
- `StageRewriteTakesEffect`：sanitizer 改写型 stage 在真实内核装配下生效
- `MultiPluginShareOneSegment`：多插件共享一段，只读插件不覆盖改写结果
- `CrashDoesNotKillKernel`：SIGKILL 插件进程，homed 存活

这些测试用**真实 example 产物**而非 testdata 假插件，且 manifest 刻意写
`"entry":"plugin.so"`——验证「业务代码零改动」这一承诺在完整内核装配下成立。
未重编时 skip 而非 fail，CI 不强制先跑重编脚本。

## 压测与延迟（Part 6.6 验收）

基准与压测在代码里而非独立脚本：
`internal/plugin/proc/bench_test.go` + `streaming_test.go`。

```bash
go test -run '^$' -bench . ./internal/plugin/proc/
go test -run 'TestStreaming_' -v ./internal/plugin/proc/
```

### 实测（2026-09-02，AMD Ryzen 7 7840HS）

| 项目 | 实测 | 基线 | 判断 |
|---|---|---|---|
| 工具调用 RPC 往返 | 24.1 µs | 实验 11: 19.6 µs | 同量级 |
| 锁仲裁（内核侧） | 0.76 µs | — | 见下注 |
| 事件环写入 | 95 ns | — | 亚微秒 |
| 事件环并发写入 | 83 ns | — | 无锁竞争恶化 |
| 完整 stage 往返 | 132 µs | — | 含 3 次进程间往返 |
| 共享段编解码 | 3.7 µs | — | 占 stage 的 2.8% |

**锁仲裁 0.76µs 不可与实验 3 的 19.40µs 对照**——两者测的不是同一个东西：
实验 3 测插件经 RPC 请求锁的完整跨进程往返，本基准只测内核侧
`lockRegistry.acquire/release`。真实成本仍在 20µs 量级（那部分是 RPC 往返）。
基准原名 `BenchmarkStageLockRoundTrip` 有误导性，已改为
`BenchmarkStageLockArbitration`。

**stage 往返 132µs 的成本构成**：共享段编解码只占 3.7µs（2.8%），
其余是**一次 stage 要走 3 次进程间往返**——`stage.invoke` 加上插件侧反向的
`stage.lock` / `stage.unlock`。相对 LLM 往返 2-8 秒可忽略；若日后要优化，
方向是把 lock/unlock 合入 `stage.invoke` 的请求/应答，省掉两次往返。

### 流式压测（§4.3 标记「风险高」的那一项）

原文的担忧：「`Bus.Publish` 路径禁用任何锁/阻塞——流式输出逐 token 发布，
任何等待都会卡顿」。

```
5000 次 Publish + 每条睡 20µs 的慢消费者
  实测 2.29ms，均摊 457 ns/token
  同步语义理论下限 100ms（5000 × 20µs）

订阅者 1 个：1.547ms（515 ns/次）
订阅者 8 个：1.518ms（506 ns/次）   ← 几乎不变，无线性恶化

环溢出（无消费者写 30000 次，cap=8192）：均摊 35 ns/次   ← 仍 O(1)
```

2.29ms 与实验 4 的数字完全一致（那次也是 2.29ms / 0.46µs per token）——
post-and-forget 在实现中成立。

最后一项的意义：消费者完全停摆时写端覆盖最旧 slot，这条路径仍是 O(1)，
故「消费者卡住」不会连带拖慢内核主循环。
