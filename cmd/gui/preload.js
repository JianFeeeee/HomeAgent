const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('homeagent', {
  connections: {
    list: () => ipcRenderer.invoke('connections:list'),
    add: (conn) => ipcRenderer.invoke('connections:add', conn),
    update: (id, updates) => ipcRenderer.invoke('connections:update', { id, updates }),
    delete: (id) => ipcRenderer.invoke('connections:delete', id),
    setCurrent: (id) => ipcRenderer.invoke('connections:setCurrent', id),
  },
});
