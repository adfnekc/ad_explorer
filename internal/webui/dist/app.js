'use strict';

/* AD_EXPLORER frontend.
 *
 * The host list and the passwords live here and only here: the gateway holds a
 * password for the length of one session and writes it nowhere. Everything in
 * this file that touches the wire is documented against
 * internal/wsframe/control.go and internal/wsframe/frame.go.
 */

// ---------------------------------------------------------------- constants

const HOSTS_KEY = 'adx_hosts_v1';

// Binary frame header, little endian:
//   0 type | 1 format | 2..3 flags | 4..5 x | 6..7 y | 8..9 w | 10..11 h
//   12..15 seq | 16..19 payload length
const HEADER_LEN = 20;
const TYPE_IMAGE = 0x01;
const TYPE_CURSOR = 0x02;
const TYPE_FILE = 0x03;
const FORMAT_WEBP = 0x01;
const FORMAT_JPEG = 0x02;
const FORMAT_PNG = 0x03;
const FORMAT_RAW = 0x04;

// File transfer. The gateway stores nothing: it asks for a range of an offered
// file when the remote's paste needs it, and streams a remote file back in
// chunks. Both directions are routed by the sequence field, so a chunk is never
// matched to a control frame by arrival order.
const FILE_OFFSET_LEN = 4;
const MAX_RANGE_PER_REQUEST = 1 << 20;

// How many clipboard entries to keep. The server does not keep any: a remote
// putting text on the clipboard must not overwrite what the operator is doing,
// so it lands here and is copied on request.
const CLIPBOARD_LIMIT = 50;

// Reconnection. A dropped socket is retried against the same session id, because
// the gateway keeps a detached session alive for its reconnect grace.
//
// The window is not assumed: the gateway reports how long it will hold this
// session, and the client retries for exactly that long. Guessing the default
// here would give up early against a gateway configured for longer, or keep
// retrying against one configured for shorter.
const RECONNECT_MIN_MS = 500;
const RECONNECT_MAX_MS = 5000;

// The active session id, per tab. A reload is the case this whole feature exists
// for: the page comes back holding nothing, and this is how it learns which
// desktop it was showing. sessionStorage rather than localStorage because a
// session belongs to one tab, and a second tab must not adopt it.
const SESSION_KEY = 'adx_active_session_v1';

// ---------------------------------------------------------------- state

const state = {
  hosts: [],
  selectedHostId: null,
  editingHostId: null,
  session: null, // { id, name, width, height }
  socket: null,
  clipboard: [],
  // localClipboard is what the local clipboard last held, and remoteClipboard
  // is what the remote last sent, so the one is not pushed back as though the
  // operator had copied it.
  // unauthorized records that the console's cookie was refused, so the page
  // stops retrying and stays on the logon form.
  unauthorized: false,
  localClipboard: '',
  remoteClipboard: '',
  frameCount: 0,

  // Files the operator offered, by name, so a range request can be answered.
  offered: new Map(),
  // Downloads in progress, by transfer id.
  downloads: new Map(),
  pingTimer: null,
  lastPing: 0,
  rtt: null,

  // Reconnect bookkeeping. reconnectDeadline is when the retry budget runs out;
  // zero means no retry is in progress.
  reconnectTimer: null,
  reconnectDelay: 0,
  reconnectDeadline: 0,
  // graceMs is what the gateway said it would hold this session for.
  graceMs: 0,
  // What has actually been sent, and what the remote said back. A remote that
  // reports no pointer position while the counters climb is a remote that is
  // receiving nothing, and counters that do not climb are the browser not
  // sending — two different problems with the same symptom on screen.
  sent: { move: 0, button: 0, key: 0, text: 0, wheel: 0, wheelEvents: 0 },
  remoteCursor: null,
  // systemCursor is the last pointer the server named, or null when it sent a
  // shape of its own or has not named one yet.
  systemCursor: null,
  // lastCloseCode is why the socket last closed, so the message shown when the
  // budget runs out can say whether another browser is holding the session.
  lastCloseCode: 0,
  // The operator asked to disconnect, or the gateway said the session is over,
  // so a close is final rather than a blip to retry.
  deliberateClose: false,
  sessionEnded: false,
};

// ---------------------------------------------------------------- elements

const $ = (id) => document.getElementById(id);

const el = {
  login: $('login'),
  loginForm: $('loginForm'),
  loginButton: $('loginButton'),
  loginStatus: $('loginStatus'),
  password: $('password'),

  workspace: $('workspace'),
  hostList: $('hostList'),
  hostForm: $('hostForm'),
  newHost: $('newHost'),
  cancelHost: $('cancelHost'),
  deleteHost: $('deleteHost'),
  fName: $('fName'),
  fHost: $('fHost'),
  fPort: $('fPort'),
  fUser: $('fUser'),
  fPassword: $('fPassword'),
  fProtocol: $('fProtocol'),
  fSize: $('fSize'),

  sessionBar: $('sessionBar'),
  sessionLabel: $('sessionLabel'),
  cad: $('cad'),
  fullscreen: $('fullscreen'),
  disconnect: $('disconnect'),

  screen: $('screen'),
  stats: $('stats'),
  inputTelemetry: $('inputTelemetry'),
  capability: $('capability'),

  quality: $('quality'),
  qualityValue: $('qualityValue'),
  resolution: $('resolution'),

  // The pointer is drawn by the browser, not into the canvas: a cursor is not
  // part of the desktop image, and the browser already knows how to place one.
  cursorURL: null,

  offered: $('offered'),
  transfers: $('transfers'),
  remoteFiles: $('remoteFiles'),
  offerFiles: $('offerFiles'),
  clearOffer: $('clearOffer'),
  filePicker: $('filePicker'),

  clipboard: $('clipboard'),
  clipboardOut: $('clipboardOut'),
  sendClipboard: $('sendClipboard'),
  requestClipboard: $('requestClipboard'),
  clearClipboard: $('clearClipboard'),
  messages: $('messages'),
  signOut: $('signOut'),
};

const ctx = el.screen.getContext('2d', { alpha: false });
ctx.imageSmoothingEnabled = false;

// ---------------------------------------------------------------- transport

async function api(path, options = {}) {
  const response = await fetch(path, {
    credentials: 'same-origin',
    headers: options.body ? { 'Content-Type': 'application/json' } : undefined,
    ...options,
  });
  let body = null;
  const text = await response.text();
  if (text) {
    try { body = JSON.parse(text); } catch { body = { code: 'bad_response', message: text }; }
  }
  if (!response.ok) {
    const error = new Error((body && body.message) || `HTTP ${response.status}`);
    error.code = (body && body.code) || 'http_error';
    error.status = response.status;
    error.body = body || {};
    // The console's own cookie has expired. Nothing on this page can fix that,
    // and the way the rest of this file reports failures would call it a network
    // problem and keep retrying a session that cannot come back. A failed logon
    // is the one 401 that must not land here, or the page would stand in its own
    // way while showing the message that explains the refusal.
    if (response.status === 401 && path !== '/api/login') onAuthExpired();
    throw error;
  }
  return body || {};
}

function send(control) {
  if (!state.socket || state.socket.readyState !== WebSocket.OPEN) return false;
  switch (control.type) {
    case 'input.mouse': state.sent[control.action === 'move' ? 'move' : 'button']++; break;
    case 'input.key': state.sent.key++; break;
    case 'input.text': state.sent.text++; break;
    // Counted as notches rather than events, because one flick of a wheel can
    // arrive as one event carrying several.
    case 'input.wheel': state.sent.wheel += Math.abs(control.delta); break;
  }
  state.socket.send(JSON.stringify(control));
  return true;
}

// ---------------------------------------------------------------- messages

function note(text, kind) {
  const div = document.createElement('div');
  div.className = 'message' + (kind ? ' ' + kind : '');
  div.textContent = text;
  el.messages.prepend(div);
  while (el.messages.childElementCount > 50) el.messages.lastElementChild.remove();
}

function noteError(code, message) {
  const div = document.createElement('div');
  div.className = 'message error';
  const span = document.createElement('span');
  span.className = 'code';
  span.textContent = code;
  div.append(span, document.createTextNode(' ' + message));
  el.messages.prepend(div);
  while (el.messages.childElementCount > 50) el.messages.lastElementChild.remove();
}

// ---------------------------------------------------------------- host store

function loadHosts() {
  try {
    const raw = localStorage.getItem(HOSTS_KEY);
    const parsed = raw ? JSON.parse(raw) : [];
    state.hosts = Array.isArray(parsed) ? parsed.filter(isHost) : [];
  } catch {
    // A corrupt store must not stop the console from opening; it is reported
    // and replaced on the next save.
    state.hosts = [];
    note('主机列表无法解析，已忽略。', 'error');
  }
}

function isHost(h) {
  return h && typeof h.id === 'string' && typeof h.host === 'string';
}

function saveHosts() {
  localStorage.setItem(HOSTS_KEY, JSON.stringify(state.hosts));
}

function hostById(id) {
  return state.hosts.find((h) => h.id === id) || null;
}

function newId() {
  const b = new Uint8Array(8);
  crypto.getRandomValues(b);
  return [...b].map((v) => v.toString(16).padStart(2, '0')).join('');
}

function renderHosts() {
  el.hostList.replaceChildren();
  if (state.hosts.length === 0) {
    const p = document.createElement('div');
    p.className = 'label';
    p.style.padding = '6px';
    p.textContent = '还没有主机。点「新建」添加一台，凭据只保存在这台浏览器里。';
    el.hostList.append(p);
    return;
  }
  for (const host of state.hosts) {
    const row = document.createElement('div');
    row.className = 'host';
    row.setAttribute('role', 'button');
    row.tabIndex = 0;
    row.setAttribute('aria-selected', String(host.id === state.selectedHostId));

    const left = document.createElement('div');
    const name = document.createElement('div');
    name.className = 'name';
    name.textContent = host.name || host.host;
    const where = document.createElement('div');
    where.className = 'where';
    where.textContent = `${host.host}:${host.port || 3389} · ${host.user || '未填账号'}`;
    left.append(name, where);

    const rtt = document.createElement('div');
    rtt.className = 'rtt';
    rtt.textContent = host.id === state.selectedHostId && state.rtt !== null
      ? `${state.rtt}ms` : '';

    row.append(left, rtt);
    const pick = () => {
      state.selectedHostId = host.id;
      state.editingHostId = null;
      renderHosts();
    };
    row.addEventListener('click', pick);
    row.addEventListener('dblclick', () => { pick(); openHostForm(host.id); });
    row.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); pick(); }
    });
    el.hostList.append(row);
  }
}

function openHostForm(id) {
  state.editingHostId = id || null;
  const host = id ? hostById(id) : null;
  el.fName.value = host ? host.name || '' : '';
  el.fHost.value = host ? host.host : '';
  el.fPort.value = host ? host.port || 3389 : 3389;
  el.fUser.value = host ? host.user || '' : '';
  el.fPassword.value = host ? host.password || '' : '';
  el.fProtocol.value = host ? host.protocol || 'nla' : 'nla';
  el.fSize.value = host ? host.size || '1280x720' : '1280x720';
  el.deleteHost.hidden = !host;
  el.hostForm.hidden = false;
  el.fHost.focus();
}

function closeHostForm() {
  el.hostForm.hidden = true;
  state.editingHostId = null;
}

el.hostForm.addEventListener('submit', (event) => {
  event.preventDefault();
  const [width, height] = el.fSize.value.split('x').map(Number);
  const entry = {
    id: state.editingHostId || newId(),
    name: el.fName.value.trim() || el.fHost.value.trim(),
    host: el.fHost.value.trim(),
    port: Number(el.fPort.value) || 3389,
    user: el.fUser.value.trim(),
    password: el.fPassword.value,
    protocol: el.fProtocol.value,
    size: el.fSize.value,
    width,
    height,
  };
  if (!entry.host) { note('地址不能为空。', 'error'); return; }

  const index = state.hosts.findIndex((h) => h.id === entry.id);
  if (index >= 0) state.hosts[index] = entry; else state.hosts.push(entry);
  state.selectedHostId = entry.id;
  saveHosts();
  renderHosts();
  closeHostForm();
  connect(entry);
});

el.deleteHost.addEventListener('click', () => {
  if (!state.editingHostId) return;
  state.hosts = state.hosts.filter((h) => h.id !== state.editingHostId);
  if (state.selectedHostId === state.editingHostId) state.selectedHostId = null;
  saveHosts();
  renderHosts();
  closeHostForm();
});

el.newHost.addEventListener('click', () => openHostForm(null));
el.cancelHost.addEventListener('click', closeHostForm);

// ---------------------------------------------------------------- session

async function connect(host) {
  if (state.session) disconnect();
  state.deliberateClose = false;
  state.sessionEnded = false;
  state.frameCount = 0;
  state.graceMs = 0;
  el.sessionLabel.textContent = `连接 ${host.name || host.host} …`;

  let created;
  try {
    created = await api('/api/sessions', {
      method: 'POST',
      body: JSON.stringify({
        host: host.host,
        port: host.port,
        user: host.user,
        password: host.password,
        protocol: host.protocol,
        width: host.width,
        height: host.height,
        name: host.name || host.host,
      }),
    });
  } catch (error) {
    el.sessionLabel.textContent = '未连接';
    noteError(error.code, error.message);
    return;
  }

  state.session = created;
  state.graceMs = Math.max(0, created.reconnectGraceMs || 0);
  try { sessionStorage.setItem(SESSION_KEY, created.id); } catch { /* private mode */ }
  el.screen.width = created.width;
  el.screen.height = created.height;
  ctx.fillStyle = '#008080';
  ctx.fillRect(0, 0, created.width, created.height);

  openSocket(created.id);
}

function openSocket(id, reconnecting) {
  const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
  const socket = new WebSocket(`${scheme}://${location.host}/api/sessions/${id}/stream`);
  socket.binaryType = 'arraybuffer';
  state.socket = socket;

  socket.addEventListener('open', () => {
    // A reconnect that got through clears the retry budget, so the next blip
    // gets the full grace again.
    cancelReconnect();
    if (reconnecting) note('已重新连接。');

    // The gateway remembers an offer for as long as the session lives, but the
    // File handles behind it belong to the page. After a reload the browser has
    // none while the gateway still advertises them, and a paste on the remote
    // would ask for bytes that can never arrive. An offer the browser cannot
    // serve is withdrawn rather than left standing.
    if (state.offered.size === 0) send({ type: 'files.clear' });
    el.sessionLabel.textContent = `已连接 ${state.session.name}`;
    el.cad.disabled = false;
    el.fullscreen.disabled = false;
    el.disconnect.disabled = false;
    renderDisconnectButton();
    el.sendClipboard.disabled = false;
    el.requestClipboard.disabled = false;
    startClipboardWatch();
    el.resolution.disabled = false;
    el.offerFiles.disabled = false;
    el.clearOffer.disabled = false;
    // The HUD's round trip figure is measured, not guessed.
    state.pingTimer = setInterval(() => {
      state.lastPing = performance.now();
      send({ type: 'ping', t: Date.now() });
    }, 1000);
    send({ type: 'session.quality', level: Number(el.quality.value) });
  });

  socket.addEventListener('message', (event) => {
    if (typeof event.data === 'string') {
      handleControl(JSON.parse(event.data));
      return;
    }
    handleFrame(event.data);
  });

  socket.addEventListener('close', (event) => {
    stopPing();
    // A superseded socket's close says nothing about the socket that replaced
    // it, so it must not drive reconnection.
    if (socket !== state.socket) return;

    state.lastCloseCode = event.code;

    // 4002 is the gateway saying the session is gone. Retrying against an id
    // that no longer exists can only fail, so this one is final.
    if (event.code === 4002) {
      state.sessionEnded = true;
      el.sessionLabel.textContent = '连接已结束：会话已不存在';
      noteError('session_gone', '会话已经结束；请重新连接主机。');
      endSession();
      return;
    }

    if (state.deliberateClose || state.sessionEnded) {
      const reason = event.reason ? `：${event.reason}` : '';
      el.sessionLabel.textContent = `连接已结束 (${event.code})${reason}`;
      note(`会话结束，代码 ${event.code}${reason}`);
      endSession();
      return;
    }
    // A refused WebSocket upgrade arrives as an abnormal close with nothing
    // said about why, so the reason is asked for once here. An expired cookie
    // answers 401 and the page goes back to the logon form; anything else
    // answers normally and reconnecting carries on as before.
    api('/api/sessions').catch(() => {});
    scheduleReconnect();
  });

  socket.addEventListener('error', () => {
    noteError('socket_error', 'WebSocket 出错，连接可能已断开。');
  });
}

// onAuthExpired returns the page to the logon form.
//
// It is deliberately final: the session is dropped rather than retried, because
// every retry would be refused the same way. Without this the page sat on a
// canvas that would never update again, and said so as though the network were
// at fault.
function onAuthExpired() {
  if (state.unauthorized) return;
  state.unauthorized = true;
  state.sessionEnded = true; // stop the reconnect loop
  cancelReconnect();
  if (state.socket) {
    try { state.socket.close(); } catch { /* already gone */ }
    state.socket = null;
  }
  if (state.session) {
    state.session = null;
    try { sessionStorage.removeItem(SESSION_KEY); } catch { /* private mode */ }
  }
  el.loginStatus.textContent = '控制台登录已过期，请重新输入口令。';
  showLogin();
}

// scheduleReconnect retries the same session with a growing delay. It stops once
// the gateway's grace has run out, because past that the session really is gone
// and the operator has to be told rather than left watching a dead canvas.
function scheduleReconnect() {
  if (!state.session) {
    endSession();
    return;
  }

  // A gateway with the grace turned off will not hold the session, so there is
  // nothing to wait for.
  if (state.graceMs <= 0) {
    el.sessionLabel.textContent = '连接已断开，会话已结束';
    noteError('session_gone', '连接已断开，且网关不会保留会话；请重新连接主机。');
    endSession();
    return;
  }

  const now = performance.now();
  if (!state.reconnectDeadline) {
    state.reconnectDeadline = now + state.graceMs;
    state.reconnectDelay = RECONNECT_MIN_MS;
    note('连接断开，正在尝试重新连接 …');
  }
  if (now >= state.reconnectDeadline) {
    // 4001 throughout means something else was holding the session the whole
    // time, which is worth saying rather than reporting a generic failure: the
    // operator's other tab is the likely answer, not a broken gateway.
    if (state.lastCloseCode === 4001) {
      el.sessionLabel.textContent = '连接已结束：会话被占用';
      noteError('session_in_use',
        '重连期间这个会话一直被另一个浏览器占用。一个会话只能有一个画面；请关掉另一个标签页后重新连接。');
    } else {
      el.sessionLabel.textContent = '连接已断开，会话已结束';
      noteError('session_gone', '重连失败，会话已结束；请重新连接主机。');
    }
    endSession();
    return;
  }

  const id = state.session.id;
  el.sessionLabel.textContent = '连接断开，正在重新连接 …';
  const delay = state.reconnectDelay;
  state.reconnectDelay = Math.min(delay * 2, RECONNECT_MAX_MS);
  state.reconnectTimer = setTimeout(() => {
    state.reconnectTimer = null;
    openSocket(id, true);
  }, delay);
}

function cancelReconnect() {
  if (state.reconnectTimer) {
    clearTimeout(state.reconnectTimer);
    state.reconnectTimer = null;
  }
  state.reconnectDeadline = 0;
  state.reconnectDelay = 0;
}

function stopPing() {
  if (state.pingTimer) { clearInterval(state.pingTimer); state.pingTimer = null; }
}

function endSession() {
  stopPing();
  cancelReconnect();
  state.socket = null;
  state.session = null;
  state.rtt = null;
  state.graceMs = 0;
  state.lastCloseCode = 0;
  try { sessionStorage.removeItem(SESSION_KEY); } catch { /* private mode */ }
  el.cad.disabled = true;
  el.fullscreen.disabled = true;
  el.disconnect.disabled = true;
  renderDisconnectButton();
  el.sendClipboard.disabled = true;
  el.requestClipboard.disabled = true;
  stopClipboardWatch();
  el.resolution.disabled = true;
  el.offerFiles.disabled = true;
  el.clearOffer.disabled = true;
  state.offered.clear();
  state.downloads.clear();
  renderOffered();
  el.transfers.replaceChildren();
  renderRemoteFiles([]);
  el.stats.textContent = 'idle';
  state.remoteCursor = null;
  renderInputTelemetry();
  setCursorDefault();
  renderHosts();
}

function disconnect() {
  state.deliberateClose = true;
  cancelReconnect();
  // The label is cleared here rather than by the close handler, because the
  // close that follows a deliberate disconnect is deliberately ignored.
  el.sessionLabel.textContent = '未连接';
  if (state.socket) {
    send({ type: 'session.close' });
    state.socket.close();
  }
  if (state.session) {
    api(`/api/sessions/${state.session.id}`, { method: 'DELETE' }).catch(() => {});
  }
  endSession();
}

// ---------------------------------------------------------------- control in

function handleControl(message) {
  switch (message.type) {
    case 'session.ready':
      state.session = { ...state.session, ...message };
      el.screen.width = message.width;
      el.screen.height = message.height;
      ctx.fillStyle = '#008080';
      ctx.fillRect(0, 0, message.width, message.height);
      // The server rounds a requested size to a mode it has, so the control
      // shows what is in effect rather than what was asked for. A size that is
      // not in the list — a synthetic desktop, or a mode nobody thought to offer
      // — is added rather than left showing a different number, because a
      // control that displays the wrong value is worse than one with an extra
      // entry.
      const size = `${message.width}x${message.height}`;
      if (![...el.resolution.options].some((o) => o.value === size)) {
        const option = document.createElement('option');
        option.value = size;
        option.textContent = `${message.width} × ${message.height}`;
        el.resolution.prepend(option);
      }
      el.resolution.value = size;
      note(`远端桌面 ${message.width}×${message.height}，协议 ${message.protocol}`);
      break;

    case 'session.stats':
      el.stats.textContent =
        `${message.fps.toFixed(0)} fps · ${message.kbps.toFixed(0)} KB/s · 质量 ${message.quality}` +
        (message.dropped ? ` · 丢弃 ${message.dropped}` : '') +
        (state.rtt !== null ? ` · ${state.rtt}ms` : '');
      renderInputTelemetry();
      break;

    case 'files.remote':
      renderRemoteFiles(message.files || []);
      break;

    case 'files.need':
      // A failure here leaves the remote's paste waiting for a range that never
      // comes, so it is reported rather than left as an unhandled rejection
      // nobody will ever see.
      answerRangeRequest(message).catch((error) => {
        noteError('file_transfer', `无法提供 ${message.name} 的数据：${error.message}`);
      });
      break;

    case 'files.begin':
      state.downloads.set(message.transferId, {
        transferId: message.transferId,
        name: message.name, size: message.size, got: 0,
        buffer: new Uint8Array(message.size),
      });
      break;

    case 'files.end': {
      const transfer = state.downloads.get(message.transferId);
      state.downloads.delete(message.transferId);
      if (!transfer) break;
      // The progress row goes away either way: a bar left at 100% reads as a
      // transfer still running, and one left on a failure reads as progress
      // that will never finish.
      clearTransferRow(transfer);
      if (message.error) {
        noteError('file_transfer', `${transfer.name}: ${message.error}`);
        break;
      }
      saveDownload(transfer);
      break;
    }

    case 'clipboard.text':
      // Remembered so the automatic push does not send the remote's own text
      // straight back to it.
      state.remoteClipboard = message.text;
      addClipboard(message.text, message.ts);
      break;

    case 'cursor.pos':
      // The server's own report of where its pointer ended up. It is the only
      // cheap confirmation that what was sent arrived.
      state.remoteCursor = [message.x, message.y];
      break;

    case 'cursor.system':
      applySystemCursor(message.systemType, message.hidden);
      break;

    case 'error':
      noteError(message.code, message.message);
      if (message.code === 'unsupported') el.capability.textContent = '部分功能未就绪';
      break;

    case 'session.closed':
      // The gateway has ended the session, so the close that follows is final:
      // retrying would just fail against a session that no longer exists.
      state.sessionEnded = true;
      note(`服务端关闭会话：${message.reason}` +
        (message.message ? `（${message.message}）` : ''));
      break;

    case 'pong':
      if (state.lastPing) {
        state.rtt = Math.round(performance.now() - state.lastPing);
      }
      break;

    default:
      note(`未知控制消息 ${message.type}`);
  }
}

// ---------------------------------------------------------------- pixels

// Frames are decoded one at a time, in the order they arrived.
//
// Decoding is asynchronous and the frames arrive faster than they decode, so
// without this the draws happen in completion order: a region of an older frame
// whose decode happened to finish later paints over a newer one, and the desktop
// shows blocks of itself in the wrong place. Larger desktops have larger frames
// and more overlap, which is why it shows up there first.
const drawQueue = [];
let drawing = false;

function handleFrame(buffer) {
  // A full frame covers the whole desktop, so anything still waiting is already
  // represented by it. Dropping the queue at that point is what stops a slow
  // decoder from replaying history, and the order of what is left is untouched.
  if (buffer.byteLength >= HEADER_LEN && (new DataView(buffer).getUint16(2, true) & 0x0001)) {
    drawQueue.length = 0;
  }
  drawQueue.push(buffer);
  if (!drawing) pumpDraw();
}

async function pumpDraw() {
  drawing = true;
  while (drawQueue.length > 0) {
    const buffer = drawQueue.shift();
    try {
      await drawFrame(buffer);
    } catch (error) {
      noteError('frame_failed', `画面绘制失败：${error.message}`);
    }
  }
  drawing = false;
}

async function drawFrame(buffer) {
  if (buffer.byteLength < HEADER_LEN) return;
  const view = new DataView(buffer);
  const type = view.getUint8(0);
  const format = view.getUint8(1);
  const flags = view.getUint16(2, true);
  const x = view.getUint16(4, true);
  const y = view.getUint16(6, true);
  const w = view.getUint16(8, true);
  const h = view.getUint16(10, true);

  if (type === TYPE_FILE) {
    receiveFileChunk(buffer);
    return;
  }
  if (type === TYPE_CURSOR) {
    applyCursorImage(buffer, x, y);
    return;
  }
  if (type !== TYPE_IMAGE) {
    return;
  }

  const payload = new Uint8Array(buffer, HEADER_LEN);
  const mime = format === FORMAT_WEBP ? 'image/webp' : 'image/jpeg';

  let bitmap;
  try {
    bitmap = await createImageBitmap(new Blob([payload], { type: mime }));
  } catch (error) {
    noteError('decode_failed', `画面解码失败：${error.message}`);
    return;
  }

  if (flags & 0x0001) {
    ctx.drawImage(bitmap, 0, 0);
  } else {
    ctx.drawImage(bitmap, x, y, w || bitmap.width, h || bitmap.height);
  }
  bitmap.close();

  state.frameCount++;
  if (state.frameCount === 1) {
    el.capability.textContent = '';
    note('收到第一帧画面。');
  }
}

// renderInputTelemetry shows what left the browser and what the remote said
// back. It exists because "the mouse does nothing" has several causes that look
// identical on screen: a browser that is not sending (the canvas has no focus,
// or something is over it), a gateway that is not forwarding, and a remote that
// is refusing input because its session is not signed in. The counters separate
// the first from the other two, and the reported position settles the rest.
function renderInputTelemetry() {
  const t = el.inputTelemetry;
  if (!t) return;
  const s = state.sent;
  t.textContent =
    `↑ 移动 ${s.move} · 点击 ${s.button} · 按键 ${s.key} · ` +
    `滚轮 ${s.wheel}刻度/${s.wheelEvents}次` +
    (state.remoteCursor ? ` · 远端光标 ${state.remoteCursor[0]},${state.remoteCursor[1]}` : ' · 远端光标 未回报') +
    (state.systemCursor === null ? '' : ` · 光标类型 0x${state.systemCursor.toString(16)}`);
}

// ---------------------------------------------------------------- files

// offerFiles tells the gateway which files the remote may paste.
function offerFiles(files) {
  const list = [...files].filter((f) => f.size > 0);
  if (list.length === 0) return;

  state.offered.clear();
  for (const f of list) state.offered.set(f.name, f);

  send({
    type: 'files.offer',
    files: list.map((f) => ({ name: f.name, size: f.size })),
  });
  renderOffered();
  note(`已分享 ${list.length} 个文件；在远端按 Ctrl+V 即可粘贴。`);
}

function renderOffered() {
  el.offered.replaceChildren();
  if (state.offered.size === 0) {
    const p = document.createElement('div');
    p.className = 'label';
    p.style.padding = '4px 6px';
    p.textContent = '把文件拖到画面上，或点「分享文件」，就能在远端粘贴。';
    el.offered.append(p);
    return;
  }
  for (const [name, file] of state.offered) {
    const row = document.createElement('div');
    row.className = 'file-row';
    const label = document.createElement('span');
    label.className = 'name';
    label.textContent = name;
    label.title = name;
    const size = document.createElement('span');
    size.className = 'where';
    size.textContent = humanSize(file.size);
    row.append(label, size);
    el.offered.append(row);
  }
}

// answerRangeRequest serves the range the remote's paste asked for.
//
// The gateway asks; the file never left this machine until now. Reading it here
// rather than uploading it up front is what lets a four gigabyte file be pasted
// without four gigabytes of memory anywhere.
async function answerRangeRequest(message) {
  const file = state.offered.get(message.name);
  if (!file) {
    noteError('file_transfer', `远端请求了未分享的文件 ${message.name}`);
    return;
  }
  const end = Math.min(message.offset + message.length, file.size);
  const slice = file.slice(message.offset, end);
  const bytes = new Uint8Array(await slice.arrayBuffer());
  sendFileChunk(message.token, message.offset, bytes);
}

// sendFileChunk builds a file frame and writes it raw: the payload is bytes, so
// it cannot go through the JSON path.
function sendFileChunk(token, offset, bytes) {
  const socket = state.socket;
  if (!socket || socket.readyState !== WebSocket.OPEN) return;

  const payload = new Uint8Array(FILE_OFFSET_LEN + bytes.length);
  new DataView(payload.buffer).setUint32(0, offset, true);
  payload.set(bytes, FILE_OFFSET_LEN);

  const frame = new Uint8Array(HEADER_LEN + payload.length);
  const view = new DataView(frame.buffer);
  view.setUint8(0, TYPE_FILE);
  view.setUint8(1, FORMAT_RAW);
  view.setUint16(2, 0, true);
  view.setUint16(4, 0, true);
  view.setUint16(6, 0, true);
  view.setUint16(8, 0, true);
  view.setUint16(10, 0, true);
  view.setUint32(12, token, true);
  view.setUint32(16, payload.length, true);
  frame.set(payload, HEADER_LEN);

  socket.send(frame);
}

// receiveFileChunk places a chunk of a download at the offset it states.
function receiveFileChunk(buffer) {
  const view = new DataView(buffer);
  const transferId = view.getUint32(12, true);
  const transfer = state.downloads.get(transferId);
  if (!transfer) return;

  const payload = new Uint8Array(buffer, HEADER_LEN);
  const offset = new DataView(payload.buffer, payload.byteOffset, FILE_OFFSET_LEN).getUint32(0, true);
  const bytes = payload.subarray(FILE_OFFSET_LEN);
  if (offset + bytes.length > transfer.buffer.length) return;

  transfer.buffer.set(bytes, offset);
  transfer.got += bytes.length;
  updateDownloadProgress(transfer);
}

function renderRemoteFiles(files) {
  el.remoteFiles.replaceChildren();
  if (files.length === 0) return;

  const head = document.createElement('div');
  head.className = 'label';
  head.style.padding = '6px 6px 2px';
  head.textContent = '远端剪贴板上的文件';
  el.remoteFiles.append(head);

  for (const file of files) {
    const row = document.createElement('div');
    row.className = 'file-row';
    const name = document.createElement('span');
    name.className = 'name';
    name.textContent = file.name;
    name.title = file.name;
    const size = document.createElement('span');
    size.className = 'where';
    size.textContent = file.isDir ? '目录' : humanSize(file.size);

    const button = document.createElement('button');
    button.className = 'act';
    button.type = 'button';
    button.textContent = file.isDir ? '—' : '下载';
    button.disabled = file.isDir;
    button.addEventListener('click', () => {
      send({ type: 'files.get', index: file.index });
      note(`开始下载 ${file.name}`);
    });

    row.append(name, size, button);
    el.remoteFiles.append(row);
  }
}

// updateDownloadProgress shows a bar for a download in flight. A 2 GiB file is
// not something an operator should have to guess about.
function updateDownloadProgress(transfer) {
  let bar = document.getElementById('dl-' + transfer.transferId);
  if (!bar) {
    bar = document.createElement('div');
    bar.id = 'dl-' + transfer.transferId;
    bar.className = 'file-row';
    bar.innerHTML = '<span class="name"></span><span class="where"></span>';
    el.transfers.append(bar);
  }
  bar.querySelector('.name').textContent = transfer.name;
  const pct = transfer.size ? Math.round((transfer.got / transfer.size) * 100) : 0;
  bar.querySelector('.where').textContent =
    `${humanSize(transfer.got)} / ${humanSize(transfer.size)} · ${pct}%`;
}

function clearTransferRow(transfer) {
  const bar = document.getElementById('dl-' + transfer.transferId);
  if (bar) bar.remove();
}

function saveDownload(transfer) {
  const blob = new Blob([transfer.buffer]);
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = transfer.name;
  link.click();
  URL.revokeObjectURL(url);
  note(`已保存 ${transfer.name}（${humanSize(transfer.size)}）`);
}

function humanSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KiB', 'MiB', 'GiB'];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++; }
  return `${value.toFixed(value < 10 ? 1 : 0)} ${units[unit]}`;
}

el.offerFiles.addEventListener('click', () => el.filePicker.click());

el.filePicker.addEventListener('change', () => {
  offerFiles(el.filePicker.files);
  el.filePicker.value = ''; // so the same file can be offered again
});

el.clearOffer.addEventListener('click', () => {
  state.offered.clear();
  send({ type: 'files.clear' });
  renderOffered();
  note('已撤销分享的文件。');
});

// Dropping files on the session shares them, which is what dropping a file on a
// remote desktop should do.
for (const name of ['dragenter', 'dragover']) {
  $('viewport').addEventListener(name, (event) => {
    if (!state.session) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = 'copy';
  });
}
$('viewport').addEventListener('drop', (event) => {
  if (!state.session) return;
  event.preventDefault();
  if (event.dataTransfer.files.length) offerFiles(event.dataTransfer.files);
});

// ---------------------------------------------------------------- pointer

// applyCursorImage installs a decoded shape as the page's pointer.
//
// The browser draws it, over the canvas, at the hotspot the frame carries. A
// blob URL rather than a data URL because a cursor can change several times a
// second and base64 would be four thirds of the bytes for no reason.
function applyCursorImage(buffer, hotspotX, hotspotY) {
  const payload = new Uint8Array(buffer, HEADER_LEN);
  const url = URL.createObjectURL(new Blob([payload], { type: 'image/png' }));
  if (state.cursorURL) URL.revokeObjectURL(state.cursorURL);
  state.cursorURL = url;
  el.screen.style.cursor = `url("${url}") ${hotspotX} ${hotspotY}, auto`;
}

// applySystemCursor handles the pointers a server names instead of drawing.
//
// applySystemCursor handles the pointers a server names instead of drawing.
//
// The whole enumeration is mapped, not just the two values the specification
// spells out. Leaving the rest to the browser's arrow made the I beam over a
// text field — the one a person meets most often — an arrow, which reads as the
// cursor being drawn wrongly rather than as a shape that was not translated.
//
// The raw value is shown in the telemetry as it arrives, because an enumeration
// transcribed from a document is exactly the sort of thing that is wrong in a
// way nobody notices until it lands on the wrong shape.
const SYSTEM_CURSORS = new Map([
  [0x00007f00, 'default'], // default arrow
  [0x00007f01, 'default'], // arrow
  [0x00007f02, 'text'], // I beam
  [0x00007f03, 'wait'],
  [0x00007f04, 'crosshair'],
  [0x00007f05, 'default'], // upward arrow
  [0x00007f06, 'nwse-resize'],
  [0x00007f07, 'nesw-resize'],
  [0x00007f08, 'ew-resize'],
  [0x00007f09, 'ns-resize'],
  [0x00007f0a, 'move'],
  [0x00007f0b, 'not-allowed'],
  [0x00007f0e, 'progress'],
  [0x00007f0f, 'help'],
]);

const SYSPTR_NULL = 0x00000000;

function applySystemCursor(systemType, hidden) {
  state.systemCursor = systemType;
  if (hidden || systemType === SYSPTR_NULL) {
    el.screen.style.cursor = 'none';
  } else {
    el.screen.style.cursor = SYSTEM_CURSORS.get(systemType) || 'default';
  }
  renderInputTelemetry();
}

function setCursorDefault() {
  if (state.cursorURL) {
    URL.revokeObjectURL(state.cursorURL);
    state.cursorURL = null;
  }
  el.screen.style.cursor = 'default';
}

// ---------------------------------------------------------------- input

// drawnRect is where the desktop actually is on screen, in page coordinates.
//
// It is measured rather than assumed. The element's rectangle and the drawn
// image are the same thing only while the stylesheet keeps them so, and a
// version of this file assumed they were: the canvas filled the viewport with
// object-fit: contain, which letterboxes the desktop inside a wider element, and
// every click was then mapped through two different scales — 0.97 across and
// 1.48 down in one window. Clicks landed hundreds of remote pixels from where
// they were aimed, which reads as the mouse not working at all.
function drawnRect() {
  const rect = el.screen.getBoundingClientRect();
  const scale = Math.min(rect.width / el.screen.width, rect.height / el.screen.height);
  const width = el.screen.width * scale;
  const height = el.screen.height * scale;
  return {
    left: rect.left + (rect.width - width) / 2,
    top: rect.top + (rect.height - height) / 2,
    scale,
  };
}

// lastPointer is where the pointer was last seen, so a button released by
// something other than a pointer event — a window losing focus — is released at
// the position the remote last knew about rather than at the origin.
const lastPointer = { x: 0, y: 0 };

function remotePoint(event) {
  const drawn = drawnRect();
  const x = Math.round((event.clientX - drawn.left) / drawn.scale);
  const y = Math.round((event.clientY - drawn.top) / drawn.scale);
  lastPointer.x = Math.max(0, Math.min(el.screen.width - 1, x));
  lastPointer.y = Math.max(0, Math.min(el.screen.height - 1, y));
  return { x: lastPointer.x, y: lastPointer.y };
}

el.screen.addEventListener('mousemove', (event) => {
  const { x, y } = remotePoint(event);
  send({ type: 'input.mouse', x, y, action: 'move' });
});

// A button that goes down on the canvas and comes up outside it must still be
// released on the remote. Without that the server believes the button is held,
// Windows enters a drag, and every later click does something other than what
// was aimed at — which reads as the mouse not working. The release is therefore
// watched on the window, and a window that loses focus releases everything,
// because whatever happened to the mouse after that cannot be observed.
const heldButtons = new Set();

el.screen.addEventListener('mousedown', (event) => {
  event.preventDefault();
  const { x, y } = remotePoint(event);
  heldButtons.add(event.button);
  send({ type: 'input.mouse', x, y, action: 'down', button: event.button });
});

window.addEventListener('mouseup', (event) => {
  if (!state.session) return;
  const { x, y } = remotePoint(event);
  const wasHeld = heldButtons.delete(event.button);
  if (!wasHeld && !event.buttons) {
    // A release for something this page never pressed, and nothing else is down
    // either: sending it anyway would be harmless but pointless.
    return;
  }
  send({ type: 'input.mouse', x, y, action: 'up', button: event.button });
});

// Dragging continues outside the canvas, which is what a selection or a window
// move needs.
window.addEventListener('mousemove', (event) => {
  if (heldButtons.size === 0) return;
  const { x, y } = remotePoint(event);
  send({ type: 'input.mouse', x, y, action: 'move' });
});

window.addEventListener('blur', () => {
  releaseHeldButtons();
  releaseHeldKeys();
});

window.addEventListener('focus', () => {
  pushLocalClipboard();
});

// A clipboard read is only allowed while the page is focused, so the moment it
// becomes visible again is the first chance to see what changed while it was
// hidden.
document.addEventListener('visibilitychange', () => {
  if (!document.hidden) pushLocalClipboard();
});

// The page going away is the other way a held button is never released.
document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    releaseHeldButtons();
    releaseHeldKeys();
  }
});

function releaseHeldButtons() {
  if (heldButtons.size === 0) return;
  for (const button of heldButtons) {
    send({ type: 'input.mouse', x: lastPointer.x, y: lastPointer.y, action: 'up', button });
  }
  heldButtons.clear();
}

el.screen.addEventListener('contextmenu', (event) => event.preventDefault());

// Wheel notches accumulate, because a trackpad reports many small deltas and the
// protocol counts whole notches.
//
// What one notch measures depends on deltaMode: pixels (a wheel notch is 100, or
// 120 on some drivers), lines, or pages. Dividing a line-mode delta of 3 by 100
// leaves a fraction that never reaches a notch, so the wheel does nothing at all;
// normalising per mode is what keeps all three working.
let wheelAccumulator = 0;
const wheelPerNotch = (mode) => {
  if (mode === 1) return 3; // lines
  if (mode === 2) return 1; // pages
  return 100; // pixels
};
el.screen.addEventListener('wheel', (event) => {
  event.preventDefault();
  const { x, y } = remotePoint(event);
  // Counted separately from the notches, because the two together are what says
  // whether a direction reports smaller deltas than the other. One event per
  // notch in both directions means the deltas are symmetrical and the
  // asymmetry is further down.
  state.sent.wheelEvents++;
  wheelAccumulator += -event.deltaY / wheelPerNotch(event.deltaMode);
  const notches = Math.trunc(wheelAccumulator);
  if (notches === 0) return;
  wheelAccumulator -= notches;
  send({ type: 'input.wheel', x, y, delta: notches });
}, { passive: false });

// Keys are taken from the document rather than from a focused text field.
//
// A text field is what the local input method attaches to, so having one focused
// meant the local IME composed the keystrokes before the remote ever saw them: a
// remote desktop wants the remote's own IME to do that, and the keys to arrive
// as keys. With nothing editable focused there is no composition session to
// interfere, and the scancodes go through untouched.
//
// The codes that are currently down are remembered so they can be released. A
// key held while the page loses focus never gets a keyup — pressing the Windows
// key opens the local Start menu and the page is gone before the release — and a
// remote that is never told lets go believes the key is still held. The remote
// then stays wedged with that key down, which is how Win+R ends up opening Run
// on the remote from a key the user let go of.
const heldKeys = new Set();

// typingIntoUI reports whether the keystroke belongs to a control on this page
// rather than to the remote. Without it, typing a host name or a clipboard
// string would also be sent to the remote.
function typingIntoUI(event) {
  const target = event.target;
  if (!target || !target.tagName) return false;
  const tag = target.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || target.isContentEditable;
}

document.addEventListener('keydown', (event) => {
  if (!state.session) return;
  if (typingIntoUI(event)) return;
  if (event.isComposing || event.keyCode === 229) return;
  if (!event.code || event.code === 'Unidentified') return;
  if (browserShortcut(event)) return;
  event.preventDefault();
  heldKeys.add(event.code);
  send({ type: 'input.key', code: event.code, down: true });
});

document.addEventListener('keyup', (event) => {
  if (!state.session) return;
  if (typingIntoUI(event)) return;
  if (event.isComposing || event.keyCode === 229) return;
  if (!event.code || event.code === 'Unidentified') return;
  if (browserShortcut(event)) return;
  event.preventDefault();
  heldKeys.delete(event.code);
  send({ type: 'input.key', code: event.code, down: false });
});

// releaseHeldKeys lets go of everything on the remote that this page believes is
// still down. Sending the release twice is harmless; leaving it unsent is not.
function releaseHeldKeys() {
  if (heldKeys.size === 0) return;
  for (const code of heldKeys) {
    send({ type: 'input.key', code, down: false });
  }
  heldKeys.clear();
}

function browserShortcut(event) {
  // Leave the keys the browser needs. Everything else goes to the remote.
  if (event.key === 'F5' || event.key === 'F11' || event.key === 'F12') return true;
  if (event.ctrlKey && event.shiftKey && ['I', 'J', 'C'].includes(event.key.toUpperCase())) return true;
  if (event.ctrlKey && event.key === 'w') return true;
  return false;
}

// The same button disconnects and connects.
//
// When there is no session it is the only way forward, so it says so and is
// marked as the primary action rather than sitting disabled under a label that
// describes something that cannot happen.
function renderDisconnectButton() {
  const live = Boolean(state.session);
  el.disconnect.textContent = live ? '断开' : '连接';
  el.disconnect.classList.toggle('primary', !live);
  el.disconnect.disabled = false;
}

// connectOffer is what the button does when nothing is connected: the first host
// that has been saved, or the form to make one.
function connectOffer() {
  if (state.hosts.length > 0) {
    connect(state.hosts[0]);
    return;
  }
  openHostForm();
}

el.disconnect.addEventListener('click', () => {
  if (state.session) disconnect();
  else connectOffer();
});

el.cad.addEventListener('click', () => send({ type: 'session.cad' }));

// Immersive, not the Fullscreen API. The browser keeps Esc for leaving real
// fullscreen and a page cannot take it back, so a remote desktop built on it
// cannot receive the one key every desktop uses to cancel. Filling the window
// from CSS costs the browser's chrome and keeps Esc.
el.fullscreen.addEventListener('click', () => setImmersive(true));
$('immersiveExit').addEventListener('click', () => setImmersive(false));

function setImmersive(on) {
  document.body.classList.toggle('immersive', on);
  $('immersiveExit').hidden = !on;
  el.fullscreen.textContent = on ? '退出全屏' : '全屏';
}

el.resolution.addEventListener('change', () => {
  const [width, height] = el.resolution.value.split('x').map(Number);
  if (!send({ type: 'session.resize', width, height })) {
    note('会话已断开，无法改变分辨率。', 'error');
    return;
  }
  note(`已请求 ${width}×${height}；服务器可能就近取一个它支持的尺寸。`);
});

el.quality.addEventListener('input', () => {
  const level = Number(el.quality.value);
  el.qualityValue.textContent = String(level);
  send({ type: 'session.quality', level });
});

// ---------------------------------------------------------------- clipboard

function addClipboard(text, ts) {
  if (!text) return;
  if (state.clipboard.length && state.clipboard[0].text === text) return;

  state.clipboard.unshift({ text, ts: ts || Date.now() });
  state.clipboard.length = Math.min(state.clipboard.length, CLIPBOARD_LIMIT);
  renderClipboard();
}

function renderClipboard() {
  el.clipboard.replaceChildren();
  if (state.clipboard.length === 0) {
    const p = document.createElement('div');
    p.className = 'label';
    p.textContent = '远端复制的内容会出现在这里，由你决定要不要放进本机剪贴板。';
    el.clipboard.append(p);
    return;
  }
  for (const entry of state.clipboard) {
    const box = document.createElement('div');
    box.className = 'clip-item';

    const text = document.createElement('div');
    text.className = 'text';
    text.textContent = entry.text;

    const meta = document.createElement('div');
    meta.className = 'meta';
    const when = document.createElement('span');
    when.textContent = new Date(entry.ts).toLocaleTimeString();
    const grow = document.createElement('span');
    grow.className = 'grow';
    const copy = document.createElement('button');
    copy.className = 'act';
    copy.type = 'button';
    copy.textContent = '复制到本机';
    copy.addEventListener('click', async () => {
      // Writing the local clipboard needs a user gesture, which is why it
      // happens here and not when the text arrived.
      try {
        await navigator.clipboard.writeText(entry.text);
        copy.textContent = '已复制';
        setTimeout(() => { copy.textContent = '复制到本机'; }, 1200);
      } catch (error) {
        noteError('clipboard_denied', `无法写入本机剪贴板：${error.message}`);
      }
    });
    meta.append(when, grow, copy);

    box.append(text, meta);
    el.clipboard.append(box);
  }
}

el.clearClipboard.addEventListener('click', () => {
  state.clipboard = [];
  renderClipboard();
});

document.addEventListener('paste', (event) => {
  if (!state.session) return;
  const text = event.clipboardData && event.clipboardData.getData('text');
  if (!text) return;
  send({ type: 'clipboard.set', text });
  note('已发送本机剪贴板到远端。');
});

el.sendClipboard.addEventListener('click', () => {
  const text = el.clipboardOut.value;
  if (!text) return;
  send({ type: 'clipboard.set', text });
  el.clipboardOut.value = '';
});

// The local clipboard reaches the remote on its own. The remote's does not
// reach the local one, which stays a decision the operator makes by clicking an
// entry: a busy remote overwriting what is being copied locally is the thing
// that makes shared clipboards unusable.
//
// Reading the local clipboard needs the page to be focused, so this runs on
// focus and on a slow timer rather than on a clipboard event, which browsers do
// not fire for a page that is not focused anyway.
const CLIPBOARD_POLL = 1500;
let clipboardTimer = null;
let clipboardDenied = false;

async function pushLocalClipboard() {
  if (!state.session || clipboardDenied) return;
  if (typeof navigator.clipboard?.readText !== 'function') return;
  let text;
  try {
    text = await navigator.clipboard.readText();
  } catch {
    // Refused, or the page is not focused. Both are ordinary, and asking again
    // every 1.5 seconds would fill the console with the same refusal.
    clipboardDenied = true;
    return;
  }
  if (!text || text === state.localClipboard) return;
  state.localClipboard = text;
  // The remote sent this one, so sending it back would have the two sides
  // copying each other's text into a loop.
  if (text === state.remoteClipboard) return;
  send({ type: 'clipboard.set', text });
}

function startClipboardWatch() {
  if (clipboardTimer !== null) return;
  clipboardTimer = setInterval(pushLocalClipboard, CLIPBOARD_POLL);
  pushLocalClipboard();
}

function stopClipboardWatch() {
  if (clipboardTimer === null) return;
  clearInterval(clipboardTimer);
  clipboardTimer = null;
}

el.requestClipboard.addEventListener('click', () => {
  send({ type: 'clipboard.request' });
});

// ---------------------------------------------------------------- sign in

el.loginForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  el.loginButton.disabled = true;
  el.loginStatus.textContent = '验证中…';
  try {
    await api('/api/login', { method: 'POST', body: JSON.stringify({ password: el.password.value }) });
    state.unauthorized = false;
    el.password.value = '';
    el.loginStatus.textContent = '';
    showWorkspace();
  } catch (error) {
    if (error.code === 'login_locked') {
      const retry = error.body.retryAfter || 0;
      el.loginStatus.textContent = `尝试次数过多，请等 ${Math.ceil(retry / 60)} 分钟后再试。`;
    } else {
      const left = error.body.attemptsLeft;
      el.loginStatus.textContent = left === undefined
        ? error.message
        : `密码错误，还能试 ${left} 次。`;
    }
  } finally {
    el.loginButton.disabled = false;
  }
});

el.signOut.addEventListener('click', async () => {
  disconnect();
  try { await api('/api/logout', { method: 'POST' }); } catch { /* leaving anyway */ }
  showLogin();
});

function showWorkspace() {
  el.login.hidden = true;
  el.workspace.hidden = false;
  loadHosts();
  renderDisconnectButton();
  renderHosts();
  renderClipboard();
  renderOffered();
  resumeSession();
}

function showLogin() {
  el.workspace.hidden = true;
  el.login.hidden = false;
  el.password.focus();
}

// A reload keeps the cookie, so the console opens straight away; asking for the
// password again when the session is still valid would be noise.
async function start() {
  el.qualityValue.textContent = el.quality.value;

  try {
    await api('/api/sessions');
    showWorkspace();
  } catch {
    showLogin();
  }
}

// resumeSession puts back the desktop this tab was showing before it reloaded.
//
// It does not delete anything. An earlier version cleaned up sessions with no
// browser attached on every page load, which was right when a dropped socket
// ended the session — and is exactly wrong now: the session waiting for this
// page to come back is detached, and deleting it is the one thing that makes a
// reload lose the desktop. Abandoned sessions are reaped by the gateway, first
// by the reconnect grace and then by the idle timeout.
async function resumeSession() {
  let id = null;
  try { id = sessionStorage.getItem(SESSION_KEY); } catch { /* private mode */ }
  if (!id) return false;

  let mine;
  let list;
  try {
    list = await api('/api/sessions');
    mine = (list.sessions || []).find((s) => s.id === id);
  } catch {
    return false; // not signed in yet, or the gateway went away
  }

  if (!mine) {
    try { sessionStorage.removeItem(SESSION_KEY); } catch { /* private mode */ }
    return false;
  }

  state.session = { id: mine.id, name: mine.name, width: mine.width, height: mine.height };
  // The create response carried this, and a reload never saw it. Without it the
  // first drop after a reload would take the "no grace" path and end the very
  // session the gateway is still holding.
  state.graceMs = Math.max(0, list.reconnectGraceMs || 0);
  if (state.graceMs <= 0) {
    // The gateway will not hold this session, so resuming it is theatre: the
    // next drop would end it anyway.
    note('网关不会保留断开的会话，恢复后一旦断开就会结束。');
  }
  el.sessionLabel.textContent = `正在恢复 ${mine.name} …`;
  note(`页面重新载入，正在恢复 ${mine.name} 的画面。`);
  openSocket(id, true);
  return true;
}

start();
