// Windows 输入注入：两种模式。
//
// ┌─ SendInput（默认，可靠）────────────────────────────────────────┐
// │ 注入到**系统输入队列**，等价真实硬件事件。Chromium/Electron/   │
// │ 游戏等自绘界面都能接收。代价：会移动用户的真实光标。           │
// │ 参考实现：Pal-AI-Lab/Coopanion 的 cortico-world-cua/win32.ts   │
// └────────────────────────────────────────────────────────────────┘
// ┌─ PostMessage（后台，不干扰用户）───────────────────────────────┐
// │ 直接投递给**某个窗口句柄**，用户鼠标纹丝不动。代价：自绘界面    │
// │ （Chromium/Electron/游戏）常忽略合成消息，点不动。             │
// └────────────────────────────────────────────────────────────────┘
//
// ★ 已实测确认（本轮）：
//   · SendInput 结构体尺寸 MOUSEINPUT=32 / KEYBDINPUT=24 / INPUT=40（x64），
//     layout 正确，SendInput 返回 1（成功送出）。
//   · GetLastInputInfo 可用，配合 GetTickCount 可算「用户空闲多久」。
//   · koffi 指针参数必须传 **Buffer**；传普通 JS 对象不写回
//     （GetCursorPos({x,y}) 读回仍是原值）。
//
// ★ SendInput 绝对坐标是 **0–65535 归一化值**，不是像素 —— 最容易踩的坑。
//   本次实现按**虚拟桌面**归一化并带 MOUSEEVENTF_VIRTUALDESK，
//   因此多显示器下坐标也正确（Coopanion 只按主屏归一化）。

let koffi = null;
let u32 = null;
let k32 = null;
let cached = null;

// koffi 类型名是**全局注册**的：同进程重复注册同名 struct 会抛
// "Duplicate type name"。统一用 reg() 尝试原名、失败换 HA_ 前缀。
function reg(name, def) {
  try {
    return koffi.struct(name, def);
  } catch (e) {
    return koffi.struct("HA_" + name, def);
  }
}

function lib() {
  if (cached) return cached;
  koffi = require("koffi");
  u32 = koffi.load("user32.dll");
  k32 = koffi.load("kernel32.dll");

  const MOUSEINPUT = reg("MOUSEINPUT", {
    dx: "long", dy: "long", mouseData: "uint32_t",
    dwFlags: "uint32_t", time: "uint32_t", dwExtraInfo: "uintptr_t",
  });
  const KEYBDINPUT = reg("KEYBDINPUT", {
    wVk: "uint16_t", wScan: "uint16_t", dwFlags: "uint32_t",
    time: "uint32_t", dwExtraInfo: "uintptr_t",
  });
  const HARDWAREINPUT = reg("HARDWAREINPUT", {
    uMsg: "uint32_t", wParamL: "uint16_t", wParamH: "uint16_t",
  });
  const INPUT = koffi.struct(
    MOUSEINPUT.name.startsWith("HA_") ? "HA_INPUT" : "INPUT",
    { type: "uint32_t", u: koffi.union({ mi: MOUSEINPUT, ki: KEYBDINPUT, hi: HARDWAREINPUT }) },
  );
  const LASTINPUTINFO = reg("LASTINPUTINFO", { cbSize: "uint32_t", dwTime: "uint32_t" });

  const f = (sig) => u32.func(sig);
  cached = {
    POINT: reg("POINT", { x: "long", y: "long" }),
    INPUT,
    LASTINPUTINFO,
    INPUT_SIZE: koffi.sizeof(INPUT),
    SendInput: f("uint32_t __stdcall SendInput(uint32_t count, " + INPUT.name + " *inputs, int size)"),
    GetLastInputInfo: f("bool __stdcall GetLastInputInfo(_Inout_ " + LASTINPUTINFO.name + " *info)"),
    GetTickCount: k32.func("uint32_t __stdcall GetTickCount()"),
    GetSystemMetrics: f("int GetSystemMetrics(int i)"),
    SetProcessDpiAwarenessContext: f("bool __stdcall SetProcessDpiAwarenessContext(intptr_t value)"),
    // 指针参数一律用 void *，调用时传 Buffer
    WindowFromPoint: f("void *WindowFromPoint(void *pt)"),
    ChildWindowFromPointEx: f("void *ChildWindowFromPointEx(void *hwndParent, void *pt, uint flags)"),
    GetAncestor: f("void *GetAncestor(void *hwnd, uint flags)"),
    IsWindow: f("int IsWindow(void *hwnd)"),
    IsWindowVisible: f("int IsWindowVisible(void *hwnd)"),
    GetWindowThreadProcessId: f("uint GetWindowThreadProcessId(void *hwnd, uint *pid)"),
    GetClientRect: f("int GetClientRect(void *hwnd, void *rect)"),
    ScreenToClient: f("int ScreenToClient(void *hwnd, void *pt)"),
    GetCursorPos: f("bool GetCursorPos(void *pt)"),
    PostMessageW: f("intptr_t PostMessageW(void *hwnd, uint msg, uintptr_t w, intptr_t l)"),
    SendMessageW: f("intptr_t SendMessageW(void *hwnd, uint msg, uintptr_t w, intptr_t l)"),
    GetWindowLongPtrW: f("intptr_t GetWindowLongPtrW(void *hwnd, int i)"),
  };
  return cached;
}

// ── Win32 常量 ─────────────────────────────────────────────────────
const GA_ROOT = 2;
const CWP_SKIPINVISIBLE = 0x0002;
const CWP_SKIPTRANSPARENT = 0x0004;
const GWL_STYLE = -16;
const GWL_EXSTYLE = -20;
const WS_EX_TRANSPARENT = 0x00000020;

// SendInput
const INPUT_MOUSE = 0, INPUT_KEYBOARD = 1;
const ME = {
  MOVE: 0x1, LEFTDOWN: 0x2, LEFTUP: 0x4, RIGHTDOWN: 0x8, RIGHTUP: 0x10,
  MIDDLEDOWN: 0x20, MIDDLEUP: 0x40, WHEEL: 0x800, HWHEEL: 0x1000,
  VIRTUALDESK: 0x4000, ABSOLUTE: 0x8000,
};
const KE = { EXTENDEDKEY: 0x1, KEYUP: 0x2, UNICODE: 0x4 };
const WHEEL_DELTA = 120;

// PostMessage
const WM_MOUSEMOVE = 0x0200;
const WM_LBUTTONDOWN = 0x0201, WM_LBUTTONUP = 0x0202, WM_LBUTTONDBLCLK = 0x0203;
const WM_RBUTTONDOWN = 0x0204, WM_RBUTTONUP = 0x0205;
const WM_MBUTTONDOWN = 0x0207, WM_MBUTTONUP = 0x0208;
const WM_MOUSEWHEEL = 0x020A, WM_MOUSEHWHEEL = 0x020E;
const WM_KEYDOWN = 0x0100, WM_KEYUP = 0x0101, WM_CHAR = 0x0102;
const MK_LBUTTON = 0x0001, MK_RBUTTON = 0x0002, MK_MBUTTON = 0x0010;

const SM_XVIRTUALSCREEN = 76, SM_YVIRTUALSCREEN = 77;
const SM_CXVIRTUALSCREEN = 78, SM_CYVIRTUALSCREEN = 79;

const VK = {
  ctrl: 0x11, control: 0x11, alt: 0x12, shift: 0x10, win: 0x5b, meta: 0x5b, super: 0x5b,
  enter: 0x0d, return: 0x0d, tab: 0x09, esc: 0x1b, escape: 0x1b,
  space: 0x20, backspace: 0x08, delete: 0x2e, del: 0x2e,
  up: 0x26, down: 0x28, left: 0x25, right: 0x27,
  home: 0x24, end: 0x23, pageup: 0x21, pagedown: 0x22,
};
const VK_EXTENDED = new Set([0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x2e, 0x5b, 0x5c, 0x6f, 0x74]);
const isExtVK = (vk) => VK_EXTENDED.has(vk);

// ── 公共小工具 ─────────────────────────────────────────────────────
function virtualScreen() {
  const k = lib();
  return {
    x: k.GetSystemMetrics(SM_XVIRTUALSCREEN),
    y: k.GetSystemMetrics(SM_YVIRTUALSCREEN),
    w: k.GetSystemMetrics(SM_CXVIRTUALSCREEN),
    h: k.GetSystemMetrics(SM_CYVIRTUALSCREEN),
  };
}

function hwndStr(h) {
  return h ? "0x" + h.toString(16) : "(null)";
}

/** 屏幕坐标 → 0–65535 归一化（按虚拟桌面）。SendInput 绝对坐标用这个。 */
function toAbsolute(x, y) {
  const v = virtualScreen();
  const nx = Math.round(((x - v.x) * 65535) / Math.max(1, v.w - 1));
  const ny = Math.round(((y - v.y) * 65535) / Math.max(1, v.h - 1));
  return { nx: Math.max(0, Math.min(65535, nx)), ny: Math.max(0, Math.min(65535, ny)) };
}

// ── 用户活动感知（「用户在操作就暂缓」的核心）───────────────────────
//
// ★ 关键陷阱：GetLastInputInfo 是**全局**的 —— agent 自己的 SendInput
//   也会被算作「输入」。若不排除自己的注入，每次注入后 idleMs() 都归零，
//   于是默认永远处于「用户正在操作」状态，所有命令都被暂缓。
//   （实测踩到：deferMs 设为 0 仍被暂缓，因为上一条命令刚注入过。）
//
// 参照 Pal-AI-Lab/Coopanion 的做法，用三重判定区分「自己注入」vs「用户操作」：
//   1. ownTick   —— 自己最后一次注入的时刻，之后 40ms 内的输入忽略
//   2. ownCursor —— 自己放的光标位置；偏离 >2px 说明是用户动的
//   3. 无符号差值 —— GetTickCount 每 2^32 ms 回绕，必须按无符号比较
let ownTick = 0; // 自己最后一次 SendInput 的 tick
let ownCursor = null; // 自己放下的光标位置 {x,y}
let ownMovedAt = 0; // 检测到光标被外力移动的时刻

/** 无符号「a - b」，正确处理 2^32 回绕。 */
function since(a, b) {
  return (a - b) >>> 0;
}

function cursorNow() {
  const k = lib();
  const pt = Buffer.alloc(8);
  if (!k.GetCursorPos(pt)) return null;
  return { x: pt.readInt32LE(0), y: pt.readInt32LE(4) };
}

/** 标记「刚刚由我们自己注入了输入」。 */
function markOwnInput() {
  ownTick = lib().GetTickCount();
  ownCursor = cursorNow();
}

/** 距上次**用户**输入过了多少毫秒（排除自己的注入）。无记录时返回 Infinity。 */
function idleMs() {
  const k = lib();
  const now = k.GetTickCount();

  // 1) 若光标偏离我们自己放的位置 >2px，说明是用户移动的
  const cur = cursorNow();
  if (ownCursor && cur) {
    if (Math.abs(cur.x - ownCursor.x) > 2 || Math.abs(cur.y - ownCursor.y) > 2) {
      ownCursor = cur;
      ownMovedAt = now;
    }
  }

  // 2) 取全局最后输入时刻，并判断它是否发生在「我们自己注入」之后
  const info = { cbSize: koffi.sizeof(k.LASTINPUTINFO), dwTime: 0 };
  if (!k.GetLastInputInfo(info)) return Number.MAX_SAFE_INTEGER;
  const last = info.dwTime;
  // ownTick 之后 40ms 内的输入视为我们自己造成的（SendInput 生效有延迟）
  const afterOwn =
    ownTick === 0 || (since(last, ownTick) > 40 && since(last, ownTick) < 0x80000000);
  let userAt = afterOwn ? last : 0;

  // 3) 光标移动也算用户活动，取两者中更晚的
  if (ownMovedAt && (!userAt || since(ownMovedAt, userAt) < 0x80000000)) userAt = ownMovedAt;

  return userAt ? since(now, userAt) : Number.MAX_SAFE_INTEGER;
}

/** 用户是否在最近 thresholdMs 内操作过（已排除 agent 自身注入）。 */
function userIsActive(thresholdMs = 1200) {
  return idleMs() < thresholdMs;
}

/**
 * 等到用户停手再执行。
 *
 * 为什么需要：SendInput 会移动真实光标。若用户正在打字/拖拽，agent
 * 插进来会抢走光标、打断输入。做法是轮询等待用户空闲，超过 maxWait
 * 则放弃等待（返回 false）—— 宁可跳过本次操作，也不硬抢。
 */
async function waitForUserIdle(maxWaitMs = 3000, thresholdMs = 800) {
  const start = Date.now();
  while (Date.now() - start < maxWaitMs) {
    if (!userIsActive(thresholdMs)) return true;
    await new Promise((r) => setTimeout(r, 120));
  }
  return !userIsActive(thresholdMs);
}

// ── SendInput 实现 ─────────────────────────────────────────────────
const mouseEvent = (dwFlags, dx = 0, dy = 0, mouseData = 0) => ({
  type: INPUT_MOUSE,
  u: { mi: { dx, dy, mouseData, dwFlags, time: 0, dwExtraInfo: 0 } },
});
const keyEvent = (vk, up, extended, unicode) => ({
  type: INPUT_KEYBOARD,
  u: {
    ki: {
      wVk: unicode ? 0 : vk,
      wScan: unicode ? vk : 0,
      dwFlags: (up ? KE.KEYUP : 0) | (extended ? KE.EXTENDEDKEY : 0) | (unicode ? KE.UNICODE : 0),
      time: 0,
      dwExtraInfo: 0,
    },
  },
});

function send(events) {
  const k = lib();
  const sent = k.SendInput(events.length, events, k.INPUT_SIZE);
  if (sent !== events.length) {
    // 常见原因：目标窗口权限更高（UIPI 阻挡）。这必须报出来，
    // 不能静默 —— 否则会表现为「命令成功但什么都没发生」。
    throw new Error(
      "SendInput 只送出 " + sent + "/" + events.length + " 个事件（可能被更高权限的窗口挡住）",
    );
  }
  // 记录「这次是我们自己注入的」，避免被 idleMs() 误判为用户活动
  markOwnInput();
  return true;
}

const BUTTON_FLAGS = {
  left: [ME.LEFTDOWN, ME.LEFTUP],
  right: [ME.RIGHTDOWN, ME.RIGHTUP],
  middle: [ME.MIDDLEDOWN, ME.MIDDLEUP],
};

/** 移动真实光标（SendInput）。坐标：屏幕物理像素。 */
function siMove(x, y) {
  const { nx, ny } = toAbsolute(x, y);
  send([mouseEvent(ME.MOVE | ME.ABSOLUTE | ME.VIRTUALDESK, nx, ny)]);
  return { ok: true, x, y };
}

function siButton(x, y, action, button) {
  const b = BUTTON_FLAGS[button] || BUTTON_FLAGS.left;
  if (action === "move") return siMove(x, y);
  siMove(x, y); // 先到位，再按键：否则会点到上一个位置
  if (action === "click") {
    send([mouseEvent(b[0]), mouseEvent(b[1])]);
  } else if (action === "doubleclick") {
    send([mouseEvent(b[0]), mouseEvent(b[1]), mouseEvent(b[0]), mouseEvent(b[1])]);
  } else if (action === "tripleclick") {
    send([
      mouseEvent(b[0]), mouseEvent(b[1]),
      mouseEvent(b[0]), mouseEvent(b[1]),
      mouseEvent(b[0]), mouseEvent(b[1]),
    ]);
  } else if (action === "mousedown") {
    send([mouseEvent(b[0])]);
  } else if (action === "mouseup") {
    send([mouseEvent(b[1])]);
  } else {
    return { ok: false, error: "unsupported action " + action };
  }
  return { ok: true, x, y, action, button: button || "left" };
}

/**
 * 拖拽：按下 → 分步移动 → 释放。
 * 分步是必需的：canvas 拖拽、列表排序、滑动条依赖中间 mousemove 事件，
 * 一步到位会失效。
 */
function siDrag(sx, sy, tx, ty, button, steps) {
  const b = BUTTON_FLAGS[button] || BUTTON_FLAGS.left;
  const n = Math.max(1, Math.min(60, steps || 12));
  siMove(sx, sy);
  send([mouseEvent(b[0])]);
  for (let i = 1; i <= n; i++) {
    const t = i / n;
    siMove(sx + (tx - sx) * t, sy + (ty - sy) * t);
  }
  siMove(tx, ty);
  send([mouseEvent(b[1])]);
  return { ok: true, from: { x: sx, y: sy }, to: { x: tx, y: ty }, steps: n };
}

/**
 * 滚轮。dy/dx 单位为「格」（1 格 = WHEEL_DELTA=120）。
 * 约定：dy > 0 = 向上滚（与 Windows 惯例一致，也与本仓既有实现一致）。
 */
function siScroll(dy, dx) {
  const events = [];
  if (dy) events.push(mouseEvent(ME.WHEEL, 0, 0, (Math.round(dy) * WHEEL_DELTA) >>> 0));
  if (dx) events.push(mouseEvent(ME.HWHEEL, 0, 0, (Math.round(dx) * WHEEL_DELTA) >>> 0));
  if (events.length) send(events);
  return { ok: true, dy: dy || 0, dx: dx || 0 };
}

/** 组合键。spec 形如 "ctrl+shift+t"。 */
function siKeyTap(spec) {
  const parts = String(spec || "")
    .split("+")
    .map((x) => x.trim().toLowerCase())
    .filter(Boolean);
  if (!parts.length) return { ok: false, error: "empty key" };
  const last = parts[parts.length - 1];
  const mods = parts.slice(0, -1);
  const codes = [];
  for (const m of mods) {
    const c = VK[m];
    if (c === undefined) return { ok: false, error: "unknown modifier: " + m };
    codes.push(c);
  }
  let keyCode = VK[last];
  if (keyCode === undefined) {
    if (last.length === 1) keyCode = last.toUpperCase().charCodeAt(0);
    else return { ok: false, error: "unknown key: " + last };
  }
  // 按下的按正序、释放按反序 —— 与真实手指动作一致
  const events = [
    ...codes.map((c) => keyEvent(c, false, false, false)),
    keyEvent(keyCode, false, isExtVK(keyCode), false),
    keyEvent(keyCode, true, isExtVK(keyCode), false),
    ...[...codes].reverse().map((c) => keyEvent(c, true, false, false)),
  ];
  send(events);
  return { ok: true, key: spec };
}

/**
 * 文本输入：用 KEYEVENTF.UNICODE 逐字符发送。
 * 比 WM_CHAR / SendKeys 可靠：不受键盘布局和输入法状态影响，
 * 中文、emoji 也能进（emoji 需按 UTF-16 代理对逐个发）。
 */
function siType(text) {
  const t = String(text || "");
  if (!t) return { ok: false, error: "empty text" };
  const events = [];
  for (let i = 0; i < t.length; i++) {
    const ch = t[i];
    if (ch === "\n") {
      events.push(keyEvent(VK.enter, false, false, false), keyEvent(VK.enter, true, false, false));
      continue;
    }
    if (ch === "\r") continue;
    if (ch === "\t") {
      events.push(keyEvent(VK.tab, false, false, false), keyEvent(VK.tab, true, false, false));
      continue;
    }
    const unit = t.charCodeAt(i); // UTF-16 code unit（代理对会被拆成两个，正好）
    events.push(keyEvent(unit, false, false, true), keyEvent(unit, true, false, true));
  }
  if (events.length) send(events);
  return { ok: true, chars: t.length };
}

// ── PostMessage 实现（后台，不干扰用户）────────────────────────────
function windowAtPoint(sx, sy) {
  const k = lib();
  const pt = Buffer.alloc(8);
  pt.writeInt32LE(Math.round(sx), 0);
  pt.writeInt32LE(Math.round(sy), 4);
  let h = k.WindowFromPoint(pt);
  if (!h) return null;
  const flags = CWP_SKIPINVISIBLE | CWP_SKIPTRANSPARENT;
  let guard = 0;
  for (;;) {
    const child = k.ChildWindowFromPointEx(h, pt, flags);
    if (!child || child === h || guard++ > 8) break;
    h = child;
  }
  return h;
}

function toClient(hwnd, sx, sy) {
  const k = lib();
  const pt = Buffer.alloc(8);
  pt.writeInt32LE(Math.round(sx), 0);
  pt.writeInt32LE(Math.round(sy), 4);
  k.ScreenToClient(hwnd, pt);
  return { x: pt.readInt32LE(0), y: pt.readInt32LE(4) };
}

function acceptsSynthetic(hwnd) {
  const k = lib();
  if (!hwnd || !k.IsWindow(hwnd)) return { ok: false, why: "invalid hwnd" };
  if (!k.IsWindowVisible(hwnd)) return { ok: false, why: "window hidden" };
  const ex = k.GetWindowLongPtrW(hwnd, GWL_EXSTYLE);
  if (ex & WS_EX_TRANSPARENT) return { ok: false, why: "WS_EX_TRANSPARENT" };
  return { ok: true, ex };
}

function post(hwnd, msg, w, l) {
  return lib().PostMessageW(hwnd, msg, w, l) !== 0;
}
const pack = (x, y) => ((y & 0xffff) << 16) | (x & 0xffff);

const PM_BTN = {
  left: { down: WM_LBUTTONDOWN, up: WM_LBUTTONUP, dbl: WM_LBUTTONDBLCLK, mk: MK_LBUTTON },
  right: { down: WM_RBUTTONDOWN, up: WM_RBUTTONUP, mk: MK_RBUTTON },
  middle: { down: WM_MBUTTONDOWN, up: WM_MBUTTONUP, mk: MK_MBUTTON },
};

function pmMouse(sx, sy, action, opts) {
  const hwnd = windowAtPoint(sx, sy);
  if (!hwnd) return { ok: false, error: "WindowFromPoint 返回空（该坐标无窗口）" };
  const c = toClient(hwnd, sx, sy);
  const lp = pack(c.x, c.y);
  const btn = PM_BTN[(opts && opts.button) || "left"] || PM_BTN.left;
  const P = (m, w) => post(hwnd, m, w, lp);
  switch (action) {
    case "move":
      return { ok: P(WM_MOUSEMOVE, 0), hwnd, client: c };
    case "click":
      P(btn.down, btn.mk);
      return { ok: P(btn.up, 0), hwnd, client: c };
    case "doubleclick":
      P(btn.down, btn.mk); P(btn.up, 0);
      P(btn.dbl, MK_LBUTTON);
      return { ok: P(btn.up, 0), hwnd, client: c };
    case "mousedown":
      return { ok: P(btn.down, btn.mk), hwnd, client: c };
    case "mouseup":
      return { ok: P(btn.up, 0), hwnd, client: c };
    default:
      return { ok: false, error: "unsupported mouse action " + action };
  }
}

function pmTripleClick(sx, sy) {
  const hwnd = windowAtPoint(sx, sy);
  if (!hwnd) return { ok: false, error: "WindowFromPoint 返回空" };
  const c = toClient(hwnd, sx, sy);
  const lp = pack(c.x, c.y);
  post(hwnd, WM_LBUTTONDOWN, MK_LBUTTON, lp);
  post(hwnd, WM_LBUTTONUP, 0, lp);
  post(hwnd, WM_LBUTTONDBLCLK, MK_LBUTTON, lp);
  post(hwnd, WM_LBUTTONUP, 0, lp);
  return { ok: true, hwnd };
}

function pmDrag(sx, sy, tx, ty, opts) {
  const hwnd = windowAtPoint(sx, sy);
  if (!hwnd) return { ok: false, error: "WindowFromPoint 返回空" };
  const btn = PM_BTN[(opts && opts.button) || "left"] || PM_BTN.left;
  const steps = Math.max(1, Math.min(60, (opts && opts.steps) || 12));
  const c0 = toClient(hwnd, sx, sy);
  post(hwnd, btn.down, btn.mk, pack(c0.x, c0.y));
  for (let i = 1; i <= steps; i++) {
    const t = i / steps;
    const c = toClient(hwnd, sx + (tx - sx) * t, sy + (ty - sy) * t);
    post(hwnd, WM_MOUSEMOVE, btn.mk, pack(c.x, c.y));
  }
  const ce = toClient(hwnd, tx, ty);
  const ok = post(hwnd, btn.up, 0, pack(ce.x, ce.y));
  return { ok, hwnd, steps };
}

function pmScroll(sx, sy, dy, dx) {
  const hwnd = windowAtPoint(sx, sy);
  if (!hwnd) return { ok: false, error: "WindowFromPoint 返回空" };
  const lp = pack(Math.round(sx), Math.round(sy)); // 滚轮用屏幕坐标
  let ok = true;
  if (dy) ok = post(hwnd, WM_MOUSEWHEEL, (Math.round(dy) * WHEEL_DELTA) >>> 0, lp) && ok;
  if (dx) ok = post(hwnd, WM_MOUSEHWHEEL, (Math.round(dx) * WHEEL_DELTA) >>> 0, lp) && ok;
  return { ok, hwnd };
}

function pmKeyTap(hwnd, spec) {
  const parts = String(spec || "").split("+").map((x) => x.trim().toLowerCase()).filter(Boolean);
  if (!parts.length) return { ok: false, error: "empty key" };
  const last = parts[parts.length - 1];
  const mods = parts.slice(0, -1);
  const codes = [];
  for (const m of mods) {
    const c = VK[m];
    if (c === undefined) return { ok: false, error: "unknown modifier: " + m };
    codes.push(c);
  }
  let keyCode = VK[last];
  if (keyCode === undefined) {
    if (last.length === 1) keyCode = last.toUpperCase().charCodeAt(0);
    else return { ok: false, error: "unknown key: " + last };
  }
  let ok = true;
  for (const c of codes) ok = post(hwnd, WM_KEYDOWN, c, 0) && ok;
  ok = post(hwnd, WM_KEYDOWN, keyCode, isExtVK(keyCode) ? 1 : 0) && ok;
  ok = post(hwnd, WM_KEYUP, keyCode, isExtVK(keyCode) ? 1 : 0) && ok;
  for (const c of codes.slice().reverse()) ok = post(hwnd, WM_KEYUP, c, 0) && ok;
  return { ok, key: spec };
}

function pmTypeText(hwnd, text) {
  const t = String(text || "");
  if (!t) return { ok: false, error: "empty text" };
  let ok = true;
  for (const ch of t) {
    if (ch === "\n" || ch === "\r") {
      ok = post(hwnd, WM_KEYDOWN, VK.enter, 0) && ok;
      ok = post(hwnd, WM_KEYUP, VK.enter, 0) && ok;
      continue;
    }
    if (ch === "\t") {
      ok = post(hwnd, WM_KEYDOWN, VK.tab, 0) && ok;
      ok = post(hwnd, WM_KEYUP, VK.tab, 0) && ok;
      continue;
    }
    ok = post(hwnd, WM_CHAR, ch.codePointAt(0), 0) && ok;
  }
  return { ok, chars: t.length };
}

module.exports = {
  available() {
    try {
      lib();
      return true;
    } catch (e) {
      return false;
    }
  },
  // 坐标/桌面
  virtualScreen,
  toAbsolute,
  hwndStr,
  // 用户活动感知
  idleMs,
  userIsActive,
  waitForUserIdle,
  markOwnInput,
  cursorNow,
  // SendInput（默认路径）
  si: {
    move: siMove,
    button: siButton,
    drag: siDrag,
    scroll: siScroll,
    key: siKeyTap,
    type: siType,
  },
  // PostMessage（后台路径）
  pm: {
    mouse: pmMouse,
    tripleClick: pmTripleClick,
    drag: pmDrag,
    scroll: pmScroll,
    key: pmKeyTap,
    type: pmTypeText,
  },
  // 兼容旧名（PostMessage）
  windowAtPoint,
  toClient,
  acceptsSynthetic,
  mouse: pmMouse,
  tripleClick: pmTripleClick,
  drag: pmDrag,
  scroll: pmScroll,
  keyTap: pmKeyTap,
  typeText: pmTypeText,
  VK,
};
