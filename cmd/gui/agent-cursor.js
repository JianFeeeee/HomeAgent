// Agent 独立光标（模式 A）：GUI 自绘 mascot 指针 + 后台注入，全程不动用户鼠标。
//
// 目标：agent 操作用户电脑时，不再抢走真实光标 —— 用户可以同时干自己的事。
//
// 三层结构：
//   1) overlay 窗口：全屏透明置顶层，setIgnoreMouseEvents(true) 让它不吃点击
//   2) mascot 指针：在这个窗口里画 mascot.webp + 点击脉冲环，做移动/点击动画
//   3) 后台注入：PostMessage 直接投递到目标窗口 hwnd，不经过系统光标
//
// 已实测确认的两条硬事实（cmd/gui/probe2 与 probe-inject/electron-probe.js）：
//   A) 坐标空间：Win32 坐标 = 物理像素 = 截图像素，**不需要换算 scaleFactor**
//      （Electron 主进程 per-monitor DPI-aware，Win32 不虚拟化）
//   B) koffi 3.x 传指针参数必须用 **Buffer**；用普通 JS 对象不写回
//      （GetCursorPos 传 {x,y} 读回仍是原值，传 Buffer 才拿到真值）
//      —— 这条决定了 WindowFromPoint 必须走 Buffer，写错会拿错目标窗口。
//
// 不做的事（明确的取舍）：
//   · 不做真实鼠标降级。模式 A 只保证对「接受窗口消息的程序」可靠；
//     Chromium/Electron 自绘、游戏、DirectX 会忽略合成消息，这是
//     Windows 消息模型的硬限制，不是实现缺陷。
//   · 不劫持用户鼠标。任何情况下都不调用 SetCursorPos。

const path = require("path");
const fs = require("fs");
const { BrowserWindow, screen } = require("electron");

// ── overlay 与动画状态 ──────────────────────────────────────────────
let cursorWin = null; // 自绘光标窗口
let cursorVisible = false;
let cursorBusy = false; // 正在执行命令时（用于画"忙碌"光环）
let cursorRaf = null;
let lastHwnd = null; // 上一次鼠标操作命中的目标窗口（键盘动作投递到这里）
const cursorPos = { x: 0, y: 0 }; // 物理像素，屏幕坐标
const cursorTarget = { x: 0, y: 0 };
let cursorPulse = 0; // 点击脉冲环 0..1，>0 表示正在播放
let cursorMotion = 0; // 移动拖尾强度

const CURSOR_PAGE = `<!doctype html>
<html><head><meta charset="utf-8">
<style>
  html,body{margin:0;padding:0;width:100%;height:100%;background:transparent;overflow:hidden;
            cursor:none;user-select:none;-webkit-user-select:none;}
  /* 自绘指针：mascot 形象 + 一圈强调环 + 点击脉冲 */
  #c{position:absolute;left:0;top:0;width:64px;height:64px;margin:-32px 0 0 -32px;
     will-change:transform;transition:none;}
  #mascot{position:absolute;inset:0;width:64px;height:64px;object-fit:contain;
          filter:drop-shadow(0 2px 6px rgba(0,0,0,.45));}
  /* 点击脉冲环：从 0 扩到 1.9 并淡出，给用户"这一下点在这儿"的确定感 */
  #pulse{position:absolute;left:50%;top:50%;width:18px;height:18px;margin:-9px 0 0 -9px;
         border-radius:50%;border:2px solid rgba(96,165,250,.95);opacity:0;
         box-shadow:0 0 12px rgba(96,165,250,.55);}
  /* 移动拖尾：光标后的一小段渐隐线，指示"正在移动过去" */
  #trail{position:absolute;left:0;top:0;height:3px;transform-origin:0 50%;
         border-radius:2px;background:linear-gradient(90deg,rgba(96,165,250,0),rgba(96,165,250,.5));}
</style></head>
<body>
  <div id="c">
    <div id="trail"></div>
    <img id="mascot" alt="">
    <div id="pulse"></div>
  </div>
<script>
  const c=document.getElementById('c'),pulse=document.getElementById('pulse'),
        trail=document.getElementById('trail'),mascot=document.getElementById('mascot');
  let cur={x:0,y:0},tgt={x:0,y:0},pulseAt=0,trailAt=0,busy=false;
  function render(t){
    // 缓动跟随：让指针有"飞过去"的观感，而不是瞬移
    cur.x+=(tgt.x-cur.x)*0.35; cur.y+=(tgt.y-cur.y)*0.35;
    c.style.transform='translate('+cur.x+'px,'+cur.y+'px)';
    // 点击脉冲：0.45s 内从 scale(0.4) 扩到 scale(2.2) 并淡出
    const dt=(t-pulseAt)/450;
    if(pulseAt&&dt>=0&&dt<=1){ pulse.style.opacity=String(1-dt);
      pulse.style.transform='scale('+(0.4+dt*1.8)+')'; }
    else { pulse.style.opacity='0'; pulse.style.transform='scale(0.4)'; }
    // 拖尾：移动后 0.4s 内衰减
    const dtr=(t-trailAt)/400;
    const mv=Math.hypot(tgt.x-cur.x,tgt.y-cur.y);
    if(trailAt&&dtr>=0&&dtr<=1&&mv>1){
      const len=Math.min(90,mv*1.6)*(1-dtr);
      const ang=Math.atan2(tgt.y-cur.y,tgt.x-cur.x)*180/Math.PI;
      trail.style.width=len+'px';
      trail.style.opacity=String((1-dtr)*0.75);
      trail.style.transform='translate('+(cur.x-30)+'px,'+(cur.y-30)+'px) rotate('+ang+'deg)';
    } else { trail.style.opacity='0'; }
    // 忙碌：mascot 轻微呼吸，提示"agent 正在操作，别急着抢鼠标"
    if(busy){ const s=1+0.06*Math.sin(t/160); mascot.style.transform='scale('+s+')'; }
    else { mascot.style.transform='scale(1)'; }
    requestAnimationFrame(render);
  }
  requestAnimationFrame(render);
  window.__agentCursor={
    move(x,y,motion){ tgt.x=x; tgt.y=y; if(motion){trailAt=performance.now();} },
    click(){ pulseAt=performance.now(); },
    setBusy(b){ busy=!!b; },
    setVisible(v){ c.style.display=v?'':'none'; },
    hide(){ c.style.display='none'; },
  };
</script></body></html>`;

// 确保 overlay 窗口存在；dispIdx 决定铺在哪块屏
function ensureCursorWindow(dispIdx) {
  try {
    if (cursorWin && !cursorWin.isDestroyed()) return cursorWin;
  } catch (e) {}
  const displays = screen.getAllDisplays();
  const d = displays[dispIdx] || displays[0] || screen.getPrimaryDisplay();
  const b = d.bounds;
  cursorWin = new BrowserWindow({
    x: b.x,
    y: b.y,
    width: b.width,
    height: b.height,
    transparent: true,
    frame: false,
    resizable: false,
    movable: false,
    minimizable: false,
    maximizable: false,
    fullscreenable: false,
    skipTaskbar: true,
    show: false,
    hasShadow: false,
    enableLargerThanScreen: false,
    // ★ 关键：不参与焦点与输入，只作视觉层
    focusable: false,
    acceptFirstMouse: true,
    webPreferences: {
      contextIsolation: true,
      nodeIntegration: false,
      backgroundThrottling: false,
    },
  });
  // 鼠标穿透：光标层绝不能挡住用户点击
  try {
    cursorWin.setIgnoreMouseEvents(true, { forward: true });
  } catch (e) {}
  // 置顶但**不抢焦点**（screen-saver 级会盖住全屏应用，normal 会挡在普通窗口之上）
  try {
    cursorWin.setAlwaysOnTop(true, "screen-saver");
  } catch (e) {}
  try {
    cursorWin.setVisibleOnAllWorkspaces(true, { visibleOnFullScreen: true });
  } catch (e) {}
  cursorWin.loadURL("data:text/html;charset=utf-8," + encodeURIComponent(CURSOR_PAGE));
  cursorWin.webContents.once("did-finish-load", () => {
    // 把 mascot 注入为 data URL：file:// 在 data: 页面里取不到本地文件
    try {
      const p = path.join(__dirname, "renderer", "mascot.webp");
      if (fs.existsSync(p)) {
        const b64 = fs.readFileSync(p).toString("base64");
        cursorWin.webContents
          .executeJavaScript(
            "document.getElementById('mascot').src='data:image/webp;base64," + b64 + "'",
          )
          .catch(() => {});
      }
    } catch (e) {}
    try {
      cursorWin.showInactive();
    } catch (e) {}
  });
  return cursorWin;
}

function agentCursorEval(js, dispIdx) {
  try {
    const w = ensureCursorWindow(dispIdx);
    if (!w || w.webContents.isLoading()) return null;
    w.webContents.executeJavaScript(js).catch(() => {});
  } catch (e) {}
  return null;
}

// 移动自绘光标到屏幕物理坐标（不动真实鼠标）
function agentCursorMove(x, y, dispIdx) {
  cursorPos.x = x;
  cursorPos.y = y;
  agentCursorEval(
    "window.__agentCursor&&window.__agentCursor.move(" +
      Math.round(x) +
      "," +
      Math.round(y) +
      ",1)",
    dispIdx,
  );
  showAgentCursor(dispIdx);
}

// 点击脉冲
function agentCursorClick(dispIdx) {
  agentCursorEval("window.__agentCursor&&window.__agentCursor.click()", dispIdx);
}

// 忙碌呼吸
function agentCursorBusy(busy, dispIdx) {
  cursorBusy = !!busy;
  agentCursorEval("window.__agentCursor&&window.__agentCursor.setBusy(" + (busy ? "1" : "0") + ")", dispIdx);
}

function showAgentCursor(dispIdx) {
  try {
    const w = ensureCursorWindow(dispIdx);
    cursorVisible = true;
    agentCursorEval("window.__agentCursor&&window.__agentCursor.setVisible(1)", dispIdx);
    if (!w.isVisible()) w.showInactive();
  } catch (e) {}
}

function hideAgentCursor() {
  cursorVisible = false;
  try {
    if (cursorWin && !cursorWin.isDestroyed()) {
      agentCursorEval("window.__agentCursor&&window.__agentCursor.setVisible(0)", 0);
      cursorWin.hide();
    }
  } catch (e) {}
}

module.exports = {
  ensureCursorWindow,
  agentCursorMove,
  agentCursorClick,
  agentCursorBusy,
  showAgentCursor,
  hideAgentCursor,
  isVisible: () => cursorVisible,
  pos: () => ({ ...cursorPos }),
  // 记住最后一次点击命中的窗口句柄。
  //
  // 键盘类 action（keypress/hotkey/type）不带坐标，Win32 又必须知道往哪个
  // 窗口发消息 —— 而后台注入**不会移动真实光标**，所以 GetCursorPos 拿到的是
  // **用户自己**的光标位置（很可能根本不在 agent 要操作的地方）。
  // 因此这里显式记录上一次鼠标操作的目标窗口，键盘动作就投递到那里。
  lastHwnd: null,
  setLastHwnd(h) {
    lastHwnd = h || null;
  },
  get busy() {
    return cursorBusy;
  },
};
