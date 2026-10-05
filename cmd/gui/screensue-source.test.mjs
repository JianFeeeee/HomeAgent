// screensue「从路径加载」的行为判据（2026-10-05）。
//
// ## 要判的是什么
//
// 改之前 screensue 只能把内容**内联**在命令字符串里：
//
//	homeagent-screensue <div>…</div>
//
// 于是场景被限死了：
//   · 一屏复杂看板（几十 KB HTML）塞不进 cmd_type 字段
//   · 模型得先把整段 HTML 生成出来再当参数传，token 成本与出错面都大
//   · 内容改了要重新发一次命令
//
// 新增三种写法：
//   ① @<路径>            从文件读，按扩展名决定渲染方式
//   ② @<路径>#<块名>     抽 <!--sue:名字--> … <!--/sue--> 之间的片段
//   ③ <字面内容>          原行为，**必须完全不变**
//
// ## 判据形态
//
// 真跑 loadCapabilitySource：造临时 .html/.md 文件与多块文件，
// 验「按扩展名渲染 / 块抽取 / 缺块报错 / 目录拒绝 / 超限拒绝 /
// @前缀才触发加载（字面内容不受影响）」。
//
// 运行：node cmd/gui/screensue-source.test.mjs

import { readFileSync, writeFileSync, mkdtempSync, rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";
import vm from "node:vm";

import { createRequire } from "node:module";
const nodeRequire = createRequire(import.meta.url);
import * as nodeFs from "node:fs";

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, "main.js"), "utf8");

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) console.log(`  ✓ ${name}`);
  else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

function extractFn(name) {
  let start = src.indexOf(`function ${name}(`);
  if (start === -1) throw new Error(`cannot find function ${name} in main.js`);
  let i = src.indexOf("{", start);
  let depth = 0;
  for (let j = i; j < src.length; j++) {
    const c = src[j];
    if (c === '"' || c === "'" || c === "`") {
      const q = c;
      j++;
      while (j < src.length && src[j] !== q) {
        if (src[j] === "\\") j++;
        j++;
      }
      continue;
    }
    if (c === "/" && src[j + 1] === "/") {
      while (j < src.length && src[j] !== "\n") j++;
      continue;
    }
    if (c === "/" && src[j + 1] === "*") {
      j = src.indexOf("*/", j) + 1;
      continue;
    }
    if (c === "{") depth++;
    else if (c === "}") {
      depth--;
      if (depth === 0) return src.slice(start, j + 1);
    }
  }
  throw new Error(`unbalanced braces for ${name}`);
}

const dir = mkdtempSync(join(tmpdir(), "screensue-test-"));
const p = (n) => join(dir, n);
writeFileSync(p("board.html"), "<h1>看板</h1><p>hello</p>", "utf-8");
writeFileSync(p("notes.md"), "# 标题\n- 一点", "utf-8");
writeFileSync(
  p("multi.html"),
  "<!--sue:alerts--><div class=alerts>A1 A2</div><!--/sue-->\n" +
    "<!--sue:schedule--><div>三点开会</div><!--/sue-->",
  "utf-8",
);
writeFileSync(p("big.html"), "x".repeat(2 * 1024 * 1024 + 10), "utf-8");

const sandbox = {
  console,
  // main.js 里用的是 CJS 的 require(...)，在 vm 沙箱里必须显式注入；
  // 不能写 require: (m) => require(m)（ESM 作用域里没有 require）。
  require: (m) => nodeRequire(m),
  fs: nodeFs,
};
// ⚠️ 不能写 require: (m) => require(m)：本文件是 ESM（.mjs），没有 require。
vm.createContext(sandbox);
vm.runInContext(
  [
    "const CAPABILITY_SOURCE_MAX_BYTES = 2 * 1024 * 1024;",
    extractFn("loadCapabilitySource"),
    extractFn("resolveCapabilityArg"),
    "globalThis.__load = loadCapabilitySource;",
    "globalThis.__resolve = resolveCapabilityArg;",
  ].join("\n"),
  sandbox,
);
const load = sandbox.__load;
const resolve = sandbox.__resolve;

console.log("screensue 从路径加载");

// ① .html 按 HTML 原样渲染
{
  const r = load(p("board.html"), "", "html");
  check("读 .html 成功", r.ok, r.err);
  check(".html 原样返回（不被转义）", r.ok && r.content.includes("<h1>看板</h1>"),
    r.ok ? r.content.slice(0, 40) : "");
  check("from 回报绝对路径", r.ok && r.from === p("board.html"));
}

// ② .md 按纯文本等宽渲染（转义）
{
  const r = load(p("notes.md"), "", "html");
  check("读 .md 成功", r.ok, r.err);
  check(".md 包 <pre> 且被转义", r.ok && r.content.startsWith("<pre") && !r.content.includes("<h1>"),
    r.ok ? r.content.slice(0, 50) : "");
}

// ③ 命名块抽取
{
  const a = load(p("multi.html"), "alerts", "html");
  const s = load(p("multi.html"), "schedule", "html");
  check("抽指定块 alerts", a.ok && a.content.includes("A1 A2") && !a.content.includes("三点开会"),
    a.ok ? a.content : a.err);
  check("抽另一块 schedule", s.ok && s.content.includes("三点开会") && !s.content.includes("A1"),
    s.ok ? s.content : s.err);
  check("from 带 #块名", a.ok && a.from.endsWith("#alerts"));
  const miss = load(p("multi.html"), "nope", "html");
  check("缺块明确报错", !miss.ok && /未找到块/.test(miss.err), miss.err);
}

// ④ 错误路径都给出可读原因，不静默
{
  check("目录被拒", !load(dir, "", "html").ok);
  check("不存在的文件被拒", !load(p("nope.html"), "", "html").ok);
  check("空路径被拒", !load("", "", "html").ok);
  const big = load(p("big.html"), "", "html");
  check("超 2MB 被拒且说明大小", !big.ok && /过大/.test(big.err), big.err);
}

// ⑤ ★ 字面内容（无 @ 前缀）必须完全不受影响
//    调用点只在 raw.startsWith("@") 时才走加载；这里直接判该分支条件。
{
  check("字面内容不触发路径加载（需 @ 前缀）", !"<div>".startsWith("@"));
  check("@ 字面量确实会触发", "@foo".startsWith("@"));
}

// ⑥ ★ speakeruse / clipboardsue 复用同一个 loader（text 模式）
//    这三个能力都是「<名字> <内联文本>」，长文本都塞不进 command 字段。
{
  const r = load(p("notes.md"), "", "text");
  check("text 模式返回纯文本（不包 <pre>）",
    r.ok && r.content.startsWith("# 标题") && !r.content.includes("<pre"),
    r.ok ? r.content.slice(0, 40) : r.err);
  const h = load(p("board.html"), "", "text");
  check("text 模式不做 HTML 分流（原样返回）",
    h.ok && h.content.includes("<h1>看板</h1>"), h.ok ? h.content.slice(0, 40) : h.err);
  const blk = load(p("multi.html"), "alerts", "text");
  check("text 模式同样支持命名块", blk.ok && blk.content.includes("A1 A2"));
}

// ⑦ resolveCapabilityArg 的解析口径
{
  check("无 @ 前缀 ⇒ 不命中（保持内联行为）", resolve("你好").hit === false);
  check("@ 路径解析", resolve("@/tmp/a.html").hit === true);
  check("@ 路径 + #块名", (() => {
    const r = resolve("@/tmp/a.html#alerts");
    return r.hit && r.filePath === "/tmp/a.html" && r.blockName === "alerts";
  })());
  check("@ 路径无块名 ⇒ blockName 为空",
    resolve("@/tmp/a.html").blockName === "");
}

rmSync(dir, { recursive: true, force: true });

console.log(
  failures === 0
    ? "\nAll screensue-source checks passed."
    : `\n${failures} check(s) failed.`,
);
process.exit(failures === 0 ? 0 : 1);