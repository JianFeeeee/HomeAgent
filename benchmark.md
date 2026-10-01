# 跑分计划（benchmark.md）

> 交接文档：供新会话执行。旧会话的测试**已全部停止**，本文件是从头开始的唯一依据。
> 新会话开始时：**先验证本文件每一条「前置事实」再执行**，不要凭记忆或信任旧会话输出。

## 0. 前置事实（新会话必须先逐条重验）

| # | 事实 | 验证命令 | 期望 |
| --- | --- | --- | --- |
| 1 | 跑分工具已就位 | `ls scripts/capability-bench/` | bench.py pi_bench.py compare.py memory_recall.py tasks.example.json tasks.zerobasis.json spawn-instance.sh README.md |
| 2 | 代码提交已完成 | `git log --oneline -8` | 见 §5 提交清单 |
| 3 | 工作区干净 | `git status --short` | 空 |
| 4 | 生产实例未受影响 | `ps -p $(pgrep -f 'homed -data /home/newqqagent' \| head -1) -o pid,etime` | 存活 |
| 5 | 测试进程已清 | `pgrep -f 'memory_recall\|pi_bench\|bench.py'` | 空 |
| 6 | llmsproxy 可用 | `curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $(sqlite3 /home/newqqagent/config.db \"SELECT value FROM config WHERE key='core.llm.api_key'\")" http://127.0.0.1:8081/v1/models` | 200 |
| 7 | pi 可用且版本 | `pi --version`（如 EADDRINUSE 用 `env -u PI_A2A_PORT -u PI_ACP_PORT`） | 0.85.1 |
| 8 | pi 的 llmsproxy provider | `python3 -c "import json;d=json.load(open('/root/.pi/agent/models.json'));print(d['providers']['llmsproxy']['baseUrl'])"` | <http://127.0.0.1:8081/v1> |

## 1. 已知的关键教训（不重犯）

1. **两侧 usage 同名不同义**（已归一，勿改回去）：
   - HomeAgent：`prompt_tokens` **含**缓存（OpenAI 口径）
   - pi：`input` 是**未命中侧**，`totalTokens = input + cacheRead + output`（Anthropic 口径，受控实验实证）
   - ⇒ 两侧各自过归一函数（pi_bench.py `_usage_of` / memory_recall.py `_norm_ha_usage`+`_norm_pi_usage`），汇总只认归一后的键
2. **缓存命中率恒 100% 的假绿已修**（`59c0689`）：上游只报命中侧时 `miss = prompt - read`。修复前 100.0% → 修复后 87.6%。
3. **cli.sock 按行读**：消息含换行 = 拆成多条独立消息（历史全乱，表现为 BrokenPipe）。memory_recall.py 已有 `assert "\n" not in text`。
4. **内核会 dedupe 完全相同的输入**（`skipped:true`）：多轮/重跑必须让每条文本不同。
5. **pi 必须隔离配置目录 + 端口**：`PI_CODING_AGENT_DIR=/var/tmp/pi-iso/agent` + `PI_A2A_PORT`/`PI_ACP_PORT` 给空闲端口。隔离后无扩展 ⇒ `pi -p` 跑完自己退出。
6. **pi 的工具事件**：事件名 `tool_execution_start`、字段 `toolName`（写错只是静默为空）。
7. **★ 超窗校验是判据的前提，不是可选项**：旧 200k 跑分因实例窗口（HomeAgent 1M / pi 600k）大于灌入量 253k，实际测的是「窗口内召回」—— 跑 19 分钟、数字漂亮、完全无效。memory_recall.py 已加 `--expect-window` 校验（对无效配置直接拒跑）；新会话用它，并先实测两侧真实窗口。
8. **改 LLM 配置必须重启实例**：provider 启动时构建，`/settings set` 落库但不生效。
9. **不要用 `pkill -f`**：会匹配到自己这条 shell 命令行。用 pid 文件或 `pgrep` 精确 pid。
10. **面板懒渲染**：WebUI 的 `#chat-panel-chat` 只在 `switchTab('chat')` 后存在；浏览器验证先切页。
11. **本机 diff 是坏的**（PATH 首位对任何输入返回 0）：用 `/usr/bin/diff` 或 Python difflib。

## 2. 测试 A：零基础认知对比（质量，可多实例并行）

**目的**：同模型、同一份材料，只变 harness，测认知而非"翻自己的库"。
（旧版硬伤：knowledge-stats 让 pi 读 HomeAgent 自己的 graph.db ⇒ pi 97s/317k token 全是权限差异，不是能力差异。）

材料：`/var/tmp/zerobasis/{incident-2026-08.md, config-notes.md}`（若不存在，按 git 历史里的任务集重建；材料必须含：512 连接池上限、4200 峰值、v2.30.4、87 订单、"600" **不在**材料里须自己算 200+400、ttl=0=永不过期、N+1、对账批处理）。

任务集：`scripts/capability-bench/tasks.zerobasis.json`（6 任务：检索/算术/推断/抗干扰/综合/多跳，全部带显式 check）。

执行：

```bash
# ① 起隔离实例（窗口两侧都配 200000）
BIN=/home/program/TrueAgent/build/homed \
  bash scripts/capability-bench/spawn-instance.sh a 18082 200000
# ② HomeAgent 侧
python3 scripts/capability-bench/bench.py --socket /var/tmp/ha-a/cli.sock \
  --api-key "$(cat /var/tmp/ha-a/apikey)" \
  --tasks scripts/capability-bench/tasks.zerobasis.json --out /var/tmp/cmp2/ha --timeout 300
# ③ pi 侧（同一任务文件 ⇒ 提示词逐字相同）
python3 scripts/capability-bench/pi_bench.py \
  --tasks scripts/capability-bench/tasks.zerobasis.json --out /var/tmp/cmp2/pi --timeout 300
# ④ 对比
python3 scripts/capability-bench/compare.py --a /var/tmp/cmp2/ha --b /var/tmp/cmp2/pi \
  --label-a HomeAgent --label-b pi --out /var/tmp/cmp2
```

验收：两侧任务集一致（compare.py 会检查）；逐任务看通过与 token；失败原因必须留在 compare.md。

## 3. 测试 B：200k 超窗记忆召回（性能敏感 ⇒ 独占实例，串行跑）

**目的**：多轮对话超出窗口后，早期内容的召回率（HomeAgent 向量裁剪 vs pi 压缩）。

**★ v3 填充模型（`95224eb`，当前唯一有效版本）**

| 版本 | 填充 | 结论 |
| --- | --- | --- |
| v1 | 显式强调针 + 语义空转 | 两例 4/4 满分零区分度 |
| v2 | 端口同构干扰 + **废话流**（天气/树叶/墨盒） | **作废**：废话被压缩直接丢弃 ⇒ 偶发针成填充里唯一的价值内容 ⇒ pi 跑分虚高 |
| v3 | **高密度叙事**（order-gw 运维主线） | 当前版本 |

v3 主线：`上线准备 → 灰度事故 → 修复验证 → 版本发布 → 交接收尾`，
每轮 5 片段、每片段 2-4 条有信息增量的工作项（数值、因果、决策、变更）、
含跨轮引用。针全部嵌在价值信息流里：

- **偶发针** = 服务依赖清单条目（同构干扰 = 其它服务的**真实**端口，同样有意义，只是不是针）
- **覆盖针** = 值班安排改号（`4379 → 4324`，真实变更场景；反向哨兵验证不塌回）
- **多跳** = 交接流程本身（团队→门禁→申请表）

实测证据支持「废话填充虚高」：v3 第 1 轮 HA 模型就同时调用
`memory_recall / doc_query / config_list_plugins / cmd_run` 四个工具，
而 v2 废话填充下只调 `doc_commit` —— 高密度负载才考得出 harness 的工具选择与
循环管理能力。

**★ 踩坑：锚点 frac 必须落在对应 phase 区间内**（生成器自证时抓到）
`narrative_block` 的 phase 由 `frac` 切分，而针锚点也用 `frac`。写
`i == int(total*0.50)` 想在 release 阶段插针，但 `0.50 < 0.61` 实际落在
verify 分支 ⇒ **永不触发**（表现为「覆盖新值找不到」，不报错）。同理
`0.63` 距边界 `0.61` 太近，浮点除法下可能仍在相邻区间。改法：把每个锚点
推到所属区间**内部**（0.52/0.66/0.70/0.76/0.85/0.92），并写落位自证。

**★ 先把两侧窗口真实配成一致（这是上次翻车点）**：

- HomeAgent：`core.llm.sources.deepseek.context_window = 200000`（spawn-instance.sh 第 3 参传 200000），起后 `sqlite3 ... "SELECT value FROM config WHERE key LIKE '%context_window%'"` 验证 = 200000
- pi：把隔离目录 `/var/tmp/pi-iso/agent/models.json` 的 AUTO 模型 `contextWindow` 改成 200000，并用 python3 读回验证

执行（串行，先 A 后 pi；v3 填充实测 `overshoot` 需给 1.45 才能达到 1.23×）：

```bash
python3 scripts/capability-bench/memory_recall.py --harness homeagent \
  --socket /var/tmp/ha-c/cli.sock --api-key "$(cat /var/tmp/ha-c/apikey)" \
  --window 200000 --expect-window 200000 --overshoot 1.45 \
  --out /var/tmp/mem/v3-ha-200k
python3 scripts/capability-bench/memory_recall.py --harness pi \
  --window 200000 --expect-window 200000 --overshoot 1.45 \
  --out /var/tmp/mem/v3-pi-200k
```

验收：工具打印「✓ 超窗校验通过」（否则结果无效，删掉重跑）；报告 recall_rate 按针分层；两侧灌入量同级（v3 = 143 轮 / 245k / 1.23×）。

注意：143 轮/侧，高密度叙事下单轮 ~25-50s，单侧约 1.5-2 小时；用 bg_run 跑且**不要** `| tail`（会缓冲到看不见进度），输出落文件。实时轮数看实例 `run.log` 的 `grep -ac 'text from cli'`（脚本 stdout 重定向后块缓冲滞后）。

## 4. 测试 C：多实例并行吞吐（可选，性能）

spawn-instance.sh 可起多实例（18082/18083/…；pluginmgr 9876 / remotedevice 9890 / kbtree 9892 写死会 bind 失败属预期降级，不影响 cli.sock 标定）。质量类测试可并行；性能类必须独占。

## 5. 提交清单（已在本地 main，未推送）

```
9ca7e34 feat(bench): 能力标定台 —— 经 cli.sock 驱动实例，记录结果与账目
59c0689 fix(usage): 缓存规则收成单一实现 —— 修掉「命中率恒 100%」的结构性假绿
07e15dc feat(context): 上下文阈值接入配置系统 —— 消除 0.8 与 600000 合成的隐形断层
52c2aae feat(webui): 对话页展示缓存命中率与 token 用量
（更早：73a6359 54ba3ff 2f898a6 6d537ca afeba70 a8ed3a8 —— usage 全链路 + 估算器校准）
```

待提交：本文件（benchmark.md）+ capability-bench 新增的 pi_bench.py / compare.py / memory_recall.py / tasks.zerobasis.json / spawn-instance.sh / README 增补。

## 6. 推送与部署（人工确认后才做）

- 推送：`git push origin main`（8+ 提交在本地）
- 部署：`deploy-plan.sh check`（已预检过：onnxruntime ✓ libonnxruntime ✓）→ backup → deploy；deploy 前需人工确认
- 生产适配器：`/home/newqqagent/adapters/` 有 9/10 与 .bundled 清单不符（openai.lua 实测仅版本落后、无用户修改）；部署前逐个核对后清掉让内核重解包（backup 会带走）
