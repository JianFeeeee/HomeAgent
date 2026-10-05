// 设备桥**心跳 + 断线重连**的行为判据（2026-10-05）。
//
// ## 要判的是什么
//
// 修之前 GUI 侧的设备桥有两个洞，合起来的效果是「一次失联，终身失联」：
//
//   1. **不发 ping**。waiter 侧有 pingLoop（internal/devicebridge/client/bridge.go:636，
//      每 30s 一个 0x9 帧），GUI 侧全文件一次 ping 都没发过。
//      而 remotedevice 服务端**没有设备侧读超时**（registry.go 里没有
//      SetReadDeadline）—— 不发心跳就没人能发现设备已死，网关会把一台
//      半死的设备长期显示为 online。
//
//   2. **不重连**。startDeviceBridge 只有两个调用点（启动 / 配置变更），
//      失败就 console.error 结束。GUI 是最容易被休眠/切网/服务端重启打断的
//      形态，合盖一次或 WiFi 抖一下，设备桥就再也回不来，
//      直到用户重启 GUI 或手动改一次配置。
//
// ## 为什么用「真实执行」而不是检查文本
//
// 判据在 node:vm 沙箱里把 device-bridge 那几个函数**抽出来真的跑**：
// 用假 socket 驱动「连接成功 → 断线 → 重连 → 再断线」，
// 数真实的重连次数与心跳帧数，而不是 grep `ping` 出现过几次。
//
// ★ 特别要判「连都没连上」那条路：connectDeviceWS 失败时走 reject，
//   当时 socket 从未 upgraded。如果重连只挂在 socket 的 close 事件上，
//   这条路**根本不会有第二次排程** —— 这正是容易写错的地方。
//
// 运行：node cmd/gui/device-bridge-liveness.test.mjs

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import vm from "node:vm";
import { setTimeout as realSleep } from "node:timers/promises";

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

// 从 main.js 里按名字抽出函数源码。
function extractFn(name) {
  // 先试 async function（connectDeviceBridgeOnce 是 async），
  // 再试普通 function。顺序不能反：普通形式的 needle
  // 「function connectDeviceBridgeOnce(」会命中 async 版本里的
  // 「async function connectDeviceBridgeOnce(」，切出源码时丢前缀。
  let start = src.indexOf(`async function ${name}(`);
  let isAsync = true;
  if (start === -1) {
    start = src.indexOf(`function ${name}(`);
    isAsync = false;
  }
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
  throw new Error(`unbalanced braces for ${name} (async=${isAsync})`);
}

const devicePingIntervalMs = Number(
  src.match(/const devicePingIntervalMs\s*=\s*(\d+)/)?.[1] ?? 0,
);
const deviceReconnectBaseMs = Number(
  src.match(/const deviceReconnectBaseMs\s*=\s*(\d+)/)?.[1] ?? 0,
);
const deviceReconnectMaxMs = Number(
  src.match(/const deviceReconnectMaxMs\s*=\s*(\d+)/)?.[1] ?? 0,
);

// ---- 假 socket：记录写出的控制帧，模拟 close ----
function makeFakeSock() {
  const written = [];
  return {
    written,
    destroyed: false,
    write(buf) {
      written.push(Buffer.from(buf));
      return true;
    },
    destroy() {
      this.destroyed = true;
    },
  };
}

function runScenario({ connectFailsFirst = 0, label }) {
  // 状态
  let deviceBridge = null;
  let devicePingTimer = null;
  let deviceReconnectTimer = null;
  let deviceReconnectAttempt = 0;
  let deviceWanted = false;
  const connectCalls = [];
  const logLines = [];
  let connectAttempt = 0;

  // 用可控的 timer：立即执行「到期」，但记录延迟值。
  const timers = [];
  function fakeSetTimeout(fn, ms) {
    timers.push({ fn, ms, kind: "timeout" });
    return timers.length;
  }
  function fakeClearTimeout(id) {
    if (id && timers[id - 1]) timers[id - 1].cancelled = true;
  }
  function fakeSetInterval(fn, ms) {
    timers.push({ fn, ms, kind: "interval", cancelled: false });
    return timers.length;
  }
  function fakeClearInterval(id) {
    if (id && timers[id - 1]) timers[id - 1].cancelled = true;
  }
  // 推进 n 个 timer（全部执行）。
  // ★ 必须 await：重连链是 async（connectDeviceBridgeOnce 里 await
  //   doConnectDeviceBridge），若不等它跑完，下一次 runTimers 会看到一个
  //   「已 ran」的 timer 列表而找不到新排程的那个，测出来就是「没重连」。
  async function runTimers(n = 1) {
    for (let k = 0; k < n; k++) {
      const t = timers.find((x) => !x.cancelled && !x.ran);
      if (!t) return false;
      t.ran = true;
      await t.fn();
      // 让被测代码里 await 之后的 continuation（排程下一轮）跑完
      await realSleep(0);
    }
    return true;
  }
  function liveIntervalMs() {
    const t = timers.find((x) => x.kind === "interval" && !x.cancelled);
    return t ? t.ms : 0;
  }

  const sandbox = {
    console: { log: (...a) => logLines.push(a.join(" ")), error: (...a) => logLines.push("ERR " + a.join(" ")) },
    crypto: { randomBytes: (n) => Buffer.alloc(n, 7) },
    loadGuiPrefs: () => ({
      deviceBridge: { enabled: true, gateway: "ws://x/ws", token: "t" },
    }),
    devicePingIntervalMs,
    deviceReconnectBaseMs,
    deviceReconnectMaxMs,
    Math,
    Buffer,
    setTimeout: fakeSetTimeout,
    clearTimeout: fakeClearTimeout,
    setInterval: fakeSetInterval,
    clearInterval: fakeClearInterval,
  };

  // 需要沙箱内能读写这几个状态：用闭包变量暴露 getter/setter
  const ctx = vm.createContext(sandbox);
  // 把状态变量以「属性」形式暴露给被测代码：改写为 sandbox 上的普通变量不可行
  // （被测代码是顶层 let），所以改为在沙箱里重新声明同名 let 并暴露 setter。
  const prelude = `
    let deviceBridge = null;
    let devicePingTimer = null;
    let deviceReconnectTimer = null;
    let deviceReconnectAttempt = 0;
    let deviceWanted = false;
    let deviceBridgeBound = false;
    let deviceBridgeBindError = "";
    let deviceBridgeAddr = "";
    let deviceBridgeId = "";
    globalThis.__socks = [];
    var __connectCalls = [];
    globalThis.__connectCalls = __connectCalls;
    var __shouldFail = __failTimes;
    globalThis.__socksHolder = true;
    globalThis.__peek = () => ({ deviceBridge, devicePingTimer, deviceReconnectTimer,
      deviceReconnectAttempt, deviceWanted });
  `;
  sandbox.__failTimes = connectFailsFirst;
  // connectDeviceWS 的替身：前 N 次 reject，之后给一个假 sock
  const connectStub = `
    async function connectDeviceWS(url, token, onMsg) {
      __connectCalls.push(url);
      if (__shouldFail > 0) { __shouldFail--; throw new Error("simulated connect failure"); }
      const sock = { __written: [], destroyed: false,
        write(b) { this.__written.push(Buffer.from(b)); return true; },
        destroy() { this.destroyed = true; } };
      __socks.push(sock);
      return { send(){}, close(){ sock.destroy(); }, __sock: sock };
    }
    function startDevicePing(sock) {
      if (devicePingTimer) clearInterval(devicePingTimer);
      devicePingTimer = setInterval(() => {
        try { if (!sock || sock.destroyed) return; sendDeviceControlFrame(sock, 0x9, null); } catch(e){}
      }, devicePingIntervalMs);
    }
    function stopDevicePing() { if (devicePingTimer) { clearInterval(devicePingTimer); devicePingTimer = null; } }
  `;

  const fns = [
    "sendDeviceControlFrame",
    "scheduleDeviceReconnect",
    "stopDevicePing",
    "startDevicePing",
    "connectDeviceBridgeOnce",
  ]
    .map((n) => extractFn(n))
    .join("\n\n");

  // ★ 用 main.js 里**真实的** startDeviceBridge，不在沙箱里重写一份。
  //   之前我在这里自己写了一个同名的 startDeviceBridge（包 try/catch + 排程），
  //   结果：把 main.js 里的排程调用删掉，判据依旧全绿 ——
  //   **测的不是被测代码，而是我的副本**。这正是本仓反复强调的
  //   「判据必须测真实行为，不能测一份影子实现」。
  const realStart = extractFn("startDeviceBridge");
  const realStop = extractFn("stopDeviceBridge");

  const script = [
    prelude,
    connectStub,
    "async function doConnectDeviceBridge(cfg) {",
    "  if (!cfg) return;",
    "  const ws = await connectDeviceWS(cfg.url || cfg.gateway || '', cfg.apiKey || cfg.token || '', function(){});",
    "  deviceBridge = ws;",
    "  startDevicePing(ws.__sock);",
    "}",
    fns,
    realStart,
    realStop,
    "globalThis.__start = startDeviceBridge;",
    "globalThis.__stop = stopDeviceBridge;",
    "globalThis.__fireClose = () => { scheduleDeviceReconnect(); };",
  ].join("\n");

  vm.runInContext(script, ctx);

  return {
    timers,
    logLines,
    // connectCalls 必须从沙箱里读：connectDeviceWS 的替身在 vm 上下文里
    // push 的是 __connectCalls，外层那个 const connectCalls 永远是空的。
    // 之前把两者当成同一个，导致「重连真的再连了一次」判据永远红。
    get connectCalls() {
      return sandbox.__connectCalls || [];
    },
    runTimers,
    liveIntervalMs,
    peek: () => sandbox.__peek(),
    socks: () => sandbox.__socks || [],
    start: () => sandbox.__start({ url: "ws://x/ws", apiKey: "t" }),
    stop: () => sandbox.__stop(),
    fireClose: () => sandbox.__fireClose(),
  };
}

// ============ 判据 ============

console.log("设备桥心跳 + 重连");

// 1) 首次连接成功 → 起了心跳，且间隔是 30s（与服务端 pingLoop 一致）
{
  const sc = runScenario({ connectFailsFirst: 0, label: "ok-first" });
  await sc.start();
  const st = sc.peek();
  check("首次连接成功即启动心跳", !!st.devicePingTimer && sc.liveIntervalMs() === 30000,
    `pingTimer=${!!st.devicePingTimer} interval=${sc.liveIntervalMs()}`);
}

// 2) 心跳真的发出 0x9 控制帧，且帧格式合规（客户端帧必须带掩码位）
{
  const sc = runScenario({ connectFailsFirst: 0 });
  await sc.start();
  const sock = sc.socks()[0];
  // 触发一次心跳 interval
  const it = sc.timers.find((t) => t.kind === "interval" && !t.ran);
  if (it) it.fn();
  const ping = (sock.__written || []).find((b) => (b[0] & 0x0f) === 0x9);
  check("心跳发出 opcode 0x9", !!ping, `frames=${(sock.__written || []).length}`);
  check("客户端帧带掩码位（RFC6455 §5.3）", ping ? (ping[1] & 0x80) === 0x80 : false,
    ping ? `byte1=0x${ping[1].toString(16)}` : "no ping frame");
}

// 3) 断线 → 自动重连（线性退避，首个等待 = base）
{
  const sc = runScenario({ connectFailsFirst: 0 });
  await sc.start();
  const before = sc.peek().deviceReconnectAttempt;
  sc.fireClose();
  const pend = sc.timers.filter((t) => t.kind === "timeout" && !t.ran && !t.cancelled);
  check("断线后排出重连定时器", pend.length > 0, `pending=${pend.length}`);
  check("退避计数递增", sc.peek().deviceReconnectAttempt === before + 1,
    `attempt ${before} -> ${sc.peek().deviceReconnectAttempt}`);
  // 执行重连 → 应真的再连一次
  await sc.runTimers(1);
  check("重连真的再发起了一次连接", sc.connectCalls.length >= 1,
    `connectCalls=${sc.connectCalls.length}`);
}

// 4) ★ 连都没连上（connectDeviceWS reject）也必须排程重连
//    —— 这是最容易漏的一格：reject 路上没有 socket close 事件。
{
  const sc = runScenario({ connectFailsFirst: 1 });
  await sc.start();
  const pend = sc.timers.filter((t) => t.kind === "timeout" && !t.ran && !t.cancelled);
  check("首次连接失败也排出重连（reject 路径无 close 事件）", pend.length > 0,
    `pending=${pend.length}`);
  // 再跑 4 个 timer，确认能一路重连上去（第 2 次会成功）
  for (let i = 0; i < 4; i++) await sc.runTimers(1);
  check("重连最终成功并连上", sc.peek().deviceBridge !== null,
    `deviceBridge=${!!sc.peek().deviceBridge}`);
  check("连上后心跳重新出现", !!sc.peek().devicePingTimer);
}

// 5) 退避随失败次数线性增长并封顶
{
  const sc = runScenario({ connectFailsFirst: 99 }); // 永远失败
  await sc.start();
  const delays = [];
  for (let i = 0; i < 6; i++) {
    const pend = sc.timers.filter((t) => t.kind === "timeout" && !t.ran && !t.cancelled);
    if (!pend.length) break;
    delays.push(pend[0].ms);
    await sc.runTimers(1);
  }
  const cap = 60000;
  check("退避线性增长", delays.length >= 2 && delays[1] > delays[0],
    `delays=${JSON.stringify(delays)}`);
  check("退避封顶在 max", delays.every((d) => d <= cap), `delays=${JSON.stringify(delays)}`);
}

// 6) 主动停用后不再重连（deviceWanted=false 是闸）
{
  const sc = runScenario({ connectFailsFirst: 0 });
  await sc.start();
  sc.stop();
  const pend = sc.timers.filter((t) => t.kind === "timeout" && !t.ran && !t.cancelled);
  check("stopDeviceBridge 后不再排程重连", pend.length === 0, `pending=${pend.length}`);
  check("stopDeviceBridge 停掉心跳", sc.peek().devicePingTimer === null);
}

// 7) 成功重连后 attempt 归零
{
  const sc = runScenario({ connectFailsFirst: 2 });
  await sc.start();
  for (let i = 0; i < 6; i++) await sc.runTimers(1);
  check("成功重连后退避归零", sc.peek().deviceReconnectAttempt === 0,
    `attempt=${sc.peek().deviceReconnectAttempt}`);
}

console.log(
  failures === 0
    ? "\nAll device-bridge liveness checks passed."
    : `\n${failures} check(s) failed.`,
);
process.exit(failures === 0 ? 0 : 1);