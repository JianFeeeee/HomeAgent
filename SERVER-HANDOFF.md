# 服务端交接：computeruse 协议对齐

> 面向服务端接手人。本文档描述 GUI 侧已完成的改动，以及**服务端必须同步修改**的部分。
> 修改范围集中在 `internal/plugins/remotedevice/`（`device.go`）。
> GUI 侧代码与判据见 `cmd/gui/`。

---

## 一、为什么需要这份文档

GUI 侧把 `computeruse` 从 9 个基础动作扩到 **18 个**，并新增了**模式 A（agent 独立光标 + 后台注入）**。
但服务端 `device.go` 目前对 `action` 做了**两重硬限制**，导致新增能力根本下发不到设备：

1. **JSON Schema 的 `enum`**（`device.go:129`）—— 7 个值之外直接被 schema 校验拒绝
2. **`switch` 的 `default` 分支**（`device.go:471`）—— 返回「不支持的操作类型」

所以：**即使 GUI 已实现，服务端不改则新功能等于不存在。**

---

## 二、必做项 1：放开 action 白名单

### 现状

```go
// device.go:129 —— schema enum，只有 7 个
"enum": []interface{}{"click", "doubleclick", "rightclick", "move", "scroll", "keypress", "type"},
```

```go
// device.go:471 —— default 直接报错
default:
    return nil, fmt.Errorf("不支持的操作类型 %s（可选 click/doubleclick/rightclick/move/scroll/keypress/type）", action)
```

### 需要改成

GUI 侧实际支持的 18 个 action（判据同源，`cmd/gui/computeruse.test.mjs` 会校验两端一致）：

```
click       doubleclick  rightclick   middleclick  tripleclick
move        hover        drag         mousedown    mouseup
scroll      keypress     hotkey       combo
type        wait         sleep        display
```

建议在 `device.go` 里提取成单一常量，schema 与 switch 共用，避免两处漂移：

```go
// computeruseActions 是设备端支持的完整动作集合。
// ★ 必须与 cmd/gui/main.js 的 computeruse switch 保持一致；
//   cmd/gui/computeruse.test.mjs 会校验 GUI 侧包含这些动作。
var computeruseActions = []string{
    "click", "doubleclick", "rightclick", "middleclick", "tripleclick",
    "move", "hover", "drag", "mousedown", "mouseup",
    "scroll", "keypress", "hotkey", "combo", "type",
    "wait", "sleep", "display",
}
```

然后：
- schema 的 `enum` 改为由 `computeruseActions` 生成
- `switch` 的 `default` 分支的错误信息里列出这份清单

### 各动作的语义（供写 tool description 时参考）

| action | 必需参数 | 说明 |
|---|---|---|
| `click` | `x`,`y` | 可选 `button`（left/right/middle） |
| `doubleclick` | `x`,`y` | |
| `rightclick` | `x`,`y` | 等价 `click` + `button:right` |
| `middleclick` | `x`,`y` | 中键 |
| `tripleclick` | `x`,`y` | 三击选中整行 |
| `move` / `hover` | `x`,`y` | 只移动不点击，用于触发 tooltip |
| `mousedown` / `mouseup` | `x`,`y` | 按下/释放分离，配对可做「按住」 |
| `drag` | `x`,`y`,`tox`,`toy` | 可选 `steps`（默认 12，上限 60） |
| `scroll` | `dy` | 可选 `dx`（横向） |
| `keypress` | `key` | 如 `ctrl+c`、`alt+f4` |
| `hotkey` / `combo` | `key` | 与 keypress 同义 |
| `type` | `text` | |
| `wait` / `sleep` | `ms` | 显式等待，等 UI/动画完成 |
| `display` | — | 返回各显示器 bounds + scaleFactor |

> ⚠ **`drag` 的分步移动不是可有可无的**：canvas 拖拽、列表排序、滑动条
> 这类应用依赖中间 `mousemove` 事件，一步到位会失效。所以 `steps` 建议透传。

---

## 三、必做项 2：透传新参数

### 现状

`device.go` 的 `switch action`（第 439 行起）只转发这些：

```go
params["x"], params["y"], params["button"]
params["dy"]
params["key"]
params["text"]
```

其余参数**被静默丢弃**，即使 LLM 传了也到不了设备。

### 需要补上

| 参数 | 哪些动作需要 | 缺失后果 |
|---|---|---|
| `tox` / `toy` | `drag` | 拖拽终点丢失，拖不动 |
| `steps` | `drag` | 退化成一步到位，拖拽排序类操作失效 |
| `dx` | `scroll` | 横向滚动不可用 |
| `ms` / `duration` | `wait` / `sleep` | 等待时长不可控 |
| `button` | `hover`/`mousedown`/`mouseup`/`tripleclick` | 中键/右键按不下 |
| `physical` | 全部坐标类 | 无法声明坐标系（见第五节） |

建议实现（保持与现有风格一致）：

```go
// 透传辅助：从 args 取整数键，缺省则不写入 params。
func putInt(params map[string]interface{}, args map[string]interface{}, key string) {
    if v, ok := args[key].(float64); ok {
        params[key] = int(v)
    }
}
```

在 `switch` 之后统一补一遍（避免每个 case 重复）：

```go
for _, k := range []string{"tox", "toy", "steps", "dx", "ms", "duration", "physical"} {
    putInt(params, args, k)
}
```

> ⚠ `physical` 的类型是布尔或字符串都可能，取决于 LLM 怎么填。
> 建议 schema 里显式声明 `"type": "boolean"`，并在 `putInt` 之外单独处理。

---

## 四、必做项 3：统一 scroll 方向语义 ⚠️

### 现状：两侧约定相反

**服务端**（`device.go:130`）：

```go
"dy": map[string]interface{}{"type": "integer", "description": "scroll 滚动量：正=向下，负=向上"},
```

**GUI / Windows 惯例**：正 = 向上。

这不是笔误级的小问题——**方向反了会让 agent 在页面上滚到完全相反的位置**，
且因为「能滚」，不会报错，只会静默地做错事，比崩溃更难发现。

### 建议

统一为 **Windows 惯例：正 = 向上，负 = 向下**（与鼠标滚轮一致，也与 GUI 实现一致）。
改 schema description：

```go
"dy": map[string]interface{}{
    "type": "integer",
    "description": "scroll 垂直滚动量：正=向上滚，负=向下滚（与鼠标滚轮方向一致）。可选 dx 做横向滚动，正=向右",
},
```

GUI 侧对应实现（`cmd/gui/main.js`）已按此约定，无需改动。

> 如果服务端坚持保留「正=向下」，则必须**同步改 GUI**，否则两侧永远对不上。
> 二选一，但必须写进注释并进判据。

---

## 五、建议项：把坐标约定写进注释（防重蹈）

这条不是功能需求，而是**防止后来人再犯同一个错**——我本人就在这一轮犯过。

### 事实

**截图给模型的像素、Win32 坐标 API 的坐标，是同一个坐标系：物理像素。不要换算。**

实测证据（Electron 主进程内，192 DPI / 200% 缩放屏）：

```
Electron screen API (DIP) : 1260 x 840    scaleFactor=2
GetSystemMetrics          : 2520 x 1680
SetCursorPos(200,200)  →  GetCursorPos 读回 (200,200)      ← 恒等，不缩放
```

原因是 Electron 主进程是 **per-monitor DPI-aware**，此感知下 Win32 坐标不做虚拟化。

### 曾经的坑

本仓曾在 `cmd/gui/main.js` 加过：

```go
absX = ox + rawX / scaleFactor   // ← 错的
```

理由是「SetCursorPos 期望 DIP」——**那个前提不成立**，DIP 行为只出现在
DPI-*unaware* 进程里。加了之后反而把原本正确的点击改坏：200% 缩放屏上
点 (600,400) 会被送到 (400,267)，**越靠右下偏得越远**。

（易误判之处：在普通 node 进程里测同一个函数，读到的是 1260×840，
那是虚拟化后的值，会把人带偏。必须在 Electron 内测。）

### 建议

在两处加注释：

1. `device.go` 的 `computeruse` 函数上方
2. tool schema 里 `x`/`y` 的 description

```go
// 坐标约定：x/y 为**物理像素**，与 screensee 截图坐标系一致，
// 也与设备端 Win32 坐标一致，**不需要按 scaleFactor 换算**。
// （设备端 GUI 是 per-monitor DPI-aware，Win32 坐标不做虚拟化。）
// 仅当调用方显式传 physical=false 时，设备端才按 DIP 再乘回缩放比。
```

`cmd/gui/computeruse.test.mjs` 已加判据防止 GUI 侧回退（会检测 `/scale` 是否回来了）。
**服务端侧目前无对等判据**，建议补一个，锁定 schema 里的 description 不含
「需换算」之类的误导表述。

---

## 六、模式 A（agent 独立光标 + 后台注入）需要服务端知道的事

### 行为变化

GUI 默认启用 `agentCursor.mode = "overlay"`：

- agent 操作时**不再移动用户的真实鼠标**，用户可同时正常使用电脑
- GUI 会在目标位置绘制一个 mascot 形象作为 agent 的虚拟光标，并有移动/点击脉冲动画
- 实际输入通过 `PostMessage` 直接投递到目标窗口句柄

配置项（`gui-prefs.json` → `deviceBridge.agentCursor.mode`）：

| 值 | 行为 |
|---|---|
| `"overlay"` | **默认**。不动用户鼠标，后台注入 |
| `"real"` | 旧行为。操作真实鼠标（`SetCursorPos` + `mouse_event`） |

### 服务端无需改动，但要知道三条限制

1. **只对「接受窗口消息的程序」可靠。**
   原生 Win32 程序（记事本、Excel、Office 等）可靠；Chromium/Electron 自绘界面、
   多数游戏、DirectX 程序会忽略合成消息。这是 Windows 消息模型的硬限制，
   不是实现缺陷。

2. **注入失败会明确报错，不静默回退。**
   用户既然选择「不抢鼠标」，就不该偷偷改成操作真实鼠标。所以失败时
   `status=error`，`error` 里说明原因（例如「该坐标下没有窗口」）。

3. **键盘类动作需要先有点击类动作。**
   `keypress`/`hotkey`/`type` 不带坐标，且后台注入**不移动真实光标**，
   所以 `GetCursorPos` 拿到的是**用户自己**的光标位置（通常不是 agent 要操作的地方）。
   设备端记录「上一次鼠标操作命中的窗口」，键盘动作投递到那里。
   **若从未点过，键盘动作会报错。** 服务端在 tool description 里说明这一依赖关系，
   可以减少 LLM 调错。

---

## 七、验收方式

GUI 侧判据（服务端改完后跑）：

```bash
cd cmd/gui && npm test
```

含 `computeruse.test.mjs`，会校验两端 action 覆盖面的一致性。

服务端侧建议补的判据（放在 `internal/plugins/remotedevice/`）：

1. schema `enum` 覆盖全部 18 个 action
2. 每个 action 在 `switch` 里都有对应 case，不落 `default`
3. `tox`/`toy`/`steps`/`dx`/`ms` 能透传到下发 JSON（可用现有
   `binary_test.go:471` 的 computeruse 下发测试作模板，参考它如何断言收到的 JSON）

---

## 八、附：本次 GUI 侧一并修掉的两个 bug（服务端可参考）

1. **GUI 误判「服务端未启动」**：原实现只探 `localhost:8080`，
   服务端在 WSL/远端时探不通 → 每次启动误判服务没起 → 去拉起 GUI 旁不存在的
   `homed.exe` → 干等 8s 超时。已改为按 `connections.json` 里已配置的连接探测。
   （服务端侧无改动，但这条能解释历史上 GUI 日志里刷屏的
   `homed failed to start within timeout`。）

2. **koffi 不认 Win32 的 `byte` 类型名**：
   `keybd_event(byte bVk, ...)` 会抛
   `Unknown or invalid type name 'byte'`，必须写 `uint8`。
   曾导致组合键整体返回 `keybd_event unavailable`，而单键 click 不受影响、极易漏掉。
   与服务端无直接关系，但若服务端将来也走 FFI 调 Win32，注意这一点。
