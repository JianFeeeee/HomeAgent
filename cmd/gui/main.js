const { app, BrowserWindow, ipcMain, Menu } = require("electron");
// 设备桥直连远程网关：绕过系统代理（本机 clash 代理会导致 wss 被雷池 403）
try {
  app.commandLine.appendSwitch("no-proxy-server");
} catch (e) {}
const path = require("path");
const fs = require("fs");
const { spawn } = require("child_process");
const http = require("http");
const crypto = require("crypto");
const { pathToFileURL } = require("url");

const CONNECTIONS_FILE = path.join(app.getPath("userData"), "connections.json");
const LOG_FILE = path.join(app.getPath("userData"), "gui.log");
function log(msg) {
  try {
    fs.appendFileSync(LOG_FILE, new Date().toISOString() + " " + msg + "\n");
  } catch (e) {}
}
const _consoleLog = console.log,
  _consoleErr = console.error;
console.log = function () {
  log(Array.prototype.slice.call(arguments).join(" "));
  _consoleLog.apply(null, arguments);
};
console.error = function () {
  log("ERR " + Array.prototype.slice.call(arguments).join(" "));
  _consoleErr.apply(null, arguments);
};

let homedProcess = null;
let mainWindow;
let authRule = null;

function installAuthRule() {
  const { session } = require("electron");
  session.defaultSession.webRequest.onBeforeSendHeaders((details, callback) => {
    const h = Object.assign({}, details.requestHeaders);
    // 按目标 host 校验作用域：网关会把请求 302 到 login.* 域，若按 url 前缀匹配，
    // 登录域自身请求会被错误附加本站 cookie。
    const hu = (() => {
      try {
        return new URL(details.url);
      } catch (e) {
        return null;
      }
    })();
    if (authRule && hu && authRule.hosts && authRule.hosts.has(hu.hostname)) {
      if (authRule.headers) {
        Object.keys(authRule.headers).forEach((k) => {
          h[k] = authRule.headers[k];
        });
      }
      // Electron 会自动附带 jar 中 Cookie（含外部网关 Set-Cookie 的 sl-session）。
      // 这里再显式合并持久化 cookie 与 sl-session 兜底，避免网关再 302。
      const extra = [authRule.cookie, authRule.slSession].filter(Boolean);
      if (extra.length) {
        const existing = h["Cookie"] || "";
        h["Cookie"] = [existing].concat(extra).filter(Boolean).join("; ");
      }
    }
    callback({ requestHeaders: h });
  });
  session.defaultSession.webRequest.onHeadersReceived((details, callback) => {
    const h = Object.assign({}, details.responseHeaders || {});
    const inScope = (() => {
      if (details.method === "OPTIONS") return true;
      if (!authRule || !authRule.urlHost) return false;
      try {
        return new URL(details.url).hostname === authRule.urlHost;
      } catch (e) {
        return false;
      }
    })();
    if (inScope) {
      h["Access-Control-Allow-Origin"] = ["*"];
      h["Access-Control-Allow-Methods"] = ["GET,POST,PUT,DELETE,OPTIONS"];
      h["Access-Control-Allow-Headers"] = [
        "Content-Type, X-API-Key, Authorization, Cookie",
      ];
      h["Access-Control-Max-Age"] = ["86400"];
    }
    if (details.method === "OPTIONS") {
      callback({ responseHeaders: h, statusLine: "HTTP/1.1 200 OK" });
      return;
    }
    callback({ responseHeaders: h });
  });
  session.defaultSession.webRequest.onCompleted((details) => {
    if (!authRule || !authRule.urlHost) return;
    try {
      if (new URL(details.url).hostname !== authRule.urlHost) return;
    } catch (e) {
      return;
    }
    if (details.url.indexOf("/api/") !== -1) {
      log(
        "[req] " +
          details.method +
          " " +
          details.statusCode +
          " " +
          details.url.slice(0, 120),
      );
    }
  });
  session.defaultSession.webRequest.onErrorOccurred((details) => {
    if (!authRule || !authRule.urlHost) return;
    try {
      if (new URL(details.url).hostname !== authRule.urlHost) return;
    } catch (e) {
      return;
    }
    log(
      "[req-err] " +
        details.method +
        " " +
        details.error +
        " " +
        details.url.slice(0, 120),
    );
  });
}

// 组装 authRule（url + 持久化 cookie + 显式 sl-session），并缓存 hosts 集合。
// 供 installAuthRule 按目标 host 做作用域匹配：jsdom 前缀匹配有误伤，host 级最稳。
async function applyAuthRule(url, cookie, headers) {
  if (!url) {
    authRule = null;
    return;
  }
  let hosts;
  let urlHost = "";
  try {
    const h = new URL(url).hostname;
    urlHost = h;
    hosts = new Set([h]);
  } catch (e) {
    hosts = new Set();
  }
  authRule = {
    url,
    urlHost,
    hosts,
    cookie: cookie || "",
    headers: headers || {},
  };
  // 显式的 sl-session 从 cookie jar 里取（网关 Set-Cookie 且 HttpOnly）
  authRule.slSession = await adoptSlSession(url);
}

// 从 Electron cookie jar 中取出目标 host 的 sl-session（若存在）。
async function adoptSlSession(url) {
  try {
    const { session } = require("electron");
    const hostname = new URL(url).hostname;
    const all = await session.defaultSession.cookies.get({});
    const hit = (all || []).find(
      (c) =>
        c.name === "sl-session" &&
        (c.domain || "").replace(/^\./, "") === hostname,
    );
    return hit ? "sl-session=" + hit.value : "";
  } catch (e) {
    return "";
  }
}

// 带 Electron cookie jar 的 fetch（跟随外部网关 302 → login 域 → 回跳，最多 6 跳），
// 使 POST /api/v1/login 真正触达内层 HomeAgent 并取回 homeagent_session。
async function sessionFetch(initUrl, opts) {
  const { session } = require("electron");
  let url = initUrl;
  let resp;
  for (let i = 0; i < 6; i++) {
    resp = await session.defaultSession.fetch(url, opts);
    if (
      resp.status >= 300 &&
      resp.status < 400 &&
      resp.headers.get("location")
    ) {
      url = new URL(resp.headers.get("location"), url).toString();
      continue;
    }
    return resp;
  }
  return resp;
}

function loadConnections() {
  try {
    if (fs.existsSync(CONNECTIONS_FILE)) {
      const raw = fs
        .readFileSync(CONNECTIONS_FILE, "utf-8")
        .replace(/^\uFEFF/, "");
      const data = JSON.parse(raw);
      normalizeConnections(data);
      return data;
    }
  } catch (e) {
    console.error("Failed to load connections:", e);
    // 配置损坏：备份后重建，避免应用一直处于"无连接"状态
    try {
      const backup = CONNECTIONS_FILE + ".bak";
      fs.copyFileSync(CONNECTIONS_FILE, backup);
      fs.writeFileSync(
        CONNECTIONS_FILE,
        '{"connections":[],"currentId":null}',
        "utf-8",
      );
      console.error("Backed up corrupt connections to", backup);
    } catch (e2) {
      console.error("Failed to recover connections file:", e2);
    }
  }
  // Fallback: check app resource dir (installer writes fallback copy there)
  try {
    const fallback = path.join(__dirname, "connections.json");
    if (fs.existsSync(fallback)) {
      const data = JSON.parse(fs.readFileSync(fallback, "utf-8"));
      normalizeConnections(data);
      saveConnections(data);
      console.log("Imported connections from app resource dir");
      return data;
    }
  } catch (e) {
    console.error("Fallback connections load failed:", e);
  }
  return { connections: [], currentId: null };
}

// 兼容旧数据：缺失的 type 默认为 webui（HTTP）
function normalizeConnections(data) {
  if (!data || !Array.isArray(data.connections)) return;
  data.connections.forEach((c) => {
    if (!c.type) c.type = "webui";
    if (c.type !== "cli" && c.type !== "webui") c.type = "webui";
  });
}

function saveConnections(data) {
  try {
    fs.writeFileSync(CONNECTIONS_FILE, JSON.stringify(data, null, 2), "utf-8");
  } catch (e) {
    console.error("Failed to save connections:", e);
  }
}

function findHomed() {
  if (process.platform !== "win32") return null;
  const exeDir = path.dirname(app.getPath("exe"));
  const p = path.resolve(exeDir, "..", "homed.exe");
  return fs.existsSync(p) ? p : null;
}

function isServerRunning() {
  return new Promise((resolve) => {
    const req = http.get("http://localhost:8080/", () => resolve(true));
    req.on("error", () => resolve(false));
    req.setTimeout(2000, () => {
      req.destroy();
      resolve(false);
    });
  });
}

function waitForServer(maxWait = 8000) {
  return new Promise((resolve) => {
    const start = Date.now();
    const check = () => {
      isServerRunning().then((running) => {
        if (running) return resolve(true);
        if (Date.now() - start > maxWait) return resolve(false);
        setTimeout(check, 300);
      });
    };
    check();
  });
}

function startHomed() {
  const homedBin = findHomed();
  if (!homedBin) {
    console.log("homed.exe not found near GUI, skipping auto-launch");
    return;
  }
  const dataDir = path.resolve(path.dirname(homedBin), "data");
  console.log("Starting homed:", homedBin, "-data", dataDir);
  homedProcess = spawn(homedBin, ["-data", dataDir], {
    stdio: "ignore",
    detached: false,
    windowsHide: true,
  });
  homedProcess.on("error", (err) => {
    console.error("homed start failed:", err.message);
    homedProcess = null;
  });
  homedProcess.on("exit", (code) => {
    console.log("homed exited with code", code);
    homedProcess = null;
  });
}

function stopHomed() {
  if (homedProcess) {
    homedProcess.kill();
    homedProcess = null;
  }
}

function createWindow() {
  const menu = Menu.buildFromTemplate([]);
  Menu.setApplicationMenu(menu);

  mainWindow = new BrowserWindow({
    width: 1280,
    height: 860,
    minWidth: 900,
    minHeight: 600,
    title: "HomeAgent",
    frame: false,
    icon: path.join(__dirname, "icon.ico"),
    webPreferences: {
      preload: path.join(__dirname, "preload.js"),
      contextIsolation: true,
      nodeIntegration: false,
    },
  });

  mainWindow.loadFile(path.join(__dirname, "renderer", "index.html"));

  if (process.argv.includes("--dev")) {
    mainWindow.webContents.openDevTools();
  }

  // 退出进托盘：拦截窗口关闭事件（exitToTray 且托盘可用时 hide 而非 close）
  mainWindow.on("close", (e) => {
    try {
      const _p = loadGuiPrefs();
      if (_p && _p.exitToTray && tray) {
        e.preventDefault();
        mainWindow.hide();
        return;
      }
    } catch (err) {}
  });
  mainWindow.on("closed", () => {
    mainWindow = null;
  });
}

ipcMain.handle("window:minimize", (e) => {
  BrowserWindow.fromWebContents(e.sender)?.minimize();
});
ipcMain.handle("window:toggleMaximize", (e) => {
  const win = BrowserWindow.fromWebContents(e.sender);
  if (!win) return;
  if (win.isMaximized()) win.unmaximize();
  else win.maximize();
});
ipcMain.handle("window:close", (e) => {
  const win = BrowserWindow.fromWebContents(e.sender);
  if (!win) return;
  // 退出进托盘：偏好开启且托盘存在时隐藏而非关闭
  try {
    const _p = loadGuiPrefs();
    console.log(
      "[window:close] exitToTray=" +
        !!(_p && _p.exitToTray) +
        " tray=" +
        !!tray,
    );
    if (_p && _p.exitToTray && tray) {
      win.hide();
      return true;
    }
  } catch (err) {}
  win.close();
});

ipcMain.handle("connections:list", () => {
  return loadConnections();
});

ipcMain.handle("connections:add", (_, conn) => {
  const data = loadConnections();
  const id = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
  data.connections.push({
    id,
    name: conn.name || "",
    url: conn.url || "",
    apiKey: conn.apiKey || "",
    type: conn.type === "cli" ? "cli" : "webui",
    socketPath: conn.socketPath || "",
    // 网关认证字段在 add 时必须一并持久化，否则新增连接后 cookie/网关标记丢失
    username: conn.username || "",
    password: conn.password || "",
    cookie: conn.cookie || "",
    headers: conn.headers || "",
    gateway: !!conn.gateway,
  });
  if (!data.currentId) data.currentId = id;
  saveConnections(data);
  try {
    rebuildTrayMenu();
  } catch (e) {}
  return data;
});

ipcMain.handle("connections:update", (_, { id, updates }) => {
  const data = loadConnections();
  const idx = data.connections.findIndex((c) => c.id === id);
  if (idx !== -1) {
    data.connections[idx] = { ...data.connections[idx], ...updates };
    saveConnections(data);
  }
  try {
    rebuildTrayMenu();
  } catch (e) {}
  return data;
});

ipcMain.handle("connections:delete", (_, id) => {
  const data = loadConnections();
  data.connections = data.connections.filter((c) => c.id !== id);
  if (data.currentId === id) {
    data.currentId =
      data.connections.length > 0 ? data.connections[0].id : null;
  }
  saveConnections(data);
  try {
    rebuildTrayMenu();
  } catch (e) {}
  return data;
});

function downloadTo(src, dest) {
  return new Promise((resolve, reject) => {
    const mod = src.startsWith("https:") ? require("https") : http;
    const req = mod.get(
      src,
      {
        headers: {
          "User-Agent":
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
        },
        rejectUnauthorized: false,
      },
      (res) => {
        if (
          res.statusCode >= 300 &&
          res.statusCode < 400 &&
          res.headers.location
        ) {
          res.resume();
          downloadTo(new URL(res.headers.location, src).toString(), dest).then(
            resolve,
            reject,
          );
          return;
        }
        if (res.statusCode !== 200) {
          res.resume();
          reject(new Error("HTTP " + res.statusCode));
          return;
        }
        const out = fs.createWriteStream(dest);
        res.pipe(out);
        out.on("finish", () => out.close(resolve));
        out.on("error", reject);
        res.on("error", reject);
      },
    );
    req.on("error", reject);
    req.setTimeout(60000, () => req.destroy(new Error("timeout")));
  });
}

ipcMain.handle("bg:cache", async (_, { src }) => {
  if (!src || typeof src !== "string") return { ok: false, error: "no src" };
  const dir = path.join(app.getPath("userData"), "bg-cache");
  try {
    fs.mkdirSync(dir, { recursive: true });
    const hash = crypto
      .createHash("sha1")
      .update(src)
      .digest("hex")
      .slice(0, 24);
    let dest = null;
    if (/^https?:\/\//i.test(src)) {
      dest = path.join(dir, hash);
      if (!fs.existsSync(dest)) {
        try {
          await downloadTo(src, dest);
        } catch (e) {
          return {
            ok: false,
            error: "下载失败: " + e.message,
            useOriginal: true,
          };
        }
      }
    } else if (/^file:\/\//i.test(src)) {
      dest = new URL(src).pathname.replace(/^\/([A-Za-z]:)/, "$1");
      if (!fs.existsSync(dest))
        return { ok: false, error: "文件不存在: " + src };
    } else if (/^data:image\//i.test(src)) {
      dest = path.join(dir, hash + ".png");
      if (!fs.existsSync(dest))
        fs.writeFileSync(dest, Buffer.from(src.split(",")[1] || "", "base64"));
    } else {
      const p = path.resolve(src);
      if (!fs.existsSync(p)) return { ok: false, error: "路径不存在: " + src };
      dest = path.join(dir, hash + (path.extname(p) || ""));
      if (!fs.existsSync(dest)) fs.copyFileSync(p, dest);
    }
    return { ok: true, file: pathToFileURL(dest).href };
  } catch (e) {
    return { ok: false, error: e.message };
  }
});

ipcMain.handle("connections:setCurrent", (_, id) => {
  const data = loadConnections();
  if (data.connections.some((c) => c.id === id)) {
    data.currentId = id;
    saveConnections(data);
  }
  try {
    rebuildTrayMenu();
  } catch (e) {}
  return data;
});

async function doWebuiLogin(baseUrl, username, password, extraCookie) {
  const u = baseUrl + "/api/v1/login";
  const headers = {
    "Content-Type": "application/json",
    Accept: "application/json",
  };
  if (extraCookie) headers["Cookie"] = extraCookie;
  const resp = await sessionFetch(u, {
    method: "POST",
    headers,
    body: JSON.stringify({ username, password }),
  });
  // 1) Set-Cookie 直读（重定向未被 session.fetch 自动吞掉时）
  const sc = resp.headers.get("set-cookie") || "";
  let m = /homeagent_session=([^;]+)/.exec(sc);
  // 2) 重定向被自动跟随时 Set-Cookie 可能在中间 302 上：从 cookie jar 兜底
  if (!m) {
    try {
      const { session } = require("electron");
      const hostname = new URL(u).hostname;
      const all = await session.defaultSession.cookies.get({});
      const hit = (all || []).find(
        (c) =>
          c.name === "homeagent_session" &&
          (c.domain || "").replace(/^\./, "").toLowerCase() ===
            hostname.toLowerCase(),
      );
      if (hit) m = [null, hit.value];
    } catch (e) {}
  }
  if (!m) {
    const bodyTxt = await resp.text();
    throw new Error(
      "login ok but no homeagent_session cookie (HTTP " +
        resp.status +
        " " +
        bodyTxt.slice(0, 150) +
        ")",
    );
  }
  return m[1];
}

ipcMain.on("log:r", (_e, m) => {
  log("[r] " + m);
});
ipcMain.handle("log:r", (_e, m) => {
  log("[r] " + m);
  return true;
});

ipcMain.handle(
  "webui:setAuth",
  async (_, { url, cookie, headers, username, password }) => {
    const resp = { ok: true, error: "" };
    let extra = cookie || "";
    try {
      if (username && url) {
        try {
          const tok = await doWebuiLogin(url, username, password, extra);
          extra = extra
            ? extra + "; homeagent_session=" + tok
            : "homeagent_session=" + tok;
        } catch (e) {
          if (extra) {
            log(
              "[setAuth] auto-login skipped (gateway cookie mode): " +
                e.message.slice(0, 120),
            );
          } else {
            throw e;
          }
        }
      } else if (!cookie) {
        await applyAuthRule("", "", {});
        return resp;
      }
    } catch (e) {
      await applyAuthRule(url, extra || "", headers || {});
      resp.ok = false;
      resp.error = e.message;
      log(
        "[setAuth] fail url=" + (url || "") + " err=" + e.message.slice(0, 150),
      );
      return resp;
    }
    await applyAuthRule(url, extra, headers || {});
    log(
      "[setAuth] ok url=" +
        (url || "") +
        " cookieLen=" +
        extra.length +
        " hasTok=" +
        (extra.indexOf("homeagent_session") !== -1),
    );
    return resp;
  },
);

ipcMain.handle("webui:openLogin", (_, { url, username, password }) => {
  if (!url) return { ok: false, error: "no url" };
  const loginWin = new BrowserWindow({
    width: 980,
    height: 740,
    parent: mainWindow,
    modal: true,
    autoHideMenuBar: true,
    title: "HomeAgent - 网关联机登录",
    webPreferences: { contextIsolation: true, nodeIntegration: false },
  });
  const u = encodeURIComponent(username || "");
  const p = encodeURIComponent(password || "");
  let autoFills = 0;
  const tryAutofill = () => {
    if (autoFills > 8) return;
    autoFills++;
    loginWin.webContents
      .executeJavaScript(
        '(function(){var pw=document.querySelector("input[type=password]");' +
          'if(!pw)return false;var f=pw.closest("form")||pw.form;if(!f)return false;' +
          'var ins=f.querySelectorAll("input");var filled=false;' +
          'for(var i=0;i<ins.length;i++){var el=ins[i];var t=(el.type||"").toLowerCase();' +
          'if(t==="password"){if(!el.value)el.value=decodeURIComponent("' +
          p +
          '");}' +
          'else if(t!=="hidden"&&t!=="checkbox"&&t!=="submit"&&t!=="button"){' +
          'if(!el.value)el.value=decodeURIComponent("' +
          u +
          '");}}' +
          'if(f.querySelector("button")){}' +
          'var sb=f.querySelector("input[type=submit],button[type=submit]");' +
          "if(sb&&sb.disabled)return false;" +
          "if(f.requestSubmit)f.requestSubmit();else f.submit();" +
          "return true;})()",
      )
      .then((r) => {
        if (r) log("[openLogin] autofilled gateway login form");
      })
      .catch((e) => log("[openLogin] autofill error: " + e.message));
  };
  loginWin.webContents.on("did-finish-load", () => {
    if (autoFills < 3) tryAutofill();
  });
  loginWin.webContents.on("did-navigate", (_e, u) => {
    log("[openLogin] nav: " + u);
    if (autoFills < 6) setTimeout(tryAutofill, 600);
  });
  loginWin.webContents.on(
    "did-fail-load",
    (_e, code, desc, isMain, failedUrl) => {
      if (isMain)
        log("[openLogin] fail-load " + code + " " + desc + " @ " + failedUrl);
    },
  );
  loginWin
    .loadURL(url)
    .catch((e) => console.error("login window load error:", e.message));
  let grabbed = null;
  loginWin.on("close", async () => {
    if (grabbed) return;
    try {
      const all = await require("electron").session.defaultSession.cookies.get(
        {},
      );
      const host = new URL(url).hostname;
      const keep = (all || []).filter((c) => {
        const d = (c.domain || "").replace(/^\./, "");
        const onHost = host === d || host.endsWith("." + d);
        const onSibling = /(^|\.)jianfgit\.xyz$/i.test(d);
        return onHost || onSibling || /^sl-/.test(c.name || "");
      });
      const list = keep.map((c) => c.name + "=" + c.value);
      // 目标 host 的 sl-session 一并带上（网关会话是 host 级，登录域抓取未必覆盖）
      const sl = await adoptSlSession(url);
      if (sl && list.indexOf(sl) === -1) list.push(sl);
      log(
        "[openLogin] close-grab: host=" +
          host +
          " kept=" +
          keep.length +
          " names=" +
          keep.map((c) => c.name + "@" + (c.domain || "")).join(","),
      );
      grabbed = {
        ok: true,
        cookie: list.join("; "),
        count: keep.length + (sl ? 1 : 0),
      };
    } catch (e) {
      log("[openLogin] close-grab error: " + e.message);
      grabbed = { ok: false, error: e.message };
    }
  });
  loginWin.on("closed", () => {
    if (mainWindow && !mainWindow.isDestroyed() && grabbed) {
      mainWindow.webContents.send("webui:login-result", { ...grabbed, url });
    }
  });
  return { ok: true };
});

// CLI 传输：通过 homed 的 unix socket（逐行 JSON 协议）发起请求。
// 认证行：/auth <apiKey>（若配置了密钥）。返回 JSON 响应行。
ipcMain.handle("cli:request", (_, { socketPath, apiKey, line }) => {
  return new Promise((resolve) => {
    const net = require("net");
    let client;
    try {
      client = net.createConnection({ path: socketPath });
    } catch (e) {
      return resolve({ error: "create connection: " + e.message });
    }
    const timeout = setTimeout(() => {
      try {
        client.destroy();
      } catch (_) {}
      resolve({ error: "timeout waiting for cli response" });
    }, 30000);

    let buf = "";
    const onData = (chunk) => {
      buf += chunk.toString("utf8");
      const idx = buf.indexOf("\n");
      if (idx === -1) return;
      const lineOut = buf.slice(0, idx);
      clearTimeout(timeout);
      try {
        client.destroy();
      } catch (_) {}
      try {
        resolve(JSON.parse(lineOut));
      } catch (e) {
        resolve({ error: "bad response: " + lineOut });
      }
    };
    const onError = (err) => {
      clearTimeout(timeout);
      try {
        client.destroy();
      } catch (_) {}
      resolve({ error: err.message });
    };

    client.on("error", onError);
    client.on("data", onData);
    client.on("connect", () => {
      let next = line;
      if (apiKey) next = "/auth " + apiKey + "\n" + next;
      client.write(next + "\n");
    });
  });
});

// ============ Device Bridge: GUI 作为设备接入 remotedevice 网关 ============
// 复用 net（raw TCP）+ 手写 WS 帧；收到 {op:"cmd"} 用 spawn 在本机执行并回 cmd_result。
const devNet = require("net");
const devTls = require("tls");
const devOs = require("os");

let deviceBridge = null; // 当前活动设备桥
let deviceBridgeId = ""; // 设备 meta device_id（hello 后可用于 cmd_result）
let deviceBridgeAddr = ""; // 设备桥网关地址

// 建立到 remotedevice WS 网关连接，返回 {send(obj), close()}，消息经 onMsg 回调。
function connectDeviceWS(url, token, onMsg) {
  // URL 支持完整端点（含路径/端口/TLS），不硬编码 host/path
  let u;
  try {
    u = new URL(url || "ws://127.0.0.1:9890/api/v1/device/ws");
  } catch (e) {
    return Promise.reject(new Error("invalid device gateway url: " + url));
  }
  const isTLS = u.protocol === "wss:" || u.protocol === "https:";
  const host = u.hostname;
  // https/wss 默认 443，ws/http 默认 9890（remotedevice 默认端口）
  const defaultPort = isTLS ? 443 : 9890;
  const port = u.port ? parseInt(u.port, 10) : defaultPort;
  const basePath = u.pathname || "/api/v1/device/ws";
  const sep = u.search ? "&" : "?";
  const path =
    basePath +
    (u.search || "") +
    sep +
    "token=" +
    encodeURIComponent(token || "");
  return new Promise((resolve, reject) => {
    const sock = isTLS
      ? devTls.connect({ host, port, rejectUnauthorized: false })
      : devNet.createConnection({ host, port });
    sock.on("error", (e) => reject(e));
    sock.on("connect", () => {
      const key = crypto.randomBytes(16).toString("base64");
      // 远程 wss 走 webui 反代需要门户会话 cookie
      // 优先 authRule（连接认证注入）；启动早期 authRule 未就绪时读 connections.json
      let cookieHdr = "";
      try {
        let ck = "";
        // 优先 connections.json 的持久 cookie（完整登录验证过，浏览器快照可能不完整）
        try {
          const conns = loadConnections();
          const curId = conns.currentId;
          const cur =
            conns.connections.find((c) => c.id === curId) ||
            conns.connections[0];
          if (cur && cur.cookie) ck = cur.cookie;
        } catch (e2) {}
        if (!ck && authRule && authRule.cookie) {
          ck = authRule.cookie;
        }
        if (ck) cookieHdr = "Cookie: " + ck + "\r\n";
        console.log("[device-bridge] ws cookie len=" + (ck || "").length);
      } catch (e) {}
      sock.write(
        "GET " +
          path +
          " HTTP/1.1\r\n" +
          "Host: " +
          host +
          ":" +
          port +
          "\r\n" +
          "Upgrade: websocket\r\nConnection: Upgrade\r\n" +
          "Sec-WebSocket-Key: " +
          key +
          "\r\nSec-WebSocket-Version: 13\r\n" +
          cookieHdr +
          "\r\n",
      );
    });
    let buf = Buffer.alloc(0);
    let upgraded = false;
    let opened = false;
    sock.on("data", (chunk) => {
      buf = Buffer.concat([buf, chunk]);
      if (!upgraded) {
        const idx = buf.indexOf("\r\n\r\n");
        if (idx === -1) return;
        const head = buf.slice(0, idx).toString("utf8");
        buf = buf.slice(idx + 4);
        upgraded = true;
        if (!head.includes("101")) {
          // 保留剩余字节（响应体），记录完整响应便于排错
          const body = buf.toString("utf8").slice(0, 800);
          sock.destroy();
          return reject(
            new Error(
              "WS upgrade failed: " +
                head.split("\r\n")[0] +
                " | BODY=" +
                body +
                " | REQ-PATH=" +
                path,
            ),
          );
        }
        opened = true;
        resolve({
          send: (obj) => sendDeviceFrame(sock, JSON.stringify(obj)),
          close: () => sock.destroy(),
        });
      }
      while (buf.length >= 2) {
        const b0 = buf[0];
        const opcode = b0 & 0x0f;
        const b1 = buf[1];
        let len = b1 & 0x7f;
        let off = 2;
        if (len === 126) {
          if (buf.length < 4) break;
          len = buf.readUInt16BE(2);
          off = 4;
        } else if (len === 127) {
          if (buf.length < 10) break;
          len = Number(buf.readBigUInt64BE(2));
          off = 10;
        }
        if (buf.length < off + len) break;
        const payload = buf.slice(off, off + len);
        buf = buf.slice(off + len);
        if (opcode === 0x1) {
          try {
            onMsg(JSON.parse(payload.toString("utf8")));
          } catch (e) {}
        } else if (opcode === 0x8) {
          sock.destroy();
          return;
        }
      }
    });
    sock.on("error", (e) => {
      if (!opened) reject(e);
    });
    sock.on("close", () => {
      deviceBridge = null;
    });
  });
}

// 发送 WS text 帧（客户端加掩码）
function sendDeviceFrame(sock, text) {
  const payload = Buffer.from(text, "utf8");
  const mask = crypto.randomBytes(4);
  const masked = Buffer.from(payload);
  for (let i = 0; i < masked.length; i++) masked[i] ^= mask[i % 4];
  const len = masked.length;
  let hdr;
  if (len < 126) {
    hdr = Buffer.from([0x81, 0x80 | len]);
  } else if (len < 65536) {
    hdr = Buffer.alloc(4);
    hdr[0] = 0x81;
    hdr[1] = 0x80 | 126;
    hdr.writeUInt16BE(len, 2);
  } else {
    hdr = Buffer.alloc(10);
    hdr[0] = 0x81;
    hdr[1] = 0x80 | 127;
    hdr.writeBigUInt64BE(BigInt(len), 2);
  }
  sock.write(Buffer.concat([hdr, mask, masked]));
}

// 处理网关 WS 消息：hello_ack/bind_ack/cmd 等
function onDeviceMsg(msg) {
  if (!msg || typeof msg !== "object") return;
  const op = msg.op || "";
  if (op === "cmd") {
    const command = msg.command || msg.cmd || "";
    const reqId = msg.req_id || msg.id || "";
    if (!command) return;
    // 记录远控活动并刷新托盘菜单
    trayLastCmd = { cmd: command, at: Date.now(), result: "执行中…" };
    trayCmdCount++;
    try {
      rebuildTrayMenu();
    } catch (e) {}
    const cp = require("child_process");
    if (argsSafe(command)) {
      cp.exec(
        command,
        { timeout: 15000, maxBuffer: 8192 },
        (err, stdout, stderr) => {
          const resp = {
            op: "cmd_result",
            req_id: reqId,
            device_id: deviceBridgeId,
            status: err ? "error" : "ok",
            output: (stdout || "") + (stderr || ""),
            error: err ? err.message : "",
          };
          if (deviceBridge && deviceBridge.send) {
            try {
              deviceBridge.send(resp);
            } catch (e) {}
          }
          // 更新结果到托盘
          if (trayLastCmd) {
            trayLastCmd.result =
              resp.status === "ok"
                ? String(resp.output || "").slice(0, 40)
                : "错误: " + String(resp.error || "");
            try {
              rebuildTrayMenu();
            } catch (e) {}
          }
        },
      );
    } else {
      const resp = {
        op: "cmd_result",
        req_id: reqId,
        device_id: deviceBridgeId,
        status: "denied",
        output: "",
        error: "command not allowed",
      };
      if (deviceBridge && deviceBridge.send) {
        try {
          deviceBridge.send(resp);
        } catch (e) {}
      }
      if (trayLastCmd) {
        trayLastCmd.result = "已拒绝(白名单)";
        try {
          rebuildTrayMenu();
        } catch (e) {}
      }
    }
  } else if (op === "hello_ack" || op === "bind_ack") {
    console.log(
      "[device-bridge] " + op + " device=" + (msg.device || deviceBridgeId),
    );
    try {
      rebuildTrayMenu();
    } catch (e) {}
  }
}

// 简单安全校验：拒绝明显危险命令
function argsSafe(cmd) {
  if (!cmd) return false;
  const cmdStr = String(cmd).toLowerCase();
  const bad = [
    "rm -rf",
    "mkfs",
    "dd if=",
    "shutdown",
    "reboot",
    "curl ",
    "wget ",
    ":(){",
    "eval ",
    "su ",
    "sudo ",
  ];
  for (const b of bad) {
    if (cmdStr.indexOf(b) !== -1) return false;
  }
  return true;
}

// 启动设备桥：url/token 来自 gui-prefs.deviceBridge（独立于连接类型）
//   url 可为 ws(s)://完整端点（含路径），token 为网关 ws_token。
async function startDeviceBridge(cfg) {
  if (!cfg) return;
  const url = cfg.url || cfg.gateway || "";
  const token = cfg.apiKey || cfg.token || "";
  if (!url || !token) {
    console.error("[device-bridge] missing url/token, skipped");
    return;
  }
  deviceBridgeAddr = url;
  deviceBridgeId =
    "gui-" + (devOs.hostname() || "local").replace(/[^a-zA-Z0-9_-]/g, "_");
  try {
    const ws = await connectDeviceWS(url, token, onDeviceMsg);
    deviceBridge = ws;
    ws.send({
      op: "hello",
      device: {
        device_id: deviceBridgeId,
        name: "HomeAgent GUI",
        kind: "computer",
        caps: ["status", "cmdrun", "deviceinfo", "cmdresult"],
        info: {
          hostname: devOs.hostname() || "",
          platform: process.platform || "",
          arch: process.arch || "",
          os_release: "", // 不提权读取 /etc/os-release，避免破坏沙箱；如需可在白名单命令里由 agent 探
          node_version:
            process.versions && process.versions.node
              ? process.versions.node
              : "",
          electron_version:
            process.versions && process.versions.electron
              ? process.versions.electron
              : "",
          version: app.getVersion ? app.getVersion() : "",
          cpus: devOs.cpus ? devOs.cpus().length : 0,
          total_mem_bytes: devOs.totalmem ? devOs.totalmem() : 0,
        },
      },
    });
    ws.send({ op: "bind", device_id: deviceBridgeId, token });
    console.log("[device-bridge] connected as " + deviceBridgeId + " @ " + url);
  } catch (e) {
    console.error("[device-bridge] connect failed: " + e.message);
  }
}

function stopDeviceBridge() {
  if (deviceBridge) {
    try {
      deviceBridge.close();
    } catch (e) {}
    deviceBridge = null;
  }
}

// ============ 系统托盘（惰性 + 安全降级） ============
let tray = null;
// 托盘菜单动态数据
const trayLastCmd = null; // 最近一次 device cmd: {cmd, at, result}
const trayCmdCount = 0; // 历史 cmd 总次数
function rebuildTrayMenu() {
  if (!tray) return;
  try {
    const electron = require("electron");
    const TMenu = electron.Menu;
    const tpl = [];
    // 标题
    tpl.push({ label: "HomeAgent", enabled: false });
    tpl.push({ type: "separator" });
    // 远程连接状态
    let connLabel = "未连接";
    let connUrl = "";
    try {
      const conns = loadConnections();
      const cur =
        conns.connections.find((c) => c.id === conns.currentId) ||
        conns.connections[0];
      if (cur) {
        connLabel = cur.name || "未命名";
        connUrl = cur.url || cur.socketPath || "";
      }
    } catch (e) {}
    tpl.push({ label: "后端: " + connLabel, enabled: false });
    if (connUrl) tpl.push({ label: connUrl, enabled: false });
    let online = false;
    try {
      if (authRule && authRule.urlHost) online = true;
    } catch (e) {}
    tpl.push({
      label: online ? "[已连接]" : "[未连接]",
      enabled: false,
    });
    tpl.push({ type: "separator" });
    // 设备桥状态
    if (deviceBridge) {
      tpl.push({ label: "设备桥: [已连接]", enabled: false });
      if (deviceBridgeId)
        tpl.push({ label: "设备ID: " + deviceBridgeId, enabled: false });
      if (deviceBridgeAddr)
        tpl.push({ label: "网关: " + deviceBridgeAddr, enabled: false });
      if (trayLastCmd) {
        tpl.push({ label: "上次远控: " + trayLastCmd.cmd, enabled: false });
        tpl.push({
          label:
            "结果: " +
            (trayLastCmd.result || "…").slice(0, 60) +
            "（" +
            (trayCmdCount || 0) +
            "次总数）",
          enabled: false,
        });
      } else {
        tpl.push({ label: "未收到远控命令", enabled: false });
      }
    } else {
      tpl.push({ label: "设备桥: [未连接]", enabled: false });
    }
    tpl.push({ type: "separator" });
    tpl.push({ label: "显示主界面", click: () => showMainWindow() });
    tpl.push({
      label: "退出",
      click: () => {
        app.isQuitting = true;
        app.quit();
      },
    });
    const tmenu = TMenu.buildFromTemplate(tpl);
    tray.setContextMenu(tmenu);
  } catch (e) {
    console.error("[tray] rebuild failed: " + e.message);
  }
}
function initTray() {
  if (tray) return;
  try {
    const electron = require("electron");
    const Tray = electron.Tray;
    const nImg = electron.nativeImage;
    let img = null;
    const cands = [
      path.join(__dirname, "icon-tray@2x.png"),
      path.join(__dirname, "icon-tray.png"),
      path.join(__dirname, "icon.ico"),
    ];
    for (const c of cands) {
      try {
        const m = nImg.createFromPath(c);
        if (m && !m.isEmpty()) {
          img = m;
          break;
        }
      } catch (e) {}
    }
    if (!img || img.isEmpty()) {
      try {
        img = nImg.createFromPath(path.join(__dirname, "icon-tray.png"));
      } catch (e) {}
    }
    if (!img || img.isEmpty()) {
      img = nImg.createEmpty();
    }
    tray = new Tray(img);
    tray.setToolTip("HomeAgent - 个人智能管家");
    rebuildTrayMenu();
    tray.on("double-click", () => showMainWindow());
    console.log("[tray] READY: " + (tray ? "tray-created" : "null"));
  } catch (e) {
    console.error("[tray] init failed (safe ignore): " + e.message);
    try {
      if (tray) {
        tray.destroy();
        tray = null;
      }
    } catch (e2) {}
  }
}
function showMainWindow() {
  try {
    if (mainWindow && !mainWindow.isDestroyed()) {
      mainWindow.show();
      mainWindow.focus();
    }
  } catch (e) {}
}
function destroyTray() {
  try {
    if (tray) {
      tray.destroy();
      tray = null;
    }
  } catch (e) {}
}

app.whenReady().then(async () => {
  installAuthRule();
  const running = await isServerRunning();
  if (!running) {
    startHomed();
    const started = await waitForServer();
    if (started) {
      console.log("homed started successfully");
    } else {
      console.error("homed failed to start within timeout");
    }
  }
  // 设备桥：由 GUI 偏好（deviceBridge 开关+网关+token）驱动，独立于连接
  try {
    const _prefs = loadGuiPrefs();
    const _db = _prefs.deviceBridge || {};
    if (_db.enabled && _db.gateway && _db.token) {
      startDeviceBridge({ url: _db.gateway, apiKey: _db.token });
    } else if (_db.enabled) {
      console.error(
        "[device-bridge] enabled but missing gateway/token, skipped",
      );
    }
  } catch (e) {
    console.error("device bridge init: " + e.message);
  }
  try {
    initTray();
  } catch (e) {
    console.error("[tray] whenReady call: " + e.message);
  }
  createWindow();
});

app.on("before-quit", () => {
  stopHomed();
  try {
    stopDeviceBridge();
  } catch (e) {}
  try {
    destroyTray();
  } catch (e) {}
});

app.on("window-all-closed", () => {
  // 退出进托盘：依偏好决定（安全降级：无托盘时退出）
  let _exitTray = false;
  try {
    const _p = loadGuiPrefs();
    _exitTray = !!(_p && _p.exitToTray);
  } catch (e) {}
  if (_exitTray && tray) {
    try {
      if (mainWindow && !mainWindow.isDestroyed()) mainWindow.hide();
    } catch (e) {}
  } else {
    app.quit();
  }
});

app.on("activate", () => {
  if (mainWindow === null) {
    createWindow();
  }
});
// IPC：prefs（renderer 设置页需要）
// IPC：本机设备身份（renderer 设备页需要）
ipcMain.handle("device:identity", () =>
  deviceBridge
    ? {
        device_id: deviceBridgeId,
        active: true,
        address: deviceBridgeAddr || "",
      }
    : { device_id: "", active: false, address: "" },
);
const GUI_PREFS_FILE = path.join(app.getPath("userData"), "gui-prefs.json");

function loadGuiPrefs() {
  try {
    if (fs.existsSync(GUI_PREFS_FILE)) {
      const d = JSON.parse(fs.readFileSync(GUI_PREFS_FILE, "utf-8"));
      const db = d.deviceBridge || {};
      return {
        autoLaunch: !!d.autoLaunch,
        silentStart: !!d.silentStart,
        exitToTray: d.exitToTray === undefined ? true : !!d.exitToTray,
        deviceBridge: {
          enabled: !!db.enabled,
          gateway: db.gateway || "",
          token: db.token || "",
        },
      };
    }
  } catch (e) {}
  return {
    autoLaunch: false,
    silentStart: false,
    exitToTray: true,
    deviceBridge: { enabled: false, gateway: "", token: "" },
  };
}

function saveGuiPrefs(p) {
  try {
    fs.writeFileSync(GUI_PREFS_FILE, JSON.stringify(p, null, 2), "utf-8");
  } catch (e) {}
  return p;
}

// 应用开机自启设置（Electron LoginItem）
function applyAutoLaunch(enabled) {
  try {
    app.setLoginItemSettings({
      openAtLogin: enabled,
      openAsHidden: true, // 开机自启时静默（Windows/macOS 支持）
      path: process.execPath,
    });
    return true;
  } catch (e) {
    console.error("setLoginItemSettings failed: " + e.message);
    return false;
  }
}

// 全局偏好缓存（供 createWindow 静默判断使用）
let guiPrefs = loadGuiPrefs();

// IPC：本机设备桥状态（启用+网关+token+连接状态）
ipcMain.handle("device-bridge:get", () => {
  const p = loadGuiPrefs();
  const db = p.deviceBridge || {};
  return {
    enabled: !!db.enabled,
    gateway: db.gateway || "",
    tokenSet: !!(db.token || ""),
    connected: !!deviceBridge,
    deviceId: deviceBridgeId,
    address: deviceBridgeAddr,
  };
});

// IPC：配置本机设备桥（开关+网关+token），保存并动态启停
ipcMain.handle("device-bridge:set", (_, cfg) => {
  const cur = loadGuiPrefs();
  const db = Object.assign({}, cur.deviceBridge || {}, cfg || {});
  const next = Object.assign({}, cur, { deviceBridge: db });
  guiPrefs = saveGuiPrefs(next);
  // 动态应用：停止现有桥，按新配置启动
  try {
    stopDeviceBridge();
  } catch (e) {}
  if (db.enabled && db.gateway && db.token) {
    try {
      startDeviceBridge({ url: db.gateway, apiKey: db.token });
    } catch (e) {
      console.error("[device-bridge] start failed: " + e.message);
    }
  }
  return {
    enabled: !!db.enabled,
    gateway: db.gateway || "",
    connected: !!deviceBridge,
    deviceId: deviceBridgeId,
    address: deviceBridgeAddr,
  };
});

// IPC：读取偏好
ipcMain.handle("prefs:get", () => {
  return loadGuiPrefs();
});
// IPC：写入并应用偏好
ipcMain.handle("prefs:set", (_, p) => {
  const cur = loadGuiPrefs();
  const next = Object.assign({}, cur, p || {});
  if (typeof next.autoLaunch === "boolean") {
    next.autoLaunch = applyAutoLaunch(next.autoLaunch)
      ? next.autoLaunch
      : false;
  }
  guiPrefs = saveGuiPrefs(next);
  return guiPrefs;
});
