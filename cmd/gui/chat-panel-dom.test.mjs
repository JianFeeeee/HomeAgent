// chat 子面板 DOM 结构判据（2026-10-05）。
//
// ## 要判的是什么
//
// 实测故障：对话页里「终端 / 运行中命令 / 记忆 / 上下文 / 知识」五个页签
// 点进去全是空白，只有「对话」和「星图」正常。
//
// 根因是 **HTML 少一个 </div>**：构造星图面板的那段 HTML 里有 4 个
// <div> 开、只有 3 个 </div> 闭，于是它没闭合 —— 后面拼接的所有
// chat-panel 都被浏览器塞进 chat-panel-starmap **内部**。
//
// 实测证据（CDP 查真实 DOM）：
//
//   .chat-layout 的直接子节点只有 3 个：
//     DIV.chat-tabs / DIV#chat-panel-chat / DIV#chat-panel-starmap
//
//   而 terminal/cmd/memory/context/knowledge 的父节点是
//   chat-panel-starmap —— 不是同级的 chat-layout。
//
// 后果：这些面板 display 是 flex（switchChatPanel 把 active 加对了），
// 但父级 display:none ⇒ getBoundingClientRect() 恒为 0x0 ⇒ 看起来空白。
//
// ★ 为什么需要判据：标签不配对在源码里完全看不出来（字符串拼接 +
//   跨行的 HTML 片段），浏览器也不报错 —— 它只是「尽力修复」成畸形结构。
//   只有断言「开闭标签数相等」才能挡住。
//
// 运行：node chat-panel-dom.test.mjs

import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const APP = join(here, "renderer", "app.js");

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) {
    console.log(`  ✓ ${name}`);
  } else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

if (!existsSync(APP)) {
  console.error(`找不到 ${APP}`);
  process.exit(1);
}
const src = readFileSync(APP, "utf8");

// ── 1) 抽出构造 chat 子面板的整段 HTML ─────────────────────────────
//
// 从第一个 chat-panel 面板开始，到最后一个面板结束。

const startIdx = src.indexOf(
  `'<div class="chat-panel active" id="chat-panel-chat">`,
);
if (startIdx < 0) {
  console.error("定位不到 chat 面板段落 —— app.js 结构可能已变");
  process.exit(1);
}
const lastIdx = src.indexOf("chat-panel-knowledge");
const seg = src.slice(startIdx, lastIdx + 4000);

// 把跨行拼接的字符串还原成一段连续 HTML：
//   '...' + \n  '...'   →  '...'
const html = seg
  .replace(/'\s*\+\s*\n\s*'/g, "")
  .replace(/'\s*\+\s*'/g, "")
  .replace(/"/g, "'");

const opens = (html.match(/<div\b/g) || []).length;
const closes = (html.match(/<\/div>/g) || []).length;

check(
  "chat 面板段落 div 标签配平",
  opens === closes,
  `开 ${opens} / 闭 ${closes}，差 ${opens - closes} —— ` +
    "少一个 </div> 会让后面所有面板被塞进前一个面板内部（表现为空白）",
);

// ── 2) 每个 chat-panel 自身也必须配平 ─────────────────────────────
//
// 总数配平还不够：某处多一个 </div>、另一处少一个，总数仍为 0。
// 逐面板断言能定位到具体是哪个。

const PANELS = ["chat", "starmap", "terminal", "cmd", "memory", "context", "knowledge"];
const unbalanced = [];
for (const name of PANELS) {
  const marker = `id='chat-panel-${name}'`;
  const at = html.indexOf(marker);
  if (at < 0) {
    unbalanced.push(`${name}(未找到)`);
    continue;
  }
  // 从该面板的 <div class="chat-panel 开始算（marker 之前最近的那个）
  const from = html.lastIndexOf("<div", at);
  // 到下一个 chat-panel 之前为止
  const nextAt = html.indexOf("id='chat-panel-", at + marker.length);
  const to = nextAt < 0 ? html.length : html.lastIndexOf("<div", nextAt);
  const piece = html.slice(from, to);
  const o = (piece.match(/<div\b/g) || []).length;
  const c = (piece.match(/<\/div>/g) || []).length;
  if (o !== c) unbalanced.push(`${name}(开${o}/闭${c})`);
}
check(
  "各子面板独立配平",
  unbalanced.length === 0,
  unbalanced.length ? "不配平：" + unbalanced.join("、") : "",
);

// ── 3) 所有 chat-panel 必须同属一个父容器 ─────────────────────────
//
// 形态判据：源码里每个面板片段的起始处都应是独立的 <div class="chat-panel"，
// 而不是接在上一段的未闭合结构里。逐面板检查它前面紧邻的字符 ——
// 合法的是 …>'（一个字符串字面量的结束）或 +（拼接）。

const nested = [];
for (const name of PANELS) {
  const marker = `id='chat-panel-${name}'`;
  const at = html.indexOf(marker);
  if (at < 0) continue;
  const from = html.lastIndexOf("<div", at);
  const before = html.slice(Math.max(0, from - 24), from);
  // 合法前缀：以 ' 结尾（字符串结束）或 '+ 结尾（仍在拼接）
  if (!/'[+]?$/.test(before.trimEnd()) && !/\+\s*$/.test(before)) {
    nested.push(`${name}(前缀=${JSON.stringify(before.slice(-12))})`);
  }
}
check(
  "各面板顶层不被嵌套进前一面板",
  nested.length === 0,
  nested.length
    ? "这些面板疑似接在未闭合结构里：" + nested.join("、")
    : "",
);

// ── 汇总 ────────────────────────────────────────────────────────────

console.log("");
console.log(`  chat 面板段落: <div> ${opens} / </div> ${closes}`);
console.log(`  面板数: ${PANELS.length}`);
console.log("");
if (failures > 0) {
  console.log(`失败 ${failures} 条`);
  process.exit(1);
}
console.log("全部通过");