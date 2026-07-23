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

function notify(method, params) {
  writeJSON({ jsonrpc: '2.0', method, params });
}

// ---- 解析插件入口 ----
const pluginDir = path.resolve(process.argv[2]);
if (!pluginDir) {
  process.stderr.write('[simulator] usage: node main.js <plugin-dir>\n');
  process.exit(1);
}

const pkgPath = path.join(pluginDir, 'package.json');
const pkg = readJSON(pkgPath);
let entryPath = null;

if (pkg && pkg.openclaw) {
  let raw = pkg.openclaw.runtimeExtensions || pkg.openclaw.extensions;
  if (typeof raw === 'string') raw = [raw];
  if (Array.isArray(raw) && raw.length > 0) {
    for (const ext of raw) {
      let ep = path.resolve(pluginDir, ext);
      if (ep.endsWith('.ts')) {
        const jsEp = ep.replace(/\.ts$/, '.js');
        if (fs.existsSync(jsEp)) { entryPath = jsEp; break; }
      }
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

// ---- 注册工具（本地存储，供 tools/list 和 tools/call 用） ----
const registeredTools = [];

// ---- Provider 存储（供 provider/call 用） ----
const registeredProviders = {}; // type -> { name, instance }

// ---- Channel 存储（供 tools/call 中通道输出路由用） ----
const registeredChannels = {}; // name -> { output, send, channelPlugin, type }

function registerTool(defOrFactory, opts) {
  if (typeof defOrFactory === 'function') {
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
        notify('register', { type: 'tool', data: { name: t.name, description: t.description, parameters: t.parameters } });
      }
    }
    return;
  }

  const def = defOrFactory;
  if (!def || !def.name) return;

  registeredTools.push({
    name: def.name,
    label: def.label || def.name,
    description: def.description || '',
    parameters: def.parameters || { type: 'object', properties: {} },
    execute: typeof def.execute === 'function' ? def.execute : undefined,
  });
  notify('register', { type: 'tool', data: { name: def.name, label: def.label, description: def.description, parameters: def.parameters } });
}

// ---- 构造完整的 OpenClawPluginApi ----
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

  // ---- 工具注册 ----
  registerTool,

  // ---- Provider 注册（同时存储实例，支持 provider/call） ----
  registerProvider: (provider) => {
    if (provider && provider.id) registeredProviders['llm'] = { name: provider.id, instance: provider };
    notify('register', { type: 'provider', data: { name: provider?.id || provider?.name, description: provider?.description } });
  },
  registerEmbeddingProvider: (p) => {
    if (p) registeredProviders['embedding'] = { name: p.name, instance: p };
    notify('register', { type: 'embedding_provider', data: { name: p?.name } });
  },
  registerSpeechProvider: (p) => {
    if (p) registeredProviders['speech'] = { name: p.name, instance: p };
    notify('register', { type: 'speech_provider', data: { name: p?.name } });
  },
  registerRealtimeTranscriptionProvider: (p) => {
    if (p) registeredProviders['realtime_transcription'] = { name: p.name, instance: p };
    notify('register', { type: 'realtime_transcription_provider', data: { name: p?.name } });
  },
  registerRealtimeVoiceProvider: (p) => {
    if (p) registeredProviders['realtime_voice'] = { name: p.name, instance: p };
    notify('register', { type: 'realtime_voice_provider', data: { name: p?.name } });
  },
  registerMediaUnderstandingProvider: (p) => {
    if (p) registeredProviders['media_understanding'] = { name: p.name, instance: p };
    notify('register', { type: 'media_understanding_provider', data: { name: p?.name } });
  },
  registerImageGenerationProvider: (p) => {
    if (p) registeredProviders['image_generation'] = { name: p.name, instance: p };
    notify('register', { type: 'image_generation_provider', data: { name: p?.name } });
  },
  registerMusicGenerationProvider: (p) => {
    if (p) registeredProviders['music_generation'] = { name: p.name, instance: p };
    notify('register', { type: 'music_generation_provider', data: { name: p?.name } });
  },
  registerVideoGenerationProvider: (p) => {
    if (p) registeredProviders['video_generation'] = { name: p.name, instance: p };
    notify('register', { type: 'video_generation_provider', data: { name: p?.name } });
  },
  registerWebFetchProvider: (p) => {
    if (p) registeredProviders['web_fetch'] = { name: p.name, instance: p };
    notify('register', { type: 'web_fetch_provider', data: { name: p?.name } });
  },
  registerWebSearchProvider: (p) => {
    if (p) registeredProviders['web_search'] = { name: p.name, instance: p };
    notify('register', { type: 'web_search_provider', data: { name: p?.name } });
  },
  registerMemoryEmbeddingProvider: (p) => {
    if (p) registeredProviders['memory_embedding'] = { name: p.name, instance: p };
    notify('register', { type: 'memory_embedding_provider', data: { name: p?.name } });
  },

  // ---- Channel 注册 ----
  registerChannel: (ch) => {
    let chName = ch.name;
    let chType = ch.type || 'text';
    const chPlugin = ch.plugin;
    if (chPlugin && typeof chPlugin === 'object') {
      registeredChannels[chName] = { channelPlugin: chPlugin, type: chType };
    } else {
      registeredChannels[chName] = { output: ch.output, send: ch.send, type: chType };
    }
    notify('register', { type: 'channel', data: { name: chName, type: chType } });
  },

  // ---- 输入提交（submitInput -> channel_input notification） ----
  submitInput: (msg) => {
    notify('channel_input', { channel: api.name, payload: msg });
  },

  // ---- Hook / 生命周期 ----
  registerHook: (hook) => notify('register', { type: 'hook', data: { name: hook.name, event: hook.event } }),
  registerRuntimeLifecycle: (lc) => notify('register', { type: 'runtime_lifecycle', data: { name: lc.name } }),

  // ---- HTTP 路由 ----
  registerHttpRoute: (route) => notify('register', { type: 'http_route', data: { path: route.path, method: route.method } }),

  // ---- CLI 命令 ----
  registerCommand: (cmd) => notify('register', { type: 'command', data: { name: cmd.name, description: cmd.description } }),
  registerCli: (cli) => notify('register', { type: 'cli', data: { name: cli.name } }),
  registerCliBackend: (cb) => notify('register', { type: 'cli_backend', data: { name: cb.name } }),
  registerNodeCliFeature: (f) => notify('register', { type: 'node_cli_feature', data: { name: f.name } }),

  // ---- Service ----
  registerService: (svc) => notify('register', { type: 'service', data: { name: svc.name } }),

  // ---- Agent 相关 ----
  registerAgentHarness: (h) => notify('register', { type: 'agent_harness', data: { name: h.name } }),
  registerAgentToolResultMiddleware: (m) => notify('register', { type: 'agent_tool_result_middleware', data: {} }),
  registerInteractiveHandler: (h) => notify('register', { type: 'interactive_handler', data: { name: h.name } }),

  // ---- Gateway ----
  registerGatewayMethod: (gm) => notify('register', { type: 'gateway_method', data: { name: gm.name } }),
  registerGatewayDiscoveryService: (gs) => notify('register', { type: 'gateway_discovery_service', data: { name: gs.name } }),

  // ---- Trust & Metadata ----
  registerTrustedToolPolicy: (p) => notify('register', { type: 'trusted_tool_policy', data: { name: p.name } }),
  registerToolMetadata: (m) => notify('register', { type: 'tool_metadata', data: { name: m.name } }),

  // ---- Context Engine ----
  registerContextEngine: (ce) => notify('register', { type: 'context_engine', data: { name: ce.name } }),

  // ---- Memory 子系统 ----
  registerMemoryCapability: (mc) => notify('register', { type: 'memory_capability', data: { name: mc.name } }),
  registerMemoryPromptSection: (ps) => notify('register', { type: 'memory_prompt_section', data: { name: ps.name } }),
  registerMemoryFlushPlan: (fp) => notify('register', { type: 'memory_flush_plan', data: { name: fp.name } }),
  registerMemoryRuntime: (mr) => notify('register', { type: 'memory_runtime', data: { name: mr.name } }),
  registerMemoryPromptSupplement: (ps) => notify('register', { type: 'memory_prompt_supplement', data: { name: ps.name } }),
  registerMemoryCorpusSupplement: (cs) => notify('register', { type: 'memory_corpus_supplement', data: { name: cs.name } }),

  // ---- 会话相关 ----
  on: (event, handler) => notify('register', { type: 'session_event', data: { event } }),
  onConversationBindingResolved: (handler) => notify('register', { type: 'conversation_binding_resolved', data: {} }),

  session: {
    state: { registerSessionExtension: (se) => notify('register', { type: 'session_extension', data: { name: se.name } }) },
    workflow: {
      enqueueNextTurnInjection: () => {},
      registerSessionSchedulerJob: (job) => notify('register', { type: 'session_scheduler_job', data: { name: job.name } }),
      sendSessionAttachment: () => {},
      scheduleSessionTurn: () => {},
      unscheduleSessionTurnsByTag: () => {},
    },
    controls: {
      registerControlUiDescriptor: (d) => notify('register', { type: 'control_ui_descriptor', data: { name: d.name } }),
      registerSessionAction: (a) => notify('register', { type: 'session_action', data: { name: a.name } }),
    },
  },

  agent: {
    events: {
      registerAgentEventSubscription: (sub) => notify('register', { type: 'agent_event_subscription', data: { event: sub.event } }),
      emitAgentEvent: (event, data) => notify('agent_event', { event, data }),
    },
  },

  lifecycle: { registerRuntimeLifecycle: (lc) => notify('register', { type: 'lifecycle', data: { name: lc.name } }) },

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

    // Channel output routing: if toolName matches a registered channel, use channel's output handler
    const ch = registeredChannels[toolName];
    if (ch) {
      try {
        const channelPlugin = ch.channelPlugin;
        if (channelPlugin && channelPlugin.outbound) {
          const meta = args.meta || '';
          let metaObj = {};
          try { metaObj = typeof meta === 'string' ? JSON.parse(meta) : meta; } catch {}
          const to = metaObj.user_id || metaObj.to || metaObj.group_id || '';
          const ctx = { to, text: args.payload || '', mediaUrl: metaObj.mediaUrl || '', cfg: {}, accountId: metaObj.accountId || null };
          let result;
          if (ctx.mediaUrl && channelPlugin.outbound.sendMedia) {
            result = await channelPlugin.outbound.sendMedia(ctx);
          } else if (channelPlugin.outbound.sendText) {
            result = await channelPlugin.outbound.sendText(ctx);
          } else {
            throw new Error(`channel ${toolName} has no sendText/sendMedia handler`);
          }
          writeJSON({ jsonrpc: '2.0', id, result: { status: 'sent', result } });
        } else if (typeof ch.output === 'function') {
          const result = await ch.output(args.payload, args.type, args.meta);
          writeJSON({ jsonrpc: '2.0', id, result: { status: 'sent', result } });
        } else if (typeof ch.send === 'function') {
          const result = await ch.send(args.payload, args.meta);
          writeJSON({ jsonrpc: '2.0', id, result: { status: 'sent', result } });
        } else {
          sendError(id, -32601, `channel ${toolName} has no output handler`);
        }
      } catch (e) { sendError(id, -32603, e.message); }
      return;
    }

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
      const result = await tool.execute('sim-call-1', args, undefined, undefined);
      if (result && typeof result === 'object' && Array.isArray(result.content)) {
        writeJSON({ jsonrpc: '2.0', id, result });
      } else {
        const text = typeof result === 'string' ? result : JSON.stringify(result);
        writeJSON({ jsonrpc: '2.0', id, result: { content: [{ type: 'text', text }] } });
      }
    } catch (e) {
      sendError(id, -32603, e.message);
    }
    return;
  }

  // ---- Provider 调用 ----
  if (method === 'provider/call') {
    const { type, action, args } = req.params || {};
    if (!type) { sendError(id, -32602, 'type required'); return; }

    const provider = registeredProviders[type];
    if (!provider) { sendError(id, -32601, `Provider not found: ${type}`); return; }

    const methodName = action || 'execute';
    if (typeof provider.instance[methodName] !== 'function') {
      sendError(id, -32603, `Provider ${type} has no method ${methodName}`);
      return;
    }

    try {
      const result = await provider.instance[methodName](args);
      if (result && typeof result === 'object' && Array.isArray(result.content)) {
        writeJSON({ jsonrpc: '2.0', id, result });
      } else {
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
