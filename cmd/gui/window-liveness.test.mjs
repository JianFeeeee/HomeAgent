// 窗口/renderer 存活性的行为判据（2026-10-05）。
//
// ## 要判的是什么
//
// 实测故障：GUI 在后台挂很久（数小时）后，从托盘打开是**白屏**，
// 且不会自愈 —— 重启进程才恢复。
//
// 原因链（三个洞叠加）：
//
//  1. **没有任何崩溃监听**。全代码搜不到 render-process-gone /
//     unresponsive / child-process-gone。renderer 被 Chromium 回收
//     （GPU context lost，或渲染进程被判hung 杀掉）后，没有任何代码
//     感知这件事。
//
//  2. **窗口「可见」不等于「有内容」**。renderer 挂掉后窗口句柄还在、
//     isVisible() 仍返回 true —— 而 showMainWindow() 原来只判
//     `if (!isVisible()) show()`，于是 show() 被跳过，从托盘打开就是白的。
//
//  3. **ready-to-show 用了 once**。once 只等第一次首帧；窗口被系统回收后
//     重建、或 GPU 重启后重新 load，那些路径都需要再show 一次，
//     once 让它们静默失败。
//
// 仓库历史上有过「白屏修复(惰性Tray)」提交，但那只修了**首次**显示
// 时机，没管**长期后台后**。本判据盯后者。
//
// ## 为什么用文本扫描而非真跑
//
// 复现需要「挂后台数小时 + renderer 被回收」，构造那个时序要真Electron
// 且依赖驱动行为，CI 里不可靠。故从源码判「该有的监听与判据是否在位」——
// 与 endpoint-align / device-bridge-liveness 同路子。真正需要证明
// 「重载后能出内容」时，靠 device-bridge-liveness 那类跑真实函数的判据。
//
// 运行：node window-liveness.test.mjs

import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const MAIN = join(here, "main.js");

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) {
    console.log(`  ✓ ${name}`);
  } else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

if (!existsSync(MAIN)) {
  console.error(`找不到 main.js：${MAIN}`);
  process.exit(1);
}
const src = readFileSync(MAIN, "utf8");

// ── 1) renderer 崩溃监听必须存在 ───────────────────────────────────
//
// 没有它 ⇒ renderer 挂掉全程无感知，窗口留在白屏态。

check(
  "监听 render-process-gone",
  /webContents\.on\(\s*["']render-process-gone["']/.test(src),
  "没有 renderer 崩溃监听 —— 渲染进程挂掉时窗口会永久白屏",
);

const goneHandler = src.match(
  /webContents\.on\(\s*["']render-process-gone["'][\s\S]{0,900}?\n {2}\}\);/,
);
check(
  "renderer 崩溃后有恢复动作",
  goneHandler ? /reload\(\)/.test(goneHandler[0]) : false,
  goneHandler
    ? "崩溃分支里没有 reload —— 监听到了也不自愈"
    : "找不到 render-process-gone 处理块",
);

// OOM 时必须**不**reload：重建只会立刻再崩一次，反复重启会把机器拖死。
check(
  "OOM 崩溃不自动重载",
  goneHandler ? /oom/.test(goneHandler[0]) : false,
  "未区分 reason==='oom' —— 内存不足时反复 reload 会打爆机器",
);

// ── 2) GPU 崩溃监听 ────────────────────────────────────────────────
//
// 无边框窗口（frame:false）在 GPU 进程重启后，常见「停在上一帧不动」
// 或整片白 —— 与 renderer 崩溃是不同现象，但表现重合。

check(
  "监听 child-process-gone 且覆盖 GPU",
  /app\.on\(\s*["']child-process-gone["']/.test(src) &&
    /type\s*!==\s*["']GPU["']\s*\)?\s*return/.test(src),
  "没有 GPU 崩溃恢复 —— 无边框窗口在 GPU 重启后可能停在空白帧",
);

// ── 3) 显示路径必须查 renderer 健康 ───────────────────────────────
//
// 这是「白屏」被直接看见的地方：只判 isVisible() 会跳过 show()。

// showMainWindow 的源码切片。
//
// ★ 不用 /function showMainWindow\(\)[\s\S]*?\n}\n/ —— 函数体内有多处
//   嵌套闭合（try/catch/if），非贪婪匹配会在中途的 `\n}\n` 就停，
//   切片里根本不含函数尾部那几行。实测：删掉 isCrashed 判定后，
//   「showMainWindow 检查 isCrashed」仍显示绿 —— 切片里还有别的
//   isCrashed 残留，而真正的判定已在切片外。
//   改为：从函数声明行扫到**列 0** 的 `}`（顶层函数结束）。
function sliceFunction(name) {
  const start = src.indexOf(`function ${name}(`);
  if (start < 0) return null;
  const lines = src.slice(start).split("\n");
  const out = [lines[0]];
  for (let i = 1; i < lines.length; i++) {
    out.push(lines[i]);
    // 列 0 的右花括号 = 顶层函数体结束（嵌套闭合都有缩进）。
    if (lines[i] === "}") return out.join("\n");
  }
  return out.join("\n");
}

const showFn = sliceFunction("showMainWindow");
// 只认**代码里的**判定（if (wc.isCrashed())），不认注释里的说明文字
// ——注释里同样写着 isCrashed()。
const hasCrashCheck = showFn ? /if \(\w+\.isCrashed\(\)\)/.test(showFn) : false;
check(
  "showMainWindow 检查 isCrashed",
  hasCrashCheck,
  showFn
    ? "没有 isCrashed 判定 —— renderer 已死时仍会跳过 show，开出白窗口"
    : "找不到 showMainWindow",
);

// 判据要钉的是「isCrashed 为真时 reload 被调用」。
// 用 if(...) 紧跟 reload 的短窗匹配，避免 showMainWindow 之外任何
// reload（render-process-gone 里就有一个）被误当成满足。
// showMainWindow 现在是**字符串**（sliceFunction 的返回），不是 match 结果。
// 早期版本这里写的是 showFn[0]，即取首字符 "f" —— 判据永远为红，
// 而目之所及一切正常。最隐蔽的一类判据 bug：切片方式一换，取值就错。
const crashReload = showFn
  // 要求「if (...isCrashed()) {...reload()」的形状：
  // 注释里也写着 isCrashed()，纯匹配 isCrashed 会命中注释。
  ? showFn.match(/if \(\w+\.isCrashed\(\)\)\s*\{[\s\S]{0,400}?reload\(\)/)
  : null;
check(
  "isCrashed 为真时重载",
  !!crashReload,
  "判定了崩溃但没在同一分支里 reload —— 白屏依旧",
);

// ── 4) ready-to-show 不能是 once ──────────────────────────────────
//
// once 只等第一次首帧；窗口重建 / GPU 重启后的再显示路径全靠它。

check(
  "ready-to-show 用 on 而非 once",
  /mainWindow\.on\(\s*["']ready-to-show["']/.test(src) &&
    !/mainWindow\.once\(\s*["']ready-to-show["']/.test(src),
  "仍是 once —— 窗口重建 / GPU 重启后再显示会静默失败",
);

// ── 5) 所有「显示窗口」的入口都应走 showMainWindow ──────────────────
//
// 通知点击、托盘菜单若自己调 show()，就绕过了上面的健康检查。

const directShows = [
  ...src.matchAll(/mainWindow\.show\(\)/g),
].length;
const showMainCalls = [...src.matchAll(/showMainWindow\(\)/g)].length;
check(
  "无绕过健康检查的直接 show",
  directShows <= 2, // createWindow 的 ready-to-show + showMainWindow 内部各一处
  `发现 ${directShows} 处 mainWindow.show()；除 createWindow 与 showMainWindow 自身外，` +
    "其余入口（如通知点击）应改走 showMainWindow()，否则会开出白窗口",
);
check(
  "通知点击走 showMainWindow",
  /n\.on\(["']click["'][\s\S]{0,300}?showMainWindow\(\)/.test(src),
  "通知点击仍直接 show() —— 绕过 renderer 健康检查",
);

// ── 汇总 ────────────────────────────────────────────────────────────

console.log("");
console.log(`  show() 调用 ${directShows} 处 / showMainWindow() 调用 ${showMainCalls} 处`);
console.log("");
if (failures > 0) {
  console.log(`失败 ${failures} 条`);
  process.exit(1);
}
console.log("全部通过");