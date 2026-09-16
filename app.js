/* prismctl 前端：右侧提问/答案/原始 JSON，左侧历史回合 + 工具调用 + 事件流调试面板 */
var $ = function (id) { return document.getElementById(id); };
var FIELDS = ['cookie','sandboxToken','listenSnapshot','conversationId','projectId','userId',
              'model','reasoningEffort','sandboxUrl','context'];

var state = { turns: [], sel: -1, selItem: null, polls: 0, timer: null, boot: null };

window.onerror = function (msg, src, line) {
  writeLog('!! JS 错误: ' + msg + ' @' + String(src || '').split('/').pop() + ':' + line);
};

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"]/g, function (c) {
    return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
  });
}
function clock(t) {
  var d = new Date(t), p = function (n) { return (n < 10 ? '0' : '') + n; };
  return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
}
function writeLog(s) { $('log').textContent = s; }
function appendLog(s) {
  var l = $('log');
  l.textContent += s + '\n';
  l.scrollTop = l.scrollHeight;
}

function form() {
  var o = { prompt: ($('prompt').value || '').trim() };
  FIELDS.forEach(function (f) { o[f] = ($(f).value || '').trim(); });
  return o;
}
async function call(path, payload) {
  var r = await fetch(path, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(payload || {}) });
  return { status: r.status, upstream: r.headers.get('x-upstream-status'), text: await r.text() };
}
function parse(t) { try { return JSON.parse(t); } catch (e) { return {}; } }

function setHealth(j) {
  var d = ((((j || {}).response || {}).payload) || {}).codexRequestDebug;
  if (!d) return;
  var f = function (k) { return d[k] ? '<span class="ok">' + k + '✓</span>' : '<span class="bad">' + k + '✗</span>'; };
  $('health').innerHTML = f('sandbox_token_present') + ' ' + f('listen_snapshot_present') + ' ' + f('backend_auth_token_present');
}

/* ---------- 历史 / 工具 / 事件 面板 ---------- */

function cur() { return state.turns[state.sel]; }

function absorb(turn, j) {
  if (!j || !j.status) return;
  turn.status = j.status;
  var p = (((j || {}).response || {}).payload) || {};
  if (p.reason) turn.reason = p.reason;
  var prog = j.codex_live_progress || {};
  // 工具调用与事件只在 pending 期间出现，逐轮合并去重
  (prog.toolCalls || []).forEach(function (t) {
    var key = t.call_id || ('line:' + t.line_index);
    var found = turn.tools.filter(function (x) { return x.key === key; })[0];
    if (found) { Object.assign(found, t); } else { t.key = key; turn.tools.push(t); }
  });
  (prog.eventPreviews || []).forEach(function (e) {
    var key = 'line:' + e.line_index;
    if (!turn.events.filter(function (x) { return x.key === key; }).length) { e.key = key; turn.events.push(e); }
  });
  turn.events.sort(function (a, b) { return (a.line_index || 0) - (b.line_index || 0); });
  var texts = [];
  (p.output || []).forEach(function (o) {
    (o.content || []).forEach(function (c) { if (c.text) texts.push(c.text); });
  });
  if (texts.length) turn.answer = texts.join('\n\n');
  turn.deltas = p.codexDeltaFiles || turn.deltas || [];
  turn.execMeta = p.codexExecMeta || turn.execMeta;
  if (j.turn_state) turn.hasTurnState = true;
  turn.lastRaw = JSON.stringify(j, null, 2);
}

function renderTurns() {
  var box = $('turns');
  if (!state.turns.length) { box.innerHTML = '<div class="empty">还没有回合</div>'; return; }
  box.innerHTML = state.turns.map(function (t, i) {
    var cls = 'card' + (i === state.sel ? ' sel' : '');
    var st = t.status === 'completed' ? '<span class="tag ok">completed</span>'
           : t.status === 'started' ? '<span class="tag">running</span>'
           : t.status ? '<span class="tag">' + esc(t.status) + '</span>' : '<span class="tag">发送中</span>';
    return '<div class="' + cls + '" onclick="selectTurn(' + i + ')">' +
      '<div class="t1"><span class="idx">#' + (i + 1) + '</span><span class="txt">' + esc(t.prompt) + '</span>' + st + '</div>' +
      '<div class="meta"><span>' + clock(t.t) + '</span><span>工具 ' + t.tools.length + '</span>' +
      '<span>事件 ' + t.events.length + '</span>' + (t.deltas.length ? '<span>文件 ' + t.deltas.length + '</span>' : '') +
      (t.reason ? '<span class="bad">' + esc(t.reason) + '</span>' : '') + '</div></div>';
  }).join('');
}

function renderTools() {
  var box = $('tools'), t = cur();
  var list = t ? t.tools : [];
  $('toolCount').textContent = list.length;
  if (!list.length) { box.innerHTML = '<div class="empty">沙箱执行期间这里会实时出现</div>'; return; }
  box.innerHTML = list.map(function (x, i) {
    var args = x.arguments_preview || '';
    var cmd = args;
    try { var a = JSON.parse(args); cmd = a.cmd || a.command || a.path || args; } catch (e) {}
    var sel = state.selItem && state.selItem.id === x.key && state.selItem.kind === 'tool';
    return '<div class="card tool' + (sel ? ' sel' : '') + '" onclick="selectItem(\'tool\',' + i + ')">' +
      '<div class="t1"><span class="idx">L' + (x.line_index || '?') + '</span><span class="txt">' + esc(x.name || x.call_type) + '</span>' +
      '<span class="tag">' + esc(x.call_type || '') + '</span></div>' +
      (cmd && cmd !== args ? '<div class="cmd">' + esc(String(cmd).slice(0, 300)) + '</div>' : '') +
      '<div class="meta"><span>' + esc((x.call_id || '').slice(0, 22)) + '</span><span>' + esc(x.source || '') + '</span></div></div>';
  }).join('');
}

function renderEvents() {
  var box = $('events'), t = cur();
  var list = t ? t.events : [];
  $('eventCount').textContent = list.length;
  if (!list.length) { box.innerHTML = '<div class="empty">task_started / user_message / function_call …</div>'; return; }
  box.innerHTML = list.map(function (e, i) {
    var sel = state.selItem && state.selItem.id === e.key && state.selItem.kind === 'event';
    return '<div class="card' + (sel ? ' sel' : '') + '" onclick="selectItem(\'event\',' + i + ')">' +
      '<div class="t1"><span class="idx">L' + e.line_index + '</span><span class="txt">' +
      esc(e.payload_type || e.summary || e.event_type) + '</span><span class="tag">' + esc(e.event_type) + '</span></div></div>';
  }).join('');
}

function renderAnswer() {
  var t = cur();
  if (!t) { $('answer').innerHTML = '<span class="dim">等待提问…</span>'; return; }
  var html = '<div class="answer-text">' + esc(t.answer || '') + '</div>';
  if (!t.answer) {
    html = '<div class="answer-text"><span class="dim">' +
      (t.status === 'completed' ? '（本轮回合没有文本输出）' : '运行中…（已轮询 ' + state.polls + ' 次）') + '</span></div>' +
      (t.reason ? '<div class="bad">[' + esc(t.reason) + '] 缺少 sandbox_token / listen_snapshot 时会这样</div>' : '');
  }
  (t.deltas || []).forEach(function (d) {
    html += '<div class="dim">📄 ' + esc(d.file_path) + ' <b>' + esc(d.status) + '</b>' + (d.diff_truncated ? '（diff 已截断）' : '') + '</div>';
  });
  $('answer').innerHTML = html;
}

function selectTurn(i) {
  state.sel = i;
  state.selItem = null;
  var t = state.turns[i];
  renderTurns(); renderTools(); renderEvents(); renderAnswer();
  $('dbgTitle').textContent = '回合 #' + (i + 1) + ' 最后一次原始响应';
  writeLog(t.lastRaw || '(无)');
}

function selectItem(kind, i) {
  var t = cur();
  if (!t) return;
  var item = kind === 'tool' ? t.tools[i] : t.events[i];
  state.selItem = { kind: kind, id: item.key };
  renderTools(); renderEvents();
  $('dbgTitle').textContent = kind === 'tool'
    ? '工具调用 ' + (item.name || '') : '事件 ' + (item.payload_type || item.event_type || '');
  var raw = item.raw || JSON.stringify(item, null, 2);
  try { raw = JSON.stringify(JSON.parse(raw), null, 2); } catch (e) { /* 保持原文 */ }
  writeLog(raw);
}

function exportHistory() {
  var blob = new Blob([JSON.stringify({ exportedAt: new Date().toISOString(), turns: state.turns }, null, 2)],
                      { type: 'application/json' });
  var a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = 'prismctl-history.json';
  a.click();
  URL.revokeObjectURL(a.href);
}

/* ---------- 主流程 ---------- */

function stopPoll() { if (state.timer) { clearInterval(state.timer); state.timer = null; } }

async function send() {
  try {
    var f = form();
    if (!f.prompt) {
      f.prompt = '这个项目里有哪些文件？只列文件名，一行一个。';
      $('prompt').value = f.prompt;
    }
    stopPoll();
    var turn = { t: Date.now(), prompt: f.prompt, status: '', tools: [], events: [], deltas: [] };
    state.turns.push(turn);
    state.sel = state.turns.length - 1;
    state.selItem = null;
    renderTurns(); renderTools(); renderEvents(); renderAnswer();
    $('status').textContent = '提交中…';

    var res = await call('/api/start', f);
    turn.startRaw = res.text;
    turn.lastRaw = res.text;
    var j = parse(res.text);
    absorb(turn, j);
    setHealth(j);
    renderTurns(); renderTools(); renderEvents(); renderAnswer();

    if (j.status === 'completed') { selectTurn(state.sel); $('status').textContent = '完成'; return; }
    if (!j.turn_state) {
      appendLog('[start] ' + res.status + ' 没有 turn_state（多半是 sandbox_token 缺失/过期）');
      turn.status = 'error';
      renderTurns();
      $('status').textContent = 'start 未受理';
      return;
    }
    state.polls = 0;
    // 1500ms 轮询：工具调用/事件只在 pending 期间回传，轮询太慢会漏掉
    state.timer = setInterval(poll, 1500);
    poll();
  } catch (e) {
    appendLog('!! send 失败: ' + e);
    $('status').textContent = '失败';
  }
}

async function poll() {
  if (state.polls++ > 60) { stopPoll(); return; }
  var turn = cur();
  try {
    var res = await call('/api/status', { cookie: ($('cookie').value || '').trim() });
    var j = parse(res.text);
    if (res.status !== 200) {
      appendLog('[status] HTTP ' + res.status + ' ' + res.text.slice(0, 300));
      stopPoll();
      return;
    }
    absorb(turn, j);
    setHealth(j);
    renderTurns(); renderTools(); renderEvents(); renderAnswer();
    if (turn.status === 'completed') {
      selectTurn(state.sel);
      stopPoll();
      $('status').textContent = '完成（' + state.polls + ' 次轮询）';
    } else {
      $('status').textContent = '运行中… 已轮询 ' + state.polls + ' 次' +
        (state.polls > 20 ? '（沙箱可能在冷启动，可点"沙箱探活"）' : '');
    }
  } catch (e) { appendLog('!! poll 失败: ' + e); stopPoll(); }
}

async function heartbeat() {
  try {
    var r = await call('/api/heartbeat', form());
    appendLog('[heartbeat] HTTP ' + r.status + ' upstream=' + (r.upstream || '') + ' ' + r.text.slice(0, 200));
  } catch (e) { appendLog('!! heartbeat 失败: ' + e); }
}

$('prompt').addEventListener('keydown', function (e) {
  if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); send(); }
});

(async function init() {
  try {
    var st = await (await fetch('/api/state')).json();
    state.boot = st;
    var b = st.bootstrap || {};
    var map = { conversationId: b.conversation_id, projectId: b.project_id, userId: b.user_id,
                sandboxToken: b.sandbox_token, sandboxUrl: b.sandbox_url, cookie: b.cookie,
                listenSnapshot: b.listen_snapshot };
    Object.keys(map).forEach(function (k) {
      if (map[k] && $(k) && !$(k).value) $(k).value = map[k];
    });
    appendLog('bootstrap: conversation=' + (b.conversation_id || '(新会话)') +
      ' sandbox_token=' + ((b.sandbox_token || '').length ? '已载入' : '缺失') +
      ' listen_snapshot=' + (b.listen_snapshot ? '已载入' : '缺失') +
      ' cookie=' + (st.cookie_loaded ? '服务端已载入' : (($('cookie').value || '').length ? '已填' : '缺失')));
  } catch (e) { appendLog('!! 初始化失败: ' + e); }
})();
