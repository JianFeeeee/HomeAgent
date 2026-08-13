const { app, BrowserWindow, ipcMain, dialog, Menu } = require('electron');
const path = require('path');
const fs = require('fs');
const { spawn } = require('child_process');
const http = require('http');

const CONNECTIONS_FILE = path.join(app.getPath('userData'), 'connections.json');

let homedProcess = null;
let mainWindow;

function loadConnections() {
  try {
    if (fs.existsSync(CONNECTIONS_FILE)) {
      const data = JSON.parse(fs.readFileSync(CONNECTIONS_FILE, 'utf-8'));
      normalizeConnections(data);
      return data;
    }
  } catch (e) {
    console.error('Failed to load connections:', e);
    // 配置损坏：备份后重建，避免应用一直处于"无连接"状态
    try {
      const backup = CONNECTIONS_FILE + '.bak';
      fs.copyFileSync(CONNECTIONS_FILE, backup);
      fs.writeFileSync(CONNECTIONS_FILE, '{"connections":[],"currentId":null}', 'utf-8');
      console.error('Backed up corrupt connections to', backup);
    } catch (e2) {
      console.error('Failed to recover connections file:', e2);
    }
  }
  // Fallback: check app resource dir (installer writes fallback copy there)
  try {
    const fallback = path.join(__dirname, 'connections.json');
    if (fs.existsSync(fallback)) {
      const data = JSON.parse(fs.readFileSync(fallback, 'utf-8'));
      normalizeConnections(data);
      saveConnections(data);
      console.log('Imported connections from app resource dir');
      return data;
    }
  } catch (e) {
    console.error('Fallback connections load failed:', e);
  }
  return { connections: [], currentId: null };
}

// 兼容旧数据：缺失的 type 默认为 webui（HTTP）
function normalizeConnections(data) {
  if (!data || !Array.isArray(data.connections)) return;
  data.connections.forEach((c) => {
    if (!c.type) c.type = 'webui';
    if (c.type !== 'cli' && c.type !== 'webui') c.type = 'webui';
  });
}

function saveConnections(data) {
  try {
    fs.writeFileSync(CONNECTIONS_FILE, JSON.stringify(data, null, 2), 'utf-8');
  } catch (e) {
    console.error('Failed to save connections:', e);
  }
}

function findHomed() {
  if (process.platform !== 'win32') return null;
  const exeDir = path.dirname(app.getPath('exe'));
  const p = path.resolve(exeDir, '..', 'homed.exe');
  return fs.existsSync(p) ? p : null;
}

function isServerRunning() {
  return new Promise((resolve) => {
    const req = http.get('http://localhost:8080/', () => resolve(true));
    req.on('error', () => resolve(false));
    req.setTimeout(2000, () => { req.destroy(); resolve(false); });
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
    console.log('homed.exe not found near GUI, skipping auto-launch');
    return;
  }
  const dataDir = path.resolve(path.dirname(homedBin), 'data');
  console.log('Starting homed:', homedBin, '-data', dataDir);
  homedProcess = spawn(homedBin, ['-data', dataDir], {
    stdio: 'ignore',
    detached: false,
    windowsHide: true,
  });
  homedProcess.on('error', (err) => {
    console.error('homed start failed:', err.message);
    homedProcess = null;
  });
  homedProcess.on('exit', (code) => {
    console.log('homed exited with code', code);
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
    title: 'HomeAgent',
    frame: false,
    icon: path.join(__dirname, 'icon.ico'),
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
    },
  });

  mainWindow.loadFile(path.join(__dirname, 'renderer', 'index.html'));

  if (process.argv.includes('--dev')) {
    mainWindow.webContents.openDevTools();
  }

  mainWindow.on('closed', () => {
    mainWindow = null;
  });
}

ipcMain.handle('window:minimize', (e) => {
  BrowserWindow.fromWebContents(e.sender)?.minimize();
});
ipcMain.handle('window:toggleMaximize', (e) => {
  const win = BrowserWindow.fromWebContents(e.sender);
  if (!win) return;
  if (win.isMaximized()) win.unmaximize(); else win.maximize();
});
ipcMain.handle('window:close', (e) => {
  BrowserWindow.fromWebContents(e.sender)?.close();
});

ipcMain.handle('connections:list', () => {
  return loadConnections();
});

ipcMain.handle('connections:add', (_, conn) => {
  const data = loadConnections();
  const id = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
  data.connections.push({
    id,
    name: conn.name,
    url: conn.url || '',
    apiKey: conn.apiKey || '',
    type: conn.type === 'cli' ? 'cli' : 'webui',
    socketPath: conn.socketPath || '',
  });
  if (!data.currentId) data.currentId = id;
  saveConnections(data);
  return data;
});

ipcMain.handle('connections:update', (_, { id, updates }) => {
  const data = loadConnections();
  const idx = data.connections.findIndex(c => c.id === id);
  if (idx !== -1) {
    data.connections[idx] = { ...data.connections[idx], ...updates };
    saveConnections(data);
  }
  return data;
});

ipcMain.handle('connections:delete', (_, id) => {
  const data = loadConnections();
  data.connections = data.connections.filter(c => c.id !== id);
  if (data.currentId === id) {
    data.currentId = data.connections.length > 0 ? data.connections[0].id : null;
  }
  saveConnections(data);
  return data;
});

ipcMain.handle('connections:setCurrent', (_, id) => {
  const data = loadConnections();
  if (data.connections.some(c => c.id === id)) {
    data.currentId = id;
    saveConnections(data);
  }
  return data;
});

// CLI 传输：通过 homed 的 unix socket（逐行 JSON 协议）发起请求。
// 认证行：/auth <apiKey>（若配置了密钥）。返回 JSON 响应行。
ipcMain.handle('cli:request', (_, { socketPath, apiKey, line }) => {
  return new Promise((resolve) => {
    const net = require('net');
    let client;
    try {
      client = net.createConnection({ path: socketPath });
    } catch (e) {
      return resolve({ error: 'create connection: ' + e.message });
    }
    const timeout = setTimeout(() => {
      try { client.destroy(); } catch (_) {}
      resolve({ error: 'timeout waiting for cli response' });
    }, 30000);

    let buf = '';
    const onData = (chunk) => {
      buf += chunk.toString('utf8');
      const idx = buf.indexOf('\n');
      if (idx === -1) return;
      const lineOut = buf.slice(0, idx);
      clearTimeout(timeout);
      try { client.destroy(); } catch (_) {}
      try {
        resolve(JSON.parse(lineOut));
      } catch (e) {
        resolve({ error: 'bad response: ' + lineOut });
      }
    };
    const onError = (err) => {
      clearTimeout(timeout);
      try { client.destroy(); } catch (_) {}
      resolve({ error: err.message });
    };

    client.on('error', onError);
    client.on('data', onData);
    client.on('connect', () => {
      let next = line;
      if (apiKey) next = '/auth ' + apiKey + '\n' + next;
      client.write(next + '\n');
    });
  });
});

app.whenReady().then(async () => {
  const running = await isServerRunning();
  if (!running) {
    startHomed();
    const started = await waitForServer();
    if (started) {
      console.log('homed started successfully');
    } else {
      console.error('homed failed to start within timeout');
    }
  }
  createWindow();
});

app.on('before-quit', stopHomed);

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') app.quit();
});

app.on('activate', () => {
  if (mainWindow === null) {
    createWindow();
  }
});
