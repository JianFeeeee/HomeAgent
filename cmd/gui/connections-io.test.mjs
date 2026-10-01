// connections.json 加载/保存的行为判据。
//
// ## 要判的是什么
//
// loadConnections() 的损坏恢复分支曾把「读到空/半截文件」当成
// 「配置损坏」，于是**把文件改写成空配置**。而 saveConnections() 用
// 非原子的 writeFileSync 截断重写，制造了产生半截内容的窗口。
// 两者叠加 = 一次并发读就能让用户的连接列表永久消失。
//
// 实测事故（2026-10-01 17:21）：connections.json 变成
// {"connections":[],"currentId":null}，连接全丢。
//
// ## 为什么用「跑真实函数」而不是检查源码文本
//
// 判据在 node:vm 沙箱里抽出 main.js 的**真实函数**执行，
// 断言的是文件最终内容与返回值 —— 与线上走同一条代码。
//
// 运行：node connections-io.test.mjs

import { readFileSync, writeFileSync, existsSync, mkdtempSync, readdirSync, unlinkSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";
import vm from "node:vm";

const here = dirname(fileURLToPath(import.meta.url));
const mainSrc = readFileSync(join(here, "main.js"), "utf8");

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) console.log(`  ✓ ${name}`);
  else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

// 抽出真实实现（从 loadConnections 到 findHomed，含 normalize/save）
function extractImpl() {
  const start = mainSrc.indexOf("function loadConnections() {");
  const end = mainSrc.indexOf("function findHomed() {");
  if (start < 0 || end < 0) throw new Error("无法从 main.js 抽出 load/save 实现");
  return mainSrc.slice(start, end);
}

function makeSandbox(file) {
  // 本地 stub：Node 的 renameSync 在同卷内会移动（源消失），
// 但为了让沙箱能在临时目录里跑，这里显式模拟真实语义。
const realUnlink = (p) => {
  try {
    unlinkSync(p);
  } catch (e) {
    /* ignore */
  }
};
const fsStub = {
    existsSync: (p) => existsSync(p),
    readFileSync: (p) => readFileSync(p, "utf8"),
    writeFileSync: (p, c) => writeFileSync(p, c, "utf8"),
    copyFileSync: (a, b) => writeFileSync(b, readFileSync(a)),
    renameSync: (a, b) => {
      writeFileSync(b, readFileSync(a));
      realUnlink(a);
    },
    unlinkSync: realUnlink,
  };
  const ctx = {
    CONNECTIONS_FILE: file,
    fs: fsStub,
    console: { log() {}, error() {} },
    process: { pid: 1234 },
    __dirname: here,
    Date,
    JSON,
    console: { log() {}, error() {}, warn() {} },
  };
  vm.createContext(ctx);
  vm.runInContext(extractImpl(), ctx);
  return ctx;
}

function scenario(name, initialContent) {
  const dir = mkdtempSync(join(tmpdir(), "conn-"));
  const file = join(dir, "connections.json");
  if (initialContent !== null) writeFileSync(file, initialContent, "utf8");
  const ctx = makeSandbox(file);
  const ret = vm.runInContext("loadConnections()", ctx);
  return { file, ret, after: readFileSync(file, "utf8") };
}

const REAL = JSON.stringify({
  connections: [{ id: "abc", name: "local", url: "http://192.168.2.60:8080" }],
  currentId: "abc",
});

// ── 1) 正常配置：原样返回，不改写文件 ─────────────────────────────────
{
  const r = scenario("normal", REAL);
  check(
    "合法 JSON 原样返回",
    r.ret.connections.length === 1 && r.ret.connections[0].id === "abc",
    JSON.stringify(r.ret).slice(0, 80),
  );
  check("合法 JSON 不被改写", r.after === REAL, "文件内容被动了");
}

// ── 2) ★ 核心判据：空文件不得被改写成空配置 ──────────────────────────
{
  const r = scenario("empty", "");
  check(
    "空文件：返回空配置",
    r.ret.connections.length === 0 && r.ret.currentId === null,
    JSON.stringify(r.ret).slice(0, 80),
  );
  check(
    "★ 空文件不被覆写（用户配置不被销毁）",
    r.after === "",
    "空文件被改写成：" + r.after.slice(0, 80),
  );
}

// ── 3) 纯空白（BOM / 换行 / 空格）同样不得覆写 ────────────────────────
{
  const r = scenario("whitespace", "﻿  \r\n  ");
  check(
    "★ 纯空白不被覆写",
    r.after === "﻿  \r\n  ",
    "被改写成：" + JSON.stringify(r.after).slice(0, 60),
  );
}

// ── 4) 真损坏（半截 JSON）：该备份重建，且备份带时间戳 ────────────────
{
  const r = scenario("corrupt", '{"connections":[{"id":"abc"');
  const dir = r.file.replace(/connections\.json$/, "");
  const backups = existsSync(dir) ? readdirSync(dir).filter((f) => f.includes("corrupt")) : [];
  check(
    "损坏：备份文件已生成",
    backups.length === 1,
    "目录里没有 .corrupt-* 备份：" + JSON.stringify(backups),
  );
  check(
    "★ 备份带时间戳（反复损坏不覆盖唯一退路）",
    backups.length === 1 && /corrupt-\d{4}-\d{2}-\d{2}T.*\.bak$/.test(backups[0]),
    "备份名不是带时间戳的形式：" + (backups[0] || "(无)"),
  );
  check(
    "损坏：重建为空配置（应用不卡在坏状态）",
    /"connections"\s*:\s*\[\s*\]/.test(r.after),
    r.after.slice(0, 60),
  );
}

// ── 5) saveConnections 必须是原子写（不留 .tmp 残留） ────────────────
{
  const dir = mkdtempSync(join(tmpdir(), "conn-save-"));
  const file = join(dir, "connections.json");
  const ctx = makeSandbox(file);
  vm.runInContext("saveConnections(" + JSON.stringify({ connections: [{ id: "z" }], currentId: "z" }) + ")", ctx);
  const left = readdirSync(dir);
  check(
    "saveConnections 写出了目标文件",
    existsSync(file),
    "文件不存在",
  );
  check(
    "★ saveConnections 不留 .tmp 残留",
    !left.some((f) => f.includes(".tmp-")),
    "残留：" + JSON.stringify(left),
  );
  const back = JSON.parse(readFileSync(file, "utf8"));
  check("saveConnections 内容正确", back.connections[0].id === "z", JSON.stringify(back));
}

console.log("");
if (failures > 0) {
  console.log(`全部失败：${failures} 条`);
  process.exit(1);
}
console.log("全部通过");