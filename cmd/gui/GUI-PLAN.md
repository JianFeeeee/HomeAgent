# GUI 改造计划与进度

> 本文件是 GUI（`cmd/gui`）当前改造的**单一事实来源**：做完了什么、为什么这么做、
> 哪些结论是实测得到的、哪些还没做完。
>
> 相关文档：
> - `cmd/gui/DESIGN-NOTES.md` —— 借鉴 PiDeck 的设计与实现
> - `SERVER-HANDOFF.md` —— 需要服务端同步配合的部分
> - 判据：`cmd/gui/*.test.mjs`，`npm test` 全绿为准（当前 127 条）

---

## 一、当前状态

分支：`feature/gui-starmap-pulse-vendor-align`（不合入 main）

最近提交：

| commit | 内容 |
|---|---|
| `02566f3` | 修启动期误判服务端未启动；keybd_event 类型名致组合键全废 |
| `0d8b129` | computeruse 补高级操作（两端 9→18）；修 HiDPI 坐标；记录 PiDeck 借鉴 |
| `3059c99` | 加载动画改三点式 + 新增系统通知 |

判据：`cd cmd/gui && npm test` —— **127 条全绿**（其中 `computeruse.test.mjs` 43 条）。

---

## 二、已完成

### 2.1 computeruse 能力面：9 → 18 个动作

此前 GUI 侧（`cmd/gui/main.js`）与 waiter 侧（`cmd/waiter/device.go`）都只有
9 个基础动作，与业界 computer-use 的基本盘差距明显。

GUI 侧现支持 **18 个**（已用脚本从 `main.js` 的 `case "computeruse"` 块精确提取核对）：

```
click       doubleclick  rightclick   middleclick  tripleclick
move        hover        drag         mousedown    mouseup
scroll      keypress     hotkey       combo
type        wait         sleep        display
```

waiter 侧（Linux）为 **20 个** —— 比 GUI 多 `middle`、`right` 两个
「button 简写形式」，少 `physical` 相关处理；其余一致。

要点：

- **拖拽分步移动**（默认 12 步，上限 60）。一步到位会让 canvas 拖拽、列表排序、
  滑动条失效 —— 这类应用依赖中间 `mousemove` 事件。
- **组合键归一化**（waiter 侧 `normKeySpec`）。旧实现把模型给的 key 字符串原样丢给
  `xdotool key`，于是 `"Ctrl+C"`、`"ctrl + c"`、`"CTRL-C"` **全部失效**。
- **`type` 加 `--clearmodifiers`**（waiter 侧）。避免残留的 Ctrl/Alt 把后续输入
  变成快捷键。
- **scroll 修正**（两端）。旧实现固定滚 `click 4/5`，忽略方向语义也不支持横向。

### 2.2 修 HiDPI 坐标错位 ⚠️ 重要，详见第五节

截图给模型的是**物理像素**，Win32 坐标 API 在 Electron 里也是物理像素，
**不需要换算**。详见第五节的实测与踩坑记录。

### 2.3 修启动期误判「服务端未启动」

`isServerRunning()` 原硬编码 `http://localhost:8080/`。实测该部署下：

```
localhost:8080      -> 000 连不上
192.168.2.60:8080   -> 302 正常（homed 跑在 WSL 里）
```

于是每次启动都：探测失败 → 误判服务没起 → 拉起 GUI 旁并不存在的 `homed.exe`
→ 干等 8s 超时 → 打印 `homed failed to start within timeout`。
该 ERR 曾累计出现数十次（实测 40+ 次，日志轮转后计数会变），
且是 `gui.log` 里唯一的错误项。

改法：按 `connections.json` 里已配置的连接（当前优先）探测，命中即在线；
服务可达时跳过 homed 自动拉起；无本地 homed 可执行文件时不再空等超时。

实测验证：修复后启动日志为 `server reachable, skip homed autostart`，该 ERR 消失。

> 这条很可能就是此前「聊天链路有问题」的根因 —— GUI 自以为服务端由自己托管，
> 实际聊天依赖的是 `connections.json` 里的连接。

### 2.4 修 keybd_event 类型名致组合键全废

koffi 不认 Win32 头文件里的 `byte`：

```
"void keybd_event(byte bVk, ...)"    -> Error: Unknown or invalid type name 'byte'
"void keybd_event(uint8 bVk, ...)"   -> 正常
```

原代码用的正是 `byte`，且整段包在**空 catch** 里，于是 `keybd_event` 恒为 null，
所有组合键返回 `keybd_event unavailable`。

这个 bug 极难发现：单键 click/move/scroll 走 `mouse_event`，完全不受影响。
现在签名解析失败会打日志，不再静默。

### 2.5 模式 A：agent 独立光标 + 后台注入（**代码已通，效果待验证**）

见第三节。

---

## 三、模式 A：agent 独立光标（进行中）

### 3.1 目标

现有 `computeruse` 走 `SetCursorPos` + `mouse_event`，**会抢走用户的真实鼠标**，
agent 一干活用户就没法用电脑。

模式 A 的目标：GUI 自绘一个 agent 专属光标（mascot 形象），实际输入通过
`PostMessage` 直接投递到目标窗口句柄，**用户真实鼠标全程不动**。

### 3.2 新增文件

| 文件 | 职责 |
|---|---|
| `cmd/gui/agent-cursor.js` | 透明置顶层窗口 + mascot 指针 + 移动/点击脉冲动画；记录上次命中的窗口句柄 |
| `cmd/gui/agent-inject.js` | 两种注入路径：`SendInput`（默认）与 `PostMessage`（后台）；含用户活动感知 |

### 3.3 配置

`gui-prefs.json` → `deviceBridge.agentCursor`：

| 键 | 值 | 行为 |
|---|---|---|
| `mode` | `"sendinput"` | **默认**。`SendInput` 注入系统输入队列，可靠性高（Chromium/游戏都能操作）。代价：会移动真实光标，故先等用户停手 |
| `mode` | `"overlay"` | `PostMessage` 直投窗口句柄。用户鼠标纹丝不动，但自绘界面常忽略合成消息 |
| `mode` | `"real"` | 保留旧代码路径（兼容） |
| `deferMs` | 数字 | 用户活动时最多等多久（默认 3000，上限 10000）。0 = 不等待 |

> 为什么默认改成 `sendinput`：用户要求「可靠 + 用户正在操作则暂缓」。
> 参考 `Pal-AI-Lab/Coopanion`（见 `DESKTOP-PET-NOTES.md` 第七节）。
>
> ⚠ 实测发现：`GetLastInputInfo` 是**全局**的，agent 自己的 `SendInput`
> 也会被算作「用户输入」，若不排除则**永远处于「用户正在操作」状态**。
> 已按 Coopanion 的三重判定修正（ownTick / ownCursor / 无符号差值）。

### 3.4 已验证（端到端）

用假网关（自实现 WS 服务端）下发 computeruse 命令，GUI 执行并回执。
两轮合计：

**overlay（PostMessage）路径**：8/8 有响应，错误处理正确 ——
无窗口时报「WindowFromPoint 返回空」，而不是静默回退到真实鼠标。

**sendinput 路径**（当前默认）：8/8 有响应，回执形如
`sendinput click @ (300,300)` / `sendinput typed 2 chars` /
`sendinput drag (100,100) -> (400,300)`。

### 3.5 ⚠️ 未完成：真实点击效果未验证

当前执行环境**没有可供注入的可见目标窗口**
（`WindowFromPoint` 对所有坐标返回 null，`GetCursorPos` 读回 0,0）。

**因此：「点击真的落到目标窗口上」这一步尚未被证明。** 需要在有交互桌面的
会话里跑一个端到端测试：

> 打开记事本 → agent 点它 → 输入文字 → 截图回读验证

同时验证「`overlay` 模式下用户鼠标位置在全过程未变」。

### 3.6 设计取舍（明确的边界）

- **两种注入的适用范围不同**：
  - `sendinput`（默认）：注入系统输入队列，等价真实硬件事件。
    Chromium/Electron 自绘界面、多数游戏都能接收 —— **可靠**。
    代价：会移动用户真实光标，故先等用户停手（`deferMs`）。
  - `overlay`：`PostMessage` 直投窗口句柄，用户鼠标纹丝不动。
    但自绘界面常忽略合成消息 ⇒ 只能操作原生 Win32 程序。

  > 本仓曾把「自绘界面点不动」写成「Windows 消息模型的硬限制」——
  > **那个说法是错的**，它只是 `PostMessage` 这条 API 的限制。
  > 用 `SendInput` 就不受此限（参考 Coopanion）。

- **不做静默降级**。无论哪种模式，失败即 `status=error` 并说明原因，
  不会自作主张换成另一种注入方式。
- **`sendinput` 模式下用户正在操作会暂缓**（而不是硬抢）：
  轮询等到用户空闲，超时则跳过本次操作并报错。
  已在 `agent-inject.js` 中排除「自身注入被误判为用户操作」的陷阱。
- **`overlay` 模式下键盘类动作依赖先前的鼠标操作**。
  `keypress`/`hotkey`/`type` 不带坐标，而该模式不移动真实光标，
  所以 `GetCursorPos` 拿到的是**用户自己**的光标位置；
  故记录「上次鼠标操作命中的 hwnd」，键盘动作投递到那里。从未点过则报错。

### 3.7 跨平台状态

| 平台 | 状态 |
|---|---|
| Windows | 已实现（sendinput 默认 / overlay 可选；真实效果待验证） |
| Linux | waiter 侧仍是 `xdotool`，**会动真鼠标**。未做独立光标 |
| macOS | 同上 |

---

## 四、待办

### 4.1 优先：修 `deferMs` 读取不生效

实测：`gui-prefs.json` 里 `agentCursor.deferMs = 0`，但命令仍按默认 3000ms
暂缓（回执写「已等待 3000ms」）。`loadGuiPrefs()` 会重建对象且**不透传
`agentCursor` 字段**，导致读取永远拿到默认值。

- [ ] `loadGuiPrefs()` 透传 `deviceBridge.agentCursor`
- [ ] 判据：配置 `deferMs:0` 时不得进入暂缓路径

### 4.2 验证 sendinput 的真实效果

当前环境无可注入的可见目标窗口（`WindowFromPoint` 恒返回 null，
`GetCursorPos` 读回 0,0），故「点击真的落到目标窗口上」尚未证明。

- [ ] 端到端测试：打开真实应用 → 点它 → 输入 → 截图回读
- [ ] 断言：`overlay` 模式下全过程用户鼠标位置未变
- [ ] 判据：固化进 `npm test`（需能在无桌面 CI 环境跳过）

### 4.3 服务端配合（详见 `SERVER-HANDOFF.md`）

- [ ] 放开 `device.go` 的 action 白名单（schema enum + switch default 双重限制）
- [ ] 透传新参数 `tox`/`toy`/`steps`/`dx`/`ms`/`physical`
- [ ] 统一 scroll 方向语义（服务端写「正=向下」，Windows 惯例是正=向上）
- [ ] 把坐标约定写进注释（防重蹈 `/scale` 覆辙）

> ⚠ 不改白名单，则新增的 11 个动作**根本下发不到设备**。

### 4.4 设置页

- [ ] `agentCursor.mode` / `deferMs` 的 UI（目前只能手改 `gui-prefs.json`）

### 4.5 可选增强（详见 `DESKTOP-PET-NOTES.md` 第六、九节）

- [ ] `main.js` 显式调 `SetProcessDpiAwarenessContext(-4)`，不依赖 Electron 默认值
- [ ] mascot 姿态：思考 / 等待 / 出错
- [ ] 动作解析容错：未知动作丢弃而非整条失败
- [ ] mascot 待机自主行为：闲置时轻微眨眼/呼吸
- [ ] Linux 侧若也要「不抢鼠标」，需 Wayland + uinput 虚拟设备
      （X11 下做不到 —— XTEST 合成的是真实设备事件）

### 4.6 其它已定位但未修

- [ ] `overlay` 模式下键盘类动作「必须先点过」的限制可放宽：
      可改为「取当前前台窗口」作为兵底，而不是直接报错。
- [ ] `real` 模式与 `sendinput` 模式代码重叠较多，可考虑合并；
      目前保留 `real` 仅为兼容旧路径，实际上与 `sendinput` 行为重复。
- [ ] `p3.js` / `p4.js` 是本轮遗留的临时补丁脚本（未被跟踪），
      确认无价值后删除，避免占坑。

---

## 五、坐标空间：实测结论与踩坑记录 ⚠️

**结论：截图给模型的像素、Win32 坐标 API 的坐标，是同一个坐标系 —— 物理像素。
不要换算。**

实测证据（Electron 主进程内，192 DPI / 200% 缩放屏）：

```
Electron screen API (DIP) : 1260 x 840    scaleFactor=2
GetSystemMetrics          : 2520 x 1680
SetCursorPos(200,200)  →  GetCursorPos 读回 (200,200)      ← 恒等，不缩放
```

原因是 Electron 主进程是 **per-monitor DPI-aware**，此感知下 Win32 坐标不做虚拟化。

### 踩坑：曾经加错的 `/scaleFactor` 换算

本仓曾在 `main.js` 加过：

```js
absX = ox + rawX / scaleFactor   // ← 错的
```

理由是「SetCursorPos 期望 DIP」—— **那个前提不成立**，DIP 行为只出现在
DPI-*unaware* 进程里。加了之后反而把原本正确的点击改坏：200% 缩放屏上
点 (600,400) 会被送到 (400,267)，**越靠右下偏得越远**。

已改回 `physical ? rawX : rawX * scale`，并在 `computeruse.test.mjs` 加判据
防止回退（检测 `/scale` 是否回来了）。

### 为什么会误判（重要）

**在普通 node 进程里测同一个函数，读到的是 1260×840**（虚拟化后的值），
会让人以为 Win32 用 DIP。**必须在 Electron 内测** —— 同一份代码，
不同进程的 DPI 感知状态不同，答案也不同。

同理，`python`/`go` 等非 DPI-aware 宿主里测同样不可作为依据。

---

## 六、端到端测试方法（可复用）

不依赖真实服务端，用自实现的假网关驱动 GUI：

1. 起一个最小 WS 服务端（自实现 HTTP `Upgrade` 握手 + 帧编解码）
2. 改写 `gui-prefs.json` 指向它，并置 `deviceBridge.authorized = true`
3. 启动 GUI，等 `hello` / `bind` 完成
4. 下发 `{op:"cmd", cmd_type:"homeagent", command:"computeruse {...}"}`
5. 读取 GUI 回的 `{op:"cmd_result", status, output, error}`

### 两个踩过的坑

- **服务端→客户端的帧不能加掩码。** RFC 6455 只要求客户端→服务端掩码。
  照抄 GUI 主进程（客户端方向）的 `sendDeviceFrame` 会导致 GUI 解不出。
- **客户端→服务端的帧是掩码的**，解帧时必须先解掩码。

---

## 七、koffi 3.x 使用约定（踩坑记录）

| 事项 | 正确做法 | 错误做法 / 现象 |
|---|---|---|
| 类型名 | `uint8` / `int32_t` / `long` | `byte` → `Unknown or invalid type name` |
| 指针参数 | 传 **Node `Buffer`** | 传普通 JS 对象 → **不写回**，读回仍是原值 |
| 结构体 | `koffi.struct(name, {...})` 后传普通对象 | 重复 `koffi.struct("POINT")` → `Duplicate type name` |
| 字符串 | `Buffer.from(s + "\0", "utf16le")` | `koffi.alloc(str)` 把 str 当**类型名** |

**指针参数必须用 Buffer** 这条尤其重要：`WindowFromPoint` 若用普通对象，
会拿不到真实 hwnd ⇒ 点击落到错误窗口上。

跨进程/跨模块重复声明同名结构体会抛 `Duplicate type name`，需 try/catch 换名注册。

---

## 八、环境注意事项

- 本仓库文件多为 **CRLF**。用 Node 脚本打补丁时先 `replace(/\r\n/g, "\n")` 处理，
  写回时按原行尾还原。
- `git status` 会把大量文件标成 `M`，但 `git diff --numstat` 显示 `0 0`
  —— 那是 CRLF 归一化噪声，不是真实改动。**提交前用 `--numstat` 甄别。**
- 本机没有 Go 工具链（`go: command not found`），`cmd/waiter/` 的改动
  **未经过编译验证**，需在有 Go 环境的机器上 `go build ./cmd/waiter` 确认。
- 本会话多数为无头/无交互桌面环境，`WindowFromPoint` / 真实光标相关行为
  **无法在此验证**，需在有桌面的会话实测。
