const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('homeagent', {
  win: {
    minimize: () => ipcRenderer.invoke('window:minimize'),
    toggleMaximize: () => ipcRenderer.invoke('window:toggleMaximize'),
    close: () => ipcRenderer.invoke('window:close'),
  },
  connections: {
    list: () => ipcRenderer.invoke('connections:list'),
    add: (conn) => ipcRenderer.invoke('connections:add', conn),
    update: (id, updates) => ipcRenderer.invoke('connections:update', { id, updates }),
    delete: (id) => ipcRenderer.invoke('connections:delete', id),
    setCurrent: (id) => ipcRenderer.invoke('connections:setCurrent', id),
  },
  cli: {
    request: (socketPath, apiKey, line) => ipcRenderer.invoke('cli:request', { socketPath, apiKey, line }),
  },
  webui: {
    setAuth: (url, cookie, headers, username, password) => ipcRenderer.invoke('webui:setAuth', { url, cookie, headers, username, password }),
    openLogin: (url, username, password) => ipcRenderer.invoke('webui:openLogin', { url, username, password }),
    onLoginResult: (cb) => ipcRenderer.on('webui:login-result', (_e, d) => cb(d)),
    cacheBg: (src) => ipcRenderer.invoke('bg:cache', { src }),
    log: (m) => ipcRenderer.send('log:r', m),
  },
});
