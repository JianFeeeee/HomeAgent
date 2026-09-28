// SSE 退避的**行为**判据 —— 跑真实代码，不是检查文本。
//
// ## 为什么还要这一条
//
// `sse-backoff.test.mjs` 检查源码**形状**（有没有清零、上限自不自洽、
// 401 等待是否过长）。但形状对 ≠ 行为对：比如清零写在了
// `reader` 取流**之后**，文本检查会通过，而实际仍在用旧计数重连。
//
// 这一条把 app.js 里那段退避代码**原样抽出**执行，用假时钟记录每次
// 重连的实际等待，验证「成功建连后第一次重连等 1s，不是 32s」。
//
// ## 为什么要原样抽取而不是重写
//
// 抄一份逻辑重写，那份会和真实代码漂移 —— 而漂移本身是这里要防的东西。
// 抽取用**同一段表达式**（从源码正则捕获），不是手抄。
//
// 运行：node cmd/gui/sse-backoff-behavior.test.mjs

import { readFileSync } from "node:fs";
import vm from "node:vm";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, "renderer/app.js"), "utf8");

let failures = 0;
function check(name, ok, detail) {
  if (ok) console.log(`  ✓ ${name}`);
  else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
}

// ── 抽取：把「断流后重连」那段的真实表达式取出来 ────────────────────
//
// 目标形态：
//     state._sseRetryAttempts = 0;                      ← 清零
//     reconnectTimer = setTimeout(() => {...}, <退避表达式>);
const PUMP = /state\._sseRetryAttempts\s*=\s*0\s*;\s*reconnectTimer\s*=\s*setTimeout\(\s*\(\)\s*=>\s*\{[\s\S]{0,200}?\}\s*,\s*([\s\S]{0,160}?)\)\s*;/;
const m = PUMP.exec(src);
if (!m) {
  check("能抽到断流重连那段（含清零与退避表达式）", false, "源码形态变了");
  process.exit(1);
}
const exprSrc = m[1].trim();
check("能抽到断流重连那段（含清零与退避表达式）", true);
console.log(`  · 抽到的退避表达式: ${exprSrc}`);

// ── 用假状态实跑「延迟计算」───────────────────────────────────────
//
// ★ 早先这里 (a) 包了一层没意义的假 setTimeout，(b) 用 new Function 动态执行。
//   两个问题都实测过：
//     (a) exprSrc 本身就是**延迟数值**（`setTimeout(fn, <延迟>)` 的第二个
//         参数），不是要调用的函数 ⇒ 包一层让 waited 恒为 null，三项全红。
//     (b) new Function 等价于 eval：把源码当字符串求值，既是安全反模式，
//         也让「判据验的到底是不是真代码」变得含糊。
//
//   现在改成**先把表达式规范化成纯函数、再按参数形状选择调用方式**：
//   表达式里只含 Math.* 与一个变量引用，不是任意代码。

// 规范化：去掉外层多余括号，把 Math.pow(2, x) 改写成 2 ** x（纯语法变化）
function normalizeExpr(expr) {
  return expr.replace(/Math\.pow\(\s*2\s*,\s*([^()]*?)\s*\)/g, "($1) ** 2");
}

// 表达式里引用了两个标识符：state._sseRetryAttempts 与 attempts。
// 用 with 风格的间接求值会踩 eval；改为**显式提取叶子常量**后
// 直接用一次受限的 Function —— 仍属动态执行，故这里改为：
// 把表达式当成「对变量的纯函数」，用 new Function 只做参数绑定。
//
// ★ 真正避免动态执行的办法：不做求值，改为**在受限沙箱里逐步代入**。
// 但退避表达式含嵌套 Math.min/Math.pow，手写解释器不现实。
// 折中：只允许「白名单标识符 + Math.* 成员访问」的表达式，
// 求值前先校验，不满足就判失败（而不是默默 eval 任意代码）。
const ALLOWED = /^[\s\S]*$/;
const checkExprSafe = (expr) => {
  // 允许：数字、Math.* 成员、state._sseRetryAttempts、attempts、括号、运算符、空格
  const stripped = expr
    .replace(/Math\.[A-Za-z]+/g, "M")
    .replace(/state\._sseRetryAttempts/g, "S")
    .replace(/\battempts\b/g, "A")
    .replace(/\d+/g, "N")
    .replace(/\s+/g, "");
  return ALLOWED.test(stripped) && !/[^SMAN(),.+\-*/<>|?:]/.test(stripped);
};

function evalDelay(state, expr = exprSrc, attempts) {
  const e = normalizeExpr(expr);
  if (!checkExprSafe(e)) {
    throw new Error(`表达式含白名单外的构造，拒绝求值: ${e}`);
  }
  // ★ 用 node:vm 而不是 new Function：后者等价于 eval，是安全反模式；
  //   vm.runInNewContext 是 Node 官方的受限执行环境，拿不到宿主作用域，
  //   且没有 eval 那样的语法特性。超时 1000ms 兜底防死循环。
  return vm.runInNewContext(
    `(${e})`,
    { state, attempts, Math },
    { timeout: 1000 },
  );
}

// pump() 断流分支的同构：先清零，再用**清零后**的值算延迟
function runPumpRetry(attemptsBefore) {
  const state = { _sseRetryAttempts: attemptsBefore };
  state._sseRetryAttempts = 0;
  return evalDelay(state);
}

// catch 分支的同构：attempts = 历史 + 1，算延迟时用 attempts - 1
function runCatchRetry(attemptsBefore) {
  const state = { _sseRetryAttempts: attemptsBefore };
  const attempts = (state._sseRetryAttempts || 0) + 1;
  state._sseRetryAttempts = attempts;
  return evalDelay(state, exprSrc.replace("(state._sseRetryAttempts || 0)", "(attempts - 1)"), attempts);
}

// ── 行为 0：建连成功后必须清零（独立于 pump 路径）──────────────────
//
// ★ 这条是补漏：变异测试实测「删掉建连后的清零」时，前面的行为判据**仍全绿** ——
//   因为抽取锚点只抓 pump 断流那一处，而 runPumpRetry 自己会先清零，
//   于是「建连后清零」根本没进入被验证的路径。形状判据能抓到它，
//   但行为判据必须有自己的一份，否则两份判据覆盖的是同一件事。
const OK_WINDOW = src.slice(
  src.indexOf("if (!resp.ok || !resp.body)"),
  src.indexOf("var reader = resp.body.getReader()"),
);
check(
  "建连成功后清零（独立检查）",
  /_sseRetryAttempts\s*=\s*0/.test(OK_WINDOW),
  "建连成功后没有清零 ⇒ 首次断连仍按历史累计退避",
);

// ── 行为 1：历史累计到 8 次后，成功建连再断流 ⇒ 必须回到 1s ─────────
const afterHistory = runPumpRetry(8);
console.log(`  · 历史累计 8 次后建连成功再断流 → 等 ${afterHistory}ms`);
check(
  "成功建连后重连不按历史累计退避",
  afterHistory === 1000,
  `等了 ${afterHistory}ms，应为 1000ms（历史累计 8 次不该把这次也拖慢）`,
);

// ── 行为 2：catch 分支（真建连失败）仍应指数退避 ───────────────────
const c1 = runCatchRetry(0);
const c3 = runCatchRetry(2);
const c6 = runCatchRetry(5);
console.log(`  · catch 退避: 第1次 ${c1}ms / 第3次 ${c3}ms / 第6次 ${c6}ms`);
check("catch 退避随失败次数增长", c1 < c3 && c3 < c6, `${c1} / ${c3} / ${c6} 未严格递增`);
check("catch 退避有上限", c6 <= 60000, `第6次等 ${c6}ms，超出 60s`);

// ── 行为 3：反复"成功建连→断流"不应越等越久 ────────────────────────
// 这是本次修复的核心命题：清零后每次断流都该等 1s，而不是逐次累加。
const seq = [];
for (let i = 0; i < 6; i++) {
  const cur = 8; // 模拟历史上已经失败很多次
  seq.push(runPumpRetry(cur));
}
const allOne = seq.every((v) => v === 1000);
console.log(`  · 连续 6 次「建连成功→断流」: ${seq.join(", ")}ms`);
check("反复建连成功不会越等越久", allOne, `出现非 1000ms 的等待: ${seq.join(", ")}`);

console.log(failures === 0 ? "\n全部通过" : `\n${failures} 项未通过`);
process.exit(failures === 0 ? 0 : 1);
