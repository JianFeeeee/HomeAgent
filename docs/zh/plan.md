# WebUI 布局与配置归位修复计划

## 一、背景

上一轮 SDK 接口化改造完成并部署后，用户指出三个问题：

1. **WebUI 窄屏布局损坏**：顶部 `<nav>` 为桌面式横排（标题 + 6 tab + 连接指示器 + 语言 + 主题），
   窄屏断点仅缩小字号不换行，`body { overflow-x:hidden }` 直接把溢出的 tab 裁掉不可点击。
2. **OpenClaw skills 目录被注册为核心配置**：`core.skills.path`（"OpenClaw 技能存储目录"）注册在核心
   配置表（`internal/config/registry.go`），但全仓无任何读取方（死配置）；实际生效路径是
   clawhubadapter 自己的 `skills_dir` 配置（`config_clawhubadapter` 表 + `core.daemon.data_dir`/skills 兜底）。
   技能目录是 clawhubadapter 适配加载的领域，不应属于核心配置。
3. **clawhubadapter 加载的微信插件成为独立配置项**：设置页出现 `channels.wechat.*`（核心表）、
   `plugin.wechat.*`（config_wechat 表）、`config_openclaw_weixin`（空表）等多处微信配置，
   全部为历史残留——当前代码零引用，OC 技能的配置实际在其自身 `~/.openclaw/openclaw.json`。
   设置页会把核心表全部键 + 全部插件表当作配置组展示，导致残留以"独立配置项"形态出现。

## 二、修复计划

| # | 动作 | 位置 | 风险 |
|---|------|------|------|
| A | 窄屏导航修复：<768px 下 nav 横向滚动、h1 缩写、连接指示器简化；body 溢出裁切改为 nav 内滚动 | `cmd/gui/renderer/style.css` | 无 |
| B | 删除 `core.skills.path` 核心配置注册（set + RegisterDef 两处） | `internal/config/registry.go` | 无（无读取方） |
| C | 备份后清理残留配置：`channels.wechat.*` 键、`config_wechat` / `config_openclaw_weixin` / `config_openclaw` 表（含微信 token，先备份） | 生产库 `/home/newqqagent/config.db` | 低（当前代码不读） |

## 三、实施记录

### 步骤 A：webui 窄屏导航修复（已完成）
- 修复对象为 webui HTTP 服务真正前端 `internal/plugins/webui/dashboard.html`（`go:embed` 内嵌，
  登录后 `/` 返回，104KB；cmd/gui 是独立 electron 客户端，非 webui 一部分）。
- `<768px` 断点：`nav { overflow-x:auto; scrollbar-width:none; flex-wrap:nowrap }` + `::-webkit-scrollbar { display:none }`；
  `nav a { white-space:nowrap; flex-shrink:0 }`；`nav h1 { font-size:0 }`（保留 logo 图、隐藏文字，弥补窄屏空间）；
  `nav > div { flex-shrink:0 }` 右侧语言/主题/退出按钮不压缩。
- 顺带在 cmd/gui（electron 客户端）同步了窄屏样式与消息来源徽标（`app.js`/`style.css`，客户端窗口缩放同样受益；
  客户端需另行构建 electron 应用才生效）。
- 验证：部署后 `/` 返回的 dashboard 含 `scrollbar-width:none`/`font-size:0`/`::-webkit-scrollbar` 规则。

### 步骤 B：删除 core.skills.path 核心配置（已完成）
- 删除 `internal/config/registry.go` 两处：`set("core.skills.path", ...)`（SeedDefaults）与
  `reg(ConfigDef{Key:"core.skills.path", ...})`（定义注册）。
- 理由：该键全仓无读取方（grep 仅命中注册处），实际生效路径是 clawhubadapter 的 `skills_dir`
  （config_clawhubadapter 表 + `core.daemon.data_dir`/skills 兜底）。技能目录属 clawhubadapter 适配领域。
- 验证：`grep -rn "skills.path" --include="*.go"` 零命中；部署后设置页无 `core.skills.path`。

### 步骤 C：清理生产库残留配置（已完成）
- 操作前 `sqlite3 .backup /tmp/opencode/config.db.pre-clean.bak`（含微信 token 数据）。
- 删除：`config` 表 `channels.wechat.*` 3 键 + `core.skills.path` 键；`DROP TABLE config_wechat /
  config_openclaw_weixin / config_openclaw`（三者均为历史残留：当前代码零引用，clawhubadapter 实际
  使用 config_clawhubadapter 表；OC 技能配置在其自身 `~/.openclaw/openclaw.json`）。
- 验证：设置页总键数 138→129，无 wechat/weixin/skills.path 残留，`plugin.clawhubadapter.skills_dir /
  simulator_dir` 正常；服务 healthcheck ready、clawhubadapter "OC plugin manager started"。

---

# SDK Stop 注册接口（RegisterStopHandler）计划

## 一、背景

2026-08-01 20:00 起生产 homeagent 进入崩溃循环（`fatal error: thread exhaustion`，
systemd 重启计数 61+）。排查定位为 SDK 示例插件 `calendar`（示例源码在 SDK 仓库
`example/calendar`，生产以 plugin.so 形态加载）三个缺陷叠加：

1. **农历引擎 3 个 bug**（`daysInLunarYear` 位循环 `i > 0` 应 `i > 0x8`、缺闰月天数、
   `lunarToSolar` 内层重复加闰月）→ `lunarToSolar(2026,4,12)` 返回 **2062-11-16**（偏移 36 年），
   农历重复事件（`lunar_yearly`）的 next 被生成到遥远错误日期。
2. **`cleanupPastEvents` 保留过时重复事件** → 每 30s ticker 对已到点的重复事件再生成一份 next，
   事件从 7 个爆炸到 **45612 个**（15MB events.json）。
3. **无提醒投递保护**：15018 份同时到点的事件一次性 `go sdk.InjectInterruptText(...)` 投递
   → interrupt 风暴 → goroutine/线程耗尽。

处置：修复农历引擎 3 处 + next 去重 + 清理过时重复事件，用**新版 SDK 仓库 + 新版 plugindev 工具链**
重建 `calendar_linux_amd64.hmap`，经 **webui `POST /api/v1/plugins`**（透明代理到 pluginmgr 安装接口）
重装，重启验证收敛（事件 4 个、next 正确生成 2027-05-17、0 崩溃）。

**过程中暴露的能力缺口**：SDK 只有 `Plugin` 接口的 `Name/Start/Stop`，**没有 stop 注册接口**
（`RegisterStopHandler`/`OnStop` 均不存在，SDK v0.7.2/v0.8.0/master 一致）。插件停止时只能在自己的
`Stop()` 里写清理逻辑，SDK 层无法统一执行"停止时清理"回调；calendar 的 `Stop() { p.saveEvents() }`
还会用陈旧内存把已清理的数据写回磁盘（曾导致删除的重复事件复活）。

## 二、计划

| # | 动作 | 位置 | 风险 |
|---|------|------|------|
| 1 | 公共 SDK `PluginSDK` 加 `RegisterStopHandler(fn func())` + `RunStopHandlers()`（幂等、后注册先执行），两处同步 | `third_party/homeagent-sdk/sdk/plugin.go`、SDK 仓库 `sdk/plugin.go` | 低（纯新增，内置 SDK 内嵌透传） |
| 2 | 内核 Registry 保存每插件 SDK 引用（`sdkRefs`），`StopAll`/`ReloadOne`/`DisablePlugin` 调 `Stop()` 前执行 `RunStopHandlers` | `internal/plugin/registry.go` | 中（生命周期路径，需回归 reload/disable） |
| 3 | 工具链 plugindev：z_bridge 模板 `bridgeState` 存 SDK，`StopPlugin` 先 `RunStopHandlers()` 再 `plugin.Stop()`；init 脚手架模板加演示 | SDK 仓库 `tools/plugindev/templates.go`、`templates/main.go.tmpl` | 低 |
| 4 | 内置示例插件演示（如 timer：ticker 停止改为 stop handler） | `internal/plugins/timer/plugin.go` | 低 |
| 5 | 外部示例插件同步（`example/calendar` 的 `saveEvents` 改由 stop handler 执行，验证 z_bridge 链路；其余 example 加演示） | SDK 仓库 `example/*` | 低 |
| 6 | 文档同步：SDK README 生命周期章节 + 主仓插件开发文档 | SDK 仓库 `README.md`/`README_EN.md` 等 | 无 |

## 三、实施记录

1. SDK 公共层（`RegisterStopHandler` + `RunStopHandlers`：后注册先执行、执行后清空幂等）已落地
   `third_party/homeagent-sdk/sdk/plugin.go`，并同步到 SDK 仓库 `/tmp/opencode/sdk-repo/sdk/plugin.go`（两处一致）。
2. 内核 Registry（`internal/plugin/registry.go`）新增 `sdkRefs map[string]*sdk.PluginSDK` + `runStopHandlers`，
   `loadOne` 注册、`StopAll`/`ReloadOne`/`DisablePlugin` 在 `Stop()` 前执行（共 4 处调用点）。
3. 工具链 plugindev（SDK 仓库）：linux `tmplLinuxBridge` 的 `go_stop_plugin` 先 `RunStopHandlers()` 再 `plg.Stop()`；
   windows `tmplBridge` 的 `bridgeState` 加 `sdk` 字段、`StopPlugin` 同链路；`tmplPluginGo` + `main.go.tmpl`
   脚手架加 `RegisterStopHandler` 演示。plugindev 重新编译通过（GOPATH=/root/go）。
4. 内置 timer 插件演示：`close(p.stopCh)` 移入 stop handler，`Stop()` 只 `wg.Wait()`。
5. 外部示例：`example/calendar` 的 `saveEvents` 改为 `s.RegisterStopHandler(p.saveEvents)`，
   `Stop()` 删除写盘调用（持久化交由 stop handler，避免陈旧内存复活已删事件）。
6. 文档：SDK 仓库 `README.md`/`README_EN.md` 生命周期章节补充 RegisterStopHandler 说明。
7. 构建测试：主仓 `go build ./...` + `go test ./internal/sdk/... ./internal/plugin/...` 全绿；
   SDK 仓库 `go build ./...` + `go test ./sdk/...` 全绿。
8. 生产部署验证：新 plugindev（--no-bundle）重建 `calendar_linux_amd64.hmap`（md5 084e97c0…，strings 确认
   `go_stop_plugin → RunStopHandlers → saveEvents` 编译进 plugin.so）；webui API 删旧装新；重装新内核
   homed（含 sdkRefs/runStopHandlers）；两次重启事件稳定 3 个不复活、events.json mtime 与 stop 时刻吻合
   （saveEvents 经 stop handler 真实执行）、0 次 thread exhaustion、服务 active。


---

# clawhubadapter OpenClaw 通道插件兼容修复计划

## 一、背景

生产 `core.llm.provider` 已是 mock LLM 源（`core.llm.sources.mocktest`，base_url
`http://127.0.0.1:18080/v1`、model mock-model、adapter openai），mock LLM 服务常驻运行。
借助 **mock 通道插件**（`/tmp/opencode/mock-skills/mock-wechat/`，完全复刻 openclaw-weixin 的
真实注册格式 `register(api) → api.registerChannel({ plugin: ChannelPlugin })`）放入生产 skills 目录
端到端复现，得出如下结论：

**已验证可用链路**：manager 加载 mock 插件 → Go 端识别 `ocplugin mock-wechat handled by manager` →
注册工具 `mock-wechat_read_mock_wechat_input`/`mock-wechat_mock_echo` → `RegisterOutputChannel("mock-wechat")`
→ mock 自推消息经 `channel_input` 通知 → `[agent] interrupt from manager/mock-wechat` →
mock LLM 正常回复（195ms）。

**复现的核心缺陷**（真实通道插件 wechat/dingding"根本不可用"的根因）：

1. **输出断链**：manager `tools/call` 通道分支只认 `channelPlugin.outbound.sendText/sendMedia`
   （旧格式），真实 ChannelPlugin（openclaw-weixin 等）无 outbound →
   `tools/call mock-wechat → error: "channel mock-wechat has no output handler"`。
2. **生命周期静止**：manager mock api 从不调用 `gateway.startAccount/stopAccount`，也无
   `api.runtime`/`channelRuntime` → 通道插件加载后永不启动（不登录、不轮询、不收消息）。
3. **输入依赖错位**：真实插件把消息经 `channelRuntime.reply.dispatchReplyWithBufferedBlockDispatcher`
   推送（manager 完全无此对象），而不是调 `api.submitInput`。
4. **stdout 污染**：插件 `console.log` 直接进 JSON-RPC 流，Go 端 readLoop 跳过非 JSON 行，有丢通知风险。

**wechat 通道的心跳机制**（`openclaw-weixin/dist/index.js` `pollLoop`，448 行起）：每账号一个常驻
`pollLoop`，循环 `POST ilink/bot/getupdates`（body `{get_updates_buf}`，超时 35s）——**长轮询即心跳**：
服务器收到 poll 请求即知通道在线，新消息随 poll 响应 push 回来；超时视为空响应继续轮询，真错误延时
5s 重试。**与 gateway 生命周期强绑定**：

- `pollLoop` 只由 `gateway.startAccount(ctx)` 启动；不被调用 → 心跳/收消息全断（服务器侧认为通道离线）。
- `startAccount` 末尾 `await new Promise(()=>{})` **永久挂起**——OC gateway 靠它配合 health-monitor
  （startAccount 退出 → 判定账号崩溃 → 重启账号）。manager 调 `startAccount` 必须 **fire-and-forget**。
- 停靠 `gateway.stopAccount(ctx)`（`ctx.account.accountId` 定位），停止时经 `statusSinks` 调
  `ctx.setStatus({running:false, connected:false, lastStopAt})`；启动即上报
  `ctx.getStatus()/ctx.setStatus({...running:true, connected:true, lastStartAt})`——`connected` 是
  gateway 判断账号存活的依据。
- `sendTyping`（ilink/bot/sendtyping + typing_ticket）是打字指示，非心跳，无需支持。

## 二、修复计划

| # | 动作 | 位置 | 风险 |
|---|------|------|------|
| A | manager 提供 **gateway 生命周期桥**：channel 插件注册后自动 `gateway.startAccount(ctx)`（fire-and-forget，不等待挂起的 Promise），构造完整 ctx `{account, cfg, channelRuntime, getStatus, setStatus}`；Go 端 `channel_stop` 通知 → `stopAccount` | `internal/plugins/clawhubadapter/manager/main.js` | 中 |
| B | 实现 **channelRuntime mock**：`reply.dispatchReplyWithBufferedBlockDispatcher`（deliver 回调 → `channel_output` 通知送 Go 端）、`getPolls`（OC 通用通道轮询输入）、`call` 透传 | 同上 | 中 |
| C | `tools/call` 通道分支改造：无 `outbound` 的 ChannelPlugin 改走 channelRuntime 事件式发送（agent 输出 → deliver），不再报 "no output handler" | 同上 | 低 |
| D | 状态上报透传：`setStatus` 经 `channel_status` 通知 → Go 端可查；health-monitor 语义（startAccount 保持挂起） | 同上 | 低 |
| E | stdout 卫生：插件 `console.log` 重定向 stderr（或 JSON-RPC 流感知封装），杜绝污染 | 同上 | 低 |
| F | Go 端：`channel_output`/`channel_status` 通知接入（事件分发），通道输出 handler 保持 `sp.CallTool` | `internal/plugins/clawhubadapter/registry.go`、`plugin.go` | 中 |
| G | 端到端验证：mock 通道插件 + mock LLM（生产环境，临时放入/移出 skills 目录）复跑全链路（登录启动→收消息→回复→出站→停止） | 生产 | 低 |

## 三、实施记录

（逐步填写）

1. **manager/main.js — 通道运行时与生命周期桥（已完成，独立运行验证）**
   - `makeChannelRuntime(chName, ch)`：`reply.dispatchReplyWithBufferedBlockDispatcher(opts)` 提取
     `dispatcherOptions.deliver`/`typingCallbacks` 按 `ctx.AccountId` 挂到 `ch.deliverers`，随后
     `notify('channel_input', {channel, payload:{content: BodyForAgent||Body, from, sessionKey, accountId,
     messageSid, chatType, raw}})` 入站；返回 dispatcher（sendNow/addToBuffer/sendBuffer/closeBuffer）。
     `chatPolls`/`getPolls` 空转（防断连误判）、`call` 转发 `channel_output` 通知。
   - `startChannels(name)`：channel 插件注册后自动枚举账号（`config.listAccountIds`→`resolveAccount`，
     缺省 `['default']`），构造完整 ctx `{account, cfg, channelRuntime, getStatus, setStatus}`，
     **fire-and-forget** 调 `gateway.startAccount`（真实插件会永久挂起，绝不等待）；崩溃/状态变更经
     `channel_status` 通知透传（health-monitor 语义：startAccount 不退出=账号存活）。
   - `stopChannels(name)`：逐个账号 `gateway.stopAccount`；进程 SIGTERM/SIGINT 时统一执行优雅停靠。
   - `tools/call` 通道分支：保留 outbound（旧格式）→ 新增 **deliver 事件式发送**
     （`deliverItem` 按 accountId 取 deliver + typingCallbacks.onReplyStart/onCleanup 包裹）→
     无 deliver 时降级 `channel_output` 通知 → 兜底报错。不再出现 "has no output handler"。
   - **stdout 卫生**：全局 `console.log` 重定向 stderr，JSON-RPC 流仅承载协议帧。
2. **mock 插件升级（/tmp/opencode/mock-skills/mock-wechat/index.js）**：完全复刻真实 weixin 行为——
   `gateway.startAccount` 永久挂起 + `setStatus` 上报 + `setInterval` 心跳轮询 + 800ms 后经
   `dispatchReplyWithBufferedBlockDispatcher` 推送入站（deliver 本地记录发送）；`stopAccount` 停轮询+状态置否。
3. **manager 独立运行验证（通过）**：`channel_status` 启动上报（running=true connected=true）；
   `tools/call mock-wechat` → `{"status":"sent","via":"channelRuntime.deliver"}`，插件 deliver 收到
   `text="hello from agent"` 且 typing onReplyStart/onCleanup 正确包裹；心跳 poll #1-4 常驻；
   SIGTERM → `stopAccount called` 退出码 0；插件 console 输出全部走 stderr（协议流零污染）。
4. **Go 端通知接入（plugin.go translateAndRegister + registry.go 状态缓存）**：`channel_status` 存
   `channelStatus` map（可查）+ 日志；`channel_output` 降级事件日志。`go build ./...` 通过。
5. **回复闭环修复（同步注入）**：`channel_input → InjectInterruptText` 的 InputEvent 不带 ResponseCh
   （internal/agent/io/channel.go:276），agent 回复在 emitResponse（eventloop.go:384）被静默丢弃。
   改为 `s.InjectInputSync(pluginName, channel, "text", payload)`（内部 SDK 已有 4 参版本，返回
   `*OutputEvent`）同步等待回复 → 提取 `Payload["content"]` → `sp.CallTool(channel, {payload, meta})`
   → manager `tools/call` → deliver → 插件发送 → 微信送达。公共 SDK IOInjector 同步补
   `InjectInputSync(source, channel, text) string`（ioAdapter 实现，供外部插件一致使用）。
6. **mock LLM 恒定文本化**：/opt/llm-mock/mock_server.py `decide()` 删除工具调用分支，一律回文本
   （"无论收到什么消息都通过微信插件发送"），保证每条入站消息回复必然走通道输出。
7. **生产微信闭环验证（通过）**：用户微信发"你好..." → pollLoop 收到 → dispatchReply →
   InjectInputSync → mock LLM 回文本 → CallTool(wechat) → deliver → `POST ilink/bot/sendmessage`
   → **status=200 message_id=7489545365740590088**，微信收到"（mock）已收到消息，长度 324 字符。"
8. **通用性审查（无硬编码）**：manager/plugin.go/registry.go 均无 weixin/wechat 特判，全部按 OC 规范
   字段实现（gateway/config/capabilities/channelRuntime/deliver/typingCallbacks）。修正规范签名参数
   约定：`listAccountIds(cfg)`、`resolveAccount(cfg, accountId)` 正确传 cfg。
9. **已知边界**（非硬编码，架构性）：a) `channelRuntime.getPolls/chatPolls` 返回空 msgs——依赖
   runtime 轮询输入的通用通道型插件收不到消息（weixin/dingding 类自带 pollLoop 的通道不受影响）；
   b) `gatewayMethods` 登录流程（web.login.start/QR 扫码）未实现（CLI 有 stub），通道凭 token
   配置直连；c) startAccount ctx 提供 account/channelRuntime/cfg/getStatus/setStatus 核心字段。
10. **补充边界 a) getPolls/chatPolls 消息源（已完成）**：manager `makeChannelRuntime` 的
    `chatPolls/getPolls` 改为读 `ch.pollQueues`（按 accountId 队列，poll 取走即消费）；新增
    `channel/send` RPC（Go 端注入 → 队列 → 插件轮询取走）；Go 端 `SendToChannel(channel, payload)`
    + `ChannelSender()` 单例（Start 时置位）。验证：mock-poll 插件（纯 chatPolls 轮询型）——
    `channel/send` → `{"status":"queued"}` → `[mock-poll] poll got msg` → dispatchReply →
    `channel_input` 入站完整（content/from/sessionKey/accountId/messageSid/chatType）。
    微信链路回归正常（Polling started + channel_status running=true）。
11. **边界 b) 登录流程核实（已解决，无需实现）**：真实登录机制是 **SKILL 脚本旁路**——
    `weixin-openclaw-login` SKILL 的 `scripts/get-login-url.js`（ilink 二维码 URL）+ 
    `poll-login-status.py`（轮询扫码状态）→ agent 经 exec 执行 → 拿 bot_token 写入
    `~/.openclaw-weixin/account.json`（2026-07-28 17:31 创建，token 有效）→ 插件启动
    `resolveAccountData` 直读。不依赖 manager gatewayMethods（web.login.start 等 OC gateway
    协议 stub 不影响真实使用）。
12. **clawhubadapter 全量管理接口（已完成并验证）**：向 agent 暴露完整插件/通道管理面——
    - 新工具：`clawhubadapter_plugin_info`（类型/工具/关联通道详情）、`plugin_reload`（reloadPlugin）、
      `channel_list`（全部注册通道 + 实时状态）、`channel_send`（SendToChannel 投递）、
      `channel_start`/`channel_stop`（manager `channel/start`、`channel/stop` RPC）。
    - manager 新增 RPC：`plugins/channels`（registeredChannels 摘要含 status/accounts）、
      `channel/start`（fire-and-forget startChannels 恢复账号）、`channel/stop`（stopAccount，
      按 channel 全停或按 accountId 单停，省略 channel 则全部停止）。
    - Go 侧 `channelSummary(mgr)` 合并 manager 注册信息与 `channel_status` 实时缓存（缓存优先）。
    - 端到端验证（生产实例，LLM 为本地 mock 源）：微信发"你好通道..." → agent 执行
      `channel_list` → `- wechat | plugin=openclaw-weixin type=text running=true connected=true
      accounts=[default]` → 回复送达；发"注入..." → 执行 `channel_send` → "消息已投递到通道
      wechat" → 回复送达。standalone manager 另验证 `channel/start`（mock-wechat startAccount
      重新执行、心跳恢复）与 `channel/stop`（stopAccount called）。
13. **插件删除回调 onRemove 全套（已完成并验证）**：`RegisterOnRemoveHandler`（仅卸载触发、
    重载/禁用不触发，与 stop handler 互补——stop 每次停止都执行）。registry.RemovePlugin 流程：
    stop handlers → Stop → runOnRemoveHandlers → 移除 plugins/sdkRefs/instances →
    UnregisterPluginTools → **配置清理**。配置清理含两层：`ConfigRegistry.RemovePlugin`
    删除 defs 中 `plugin.<name>.*` 配置项定义 + DROP `config_<name>` 插件配置表
    （含用户设置值，ListPlugins 基于 config_% 表枚举故配置区完全消失）；已用临时程序验证
    （before: defs=1/plugins=[timer] → after: defs=0/plugins=[]）。示例盘点（SDK 仓库 15 个）：
    calendar（events.json）、memo（memos.json）、rss（订阅数据目录）、weather（缓存目录）已加；
    files（filesDir 为用户配置的访问根目录，默认 /）、bili/qq（用户下载资产）、
    ocr（函数内 defer RemoveAll 自清理）按语义不加；plugindev 模板 main.go.tmpl + README.tmpl
    含 onRemove 演示；SDK README/README_EN 生命周期文档补"删除清理（onRemove）"小节。

14. [2026-08-03] SDK 工具链/打包/重装 + dlclose 修复：
    - 工具链源码位置澄清：SDK 仓完整内容位于 third_party/homeagent-sdk（主仓 .gitignore 仅跟踪
      sdk/meta/go.mod，"两个远程仓库各取所需"；/tmp/opencode/sdk-repo 为工作克隆，远程=gitcode）。
    - 工具链支持公共 IOInjector.InjectInputSync：CORE_INJECT_INPUT_SYNC=47（C 桥 dispatchIO
      callString 回传回复文本）；主仓 cabi loader case 47 用内部 4 参版 InjectInputSync 取
      OutputEvent.Payload["content"] setResult（meta.go ID 47 + loader.go，主仓 3f252ed）。
    - plugindev 构建环境：GOMODCACHE=/root/go/pkg/mod（yaegi 缓存所在）、GOPROXY=off。
    - 工具链打包 memo：plg.json BOM 去除、name_en "Memo/Notes"→"Memo"（toSnake 不处理斜杠，
      name_en 带 / 会使 hmap 名含子路径报错）；bundle=true 时走全平台交叉编译（本机无 darwin
      工具链），打包用 `build --no-bundle --target linux/amd64`；产物 dist/memo_linux_amd64.hmap。
    - 重装：pluginmgr HTTP API（127.0.0.1:9876）DELETE /plugins/memo 卸载（走内核 RemovePlugin
      + onRemove）→ POST /plugins binary body 传 hmap（返回 installed+checksum）；生效用
      webui `POST /api/v1/plugins/reload`（X-API-Key，生产 admin123）。
    - 关键 bug：Linux dlopen 同路径复用旧句柄——RemovePlugin/ReloadOne 只 Stop 不 dlclose，
      插件二进制更新后重载仍执行旧代码（生产 memo 装新版仍注册旧 3 工具）。修复：
      cabiPlugin.Close()（handle.Close）+ Registry.closeDynamic 在卸载/重载时调用（主仓 649e312）。
      生产 homed-new7 验证：memo 6 工具（memo_todo_add/complete/list + memo_memo_create/list/delete）
      注册正常，wechat 通道 running。
    - SDK 仓推送 b6e30f9（工具链 47 + 重建 bin 二进制 + memo plg.json + sdk/plugin.go 注释精简）。
15. [2026-08-03] 嵌套 git 恢复 + 工作区清理 + codegraph 索引修正：
    - 嵌套 git 恢复：third_party/homeagent-sdk 原本是"单仓库双提交"（目录内嵌套 .git 推 gitcode
      homeagent-sdk 仓，主仓 git 跟踪 sdk/meta/go.mod 推 HomeAgent 仓），嵌套 .git 此前被误删；
      已从 /tmp/opencode/sdk-repo 复制 .git 恢复（remote=homeagent-sdk.git，HEAD=b6e30f9，工作区干净），
      主仓 git 不受影响。以后 SDK 改动直接在 third_party 内 git commit+push（SDK 仓推送仍用带凭据
      URL https://JianFeeeee:REDACTED_GITCODE_TOKEN@gitcode.com/JianFeeeee/homeagent-sdk.git），
      不再经 /tmp 中转。
    - /tmp 清理：删除 /tmp/opencode/sdk-repo、hasdk-fresh、plugindev-new、plugindev_new、mock-run、
      lunartest（SDK 中转/临时目录）；保留 homed-new*（生产二进制备份）、mock-skills 等非 SDK 内容。
    - replace 修正：go.mod 第 17 行已是 `./third_party/homeagent-sdk`（正确）；package-linux.sh
      prepare_gomod 优先用 $PROJECT_ROOT/third_party/homeagent-sdk，仅缺失时才 clone /tmp/homeagent-sdk
      兜底（主仓 108faac）。
    - codegraph 索引修正：根目录 codegraph.json（PROJECT_CONFIG_FILENAME）配
      includeIgnored+include: ["third_party/homeagent-sdk"]，codegraph index 后 Files 179→233，
      third_party 文件 7→61，tools/plugindev 与 sdk/plugin.go（InjectInputSync 等）均可查询
      （此前嵌套 SDK 仓被主仓 .gitignore 挡在索引外）；codegraph sync 不感知配置变更，需 index 全量重建。
