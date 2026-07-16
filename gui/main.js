const { app, BrowserWindow, ipcMain, dialog } = require('electron');
const path = require('path');
const fs = require('fs');

const CONNECTIONS_FILE = path.join(app.getPath('userData'), 'connections.json');

function loadConnections() {
  try {
    if (fs.existsSync(CONNECTIONS_FILE)) {
      return JSON.parse(fs.readFileSync(CONNECTIONS_FILE, 'utf-8'));
    }
  } catch (e) {
    console.error('Failed to load connections:', e);
  }
  return { connections: [], currentId: null };
}

function saveConnections(data) {
  try {
    fs.writeFileSync(CONNECTIONS_FILE, JSON.stringify(data, null, 2), 'utf-8');
  } catch (e) {
    console.error('Failed to save connections:', e);
  }
}

let mainWindow;

function createWindow() {
  mainWindow = new BrowserWindow({
    width: 1280,
    height: 860,
    minWidth: 900,
    minHeight: 600,
    title: 'HomeAgent',
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

ipcMain.handle('connections:list', () => {
  return loadConnections();
});

ipcMain.handle('connections:add', (_, conn) => {
  const data = loadConnections();
  const id = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
  data.connections.push({ id, name: conn.name, url: conn.url, apiKey: conn.apiKey });
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

app.whenReady().then(createWindow);

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') app.quit();
});

app.on('activate', () => {
  if (mainWindow === null) createWindow();
});
