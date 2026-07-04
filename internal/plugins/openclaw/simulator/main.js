const fs = require('fs');
const path = require('path');

// ---- 工具函数 ----
function writeJSON(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}

function sendError(id, code, message) {
  writeJSON({ jsonrpc: '2.0', id, error: { code, message } });
}

function readJSON(file) {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (e) {
    return null;
  }
}

// ---- 解析插件入口 ----
const pluginDir = path.resolve(process.argv[2]);
if (!pluginDir) {
  process.stderr.write('[simulator] usage: node main.js <plugin-dir>\n');
  process.exit(1);
}

// 1. 先读 package.json 找 extensions
const pkgPath = path.join(pluginDir, 'package.json');
const pkg = readJSON(pkgPath);
let entryPath = null;

if (pkg && pkg.openclaw) {
  // runtimeExtensions > extensions (安装包首选编译后的 JS)
  let raw = pkg.openclaw.runtimeExtensions || pkg.openclaw.extensions;
  if (typeof raw === 'string') raw = [raw];
  if (Array.isArray(raw) && raw.length > 0) {
    // 优先选已编译的 JS 入口: .ts 映射到 .js, .js 直接用
    for (const ext of raw) {
      let ep = path.resolve(pluginDir, ext);
      // .ts → 同级 .js
      if (ep.endsWith('.ts')) {
        const jsEp = ep.replace(/\.ts$/, '.js');
        if (fs.existsSync(jsEp)) { entryPath = jsEp; break; }
      }
      if (fs.existsSync(ep)) { entryPath = ep; break; }
    }
  }
}

// 2. 回退: openclaw.plugin.json 的 entry/main
if (!entryPath) {
  const manifest = readJSON(path.join(pluginDir, 'openclaw.plugin.json'));
  if (manifest) {
    const ep = manifest.entry || manifest.main || 'index.js';
    entryPath = path.join(pluginDir, ep);
  }
}

// 3. 最后尝试 index.js
if (!entryPath) {
  entryPath = path.join(pluginDir, 'index.js');
}

if (!fs.existsSync(entryPath)) {
  process.stderr.write(`[simulator] entry not found: ${entryPath}\n`);
  process.exit(1);
}

// ---- 加载插件 ----
let pluginEntry;
try {
  pluginEntry = require(entryPath);
} catch (e) {
  process.stderr.write(`[simulator] load plugin: ${e.message}\n`);
  process.exit(1);
}

const entry = pluginEntry.default || pluginEntry;

if (typeof entry !== 'object' || typeof entry.register !== 'function') {
  process.stderr.write(`[simulator] plugin entry must export {default: {register(api)}}\n`);
  process.exit(1);
}

// ---- 构造完整的 PluginApi 模拟 ----
const registeredTools = [];

// registerTool 支持两种签名:
//   api.registerTool(toolDef, opts?)          — 对象形式
//   api.registerTool(factory, opts?)           — 工厂函数形式
function registerTool(defOrFactory, opts) {
  if (typeof defOrFactory === 'function') {
    // 工厂形式: 传入 toolContext, 返回工具对象或数组
    const toolCtx = {
      id: 'simulator',
      cwd: pluginDir,
      env: process.env,
      allow: ['*'],
    };
    const result = defOrFactory(toolCtx);
    const tools = Array.isArray(result) ? result : [result];
    for (const t of tools) {
      if (t && typeof t.execute === 'function') {
        registeredTools.push(t);
      }
    }
    return;
  }

  // 对象形式
  const def = defOrFactory;
  if (!def || !def.name) return;

  // definePluginEntry 的 register 传给 api.registerTool 时是完整工具定义
  // defineToolPlugin 包装后传给 api.registerTool 的也是完整工具定义
  // 关键是工具必须要有 execute 函数（或 factory 在之前展开）
  registeredTools.push({
    name: def.name,
    label: def.label || def.name,
    description: def.description || '',
    parameters: def.parameters || { type: 'object', properties: {} },
    execute: typeof def.execute === 'function' ? def.execute : undefined,
  });
}

// 完整的 OpenClawPluginApi 模拟
const api = {
  id: entry.id || 'unknown',
  name: entry.name || 'Unknown',
  version: entry.version,
  description: entry.description,
  source: pluginDir,
  rootDir: pluginDir,
  config: {},
  pluginConfig: {},
  registrationMode: 'full',
  logger: {
    debug: (...args) => {},
    info: (...args) => {},
    warn: (...args) => {},
    error: (...args) => process.stderr.write(`[plugin] ${args.join(' ')}\n`),
  },
  resolvePath: (p) => path.resolve(pluginDir, p),

  // 工具注册
  registerTool,

  // 以下 api 方法留为 no-op，保证真实插件调用时不崩溃
  registerProvider: () => {},
  registerChannel: () => {},
  registerEmbeddingProvider: () => {},
  registerSpeechProvider: () => {},
  registerRealtimeTranscriptionProvider: () => {},
  registerRealtimeVoiceProvider: () => {},
  registerMediaUnderstandingProvider: () => {},
  registerImageGenerationProvider: () => {},
  registerMusicGenerationProvider: () => {},
  registerVideoGenerationProvider: () => {},
  registerWebFetchProvider: () => {},
  registerWebSearchProvider: () => {},
  registerMemoryEmbeddingProvider: () => {},
  registerAgentHarness: () => {},
  registerCliBackend: () => {},
  registerHook: () => {},
  registerHttpRoute: () => {},
  registerGatewayMethod: () => {},
  registerGatewayDiscoveryService: () => {},
  registerCli: () => {},
  registerNodeCliFeature: () => {},
  registerService: () => {},
  registerCommand: () => {},
  registerInteractiveHandler: () => {},
  registerAgentToolResultMiddleware: () => {},
  registerTrustedToolPolicy: () => {},
  registerToolMetadata: () => {},
  registerContextEngine: () => {},
  registerMemoryCapability: () => {},
  registerMemoryPromptSection: () => {},
  registerMemoryFlushPlan: () => {},
  registerMemoryRuntime: () => {},
  registerMemoryPromptSupplement: () => {},
  registerMemoryCorpusSupplement: () => {},

  // 会话相关
  on: () => {},
  onConversationBindingResolved: () => {},

  session: {
    state: { registerSessionExtension: () => {} },
    workflow: {
      enqueueNextTurnInjection: () => {},
      registerSessionSchedulerJob: () => {},
      sendSessionAttachment: () => {},
      scheduleSessionTurn: () => {},
      unscheduleSessionTurnsByTag: () => {},
    },
    controls: {
      registerControlUiDescriptor: () => {},
      registerSessionAction: () => {},
    },
  },

  agent: {
    events: {
      registerAgentEventSubscription: () => {},
      emitAgentEvent: () => {},
    },
  },

  lifecycle: { registerRuntimeLifecycle: () => {} },

  runContext: {
    setRunContext: () => {},
    getRunContext: () => ({}),
    clearRunContext: () => {},
  },

  runtime: {},
};

// ---- 注册插件 ----
entry.register(api);

// ---- JSON-RPC 协议处理 ----
const readline = require('readline');
const rl = readline.createInterface({
  input: process.stdin,
  output: process.stdout,
  terminal: false,
});

rl.on('line', async (line) => {
  let req;
  try {
    req = JSON.parse(line);
  } catch {
    sendError(null, -32700, 'Parse error');
    return;
  }

  const id = req.id;
  const method = req.method;

  if (method === 'ping') {
    writeJSON({ jsonrpc: '2.0', id, result: { status: 'ok' } });
    return;
  }

  if (method === 'tools/list') {
    const tools = registeredTools.map(t => ({
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

    const tool = registeredTools.find(t => t.name === toolName);
    if (!tool) {
      sendError(id, -32601, `Tool not found: ${toolName}`);
      return;
    }

    if (typeof tool.execute !== 'function') {
      sendError(id, -32603, `Tool ${toolName} has no execute function`);
      return;
    }

    try {
      // OpenClaw 工具 execute 签名: (toolCallId, params, signal, onUpdate) => AgentToolResult
      const result = await tool.execute('sim-call-1', args, undefined, undefined);
      // 如果返回已经是 AgentToolResult 格式，直接转发
      if (result && typeof result === 'object' && Array.isArray(result.content)) {
        writeJSON({ jsonrpc: '2.0', id, result });
      } else {
        // 否则包装为 text result
        const text = typeof result === 'string' ? result : JSON.stringify(result);
        writeJSON({ jsonrpc: '2.0', id, result: { content: [{ type: 'text', text }] } });
      }
    } catch (e) {
      sendError(id, -32603, e.message);
    }
    return;
  }

  sendError(id, -32601, `Method not found: ${method}`);
});
