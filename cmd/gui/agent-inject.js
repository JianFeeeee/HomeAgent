// Windows 后台输入注入：不经过系统光标，直接把消息投递到目标窗口。
//
// 为什么不调 SetCursorPos：
//   那样会抢走用户的真实光标，用户在 agent 操作期间无法正常使用电脑。
//   本模块用 PostMessage 把输入直接送到目标窗口的 hwnd，系统光标纹丝不动。
//
// 已实测确认（本轮 probe-inject/electron-probe.js）：
//   PostMessage(EDIT, WM_CHAR, 'H') 后读回控件文本 = "Ha"
//   且 GetCursorPos 前后一致 ⇒ 用户鼠标确实未被移动。
//
// ★ koffi 3.x 的硬约定（踩过坑，务必遵守）：
//   指针参数必须传 **Buffer**。传普通 JS 对象不会写回 ——
//     GetCursorPos({x,y})   读回仍是原值（无效）
//     GetCursorPos(Buffer)  读到真实坐标（有效）
//   写错会导致拿错目标窗口 hwnd ⇒ 点击落到别的窗口上。

let koffi = null;
let u32 = null;
let cached = null;

function lib() {
  if (cached) return cached;
  koffi = require("koffi");
  u32 = koffi.load("user32.dll");
  // ★ koffi 的类型名是**全局注册**的：同一个进程里重复 koffi.struct("POINT", ...)
  // 会抛 "Duplicate type name 'POINT'"。main.js / 探针脚本可能已声明过同名结构体，
  // 所以这里先试着复用，失败就换个名字注册。
  let POINT;
  try {
    POINT = koffi.struct("POINT", { x: "long", y: "long" });
  } catch (e) {
    POINT = koffi.struct("HA_POINT", { x: "long", y: "long" });
  }

  const f = (sig) => u32.func(sig);
  cached = {
    POINT,
    // 指针参数一律用 void *，调用时传 Buffer
    WindowFromPoint: f("void *WindowFromPoint(void *pt)"),
    ChildWindowFromPointEx: f(
      "void *ChildWindowFromPointEx(void *hwndParent, void *pt, uint flags)",
    ),
    GetAncestor: f("void *GetAncestor(void *hwnd, uint flags)"),
    IsWindow: f("int IsWindow(void *hwnd)"),
    IsWindowVisible: f("int IsWindowVisible(void *hwnd)"),
    GetWindowThreadProcessId: f("uint GetWindowThreadProcessId(void *hwnd, uint *pid)"),
    GetClientRect: f("int GetClientRect(void *hwnd, void *rect)"),
    ScreenToClient: f("int ScreenToClient(void *hwnd, void *pt)"),
    GetCursorPos: f("bool GetCursorPos(void *pt)"),
    PostMessageW: f("intptr_t PostMessageW(void *hwnd, uint msg, uintptr_t w, intptr_t l)"),
    SendMessageW: f("intptr_t SendMessageW(void *hwnd, uint msg, uintptr_t w, intptr_t l)"),
    // 这些用于"这类窗口吃不吃合成消息"的能力探测
    GetWindowLongPtrW: f("intptr_t GetWindowLongPtrW(void *hwnd, int i)"),
  };
  return cached;
}

// Win32 常量
const GA_ROOT = 2;
const CWP_SKIPINVISIBLE = 0x0002;
const CWP_SKIPTRANSPARENT = 0x0004;
const GWL_STYLE = -16;
const GWL_EXSTYLE = -20;
const WS_EX_LAYERED = 0x00080000;
const WS_EX_TRANSPARENT = 0x00000020;
const WS_EX_NOACTIVATE = 0x08000000;

const WM_MOUSEMOVE = 0x0200;
const WM_LBUTTONDOWN = 0x0201;
const WM_LBUTTONUP = 0x0202;
const WM_LBUTTONDBLCLK = 0x0203;
const WM_RBUTTONDOWN = 0x0204;
const WM_RBUTTONUP = 0x0205;
const WM_MBUTTONDOWN = 0x0207;
const WM_MBUTTONUP = 0x0208;
const WM_MOUSEWHEEL = 0x020A;
const WM_MOUSEHWHEEL = 0x020E;
const WM_KEYDOWN = 0x0100;
const WM_KEYUP = 0x0101;
const WM_CHAR = 0x0102;
const WM_SYSKEYDOWN = 0x0104;
const WM_SYSKEYUP = 0x0105;
const MK_LBUTTON = 0x0001;
const MK_RBUTTON = 0x0002;
const MK_MBUTTON = 0x0010;
const MK_CONTROL = 0x0008;
const MK_SHIFT = 0x0004;

const SM_XVIRTUALSCREEN = 76;
const SM_YVIRTUALSCREEN = 77;
const SM_CXVIRTUALSCREEN = 78;
const SM_CYVIRTUALSCREEN = 79;

// 虚拟键码（与 main.js 的 tapVk 共用同一张表）
const VK = {
  ctrl: 0x11, control: 0x11, alt: 0x12, shift: 0x10, win: 0x5b, meta: 0x5b, super: 0x5b,
  enter: 0x0d, return: 0x0d, tab: 0x09, esc: 0x1b, escape: 0x1b,
  space: 0x20, backspace: 0x08, delete: 0x2e, del: 0x2e,
  up: 0x26, down: 0x28, left: 0x25, right: 0x27,
  home: 0x24, end: 0x23, pageup: 0x21, pagedown: 0x22,
};
const VK_EXTENDED = new Set([0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x2e, 0x5b, 0x5c, 0x6f, 0x74]);
const isExtVK = (vk) => VK_EXTENDED.has(vk);

function getSystemMetrics() {
  const m = u32.func("int GetSystemMetrics(int i)");
  return {
    x: m(SM_XVIRTUALSCREEN),
    y: m(SM_YVIRTUALSCREEN),
    w: m(SM_CXVIRTUALSCREEN),
    h: m(SM_CYVIRTUALSCREEN),
  };
}

function hwndStr(h) {
  return h ? "0x" + h.toString(16) : "(null)";
}

// 屏幕物理坐标 → 目标窗口 hwnd（含子窗口命中）
// ★ pt 必须是 Buffer
function windowAtPoint(sx, sy) {
  const k = lib();
  const pt = Buffer.alloc(8);
  pt.writeInt32LE(Math.round(sx), 0);
  pt.writeInt32LE(Math.round(sy), 4);
  let h = k.WindowFromPoint(pt);
  if (!h) return null;
  // WindowFromPoint 只返回顶层窗口；对 UI 框架要下钻到真实接收消息的子窗口
  const flags = CWP_SKIPINVISIBLE | CWP_SKIPTRANSPARENT;
  let guard = 0;
  for (;;) {
    const child = k.ChildWindowFromPointEx(h, pt, flags);
    if (!child || child === h || guard++ > 8) break;
    h = child;
  }
  return h;
}

// 屏幕坐标 → 目标窗口客户区坐标（lParam 用的就是客户区坐标）
function toClient(hwnd, sx, sy) {
  const k = lib();
  const pt = Buffer.alloc(8);
  pt.writeInt32LE(Math.round(sx), 0);
  pt.writeInt32LE(Math.round(sy), 4);
  k.ScreenToClient(hwnd, pt);
  return { x: pt.readInt32LE(0), y: pt.readInt32LE(4) };
}

// 窗口是否"看起来"能接收合成消息（启发式判断，不保证）
function acceptsSynthetic(hwnd) {
  const k = lib();
  if (!hwnd || !k.IsWindow(hwnd)) return { ok: false, why: "invalid hwnd" };
  if (!k.IsWindowVisible(hwnd)) return { ok: false, why: "window hidden" };
  const ex = k.GetWindowLongPtrW(hwnd, GWL_EXSTYLE);
  // WS_EX_TRANSPARENT 的窗口通常不接收命中测试类消息
  if (ex & WS_EX_TRANSPARENT) return { ok: false, why: "WS_EX_TRANSPARENT" };
  return { ok: true, ex };
}

function post(hwnd, msg, w, l) {
  const k = lib();
  const r = k.PostMessageW(hwnd, msg, w, l);
  return r !== 0;
}

function pack(x, y) {
  // lParam: 低 16 位 x，高 16 位 y（signed 16-bit）
  return ((y & 0xffff) << 16) | (x & 0xffff);
}

// ── 鼠标操作 ───────────────────────────────────────────────────────
const BTN_MSG = {
  left: { down: WM_LBUTTONDOWN, up: WM_LBUTTONUP, dbl: WM_LBUTTONDBLCLK, mk: MK_LBUTTON },
  right: { down: WM_RBUTTONDOWN, up: WM_RBUTTONUP, mk: MK_RBUTTON },
  middle: { down: WM_MBUTTONDOWN, up: WM_MBUTTONUP, mk: MK_MBUTTON },
};

function mouseOp(sx, sy, action, opts) {
  const hwnd = windowAtPoint(sx, sy);
  if (!hwnd) return { ok: false, error: "WindowFromPoint 返回空（该坐标无窗口）" };
  const c = toClient(hwnd, sx, sy);
  const lp = pack(c.x, c.y);
  const btn = BTN_MSG[(opts && opts.button) || "left"] || BTN_MSG.left;
  const post_ = (m, w) => post(hwnd, m, w, lp);

  switch (action) {
    case "move":
      return { ok: post_(WM_MOUSEMOVE, 0), hwnd, client: c };
    case "click": {
      post_(btn.down, btn.mk);
      const up = post_(btn.up, 0);
      return { ok: up, hwnd, client: c };
    }
    case "doubleclick": {
      post_(btn.down, btn.mk);
      post_(btn.up, 0);
      post_(btn.dbl, MK_LBUTTON);
      const up = post_(btn.up, 0);
      return { ok: up, hwnd, client: c };
    }
    case "mousedown":
      return { ok: post_(btn.down, btn.mk), hwnd, client: c };
    case "mouseup":
      return { ok: post_(btn.up, 0), hwnd, client: c };
    default:
      return { ok: false, error: "unsupported mouse action " + action };
  }
}

function tripleClick(sx, sy) {
  const hwnd = windowAtPoint(sx, sy);
  if (!hwnd) return { ok: false, error: "WindowFromPoint 返回空" };
  const c = toClient(hwnd, sx, sy);
  const lp = pack(c.x, c.y);
  post(hwnd, WM_LBUTTONDOWN, MK_LBUTTON, lp);
  post(hwnd, WM_LBUTTONUP, 0, lp);
  post(hwnd, WM_LBUTTONDBLCLK, MK_LBUTTON, lp);
  post(hwnd, WM_LBUTTONUP, 0, lp);
  return { ok: true, hwnd, client: c };
}

function drag(sx, sy, tx, ty, opts) {
  const hwnd = windowAtPoint(sx, sy);
  if (!hwnd) return { ok: false, error: "WindowFromPoint 返回空" };
  const btn = BTN_MSG[(opts && opts.button) || "left"] || BTN_MSG.left;
  const steps = Math.max(1, Math.min(60, (opts && opts.steps) || 12));
  const c0 = toClient(hwnd, sx, sy);
  post(hwnd, btn.down, btn.mk, pack(c0.x, c0.y));
  // 分步移动：部分应用（canvas 拖拽、排序、滑块）依赖中间 move 事件
  for (let i = 1; i <= steps; i++) {
    const t = i / steps;
    const c = toClient(hwnd, sx + (tx - sx) * t, sy + (ty - sy) * t);
    post(hwnd, WM_MOUSEMOVE, btn.mk, pack(c.x, c.y));
  }
  const cEnd = toClient(hwnd, tx, ty);
  const up = post(hwnd, btn.up, 0, pack(cEnd.x, cEnd.y));
  return { ok: up, hwnd, steps };
}

// 滚轮：lParam 用**屏幕**坐标，不是客户区坐标
function scroll(sx, sy, dy, dx) {
  const hwnd = windowAtPoint(sx, sy);
  if (!hwnd) return { ok: false, error: "WindowFromPoint 返回空" };
  const lp = pack(Math.round(sx), Math.round(sy));
  let ok = true;
  if (dy) ok = post(hwnd, WM_MOUSEWHEEL, deltaWord(Math.round(dy)), lp) && ok;
  if (dx) ok = post(hwnd, WM_MOUSEHWHEEL, deltaWord(Math.round(dx)), lp) && ok;
  return { ok, hwnd };
}

// 把像素量换算成滚轮"格"（WHEEL_DELTA=120）
function deltaWord(v) {
  const clicks = Math.round(v / 120) || (v > 0 ? 1 : -1);
  // high word 存 delta，low word 存按键状态
  return (clicks << 16) >>> 0;
}

// ── 键盘操作 ───────────────────────────────────────────────────────
function keyTap(hwnd, spec) {
  const parts = String(spec || "")
    .split("+")
    .map((x) => x.trim().toLowerCase())
    .filter(Boolean);
  if (!parts.length) return { ok: false, error: "empty key" };
  const last = parts[parts.length - 1];
  const mods = parts.slice(0, -1);
  const modCodes = [];
  for (const m of mods) {
    const c = VK[m];
    if (c === undefined) return { ok: false, error: "unknown modifier: " + m };
    modCodes.push(c);
  }
  let keyCode = VK[last];
  if (keyCode === undefined) {
    if (last.length === 1) keyCode = last.toUpperCase().charCodeAt(0);
    else return { ok: false, error: "unknown key: " + last };
  }
  let ok = true;
  for (const c of modCodes) {
    ok = post(hwnd, WM_KEYDOWN, c, 0) && ok;
  }
  ok = post(hwnd, WM_KEYDOWN, keyCode, isExtVK(keyCode) ? 1 : 0) && ok;
  ok = post(hwnd, WM_KEYUP, keyCode, isExtVK(keyCode) ? 1 : 0) && ok;
  for (const c of modCodes.slice().reverse()) {
    ok = post(hwnd, WM_KEYUP, c, 0) && ok;
  }
  return { ok, mods: modCodes, key: keyCode };
}

// 文本注入：走 WM_CHAR，不做输入法合成，因此对原生控件最可靠。
// 换行/回车单独发 VK_RETURN，否则 WM_CHAR 的 \n 多数控件不认。
function typeText(hwnd, text) {
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
  windowAtPoint,
  toClient,
  acceptsSynthetic,
  getSystemMetrics,
  hwndStr,
  // 鼠标
  mouse: mouseOp,
  tripleClick,
  drag,
  scroll,
  // 键盘
  keyTap,
  typeText,
  VK,
};
