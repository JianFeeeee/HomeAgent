const fs = require('fs');
const path = require('path');
const { execSync } = require('child_process');

// ---- Utility ----
function writeJSON(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}
function sendError(id, code, message) {
  writeJSON({ jsonrpc: '2.0', id, error: { code, message } });
}
function notify(method, params) {
  writeJSON({ jsonrpc: '2.0', method, params });
}
function readJSON(file) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch (e) { return null; }
}

// ---- Plugin registry ----
const loadedPlugins = {}; // name -> { entry, tools: [{name, execute, ...}] }
const allTools = [];      // flat list of all tools across all plugins

function registerPluginTools(name, tools, api) {
  for (const t of tools) {
    if (t && t.name) {
      t._plugin = name;
      allTools.push(t);
      notify('register', { type: 'tool', data: { name: t.name, description: t.description, parameters: t.parameters } });
    }
  }
  loadedPlugins[name] = { tools, api };
}

function loadPlugin(pluginDir, name) {
  // Resolve entry
  const pkg = readJSON(path.join(pluginDir, 'package.json'));
  let entryPath = null;

  if (pkg && pkg.openclaw) {
    let raw = pkg.openclaw.runtimeExtensions || pkg.openclaw.extensions;
    if (typeof raw === 'string') raw = [raw];
    if (Array.isArray(raw)) {
      for (const ext of raw) {
        let ep = path.resolve(pluginDir, ext);
        if (ep.endsWith('.ts')) { const js = ep.replace(/\.ts$/, '.js'); if (fs.existsSync(js)) { entryPath = js; break; } }
        if (fs.existsSync(ep)) { entryPath = ep; break; }
      }
    }
  }

  if (!entryPath) {
    const manifest = readJSON(path.join(pluginDir, 'openclaw.plugin.json'));
    if (manifest) {
      const ep = manifest.entry || manifest.main || 'index.js';
      entryPath = path.join(pluginDir, ep);
    }
  }

  if (!entryPath) entryPath = path.join(pluginDir, 'index.js');
  if (!fs.existsSync(entryPath)) {
    process.stderr.write(`[manager] entry not found for ${name}: ${entryPath}\n`);
    return false;
  }

  let pluginEntry;
  try { pluginEntry = require(entryPath); } catch (e) {
    process.stderr.write(`[manager] load ${name}: ${e.message}\n`);
    return false;
  }

  const entry = pluginEntry.default || pluginEntry;
  if (typeof entry !== 'object' || typeof entry.register !== 'function') {
    process.stderr.write(`[manager] ${name}: entry must export {register(api)}\n`);
    return false;
  }

  const registeredTools = [];
  const api = {
    id: name,
    name,
    version: (pkg && pkg.version) || '1.0.0',
    description: (pkg && pkg.description) || '',
    source: pluginDir,
    rootDir: pluginDir,
    config: {},
    pluginConfig: {},
    registrationMode: 'full',
    logger: { debug: () => {}, info: () => {}, warn: () => {}, error: (...args) => process.stderr.write(`[${name}] ${args.join(' ')}\n`) },
    resolvePath: (p) => path.resolve(pluginDir, p),
    registerTool: (def, opts) => {
      if (typeof def === 'function') {
        const toolCtx = { id: name, cwd: pluginDir, env: process.env, allow: ['*'] };
        const result = def(toolCtx);
        const tools = Array.isArray(result) ? result : [result];
        for (const t of tools) { if (t && typeof t.execute === 'function') registeredTools.push(t); }
        return;
      }
      if (!def || !def.name) return;
      registeredTools.push({ name: def.name, label: def.label || def.name, description: def.description || '', parameters: def.parameters || { type: 'object', properties: {} }, execute: typeof def.execute === 'function' ? def.execute : undefined });
    },
    registerProvider: (p) => notify('register', { type: 'provider', data: { name: p.name } }),
    registerChannel: (ch) => notify('register', { type: 'channel', data: { name: ch.name, type: ch.type } }),
    registerHook: (hook) => notify('register', { type: 'hook', data: { name: hook.name, event: hook.event } }),
    registerHttpRoute: (route) => notify('register', { type: 'http_route', data: { path: route.path, method: route.method } }),
    registerCommand: (cmd) => notify('register', { type: 'command', data: { name: cmd.name, description: cmd.description } }),
    registerService: (svc) => notify('register', { type: 'service', data: { name: svc.name } }),
    registerImageGenerationProvider: (p) => notify('register', { type: 'image_generation_provider', data: { name: p.name } }),
    registerWebFetchProvider: (p) => notify('register', { type: 'web_fetch_provider', data: { name: p.name } }),
    registerWebSearchProvider: (p) => notify('register', { type: 'web_search_provider', data: { name: p.name } }),
    start: (cb) => {},
    stop: (cb) => {},
  };

  entry.register(api);
  registerPluginTools(name, registeredTools, api);
  process.stderr.write(`[manager] loaded plugin: ${name} (${registeredTools.length} tools)\n`);
  return true;
}

// ---- Install npm package ----
function installNPMPackage(spec, skillsDir) {
  process.stderr.write(`[manager] installing: ${spec}\n`);
  const installDir = path.join(skillsDir, '.npm_install_' + Date.now());
  fs.mkdirSync(installDir, { recursive: true });

  try {
    execSync(`npm install ${spec} --no-save --prefix "${installDir}"`, {
      cwd: installDir, stdio: ['pipe', 'pipe', 'pipe'],
      timeout: 120000, env: { ...process.env, NODE_PATH: path.join(installDir, 'node_modules') }
    });
  } catch (e) {
    fs.rmSync(installDir, { recursive: true, force: true });
    return { error: e.stderr ? e.stderr.toString() : e.message };
  }

  const nm = path.join(installDir, 'node_modules');
  if (!fs.existsSync(nm)) {
    fs.rmSync(installDir, { recursive: true, force: true });
    return { error: 'node_modules not created' };
  }

  let foundPluginDir = null;
  let foundName = null;

  const entries = fs.readdirSync(nm);
  for (const entry of entries) {
    const dir = path.join(nm, entry);
    if (!fs.statSync(dir).isDirectory()) continue;

    if (entry.startsWith('@')) {
      const subs = fs.readdirSync(dir);
      for (const sub of subs) {
        const subDir = path.join(dir, sub);
        if (fs.existsSync(path.join(subDir, 'openclaw.plugin.json')) ||
            (fs.existsSync(path.join(subDir, 'package.json')) && readJSON(path.join(subDir, 'package.json'))?.openclaw)) {
          foundPluginDir = subDir;
          foundName = entry + '/' + sub;
        }
      }
      continue;
    }

    if (fs.existsSync(path.join(dir, 'openclaw.plugin.json')) ||
        (fs.existsSync(path.join(dir, 'package.json')) && readJSON(path.join(dir, 'package.json'))?.openclaw)) {
      foundPluginDir = dir;
      foundName = entry;
    }
  }

  if (!foundPluginDir) {
    fs.rmSync(installDir, { recursive: true, force: true });
    return { error: `no OC plugin found in installed package "${spec}"` };
  }

  // Copy to skills dir
  const targetDir = path.join(skillsDir, foundName);
  if (fs.existsSync(targetDir)) fs.rmSync(targetDir, { recursive: true, force: true });
  cpSync(foundPluginDir, targetDir);
  fs.rmSync(installDir, { recursive: true, force: true });

  return { name: foundName, dir: targetDir };
}

function cpSync(src, dst) {
  fs.mkdirSync(dst, { recursive: true });
  for (const entry of fs.readdirSync(src)) {
    const s = path.join(src, entry);
    const d = path.join(dst, entry);
    if (fs.statSync(s).isDirectory()) {
      cpSync(s, d);
    } else {
      fs.copyFileSync(s, d);
    }
  }
}

// ---- Main ----
const args = process.argv.slice(2);
if (args.length < 1) {
  process.stderr.write('[manager] usage: node main.js <skills-dir>\n');
  process.exit(1);
}

const skillsDir = path.resolve(args[0]);

// Load existing plugins on startup
process.stderr.write(`[manager] scanning: ${skillsDir}\n`);
if (fs.existsSync(skillsDir)) {
  for (const entry of fs.readdirSync(skillsDir)) {
    if (entry.startsWith('.')) continue; // skip hidden
    const pluginDir = path.join(skillsDir, entry);
    if (!fs.statSync(pluginDir).isDirectory()) continue;
    if (fs.existsSync(path.join(pluginDir, 'main.js')) || fs.existsSync(path.join(pluginDir, 'main.py'))) {
      process.stderr.write(`[manager] skip non-OC plugin: ${entry} (main.js/main.py)\n`);
      continue;
    }
    if (fs.existsSync(path.join(pluginDir, 'openclaw.plugin.json')) ||
        (fs.existsSync(path.join(pluginDir, 'package.json')) && readJSON(path.join(pluginDir, 'package.json'))?.openclaw)) {
      loadPlugin(pluginDir, entry);
    }
  }
}

// ---- JSON-RPC ----
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin, output: process.stdout, terminal: false });

rl.on('line', async (line) => {
  let req;
  try { req = JSON.parse(line); } catch { sendError(null, -32700, 'Parse error'); return; }

  const id = req.id;
  const method = req.method;

  if (method === 'ping') {
    writeJSON({ jsonrpc: '2.0', id, result: { status: 'ok', plugins: Object.keys(loadedPlugins).length } });
    return;
  }

  if (method === 'plugins/list') {
    const list = Object.entries(loadedPlugins).map(([name, p]) => ({
      name,
      tools: p.tools.map(t => ({ name: t.name, description: t.description })),
    }));
    writeJSON({ jsonrpc: '2.0', id, result: { plugins: list } });
    return;
  }

  if (method === 'plugins/install') {
    const pkg = req.params?.package;
    if (!pkg) { sendError(id, -32602, 'package required'); return; }

    const result = installNPMPackage(pkg, skillsDir);
    if (result.error) {
      sendError(id, -32603, result.error);
      return;
    }

    // Load the newly installed plugin
    const ok = loadPlugin(result.dir, result.name);
    if (!ok) {
      sendError(id, -32603, `failed to load installed plugin: ${result.name}`);
      return;
    }

    writeJSON({ jsonrpc: '2.0', id, result: { name: result.name, tools: loadedPlugins[result.name].tools.map(t => t.name) } });
    return;
  }

  if (method === 'plugins/uninstall') {
    const name = req.params?.name;
    if (!name) { sendError(id, -32602, 'name required'); return; }

    if (!loadedPlugins[name]) { sendError(id, -32601, `plugin not found: ${name}`); return; }

    // Remove tools
    const idxs = [];
    for (let i = allTools.length - 1; i >= 0; i--) {
      if (allTools[i]._plugin === name) allTools.splice(i, 1);
    }
    delete loadedPlugins[name];

    // Remove directory
    const pluginDir = path.join(skillsDir, name);
    if (fs.existsSync(pluginDir)) fs.rmSync(pluginDir, { recursive: true, force: true });

    writeJSON({ jsonrpc: '2.0', id, result: { status: 'uninstalled', name } });
    return;
  }

  if (method === 'tools/list') {
    const tools = allTools.map(t => ({
      name: t.name,
      description: t.description || '',
      inputSchema: t.parameters || { type: 'object', properties: {} },
    }));
    writeJSON({ jsonrpc: '2.0', id, result: { tools } });
    return;
  }

  if (method === 'tools/call') {
    const params = req.params || {};
    const toolName = params.name;
    const args = params.arguments || {};

    const tool = allTools.find(t => t.name === toolName);
    if (!tool) { sendError(id, -32601, `Tool not found: ${toolName}`); return; }
    if (typeof tool.execute !== 'function') { sendError(id, -32603, `Tool ${toolName} has no execute`); return; }

    try {
      const result = await tool.execute('mgr-call-1', args, undefined, undefined);
      if (result && typeof result === 'object' && Array.isArray(result.content)) {
        writeJSON({ jsonrpc: '2.0', id, result });
      } else {
        const text = typeof result === 'string' ? result : JSON.stringify(result);
        writeJSON({ jsonrpc: '2.0', id, result: { content: [{ type: 'text', text }] } });
      }
    } catch (e) { sendError(id, -32603, e.message); }
    return;
  }

  sendError(id, -32601, `Method not found: ${method}`);
});
