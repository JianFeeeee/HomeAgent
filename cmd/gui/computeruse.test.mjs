// computeruse 能力面 + PiDeck 借鉴声明的判据。
//
// ## 要判的是什么
//
// 1) computeruse 高级操作。两处实现（cmd/gui/main.js 的本机桥、
//    cmd/waiter/device.go 的设备侧）此前都只有 click/move/scroll/
//    keypress/type 这 9 个基础 action，缺拖拽、悬停、按键按下/释放、
//    三击、显式等待、显示器信息 —— 业界 computer-use 的基本盘。
//
// 2) HiDPI 坐标换算。截图给模型的是**物理像素**，而 SetCursorPos /
//    xdotool 期望**逻辑点(DIP)**。原先只加了 display bounds 的原点
//    偏移，没换算 scaleFactor ⇒ 150% 缩放屏上点击系统性偏移。
//
// 3) PiDeck 借鉴声明。用户要求显式声明 GUI 借鉴了 PiDeck 的设计与实现，
//    这类"来源说明"一旦丢失就无从追溯，故钉在这里。
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

// ── 1) 高级操作：GUI 侧 ────────────────────────────────────────────
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

// ── 2) 高级操作：waiter 侧（设备侧主力，Linux 才是真正执行者）──
const wStart = waiter.indexOf("func execComputeruseLinux(");
const wEnd = waiter.indexOf("func execComputeruseWindows(");
const linuxSeg = waiter.slice(wStart, wEnd);
for (const [act, desc] of advanced) {
  if (act === "display") {
    // display 在 waiter 侧是提示用命令（设备上没有 Electron screen API）
    continue;
  }
  // 匹配要允许 `case "a", "b":` 这种合并写法。
  // waiter 的 hover 与 move 同分支（`case "move", "hover":`）、
  // keypress 与 hotkey/combo 同分支，只匹配单个 case 名会误报「未实现」。
  // 用「该 action 名出现在某个 case 标签列表里」来判，而不绑定 case 关键字位置。
  const re = new RegExp('case[^:]*"' + act + '"[^:]*:');
  check(
    `waiter Linux 支持 ${act}（${desc}）`,
    re.test(linuxSeg),
    "waiter Linux 未实现 " + act,
  );
}

// ── 3) HiDPI 坐标换算 ──────────────────────────────────────────────
check(
  "★ GUI 按 scaleFactor 换算坐标",
  /physical\s*!==\s*false/.test(main) && /rawX\s*\/\s*scale/.test(main),
  "未做 物理像素→DIP 换算 ⇒ HiDPI 屏上点击系统性偏移",
);
check(
  "GUI 有 physical 参数供调用方声明坐标空间",
  /params\.physical/.test(main),
  "缺少 physical 开关，模型无法声明坐标已是逻辑点",
);
check(
  "★ waiter Linux 有 normKeySpec 归一化组合键",
  /func normKeySpec/.test(waiter),
  "缺少组合键归一化 ⇒ 模型输出 \"Ctrl+C\" 这类自然写法会失效",
);
check(
  "waiter type 用 --clearmodifiers",
  /type.*--clearmodifiers|--clearmodifiers.*--delay/s.test(linuxSeg),
  "type 未清修饰键 ⇒ 残留 Ctrl 会把后续输入变成快捷键",
);

// ── 4) 拖拽要分步移动（部分应用依赖中间 move 事件）────────────────
check(
  "★ 拖拽分步移动（GUI）",
  /steps[\s\S]{0,400}SetCursorPos/.test(main),
  "拖拽一步到位 ⇒ canvas 拖拽/排序类操作会失效",
);
check(
  "★ 拖拽分步移动（waiter）",
  /steps[\s\S]{0,400}mousemove/.test(linuxSeg),
  "拖拽一步到位 ⇒ canvas 拖拽/排序类操作会失效",
);

// ── 5) PiDeck 借鉴声明 ─────────────────────────────────────────────
// 声明位置：app.js（工具组卡与呼吸光晕的逻辑）、style.css（三项动画）、
// 以及本仓的文档。用户明确要求「显式声明 GUI 借鉴了 PiDeck 的设计与
// 实现」—— 这类来源说明一旦丢失，后来人就无法判断某段设计是本地决定
// 还是外部借鉴，也无从追溯许可与出处。
check(
  "★ app.js 有 PiDeck 借鉴说明",
  /PiDeck/.test(app),
  "app.js 缺 PiDeck 借鉴说明",
);
check("★ style.css 有 PiDeck 借鉴说明", /PiDeck/.test(css), "style.css 缺 PiDeck 借鉴说明");
check(
  "★ 文档记录了 PiDeck 借鉴",
  existsSync(join(repoRoot, "cmd", "gui", "DESIGN-NOTES.md")),
  "缺 cmd/gui/DESIGN-NOTES.md（借鉴来源与设计决策的落点）",
);

// 借鉴的三个具体设计要写明（便于追溯"抄了什么"）
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