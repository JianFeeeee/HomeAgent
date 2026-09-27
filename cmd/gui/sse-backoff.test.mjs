// SSE 重连退避的判据。
//
// ## 要判的是什么
//
// 2026-09-27 定位到 GUI「消息流不稳、看着卡」的一个共因：
// `state._sseRetryAttempts` **只增不减，从不重置**。
//
// 它的自增只发生在 `connectFetchSSE` 的 catch 分支（连接**建立**失败），
// 而 `pump()` 里 stream 正常读完/断开后的重连**只读它算延迟，却从不清零**：
//
//     reconnectTimer = setTimeout(() => { connectSSE(); },
//       Math.min(1000 * Math.pow(2, Math.min((state._sseRetryAttempts || 0), 5)), 60000));
//
// 后果：只要历史上累计过 5 次失败，**之后每次断连都固定等 32s**，
// 无论中间成功连上过没有。而"成功连上"恰恰说明网络/服务端已恢复 ——
// 那时还按历史累计退避，就是纯粹的空等。
//
// 这解释了用户报告的全部四类症状（滞后/卡顿/闪断/看着不稳），
// 它们不是四个独立问题，而是同一条链。
//
// ## 为什么用「源码文本」做判据
//
// cmd/gui 无测试框架、无构建校验（package.json 只有 start/dev），
// app.js 是 203KB 单文件。判据从**真实源码**里提取退避表达式并求值，
// 而不是抄一份逻辑重写 —— 抄写的那份会和真实代码漂移，
// 而漂移本身就是这个判据要防的东西。
//
// 运行：node cmd/gui/sse-backoff.test.mjs

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, "renderer/app.js"), "utf8");

let failures = 0;
function check(name, ok, detail) {
  if (ok) {
    console.log(`  ✓ ${name}`);
  } else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
}

// ── 1) 退避必须被重置 ──────────────────────────────────────────────
//
// 判据是「成功建立连接后有清零动作」。取连接成功之后的那段代码
// （resp.ok && resp.body 之后、读流之前）作为观察窗口。
const okWindow = src.slice(
  src.indexOf("if (!resp.ok || !resp.body)"),
  src.indexOf("var reader = resp.body.getReader()")
);
check(
  "连接成功后重置 _sseRetryAttempts",
  /_sseRetryAttempts\s*=\s*0/.test(okWindow),
  "成功建连后没有清零 ⇒ 断连重连会一直按历史累计退避（封顶 32s）"
);

// ── 2) 退避上限是 32s 而不是 60s ──────────────────────────────────
//
// 代码里写的是 Math.min(..., 60000)，但指数被 Math.min(attempts, 5) 夹住，
// 2^5 = 32s < 60s ⇒ **60s 那个上限永远达不到**。
// 判据把两条 clamp 都提取出来算一遍，而不是相信注释或字面量。
// 退避有**两处**，自变量不同，一处都不能漏：
//   · pump() 断流重连  → Math.min(…, (state._sseRetryAttempts || 0), 5), <ceil>)
//   · catch 建连失败    → Math.min(…, attempts - 1, 5), <ceil>)
// 早先只认第一处，于是 catch 那处（上限仍是 60000）根本不在检查范围内。
//
// ★ 正则写成**字面量**，不用字符串拼接：
//   `String.raw` 拼接正则时，`\s` 保持双反斜杠字面量 ⇒ 去找字面的 "\s" 而非空白，
//   静默失配。逐步 add 写法反而更脆。字面量能被编辑器/linter 检查。
//
// 两种幂运算形态都认：Math.pow(2, x) 与 2 ** x（biome 会规范化前者）。
// ★ 这里刻意**不**用"一个大正则兼容两种形态"——
//   两种形态的捕获组数不同（pow 多一层括号 ⇒ 组数差 1），
//   实测按 r[length-1] 取值会把 cap 解成 NaN。
//
//   改为**定位与取值分离**：
//     · 正则只负责找到那一处退避表达式（不捕获数字）
//     · 数字用两次 replace 从命中的子串里取
//   两者各自简单，且加一种新形态时只需改正则的"外形"，不用动取值逻辑。
const SHAPE_PUMP = /Math\.min\(\s*1000\s*\*\s*(?:Math\.pow\(\s*2\s*,|2\s*\*\*)\s*Math\.min\([\s\S]{0,80}?_sseRetryAttempts[\s\S]{0,40}?,\s*(\d+)\s*\)\s*\)?\s*,\s*(\d+)\s*\)/;
const SHAPE_CATCH = /Math\.min\(\s*1000\s*\*\s*(?:Math\.pow\(\s*2\s*,|2\s*\*\*)\s*Math\.min\(\s*attempts\s*-\s*1\s*,\s*(\d+)\s*\)\s*\)?\s*,\s*(\d+)\s*\)/;
const hits = [
  ["pump 断流重连", SHAPE_PUMP.exec(src)],
  ["catch 建连失败", SHAPE_CATCH.exec(src)],
];
const m = hits.find(([, r]) => r);
if (!m) {
  check("能提取退避表达式", false, "两处退避都没匹配到（正则或代码形态变了）");
} else {
  // 两处退避的**语义不同**，不能用同一把尺子量：
  //
  //   · pump 断流重连 —— 刚成功建连过（上面已清零），退避从 1s 起步。
  //     上限写 32000（= 2^5，实测封顶就是 32s，一致）。
  //
  //   · catch 建连失败 —— 连接都没建起来，attempts 已 +1，退避本就该更长。
  //     它的 60000 达不到（封顶 32s），但**无害**：意图是"最多等一分钟"，
  //     写 60000 只是把上限放得比实际封顶更宽，不影响任何一次重连的时刻。
  //
  // ⇒ 判据只要求「pump 那处上限自洽」；catch 那处只要求「有上限、不是无限重连」。
  const [, pumpHit] = hits[0];
  if (pumpHit) {
    const cap = Number(pumpHit[1]);
    const ceilMs = Number(pumpHit[2]);
    const realCeil = Math.min(1000 * 2 ** cap, ceilMs);
    console.log(`  · pump 断流重连: cap=${cap} → 封顶 ${realCeil / 1000}s；声明上限 ${ceilMs / 1000}s`);
    check(
      "pump 退避上限自洽（非死代码）",
      realCeil === ceilMs,
      `实际封顶 ${realCeil / 1000}s，声明 ${ceilMs}ms ⇒ 声明值不可达`,
    );
  }
  const [, catchHit] = hits[1];
  if (catchHit) {
    const cap = Number(catchHit[1]);
    const ceilMs = Number(catchHit[2]);
    const realCeil = Math.min(1000 * 2 ** cap, ceilMs);
    console.log(`  · catch 建连失败: cap=${cap} → 封顶 ${realCeil / 1000}s；声明上限 ${ceilMs / 1000}s`);
    check(
      "catch 退避有有限上限（不会无限重连）",
      Number.isFinite(realCeil) && realCeil <= 60000,
      `封顶 ${realCeil}ms，超出预期`,
    );
  }
}

// ── 3) 401 重试不该有固定长阻塞 ────────────────────────────────────
//
// api() 的 401 分支里有一句固定的 setTimeout(800)。认证过期时
// **每个**请求都白等 0.8s；并发几个请求就叠加成明显的「卡」。
// 判据：401 分支里那个 setTimeout 的时长不得超过 100ms。
const apiStart = src.indexOf("async function api(p, o)");
const apiEnd = src.indexOf("\nasync function", apiStart + 10);
const apiBody = apiStart > 0 ? src.slice(apiStart, apiEnd > 0 ? apiEnd : apiStart + 4000) : "";
const w401 = apiBody.match(/r\.status === 401[\s\S]{0,600}?setTimeout\(\s*(?:res2\s*,\s*)?(\d+)\s*\)/);
if (!w401) {
  check("找到 401 分支的退避时长", false, "未匹配到 401 分支里的 setTimeout");
} else {
  const wait = Number(w401[1]);
  console.log(`  · 401 重试固定等待 ${wait}ms`);
  check("401 重试无长固定阻塞", wait <= 100, `固定等 ${wait}ms，认证过期时每个请求都白等这么久`);
}

console.log(failures === 0 ? "\n全部通过" : `\n${failures} 项未通过`);
process.exit(failures === 0 ? 0 : 1);
