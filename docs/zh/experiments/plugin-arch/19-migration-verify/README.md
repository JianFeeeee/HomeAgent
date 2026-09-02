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
