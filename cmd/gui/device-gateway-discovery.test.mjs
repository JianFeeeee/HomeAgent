// 设备桥 gateway 自动发现链路的门禁。
//
// ## 要判的是什么
//
// 2026-10-05 实测故障：GUI 面板里能看到设备通道地址，但设备桥连不上。
// 日志只有一行 `ws cookie len=0` + 401，没有任何「地址来自发现」的痕迹。
//
// 根因不是配置错，是**发现结果与实际连接配置从不联通**：
//
//   · 渲染进程 app.js 的 loadDiscoveredGateway() 调 /device/gateway，
//     拿到的地址只写进 **state.discoveredGateway**（UI 展示用）；
//   · 主进程 startDeviceBridge() 读的是 **gui-prefs.deviceBridge.gateway**；
//   · 两边从未交换过 —— state 是渲染进程内存，prefs 是主进程文件。
//
// 于是服务端明明已经通过 /device/gateway 告诉了客户端正确地址
// （proxy.go:1078 生成的 url_portal），GUI 却仍在用用户几个月前
// 手填的旧地址，且**永远 401/ECONNREFUSED，且不重试**。
//
// ## 判据形态：文本扫描而非运行时
//
// 与 endpoint-align / device-align 同路子。自动回写的触发点在
// loadDiscoveredGateway() 内，由 refreshAll 的 15s 定时器驱动 ——
// 构造那个时序需要真Electron + 真后端，而 /device/gateway 还要求
// 会话 cookie（CI 里没有）。故从源码文本判「链路是否接通」。
//
// 运行：node device-gateway-discovery.test.mjs

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const APP = join(here, "renderer", "app.js");
const MAIN = join(here, "main.js");
const PRELOAD = join(here, "preload.js");

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) {
    console.log(`  ✓ ${name}`);
  } else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

const appSrc = readFileSync(APP, "utf8");
const mainSrc = readFileSync(MAIN, "utf8");
const preloadSrc = readFileSync(PRELOAD, "utf8");

// ── 1) 发现函数存在且会调服务端权威端点 ────────────────────────────

check(
  "存在 /device/gateway 发现调用",
  appSrc.includes("/device/gateway"),
  "找不到 —— 服务端发现端点没被调用",
);

// ── 2) 发现结果必须回写到主进程（本判据的核心）──────────────────────
//
// 这是本次修复的要点。若这段回写被删掉（重构时不慎），设备桥会退回
// 「只用用户手填地址」的行为，且没有任何报错——
// 这正是 2026-10-05 那次故障的形态。

check(
  "发现结果回写到主进程 deviceBridge.set",
  /discoveredGateway[\s\S]{0,600}?deviceBridge\s*\.\s*set\s*\(/.test(appSrc),
  "loadDiscoveredGateway 里没有 deviceBridge.set 回写 —— " +
    "state.discoveredGateway 只用于 UI，主进程仍在用旧的手填地址",
);

// ── 3) 回写不得覆盖用户手填值 ──────────────────────────────────────
//
// 发现只做**缺省补全**。若用户在设置页手填了内网直连地址（灰度实例、
// 绕过反代），自动回写把它冲掉就是新的回归。判据钉住「本地为空才写」。

// 注意不能用 /set\(\s*\{[\s\S]{0,300}?\}/ —— 调用点前后的 if 判断在花括号
// **外面**，而 300 字窗口会跨到下一个 deviceBridge.set（saveBridgeChannel
// 的手动保存），判据就误报了。改为直接取「发现函数体内」的那次调用。
const discoFn = appSrc.match(
  /async function loadDiscoveredGateway\(\)[\s\S]*?\n}/,
);
const setCall = discoFn
  ? discoFn[0].match(/window\.homeagent\.deviceBridge\.set\([^)]*\)/)
  : null;
check(
  "回写前检查本地 gateway 为空",
  discoFn ? /cur\s*&&\s*!cur\.gateway/.test(discoFn[0]) : false,
  discoFn
    ? "回写处没看到「本地已配置则跳过」的判断 —— 会覆盖用户手填值"
    : "找不到 loadDiscoveredGateway",
);
check(
  "回写只传 gateway，不覆盖 token",
  setCall ? !/token/.test(setCall[0]) : false,
  setCall ? "回写里带了 token —— 发现端点不返回设备令牌，会写坏" : "找不到回写调用",
);

// ── 4) IPC 链路完整：preload 暴露 get/set，主进程处理 ──────────────
//
// 回写用的是 window.homeagent.deviceBridge.set(...)；contextBridge
// 若没暴露或主进程没注册，这次调用是 undefined TypeError（被 catch
// 吞掉，于是又回到「静默不生效」）。

check(
  "preload 暴露 deviceBridge.set",
  /deviceBridge[\s\S]{0,120}?set\s*:/.test(preloadSrc),
  "contextBridge 未暴露 set —— 渲染进程调不到",
);
check(
  "主进程注册 device-bridge:set",
  mainSrc.includes('ipcMain.handle("device-bridge:set"'),
  "主进程没有该 handler",
);
check(
  "主进程 device-bridge:get 返回明文 token",
  /device-bridge:get[\s\S]{0,900}?token:\s*db\.token/.test(mainSrc),
  "get 未返回 token 明文 —— 渲染侧拿不到设备令牌",
);

// ── 5) device-bridge:set 必须真正重启桥 ───────────────────────────
//
// 只存 prefs 不重启，配置不会立即生效（要等下次进程启动）。
// 这是同一类「存了但没生效」的静默失效。

// 不用大正则匹配整个 handler —— 返回对象里的嵌套 } 会让边界判断很脆。
// 改为取两个 handler 之间的**源码切片**，再在里面判 stop/start 是否存在。
const setStart = mainSrc.indexOf('ipcMain.handle("device-bridge:set"');
const setEnd = mainSrc.indexOf('ipcMain.handle("prefs:get"');
const setHandler =
  setStart >= 0 && setEnd > setStart
    ? mainSrc.slice(setStart, setEnd)
    : null;
check(
  "device-bridge:set 会重启设备桥",
  setHandler
    ? setHandler.includes("stopDeviceBridge") &&
      setHandler.includes("startDeviceBridge")
    : false,
  setHandler
    ? "handler 里没有 stop+start —— 新配置要等重启进程才生效"
    : "找不到 device-bridge:set handler",
);

// ── 6) 主进程连接必须带 token ─────────────────────────────────────
//
// 设备桥 WS 走 ?token=（registry.go ServeWS 只认 query/X-API-Key/
// Sec-WebSocket-Protocol 三种，**cookie 不参与** acceptBind）。
// 这是实测踩过的坑：曾误以为发 cookie 就能过，实际服务端仍 401。

const connectFn = mainSrc.match(
  /function connectDeviceWS[\s\S]*?\n}\n/,
);
check(
  "connectDeviceWS 把 token 放进 query",
  connectFn ? /token=/.test(connectFn[0]) : false,
  connectFn
    ? "query 里没带 token —— 服务端 acceptBind 会拒"
    : "找不到 connectDeviceWS",
);

// ── 6) keepalive：设备桥必须有主动 ping ──────────────────────────────
//
// 2026-10-05 实测故障：GUI bind 成功、设备进了 devices 表（面板可见），
// 但几秒后能力调用全失败：
//     POST /api/v1/device/push {"device_id":"gui-JianF"}
//     → {"error":"device gui-JianF not online"}
// 同时 waiter/NAS 完全正常（last_seen 持续刷新）。
//
// 根因：GUI **从不发 ping**。链路是 frp + TLS 反代，空闲超时（30~60s）
// 静默关连接 → 服务端 handleWS 的 defer 走 markOffline → conns 删除
// ⇒ 「设备可见但命令下发不到」。
//
// 服务端 registry.go:735 那段 ping/pong 注释修的是「**回** pong 不能因
// 未bind 而失败」，不是「客户端有没有发」—— 那是另一个方向的坑。

const pingFn = mainSrc.match(/function sendDevicePing[\s\S]*?\n}\n/);
check(
  "存在 sendDevicePing",
  !!pingFn,
  "找不到 sendDevicePing —— 设备桥没有 keepalive 能力",
);
check(
  "ping 用 WebSocket 控制帧 0x89",
  pingFn ? /0x89/.test(pingFn[0]) : false,
  pingFn ? "ping 帧构造不对（应为 0x89 = FIN|ping 且置掩码位）" : "",
);

// ping 必须被定时器驱动，且间隔要压在常见空闲阈值之下。
const openBlock = mainSrc.slice(
  mainSrc.indexOf("opened = true"),
  mainSrc.indexOf("opened = true") + 2000,
);
check(
  "连接建立后启动 ping 定时器",
  /setInterval/.test(openBlock) && /sendDevicePing/.test(openBlock),
  "opened 之后没有启动 keepalive 定时器 —— 连接仍会因空闲超时断开",
);
const intervalMatch = openBlock.match(/setInterval\([^,]*,\s*(\d+)\)/);
check(
  "keepalive 间隔 ≤ 30s",
  intervalMatch ? parseInt(intervalMatch[1], 10) <= 30000 : false,
  intervalMatch
    ? `间隔 ${intervalMatch[1]}ms 太长，压不住常见 30s 空闲超时`
    : "找不到 setInterval 的间隔参数",
);

// 关闭路径必须清定时器，否则进程不退出 / 往死 socket 写。
check(
  "close 时清理 ping 定时器",
  /close:[\s\S]{0,200}?clearInterval/.test(mainSrc),
  "close() 里没有 clearInterval —— 定时器泄漏且会向已销毁 socket 写入",
);
check(
  "socket close 事件也清理定时器",
  /on\("close"[\s\S]{0,300}?clearInterval/.test(mainSrc),
  'sock.on("close") 里没有 clearInterval —— 异常断开后定时器泄漏',
);

// ── 汇总 ────────────────────────────────────────────────────────────

console.log("");
if (failures > 0) {
  console.log(`失败 ${failures} 条`);
  process.exit(1);
}
console.log("全部通过");