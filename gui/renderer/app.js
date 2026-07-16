// ===== State =====
let state = {
  connections: [], currentConn: null,
  status: {}, kernel: null,
  settings: {}, meta: {}, pluginMeta: {}, settingsPlugins: ['core'],
  selectedSection: 'core',
  messages: [], chatLoading: false, chatStage: '',
  installedPlugins: [], pluginInfo: null,
  startedAt: null, uptimeTick: null, sidebarRefreshTick: null, eventSource: null,
  _chatHistoryLoaded: false, _starmapData: null,
};

// ===== Markdown Renderer (lightweight, no dependencies) =====
function renderMarkdown(t) {
  if (!t) return '';
  let s = String(t)
    .replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
  // code blocks (fenced)
  s = s.replace(/```(\w*)\n([\s\S]*?)```/g, '<pre><code>$2</code></pre>');
  // inline code
  s = s.replace(/`([^`]+)`/g, '<code>$1</code>');
  // headers
  s = s.replace(/^### (.+)$/gm, '<h3>$1</h3>');
  s = s.replace(/^## (.+)$/gm, '<h2>$1</h2>');
  s = s.replace(/^# (.+)$/gm, '<h1>$1</h1>');
  // bold & italic
  s = s.replace(/\*\*\*(.+?)\*\*\*/g, '<strong><em>$1</em></strong>');
  s = s.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/\*(.+?)\*/g, '<em>$1</em>');
  // links
  s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" target="_blank">$1</a>');
  // images
  s = s.replace(/!\[([^\]]*)\]\(([^)]+)\)/g, '<img src="$2" alt="$1" style="max-width:100%">');
  // blockquote
  s = s.replace(/^> (.+)$/gm, '<blockquote>$1</blockquote>');
  // horizontal rule
  s = s.replace(/^---$/gm, '<hr>');
  // unordered list
  s = s.replace(/^[\s]*[-*] (.+)$/gm, '<li>$1</li>');
  s = s.replace(/(<li>.*<\/li>\n?)+/g, '<ul>$&</ul>');
  // ordered list
  s = s.replace(/^[\s]*\d+\. (.+)$/gm, '<li>$1</li>');
  // paragraphs: double newlines
  s = s.replace(/\n\n/g, '</p><p>');
  s = '<p>' + s + '</p>';
  // clean nested ps from lists
  s = s.replace(/<\/p>\n?<ul>/g, '<ul>').replace(/<\/ul>\n?<p>/g, '</ul>');
  s = s.replace(/<\/p>\n?<li>/g, '<li>').replace(/<\/li>\n?<p>/g, '</li>');
  s = s.replace(/<p><\/p>/g, '');
  return s;
}

// ===== Connection Management =====
async function initApp() {
  const data = await window.homeagent.connections.list();
  state.connections = data.connections || [];
  if (data.currentId) state.currentConn = state.connections.find(c => c.id === data.currentId) || null;
  if (state.currentConn) {
    document.getElementById('app').style.display = 'block';
    document.getElementById('conn-overlay').style.display = 'none';
    updateConnIndicator();
    await renderAll();
    startUptimeTicker(); startSidebarRefresh(); connectSSE();
  } else {
    document.getElementById('conn-overlay').style.display = 'flex';
  }
  renderConnList();
}

function updateConnIndicator() {
  const el = document.getElementById('conn-name-display');
  const dot = document.getElementById('conn-dot');
  if (state.currentConn) {
    el.textContent = state.currentConn.name;
    dot.className = 'status-dot ' + (state.status.status === 'running' ? 'dot-green' : 'dot-yellow');
  } else { el.textContent = '未连接'; dot.className = 'status-dot dot-gray'; }
}

function openConnManager() { renderConnList(); document.getElementById('conn-overlay').style.display = 'flex'; }

async function selectConnection(id) {
  disconnectSSE();
  const data = await window.homeagent.connections.setCurrent(id);
  state.currentConn = data.connections.find(c => c.id === id) || null;
  state.connections = data.connections;
  state.messages = []; state._chatHistoryLoaded = false;
  document.getElementById('app').style.display = 'block';
  document.getElementById('conn-overlay').style.display = 'none';
  updateConnIndicator();
  await renderAll();
  startUptimeTicker(); startSidebarRefresh(); connectSSE();
}

async function deleteConnection(id, e) {
  e.stopPropagation();
  if (!confirm('确定删除此连接？')) return;
  const wasCurrent = state.currentConn && state.currentConn.id === id;
  const data = await window.homeagent.connections.delete(id);
  state.connections = data.connections;
  state.currentConn = data.currentId ? state.connections.find(c => c.id === data.currentId) : null;
  if (wasCurrent) { disconnectSSE(); if (state.sidebarRefreshTick) { clearInterval(state.sidebarRefreshTick); state.sidebarRefreshTick = null; } }
  if (state.currentConn) {
    updateConnIndicator(); await renderAll(); startSidebarRefresh(); connectSSE();
  } else {
    document.getElementById('app').style.display = 'none'; document.getElementById('conn-overlay').style.display = 'flex';
  }
  renderConnList();
}

function renderConnList() {
  document.getElementById('conn-list').innerHTML = state.connections.map(c =>
    '<div class="conn-item ' + (state.currentConn && state.currentConn.id === c.id ? 'active' : '') + '" onclick="selectConnection(\'' + c.id + '\')">'
    + '<span class="status-dot ' + (state.currentConn && state.currentConn.id === c.id ? 'dot-green' : 'dot-gray') + '"></span>'
    + '<div class="conn-info"><div class="conn-name">' + escHtml(c.name) + '</div><div class="conn-url">' + escHtml(c.url) + '</div></div>'
    + '<div class="conn-actions">'
    + '<button class="btn btn-ghost btn-sm" onclick="editConnection(\'' + c.id + '\', event)">编辑</button>'
    + '<button class="btn btn-danger btn-sm" onclick="deleteConnection(\'' + c.id + '\', event)">删除</button></div></div>'
  ).join('');
}

let editingConnId = null;

function showConnForm() {
  editingConnId = null;
  document.getElementById('conn-form-title').textContent = '添加连接 / Add Connection';
  document.getElementById('conn-name').value = 'My HomeAgent';
  document.getElementById('conn-url').value = 'http://localhost:8080';
  document.getElementById('conn-key').value = '';
  document.getElementById('conn-form').style.display = 'block';
  document.getElementById('conn-add-btn').style.display = 'none';
}

function editConnection(id, e) {
  e.stopPropagation();
  const c = state.connections.find(x => x.id === id);
  if (!c) return;
  editingConnId = id;
  document.getElementById('conn-form-title').textContent = '编辑连接 / Edit Connection';
  document.getElementById('conn-name').value = c.name;
  document.getElementById('conn-url').value = c.url;
  document.getElementById('conn-key').value = c.apiKey;
  document.getElementById('conn-form').style.display = 'block';
  document.getElementById('conn-add-btn').style.display = 'none';
  document.querySelectorAll('.conn-item').forEach(el => el.style.opacity = '0.4');
}

function cancelConnForm() {
  document.getElementById('conn-form').style.display = 'none';
  document.getElementById('conn-add-btn').style.display = 'block';
  document.querySelectorAll('.conn-item').forEach(el => el.style.opacity = '1');
}

async function saveConnForm() {
  const name = document.getElementById('conn-name').value.trim();
  const url = document.getElementById('conn-url').value.trim().replace(/\/+$/, '');
  const apiKey = document.getElementById('conn-key').value.trim();
  if (!name || !url) { toast('名称和地址不能为空', true); return; }
  // test connection before saving
  const testBtn = document.querySelector('#conn-form .btn-primary');
  testBtn.textContent = '测试中...'; testBtn.disabled = true;
  try {
    const testR = await fetch(url + '/api/v1/status', {
      headers: apiKey ? { 'X-API-Key': apiKey } : {}
    });
    if (!testR.ok) { toast('连接测试失败: HTTP ' + testR.status, true); testBtn.textContent = '保存 / Save'; testBtn.disabled = false; return; }
  } catch(e) {
    toast('无法连接到 ' + url + ': ' + e.message, true);
    testBtn.textContent = '保存 / Save'; testBtn.disabled = false; return;
  }
  testBtn.textContent = '保存 / Save'; testBtn.disabled = false;
  let data;
  if (editingConnId) {
    data = await window.homeagent.connections.update(editingConnId, { name, url, apiKey });
  } else {
    data = await window.homeagent.connections.add({ name, url, apiKey });
  }
  state.connections = data.connections;
  const curId = data.currentId;
  const cur = data.connections.find(c => c.id === curId);
  if (cur) {
    state.currentConn = cur;
    if (!document.getElementById('app').style.display || document.getElementById('app').style.display === 'none') {
      document.getElementById('app').style.display = 'block';
      document.getElementById('conn-overlay').style.display = 'none';
      updateConnIndicator(); await renderAll(); startUptimeTicker(); startSidebarRefresh(); connectSSE();
    } else { updateConnIndicator(); if (editingConnId) await renderAll(); }
  }
  cancelConnForm(); renderConnList();
}

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape' && document.getElementById('conn-form').style.display === 'block') cancelConnForm();
});

// ===== API Client =====
async function api(path, opts = {}) {
  if (!state.currentConn) throw new Error('No connection selected');
  const headers = { 'Content-Type': 'application/json', ...opts.headers };
  if (state.currentConn.apiKey) headers['X-API-Key'] = state.currentConn.apiKey;
  const url = state.currentConn.url + '/api/v1' + path;
  const res = await fetch(url, { ...opts, headers });
  if (res.status === 401) throw new Error('unauthorized');
  if (opts.raw) return res;
  const ct = res.headers.get('content-type') || '';
  if (ct.includes('json')) return res.json();
  return res.text();
}

// ===== Toast =====
function toast(msg, isError) {
  const t = document.getElementById('toast');
  t.textContent = msg; t.className = 'toast' + (isError ? ' error' : ''); t.style.display = 'block';
  setTimeout(function() { t.style.display = 'none' }, 3000);
}

// ===== Utility =====
function escHtml(s) { return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }
function fmtUptime(ms) {
  const s = Math.floor(ms / 1000);
  if (s < 60) return s + 's'; const m = Math.floor(s / 60); s = s % 60;
  if (m < 60) return m + 'm ' + s + 's'; const h = Math.floor(m / 60); m = m % 60;
  return h + 'h ' + m + 'm ' + s + 's';
}

// ===== Theme =====
function setTheme(name) {
  document.documentElement.setAttribute('data-theme', name);
  localStorage.setItem('ha-theme', name);
  document.getElementById('theme-btn').textContent = name === 'light' ? '☀️' : '🌙';
}
function toggleTheme() { setTheme(document.documentElement.getAttribute('data-theme') === 'light' ? 'dark' : 'light'); }
(function() { setTheme(localStorage.getItem('ha-theme') || 'dark') })();

// ===== Navigation =====
function switchTab(n) {
  document.querySelectorAll('.tab-content').forEach(function(e) { e.classList.remove('active') });
  const el = document.getElementById('tab-' + n); if (el) el.classList.add('active');
  document.querySelectorAll('nav a').forEach(function(e) { e.classList.remove('active') });
  const m = document.querySelector('nav a[onclick*="' + n + '"]'); if (m) m.classList.add('active');
  renderAll();
}

// ===== Tab Render Dispatch =====
async function renderAll() {
  if (!state.currentConn) return;
  try { const s = await api('/status'); state.status = s; state.startedAt = s.startedAt ? new Date(s.startedAt).getTime() : null; updateConnIndicator() } catch(e) {}
  try { state.kernel = await api('/kernel') } catch(e) {}
  try {
    const s = await api('/settings'); state.settings = s.settings || {}; state.meta = s.meta || {};
    state.settingsPlugins = s.plugins || ['core']; state.pluginMeta = s.plugin_meta || {};
  } catch(e) {}
  try { state.installedPlugins = await api('/plugins') } catch(e) {}
  try { renderOverview() } catch(e) {} try { renderChat() } catch(e) {} try { renderPlugins() } catch(e) {}
  try { renderKernel() } catch(e) {} try { renderOneSettings() } catch(e) {} try { renderAdapters() } catch(e) {}
}

function startUptimeTicker() {
  if (state.uptimeTick) clearInterval(state.uptimeTick);
  state.uptimeTick = setInterval(function() {
    const el = document.querySelector('#uptime-val');
    if (el && state.startedAt) el.textContent = fmtUptime(Date.now() - state.startedAt);
  }, 1000);
}

// ===== SSE =====
function disconnectSSE() { if (state.eventSource) { state.eventSource.close(); state.eventSource = null; } }

function connectSSE() {
  disconnectSSE(); if (!state.currentConn) return;
  connectFetchSSE(state.currentConn.url + '/api/v1/chat/events');
}

async function connectFetchSSE(url) {
  try {
    const headers = {};
    if (state.currentConn && state.currentConn.apiKey) headers['X-API-Key'] = state.currentConn.apiKey;
    const resp = await fetch(url, { headers, cache: 'no-store' });
    if (!resp.ok || !resp.body) { setTimeout(function() { connectSSE() }, 5000); return; }
    const reader = resp.body.getReader(); const decoder = new TextDecoder();
    let buffer = ''; let reconnectTimer = null;
    state.eventSource = { close: function() { reader.cancel(); if (reconnectTimer) clearTimeout(reconnectTimer) } };
    function processLines() {
      const lines = buffer.split('\n'); buffer = lines.pop() || '';
      let eventType = '', data = '';
      for (const line of lines) {
        if (line.startsWith('event: ')) eventType = line.slice(7).trim();
        else if (line.startsWith('data: ')) data = line.slice(6).trim();
        else if (line === '' && eventType && data) { handleSSEEvent(eventType, data); eventType = ''; data = ''; }
      }
    }
    function handleSSEEvent(type, raw) {
      try {
        const p = JSON.parse(raw);
        if (type === 'agent_output') {
          const last = state.messages[state.messages.length - 1];
          if (last && last.role === 'assistant' && last._streaming) {
            last.content = (last.content || '') + (p.content || '');
            rerenderChatIfActive();
          }
        } else if (type === 'stage') {
          const phase = p.payload?.phase;
          if (phase === 'thinking') state.chatStage = 'Thinking...';
          else if (phase === 'before_toolcall') state.chatStage = 'Tool: ' + (p.payload?.tool || '');
          else if (phase === 'before_output') state.chatStage = 'Output...';
          updateChatStageBadge();
        } else if (type === 'reasoning') {
          const last = state.messages[state.messages.length - 1];
          if (last && last.role === 'assistant' && last._streaming) {
            last.reasoning_content = (last.reasoning_content || '') + (p.payload?.content || '');
          }
        }
      } catch(err) {}
    }
    async function pump() {
      while (true) {
        try { const { done, value } = await reader.read(); if (done) break; buffer += decoder.decode(value, { stream: true }); processLines(); } catch(e) { break; }
      }
      reconnectTimer = setTimeout(function() { connectSSE() }, 3000);
    }
    pump();
  } catch(e) { setTimeout(function() { connectSSE() }, 5000); }
}

function startSidebarRefresh() {
  if (state.sidebarRefreshTick) clearInterval(state.sidebarRefreshTick);
  state.sidebarRefreshTick = setInterval(async function() {
    try { await loadSidebarData() } catch(e) {}
  }, 5000);
}

// ===== Overview =====
function statCard(l, v) { return '<div class="card stat-card"><div class="stat-value">' + v + '</div><div class="stat-label">' + l + '</div></div>'; }

function renderOverview() {
  const s = state.status || {}; const k = state.kernel;
  let html = '<div class="grid-4">' + statCard('Status', s.status || 'unknown')
    + statCard('Uptime', '<span id="uptime-val">' + (state.startedAt ? fmtUptime(Date.now() - state.startedAt) : '-') + '</span>')
    + statCard('Plugins', (k?.plugins || []).length || 0)
    + statCard('Version', s.version || '-') + '</div>';
  if (k) {
    html += '<div class="grid-2">'
      + '<div class="card"><h2>LLM Status</h2>'
      + '<div class="kv-row"><span class="key">Provider</span><span class="val">' + (k.llm?.provider || 'Not configured') + '</span></div>'
      + '<div class="kv-row"><span class="key">Sources</span><span class="val">' + (k.llm?.sources || 0) + '</span></div>'
      + '<div class="kv-row"><span class="key">Status</span><span class="val"><span class="status-dot ' + (k.llm?.available ? 'dot-green' : 'dot-red') + '"></span>' + (k.llm?.available ? 'Running' : 'Unavailable') + '</span></div></div>'
      + '<div class="card"><h2>Memory Status</h2>'
      + '<div class="kv-row"><span class="key">Graph Memory</span><span class="val"><span class="status-dot ' + (k.memory?.available ? 'dot-green' : 'dot-gray') + '"></span>' + (k.memory?.available ? k.memory.entity_count + ' entities, ' + k.memory.relation_count + ' relations' : 'Uninitialized') + '</span></div>'
      + '<div class="kv-row"><span class="key">Document Memory</span><span class="val">' + (k.documents?.available ? k.documents.doc_count + ' docs' : 'Uninitialized') + '</span></div>'
      + '<div class="kv-row"><span class="key">Text Memory</span><span class="val">' + (k.text_memory?.available ? k.text_memory.file_count + ' files' : 'Uninitialized') + '</span></div>'
      + '<div class="kv-row"><span class="key">Knowledge</span><span class="val">' + (k.knowledge?.available ? k.knowledge.item_count + ' items' : 'Uninitialized') + '</span></div></div></div>';
  }
  html += '<div class="card"><h2>Runtime</h2><div class="grid-3">' + statCard('Goroutines', k?.runtime?.goroutines || '-') + statCard('Memory', k?.runtime?.memory_mb ? k.runtime.memory_mb + ' MB' : '-') + statCard('Go Version', k?.runtime?.go_version || '-') + '</div></div>'
    + '<div class="card"><h2>Memory Graph</h2><div id="starmap-container" style="height:280px;background:var(--bg-input);border-radius:8px;display:flex;align-items:center;justify-content:center;color:var(--text-muted);font-size:13px">'
    + '<span id="starmap-placeholder">Loading memory graph...</span></div></div>';
  document.getElementById('tab-overview').innerHTML = html;
  loadStarmapData();
}

async function loadStarmapData() {
  try {
    const resp = await api('/memory/graph');
    if (resp && resp.success && resp.data && resp.data.nodes && resp.data.nodes.length > 0) {
      state._starmapData = resp.data;
      document.getElementById('starmap-placeholder').textContent = resp.data.nodes.length + ' nodes, ' + (resp.data.edges?.length || 0) + ' edges';
    } else {
      document.getElementById('starmap-placeholder').textContent = 'No memory data yet';
    }
  } catch(e) {
    document.getElementById('starmap-placeholder').textContent = 'Failed to load: ' + e.message;
  }
}

// ===== Chat =====
let _chatLayoutBuilt = false;
let _terminals = [], _cmdHistory = [];

function buildChatLayout() {
  const k = state.kernel || {};
  document.getElementById('tab-chat').innerHTML =
    '<div class="chat-layout"><div class="chat-main">'
    + '<div class="card"><h2>Chat <span id="chat-stage" class="badge" style="font-size:10px;font-weight:400;display:none"></span></h2>'
    + '<div class="chat-messages" id="chat-msgs"><div class="empty-state" style="flex:1;display:flex;align-items:center;justify-content:center"><p>Start a conversation</p></div></div>'
    + '<div class="chat-input-row"><input id="chat-input" placeholder="Type a message..." onkeydown="if(event.key==\'Enter\')sendChat()">'
    + '<button class="btn btn-primary" onclick="sendChat()" id="chat-send-btn">Send</button></div></div></div>'
    + '<div class="chat-sidebar">'
    + '<div class="card" style="padding:12px"><h2 style="font-size:13px;margin-bottom:8px">Terminals <span id="term-count-badge" class="badge badge-blue">0</span></h2><div id="term-list" style="max-height:140px;overflow-y:auto;font-size:11px"></div></div>'
    + '<div class="card" style="padding:12px"><h2 style="font-size:13px;margin-bottom:8px">Command History <span id="cmd-count-badge" class="badge badge-blue">0</span></h2><div id="cmd-list" style="max-height:100px;overflow-y:auto;font-size:11px"></div></div>'
    + '<div class="card" style="padding:12px">'
    + '<div class="sidebar-subnav"><span class="active" onclick="switchChatSub(\'memory\',this)">Memory</span><span onclick="switchChatSub(\'context\',this)">Context</span><span onclick="switchChatSub(\'knowledge\',this)">Knowledge</span></div>'
    + '<div id="chat-sub-memory">'
    + '<div class="kv-row"><span class="key">Entities</span><span class="val">' + (k?.memory?.entity_count || '-') + '</span></div>'
    + '<div class="kv-row"><span class="key">Relations</span><span class="val">' + (k?.memory?.relation_count || '-') + '</span></div>'
    + '<div style="margin-top:8px"><input id="mem-query" placeholder="Keyword query"><button class="btn btn-primary btn-sm" onclick="queryMemoryChat()">Query</button></div>'
    + '<div id="mem-result-chat" style="margin-top:8px;max-height:160px;overflow:auto"></div></div>'
    + '<div id="chat-sub-context" style="display:none"><div style="margin-top:8px"><input id="ctx-query" placeholder="Enter current topic">'
    + '<button class="btn btn-primary btn-sm" onclick="queryMemoryContext()">Get Context</button></div><div id="ctx-result" style="margin-top:8px;max-height:180px;overflow:auto"></div></div>'
    + '<div id="chat-sub-knowledge" style="display:none">'
    + '<div class="kv-row"><span class="key">Items</span><span class="val">' + (k?.knowledge?.item_count || '-') + '</span></div>'
    + '<div style="margin-top:8px"><input id="know-query" placeholder="Search knowledge"><button class="btn btn-primary btn-sm" onclick="searchKnowledgeChat()">Search</button></div>'
    + '<div id="know-result-chat" style="margin-top:8px;max-height:140px;overflow:auto"></div>'
    + '<div style="margin-top:12px;border-top:1px solid var(--border-color);padding-top:8px">'
    + '<input id="know-name" placeholder="Knowledge name" style="margin-bottom:4px">'
    + '<textarea id="know-content" placeholder="Content" style="min-height:50px;margin-bottom:4px"></textarea>'
    + '<button class="btn btn-primary btn-sm" onclick="createKnowledgeChat()">Create</button></div></div></div></div></div>';
  _chatLayoutBuilt = true;
}

function renderChat() {
  if (!_chatLayoutBuilt) { buildChatLayout(); renderTerminalsList(); renderCmdHistoryList(); }
  const msgsEl = document.getElementById('chat-msgs');
  if (!msgsEl) return;
  if (state.messages.length === 0) {
    msgsEl.innerHTML = '<div class="empty-state" style="flex:1;display:flex;align-items:center;justify-content:center"><p>Start a conversation</p></div>'; return;
  }
  let html = '';
  state.messages.forEach(function(m) {
    const role = m.role || 'user'; let c = m.content || '';
    if (role === 'assistant') { c = renderMarkdown(c) } else { c = '<pre>' + escHtml(c) + '</pre>' }
    const rc = m.reasoning_content ? '<div class="reasoning"><div class="reasoning-title" onclick="var n=this.nextElementSibling;n.style.display=n.style.display===\'none\'?\'block\':\'none\';this.textContent=this.textContent===\'Collapse\'?\'Expand\':\'Collapse\'">Collapse</div><div class="reasoning-body" style="display:none">' + renderMarkdown(m.reasoning_content) + '</div></div>' : '';
    html += '<div class="msg msg-' + role + '"><div class="msg-avatar">' + (role === 'user' ? 'U' : 'A') + '</div>'
      + '<div class="msg-content"><div class="msg-bubble">' + rc + '<div class="text">' + c + '</div></div></div></div>';
  });
  msgsEl.innerHTML = html; msgsEl.scrollTop = msgsEl.scrollHeight;
  updateChatStageBadge();
}

function switchChatSub(name, el) {
  document.querySelectorAll('.sidebar-subnav span').forEach(function(e) { e.classList.remove('active') });
  if (el) el.classList.add('active');
  ['memory','context','knowledge'].forEach(function(s) { document.getElementById('chat-sub-' + s).style.display = s === name ? 'block' : 'none' });
}

async function sendChat() {
  const inp = document.getElementById('chat-input'); const btn = document.getElementById('chat-send-btn');
  const text = inp.value.trim(); if (!text || state.chatLoading) return;
  state.messages.push({ role: 'user', content: text }); inp.value = '';
  const streamingMsg = { role: 'assistant', content: '', reasoning_content: '', _streaming: true };
  state.messages.push(streamingMsg); renderChat();
  state.chatLoading = true; btn.disabled = true; btn.textContent = '...';
  try {
    const r = await api('/chat', { method: 'POST', body: JSON.stringify({ message: text }) });
    if (streamingMsg._streaming) {
      streamingMsg.content = r.response || '(no response)'; streamingMsg.reasoning_content = r.reasoning_content || '';
    } else {
      streamingMsg.content = (streamingMsg.content || '') + (r.response || ''); streamingMsg.reasoning_content = (streamingMsg.reasoning_content || '') + (r.reasoning_content || '');
    }
    delete streamingMsg._streaming; saveChatHistory(); renderChat();
  } catch(e) {
    if (streamingMsg._streaming) { streamingMsg.content = 'Error: ' + e.message; delete streamingMsg._streaming; }
    renderChat(); toast('Request failed: ' + e.message, true);
  } finally {
    state.chatLoading = false; btn.disabled = false; btn.textContent = 'Send'; renderChat();
  }
}

function updateChatStageBadge() {
  const badge = document.getElementById('chat-stage');
  if (!badge) return; badge.textContent = state.chatStage || ''; badge.style.display = state.chatStage ? 'inline' : 'none';
}

function rerenderChatIfActive() {
  const tab = document.getElementById('tab-chat');
  if (tab && tab.classList.contains('active')) renderChat();
}

async function loadSidebarData() {
  try { const d = await api('/terminals'); _terminals = d?.terminals || []; renderTerminalsList() } catch(e) {}
  try { const d = await api('/cmd/history'); _cmdHistory = d?.history || []; renderCmdHistoryList() } catch(e) {}
}

function renderTerminalsList() {
  const el = document.getElementById('term-list'); const badge = document.getElementById('term-count-badge');
  if (!el) return; if (badge) badge.textContent = _terminals.length;
  if (_terminals.length === 0) { el.innerHTML = '<p style="color:var(--text-muted);font-size:11px">No active terminals</p>'; return; }
  el.innerHTML = _terminals.map(function(t) {
    const status = t.running ? '<span class="status-dot dot-green"></span>' : '<span class="status-dot dot-gray"></span>';
    return '<div style="padding:3px 0;border-bottom:1px solid var(--border-color);font-size:11px">' + status + ' ' + escHtml((t.command || t.id || '').substring(0, 40)) + ' <span style="color:var(--text-muted)">' + (t.uptime || '') + '</span></div>';
  }).join('');
}

function renderCmdHistoryList() {
  const el = document.getElementById('cmd-list'); const badge = document.getElementById('cmd-count-badge');
  if (!el) return; if (badge) badge.textContent = _cmdHistory.length;
  if (_cmdHistory.length === 0) { el.innerHTML = '<p style="color:var(--text-muted);font-size:11px">No command history</p>'; return; }
  el.innerHTML = _cmdHistory.slice(-10).reverse().map(function(c) {
    const status = c.status === 'completed' ? '<span class="badge badge-green">OK</span>' : '<span class="badge badge-red">' + escHtml(c.status || 'FAIL') + '</span>';
    return '<div style="padding:3px 0;border-bottom:1px solid var(--kv-border);font-size:11px;display:flex;justify-content:space-between"><span>' + escHtml((c.command || '').substring(0, 40)) + '</span><span>' + status + '</span></div>';
  }).join('');
}

// ===== Chat History Persistence =====
async function saveChatHistory() {
  try {
    const msgs = state.messages.filter(function(m) { return !m._streaming }).map(function(m) {
      return { role: m.role, content: m.content, reasoning_content: m.reasoning_content, time: new Date().toISOString() };
    }).slice(-100);
    await api('/settings', { method: 'PUT', body: JSON.stringify({ key: 'plugin.webui.chathistory', value: JSON.stringify(msgs) }) });
  } catch(e) {}
}

async function loadChatHistory() {
  if (state._chatHistoryLoaded || !state.currentConn) return;
  try {
    const s = await api('/settings?prefix=plugin.webui');
    const raw = s?.settings?.['plugin.webui.chathistory'];
    if (raw) {
      const parsed = JSON.parse(raw);
      if (Array.isArray(parsed) && parsed.length > 0) {
        state.messages = parsed.map(function(m) { return { role: m.role, content: m.content || '', reasoning_content: m.reasoning_content || '' } });
        state._chatHistoryLoaded = true;
        renderChat();
      }
    }
  } catch(e) {}
  state._chatHistoryLoaded = true;
}

// hook into renderChat init
const _origRenderChat = renderChat;
renderChat = function() {
  loadChatHistory();
  return _origRenderChat.apply(this, arguments);
};

async function queryMemoryChat() {
  const q = document.getElementById('mem-query')?.value; const r = document.getElementById('mem-result-chat');
  if (!r || !q) return; r.innerHTML = '<div class="loading"></div>';
  try { const d = await api('/memory?q=' + encodeURIComponent(q) + '&depth=2'); r.innerHTML = '<pre style="font-size:11px">' + escHtml(JSON.stringify(d, null, 2)) + '</pre>'; }
  catch(e) { r.innerHTML = '<p style="color:#fca5a5">Query failed: ' + escHtml(e.message) + '</p>'; }
}

async function queryMemoryContext() {
  const q = document.getElementById('ctx-query')?.value; const r = document.getElementById('ctx-result');
  if (!r) return; r.innerHTML = '<div class="loading"></div>';
  try {
    const d = await api('/memory/context?q=' + encodeURIComponent(q || ''));
    let html = '<div style="font-size:11px">';
    if (d?.summary) html += '<div class="kv-row"><span class="key">Summary</span><span class="val">' + escHtml(d.summary) + '</span></div>';
    html += '<div class="kv-row"><span class="key">Token Estimate</span><span class="val">' + (d?.token_estimate || 0) + '</span></div>';
    if (d?.entities?.length) html += '<div class="kv-row"><span class="key">Entities</span><span class="val">' + d.entities.map(function(e) { return escHtml(e.name || e.id || '') }).join(', ') + '</span></div>';
    html += '<h3 style="font-size:12px;margin:8px 0 4px">Context</h3><pre>' + escHtml(d?.context || 'No context') + '</pre></div>';
    r.innerHTML = html;
  } catch(e) { r.innerHTML = '<p style="color:#fca5a5">Query failed: ' + escHtml(e.message) + '</p>'; }
}

async function searchKnowledgeChat() {
  const q = document.getElementById('know-query')?.value; const r = document.getElementById('know-result-chat');
  if (!r || !q) return; r.innerHTML = '<div class="loading"></div>';
  try { const d = await api('/knowledge?q=' + encodeURIComponent(q)); r.innerHTML = '<pre style="font-size:11px">' + escHtml(JSON.stringify(d, null, 2)) + '</pre>'; }
  catch(e) { r.innerHTML = '<p style="color:#fca5a5">Search failed: ' + escHtml(e.message) + '</p>'; }
}

async function createKnowledgeChat() {
  const name = document.getElementById('know-name')?.value; const content = document.getElementById('know-content')?.value;
  if (!name || !content) { toast('Name and content required', true); return; }
  try { await api('/knowledge', { method: 'POST', body: JSON.stringify({ name, content }) }); toast('Knowledge created'); document.getElementById('know-name').value = ''; document.getElementById('know-content').value = ''; }
  catch(e) { toast('Create failed: ' + e.message, true); }
}

// ===== Plugins =====
async function renderPlugins() {
  const list = state.installedPlugins?.plugins || []; const info = state.pluginInfo;
  let html = '<div class="card"><div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:12px">'
    + '<h2 style="margin-bottom:0">Installed Plugins</h2><div style="display:flex;gap:8px">'
    + '<button class="btn btn-ghost btn-sm" onclick="document.getElementById(\'plugin-file-input\').click()">Upload .hmap</button>'
    + '<input type="file" id="plugin-file-input" accept=".hmap,.so,.dll" style="display:none" onchange="installPluginFile(this.files[0])">'
    + '<button class="btn btn-ghost btn-sm" onclick="reloadPlugins()">Reload</button></div></div>';
  if (list.length === 0) { html += '<p class="empty-state">No plugins installed</p>'; }
  else {
    html += '<table><tr><th>Name</th><th>Type</th><th>Status</th><th></th></tr>';
    list.forEach(function(p) {
      const status = p.loaded ? '<span class="badge badge-green">Loaded</span>' : '<span class="badge badge-red">Error</span>';
      html += '<tr><td>' + escHtml(p.name || '') + '</td><td>' + escHtml(p.type || '') + '</td><td>' + status + '</td>'
        + '<td><button class="btn btn-ghost btn-sm" onclick="showPluginInfo(\'' + p.name + '\')">Info</button></td></tr>';
    }); html += '</table>';
  }
  html += '</div>';
  if (info) {
    html += '<div class="card"><h2>' + escHtml(info.name || '') + ' Details</h2><pre>' + escHtml(JSON.stringify(info, null, 2)) + '</pre>'
      + '<button class="btn btn-ghost btn-sm" onclick="state.pluginInfo=null;renderPlugins()" style="margin-top:8px">Close</button></div>';
  }
  document.getElementById('tab-plugins').innerHTML = html;
}

async function showPluginInfo(name) {
  try { const d = await api('/plugins/' + encodeURIComponent(name)); state.pluginInfo = d; renderPlugins(); }
  catch(e) { toast('Failed: ' + e.message, true); }
}

async function installPluginFile(file) {
  if (!file) return;
  try {
    const form = new FormData(); form.append('plugin', file);
    await fetch(state.currentConn.url + '/api/v1/plugins', { method: 'POST', body: form, headers: state.currentConn.apiKey ? { 'X-API-Key': state.currentConn.apiKey } : {} });
    toast('Plugin uploaded'); state.installedPlugins = await api('/plugins'); renderPlugins();
  } catch(e) { toast('Upload failed: ' + e.message, true); }
}

async function reloadPlugins() {
  try { await api('/plugins/reload', { method: 'POST' }); state.kernel = await api('/kernel'); state.installedPlugins = await api('/plugins'); toast('Plugins reloaded'); renderPlugins(); renderOverview(); }
  catch(e) { toast('Reload failed: ' + e.message, true); }
}

// ===== Knowledge Browser =====
async function renderKnowledgeBrowser() {
  if (document.getElementById('tab-knowledge')) return;
  const tab = document.getElementById('tab-kernel');
  if (!tab || !tab.classList.contains('active')) return;
  const cont = document.getElementById('knowledge-browser');
  if (!cont) return;
  try {
    const d = await api('/knowledge');
    let html = '<div class="card"><h2>Knowledge Base</h2>';
    if (d?.categories) {
      html += '<table><tr><th>Name</th><th>Size</th></tr>';
      (d.categories || []).forEach(function(c) {
        html += '<tr><td>' + escHtml(c.name || c) + '</td><td>' + (c.content_length || '-') + '</td></tr>';
      });
      html += '</table>';
    }
    if (d?.stats) {
      html += '<div class="grid-3" style="margin-top:12px">' + statCard('Categories', d.stats.categories || 0) + statCard('Items', d.stats.items || 0) + statCard('Size', d.stats.size || 0) + '</div>';
    }
    html += '</div>';
    cont.innerHTML = html;
  } catch(e) { cont.innerHTML = '<p style="color:#fca5a5">' + escHtml(e.message) + '</p>'; }
}

// ===== Settings =====
function renderOneSettings() {
  const section = state.selectedSection || 'core';
  const isPlugin = section.startsWith('plugin.'); const prefix = isPlugin ? section : (section === 'core' ? '' : section);
  const values = {}; const meta = {};
  if (isPlugin) { const pname = section.substring(7); Object.entries(state.settings).filter(function(e) { return e[0].startsWith('plugin.' + pname + '.') }).forEach(function(e) { values[e[0]] = e[1] }) }
  else if (section === 'core') { Object.entries(state.settings).filter(function(e) { return !e[0].startsWith('plugin.') }).forEach(function(e) { values[e[0]] = e[1] }) }
  else { Object.entries(state.settings).filter(function(e) { return e[0].startsWith(section + '.') || (!e[0].startsWith('plugin.') && e[0].startsWith(section)) }).forEach(function(e) { values[e[0]] = e[1] }) }
  Object.assign(meta, state.meta);
  let html = '<div class="card"><h2>' + (isPlugin ? 'Plugin: ' + section.substring(7) : 'Core Settings') + '</h2>';
  const keys = Object.keys(values);
  if (keys.length === 0) { html += '<p class="empty-state">No settings</p>' }
  else {
    keys.sort().forEach(function(k) {
      const v = values[k]; const m = meta[k]; const display = m?.displayName || k.split('.').pop() || k; const desc = m?.description || '';
      html += '<div style="margin-bottom:12px;padding-bottom:12px;border-bottom:1px solid var(--kv-border)"><div class="settings-key">' + escHtml(k) + '</div><label>' + escHtml(display) + '</label>'
        + '<input value="' + escHtml(v != null ? String(v) : '') + '" onchange="saveSetting(\'' + escHtml(k) + '\', this.value)" placeholder="' + escHtml(desc) + '">'
        + (desc ? '<div style="font-size:10px;color:var(--text-muted);margin-top:-8px;margin-bottom:4px">' + escHtml(desc) + '</div>' : '') + '</div>';
    });
  }
  html += '</div>';
  document.getElementById('tab-settings').innerHTML = '<div class="settings-layout"><div class="settings-sidebar">'
    + state.settingsPlugins.map(function(p) { const label = p === 'core' ? 'Core' : p.replace('plugin.', ''); return '<a class="' + (section === p ? 'active' : '') + '" onclick="state.selectedSection=\'' + p + '\';renderOneSettings()">' + escHtml(label) + '</a>' }).join('')
    + '</div><div class="settings-content">' + html + '</div></div>';
}

async function saveSetting(key, value) {
  try { await api('/settings', { method: 'PUT', body: JSON.stringify({ key, value }) }); toast('Saved'); const s = await api('/settings'); state.settings = s.settings || {}; state.meta = s.meta || {}; renderOneSettings(); }
  catch(e) { toast('Save failed: ' + e.message, true); }
}

// ===== Adapters =====
async function renderAdapters() {
  let html = '<div class="card"><div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:12px"><h2 style="margin-bottom:0">LLM Adapters</h2><button class="btn btn-primary btn-sm" onclick="showUploadAdapter()">Upload</button></div>';
  try {
    const d = await api('/adapters'); const adapters = d?.adapters || [];
    if (adapters.length === 0) { html += '<p class="empty-state">No adapters</p>' }
    else {
      html += '<table><tr><th>Name</th><th>Type</th><th></th></tr>';
      adapters.forEach(function(a) { html += '<tr><td>' + escHtml(a.name || a) + '</td><td>' + escHtml(a.type || 'lua') + '</td><td><button class="btn btn-danger btn-sm" onclick="deleteAdapter(\'' + escHtml(a.name || a) + '\')">Delete</button></td></tr>' });
      html += '</table>';
    }
  } catch(e) { html += '<p class="empty-state">Failed to load: ' + escHtml(e.message) + '</p>' }
  html += '</div>';
  document.getElementById('tab-adapters').innerHTML = html;
}

function showUploadAdapter() { const name = prompt('Adapter name:'); if (!name) return; const code = prompt('Paste Lua adapter code:'); if (!code) return; uploadAdapter(name, code); }
async function uploadAdapter(name, code) { try { await api('/adapters', { method: 'POST', body: JSON.stringify({ name, code }) }); toast('Uploaded'); renderAdapters(); } catch(e) { toast('Failed: ' + e.message, true); } }
async function deleteAdapter(name) { if (!confirm('Delete: ' + name + '?')) return; try { await api('/adapters/' + encodeURIComponent(name), { method: 'DELETE' }); toast('Deleted'); renderAdapters(); } catch(e) { toast('Failed: ' + e.message, true); } }

// ===== Kernel =====
async function renderKernel() {
  const k = state.kernel;
  let html = '<div class="card"><h2>Kernel Status</h2>';
  if (!k) { html += '<p class="empty-state">Unavailable</p>' }
  else {
    html += '<div class="grid-2"><div><h3>LLM</h3><div class="kv-row"><span class="key">Provider</span><span class="val">' + escHtml(k.llm?.provider || '-') + '</span></div><div class="kv-row"><span class="key">Sources</span><span class="val">' + (k.llm?.sources || 0) + '</span></div><div class="kv-row"><span class="key">Available</span><span class="val"><span class="status-dot ' + (k.llm?.available ? 'dot-green' : 'dot-red') + '"></span>' + (k.llm?.available ? 'Yes' : 'No') + '</span></div></div>'
    + '<div><h3>Memory</h3><div class="kv-row"><span class="key">Available</span><span class="val"><span class="status-dot ' + (k.memory?.available ? 'dot-green' : 'dot-gray') + '"></span>' + (k.memory?.available ? 'Yes' : 'No') + '</span></div>'
    + (k.memory?.available ? '<div class="kv-row"><span class="key">Entities</span><span class="val">' + k.memory.entity_count + '</span></div><div class="kv-row"><span class="key">Relations</span><span class="val">' + k.memory.relation_count + '</span></div>' : '') + '</div></div>';
    html += '<h3>Runtime</h3><div class="kv-row"><span class="key">Goroutines</span><span class="val">' + (k.runtime?.goroutines || '-') + '</span></div><div class="kv-row"><span class="key">Memory</span><span class="val">' + (k.runtime?.memory_mb || '-') + ' MB</span></div><div class="kv-row"><span class="key">Go Version</span><span class="val">' + escHtml(k.runtime?.go_version || '-') + '</span></div>';
    html += '<h3>Plugins</h3>';
    if (k.plugins && k.plugins.length > 0) { html += '<div>' + k.plugins.map(function(p) { return '<span class="badge badge-blue" style="margin:2px">' + escHtml(p.name || p) + '</span>' }).join('') + '</div>' }
  }
  html += '</div><div class="card"><h2>Actions</h2><button class="btn btn-primary" onclick="runHealthcheck()" style="margin-right:8px">Run Healthcheck</button>'
    + '<button class="btn btn-ghost" onclick="loadTextMemory()">View Text Memory</button></div>'
    + '<div id="health-result"></div><div id="text-memory-result"></div>'
    + '<div id="knowledge-browser"></div>';
  document.getElementById('tab-kernel').innerHTML = html;
  renderKnowledgeBrowser();
}

async function runHealthcheck() {
  const el = document.getElementById('health-result'); el.innerHTML = '<div class="loading-spinner"></div>';
  try { const r = await api('/chat', { method: 'POST', body: JSON.stringify({ message: 'Please run the healthcheck tool for a full system check and report the results' }) }); el.innerHTML = '<div class="card"><h2>Healthcheck Result</h2><pre>' + escHtml(r.response || '') + '</pre></div>'; }
  catch(e) { el.innerHTML = '<div class="card"><p style="color:#fca5a5">' + escHtml(e.message) + '</p></div>'; }
}
async function loadTextMemory() {
  const el = document.getElementById('text-memory-result'); el.innerHTML = '<div class="loading-spinner"></div>';
  try { const d = await api('/memory/text'); el.innerHTML = '<div class="card"><h2>Text Memory</h2><pre>' + escHtml(JSON.stringify(d, null, 2)) + '</pre></div>'; }
  catch(e) { el.innerHTML = '<div class="card"><p style="color:#fca5a5">' + escHtml(e.message) + '</p></div>'; }
}

// ===== Init =====
document.addEventListener('DOMContentLoaded', initApp);
