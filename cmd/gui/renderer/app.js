// ===== State =====
let state = {
  status: {},
  kernel: null,
  settings: {},
  meta: {},
  pluginMeta: {},
  settingsPlugins: ['core'],
  currentView: 'chat',
  selectedSection: 'core',
  messages: [],
  chatLoading: false,
  chatStage: '',
  healthResult: null,
  starmapInit: false,
  starmapLoading: false,
  starmapData: null,
  chatHistory: [],
  terminals: [],
  cmdHistory: [],
  termScreens: {},
  chatStick: true,
  pendingTools: [],
  eventSource: null,
  chatFinalIdx: -1,
  lang: localStorage.getItem('ha-lang') || 'zh',
  connections: [], currentConn: null,

};

// ===== I18n =====
window._i18n = {
  navOverview: ['概览','Overview'],
  navChat: ['对话','Chat'],
  navPlugins: ['插件','Plugins'],
  navSettings: ['设置','Settings'],
  navAdapters: ['适配器','Adapters'],
  navKernel: ['内核','Kernel'],
  navLogout: ['退出登录','Logout'],
  themeToggle: ['切换亮色/暗色模式','Toggle theme'],
  clickManage: ['点击管理连接','Click to manage connections'],
  secondsAgo: ['秒前','s ago'],
  minutesAgo: ['分钟前','min ago'],
  hoursAgo: ['小时前','h ago'],
  noConnection: ['未连接','Not connected'],
  agentAvatar: ['小宅','Agent'],
  waitingAI: ['等待AI回复...','Waiting for AI...'],
  noResponse: ['(无响应)','(no response)'],
  error: ['错误: ','Error: '],
  requestFailed: ['请求失败: ','Request failed: '],
  send: ['发送','Send'],
  queryFailed: ['查询失败: ','Query failed: '],
  searchFailed: ['搜索失败: ','Search failed: '],
  getFailed: ['获取失败: ','Get failed: '],
  createFailed: ['创建失败','Create failed'],
  createFailedWith: ['创建失败: ','Create failed: '],
  nameContentEmpty: ['名称和内容不能为空','Name and content cannot be empty'],
  knowledgeCreated: ['知识「','Knowledge "'],
  knowledgeCreatedEnd: ['」已创建','" created'],
  noContext: ['无上下文','No context'],
  noSessions: ['暂无终端会话','No terminal sessions'],
  noHistory: ['暂无命令记录','No command history'],
  running: ['运行中','Running'],
  closed: ['已关闭','Closed'],
  command: ['命令','Command'],
  status: ['状态','Status'],
  created: ['创建时间','Created'],
  uptime: ['运行时长','Uptime'],
  output: ['输出预览','Output'],
  time: ['时间','Time'],
  actions: ['操作','Actions'],
  name: ['名称','Name'],
  description: ['描述','Description'],
  version: ['版本','Version'],
  details: ['详情','Details'],
  close: ['关闭','Close'],
  install: ['安装','Install'],
  installPlugin: ['安装插件','Install Plugin'],
  packageUrl: ['.hmap 包下载 URL','Package URL'],
  uploadHmap: ['选择 .hmap 文件上传','Upload .hmap file'],
  loadedPlugins: ['已加载插件','Loaded Plugins'],
  noLoadedPlugins: ['暂无已加载插件','No loaded plugins'],
  loaded: ['已加载','Loaded'],
  builtin: ['内置','Built-in'],
  unload: ['卸载','Unload'],
  installedExternal: ['已安装外部插件','Installed Plugins'],
  pluginDetails: ['插件详情','Plugin Details'],
  registeredTools: ['已注册工具','Registered Tools'],
  systemOps: ['系统操作','System Operations'],
  reloadPlugins: ['重载插件','Reload Plugins'],
};

function __(zh, en) { return state.lang === 'en' ? en : zh }
function L() { return state.lang }

function toggleLang() {
  state.lang = state.lang === 'zh' ? 'en' : 'zh';
  localStorage.setItem('ha-lang', state.lang);
  document.querySelectorAll('[data-i18n]').forEach(function(el) {
    var k = el.getAttribute('data-i18n');
    var m = window._i18n && window._i18n[k];
    if (m) el.textContent = __(m[0], m[1]);
  });
  renderAll();
}

function applyI18n() {
  var lang = state.lang;
  var btn = document.getElementById('lang-btn');
  if (btn) btn.textContent = lang === 'zh' ? 'EN' : '中';
  document.querySelectorAll('[data-i18n]').forEach(function(el) {
    var k = el.getAttribute('data-i18n');
    var m = window._i18n && window._i18n[k];
    if (m) el.textContent = lang === 'en' ? m[1] : m[0];
  });
}

// ===== Theme =====
var ICON_SUN_GUI =
  '<svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M2 12h2m16 0h2M4.9 19.1l1.4-1.4m11.4-11.4 1.4-1.4"/></svg>';
var ICON_MOON_GUI =
  '<svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/></svg>';

function setTheme(name) {
  document.documentElement.setAttribute('data-theme', name);
  localStorage.setItem('ha-theme', name);
  var btn = document.getElementById('theme-btn');
  if (btn) btn.innerHTML = name === 'light' ? ICON_SUN_GUI : ICON_MOON_GUI;
}

function toggleTheme() {
  var cur = document.documentElement.getAttribute('data-theme');
  setTheme(cur === 'light' ? 'dark' : 'light');
}

(function() {
  var saved = localStorage.getItem('ha-theme');
  setTheme(saved || 'dark');
})();

// ===== Utility =====
function escHtml(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function timeAgo(t) {
  var s = Math.floor((Date.now() - new Date(t).getTime()) / 1000);
  if (s < 60) return s + __('秒前','s ago');
  var m = Math.floor(s / 60);
  if (m < 60) return m + __('分钟前','min ago');
  return Math.floor(m / 60) + __('小时前','h ago');
}

function toast(m, isError) {
  var t = document.getElementById('toast');
  t.textContent = m;
  t.className = 'toast' + (isError ? ' error' : '');
  t.style.display = 'block';
  setTimeout(function() { t.style.display = 'none' }, 3000);
}

// ===== API =====
async function cliRequest(line) {
  var conn = state.currentConn;
  if (!conn) throw new Error(__('未选择连接','No connection selected'));
  if (!window.homeagent || !window.homeagent.cli) throw new Error('cli bridge unavailable');
  var resp = await window.homeagent.cli.request(conn.socketPath || conn.url, conn.apiKey, line);
  if (resp && resp.error) throw new Error(resp.error);
  return resp;
}

// CLI 传输映射：将 REST 路径转换为 cli 内置命令或直接对话
function cliMap(path, o) {
  o = o || {};
  var m = o.method || 'GET';
  if (m === 'POST' && path.indexOf('/chat') !== -1) {
    var body = {};
    try { body = JSON.parse(o.body || '{}'); } catch (e) {}
    return cliRequest(body.message || '');
  }
  if (path === '/status') return cliRequest('/status');
  if (path === '/kernel') return cliRequest('/kernel');
  if (path === '/settings') return cliRequest('/settings');
  if (path === '/chat/history') return Promise.resolve({ messages: [] });
  return Promise.reject(new Error(__('CLI 连接不支持此功能','Not supported on CLI connection')));
}

async function api(p, o) {
  if (!state.currentConn) throw new Error(__('未选择连接','No connection selected'));
  if (state.currentConn.type === 'cli') {
    return cliMap(p, o);
  }
  var opts = o || {};
  var headers = { 'Content-Type': 'application/json', ...(opts.headers || {}) };
  if (state.currentConn.apiKey) headers['X-API-Key'] = state.currentConn.apiKey;
  var r = await fetch(state.currentConn.url + '/api/v1' + p, { ...opts, headers: headers });
  if (r.status === 401) throw new Error(__('认证失败','unauthorized'));
  if (opts.raw) return r;
  var ct = r.headers.get('content-type') || '';
  if (ct.includes('json')) return r.json();
  return r.text();
}

// ===== Navigation =====
function switchView(n) {
  document.querySelectorAll('.view').forEach(function(e) { e.classList.remove('active') });
  var el = document.getElementById('view-' + n);
  if (el) el.classList.add('active');
  document.querySelectorAll('.rail-btn').forEach(function(e) { e.classList.remove('active') });
  var rb = document.getElementById('rail-' + n);
  if (rb) rb.classList.add('active');
  state.currentView = n;
  if (n === 'chat') {
    state.chatStick = true;
    var msgsEl = document.getElementById('chat-msgs');
    if (msgsEl) { try { msgsEl.scrollTo({ top: msgsEl.scrollHeight, behavior: 'smooth' }) } catch(e) { msgsEl.scrollTop = msgsEl.scrollHeight } }
  }
  renderAll();
}

// ===== Tab Render Dispatch =====
async function doRenderAll() {
  try { var s = await api('/status'); state.status = s; state.startedAt = s.startedAt ? new Date(s.startedAt).getTime() : null; updateConnIndicator() } catch(e) {}
  try { state.kernel = await api('/kernel') } catch(e) {}
  try {
    var s = await api('/settings');
    state.settings = s.settings || {};
    state.meta = s.meta || {};
    state.settingsPlugins = s.plugins || ['core'];
    state.pluginMeta = s.plugin_meta || {};
  } catch(e) {}
  try { state.installedPlugins = await api('/plugins') } catch(e) {}
  try { await loadTerminals() } catch(e) {}
  try { await loadCmdHistory() } catch(e) {}
  renderAll();
}

async function renderAll() {
  try { var s = await api('/status'); state.status = s; state.startedAt = s.startedAt ? new Date(s.startedAt).getTime() : null } catch(e) {}
  try { state.kernel = await api('/kernel') } catch(e) {}
  try {
    var s = await api('/settings');
    state.settings = s.settings || {};
    state.meta = s.meta || {};
    state.settingsPlugins = s.plugins || ['core'];
    state.pluginMeta = s.plugin_meta || {};
  } catch(e) {}
  try { state.installedPlugins = await api('/plugins') } catch(e) {}
  try { await loadTerminals() } catch(e) {}
  try { await loadCmdHistory() } catch(e) {}
  try { renderOverview() } catch(e) { console.error('renderOverview', e) }
  try { renderChat() } catch(e) { console.error('renderChat', e) }
  try { renderChatStarmap() } catch(e) { console.error('renderChatStarmap', e) }
  try { renderPlugins() } catch(e) { console.error('renderPlugins', e) }
  try { renderKernel() } catch(e) { console.error('renderKernel', e) }
  try { renderOneSettings() } catch(e) { console.error('renderOneSettings', e) }
  try { renderAdapters() } catch(e) { console.error('renderAdapters', e) }
  applyI18n();
}

function fmtUptime(ms) {
  var s = Math.floor(ms / 1000);
  if (s < 60) return s + 's';
  var m = Math.floor(s / 60); s = s % 60;
  if (m < 60) return m + 'm ' + s + 's';
  var h = Math.floor(m / 60); m = m % 60;
  return h + 'h ' + m + 'm ' + s + 's';
}

var uptimeTick = null;
function startUptimeTicker() {
  if (uptimeTick) clearInterval(uptimeTick);
  uptimeTick = setInterval(function() {
    var el = document.querySelector('#uptime-val');
    if (el && state.startedAt) {
      var now = Date.now();
      el.textContent = fmtUptime(now - state.startedAt);
    } else if (!state.startedAt) {
      var el2 = document.querySelector('#uptime-val');
      if (el2) el2.textContent = '-';
    }
  }, 1000);
}

// ===== Overview =====
function statCard(l, v) {
  return '<div class="card stat-card"><div class="stat-value">' + v + '</div><div class="stat-label">' + l + '</div></div>';
}

function renderOverview() {
  var s = state.status || {};
  var k = state.kernel;
  var html = '<div class="grid-4">'
    + statCard(__('运行状态','Status'), s.status || 'unknown', 'running')
    + statCard(__('运行时间','Uptime'), '<span id="uptime-val">' + (state.startedAt ? fmtUptime(Date.now() - state.startedAt) : '-') + '</span>', 'uptime')
    + statCard(__('插件','Plugins'), (k?.plugins || []).length || 0, 'plugin')
    + statCard(__('版本','Version'), s.version || '0.1.0', 'version')
    + '</div>';
  if (k) {
    html += '<div class="grid-2">'
      + '<div class="card"><h2>' + __('LLM 状态','LLM Status') + '</h2>'
      + '<div class="kv-row"><span class="key">Provider</span><span class="val">' + (k.llm?.provider || __('未配置','Not configured')) + '</span></div>'
      + '<div class="kv-row"><span class="key">' + __('可用源','Sources') + '</span><span class="val">' + (k.llm?.sources || 0) + '</span></div>'
      + '<div class="kv-row"><span class="key">' + __('状态','Status') + '</span><span class="val"><span class="status-dot ' + (k.llm?.available ? 'dot-green' : 'dot-red') + '"></span>' + (k.llm?.available ? __('运行中','Running') : __('不可用','Unavailable')) + '</span></div>'
      + '</div>'
      + '<div class="card"><h2>' + __('记忆状态','Memory Status') + '</h2>'
      + '<div class="kv-row"><span class="key">' + __('图记忆','Graph Memory') + '</span><span class="val"><span class="status-dot ' + (k.memory?.available ? 'dot-green' : 'dot-gray') + '"></span>' + (k.memory?.available ? k.memory.entity_count + __(' 实体, ',' entities, ') + k.memory.relation_count + __(' 关系',' relations') : __('未初始化','Uninitialized')) + '</span></div>'
      + '<div class="kv-row"><span class="key">' + __('文档记忆','Document Memory') + '</span><span class="val">' + (k.documents?.available ? k.documents.doc_count + __(' 文档',' docs') : __('未初始化','Uninitialized')) + '</span></div>'
      + '<div class="kv-row"><span class="key">' + __('文本记忆','Text Memory') + '</span><span class="val">' + (k.text_memory?.available ? k.text_memory.file_count + __(' 文件',' files') : __('未初始化','Uninitialized')) + '</span></div>'
      + '<div class="kv-row"><span class="key">' + __('知识库','Knowledge') + '</span><span class="val">' + (k.knowledge?.available ? k.knowledge.item_count + __(' 项',' items') : __('未初始化','Uninitialized')) + '</span></div>'
      + '</div></div>';
  }
  html += '<div class="card"><h2>' + __('运行时','Runtime') + '</h2><div class="grid-3">'
    + statCard('Goroutines', k?.runtime?.goroutines || '-', '')
    + statCard(__('内存','Memory'), k?.runtime?.memory_mb ? k.runtime.memory_mb + ' MB' : '-', '')
    + statCard('Go ' + __('版本','Version'), k?.runtime?.go_version || '-', '')
    + '</div></div>';
  document.getElementById('view-overview').innerHTML = html;
}

// ===== Chat =====
var _chatLayoutBuilt = false;

function buildChatLayout() {
  var cont = document.getElementById('view-chat');
  var k = state.kernel || {};
  var html = '<div class="chat-layout">';
  html += '<div class="chat-tabs">'
    + '<span class="active" onclick="switchChatPanel(\'chat\',this)">' + __('对话','Chat') + '</span>'
    + '<span onclick="switchChatPanel(\'starmap\',this)">' + __('星图','Star Map') + '</span>'
    + '<span onclick="switchChatPanel(\'terminal\',this)">' + __('终端','Terminal') + '</span>'
    + '<span onclick="switchChatPanel(\'cmd\',this)">' + __('运行中命令','Running Commands') + '</span>'
    + '<span onclick="switchChatPanel(\'memory\',this)">' + __('记忆','Memory') + '</span>'
    + '<span onclick="switchChatPanel(\'context\',this)">' + __('上下文','Context') + '</span>'
    + '<span onclick="switchChatPanel(\'knowledge\',this)">' + __('知识','Knowledge') + '</span>'
    + '</div>';
  if (!state.currentConn) {
    html += '<div class="chat-panel active" id="chat-panel-chat"><div class="card setup-card">'
      + '<svg class="setup-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z"/></svg>'
      + '<h2>' + __('未配置后端','No backend configured') + '</h2>'
      + '<p>' + __('连接 HomeAgent 服务端后即可开始对话。请在设置中添加后端连接。','Connect to a HomeAgent server to start chatting. Add a backend connection in Settings.') + '</p>'
      + '<button class="btn btn-primary" onclick="goSettingsConn()">' + __('前往设置添加后端','Go to Settings to add backend') + '</button>'
      + '</div></div>';
    for (var p2 = 0; p2 < 6; p2++) {
      html += '<div class="chat-panel" id="chat-panel-' + ['starmap','terminal','cmd','memory','context','knowledge'][p2] + '"><div class="card"><p style="color:var(--text-muted)">' + __('请先在设置中添加后端连接','Add a backend connection in Settings first') + '</p></div></div>';
    }
    cont.innerHTML = html;
    _chatLayoutBuilt = true;
    return;
  }
  html += '<div class="chat-panel active" id="chat-panel-chat"><div class="chat-main">';
  html += '<div class="card"><h2>' + __('对话','Chat') + ' <span id="chat-stage" class="badge" style="font-size:10px;font-weight:400;display:none">' + escHtml(state.chatStage || '') + '</span></h2><div class="chat-messages" id="chat-msgs">';
  if (state.messages.length === 0) {
    html += '<div class="empty-state" style="flex:1;display:flex;align-items:center;justify-content:center"><p>' + __('开始对话以测试 Agent 回复','Start a conversation to test Agent replies') + '</p></div>';
  }
  html += '</div>'
    + '<div class="chat-input-row">'
    + '<input id="chat-input" placeholder="' + __('输入消息...','Type a message...') + '" onkeydown="if(event.key==\'Enter\')sendChat()">'
    + '<button class="btn btn-primary" onclick="sendChat()" id="chat-send-btn">' + __('发送','Send') + '</button>'
    + '</div></div></div></div>';
  html += '<div class="chat-panel" id="chat-panel-starmap"><div class="card"><h2>' + __('星图','Star Map') + '</h2>'
    + '<div id="sm-container-chat" style="display:flex;align-items:center;justify-content:center;min-height:480px"><div class="loading-spinner"></div></div></div></div>';
  html += '<div class="chat-panel" id="chat-panel-terminal"><div class="card"><h2>' + __('终端','Terminal') + ' <span id="term-count-badge" class="badge badge-blue">0</span></h2>'
    + '<div id="term-list" style="max-height:60vh;overflow-y:auto;font-size:12px"></div></div></div>';
  html += '<div class="chat-panel" id="chat-panel-cmd"><div class="card"><h2>' + __('运行中命令','Running Commands') + ' <span id="cmd-count-badge" class="badge badge-blue">0</span></h2>'
    + '<div id="cmd-list" style="max-height:60vh;overflow-y:auto;font-size:12px"></div></div></div>';
  html += '<div class="chat-panel" id="chat-panel-memory"><div class="card"><h2>' + __('记忆','Memory') + '</h2>'
    + '<div class="kv-row"><span class="key">' + __('实体','Entities') + '</span><span class="val">' + (k?.memory?.entity_count || '-') + '</span></div>'
    + '<div class="kv-row"><span class="key">' + __('关系','Relations') + '</span><span class="val">' + (k?.memory?.relation_count || '-') + '</span></div>'
    + '<div style="margin-top:8px">'
    + '<input id="mem-query" placeholder="' + __('关键词查询','Keyword query') + '">'
    + '<button class="btn btn-primary btn-sm" onclick="queryMemoryChat()">' + __('查询','Query') + '</button>'
    + '</div><div id="mem-result-chat" style="margin-top:8px;max-height:180px;overflow:auto"></div>'
    + '</div></div>';
  html += '<div class="chat-panel" id="chat-panel-context"><div class="card"><h2>' + __('上下文','Context') + '</h2>'
    + '<div style="margin-top:8px">'
    + '<input id="ctx-query" placeholder="' + __('输入当前话题','Enter current topic') + '">'
    + '<button class="btn btn-primary btn-sm" onclick="queryMemoryContext()">' + __('获取上下文','Get Context') + '</button>'
    + '</div><div id="ctx-result" style="margin-top:8px;max-height:200px;overflow:auto"></div>'
    + '</div></div>';
  html += '<div class="chat-panel" id="chat-panel-knowledge"><div class="card"><h2>' + __('知识','Knowledge') + '</h2>'
    + '<div class="kv-row"><span class="key">' + __('项目','Items') + '</span><span class="val">' + (k?.knowledge?.item_count || '-') + '</span></div>'
    + '<div style="margin-top:8px">'
    + '<input id="know-query" placeholder="' + __('搜索知识','Search knowledge') + '">'
    + '<button class="btn btn-primary btn-sm" onclick="searchKnowledgeChat()">' + __('搜索','Search') + '</button>'
    + '</div><div id="know-result-chat" style="margin-top:8px;max-height:180px;overflow:auto"></div>'
    + '<div style="margin-top:12px;border-top:1px solid var(--border-color);padding-top:8px">'
    + '<input id="know-name" placeholder="' + __('知识名称','Knowledge name') + '" style="margin-bottom:4px">'
    + '<textarea id="know-content" placeholder="' + __('内容','Content') + '" style="min-height:50px;margin-bottom:4px"></textarea>'
    + '<button class="btn btn-primary btn-sm" onclick="createKnowledgeChat()">' + __('创建','Create') + '</button>'
    + '</div></div></div>';
  cont.innerHTML = html;
  _chatLayoutBuilt = true;
}

var CHAN_COLORS = ['#e08a5f', '#5f9fe0', '#6bbf8f', '#c06bbf', '#d9a13b', '#5fb3bf', '#b06b6b', '#7f8ce0'];

function chanColor(src) {
  var h = 0;
  for (var i = 0; i < src.length; i++) h = (h * 31 + src.charCodeAt(i)) >>> 0;
  return CHAN_COLORS[h % CHAN_COLORS.length];
}

function chanLetter(src) {
  var s = (src || '').trim();
  if (!s) return 'C';
  var ch = s.charAt(0).toUpperCase();
  return /[A-Za-z0-9]/.test(ch) ? ch : 'C';
}

function renderChat() {
  if (!_chatLayoutBuilt) { buildChatLayout(); renderChatStarmap(); renderTerminals(); renderCmdHistory() }
  var msgsEl = document.getElementById('chat-msgs');
  if (!msgsEl) return;
  if (!msgsEl._stickBound) {
    msgsEl._stickBound = true;
    msgsEl.addEventListener('scroll', function() {
      state.chatStick = msgsEl.scrollHeight - msgsEl.scrollTop - msgsEl.clientHeight < 80;
    }, { passive: true });
  }
  var msgs = state.messages;
  var sig = msgs.map(function(m) {
    var c = m.content || '';
    return (m.role || '') + ':' + c.length + ':' + c.slice(-40) + ':' + (m.tool_calls || []).map(function(t) { return (t.tool || t.name || '') + '/' + (t.status || '') }).join(',');
  }).join('|') + '|L' + (state.chatLoading ? '1' : '0') + '|P' + (state.pendingTools || []).join(',');
  if (msgsEl._chatSig === sig && msgsEl.childElementCount > 0) { return; }
  msgsEl._chatSig = sig;
  var prevPending = msgsEl._lastPending || [];
  var newPending = (state.pendingTools || []).slice();
  var lastM = msgs.length ? msgs[msgs.length - 1] : null;
  if (state.chatLoading && lastM && lastM.role === 'assistant') {
    (lastM.tool_calls || []).forEach(function(tc) {
      if (!tc.result && tc.status !== 'denied') {
        var nm = tc.tool || tc.name || '';
        if (newPending.indexOf(nm) === -1) newPending.push(nm);
      }
    });
  }
  var newlyDone = prevPending.filter(function(n) { return newPending.indexOf(n) === -1; });
  msgsEl._lastPending = newPending;
  var streamingLast = !!(state.chatLoading && lastM && lastM.role === 'assistant' && !lastM._final);
  function pillHtml() {
    var s = '';
    newPending.forEach(function(nm) {
      var anim = prevPending.indexOf(nm) !== -1 ? '' : ' pill-in';
      s += '<span class="thinking-tool' + anim + '" data-tool="' + escHtml(nm) + '">'
        + '<svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.9 2.9-2.5-.6-.6-2.5z"/></svg>'
        + escHtml(nm) + '</span>';
    });
    return s;
  }
  var html = '';
  if (msgs.length === 0) {
    html = '<div class="empty-state" style="flex:1;display:flex;align-items:center;justify-content:center"><p>' + __('开始对话以测试 Agent 回复','Start a conversation to test Agent replies') + '</p></div>';
  } else {
    msgs.forEach(function(m, i) {
      var role = m.role || 'user';
      var c = m.content || '';
      if (role === 'assistant') {
        if (typeof marked !== 'undefined') { c = marked.parse(c) } else { c = '<pre>' + escHtml(c) + '</pre>' }
      } else if (role === 'system') {
        c = escHtml(c);
      } else {
        c = escHtml(c);
      }
      var isChan = !!(m.source && m.source !== 'webui');
      var rc = '';
      if (m.reasoning_content) {
        var rcBody = (typeof marked !== 'undefined' ? marked.parse(m.reasoning_content) : escHtml(m.reasoning_content));
        rc = '<div class="msg-bubble"><div class="reasoning">'
          + '<div class="reasoning-title" onclick="var n=this.nextElementSibling;n.style.display=n.style.display===\'none\'?\'block\':\'none\';this.textContent=this.textContent===\'' + __('展开思考','Expand') + '\'?\'' + __('收起思考','Collapse') + '\':\'' + __('展开思考','Expand') + '\'">' + __('展开思考','Expand') + '</div>'
          + '<div class="reasoning-body" style="display:none">' + rcBody + '</div></div></div>';
      }
      var tcs = '';
      if (m.tool_calls && m.tool_calls.length > 0) {
        m.tool_calls.forEach(function(tc) {
          var argsStr = typeof tc.args === 'object' ? JSON.stringify(tc.args, null, 1) : (tc.args || '');
          var resultStr = tc.result ? (typeof tc.result === 'object' ? JSON.stringify(tc.result, null, 1) : String(tc.result)) : '';
          var statusIcon = tc.status === 'denied'
            ? '<svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" style="color:var(--error);vertical-align:-1px"><circle cx="12" cy="12" r="9"/><path d="M5.6 5.6l12.8 12.8"/></svg>'
            : '<svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" style="color:var(--accent);vertical-align:-1px"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.9 2.9-2.5-.6-.6-2.5z"/></svg>';
          tcs += '<div class="msg-bubble"><div class="tool-call' + (newlyDone.indexOf(tc.tool || tc.name || '') !== -1 ? ' tool-drip-in' : '') + '" onclick="toggleToolCall(this)">'
            + '<div class="tc-line"><span class="tc-ico">' + statusIcon + '</span><span class="tc-name">' + escHtml(tc.tool || tc.name || '') + '</span>'
            + (tc.status === 'denied'
                ? '<span class="tc-state tc-deny">' + __('已拒绝','Denied') + '</span>'
                : (resultStr
                    ? '<span class="tc-state tc-done">' + __('完成','Done') + '</span>'
                    : '<span class="tc-state tc-run">' + __('调用中','Running') + '</span>'))
            + '<span class="tc-caret">▾</span></div>'
            + '<div class="tc-detail" style="display:none">'
            + (argsStr && argsStr !== '{}' ? '<div class="tc-args">' + escHtml(argsStr) + '</div>' : '')
            + (resultStr ? '<div class="tc-result">' + escHtml(resultStr) + '</div>' : '')
            + '</div></div></div>';
        });
      }
      var body = rc + tcs;
      var growCls = m._grow ? ' grow-in' : '';
      if (m._grow) m._grow = false;
      var isStreamingLast = i === msgs.length - 1 && streamingLast;
      if (isStreamingLast) {
        var liveRow = '<span class="live-spinner"></span>' + (newPending.length ? '<span class="thinking-tools">' + pillHtml() + '</span>' : '');
        if (c) {
          body += '<div class="msg-bubble' + growCls + '">' + liveRow + '<div class="text">' + c + '</div></div>';
          c = '';
        } else {
          body += '<div class="msg-bubble">' + liveRow + '</div>';
        }
      } else if (c) {
        body += '<div class="msg-bubble' + growCls + '"><div class="text">' + c + '</div></div>';
      }
      if (role === 'system') {
        html += '<div class="msg msg-system"><div class="msg-bubble">' + (c || '') + '</div></div>';
      } else if (isChan) {
        html += '<div class="msg msg-channel">'
          + '<div class="msg-avatar chan-avatar" style="background:' + chanColor(m.source) + '">' + chanLetter(m.source) + '</div>'
          + '<div class="msg-content"><div class="msg-chan-name">' + escHtml(m.source) + '</div>' + body + '</div>'
          + '</div>';
      } else {
        var userAvatar = '<svg viewBox="0 0 24 24" style="width:16px;height:16px" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="8" r="4"/><path d="M4 20c0-4 4-6 8-6s8 2 8 6"/></svg>';
        var aiAvatar = '<img src="mascot.webp" style="width:28px;height:28px;border-radius:50%;object-fit:cover" alt="' + __('小宅','Agent') + '">';
        html += '<div class="msg msg-' + role + '">'
          + '<div class="msg-avatar">' + (role === 'user' ? userAvatar : aiAvatar) + '</div>'
          + '<div class="msg-content">' + body + '</div>'
          + '</div>';
      }
    });
  }
  if (state.chatLoading && !streamingLast) {
    var aiAvatar2 = '<img src="mascot.webp" style="width:28px;height:28px;border-radius:50%;object-fit:cover" alt="' + __('小宅','Agent') + '">';
    html += '<div class="msg msg-assistant"><div class="msg-avatar">' + aiAvatar2 + '</div><div class="msg-content"><div class="msg-bubble">'
      + '<span class="live-spinner"></span>'
      + (newPending.length ? '<span class="thinking-tools">' + pillHtml() + '</span>' : '')
      + '</div></div></div>';
  }
  msgsEl.innerHTML = html;
  if (state.chatStick !== false) { try { msgsEl.scrollTo({ top: msgsEl.scrollHeight, behavior: 'smooth' }) } catch(e) { msgsEl.scrollTop = msgsEl.scrollHeight } }
  updateChatBadge();
}

function updateChatBadge() {
  var badge = document.getElementById('chat-stage');
  if (!badge) return;
  badge.textContent = state.chatStage || '';
  badge.style.display = 'none';
}

function rerenderChat() { renderChat(); renderChatStarmap(); renderTerminals(); renderCmdHistory() }

function toggleToolCall(el) {
  var d = el.querySelector('.tc-detail');
  if (!d) return;
  var open = d.style.display !== 'none';
  d.style.display = open ? 'none' : 'block';
  if (open) { el.classList.remove('open'); } else { el.classList.add('open'); }
}

function renderChatStarmap() {
  var cont = document.getElementById('sm-container-chat');
  if (!cont) return;
  if (window._THREE_FAILED || (!window.THREE && window._THREE_FAILED !== undefined)) {
    cont.innerHTML = '<p style="color:var(--text-muted);padding:20px;text-align:center;font-size:11px">' + __('3D 星图不可用（CDN 加载失败）','Star map unavailable (CDN load failed)') + '</p>';
    state.starmapInit = true;
    state.starmapLoading = false;
    return;
  }
  if (!window.THREE) {
    cont.innerHTML = '<div style="display:flex;align-items:center;justify-content:center;height:100%;padding:20px"><div class="loading-spinner"></div></div>';
    state.starmapInit = false;
    state.starmapLoading = false;
    return;
  }
  if (cont.querySelector('canvas')) {
    var rect = cont.getBoundingClientRect();
    if (starmapRen && rect.width > 0) starmapRen.setSize(rect.width, Math.max(rect.height, 250));
    return;
  }
  if (state.starmapInit) {
    if (starmapRen) {
      var rect = cont.getBoundingClientRect();
      if (rect.width > 0) starmapRen.setSize(rect.width, Math.max(rect.height, 250));
      cont.appendChild(starmapRen.domElement);
      starmapRen.domElement.style.display = 'block';
    } else {
      // starmapRen was destroyed (e.g. re-render cycle), restart
      state.starmapInit = false;
      state.starmapLoading = false;
    }
    return;
  }
  if (state.starmapLoading) return;
  state.starmapLoading = true;
  loadChatStarmapData();
}

async function loadChatStarmapData() {
  try {
    var resp = await api('/memory/graph');
    if (!resp || !resp.success || !resp.data || !resp.data.nodes || resp.data.nodes.length === 0) {
      document.getElementById('sm-container-chat').innerHTML
        = '<p style="color:var(--text-muted);padding:20px;text-align:center">'
        + __('暂无记忆数据','No memory data') + '</p>';
      state.starmapInit = true;
      state.starmapLoading = false;
      return;
    }
    var d = resp.data;
    starmapNodes = d.nodes || [];
    starmapEdges = d.edges || [];
    state.starmapInit = true;
    state.starmapLoading = false;
    initChatStarmap();
  } catch(e) {
    document.getElementById('sm-container-chat').innerHTML
      = '<p style="color:var(--text-muted);padding:20px;text-align:center">'
      + __('加载失败','Load failed') + '</p>';
    state.starmapInit = true;
    state.starmapLoading = false;
  }
}

function getStarmapBg() {
  return 0x0a0a1a;
}

function initChatStarmap() {
  var cont = document.getElementById('sm-container-chat');
  if (!cont) return;
  var rect = cont.getBoundingClientRect();
  var w = Math.max(rect.width || 300, 100);
  var h = Math.max(rect.height || 250, 100);
  if (starmapRen) {
    starmapRen.setSize(w, h);
    cont.appendChild(starmapRen.domElement);
    starmapRen.domElement.style.display = 'block';
    return;
  }
  starmapScene = new THREE.Scene();
  starmapScene.fog = new THREE.FogExp2(0x0a0a1a, 0.015);
  starmapCam = new THREE.PerspectiveCamera(60, w / h, 0.1, 2000);
  starmapCam.position.set(0, 20, 40);
  starmapRen = new THREE.WebGLRenderer({ antialias: true, alpha: true });
  starmapRen.setSize(w, h);
  starmapRen.setPixelRatio(Math.min(window.devicePixelRatio, 2));
  starmapRen.setClearColor(0x0a0a1a, 1);
  cont.innerHTML = '';
  cont.appendChild(starmapRen.domElement);
  starmapCtrl = new THREE.OrbitControls(starmapCam, starmapRen.domElement);
  starmapCtrl.enableDamping = true;
  starmapCtrl.dampingFactor = 0.05;
  starmapCtrl.rotateSpeed = 0.5;
  starmapCtrl.zoomSpeed = 0.8;
  var al = new THREE.AmbientLight(0x444466, 0.6);
  starmapScene.add(al);
  var dl = new THREE.DirectionalLight(0xffffff, 0.8);
  dl.position.set(50, 100, 50);
  starmapScene.add(dl);
  createStarField();
  createNebula();
  buildChatStarmapGraph();
  starmapRen.domElement.addEventListener('mousemove', onStarmapMove);
  starmapRen.domElement.addEventListener('click', onStarmapClick);
  window.addEventListener('resize', onStarmapResize);
  if (starmapRaf) cancelAnimationFrame(starmapRaf);
  starmapAnimate();
}

function buildChatStarmapGraph() {
  starmapNodeMeshes.forEach(function(m) { starmapScene.remove(m) });
  starmapEdgeLines.forEach(function(l) { starmapScene.remove(l) });
  starmapNodeMeshes = [];
  starmapEdgeLines = [];
  if (starmapNodes.length === 0) return;
  // Calculate node degrees for leaf node detection
  var nodeDegs = {};
  starmapNodes.forEach(function(n) { nodeDegs[n.id] = 0 });
  starmapEdges.forEach(function(e) {
    nodeDegs[e.source_id] = (nodeDegs[e.source_id] || 0) + 1;
    nodeDegs[e.target_id] = (nodeDegs[e.target_id] || 0) + 1;
  });
  var nodeMap = {};
  starmapNodes.forEach(function(n) { nodeMap[n.id] = n });
  var sorted = starmapNodes.slice().sort(function(a, b) {
    return (b.mention_count || 0) - (a.mention_count || 0);
  });
  var mc = sorted.map(function(n) { return n.mention_count || 0 });
  var maxMc = Math.max(...mc, 1), minMc = Math.min(...mc, 0), rng = maxMc - minMc || 1;
  // Layout positions
  var pos = {};
  var baseR = 15, maxR = 80;
  var total = sorted.length;
  var acc = 0;
  sorted.forEach(function(n, i) {
    var m = n.mention_count || 0, mn = rng > 0 ? (m - minMc) / rng : 0;
    var radius = baseR + mn * (maxR - baseR);
    var baseStep = (Math.PI * 2) / total;
    var extra = mn * baseStep * 2;
    var angle = acc + extra / 2;
    acc += baseStep + extra;
    pos[n.id] = {
      x: radius * Math.cos(angle),
      y: (Math.random() - 0.5) * (10 + mn * 20),
      z: radius * Math.sin(angle),
      mn: mn,
      rad: radius
    };
  });
  // Leaf nodes (degree 1) reposition near parent
  sorted.forEach(function(n) {
    var deg = nodeDegs[n.id] || 0;
    if (deg !== 1) return;
    var edge = starmapEdges.find(function(e) { return e.source_id === n.id || e.target_id === n.id });
    if (!edge) return;
    var parentId = edge.source_id === n.id ? edge.target_id : edge.source_id;
    if (!pos[parentId]) return;
    var pp = pos[parentId];
    var m = n.mention_count || 0, mn = rng > 0 ? (m - minMc) / rng : 0;
    var off = 6 + mn * 8 + Math.random() * 4;
    var a2 = Math.random() * Math.PI * 2;
    pos[n.id] = {
      x: pp.x + off * Math.cos(a2),
      y: pp.y + (Math.random() - 0.5) * (4 + mn * 6),
      z: pp.z + off * Math.sin(a2),
      mn: mn,
      rad: off
    };
  });
  // Force-directed simulation
  for (var it = 0; it < 50; it++) {
    var ids = Object.keys(pos);
    // Repulsion
    for (var i = 0; i < ids.length; i++) {
      for (var j = i + 1; j < ids.length; j++) {
        var a = pos[ids[i]], b = pos[ids[j]];
        var dx = a.x - b.x, dy = a.y - b.y, dz = a.z - b.z, d = Math.sqrt(dx*dx + dy*dy + dz*dz) + 0.1;
        var rf = 0.5 + (a.mn + b.mn) * 0.5;
        if (d < 25) {
          var force = (0.06 * rf) / Math.max(d, 0.5);
          a.x += dx / d * force; a.y += dy / d * force; a.z += dz / d * force;
          b.x -= dx / d * force; b.y -= dy / d * force; b.z -= dz / d * force;
        }
      }
    }
    // Attraction along edges
    starmapEdges.forEach(function(e) {
      var a = pos[e.source_id], b = pos[e.target_id];
      if (!a || !b) return;
      var dx = b.x - a.x, dy = b.y - a.y, dz = b.z - a.z, d = Math.sqrt(dx*dx + dy*dy + dz*dz) + 0.1;
      var af = Math.max(0.3, 1.0 - (a.mn + b.mn) * 0.3);
      if (d > 20) {
        var force = 0.04 * af;
        a.x += dx / d * force; a.y += dy / d * force; a.z += dz / d * force;
        b.x -= dx / d * force; b.y -= dy / d * force; b.z -= dz / d * force;
      }
    });
    // Centering constraint
    ids.forEach(function(id) {
      var p = pos[id];
      var dist = Math.sqrt(p.x * p.x + p.y * p.y + p.z * p.z);
      var maxA = maxR * 1.5;
      if (dist > maxA) { var s = maxA / dist; p.x *= s; p.y *= s; p.z *= s }
    });
  }
  // Create nodes
  starmapNodes.forEach(function(n) {
    var p = pos[n.id];
    if (!p) return;
    var mn = n.mention_count || 0, mnr = rng > 0 ? (mn - minMc) / rng : 0;
    var rad = 0.5 + mnr * 2.0;
    var col = smTypeColors[n.type] || 0xcccccc;
    var ei = 0.3 + mnr * 0.7;
    var g = new THREE.SphereGeometry(rad, 16, 12);
    var mat = new THREE.MeshPhongMaterial({ color: col, emissive: col, emissiveIntensity: ei, shininess: 30 });
    var mesh = new THREE.Mesh(g, mat);
    mesh.position.set(p.x, p.y, p.z);
    mesh.userData.nodeData = n;
    mesh.userData.nodeId = n.id;
    mesh.userData.baseEmissive = ei;
    // Glow sphere
    var gr = rad * 1.2 + mnr * 0.5;
    var gg = new THREE.SphereGeometry(gr, 16, 12);
    var gm = new THREE.MeshBasicMaterial({ color: col, transparent: true, opacity: 0.12 + mnr * 0.08, side: THREE.BackSide, blending: THREE.AdditiveBlending });
    var gs = new THREE.Mesh(gg, gm);
    mesh.add(gs);
    mesh.userData.glowSphere = gs;
    // Label sprite
    var canvas = document.createElement('canvas');
    canvas.width = 256;
    canvas.height = 64;
    var ctx = canvas.getContext('2d');
    ctx.clearRect(0, 0, 256, 64);
    ctx.font = 'Bold 24px Courier New';
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.shadowColor = '#aaccff';
    ctx.shadowBlur = 8;
    ctx.fillStyle = '#ffffff';
    ctx.fillText((n.name || n.id).substring(0, 12), 128, 32);
    var tex = new THREE.CanvasTexture(canvas);
    tex.needsUpdate = true;
    var spMat = new THREE.SpriteMaterial({ map: tex, transparent: true, opacity: 0.9, depthTest: false, depthWrite: false, blending: THREE.AdditiveBlending });
    var sprite = new THREE.Sprite(spMat);
    sprite.scale.set(8, 2, 1);
    sprite.position.y = rad + 2;
    mesh.add(sprite);
    starmapScene.add(mesh);
    starmapNodeMeshes.push(mesh);
  });
  // Create edges
  starmapEdges.forEach(function(e) {
    var a = pos[e.source_id], b = pos[e.target_id];
    if (!a || !b) return;
    var col = smEdgeColors[e.relation_type] || smEdgeColors[e.type] || 0x444466;
    var pts = [
      new THREE.Vector3(a.x, a.y, a.z),
      new THREE.Vector3(b.x, b.y, b.z)
    ];
    var geo = new THREE.BufferGeometry().setFromPoints(pts);
    var mat = new THREE.LineBasicMaterial({ color: col, transparent: true, opacity: 0.4 });
    var line = new THREE.Line(geo, mat);
    line.userData = { edgeId: e.id, edgeData: e };
    starmapScene.add(line);
    starmapEdgeLines.push(line);
  });
}

async function sendChat() {
  var inp = document.getElementById('chat-input');
  var btn = document.getElementById('chat-send-btn');
  var text = inp.value.trim();
  if (!text || state.chatLoading) return;
  state.chatStick = true;
  state.chatFinalIdx = -1;
  state.messages.push({ role: 'user', content: text });
  inp.value = '';
  rerenderChat();
  state.chatLoading = true;
  state.chatStage = __('等待AI回复...','Waiting for AI...');
  btn.disabled = true;
  btn.textContent = '';
  rerenderChat();
  try {
    var r = await api('/chat', { method: 'POST', body: JSON.stringify({ message: text }) });
    state.chatStage = '';
    var last = state.messages[state.messages.length - 1];
    console.log('[sendChat] POST returned, last msg:', last ? {role:last.role, _streaming:last._streaming, _final:last._final, tool_calls:last.tool_calls?.length, content_len:last.content?.length} : null);
    if (last && last.role === 'assistant' && last._streaming) {
      console.log('[sendChat] updating existing streaming msg, tool_calls before:', last.tool_calls?.length);
      last.content = r.response || __('(无响应)','(no response)');
      last._grow = true;
      if (!last.reasoning_content) last.reasoning_content = r.reasoning_content || '';
      last._final = true;
      delete last._streaming;
    } else {
      state.messages.push({
        role: 'assistant',
        content: r.response || __('(无响应)','(no response)'),
        reasoning_content: r.reasoning_content,
        tool_calls: last && last.role === 'assistant' && last.tool_calls ? last.tool_calls : [],
        _final: true,
        _grow: true
      });
    }
    state.chatFinalIdx = state.messages.length - 1;
    rerenderChat();
  } catch(e) {
    state.messages.push({ role: 'assistant', content: __('错误: ','Error: ') + e.message, _final: true });
    rerenderChat();
    toast(__('请求失败: ','Request failed: ') + e.message, true);
  } finally {
    state.chatLoading = false;
    state.chatStage = '';
    btn.disabled = false;
    btn.textContent = __('发送','Send');
    rerenderChat();
  }
}

async function queryMemoryChat() {
  var q = document.getElementById('mem-query')?.value;
  var r = document.getElementById('mem-result-chat');
  if (!r || !q) return;
  r.innerHTML = '<div class="loading"></div>';
  try {
    var data = await api('/memory?q=' + encodeURIComponent(q) + '&depth=2');
    r.innerHTML = '<pre style="font-size:11px">' + escHtml(JSON.stringify(data, null, 2)) + '</pre>';
  } catch(e) {
    r.innerHTML = '<p style="color:#fca5a5">' + __('查询失败: ','Query failed: ') + escHtml(e.message) + '</p>';
  }
}

async function queryMemoryContext() {
  var q = document.getElementById('ctx-query')?.value;
  var r = document.getElementById('ctx-result');
  if (!r) return;
  r.innerHTML = '<div class="loading"></div>';
  try {
    var data = await api('/memory/context?q=' + encodeURIComponent(q || ''));
    var ctx = data?.context || __('无上下文','No context');
    var summary = data?.summary || '';
    var entities = data?.entities || [];
    var tk = data?.token_estimate || 0;
    var html = '<div style="font-size:11px">';
    if (summary) html += '<div class="kv-row"><span class="key">' + __('摘要','Summary') + '</span><span class="val">' + escHtml(summary) + '</span></div>';
    html += '<div class="kv-row"><span class="key">Token ' + __('预估','Estimate') + '</span><span class="val">' + tk + '</span></div>';
    if (entities.length) {
      html += '<div class="kv-row"><span class="key">' + __('实体','Entities') + '</span><span class="val">'
        + entities.map(function(e) { return escHtml(e.name || e.id || '') }).join(', ')
        + '</span></div>';
    }
    html += '<pre style="font-size:11px;margin-top:8px">' + escHtml(ctx) + '</pre></div>';
    r.innerHTML = html;
  } catch(e) {
    r.innerHTML = '<p style="color:#fca5a5">' + __('获取失败: ','Get failed: ') + escHtml(e.message) + '</p>';
  }
}

async function searchKnowledgeChat() {
  var q = document.getElementById('know-query')?.value;
  var r = document.getElementById('know-result-chat');
  if (!r || !q) return;
  r.innerHTML = '<div class="loading"></div>';
  try {
    var data = await api('/knowledge?q=' + encodeURIComponent(q));
    r.innerHTML = '<pre style="font-size:11px">' + escHtml(JSON.stringify(data, null, 2)) + '</pre>';
  } catch(e) {
    r.innerHTML = '<p style="color:#fca5a5">' + __('搜索失败: ','Search failed: ') + escHtml(e.message) + '</p>';
  }
}

async function createKnowledgeChat() {
  var name = document.getElementById('know-name')?.value;
  var content = document.getElementById('know-content')?.value;
  if (!name || !content) { toast(__('名称和内容不能为空','Name and content cannot be empty'), true); return }
  try {
    var r = await api('/knowledge', { method: 'POST', body: JSON.stringify({ name: name, content: content }) });
    if (r.status || r.id) {
      toast(__('知识「','Knowledge "') + name + __('」已创建','" created'));
      document.getElementById('know-name').value = '';
      document.getElementById('know-content').value = '';
    } else {
      toast(__('创建失败','Create failed'), true);
    }
  } catch(e) {
    toast(__('创建失败: ','Create failed: ') + e.message, true);
  }
}

function switchChatPanel(tab, el) {
  var panels = {
    'chat': document.getElementById('chat-panel-chat'),
    'starmap': document.getElementById('chat-panel-starmap'),
    'terminal': document.getElementById('chat-panel-terminal'),
    'cmd': document.getElementById('chat-panel-cmd'),
    'memory': document.getElementById('chat-panel-memory'),
    'context': document.getElementById('chat-panel-context'),
    'knowledge': document.getElementById('chat-panel-knowledge')
  };
  Object.keys(panels).forEach(function(k) {
    var p = panels[k];
    if (p) p.classList.toggle('active', k === tab);
  });
  if (el) {
    var parent = el.parentElement;
    if (parent) {
      Array.from(parent.children).forEach(function(ch) { ch.classList.remove('active') });
      el.classList.add('active');
    }
  }
  if (tab === 'starmap') { renderChatStarmap(); onStarmapResize(); }
  if (tab === 'terminal') renderTerminals();
  if (tab === 'cmd') renderCmdHistory();
  if (tab === 'memory') queryMemoryChat();
  if (tab === 'context') queryMemoryContext();
  if (tab === 'knowledge') searchKnowledgeChat();
}

async function loadChatHistory() {
  try { var data = await api('/chat/history'); if (data && data.messages) state.messages = data.messages } catch(e) {}
}

async function loadTerminals() {
  try { var data = await api('/terminals'); if (data && data.terminals) state.terminals = data.terminals } catch(e) {}
}

async function loadCmdHistory() {
  try { var data = await api('/cmd/history'); if (data && data.history) state.cmdHistory = data.history } catch(e) {}
}

function appendTermBuf(el, text) {
  if (!text) return;
  el.textContent += text;
  if (el.textContent.length > 262144) {
    el.textContent = el.textContent.slice(el.textContent.length - 262144);
  }
  el.scrollTop = el.scrollHeight;
}

function renderTerminals() {
  var r = document.getElementById('term-list');
  var cnt = document.getElementById('term-count-badge');
  if (!r) return;
  var list = state.terminals || [];
  if (cnt) cnt.textContent = list.length;
  if (list.length === 0) {
    r.innerHTML = '<p style="color:var(--text-muted);padding:8px;text-align:center;font-size:11px">' + __('暂无终端会话','No terminal sessions') + '</p>';
    return;
  }
  var html = '';
  list.forEach(function(t, i) {
    var detailId = 'term-detail-' + i;
    var scr = (state.termScreens && state.termScreens[t.id]) || null;
    var running = scr ? scr.running : !!t.running;
    var fullOut = scr ? scr.output : t.output || '';
    if (!fullOut) {
      fullOut = '<span style="color:#5c6672">' + __('[终端暂无输出]','[No terminal output]') + '</span>';
    } else {
      fullOut = escHtml(fullOut);
    }
    html += '<div style="border:1px solid var(--border-color);border-radius:6px;margin-bottom:4px;font-size:11px">';
    html += '<div style="display:flex;align-items:center;gap:6px;padding:6px 8px;cursor:pointer;background:var(--bg-hover)" onclick="var d=document.getElementById(\'' + detailId + '\');d.style.display=d.style.display===\'none\'?\'block\':\'none\'">';
    html += '<span style="font-family:monospace;font-size:10px;flex:1">' + escHtml(t.id || '-') + '</span>';
    html += '<span style="flex:1;color:var(--text-muted);overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + escHtml(t.command || '') + '</span>';
    html += '<span class="badge ' + (running ? 'badge-green' : 'badge-red') + '">' + (running ? __('运行中','Running') : __('已关闭','Closed')) + '</span>';
    html += '<span style="color:var(--text-muted);font-size:10px">' + escHtml(t.created_at || '') + '</span>';
    html += '</div>';
    html += '<div id="' + detailId + '" style="display:none;padding:8px;border-top:1px solid var(--border-color);background:var(--bg-input)">';
    html += '<div class="term-screen">';
    html += '<div class="term-head"><span class="term-dot' + (running ? '' : ' stopped') + '" id="term-dot-' + escHtml(t.id) + '"></span><span style="font-weight:600">' + escHtml(t.id) + '</span><span>' + escHtml(t.command || '') + '</span><span style="flex:1"></span><span>' + escHtml(t.uptime || '') + '</span></div>';
    html += '<pre class="term-buf" id="term-buf-' + escHtml(t.id) + '">' + fullOut + '</pre>';
    html += '</div></div></div>';
  });
  r.innerHTML = html;
}

function renderCmdHistory() {
  var r = document.getElementById('cmd-list');
  var cnt = document.getElementById('cmd-count-badge');
  if (!r) return;
  var running = (state.terminals || []).filter(function(t) { return t.running; });
  if (cnt) cnt.textContent = running.length;
  if (running.length === 0) {
    r.innerHTML = '<p style="color:var(--text-muted);padding:8px;text-align:center;font-size:11px">' + __('暂无运行中的命令','No running commands') + '</p>';
    return;
  }
  var html = '<table style="font-size:10px"><tr><th>' + __('命令','Command') + '</th><th>' + __('状态','Status') + '</th><th>' + __('运行时长','Uptime') + '</th></tr>';
  running.forEach(function(t) {
    var scr = (state.termScreens && state.termScreens[t.id]) || null;
    var out = scr ? scr.output : t.output || '';
    html += '<tr>'
      + '<td style="font-family:monospace;max-width:200px;overflow:hidden;text-overflow:ellipsis">' + escHtml(t.command || t.id || '') + '</td>'
      + '<td><span class="badge badge-green">' + __('运行中','Running') + '</span></td>'
      + '<td style="color:var(--text-muted);white-space:nowrap">' + escHtml(t.uptime || '-') + '</td>'
      + '</tr>';
    if (out) {
      html += '<tr><td colspan="3" style="padding:0"><pre style="margin:0;padding:4px 8px;max-height:120px;overflow:auto;background:var(--bg-input);border-radius:4px;font-size:10px;color:var(--text-secondary)">' + escHtml(out.substring(0, 2000)) + '</pre></td></tr>';
    }
  });
  html += '</table>';
  r.innerHTML = html;
}

// ===== Plugins =====
function renderPlugins() {
  var k = state.kernel;
  var plugins = k?.plugins || [];
  var tools = k?.tools || [];
  var installed = state.installedPlugins || [];
  var html = '<div class="card"><h2>' + __('安装插件','Install Plugin') + '</h2>'
    + '<div style="display:flex;gap:8px;margin-bottom:8px">'
    + '<input id="plugin-url" placeholder=".hmap ' + __('包下载 URL','Package URL') + '" style="flex:1" onkeydown="if(event.key==\'Enter\')installPlugin()">'
    + '<button class="btn btn-primary" onclick="installPlugin()">' + __('安装','Install') + '</button></div>'
    + '<div><input type="file" id="plugin-file" accept=".hmap" style="display:inline;width:auto" onchange="installPluginFile(this.files[0])">'
    + '<label for="plugin-file" class="btn btn-ghost" style="cursor:pointer">' + __('选择 .hmap 文件上传','Upload .hmap file') + '</label></div></div>';
  var installedNames = (state.installedPlugins || []).map(function(p) { return p.name });
  html += '<div class="card"><h2>' + __('已加载插件','Loaded Plugins') + ' (' + plugins.length + ')</h2>';
  if (plugins.length === 0) {
    html += '<div class="empty-state"><p>' + __('暂无已加载插件','No loaded plugins') + '</p></div>';
  } else {
    html += '<table><tr><th>' + __('名称','Name') + '</th><th>' + __('状态','Status') + '</th><th>' + __('操作','Actions') + '</th></tr>';
    plugins.forEach(function(p) {
      var isExternal = installedNames.indexOf(p.name) >= 0;
      html += '<tr><td>' + escHtml(p.name) + '</td>'
        + '<td><span class="badge badge-green">' + __('已加载','Loaded') + '</span></td>'
        + '<td>' + (isExternal ? '<button class="btn btn-sm btn-danger" onclick="removePlugin(\'' + escHtml(p.name) + '\')">' + __('卸载','Unload') + '</button>' : '<span class="badge badge-blue">' + __('内置','Built-in') + '</span>') + '</td></tr>';
    });
    html += '</table>';
  }
  html += '</div>';
  if (installed.length > 0) {
    html += '<div class="card"><h2>' + __('已安装外部插件','Installed Plugins') + ' (' + installed.length + ')</h2>'
      + '<table><tr><th>' + __('名称','Name') + '</th><th>' + __('版本','Version') + '</th><th>' + __('描述','Description') + '</th><th>' + __('操作','Actions') + '</th></tr>';
    installed.forEach(function(p) {
      html += '<tr><td>' + escHtml(p.name) + '</td>'
        + '<td>' + escHtml(p.version || '-') + '</td>'
        + '<td>' + escHtml((p.description || '').substring(0, 50)) + '</td>'
        + '<td><button class="btn btn-sm btn-ghost" onclick="showPluginInfo(\'' + escHtml(p.name) + '\')">' + __('详情','Details') + '</button> '
        + '<button class="btn btn-sm btn-danger" onclick="removePlugin(\'' + escHtml(p.name) + '\')">' + __('卸载','Unload') + '</button></td></tr>';
    });
    html += '</table></div>';
  }
  if (state.pluginInfo) {
    html += '<div class="card"><h2>' + __('插件详情','Plugin Details') + ': ' + escHtml(state.pluginInfo.name) + '</h2>'
      + '<pre>' + escHtml(JSON.stringify(state.pluginInfo, null, 2)) + '</pre>'
      + '<button class="btn btn-ghost" onclick="closePluginInfo()">' + __('关闭','Close') + '</button></div>';
  }
  if (tools.length > 0) {
    html += '<div class="card"><h2>' + __('已注册工具','Registered Tools') + ' (' + tools.length + ')</h2>'
      + '<div style="display:flex;flex-wrap:wrap;gap:4px">';
    tools.forEach(function(t) {
      html += '<span class="tool-badge" title="' + escHtml(t.description || '') + '">' + escHtml(t.name) + '</span>';
    });
    html += '</div></div>';
  }
  html += '<div class="card"><h2>' + __('系统操作','System Operations') + '</h2>'
    + '<button class="btn btn-primary" onclick="reloadPlugins()" style="margin-right:8px">' + __('重载插件','Reload Plugins') + '</button>'
    + '<button class="btn btn-ghost" onclick="runHealthcheck()" style="margin-right:8px">' + __('健康检查','Health Check') + '</button></div>';
  html += '<div class="card"><h2>' + __('健康检查','Health Check') + '</h2><div id="health-panel">';
  if (state.healthResult) {
    html += renderHealthResult(state.healthResult);
  } else {
    html += '<p style="color:var(--text-muted);font-size:13px">' + __('点击上方按钮运行','Click the button above to run') + '</p>';
  }
  html += '</div></div>';
  document.getElementById('view-plugins').innerHTML = html;
}

async function loadInstalledPlugins() {
  try { state.installedPlugins = await api('/plugins') } catch(e) { state.installedPlugins = [] }
}

async function installPlugin() {
  var inp = document.getElementById('plugin-url');
  var url = inp?.value.trim();
  if (!url) { toast(__('请输入插件包 URL','Please enter plugin URL'), true); return }
  try {
    var r = await api('/plugins', { method: 'POST', body: JSON.stringify({ url: url }) });
    toast(__('安装结果: ','Install result: ') + (r.status || JSON.stringify(r)));
    if (r.action === 'reload_required') toast(__('已安装，请点击「重载插件」加载','Installed, click "Reload Plugins" to load'), false);
    loadInstalledPlugins(); renderPlugins();
  } catch(e) { toast(__('安装失败: ','Install failed: ') + e.message, true) }
}

async function installPluginFile(file) {
  if (!file) return;
  try {
    var r = await fetch('/api/v1/plugins', { method: 'POST', body: file, headers: { 'Content-Type': 'application/octet-stream' } });
    var data = await r.json();
    toast(__('上传安装: ','Upload install: ') + (data.status || JSON.stringify(data)));
    if (data.action === 'reload_required') toast(__('已安装，请点击「重载插件」加载','Installed, click "Reload Plugins" to load'), false);
    loadInstalledPlugins(); renderPlugins();
  } catch(e) { toast(__('上传失败: ','Upload failed: ') + e.message, true) }
}

async function showPluginInfo(name) {
  try { state.pluginInfo = await api('/plugins/' + encodeURIComponent(name)); renderPlugins() } catch(e) { toast(__('获取详情失败: ','Get details failed: ') + e.message, true) }
}

function closePluginInfo() { state.pluginInfo = null; renderPlugins() }

async function removePlugin(name) {
  if (!confirm(__('确定卸载插件','Are you sure to unload plugin') + '「' + name + '」？')) return;
  try {
    var r = await api('/plugins/' + encodeURIComponent(name), { method: 'DELETE' });
    toast(__('已卸载: ','Unloaded: ') + (r.status || r.name));
    if (r.action === 'reload_required') toast(__('已卸载，请点击「重载插件」生效','Unloaded, click "Reload Plugins" to apply'), false);
    loadInstalledPlugins(); renderPlugins();
  } catch(e) { toast(__('卸载失败: ','Unload failed: ') + e.message, true) }
}

async function reloadPlugins() {
  try {
    var r = await api('/plugins/reload', { method: 'POST' });
    toast(__('插件已重载','Plugins reloaded'));
    state.kernel = await api('/kernel');
    renderPlugins();
  } catch(e) { toast(__('重载失败: ','Reload failed: ') + e.message, true) }
}

async function runHealthcheck() {
  var panel = document.getElementById('health-panel');
  if (!panel) return;
  panel.innerHTML = '<div class="loading" style="margin:12px auto"></div><p style="text-align:center;color:var(--text-muted)">' + __('运行中...','Running...') + '</p>';
  try {
    var r = await api('/kernel');
    var tools = r?.tools || [];
    var healthTool = tools.find(function(t) { return t.name === 'healthcheck' });
    if (!healthTool) { panel.innerHTML = '<p style="color:var(--text-secondary)">' + __('healthcheck 工具未注册','healthcheck tool not registered') + '</p>'; return }
    panel.innerHTML = '<p style="color:var(--text-secondary)">' + __('通过 Agent 对话触发 healthcheck...','Triggering healthcheck via Agent...') + '</p>';
    var chatR = await api('/chat', { method: 'POST', body: JSON.stringify({ message: __('请运行 healthcheck 工具进行全面健康检查并报告结果','Please run the healthcheck tool for a full system check and report the results') }) });
    panel.innerHTML = '<pre>' + escHtml(JSON.stringify(chatR, null, 2)) + '</pre>';
  } catch(e) { panel.innerHTML = '<p style="color:#fca5a5">' + __('错误: ','Error: ') + escHtml(e.message) + '</p>'; toast(__('健康检查失败: ','Health check failed: ') + e.message, true) }
}

function renderHealthResult(r) {
  if (!r || !r.checks) return '<p style="color:var(--text-secondary)">' + __('暂无健康检查数据','No health check data') + '</p>';
  var checks = r.checks || [];
  var passed = checks.filter(function(c) { return c.pass }).length;
  var failed = checks.filter(function(c) { return !c.pass }).length;
  var html = '<div style="margin-bottom:12px;display:flex;gap:16px;align-items:center">'
    + '<span class="badge badge-green">' + __('通过: ','Pass: ') + passed + '</span>'
    + '<span class="badge ' + (failed > 0 ? 'badge-red' : 'badge-green') + '">' + __('失败: ','Fail: ') + failed + '</span>'
    + '<span class="badge badge-blue">' + __('总计: ','Total: ') + checks.length + '</span></div>';
  checks.forEach(function(c) {
    var passClass = c.pass ? 'check-pass' : 'check-fail';
    if (c.status === 'skip') passClass = 'check-skip';
    html += '<div class="health-item">'
      + '<span class="check-name">' + escHtml(c.name) + '</span>'
      + '<span class="check-status ' + passClass + '">' + (c.status || 'unknown') + '</span>'
      + '<span style="color:var(--text-muted);font-size:11px">' + escHtml(c.detail || '') + '</span></div>';
  });
  return html;
}

// ===== Kernel =====
function renderKernel() {
  var k = state.kernel;
  if (!k) { document.getElementById('view-kernel').innerHTML = '<div class="card"><p style="color:var(--text-muted)">' + __('内核未响应','Kernel not responding') + '</p></div>'; return }
  var html = '<div class="card"><h2>' + __('运行时','Runtime') + '</h2><div class="grid-3">'
    + statCard('Goroutines', k?.runtime?.goroutines || '-', '')
    + statCard(__('内存','Memory'), k?.runtime?.memory_mb ? k.runtime.memory_mb + ' MB' : '-', '')
    + statCard('Go ' + __('版本','Version'), k?.runtime?.go_version || '-', '')
    + '</div></div>';
  html += '<div class="card"><h2>LLM</h2>'
    + '<div class="kv-row"><span class="key">Provider</span><span class="val">' + (k.llm?.provider || __('未配置','Not configured')) + '</span></div>'
    + '<div class="kv-row"><span class="key">' + __('可用源','Sources') + '</span><span class="val">' + (k.llm?.sources || 0) + '</span></div>'
    + '<div class="kv-row"><span class="key">' + __('状态','Status') + '</span><span class="val"><span class="status-dot ' + (k.llm?.available ? 'dot-green' : 'dot-red') + '"></span>' + (k.llm?.available ? __('运行中','Running') : __('不可用','Unavailable')) + '</span></div></div>';
  html += '<div class="card"><h2>' + __('记忆','Memory') + '</h2>'
    + '<div class="kv-row"><span class="key">' + __('图记忆','Graph Memory') + '</span><span class="val">' + (k.memory?.available ? k.memory.entity_count + __(' 实体, ',' entities, ') + k.memory.relation_count + __(' 关系',' relations') : __('未初始化','Uninitialized')) + '</span></div>'
    + '<div class="kv-row"><span class="key">' + __('文档记忆','Document Memory') + '</span><span class="val">' + (k.documents?.available ? k.documents.doc_count + __(' 文档',' docs') : __('未初始化','Uninitialized')) + '</span></div>'
    + '<div class="kv-row"><span class="key">' + __('文本记忆','Text Memory') + '</span><span class="val">' + (k.text_memory?.available ? k.text_memory.file_count + __(' 文件',' files') : __('未初始化','Uninitialized')) + '</span></div>'
    + '<div class="kv-row"><span class="key">' + __('知识库','Knowledge') + '</span><span class="val">' + (k.knowledge?.available ? k.knowledge.item_count + __(' 项',' items') : __('未初始化','Uninitialized')) + '</span></div></div>';
  html += '<div class="card"><h2>' + __('插件','Plugins') + ' (' + (k.plugins?.length || 0) + ')</h2>';
  if (k.plugins?.length) {
    html += '<div style="display:flex;flex-wrap:wrap;gap:4px">';
    k.plugins.forEach(function(p) { html += '<span class="badge badge-blue">' + escHtml(p.name) + '</span>' });
    html += '</div>';
  } else {
    html += '<p style="color:var(--text-muted)">' + __('无','None') + '</p>';
  }
  html += '</div>';
  document.getElementById('view-kernel').innerHTML = html;
}

// ===== Star Map =====
var starmapScene = null, starmapCam = null, starmapRen = null, starmapCtrl = null;
var starmapNodes = [], starmapEdges = [];
var starmapNodeMeshes = [], starmapEdgeLines = [], starmapStarField = null;
var starmapHovered = null, starmapSelected = null, starmapAutoView = true;
var starmapRaf = null;
var smTypeColors = { person: 0x4488ff, task: 0xff8844, ai: 0xaa44ff, concept: 0x44ff88, object: 0xff4444 };
var smEdgeColors = { '喜欢': 0xff6b6b, '学习': 0x4ecdc4, '属于': 0x45b7d1, '相关': 0x96ceb4, '使用': 0xfeca57, '创建': 0xff9ff3 };

function createStarField() {
  var c = 3000;
  var p = new Float32Array(c * 3), cl = new Float32Array(c * 3), s = new Float32Array(c);
  for (var i = 0; i < c; i++) {
    var i3 = i * 3;
    var r = 400 + Math.random() * 600, th = Math.random() * Math.PI * 2, ph = Math.acos(2 * Math.random() - 1);
    p[i3] = r * Math.sin(ph) * Math.cos(th);
    p[i3 + 1] = r * Math.sin(ph) * Math.sin(th);
    p[i3 + 2] = r * Math.cos(ph);
    if (Math.random() < 0.7) {
      cl[i3] = 0.8 + Math.random() * 0.2; cl[i3 + 1] = 0.8 + Math.random() * 0.2; cl[i3 + 2] = 1;
    } else {
      cl[i3] = 1; cl[i3 + 1] = 0.9 + Math.random() * 0.1; cl[i3 + 2] = 0.8 + Math.random() * 0.2;
    }
    s[i] = 0.5 + Math.random() * 2;
  }
  var g = new THREE.BufferGeometry();
  g.setAttribute('position', new THREE.BufferAttribute(p, 3));
  g.setAttribute('color', new THREE.BufferAttribute(cl, 3));
  g.setAttribute('size', new THREE.BufferAttribute(s, 1));
  var m = new THREE.PointsMaterial({ size: 1.5, vertexColors: true, transparent: true, opacity: 0.8, sizeAttenuation: true });
  starmapStarField = new THREE.Points(g, m);
  starmapScene.add(starmapStarField);
}

function onStarmapMove(e) {
  if (!starmapRen || !starmapCam) return;
  var rect = starmapRen.domElement.getBoundingClientRect();
  var mouse = new THREE.Vector2(((e.clientX - rect.left) / rect.width) * 2 - 1, -((e.clientY - rect.top) / rect.height) * 2 + 1);
  var rc = new THREE.Raycaster();
  rc.setFromCamera(mouse, starmapCam);
  var hits = rc.intersectObjects(starmapNodeMeshes);
  var infoEl = document.getElementById('starmap-info');
  if (hits.length > 0) {
    var n = hits[0].object;
    if (starmapHovered !== n) {
      if (starmapHovered) starmapHovered.scale.set(1, 1, 1);
      starmapHovered = n;
      n.scale.set(1.2, 1.2, 1.2);
      var nd = n.userData.nodeData;
      if (infoEl) {
        var e1 = document.getElementById('sm-info-name'); if (e1) e1.textContent = nd.name || '';
        var e2 = document.getElementById('sm-info-type'); if (e2) e2.textContent = nd.type || '';
        var e3 = document.getElementById('sm-info-mentions'); if (e3) e3.textContent = (nd.mention_count || 0) + '';
        var lk = starmapEdges.filter(function(e) { return e.source_id === nd.id || e.target_id === nd.id }).length;
        var e4 = document.getElementById('sm-info-links'); if (e4) e4.textContent = lk + '';
        infoEl.style.display = 'block';
      }
    }
  } else {
    if (starmapHovered) { starmapHovered.scale.set(1, 1, 1); starmapHovered = null }
    if (!starmapSelected && infoEl) infoEl.style.display = 'none';
  }
}

function onStarmapClick(e) {
  if (!starmapRen || !starmapCam) return;
  var rect = starmapRen.domElement.getBoundingClientRect();
  var mouse = new THREE.Vector2(((e.clientX - rect.left) / rect.width) * 2 - 1, -((e.clientY - rect.top) / rect.height) * 2 + 1);
  var rc = new THREE.Raycaster();
  rc.setFromCamera(mouse, starmapCam);
  var hits = rc.intersectObjects(starmapNodeMeshes);
  if (hits.length > 0) {
    var n = hits[0].object;
    starmapSelected = (starmapSelected === n) ? null : n;
    if (starmapAutoView && starmapSelected) flyStarmapTo(starmapSelected.userData.nodeId, 500);
    onStarmapMove(e);
  } else {
    starmapSelected = null;
  }
}

function flyStarmapTo(nodeId, dur) {
  if (!starmapAutoView) return;
  var m = starmapNodeMeshes.find(function(x) { return x.userData.nodeId === nodeId });
  if (!m) return;
  var tp = m.position.clone(), sp = starmapCam.position.clone(), st = starmapCtrl.target.clone();
  var dist = tp.length() + 25, ep = new THREE.Vector3(tp.x, tp.y + dist * 0.4, tp.z + dist * 0.8);
  var t0 = Date.now();
  (function lerp() {
    var t = Math.min((Date.now() - t0) / dur, 1), e = 1 - Math.pow(1 - t, 3);
    starmapCam.position.lerpVectors(sp, ep, e);
    starmapCtrl.target.lerpVectors(st, tp, e);
    if (t < 1) requestAnimationFrame(lerp);
  })();
}

function onStarmapResize() {
  if (!starmapRen || !starmapCam) return;
  var cont = starmapRen.domElement.parentElement;
  if (!cont) return;
  var rect = cont.getBoundingClientRect();
  var w = rect.width || 800, h = Math.max(rect.height || 250, 100);
  if (w > 0 && h > 0) { starmapCam.aspect = w / h; starmapCam.updateProjectionMatrix(); starmapRen.setSize(w, h) }
}

function toggleStarmapAuto() {
  starmapAutoView = !starmapAutoView;
  var b = document.getElementById('sm-auto-btn');
  if (b) b.className = starmapAutoView ? 'on' : '';
}

function resetStarmapCamera() {
  if (!starmapCam || !starmapCtrl || !starmapNodeMeshes) return;
  var maxD = 0;
  starmapNodeMeshes.forEach(function(m) { var d = m.position.length(); if (d > maxD) maxD = d });
  if (maxD < 1) maxD = 30;
  var td = Math.min(Math.max(maxD + 20, 30), 150);
  var sp = starmapCam.position.clone(), ep = new THREE.Vector3(td * 0.9, td * 0.6, td * 0.9);
  var st = starmapCtrl.target.clone(), t0 = Date.now();
  (function lerp() {
    var t = Math.min((Date.now() - t0) / 400, 1), e = 1 - Math.pow(1 - t, 3);
    starmapCam.position.lerpVectors(sp, ep, e);
    starmapCtrl.target.lerpVectors(st, new THREE.Vector3(0, 0, 0), e);
    if (t < 1) requestAnimationFrame(lerp);
  })();
}

function starmapAnimate() {
  starmapRaf = requestAnimationFrame(starmapAnimate);
  if (starmapCtrl) starmapCtrl.update();
  if (starmapStarField) starmapStarField.rotation.y += 0.0001;
  if (starmapRen && starmapScene && starmapCam) starmapRen.render(starmapScene, starmapCam);
}

function createNebula() {
  var nc = 500;
  var p = new Float32Array(nc * 3), cl = new Float32Array(nc * 3);
  for (var i = 0; i < nc; i++) {
    var i3 = i * 3;
    p[i3] = (Math.random() - 0.5) * 800;
    p[i3+1] = (Math.random() - 0.5) * 800;
    p[i3+2] = (Math.random() - 0.5) * 800;
    var ch = Math.random();
    if (ch < 0.33) {
      cl[i3]=0.5+Math.random()*0.3; cl[i3+1]=0.2+Math.random()*0.2; cl[i3+2]=0.7+Math.random()*0.3;
    } else if (ch < 0.66) {
      cl[i3]=0.2+Math.random()*0.2; cl[i3+1]=0.3+Math.random()*0.3; cl[i3+2]=0.8+Math.random()*0.2;
    } else {
      cl[i3]=0.7+Math.random()*0.3; cl[i3+1]=0.2+Math.random()*0.2; cl[i3+2]=0.5+Math.random()*0.3;
    }
  }
  var g = new THREE.BufferGeometry();
  g.setAttribute('position', new THREE.BufferAttribute(p, 3));
  g.setAttribute('color', new THREE.BufferAttribute(cl, 3));
  var m = new THREE.PointsMaterial({ size: 8, vertexColors: true, transparent: true, opacity: 0.15, sizeAttenuation: true, blending: THREE.AdditiveBlending });
  var np = new THREE.Points(g, m);
  starmapScene.add(np);
}

// ===== Settings =====
function pluginDisplayName(p) {
  if (p === 'core') return __('核心', 'Core');
  var name = p.replace('plugin.', '');
  var meta = state.pluginMeta && state.pluginMeta[name];
  if (meta) return state.lang === 'en' ? (meta.name_en || name) : (meta.name_zh || name);
  return name;
}

function renderSettingsTabs() {
  var el = document.getElementById('settings-tabs');
  if (!el) return;
  el.innerHTML = '';
  state.settingsPlugins.forEach(function(p) {
    var s = document.createElement('span');
    s.textContent = pluginDisplayName(p);
    if (p === state.selectedSection) s.className = 'active';
    s.onclick = function() { state.selectedSection = p; renderOneSettings() };
    el.appendChild(s);
  });
}

function renderOneSettings() {
  var prefix = state.selectedSection + '.';
  var allKeys = Object.keys(state.settings || {});
  var filtered = allKeys.filter(function(k) { return k === prefix.slice(0, -1) || k.startsWith(prefix) });
  filtered.sort();
  var hideTopLlms = ['core.llm.base_url','core.llm.model','core.llm.api_key','core.llm.adapter','core.llm.adapter_path','core.llm.thinking_enabled'];
  var sourceKeys = filtered.filter(function(k) { return k.startsWith('core.llm.sources.') });
  var sourceMap = {};
  sourceKeys.forEach(function(k) {
    var parts = k.split('.');
    var srcName = parts[3];
    if (!sourceMap[srcName]) sourceMap[srcName] = {};
    sourceMap[srcName][k] = true;
  });
  var mcpServerKeys = filtered.filter(function(k) { return k.startsWith('plugin.mcp.servers.') && k.split('.').length >= 5 });
  var mcpServerMap = {};
  mcpServerKeys.forEach(function(k) {
    var parts = k.split('.');
    var srvName = parts[3];
    if (!mcpServerMap[srvName]) mcpServerMap[srvName] = {};
    mcpServerMap[srvName][k] = true;
  });
  var regularKeys = filtered.filter(function(k) {
    return !k.startsWith('core.llm.sources.') && hideTopLlms.indexOf(k) === -1 && !k.startsWith('plugin.mcp.servers.') && k !== 'plugin.mcp.servers';
  });
  var html = '<div class="card"><h2>' + __('后端连接','Backend Connections') + '</h2>'
    + '<div id="conn-manager"></div></div>'
    + '<div class="settings-tabs" id="settings-tabs"></div>'
    + '<div class="settings-content">';
  if (regularKeys.length === 0 && Object.keys(sourceMap).length === 0 && Object.keys(mcpServerMap).length === 0 && state.selectedSection !== 'plugin.mcp') {
    html += '<div class="card"><h2>' + escHtml(state.selectedSection) + '</h2><p style="color:var(--text-muted)">' + __('暂无设置项','No settings') + '</p></div>';
  } else {
    regularKeys.forEach(function(k) {
      var v = state.settings[k];
      var sv = typeof v === 'object' ? JSON.stringify(v) : String(v);
      var m = state.meta?.[k];
      var shortName = k.split('.').pop().replace(/_/g, ' ');
      var label = m?.display_name || shortName;
      var desc = m?.description || '';
      var typ = m?.type || 'string';
      var ph = m?.placeholder || '';
      var opts = m?.options || [];
      var inpId = 'inp-' + k.replace(/\./g, '_');
      var inp = '';
      if (typ === 'bool') {
        var chk = sv === 'true' ? 'checked' : '';
        inp = '<label style="display:flex;align-items:center;gap:8px;cursor:pointer"><input type="checkbox" id="' + inpId + '" ' + chk + ' onchange="markDirty(\'' + k + '\')" style="width:auto;margin:0"> ' + label + '</label>';
      } else if (typ === 'select') {
        var selOpts = '';
        opts.forEach(function(o) { selOpts += '<option value="' + o + '"' + (sv === o ? ' selected' : '') + '>' + o + '</option>' });
        inp = '<label>' + label + '</label><select id="' + inpId + '" onchange="markDirty(\'' + k + '\')">' + selOpts + '</select>';
      } else if (typ === 'text') {
        inp = '<label>' + label + '</label><textarea id="' + inpId + '" placeholder="' + escHtml(ph) + '" onchange="markDirty(\'' + k + '\')">' + escHtml(sv) + '</textarea>';
      } else {
        inp = '<label>' + label + '</label><input type="text" id="' + inpId + '" value="' + escHtml(sv) + '" placeholder="' + escHtml(ph) + '" onchange="markDirty(\'' + k + '\')">';
      }
      var extra = '';
      if (m?.extra) {
        m.extra.forEach(function(f) {
          var fk = (k ? k + '.' : '') + f.key;
          var fv = state.settings?.[fk];
          var fph = f.placeholder || __('输入','Enter ') + f.label;
          extra += '<div class="form-row" style="margin-left:16px;margin-top:4px"><label>' + f.label + '</label>';
          if (f.type === 'select') {
            var fopts = '';
            if (f.options) f.options.forEach(function(o) { fopts += '<option value="' + o + '"' + (fv === o ? 'selected' : '') + '>' + o + '</option>' });
            extra += '<select onchange="saveSetting(\'' + fk + '\',this.value)">' + fopts + '</select>';
          } else {
            extra += '<input type="text" value="' + escHtml(fv || '') + '" placeholder="' + escHtml(fph) + '" onchange="markDirty(\'' + fk + '\')" style="margin-bottom:0">';
          }
          extra += '</div>';
        });
      }
      var descHtml = desc ? '<p style="font-size:11px;color:var(--text-muted);margin:-6px 0 10px">' + escHtml(desc) + '</p>' : '';
      html += '<div class="card"><div class="settings-key">' + escHtml(k) + '</div>' + inp + descHtml + extra
        + '<button class="btn btn-sm btn-ghost" style="border-color:var(--save-btn-border);margin-top:4px" onclick="saveSetting(\'' + k + '\')">' + __('保存','Save') + '</button></div>';
    });
    // LLM Sources
    Object.keys(sourceMap).sort().forEach(function(src) {
      var baseKey = 'core.llm.sources.' + src;
      var srcData = state.settings?.[baseKey + '.adapter'] || state.settings?.[baseKey + '.base_url'] || '';
      var fields = [
        { key: 'adapter', label: __('适配器','Adapter'), type: 'text' },
        { key: 'base_url', label: 'Base URL', type: 'text' },
        { key: 'model', label: __('模型','Model'), type: 'text' },
        { key: 'api_key', label: 'API Key', type: 'text' },
        { key: 'thinking_enabled', label: __('思考模式','Thinking Mode'), type: 'select', options: ['true','false'] },
        { key: 'adapter_path', label: __('适配器路径','Adapter Path'), type: 'text' }
      ];
      var headerLabel = mL10n(src, 'LLM Source: ' + src);
      html += '<div class="card"><h2>' + escHtml(headerLabel) + '</h2>';
      fields.forEach(function(f) {
        var fk = baseKey + '.' + f.key;
        var fv = state.settings?.[fk] || '';
        var flabel = f.label;
        var fieldId = 'inp-' + fk.replace(/\./g, '_');
        if (f.type === 'select') {
          var fopts = '';
          f.options.forEach(function(o) { fopts += '<option value="' + o + '"' + (fv === o ? 'selected' : '') + '>' + o + '</option>' });
          html += '<label>' + flabel + '</label><select id="' + fieldId + '" onchange="markDirty(\'' + fk + '\')" style="margin-bottom:4px">' + fopts + '</select>';
        } else {
          html += '<label>' + flabel + '</label><input type="text" id="' + fieldId + '" value="' + escHtml(fv) + '" placeholder="' + (f.key === 'api_key' ? __('输入 API Key','Enter API Key') : __('输入','Enter ') + flabel) + '" onchange="markDirty(\'' + fk + '\')" style="margin-bottom:4px">';
        }
      });
      html += '<div style="display:flex;gap:8px;margin-top:8px">'
        + '<button class="btn btn-sm btn-ghost" style="border-color:var(--save-btn-border)" onclick="saveSetting(\'' + baseKey + '.adapter\');saveSetting(\'' + baseKey + '.base_url\');saveSetting(\'' + baseKey + '.model\');saveSetting(\'' + baseKey + '.api_key\');var inp=document.getElementById(\'' + ('inp-' + baseKey + '.thinking_enabled').replace(/\./g, '_') + '\');if(inp)saveSetting(\'' + baseKey + '.thinking_enabled\');toast(\'' + __('源','Source') + ' \\\'' + src + '\\\' ' + __('已保存','saved') + '\')">' + __('保存','Save') + '</button>'
        + '<button class="btn btn-sm btn-danger" onclick="deleteSource(\'' + src + '\')">' + __('删除','Delete') + '</button></div></div>';
    });
    if (Object.keys(sourceMap).length > 0 || state.selectedSection === 'core.llm') {
      html += '<button class="btn btn-ghost btn-sm" onclick="showAddSourceDialog()" style="margin-bottom:16px">+ ' + __('添加 LLM 源','Add LLM Source') + '</button>';
    }
    // MCP Servers
    if (state.selectedSection === 'plugin.mcp' || Object.keys(mcpServerMap).length > 0) {
      html += '<div class="card"><h2>' + __('MCP 服务器','MCP Servers') + '</h2><p style="font-size:11px;color:var(--text-muted);margin-bottom:8px">' + __('配置 Model Context Protocol 服务端连接','Configure Model Context Protocol server connections') + '</p></div>';
      Object.keys(mcpServerMap).sort().forEach(function(srv) {
        var baseKey = 'plugin.mcp.servers.' + srv;
        var fields = [
          { key: 'command', label: __('启动命令','Command'), type: 'text' },
          { key: 'url', label: 'SSE URL', type: 'text' },
          { key: 'args', label: __('参数(JSON数组)','Args (JSON array)'), type: 'text' },
          { key: 'env', label: __('环境变量(JSON数组)','Env (JSON array)'), type: 'text' }
        ];
        html += '<div class="card"><h2>' + escHtml(srv) + '</h2>';
        fields.forEach(function(f) {
          var fk = baseKey + '.' + f.key;
          var fv = state.settings?.[fk] || '';
          var fieldId = 'inp-' + fk.replace(/\./g, '_');
          html += '<label>' + f.label + '</label><input type="text" id="' + fieldId + '" value="' + escHtml(fv) + '" placeholder="' + __('输入','Enter ') + f.label + '" onchange="markDirty(\'' + fk + '\')" style="margin-bottom:4px">';
        });
        html += '<div style="display:flex;gap:8px;margin-top:8px">'
          + '<button class="btn btn-sm btn-ghost" style="border-color:var(--save-btn-border)" onclick="saveSetting(\'' + baseKey + '.command\');saveSetting(\'' + baseKey + '.url\');saveSetting(\'' + baseKey + '.args\');saveSetting(\'' + baseKey + '.env\');toast(\'MCP \\\'' + srv + '\\\' ' + __('已保存','saved') + '\')">' + __('保存','Save') + '</button>'
          + '<button class="btn btn-sm btn-danger" onclick="deleteMCPServer(\'' + srv + '\')">' + __('删除','Delete') + '</button></div></div>';
      });
      html += '<button class="btn btn-ghost btn-sm" onclick="addMCPSource()" style="margin-bottom:16px">+ ' + __('添加 MCP 服务器','Add MCP Server') + '</button>';
    }
  }
  html += '</div></div>';
  document.getElementById('view-settings').innerHTML = html;
  renderSettingsTabs();
  renderConnSection();
}

function markDirty(k) {
  var inp = document.getElementById('inp-' + k.replace(/\./g, '_'));
  if (inp) inp.style.borderColor = 'var(--save-btn-border)';
}

async function saveSetting(k) {
  var inp = document.getElementById('inp-' + k.replace(/\./g, '_'));
  if (!inp) return;
  var val;
  var m = state.meta?.[k];
  if (m?.type === 'bool') { val = inp.checked ? 'true' : 'false' }
  else if (m?.type === 'select') { val = inp.value }
  else { var raw = inp.value; try { val = JSON.parse(raw) } catch(e) { val = raw } }
  try {
    var r = await api('/settings', { method: 'PUT', body: JSON.stringify({ key: k, value: val }) });
    if (r.status === 'ok') {
      inp.style.borderColor = '';
      state.settings[k] = val;
      toast(__('已保存: ','Saved: ') + k);
    } else {
      toast(__('保存失败: ','Save failed: ') + (r.error || 'unknown'), true);
    }
  } catch(e) { toast(__('保存失败: ','Save failed: ') + e.message, true) }
}

function renderConfigDisabled() {
  document.getElementById('view-settings').innerHTML = '<div class="card"><h2>' + __('设置','Settings') + '</h2><p style="color:var(--text-muted)">' + __('设置面板已加载','Settings panel loaded') + '</p></div>';
  renderOneSettings();
}

function mL10n(key, fallback) {
  var meta = state.meta?.[key];
  if (meta?.display_name) return meta.display_name;
  return fallback || key;
}

function showAddSourceDialog() {
  var name = prompt(__('输入新 LLM 源名称（如 openai、anthropic）:','Enter new LLM source name (e.g. openai, anthropic):'));
  if (!name || !name.trim()) return;
  name = name.trim().toLowerCase().replace(/[^a-z0-9_]/g, '_');
  if (!name) { toast(__('名称无效','Invalid name'), true); return }
  var keys = ['base_url','model','api_key','adapter','adapter_path','thinking_enabled'];
  var values = { base_url: 'https://api.' + name + '.com', model: '', api_key: '', adapter: name, adapter_path: '', thinking_enabled: 'false' };
  var promises = keys.map(function(f) {
    return api('/settings', { method: 'PUT', body: JSON.stringify({ key: 'core.llm.sources.' + name + '.' + f, value: values[f] }) });
  });
  Promise.all(promises).then(function() { toast(__('源','Source') + ' "' + name + '" ' + __('已创建，请配置各项参数','created, please configure parameters')); renderAll() }).catch(function(e) { toast(__('创建失败: ','Create failed: ') + e.message, true) });
}

async function deleteSource(name) {
  if (!confirm(__('确认删除源','Are you sure to delete source') + ' "' + name + '"?')) return;
  var base = 'core.llm.sources.' + name;
  var fields = ['adapter','base_url','model','api_key','thinking_enabled','adapter_path'];
  try {
    for (var f of fields) { await api('/settings', { method: 'PUT', body: JSON.stringify({ key: base + '.' + f, value: null }) }) }
    toast(__('源','Source') + ' "' + name + '" ' + __('已删除','deleted')); renderAll();
  } catch(e) { toast(__('删除失败: ','Delete failed: ') + e.message, true) }
}

function addMCPSource() {
  var name = prompt(__('输入新 MCP 服务器名称:','Enter new MCP server name:'));
  if (!name || !name.trim()) return;
  name = name.trim().toLowerCase().replace(/[^a-z0-9_]/g, '_');
  if (!name) { toast(__('名称无效','Invalid name'), true); return }
  var fields = ['command','url','args','env'];
  var values = { command: '', url: '', args: '[]', env: '[]' };
  var promises = fields.map(function(f) {
    return api('/settings', { method: 'PUT', body: JSON.stringify({ key: 'plugin.mcp.servers.' + name + '.' + f, value: values[f] }) });
  });
  Promise.all(promises).then(function() { toast('MCP ' + __('服务器','server') + ' "' + name + '" ' + __('已创建','created')); renderAll() }).catch(function(e) { toast(__('创建失败: ','Create failed: ') + e.message, true) });
}

async function deleteMCPServer(name) {
  if (!confirm(__('确认删除 MCP 服务器','Are you sure to delete MCP server') + ' "' + name + '"?')) return;
  var fields = ['command','url','args','env'];
  try {
    for (var f of fields) { await api('/settings', { method: 'PUT', body: JSON.stringify({ key: 'plugin.mcp.servers.' + name + '.' + f, value: null }) }) }
    toast('MCP ' + __('服务器','server') + ' "' + name + '" ' + __('已删除','deleted')); renderAll();
  } catch(e) { toast(__('删除失败: ','Delete failed: ') + e.message, true) }
}

// ===== Adapters =====
async function renderAdapters() {
  var html = '';
  try {
    var r = await api('/adapters');
    var adapters = r.adapters || [];
    window._adapters = adapters;
    html += '<div class="card"><h2>' + __('已加载的适配器','Loaded Adapters') + ' (' + adapters.length + ')</h2>';
    if (adapters.length === 0) {
      html += '<p style="color:var(--text-muted)">' + __('暂无适配器','No adapters') + '</p>';
    } else {
      html += '<table><tr><th>' + __('名称','Name') + '</th><th>' + __('版本','Version') + '</th><th>' + __('操作','Actions') + '</th></tr>';
      adapters.forEach(function(a) {
        html += '<tr><td>' + escHtml(a.name) + '</td><td>' + escHtml(a.version || '-') + '</td>'
          + '<td><button class="btn btn-danger btn-sm" onclick="deleteAdapter(\'' + escHtml(a.name) + '\')">' + __('删除','Delete') + '</button></td></tr>';
      });
      html += '</table>';
    }
    html += '</div>';
    html += '<div class="card"><h2>' + __('上传新适配器','Upload New Adapter') + '</h2>'
      + '<label>' + __('适配器名称（不带 .lua）','Adapter name (without .lua)') + '</label>'
      + '<input id="adapter-name" placeholder="' + __('如 openai','e.g. openai') + '">'
      + '<label>' + __('Lua 脚本代码','Lua Script Code') + '</label>'
      + '<textarea id="adapter-code" rows="12" placeholder="-- ' + __('返回一个适配器表','return an adapter table') + '\nreturn {\n  name = \&quot;openai\&quot;,\n  version = \&quot;1.0\&quot;,\n  transform_request = function(raw) ... end,\n  transform_response = function(raw) ... end,\n}"></textarea>'
      + '<button class="btn btn-primary" onclick="uploadAdapter()">' + __('上传','Upload') + '</button></div>';
  } catch(e) {
    html += '<div class="card"><p style="color:var(--text-muted)">' + __('加载适配器失败: ','Failed to load adapters: ') + escHtml(e.message) + '</p></div>';
  }
  document.getElementById('view-adapters').innerHTML = html;
}

async function uploadAdapter() {
  var name = document.getElementById('adapter-name')?.value;
  var code = document.getElementById('adapter-code')?.value;
  if (!name || !code) { toast(__('名称和代码不能为空','Name and code cannot be empty'), true); return }
  try {
    var r = await api('/adapters', { method: 'POST', body: JSON.stringify({ name: name, code: code }) });
    if (r.status === 'loaded') { toast(__('适配器','Adapter') + ' "' + name + '" ' + __('已加载','loaded')); renderAdapters() }
    else { toast(__('上传失败: ','Upload failed: ') + (r.error || 'unknown'), true) }
  } catch(e) { toast(__('上传失败: ','Upload failed: ') + e.message, true) }
}

async function deleteAdapter(name) {
  if (!confirm(__('确定删除适配器','Are you sure to delete adapter') + ' "' + name + '"？')) return;
  try {
    var r = await api('/adapters/' + encodeURIComponent(name), { method: 'DELETE' });
    if (r.status === 'deleted') { toast(__('适配器','Adapter') + ' "' + name + '" ' + __('已删除','deleted')); renderAdapters() }
    else { toast(__('删除失败','Delete failed'), true) }
  } catch(e) { toast(__('删除失败: ','Delete failed: ') + e.message, true) }
}

// ===== Init =====

(async function() {
  var data = await window.homeagent.connections.list();
  state.connections = data.connections || [];
  if (data.currentId) state.currentConn = state.connections.find(function(c) { return c.id === data.currentId }) || null;
  if (state.currentConn) {
    connectSSE();
    await loadChatHistory();
    doRenderAll();
    startUptimeTicker();
    setInterval(doRenderAll, 15000);
  } else {
    renderAll();
    updateConnIndicator();
  }
})();

// ===== Connection Management =====
function updateConnIndicator() {
  var el = document.getElementById('conn-name-display');
  var dot = document.getElementById('conn-dot');
  var rdot = document.getElementById('rail-conn-dot');
  if (state.currentConn) {
    el.textContent = state.currentConn.name;
    var cls = state.status.status === 'running' ? 'dot-green pulse' : 'dot-yellow';
    dot.className = 'status-dot ' + cls;
    if (rdot) rdot.className = 'conn-dot ' + (state.status.status === 'running' ? 'dot-green' : 'dot-yellow');
  } else {
    el.textContent = __('未连接','Not connected');
    dot.className = 'status-dot dot-gray';
    if (rdot) rdot.className = 'conn-dot';
  }
}

function goSettingsConn() {
  switchView('settings');
  renderConnSection();
}

function openConnManager() { renderConnSection(); switchView('settings'); }

function renderConnSection() {
  var cont = document.getElementById('conn-manager');
  if (!cont) return;
  cont.innerHTML = '';
  if (state.connections.length === 0) {
    cont.innerHTML += '<div class="card"><p style="color:var(--text-muted)">' + __('暂无后端连接，添加一个以开始使用','No backend connections yet. Add one to get started.') + '</p></div>';
  } else {
    state.connections.forEach(function(c) {
      var div = document.createElement('div');
      div.className = 'conn-item ' + (state.currentConn && state.currentConn.id === c.id ? 'active' : '');
      div.innerHTML = '<span class="status-dot ' + (state.currentConn && state.currentConn.id === c.id ? 'dot-green' : 'dot-gray') + '"></span>'
        + '<div class="conn-info"><div class="conn-name">' + escHtml(c.name) + '</div><div class="conn-url">' + escHtml(c.url) + '</div></div>'
        + '<div class="conn-actions">'
        + '<button class="btn btn-ghost btn-sm" onclick="selectConnection(\'' + c.id + '\')">' + __('连接','Connect') + '</button> '
        + '<button class="btn btn-ghost btn-sm" onclick="editConnection(\'' + c.id + '\', event)">' + __('编辑','Edit') + '</button> '
        + '<button class="btn btn-danger btn-sm" onclick="deleteConnection(\'' + c.id + '\', event)">' + __('删除','Delete') + '</button></div>';
      cont.appendChild(div);
    });
  }
  var form = document.createElement('div');
  form.className = 'conn-form';
  form.id = 'conn-form';
  form.style.display = 'none';
  form.innerHTML = '<h3 id="conn-form-title">' + __('添加连接','Add Connection') + '</h3>'
    + '<label>' + __('名称','Name') + '</label><input id="conn-name" placeholder="My HomeAgent">'
    + '<label>' + __('连接类型','Type') + '</label><select id="conn-type" onchange="toggleConnType()">'
    + '<option value="webui">WebUI (HTTP)</option>'
    + '<option value="cli">CLI (unix socket)</option></select>'
    + '<div id="conn-addr-webui"><label>' + __('地址','URL') + '</label><input id="conn-url" placeholder="http://localhost:18080"></div>'
    + '<div id="conn-addr-cli" style="display:none"><label>' + __('Socket 路径','Socket Path') + '</label><input id="conn-sock" placeholder="C:\\path\\to\\cli.sock"></div>'
    + '<label>' + __('API 密钥','API Key') + ' <span style="color:var(--text-muted);font-weight:400">(' + __('可选','optional') + ')</span></label>'
    + '<input id="conn-key" type="password" placeholder="sk-...">'
    + '<div class="conn-form-actions">'
    + '<button class="btn btn-ghost" onclick="cancelConnForm()">' + __('取消','Cancel') + '</button>'
    + '<button class="btn btn-primary" onclick="saveConnForm()" id="conn-save-btn">' + __('保存','Save') + '</button></div>';
  cont.appendChild(form);
  var addBtn = document.createElement('button');
  addBtn.className = 'btn btn-primary';
  addBtn.id = 'conn-add-btn';
  addBtn.textContent = '+ ' + __('添加连接','Add Connection');
  addBtn.style.marginTop = '8px';
  addBtn.onclick = showConnForm;
  cont.appendChild(addBtn);
}

function toggleConnType() {
  var t = document.getElementById('conn-type').value;
  document.getElementById('conn-addr-webui').style.display = t === 'cli' ? 'none' : 'block';
  document.getElementById('conn-addr-cli').style.display = t === 'cli' ? 'block' : 'none';
}

function showConnForm() {
  editingConnId = null;
  document.getElementById('conn-form-title').textContent = __('添加连接','Add Connection');
  document.getElementById('conn-name').value = '';
  document.getElementById('conn-url').value = 'http://localhost:18080';
  document.getElementById('conn-sock').value = '';
  document.getElementById('conn-key').value = '';
  document.getElementById('conn-type').value = 'webui';
  toggleConnType();
  document.getElementById('conn-form').style.display = 'block';
  document.getElementById('conn-add-btn').style.display = 'none';
}

function editConnection(id, e) {
  if (e) e.stopPropagation();
  var c = state.connections.find(function(x) { return x.id === id });
  if (!c) return;
  editingConnId = id;
  document.getElementById('conn-form-title').textContent = __('编辑连接','Edit Connection');
  document.getElementById('conn-name').value = c.name;
  document.getElementById('conn-url').value = c.url || 'http://localhost:18080';
  document.getElementById('conn-sock').value = c.socketPath || '';
  document.getElementById('conn-key').value = c.apiKey;
  document.getElementById('conn-type').value = c.type === 'cli' ? 'cli' : 'webui';
  toggleConnType();
  document.getElementById('conn-form').style.display = 'block';
  document.getElementById('conn-add-btn').style.display = 'none';
}

function cancelConnForm() {
  document.getElementById('conn-form').style.display = 'none';
  document.getElementById('conn-add-btn').style.display = 'block';
}

async function selectConnection(id) {
  if (state.eventSource) { state.eventSource.close(); state.eventSource = null; }
  var data = await window.homeagent.connections.setCurrent(id);
  state.currentConn = data.connections.find(function(c) { return c.id === id }) || null;
  state.connections = data.connections;
  state.messages = [];
  updateConnIndicator();
  connectSSE();
  await loadChatHistory();
  doRenderAll();
  startUptimeTicker();
  switchView('chat');
  renderConnSection();
}

async function deleteConnection(id, e) {
  if (e) e.stopPropagation();
  if (!confirm(__('确定删除此连接？','Delete this connection?'))) return;
  var wasCurrent = state.currentConn && state.currentConn.id === id;
  var data = await window.homeagent.connections.delete(id);
  state.connections = data.connections;
  state.currentConn = data.currentId ? state.connections.find(function(c) { return c.id === data.currentId }) : null;
  if (wasCurrent && state.eventSource) { state.eventSource.close(); state.eventSource = null; }
  if (state.currentConn) {
    updateConnIndicator(); doRenderAll(); connectSSE();
  } else {
    updateConnIndicator();
  }
  renderConnSection();
}

var editingConnId = null;

async function saveConnForm() {
  var name = document.getElementById('conn-name').value.trim();
  var ctype = document.getElementById('conn-type').value;
  var url = document.getElementById('conn-url').value.trim().replace(/\/+$/, '');
  var sock = document.getElementById('conn-sock').value.trim();
  var apiKey = document.getElementById('conn-key').value.trim();
  if (ctype === 'cli') {
    if (!name || !sock) { toast(__('名称和 Socket 路径不能为空','Name and Socket Path required'), true); return; }
  } else {
    if (!name || !url) { toast(__('名称和地址不能为空','Name and URL required'), true); return; }
  }
  var testBtn = document.querySelector('#conn-form .btn-primary');
  testBtn.textContent = __('测试中...','Testing...'); testBtn.disabled = true;
  try {
    if (ctype === 'cli') {
      if (!window.homeagent.cli) throw new Error('cli bridge unavailable');
      var testR = await window.homeagent.cli.request(sock, apiKey, '/status');
      if (testR.error || testR.type === 'error') {
        toast(__('CLI 连接测试失败: ','CLI test failed: ') + (testR.error || testR.type), true);
        testBtn.textContent = __('保存','Save'); testBtn.disabled = false; return;
      }
    } else {
      var testR = await fetch(url + '/api/v1/status', { headers: apiKey ? { 'X-API-Key': apiKey } : {} });
      if (!testR.ok) { toast(__('连接测试失败: HTTP ','Connection test failed: HTTP ') + testR.status, true); testBtn.textContent = __('保存','Save'); testBtn.disabled = false; return; }
    }
  } catch(e) {
    toast(__('无法连接到 ','Cannot connect to ') + (ctype === 'cli' ? sock : url) + ': ' + e.message, true);
    testBtn.textContent = __('保存','Save'); testBtn.disabled = false; return;
  }
  testBtn.textContent = __('保存','Save'); testBtn.disabled = false;
  var connData = ctype === 'cli'
    ? { name: name, type: 'cli', socketPath: sock, url: '', apiKey: apiKey }
    : { name: name, type: 'webui', url: url, apiKey: apiKey };
  var data;
  if (editingConnId) {
    data = await window.homeagent.connections.update(editingConnId, connData);
  } else {
    data = await window.homeagent.connections.add(connData);
  }
  state.connections = data.connections;
  var cur = data.connections.find(function(c) { return c.id === data.currentId });
  var switched = !!cur && (!state.currentConn || state.currentConn.id !== cur.id);
  if (cur) {
    state.currentConn = cur;
    if (switched) {
      if (state.eventSource) { state.eventSource.close(); state.eventSource = null; }
      state.messages = [];
      updateConnIndicator(); connectSSE(); await loadChatHistory(); doRenderAll(); startUptimeTicker();
      switchView('chat');
    } else {
      updateConnIndicator(); doRenderAll();
    }
  }
  cancelConnForm(); renderConnSection();
}

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape' && document.getElementById('conn-form').style.display === 'block') cancelConnForm();
});

// ===== SSE (override for fetch-based) =====
connectSSE = function() {
  if (state.eventSource) { state.eventSource.close(); state.eventSource = null; }
  if (!state.currentConn) return;
  // CLI 连接无 SSE 通道，聊天走同步 cli:request
  if (state.currentConn.type === 'cli') return;
  connectFetchSSE(state.currentConn.url + '/api/v1/chat/events');
};

async function connectFetchSSE(url) {
  try {
    var headers = {};
    if (state.currentConn && state.currentConn.apiKey) headers['X-API-Key'] = state.currentConn.apiKey;
    var resp = await fetch(url, { headers: headers, cache: 'no-store' });
    if (!resp.ok || !resp.body) { setTimeout(function() { connectSSE() }, 5000); return; }
    var reader = resp.body.getReader(); var decoder = new TextDecoder();
    var buffer = ''; var reconnectTimer = null;
    state.eventSource = { close: function() { reader.cancel(); if (reconnectTimer) clearTimeout(reconnectTimer) } };
    function processLines() {
      var lines = buffer.split('\n'); buffer = lines.pop() || '';
      var eventType = '', data = '';
      for (var i = 0; i < lines.length; i++) {
        var line = lines[i];
        if (line.startsWith('event: ')) eventType = line.slice(7).trim();
        else if (line.startsWith('data: ')) data = line.slice(6).trim();
        else if (line === '' && eventType && data) { handleSSEEvent(eventType, data); eventType = ''; data = ''; }
      }
    }
    function handleSSEEvent(type, raw) {
      try {
        var ev = JSON.parse(raw); var p = ev.payload || {};
        if (type === 'agent_output') {
          state.chatStage = __('AI 回复中...','AI replying...');
          if (p.kind === 'channel_output') {
            var cm = { role: 'assistant', content: p.content || '', source: p.channel || '', _final: true, _grow: true };
            if (state.chatFinalIdx >= 0 && state.chatFinalIdx < state.messages.length) {
              state.messages.splice(state.chatFinalIdx, 0, cm);
              state.chatFinalIdx++;
            } else {
              state.messages.push(cm);
            }
            rerenderChatIfActive(); return;
          }
          var last = state.messages.length > 0 ? state.messages[state.messages.length - 1] : null;
          if (last && last.role === 'assistant' && !last._final) {
            last._grow = true;
            last.content += (p.content || '');
            rerenderChatIfActive(); return;
          }
          if (last && last.role === 'assistant' && last._final && !last.source) { return; }
          state.messages.push({ role: 'assistant', content: p.content || '', _streaming: true, _grow: true });
          rerenderChatIfActive();
        } else if (type === 'reasoning') {
          if (p.content) {
            state.chatStage = __('AI 思考中...','AI thinking...');
            var last = state.messages.length > 0 ? state.messages[state.messages.length - 1] : null;
            if (!last || last.role !== 'assistant' || last._final) {
              state.messages.push({ role: 'assistant', content: '', reasoning_content: '', tool_calls: [], _streaming: true });
              last = state.messages[state.messages.length - 1];
            }
            last.reasoning_content = (last.reasoning_content || '') + (p.content || '');
            rerenderChatIfActive();
          }
        } else if (type === 'tool_call') {
          if (!p.tool) return;
          var last = state.messages.length > 0 ? state.messages[state.messages.length - 1] : null;
          if (!last || last.role !== 'assistant' || last._final) {
            state.messages.push({ role: 'assistant', content: '', tool_calls: [], _streaming: true });
            last = state.messages[state.messages.length - 1];
          }
          if (!last.tool_calls) last.tool_calls = [];
          last.tool_calls.push({ tool: p.tool, name: p.tool, args: p.args || {}, result: p.result || '', status: p.status || 'ok', plugin: p.plugin || '' });
          var pidx = (state.pendingTools || []).indexOf(p.tool);
          if (pidx !== -1) state.pendingTools.splice(pidx, 1);
          state.chatStage = __('工具调用: ','Tool: ') + (p.tool || '');
          rerenderChatIfActive();
        } else if (type === 'terminal_output') {
          if (!p.terminal_id) return;
          var tid = p.terminal_id;
          if (!state.termScreens) state.termScreens = {};
          var scr = state.termScreens[tid] || (state.termScreens[tid] = { output: '', running: true });
          if (p.output) scr.output += p.output;
          if (typeof p.running === 'boolean') scr.running = p.running;
          var bufel = document.getElementById('term-buf-' + tid);
          if (bufel) {
            appendTermBuf(bufel, p.output || '');
            var dot = document.getElementById('term-dot-' + tid);
            if (dot) dot.className = 'term-dot' + (scr.running ? '' : ' stopped');
          }
        } else if (type === 'stage') {
          var phase = p.phase || ''; var tool = p.tool || '';
          if (p.channel !== '_consolidation_') {
            if (phase === 'pre_action') state.chatStage = __('AI 思考中...','AI thinking...');
            else if (phase === 'before_toolcall') {
              state.chatStage = __('工具调用: ','Tool: ') + (tool || '');
              if (tool && (state.pendingTools || []).indexOf(tool) === -1) {
                if (!state.pendingTools) state.pendingTools = [];
                state.pendingTools.push(tool);
                rerenderChatIfActive();
              }
            }
            else if (phase === 'before_output') state.chatStage = __('生成回复中...','Generating response...');
          }
          var badge = document.getElementById('chat-stage');
          if (badge) { badge.textContent = state.chatStage || ''; badge.style.display = 'none'; }
        }
      } catch(err) {}
    }
    async function pump() {
      while (true) {
        try { var result = await reader.read(); if (result.done) break; buffer += decoder.decode(result.value, { stream: true }); processLines(); } catch(e) { break; }
      }
      reconnectTimer = setTimeout(function() { connectSSE() }, 3000);
    }
    pump();
  } catch(e) { setTimeout(function() { connectSSE() }, 5000); }
}

function rerenderChatIfActive() {
  var tab = document.getElementById('view-chat');
  if (tab && tab.classList.contains('active')) { renderChat(); renderChatStarmap(); renderTerminals(); renderCmdHistory(); }
}

