const { app, BrowserWindow, ipcMain, dialog, Menu } = require("electron");
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
  BrowserWindow.fromWebContents(e.sender)?.close();
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
  return data;
});

ipcMain.handle("connections:update", (_, { id, updates }) => {
  const data = loadConnections();
  const idx = data.connections.findIndex((c) => c.id === id);
  if (idx !== -1) {
    data.connections[idx] = { ...data.connections[idx], ...updates };
    saveConnections(data);
  }
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
  createWindow();
});

app.on("before-quit", stopHomed);

app.on("window-all-closed", () => {
  if (process.platform !== "darwin") app.quit();
});

app.on("activate", () => {
  if (mainWindow === null) {
    createWindow();
  }
});
