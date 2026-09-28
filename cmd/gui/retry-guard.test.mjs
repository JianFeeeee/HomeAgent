// 401 恢复路径的**行为**判据 —— 抓「无限重试」这个真缺陷。
//
// ## 要判的是什么
//
// 2026-09-28 真机实测（Xvfb + Electron + CDP）时发现：
//
//     api() 的 401 分支：每次重试前都把 _haReloginLock 设回 false
//     ⇒ 那把锁**永远拦不住自我递归**
//
//     第1次: lock=false → 重登 → lock=false → return api()   ← 递归
//     第2次: lock=false → 重登 → lock=false → return api()   ← 又递归
//     …
//
// 锁的语义本该是「已经重登过一次，别再登」。但它在递归**之前**就被清掉了，
// 于是每层递归看到的都是 false。
//
// 后果与我的另一处改动直接相关：401 分支的固定等待从 800ms 降到 30ms
// （commit 1ef1998，那是「认证过期时每个请求白等 0.8s」的修复）。
// 但重试**没有次数上限** ⇒ 改前是「每 800ms 慢速空转」，
// 改后变成「每 30ms 快速烧 CPU + 反复打服务端」。**我的优化放大成了 bug。**
//
// 真机上表现为：探针调用 api() 后永不返回（我实测时探针直接挂死，
// 得靠封掉第二次 fetch 才能测出耗时）。
//
// ## 为什么用「真实执行」而不是检查文本
//
// 判据在 node:vm 沙箱里跑**从源码抽出的** 401 分支代码，
// 验证真实调用次数，而不是 grep `_haReloginLock` 出现过几次。
//
// 运行：node cmd/gui/retry-guard.test.mjs

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import vm from "node:vm";

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, "renderer/app.js"), "utf8");

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) console.log(`  ✓ ${name}`);
  else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

// ── 抽取 401 分支的真实代码 ─────────────────────────────────────────
// 取 `if (r.status === 401) { ... }` 那一整块（含嵌套的 return api()）
const start401 = src.indexOf("if (r.status === 401) {");
if (start401 < 0) {
  console.log("  ✗ 源码里找不到 401 分支");
  process.exit(1);
}
// 用括号配平取整块
let depth = 0, end401 = -1;
for (let i = src.indexOf("{", start401); i < src.length; i++) {
  if (src[i] === "{") depth++;
  else if (src[i] === "}") {
    depth--;
    if (depth === 0) {
      end401 = i + 1;
      break;
    }
  }
}
const block401 = src.slice(start401, end401);
check("能抽出 401 分支整块", true);
console.log(`  · 抽取长度 ${block401.length} 字符`);

// ── 在沙箱里真跑 ──────────────────────────────────────────────────
//
// ★ 修一次假绿灯：先前我把 401 分支整块当成**独立函数体**执行，
//   但那块以 `return api(p, o)` 结尾、外面没有调用它的上下文
//   ⇒ 我从未真正「进入」那个 if ⇒ api 递归=0 ⇒ 判据一路绿灯，
//   压根没测到无限重试。
//
// 正确做法：造一个**会返回 401 的 fetch**，让真实的 api() 走完整路径。
// 沙箱提供：state / window / fetch(恒 401) / syncConnAuth / setTimeout /
// ApiError / __ / performance。
let reloginCalls = 0;
let fetchCalls = 0;
const waitLog = [];
const MAX_FETCH = 12; // 超过就判定为「无上限」

const sandbox = {
  state: { currentConn: { apiKey: "wrong" } },
  window: { _haReloginLock: false },
  __: (zh) => zh,
  ApiError: class ApiError extends Error {
    constructor(msg, status) {
      super(msg);
      this.status = status;
    }
  },
  // ★ setTimeout 必须**执行回调**：api() 用它做超时控制
  //   （setTimeout(() => ctl.abort(), to)）。先前只记录不执行 ⇒
  //   AbortController 永远不被 abort、异常清理路径走不到，
  //   表现为「fetch 只调 1 次、8000ms 被当成 401 等待」。
  //   ⇒ 判据测的根本不是 401 路径。
  setTimeout: (fn, ms) => {
    // 只把**小延迟**记进 waitLog：8000ms 那个是 api() 的整体超时控制，
    // 与 401 恢复无关，混进来会让判据误判。
    if (ms <= 100) waitLog.push(ms);
    if (typeof fn === "function") {
      // 不真等 8 秒：微任务里立刻执行，等价于「超时立刻触发」
      queueMicrotask(() => {
        try {
          fn();
        } catch {}
      });
    }
    return Promise.resolve();
  },
  syncConnAuth: () => {
    reloginCalls++;
    return Promise.resolve();
  },
  performance: { now: () => Date.now() },
  // ★ clearTimeout 也必须提供：api() 在 fetch 之后调它。
  //   缺了会抛 "clearTimeout is not defined" ⇒ 整段在 fetch 之后就断了
  //   ⇒ 永远走不到 401 分支 ⇒ 判据静默地什么都没测。
  //   这就是「沙箱不完整 ⇒ 假绿灯」的又一处。
  clearTimeout: () => {},
  setInterval: () => 0,
  clearInterval: () => {},
  AbortController: class {
    constructor() {
      this.signal = {};
    }
    abort() {}
  },
  // 恒回 401 的 fetch —— 真实场景：重登后凭据仍是错的
  fetch: () => {
    fetchCalls++;
    if (fetchCalls > MAX_FETCH) {
      return Promise.reject(new Error(`NO-RETRY-LIMIT: fetch 被调 ${fetchCalls} 次`));
    }
    return Promise.resolve({
      ok: false,
      status: 401,
      statusText: "Unauthorized",
      headers: { get: () => null },
      text: () => Promise.resolve(""),
    });
  },
};
sandbox.globalThis = sandbox;

// 抽出**真实的 api 函数**（不是 401 分支），让它自己走到那个 if
const apiStart = src.indexOf("async function api(p, o) {");
let ad = 0, ae = -1;
for (let i = src.indexOf("{", apiStart); i < src.length; i++) {
  if (src[i] === "{") ad++;
  else if (src[i] === "}") {
    ad--;
    if (ad === 0) {
      ae = i + 1;
      break;
    }
  }
}
const apiSrc = src.slice(apiStart, ae);
check("能抽出真实 api() 函数", apiSrc.includes("r.status === 401"), "抽到的函数里没有 401 分支");

await vm
  .runInNewContext(`(async () => { ${apiSrc}; return await api("/status", {}); })()`, sandbox, { timeout: 3000 })
  .catch((e) => {
    if (String(e.message).startsWith("NO-RETRY-LIMIT")) fetchCalls = MAX_FETCH + 1;
  });

// ── 判据 ──────────────────────────────────────────────────────────

// ① 无限重试：重试必须有次数上限
console.log(`  · fetch 被调 ${fetchCalls} 次，重登 ${reloginCalls} 次`);
check(
  "401 重试有次数上限（不会无限递归）",
  fetchCalls <= 2,
  `fetch 被调 ${fetchCalls} 次 ⇒ 每轮 401 都重新登录并重试，没有上限。` +
    `改前是 800ms/轮慢速空转，改后（30ms）变成快速烧 CPU + 反复打服务端。`,
);

// ② 每次重试的固定等待不能太长（那是 1ef1998 的修复，别回退）
const waits = waitLog.filter((n) => typeof n === "number");
if (waits.length === 0) {
  check("能观察到 401 分支的固定等待", false, "没抓到 setTimeout —— 分支形态可能变了");
} else {
  const maxWait = Math.max(...waits);
  console.log(`  · 观察到的固定等待: ${waits.join(", ")}ms`);
  check("401 恢复无长固定阻塞", maxWait <= 100, `最大等 ${maxWait}ms，超过 100ms`);
}

console.log(failures === 0 ? "\n全部通过" : `\n${failures} 项未通过`);
process.exit(failures === 0 ? 0 : 1);
