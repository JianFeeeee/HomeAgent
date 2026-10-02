const { contextBridge, ipcRenderer } = require("electron");

contextBridge.exposeInMainWorld("homeagent", {
  win: {
    minimize: () => ipcRenderer.invoke("window:minimize"),
    toggleMaximize: () => ipcRenderer.invoke("window:toggleMaximize"),
    close: () => ipcRenderer.invoke("window:close"),
  },
  connections: {
    list: () => ipcRenderer.invoke("connections:list"),
    add: (conn) => ipcRenderer.invoke("connections:add", conn),
    update: (id, updates) =>
      ipcRenderer.invoke("connections:update", { id, updates }),
    delete: (id) => ipcRenderer.invoke("connections:delete", id),
    setCurrent: (id) => ipcRenderer.invoke("connections:setCurrent", id),
  },
  cli: {
    request: (socketPath, apiKey, line) =>
      ipcRenderer.invoke("cli:request", { socketPath, apiKey, line }),
  },
  webui: {
    setAuth: (url, cookie, headers, username, password) =>
      ipcRenderer.invoke("webui:setAuth", {
        url,
        cookie,
        headers,
        username,
        password,
      }),
    openLogin: (url, username, password) =>
      ipcRenderer.invoke("webui:openLogin", { url, username, password }),
    onLoginResult: (cb) =>
      ipcRenderer.on("webui:login-result", (_e, d) => cb(d)),
  },
  cacheBg: (src) => ipcRenderer.invoke("bg:cache", { src }),
  log: (m) => ipcRenderer.invoke("log:r", m),
  notify: {
    show: (payload) => ipcRenderer.invoke("notify:show", payload),
    supported: () => ipcRenderer.invoke("notify:supported"),
    onClicked: (cb) => ipcRenderer.on("notify:clicked", (_e, d) => cb(d)),
  },
  prefs: {
    get: () => ipcRenderer.invoke("prefs:get"),
    set: (p) => ipcRenderer.invoke("prefs:set", p),
  },
  device: {
    identity: () => ipcRenderer.invoke("device:identity"),
  },
  deviceBridge: {
    get: () => ipcRenderer.invoke("device-bridge:get"),
    set: (cfg) => ipcRenderer.invoke("device-bridge:set", cfg),
    setAuthorized: (auth) =>
      ipcRenderer.invoke("device-bridge:setAuthorized", auth),
  },
  displays: {
    list: () => ipcRenderer.invoke("displays:list"),
  },
  audio: {
    list: () => ipcRenderer.invoke("audio:list"),
  },
});
