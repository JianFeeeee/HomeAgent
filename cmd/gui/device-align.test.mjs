// DeviceBridge 能力对齐门禁：GUI ↔ remotedevice 插件 ↔ devicebridge 客户端库。
//
// ## 为什么需要这个门禁（endpoint-align.test.mjs 覆盖不到的那一半）
//
// endpoint-align.test.mjs 守的是 **REST 面**：GUI 的 api("...") 对齐 webui 的
// mux.HandleFunc。它明确不碰设备桥 —— 而设备桥是 GUI 作为**设备端**被
// agent 驱动的唯一通道，走的是 WS 上行/下行的 op 消息，两端没有任何
// 共享契约文件，全靠注释人工对齐。
//
// 真实漂移实例（2026-10-05 核实）：
//
//   1. GUI main.js 声明 caps 含 "omniparse"，且 executeHomeagentCmd 有完整
//      case "omniparse"（调 PowerShell + ha_omniparse_*.ps1）。但服务端
//      registry.go 的 capabilityTools 里**没有 omniparse 这个键**，整个
//      remotedevice 包 grep omniparse 命中 0 次 ⇒ deviceSupportsTool 对它
//      永远返回 false ⇒ agent 永远不会下发 omniparse ⇒ 那段实现是死代码。
//
//   2. cmd/gui/devicebridge_dll.js 声明了 12 个 devicebridge_* 导出，但
//      start() 无条件 `throw new Error('koffi not fully implemented')`，
//      且 loadFFI() 的 ffi-napi 备选分支只 return true 不返回 lib。
//      且全仓无人引用它 —— GUI 实际走 main.js:999 手工构造的原生 WebSocket。
//      已于 fix/devicebridge-capability-align 删除该文件。
//      （koffi 依赖保留：main.js:1873 用它 load user32.dll 做 computeruse 注入。）
//
//   3. GUI caps 里同时声明了 cmdrun/cmdresult（compatFullCaps，历史全能力
//      标记）与完整的 computeruse/screensue/... 精确能力。初判为「精确声明
//      被绕过」，**实为误判**：device_ctl_cmdrun 是所有能力的统一下发通道，
//      agent 调 computeruse/screensue 最终都由它下发 homeagent-*，
//      SupportsTool 查的是具体能力名而非 cmdrun。声明 cmdrun 是对的。
//
//   4. GUI caps 里的 status / deviceinfo 不在 capabilityTools 矩阵里。初判为
//      「服务端不认识的漂移」，**实为误判**：二者是元信息通道而非设备工具 ——
//      status 是心跳上报（registry.go:831），deviceinfo 的实现（device.go:338）
//      直接回显 meta.Caps 原始值、不经 deviceSupportsTool。
//
// 这些都不报错、不告警，GUI 照常运行，只是某些能力永远不会被触发。
//
// ## 与既有判据同路子
//
// endpoint-align / sse-backoff / retry-guard 都是：从**真实源码**提取，
// 不抄一份逻辑重写（抄的那份会和真实代码漂移，而漂移正是要防的）。
// 运行时探测在这里同样不可用：omniparse 是否可达取决于 agent 何时调用
// 哪个工具（LLM 决策），不是能构造出来的固定条件。
//
// 运行：node device-align.test.mjs

import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, "..", "..");

const MAIN_JS = join(here, "main.js");
const REGISTRY_GO = join(repoRoot, "internal", "plugins", "remotedevice", "registry.go");
const PROTOCOL_GO = join(
  repoRoot,
  "internal",
  "devicebridge",
  "client",
  "protocol.go",
);
const DLL_JS = join(here, "devicebridge_dll.js");

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) {
    console.log(`  ✓ ${name}`);
  } else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

// ── 1) 读源码 ───────────────────────────────────────────────────────

for (const [label, p] of [
  ["GUI 主进程", MAIN_JS],
  ["服务端能力矩阵", REGISTRY_GO],
  ["客户端协议", PROTOCOL_GO],
]) {
  if (!existsSync(p)) {
    console.error(`找不到${label}：${p}`);
    process.exit(1);
  }
}

const mainSrc = readFileSync(MAIN_JS, "utf8");
const registrySrc = readFileSync(REGISTRY_GO, "utf8");
const protocolSrc = readFileSync(PROTOCOL_GO, "utf8");

// ── 2) 抽能力集合 ───────────────────────────────────────────────────

// 服务端 capabilityTools 的键。形如：
//
//	var capabilityTools = map[string][]string{
//		"screen":    {"screensue", "screensee"},
//		...
//	}
//
// 只在 map 字面量的键位置上匹配（行首 tab + 引号 + 冒号），
// 不去匹配值位置的字符串，否则 tools 列表会被当成 cap。
function serverCaps() {
  const start = registrySrc.indexOf("var capabilityTools");
  if (start < 0) return null;
  // 找 map 字面量结尾 "\n}"，范围收窄以免吃到后面的 map
  const end = registrySrc.indexOf("\n}", start);
  if (end < 0) return null;
  const body = registrySrc.slice(start, end);
  const out = new Set();
  const re = /^\t"([a-z]+)":/gm;
  let m;
  while ((m = re.exec(body)) !== null) out.add(m[1]);
  return out;
}

// compatFullCaps：声明了这些的设备视为全能力，不参与能力裁剪。
function compatCaps() {
  const start = registrySrc.indexOf("var compatFullCaps");
  if (start < 0) return new Set();
  const end = registrySrc.indexOf("\n}", start);
  const body = registrySrc.slice(start, end < 0 ? undefined : end);
  const out = new Set();
  const re = /"([a-z]+)":\s*true/g;
  let m;
  while ((m = re.exec(body)) !== null) out.add(m[1]);
  return out;
}

// metaCaps：服务端消费但不参与 deviceSupportsTool 裁剪的 cap。
//
// 这类 cap 不是设备工具，而是**元信息通道**，拿 capabilityTools 去比对必然误报：
//
//   - status：registry.go 收设备的 {"op":"status"} 上报刷 LastSeen（心跳），
//     不在工具面。
//   - deviceinfo：是 agent 工具名（device.go:168），但 deviceinfo 的实现
//     （device.go:338 info()）直接读 meta.Caps 原始值回显，**不经过**
//     deviceSupportsTool —— 它要的就是「完整 caps」，正是能力裁剪的反面。
//
// 所以「服务端能不能识别一个 cap」有两个不同的问题：能被裁剪矩阵识别（capabilityTools），
// 与被别处消费（metaCaps）。只查前者会把这两个误判成漂移。
const metaCaps = new Set(["status", "deviceinfo"]);

// GUI 声明的 caps。main.js 里有**两处** hello 消息（首次连接 + set authorized
// 后重发），两处必须一致 —— 不一致会让服务端在不同时机看到不同的能力集。
function guiCaps() {
  const out = new Set();
  const re = /caps:\s*\[([\s\S]*?)\]/g;
  let m;
  while ((m = re.exec(mainSrc)) !== null) {
    const body = m[1];
    const item = /"([a-z]+)"/g;
    let c;
    while ((c = item.exec(body)) !== null) out.add(c[1]);
  }
  return out;
}

const caps = serverCaps();
const compat = compatCaps();
const gui = guiCaps();

check(
  "能抽到服务端能力矩阵",
  caps && caps.size >= 8,
  caps ? `只抽到 ${caps.size} 条 —— 正则可能已与 registry.go 漂移` : "找不到 capabilityTools",
);
check(
  "能抽到 GUI caps",
  gui.size >= 8,
  `只抽到 ${gui.size} 条 —— 正则可能已与 main.js 漂移`,
);

// ── 3) GUI 声明的每个 cap 服务端都要认识 ────────────────────────────
//
// 这是**硬门禁**：GUI 声明一个服务端不认识的 cap，服务端不会报错（未知 cap
// 只是匹配不到任何 tool），但依赖它的能力永远不会被下发 ⇒ 静默失效。
//
// 注意语义：命中 compatFullCaps 的 cap（cmd/cmdrun/cmdresult）在服务端
// 眼里是「全能力」，本身不算漂移 —— 第 4) 条单独盯它。

const unknown = [...gui].filter(
  (c) => !caps.has(c) && !compat.has(c) && !metaCaps.has(c),
);
check(
  "GUI 声明的 cap 服务端都认识",
  unknown.length === 0,
  unknown.length
    ? "服务端无此能力（对应实现永远不会被下发）：\n       - " + unknown.join("\n       - ")
    : "",
);

// ── 4) GUI 的能力声明必须真的起作用（不得整体退化为全能力）──────────
//
// ★ 早期版本这条判据说「GUI 不该声明 cmdrun/cmdresult」是**错的**，已撤回。
//   错在哪：device_ctl_cmdrun（device.go:270）是所有设备能力的**统一下发通道**
//   ——agent 对 computeruse/screensue/clipboardsue 的每次调用，最终都是
//   cmdrun 下发一条 homeagent-* 命令。SupportsTool 查的 tool 名是
//   computeruse 这类具体能力，不是 cmdrun。所以 GUI 声明 cmdrun 是对的。
//
// 真正的问题是反向的失效：GUI 若**只**声明 compatFullCaps
// （cmd/cmdrun/cmdresult）而漏掉具体能力，那 deviceSupportsTool 首循环
// 就 return true，精确声明形同虚设；但只要 GUI 声明了具体能力，服务端
// 就已经在按矩阵裁剪了，compat 项并不改变这一点（首个 return true 只在
// 没有任何已知能力时才会「意外」发生，而 GUI 明确声明了 12 个能力，
// 其中多数命中 capabilityTools）。
//
// 所以这条判据改为检查**真正会静默失效的形态**：声明了具体能力却同时
// 带着 compat 标记，使矩阵裁剪与实际能力不一致 —— 即 GUI 少声明了某个
// 它已实现的能力，但 compat 标记让服务端误以为它有。
//
// 若将来 GUI 要作为「只能跑 shell、无屏幕能力」的设备存在（即真的只有
// compat 能力），请连同下面这条判据一起删，并在注释里写明理由。

const guiHasConcrete = [...gui].filter((c) => caps.has(c));
const guiHasCompat = [...gui].filter((c) => compat.has(c));
check(
  "GUI 能力声明不退化为 compat-only",
  !(guiHasCompat.length > 0 && guiHasConcrete.length === 0),
  guiHasCompat.length > 0 && guiHasConcrete.length === 0
    ? `只声明了 ${guiHasCompat.join(", ")} ⇒ deviceSupportsTool 首循环 return true，` +
      "服务端无法按能力裁剪"
    : "",
);

// ── 5) GUI 实现的每个命令都要对应一个服务端能力 ────────────────────
//
// executeHomeagentCmd 的 case 分支名就是设备桥命令名。服务端只会下发
// capabilityTools 值里的工具名（经 device_ctl_* 工具面），所以 GUI
// 实现了一个服务端没有对应工具的命令 ⇒ 永远收不到。

function guiCommandCases() {
  const start = mainSrc.indexOf("function executeHomeagentCmd");
  if (start < 0) return null;
  // 函数体到下一个顶层 function 声明为止
  const rest = mainSrc.slice(start);
  const end = rest.indexOf("\nfunction ");
  const body = end < 0 ? rest : rest.slice(0, end);
  const out = new Set();
  // 只取 switch 里的 case（缩进 4 空格），避免抓到嵌套 switch（computeruse
  // 内部还有一层 switch，case move/click/... 是动作名不是命令名）
  const re = /^ {4}case "([a-z]+)":/gm;
  let m;
  while ((m = re.exec(body)) !== null) out.add(m[1]);
  return out;
}

// 服务端可下发的工具名 = capabilityTools 所有 value 的并集。
function serverTools() {
  const start = registrySrc.indexOf("var capabilityTools");
  if (start < 0) return new Set();
  const end = registrySrc.indexOf("\n}", start);
  const body = registrySrc.slice(start, end);
  const out = new Set();
  const re = /"([a-z]+)"/g;
  let m;
  while ((m = re.exec(body)) !== null) out.add(m[1]);
  return out;
}

const cmdCases = guiCommandCases();
const tools = serverTools();

check(
  "能抽到 GUI 命令 case",
  cmdCases && cmdCases.size >= 6,
  cmdCases ? `只抽到 ${cmdCases.size} 条 —— 正则可能已与 main.js 漂移` : "找不到 executeHomeagentCmd",
);

const orphanCmds = cmdCases ? [...cmdCases].filter((c) => !tools.has(c)) : [];
check(
  "GUI 实现的命令服务端都有对应工具",
  orphanCmds.length === 0,
  orphanCmds.length
    ? "服务端无对应工具（下发不了）：\n       - " + orphanCmds.join("\n       - ")
    : "",
);

// ── 6) 声明了能力就必须实现它 ───────────────────────────────────────
//
// 反向：GUI 在 caps 里声明了 X，executeHomeagentCmd 却没有 case "X"
// ⇒ 服务端会下发 X（因为 capabilityTools 里 X 是合法工具），GUI 落到
// default 分支。这是**运行时空转**：req_id 收不到回执，网关侧超时。

const declaredButNotImplemented = [...gui]
  .filter((c) => tools.has(c) && !compat.has(c) && cmdCases && !cmdCases.has(c));
check(
  "声明的能力都已实现",
  declaredButNotImplemented.length === 0,
  declaredButNotImplemented.length
    ? "声明了但无 case 实现：\n       - " + declaredButNotImplemented.join("\n       - ")
    : "",
);

// ── 7) devicebridge_dll.js 不得回来 ──────────────────────────────────
//
// ★ 这个文件已于 fix/devicebridge-capability-align 删除。删除理由（三条都查证过）：
//
//   1. 无人引用：全仓（除本判据外）grep DeviceBridgeDLL 命中 0。
//   2. 必然抛：start() 无条件 throw 'koffi not fully implemented'。
//   3. 方向已废弃：GUI 走的是 main.js:999 手工构造的原生 WebSocket
//      （Sec-WebSocket-Key/Version 握手），不经 FFI。
//
//   ⚠ 但 koffi **依赖本身要留着**：main.js:1873 用它 koffi.load("user32.dll")
//      做 computeruse 的鼠标键盘注入。那是真实在用的，与本文件无关。
//      删文件不等于删依赖。
//
// 判据保留为反向守卫：文件若被重新引入，必须是完整实现而非抛异常的桩。

if (!existsSync(DLL_JS)) {
  console.log("  ✓ devicebridge_dll.js 已删除（GUI 走原生 WebSocket，不经 FFI）");
} else {
  const dllSrc = readFileSync(DLL_JS, "utf8");
  const stubs = [
    /throw new Error\(["']koffi not fully implemented["']\)/,
    /TODO:\s*实现 koffi 调用/,
    /TODO:.*FFI/,
  ].filter((re) => re.test(dllSrc));
  check(
    "devicebridge_dll.js 不是未实现桩",
    stubs.length === 0,
    stubs.length
      ? "存在未实现桩（require 即抛）。走 FFI 前补全，或删除该文件改用已实现的路径"
      : "",
  );
}

// ── 8) protocol.go 的消息类型要在 GUI 侧有落点 ──────────────────────
//
// DataStart/DataEnd/SpeechStart/SpeechEnd/EventMsg/StatusMsg 是设备桥
// 上行的消息契约。GUI 若不实现其中任何一个，对应能力（媒体回传、TTS
// 播放、主动事件上报）就是哑的 —— 而 GUI 的 caps 里声明了 camerasue，
// camerasue 正是靠 sendDeviceDataChunked 走 DataStart/DataEnd。
//
// 只判「caps 声明了该能力 ⇒ 相关消息类型必须有实现」。

const protocolTypes = new Set();
{
  const re = /^type\s+([A-Z]\w+)\s+struct/mg;
  let m;
  while ((m = re.exec(protocolSrc)) !== null) protocolTypes.add(m[1]);
}

check(
  "能抽到 protocol.go 消息类型",
  protocolTypes.size >= 6,
  `只抽到 ${protocolTypes.size} 条 —— 正则可能已与 protocol.go 漂移`,
);

// 媒体回传能力 → 必须有 DataStart/DataEnd 实现
if (gui.has("camerasue")) {
  const hasDataStart = /DataStart|data_start|dataStart/i.test(mainSrc);
  const hasDataEnd = /DataEnd|data_end|dataEnd/i.test(mainSrc);
  check(
    "声明 camerasue ⇒ 已实现媒体分块回传",
    hasDataStart && hasDataEnd,
    !hasDataStart || !hasDataEnd
      ? "camerasue 依赖 cmd_data_start/cmd_data_end 分块回传，但 GUI 侧找不到实现"
      : "",
  );
} else {
  console.log("  · GUI 未声明 camerasue，跳过媒体回传判据");
}

// ── 9) 两处 hello 的 caps 必须一致 ──────────────────────────────────
//
// main.js 有两处 caps 数组（首次连接 / set authorized 后重发）。不一致
// ⇒ 服务端在不同时机看到不同的能力集，表现为「改授权后能力变了」这类
// 难复现的偶发问题。

const capsBlocks = [];
{
  const re = /caps:\s*\[([\s\S]*?)\]/g;
  let m;
  while ((m = re.exec(mainSrc)) !== null) {
    const set = new Set();
    const item = /"([a-z]+)"/g;
    let c;
    while ((c = item.exec(m[1])) !== null) set.add(c[1]);
    capsBlocks.push(set);
  }
}

if (capsBlocks.length < 2) {
  console.log(`  · 只找到 ${capsBlocks.length} 处 caps 声明，跳过一致性判据`);
} else {
  const [first, ...rest] = capsBlocks;
  const inconsistent = rest.filter(
    (b) => b.size !== first.size || [...b].some((v) => !first.has(v)),
  );
  check(
    "多处 hello 的 caps 声明一致",
    inconsistent.length === 0,
    inconsistent.length
      ? `第 2 处起有 ${inconsistent.length} 处与第 1 处不一致`
      : "",
  );
}

// ── 汇总 ────────────────────────────────────────────────────────────

console.log("");
console.log(
  `  服务端能力 ${caps ? caps.size : 0} 个 / compat ${compat.size} 个 / ` +
    `可下发工具 ${tools.size} 个 / GUI caps ${gui.size} 个 / GUI 实现 ${cmdCases ? cmdCases.size : 0} 个`,
);
console.log("");

if (failures > 0) {
  console.log(`失败 ${failures} 条`);
  process.exit(1);
}
console.log("全部通过");