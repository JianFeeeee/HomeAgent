// computeruse 能力面 + GUI 启动期误判的判据。
//
// 覆盖三组回归：
//
// 1) computeruse 高级操作（两端：GUI 本机桥 / waiter 设备侧）。
//    此前两端都只有 9 个基础 action，缺拖拽、悬停、按下/释放、三击、
//    显式等待、显示器信息 —— 与业界 computer-use 的基本盘差距明显。
//
// 2) HiDPI 与 koffi 调用约定。
//    · 坐标：截图给模型的是**物理像素**，而 SetCursorPos/xdotool 期望
//      **逻辑点(DIP)**；原先只加了 bounds 原点偏移，没换算 scaleFactor
//      ⇒ 150% 缩放屏上点击系统性偏移。
//    · 类型名：koffi 不认 Win32 的 `byte`，曾让 keybd_event 整体
//      拿不到 ⇒ 组合键全废，而单键 click 不受影响、极易漏掉。
//
// 3) 启动期误判「服务端未启动」。
//    isServerRunning 硬编码 localhost:8080，在「服务端跑在 WSL/远端」
//    的部署下必然探不通 → 误判服务没起 → 去拉起 GUI 旁并不存在的
//    homed.exe → 干等 8s 超时 → 每次启动刷一条
//    "homed failed to start within timeout"。
//
// 另含 PiDeck 借鉴声明（用户明确要求显式记录设计与实现的出处）。
//
// 运行：node computeruse.test.mjs

import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, "..", "..");
const main = readFileSync(join(here, "main.js"), "utf8");
const app = readFileSync(join(here, "renderer", "app.js"), "utf8");
const css = readFileSync(join(here, "renderer", "style.css"), "utf8");
const waiter = readFileSync(join(repoRoot, "cmd", "waiter", "device.go"), "utf8");

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) console.log(`  ✓ ${name}`);
  else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

// ── 1) 高级操作 ────────────────────────────────────────────────────
const advanced = [
  ["drag", "拖拽"],
  ["hover", "悬停（触发 tooltip）"],
  ["mousedown", "按键按下（与 mouseup 配对做按住）"],
  ["mouseup", "按键释放"],
  ["tripleclick", "三击选中整行"],
  ["middleclick", "中键点击"],
  ["hotkey", "组合键"],
  ["wait", "显式等待（等 UI/动画完成）"],
  ["display", "显示器几何信息（多屏定位）"],
];
for (const [act, desc] of advanced) {
  check(
    `GUI 支持 ${act}（${desc}）`,
    new RegExp('case\\s+"' + act + '"').test(main),
    "未实现 " + act,
  );
}

const wStart = waiter.indexOf("func execComputeruseLinux(");
const wEnd = waiter.indexOf("func execComputeruseWindows(");
const linuxSeg = waiter.slice(wStart, wEnd);
for (const [act, desc] of advanced) {
  if (act === "display") continue; // waiter 侧无 Electron screen API
  // 匹配要允许 `case "a", "b":` 合并写法：waiter 的 hover 与 move 同分支、
  // keypress 与 hotkey/combo 同分支，只匹配单个 case 名会误报「未实现」。
  const re = new RegExp('case[^:]*"' + act + '"[^:]*:');
  check(`waiter Linux 支持 ${act}（${desc}）`, re.test(linuxSeg), "waiter Linux 未实现 " + act);
}

// ── 2) HiDPI 与 koffi 约定 ─────────────────────────────────────────
// ★ 坐标空间：Win32 = 物理像素 = 截图像素，**不应再除以 scaleFactor**。
//
// 实测（Electron 主进程内）：
//   screen API (DIP) : 1260 x 840  scaleFactor=2
//   GetSystemMetrics : 2520 x 1680
//   SetCursorPos(200,200) -> GetCursorPos 读回 (200,200)   ← 恒等，不缩放
//
// 因为 Electron 主进程是 per-monitor DPI-aware，Win32 坐标不虚拟化。
// （对照：普通 node 进程里同一函数读到 1260x840，那是 DPI-*unaware*
//   的虚拟化行为，不能拿来推断 Electron 内的行为。）
//
// 本仓曾按「SetCursorPos 期望 DIP」加过 `/ scaleFactor`，那个前提是错的，
// 会把原本正确的点击改坏（150% 屏上点 (600,400) 被送到 (400,267)）。
const kx = main.match(/const absX = Math\.round\([^;]*?physical \? ([^:]+):/);
check(
  "★ Win32 坐标按物理像素直接用（不除 scaleFactor）",
  !!kx && !/\/\s*scale\b/.test(kx[1]),
  kx
    ? "absX 仍对物理像素做了 /scale 换算 ⇒ HiDPI 屏上点击偏移"
    : "未找到 absX 计算",
);
check(
  "physical:false 时才乘回 scaleFactor（DIP→物理）",
  /physical \? rawX : rawX \* scale/.test(main),
  "DIP 输入未乘回缩放比",
);
check(
  "拖拽终点坐标与主坐标用同一套换算",
  /physical \? \(parseFloat\(params\.tox[^\n]*\* scale\)/.test(main),
  "drag 终点仍在用旧的 /scale 逻辑，会与起点偏移不一致",
);
// 只看**真实代码**里 user32.func(...) 传的签名，不看注释
// （注释里会引用 `keybd_event(byte ...)` 描述旧 bug，不能因此误报）。
const kbdSig = main.match(/user32\.func\(\s*"[^"]*keybd_event\((uint8|byte)/);
check(
  "★ keybd_event 用 koffi 支持的类型名（uint8 而非 byte）",
  !!kbdSig && kbdSig[1] === "uint8",
  kbdSig
    ? "user32.func 里用的是 " + kbdSig[1] + "，koffi 不认 `byte` ⇒ 组合键整体报 keybd_event unavailable"
    : "未找到 user32.func(...keybd_event...) 调用",
);
check(
  "keybd_event 解析失败会记录日志",
  /keybd_event signature failed/.test(main),
  "静默 catch 掉签名错误，线上无法诊断",
);
check(
  "★ waiter Linux 有 normKeySpec 归一化组合键",
  /func normKeySpec/.test(waiter),
  "缺少组合键归一化 ⇒ 模型输出 \"Ctrl+C\" 会失效",
);
check(
  "waiter type 用 --clearmodifiers",
  /type[\s\S]{0,80}--clearmodifiers/.test(linuxSeg),
  "type 未清修饰键 ⇒ 残留 Ctrl 会把后续输入变成快捷键",
);
check(
  "★ 拖拽分步移动（GUI）",
  /steps[\s\S]{0,400}SetCursorPos/.test(main),
  "一步到位 ⇒ canvas 拖拽/排序失效",
);
check(
  "★ 拖拽分步移动（waiter）",
  /steps[\s\S]{0,400}mousemove/.test(linuxSeg),
  "一步到位 ⇒ canvas 拖拽/排序失效",
);

// ── 3) 启动期不得误判服务端 ───────────────────────────────────────
check(
  "★ 服务探测不只探 localhost",
  /function serverProbeTargets/.test(main) && /loadConnections\(\)/.test(
    main.slice(main.indexOf("function serverProbeTargets"), main.indexOf("function probeOne")),
  ),
  "未从 connections.json 取已配置端点",
);
check(
  "★ localhost 仅作为兜底候选",
  /push\("http:\/\/localhost:8080"\)/.test(main),
  "缺少 localhost 兜底",
);
check(
  "★ 仅当存在本地 homed 才尝试拉起",
  /else if \(!findHomed\(\)\)/.test(main),
  "无 homed 可执行文件时仍会空等 8s 超时",
);
check(
  "★ 服务可达时跳过 homed 自动拉起",
  /server reachable, skip homed autostart/.test(main),
  "服务在远端时仍会拉起 homed 并超时",
);
check(
  "探测有超时保护（不会无限等）",
  /probeOne[\s\S]{0,300}setTimeout/.test(main),
  "探测缺超时 ⇒ 启动被挂住",
);

// ── 4) 模式 A：agent 独立光标 + 后台注入 ─────────────────────────
// 模式 A 的核心承诺是「不动用户真实鼠标」。这条承诺必须由判据钉住 ——
// 一旦有人图省事改回 SetCursorPos，用户的光标就会被抢，且很难归因。
const injectSrc = readFileSync(join(here, "agent-inject.js"), "utf8");
const cursorSrc = readFileSync(join(here, "agent-cursor.js"), "utf8");
// 去掉整行注释后再查 SetCursorPos —— 注释里会引用这个名字来解释「为什么不用它」
const stripComments = (s) => s.replace(/^\s*\/\/.*$/gm, "");
check(
  "★ 模式 A 不碰真实鼠标（agent-inject 无 SetCursorPos 调用）",
  !/SetCursorPos\(/.test(stripComments(injectSrc)),
  "agent-inject 出现 SetCursorPos 调用 ⇒ 会抢用户鼠标",
);
check(
  "★ 模式 A 不碰真实鼠标（agent-cursor 无 SetCursorPos 调用）",
  !/SetCursorPos\(/.test(stripComments(cursorSrc)),
  "agent-cursor 出现 SetCursorPos 调用 ⇒ 会抢用户鼠标",
);
check(
  "★ koffi 指针参数用 Buffer（普通对象不写回）",
  /Buffer\.alloc\(8\)/.test(injectSrc),
  "指针参数未用 Buffer ⇒ WindowFromPoint 拿不到真实 hwnd，点击会落错窗口",
);
check(
  "★ overlay 默认开启且可切回 real",
  /cursorMode = "overlay"/.test(main) && /cursorMode = "real"/.test(main),
  "未提供 overlay/real 切换",
);
check(
  "自绘光标层不吃点击（setIgnoreMouseEvents）",
  /setIgnoreMouseEvents\(true/.test(cursorSrc),
  "光标层会挡住用户点击",
);

// ── 5) PiDeck 借鉴声明 ─────────────────────────────────────────────
check("★ app.js 有 PiDeck 借鉴说明", /PiDeck/.test(app), "app.js 缺 PiDeck 借鉴说明");
check("★ style.css 有 PiDeck 借鉴说明", /PiDeck/.test(css), "style.css 缺 PiDeck 借鉴说明");
check(
  "★ 文档记录了 PiDeck 借鉴",
  existsSync(join(here, "DESIGN-NOTES.md")),
  "缺 cmd/gui/DESIGN-NOTES.md（借鉴来源与设计决策的落点）",
);
check("声明了三点渐次明暗动画", /haDots/.test(css) && /PiDeck/.test(css));
check("声明了运行态呼吸光晕", /haRunBreathe/.test(css) && /msg-running/.test(css));
check("声明了工具调用分组卡", /tool-group-card/.test(app) && /tool-group-card/.test(css));
check(
  "工具组卡有 PiDeck 出处注释",
  /renderToolGroupOrSingle[\s\S]{0,400}PiDeck/.test(app) ||
    /tool-group-card[\s\S]{0,300}PiDeck/.test(app) ||
    /PiDeck[\s\S]{0,300}tool-group-card/.test(app),
  "renderToolGroupOrSingle 上方应有 PiDeck 出处说明",
);

console.log("");
if (failures > 0) {
  console.log(`全部失败：${failures} 条`);
  process.exit(1);
}
console.log("全部通过");
