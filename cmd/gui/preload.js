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
});
