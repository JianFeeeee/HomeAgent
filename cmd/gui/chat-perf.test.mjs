// 聊天页重渲性能的判据 —— 在**真实 Electron 渲染进程**里跑。
//
// ## 要判的是什么
//
// 2026-09-28 用户报「聊天页面卡得让人没有用的欲望」。真机实测
// （Xvfb + Electron + CDP）确认：`renderChat()` 单次耗时随消息数
// **严格线性**，约 1.33ms/消息：
//
//   10 条 →  13.7ms      100 条 → 128ms
//   50 条 →  61.7ms      200 条 → 259ms
//                      400 条 → 534ms
//
// 而流式追加时每个 chunk 也会走这条路（即使有节流，放行时就是一次
// 全量重渲）。⇒ 聊到几百条时，单次重渲要 **0.5 秒**，体感必然是「卡」。
//
// ## 为什么必须真浏览器跑
//
// 瓶颈在 **DOM 操作与 markdown 渲染**（innerHTML 赋值 + 布局），
// 在 node 里跑毫无意义 —— jsdom 不会做布局，测出来的数是假的。
//
// ★ 我第一版在无后端连接时测，得到「0ms / 0 DOM 节点」—— 那是
//   `buildChatLayout()` 走了「请先添加后端连接」分支、聊天区压根没建
//   出来，**测不到**而不是「不卡」。这类假绿灯必须排除。
//
// 运行：node cmd/gui/chat-perf.test.mjs
// 需要：DISPLAY 或 xvfb-run、已构建的 electron、正在运行的 GUI 实例
//       （脚本自己会拉起，见 main()）。

import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const GUI = here;   // 判据就放在 cmd/gui/ 下（here 已是该目录）
const PORT = Number(process.env.GUI_CDP_PORT || 9350);

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) console.log(`  ✓ ${name}`);
  else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

// ── 拉起 GUI（若未在跑）──────────────────────────────────────────
// 连不上时返回空数组而不是抛 —— 「GUI 没起」是**正常起始状态**，
// 由 ensureGui 去拉起它。让 fetch 的 ECONNREFUSED 冒到顶层会直接崩掉，
// 什么都测不到。
async function cdpTargets() {
  try {
    const r = await fetch(`http://127.0.0.1:${PORT}/json`);
    return r.json();
  } catch {
    return [];
  }
}

async function ensureGui() {
  const t = await cdpTargets();
  if (t.some((x) => x.type === "page" && x.url.includes("index.html"))) return null;
  const bin = join(GUI, "node_modules/.bin/electron");
  if (!existsSync(bin)) throw new Error("electron 未安装，先 cd cmd/gui && npm i");
  // ★ --server-args 必须作为**单个** argv 元素。
  //   写成 "--server-args=-screen 0 1280x900x24" 时，若用 shell 会被按空格
  //   拆成多个参数（--server-args=-screen / 0 / 1280x900x24），
  //   xvfb-run 收到非法的 display 尺寸 ⇒ 静默起不来。
  //   这里用 spawn 的数组形式（不经 shell），每个元素原样传递。
  const child = spawn(
    "setsid",
    ["xvfb-run", "-a", "--server-args=-screen 0 1280x900x24", bin, "--no-sandbox",
     `--remote-debugging-port=${PORT}`, "."],
    // ★ detached 只脱离**进程组**，不脱离**会话**；脚本结尾 process.exit()
    //   仍会把它带走（实测：GUI 明明 [tray] READY 起来了，判据却报
    //   「找不到 GUI 页面」—— 因为它被自己启动的父脚本杀了）。
    //   彻底的做法是 setsid：新建会话，父进程退出后不受影响。
    { cwd: GUI, detached: true, stdio: ["ignore", "ignore", "ignore"] },
  );
  child.unref();
  for (let i = 0; i < 40; i++) {
    await new Promise((r) => setTimeout(r, 1000));
    const t = await cdpTargets();
    if (t.some((x) => x.type === "page" && x.url.includes("index.html"))) return child;
  }
  throw new Error(
    `GUI 启动超时（${PORT}）。手动验证命令：\n` +
      `  cd cmd/gui && setsid xvfb-run -a --server-args="-screen 0 1280x900x24" ` +
      `./node_modules/.bin/electron --no-sandbox --remote-debugging-port=${PORT} . &`,
  );
}

async function evalInGui(expr) {
  const tabs = await cdpTargets();
  const page = tabs.find((t) => t.type === "page" && t.url.includes("index.html"));
  if (!page) throw new Error("找不到 GUI 页面");
  const sock = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((r) => (sock.onopen = r));
  const id = Math.floor(Math.random() * 1e6);
  sock.send(JSON.stringify({
    id, method: "Runtime.evaluate",
    params: { expression: expr, awaitPromise: true, returnByValue: true },
  }));
  // ★ JSON.parse 必须包 try —— CDP 的 onmessage 也可能收到二进制帧
  //   （DevTools 自己的通知），e.data 不是 JSON 时裸 parse 会抛，
  //   抛在事件回调里既冒泡不到 await、也等不到 resolve ⇒ 整个判据挂死。
  const res = await new Promise((r) => {
    sock.onmessage = (e) => {
      let m;
      try {
        m = JSON.parse(e.data);
      } catch {
        return;   // 非 JSON 帧，忽略
      }
      if (m && m.id === id) r(m);
    };
  });
  sock.close();
  const v = res.result?.result?.value;
  if (v === undefined) throw new Error(res.result?.exceptionDetails?.text || "求值失败");
  return v;
}

// ── 判据 ────────────────────────────────────────────────────────

// ★ 必须先确保 GUI 在跑（自己拉起）。
//   漏掉这一行时，脚本会在 GUI 未启动时直接报「找不到 GUI 页面」——
//   我重写文件时把调用丢了，而 ensureGui 的定义还在，看起来一切正常。
await ensureGui();

const PROBE = `(async () => {
  // ★ 必须先有后端连接，否则 buildChatLayout() 走「请先添加」分支，
  //   chat-msgs 压根不建 ⇒ 后面全是 0ms / 0 DOM 的假绿灯。
  const K = ${JSON.stringify(process.env.GUI_API_KEY || "probe-key")};
  state.connections = [{id:'p',name:'p',type:'webui',url:'http://127.0.0.1:8080',apiKey:K}];
  state.currentConn = state.connections[0];
  _chatLayoutBuilt = false;
  buildChatLayout();
  if (!document.getElementById('chat-msgs')) return { error: 'chat-msgs 未建出（无后端连接？）' };

  const body = '**要点** 一段中文正文。'.repeat(20);
  const measure = (n) => {
    state.messages = Array.from({length:n}, (_,i)=>({role: i%2?'assistant':'user', content: body+' #'+i}));
    document.querySelector('#view-chat')?.classList.add('active');
    renderChat();
    const t = [];
    for (let k=1;k<=4;k++) {
      state.messages[n-1].content += 'x'.repeat(20);   // 变内容 ⇒ signature 变 ⇒ 不会走缓存
      const t0 = performance.now();
      renderChat();
      t.push(performance.now()-t0);
    }
    return { n, avg: t.reduce((a,b)=>a+b,0)/t.length,
             dom: document.querySelectorAll('#chat-msgs *').length };
  };
  return [50, 200, 400].map(measure);
})()`;

const SPLIT = `(async () => {
  const K = ${JSON.stringify(process.env.GUI_API_KEY || "probe-key")};
  state.connections = [{id:'p',name:'p',type:'webui',url:'http://127.0.0.1:8080',apiKey:K}];
  state.currentConn = state.connections[0];
  _chatLayoutBuilt = false;
  buildChatLayout();
  if (!document.getElementById('chat-msgs')) return { error: 'chat-msgs 未建出' };
  const body = '**要点** 一段中文正文。'.repeat(20);
  state.messages = Array.from({length:200}, (_,i)=>({role:i%2?'assistant':'user', content: body+' #'+i}));
  document.querySelector('#view-chat')?.classList.add('active');
  renderChat();

  // 拆开量：markdown 渲染 vs 整体（含 DOM 写入/布局）
  const texts = state.messages.map(m=>m.content);
  let md = 0;
  for (let k=0;k<3;k++){ texts[0]+='y'; const t0=performance.now(); for(const c of texts) renderMd(c); md += performance.now()-t0; }
  md /= 3;
  let full = 0;
  for (let k=1;k<=3;k++){ state.messages[0].content+='y'; const t0=performance.now(); renderChat(); full += performance.now()-t0; }
  full /= 3;
  return { full, md, dom: document.querySelectorAll('#chat-msgs *').length };
})()`;

const split = await evalInGui(SPLIT);
if (split && split.error) {
  check("能拆出 md / 整体耗时", false, split.error);
} else {
  const mdPct = Math.round((split.md / split.full) * 100);
  console.log(`  · 200 条：整体 ${split.full.toFixed(1)}ms，其中 renderMd ${split.md.toFixed(1)}ms（${mdPct}%）`);
  check(
    "瓶颈已定位（拆分数据可用）",
    split.full > 0 && split.md >= 0,
    "拆不出比例 ⇒ 定位不了该优化哪一层",
  );
}

const rows = await evalInGui(PROBE);
if (rows && rows.error) {
  check("能测到真实 DOM", false, rows.error);
} else {
  check("能测到真实 DOM", true);

  const byN = Object.fromEntries(rows.map((r) => [r.n, r]));
  for (const r of rows) {
    console.log(`  · ${r.n} 条 → ${r.avg.toFixed(1)}ms，DOM ${r.dom} 节点`);
  }

  // ★ 体感阈值：200 条时单次重渲超过 100ms，用户就会明确感到"卡"；
  //   实测 250ms。判据定在 100ms —— 这是「产品体感」而非 benchmark 数字。
  const at200 = byN[200];
  check(
    "200 条消息时单次重渲 < 100ms",
    at200 && at200.avg < 100,
    `实测 ${at200 ? at200.avg.toFixed(1) : "?"}ms —— 聊天越久越卡的主因`,
  );

  const g1 = byN[50].avg / 50;
  const g2 = byN[400].avg / 400;
  check(
    "单条成本随规模下降或持平（次线性）",
    g2 <= g1 * 1.05,
    `每条成本：50 条时 ${g1.toFixed(3)}ms，400 条时 ${g2.toFixed(3)}ms` +
      ` ⇒ ${g2 > g1 * 1.05 ? "严格线性 ⇒ 没有上限，聊久必卡" : ""}`,
  );
}

// ── 判据：打开页面必须直接停在最新消息 ───────────────────────────
//
// 用户报「打开 app 和 webui，没有停在最新消息处，还要反复滑动」。
//
// 真机实测（200 条 / 3500 DOM 节点）：
//   behavior:"smooth" → 立即 scrollTop=0，300ms 后只到 6894（上限 22838）
//                       ⇒ 既慢又**没到位**
//   scrollTop=scrollHeight → 立即 22838，一次到位
//
// 原因：紧邻的 innerHTML 全量重建让 smooth 动画的起点算在**旧**布局上。
const STICK = `(async () => {
  const K = ${JSON.stringify(process.env.GUI_API_KEY || "probe-key")};
  state.connections = [{id:'p',name:'p',type:'webui',url:'http://127.0.0.1:8080',apiKey:K}];
  state.currentConn = state.connections[0];
  _chatLayoutBuilt = false;
  buildChatLayout();
  const body = '要点正文。'.repeat(30);
  state.messages = Array.from({length:200}, (_,i)=>({role:i%2?'assistant':'user', content: body+' #'+i}));
  document.querySelector('#view-chat')?.classList.add('active');
  const el = document.getElementById('chat-msgs');
  el.scrollTop = 0;                                  // 模拟「刚打开，在顶部」
  await new Promise(r=>setTimeout(r,60));
  state.messages[0].content += 'q';                   // 触发 signature 变化
  renderChat();
  const immediate = Math.round(el.scrollTop);
  await new Promise(r=>setTimeout(r,300));
  const max = Math.round(el.scrollHeight - el.clientHeight);
  return { immediate, after: Math.round(el.scrollTop), max, stick: state.chatStick };
})()`;

const st = await evalInGui(STICK);
if (st && st.error) {
  check("能测到滚动位置", false, st.error);
} else {
  check(
    "打开即停在最新消息（立即到位）",
    st.immediate >= st.max - 5,
    `立即 scrollTop=${st.immediate}，上限=${st.max}` +
      ` ⇒ smooth 动画在 innerHTML 重建后算错起点，用户得手动滑到底`,
  );
  check(
    "300ms 后仍在底部（不被后续渲染带偏）",
    st.after >= st.max - 5,
    `300ms 后 scrollTop=${st.after}，上限=${st.max}`,
  );
}

// ── 判据：增量渲染不能丢消息 ─────────────────────────────────────
//
// ★ 快了但丢消息就白搭 —— 这条比性能判据更重要。
//
// 覆盖增删改四种路径：尾部追加（发消息/工具轮）、头部前插（loadOlderChat）、
// 中间修改（内容更新）、尾部删除（去重/截断）。
const CORRECT = `(async () => {
  const K = ${JSON.stringify(process.env.GUI_API_KEY || "probe-key")};
  state.connections = [{id:'p',name:'p',type:'webui',url:'http://127.0.0.1:8080',apiKey:K}];
  state.currentConn = state.connections[0];
  _chatLayoutBuilt = false;
  buildChatLayout();
  const el = document.getElementById('chat-msgs');
  const body = '要点正文。'.repeat(20);
  const out = [];
  const chk = (label) => out.push({
    label, state: state.messages.length, dom: el.childElementCount,
    ok: el.childElementCount === state.messages.length,
  });
  state.messages = Array.from({length:50},(_,i)=>({role:i%2?'assistant':'user', content: body+' #'+i}));
  renderChat(); chk('初始 50 条');
  for (let k=0;k<3;k++) state.messages.push({role:'user', content: body+' new'+k});
  renderChat(); chk('尾部追加 3 条');
  for (let k=0;k<5;k++) state.messages.unshift({role:'user', content: body+' old'+k});
  renderChat(); chk('头部前插 5 条');
  state.messages[10].content = body + ' CHANGED';
  renderChat(); chk('修改中间一条');
  state.messages.splice(-2);
  renderChat(); chk('删除尾部 2 条');
  return out;
})()`;

const corr = await evalInGui(CORRECT);
if (!Array.isArray(corr) || corr.length === 0) {
  check("能测到增删改一致性", false, String(corr));
} else {
  check("能测到增删改一致性", true);
  for (const r of corr) {
    check(
      `增量不丢消息：${r.label}`,
      r.ok,
      `state 有 ${r.state} 条，DOM 只有 ${r.dom} 个子节点`,
    );
  }
}

console.log(failures === 0 ? "\n全部通过" : `\n${failures} 项未通过`);
process.exit(failures === 0 ? 0 : 1);
