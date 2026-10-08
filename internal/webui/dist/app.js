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

// ---------------------------------------------------------------- state

const state = {
  hosts: [],
  selectedHostId: null,
  editingHostId: null,
  session: null, // { id, name, width, height }
  socket: null,
  clipboard: [],
  frameCount: 0,

  // Files the operator offered, by name, so a range request can be answered.
  offered: new Map(),
  // Downloads in progress, by transfer id.
  downloads: new Map(),
  pingTimer: null,
  lastPing: 0,
  rtt: null,
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
  keySink: $('keySink'),
  stats: $('stats'),
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
    throw error;
  }
  return body || {};
}

function send(control) {
  if (!state.socket || state.socket.readyState !== WebSocket.OPEN) return false;
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
  el.screen.width = created.width;
  el.screen.height = created.height;
  ctx.fillStyle = '#008080';
  ctx.fillRect(0, 0, created.width, created.height);

  openSocket(created.id);
}

function openSocket(id) {
  const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
  const socket = new WebSocket(`${scheme}://${location.host}/api/sessions/${id}/stream`);
  socket.binaryType = 'arraybuffer';
  state.socket = socket;

  socket.addEventListener('open', () => {
    el.sessionLabel.textContent = `已连接 ${state.session.name}`;
    el.cad.disabled = false;
    el.fullscreen.disabled = false;
    el.disconnect.disabled = false;
    el.sendClipboard.disabled = false;
    el.requestClipboard.disabled = false;
    el.resolution.disabled = false;
    el.offerFiles.disabled = false;
    el.clearOffer.disabled = false;
    el.keySink.focus();
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
    const reason = event.reason ? `：${event.reason}` : '';
    el.sessionLabel.textContent = `连接已结束 (${event.code})${reason}`;
    note(`会话结束，代码 ${event.code}${reason}`);
    endSession();
  });

  socket.addEventListener('error', () => {
    noteError('socket_error', 'WebSocket 出错，连接可能已断开。');
  });
}

function stopPing() {
  if (state.pingTimer) { clearInterval(state.pingTimer); state.pingTimer = null; }
}

function endSession() {
  stopPing();
  state.socket = null;
  state.session = null;
  state.rtt = null;
  el.cad.disabled = true;
  el.fullscreen.disabled = true;
  el.disconnect.disabled = true;
  el.sendClipboard.disabled = true;
  el.requestClipboard.disabled = true;
  el.resolution.disabled = true;
  el.offerFiles.disabled = true;
  el.clearOffer.disabled = true;
  state.offered.clear();
  state.downloads.clear();
  renderOffered();
  el.transfers.replaceChildren();
  renderRemoteFiles([]);
  el.stats.textContent = 'idle';
  setCursorDefault();
  renderHosts();
}

function disconnect() {
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
      addClipboard(message.text, message.ts);
      break;

    case 'cursor.pos':
      break;

    case 'cursor.system':
      applySystemCursor(message.systemType, message.hidden);
      break;

    case 'error':
      noteError(message.code, message.message);
      if (message.code === 'unsupported') el.capability.textContent = '部分功能未就绪';
      break;

    case 'session.closed':
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

async function handleFrame(buffer) {
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
// Only the two values the specification names are mapped. The rest of the
// enumeration is left to the browser's arrow: showing an arrow where an I beam
// belongs is a small wrong, and showing the wrong shape where a resize handle
// belongs is a confusing one, so the gateway does not guess at values it has not
// verified against a server.
const SYSPTR_NULL = 0x00000000;
const SYSPTR_DEFAULT = 0x00007f00;

function applySystemCursor(systemType, hidden) {
  if (hidden || systemType === SYSPTR_NULL) {
    el.screen.style.cursor = 'none';
    return;
  }
  el.screen.style.cursor = systemType === SYSPTR_DEFAULT ? 'default' : 'default';
}

function setCursorDefault() {
  if (state.cursorURL) {
    URL.revokeObjectURL(state.cursorURL);
    state.cursorURL = null;
  }
  el.screen.style.cursor = 'default';
}

// ---------------------------------------------------------------- input

function remotePoint(event) {
  const rect = el.screen.getBoundingClientRect();
  const scaleX = el.screen.width / rect.width;
  const scaleY = el.screen.height / rect.height;
  const x = Math.round((event.clientX - rect.left) * scaleX);
  const y = Math.round((event.clientY - rect.top) * scaleY);
  return {
    x: Math.max(0, Math.min(el.screen.width - 1, x)),
    y: Math.max(0, Math.min(el.screen.height - 1, y)),
  };
}

el.screen.addEventListener('mousemove', (event) => {
  const { x, y } = remotePoint(event);
  send({ type: 'input.mouse', x, y, action: 'move' });
});

for (const [name, button] of [['mousedown', 'down'], ['mouseup', 'up']]) {
  el.screen.addEventListener(name, (event) => {
    event.preventDefault();
    el.keySink.focus();
    const { x, y } = remotePoint(event);
    send({ type: 'input.mouse', x, y, action: button, button: event.button });
  });
}

el.screen.addEventListener('contextmenu', (event) => event.preventDefault());

// Wheel notches accumulate: a trackpad reports many small deltas, and the
// protocol counts whole notches.
let wheelAccumulator = 0;
el.screen.addEventListener('wheel', (event) => {
  event.preventDefault();
  const { x, y } = remotePoint(event);
  wheelAccumulator += -event.deltaY / 100;
  const notches = Math.trunc(wheelAccumulator);
  if (notches === 0) return;
  wheelAccumulator -= notches;
  send({ type: 'input.wheel', x, y, delta: notches });
}, { passive: false });

// Keys go through the invisible input, so both plain keys and input method
// compositions land in one place.
el.keySink.addEventListener('keydown', (event) => {
  if (event.isComposing || event.keyCode === 229) return; // mid-composition
  if (!event.code || event.code === 'Unidentified') return;
  if (browserShortcut(event)) return;
  event.preventDefault();
  send({ type: 'input.key', code: event.code, down: true });
});

el.keySink.addEventListener('keyup', (event) => {
  if (event.isComposing || event.keyCode === 229) return;
  if (!event.code || event.code === 'Unidentified') return;
  if (browserShortcut(event)) return;
  event.preventDefault();
  send({ type: 'input.key', code: event.code, down: false });
});

// Intermediate composition state is local UI and is deliberately not sent; a
// scancode cannot carry it and the remote would receive the pinyin.
el.keySink.addEventListener('compositionend', (event) => {
  if (event.data) send({ type: 'input.text', text: event.data });
  el.keySink.value = '';
});

el.keySink.addEventListener('input', (event) => {
  // Characters that arrived without a composition: an emoji picker, or a
  // layout where a scancode would be wrong.
  const text = el.keySink.value;
  if (!text) return;
  send({ type: 'input.text', text });
  el.keySink.value = '';
});

el.keySink.addEventListener('blur', () => {
  if (state.session) el.keySink.focus(); // keyboard capture stays with the remote
});

function browserShortcut(event) {
  // Leave the keys the browser needs. Everything else goes to the remote.
  if (event.key === 'F5' || event.key === 'F11' || event.key === 'F12') return true;
  if (event.ctrlKey && event.shiftKey && ['I', 'J', 'C'].includes(event.key.toUpperCase())) return true;
  if (event.ctrlKey && event.key === 'w') return true;
  return false;
}

el.cad.addEventListener('click', () => send({ type: 'session.cad' }));

el.fullscreen.addEventListener('click', () => {
  if (document.fullscreenElement) document.exitFullscreen();
  else $('viewport').requestFullscreen();
});

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
  renderHosts();
  renderClipboard();
  renderOffered();
  refreshSessions();
}

function showLogin() {
  el.workspace.hidden = true;
  el.login.hidden = false;
  el.password.focus();
}

// A reload keeps the cookie, so the console opens straight away; asking for the
// password again when the session is still valid would be noise.
async function start() {
  document.addEventListener('fullscreenchange', () => {
    el.fullscreen.textContent = document.fullscreenElement ? '退出全屏' : '全屏';
  });
  el.qualityValue.textContent = el.quality.value;

  try {
    await api('/api/sessions');
    showWorkspace();
  } catch {
    showLogin();
  }
}

async function refreshSessions() {
  try {
    const { sessions } = await api('/api/sessions');
    // A session left behind by a reload would hold a remote desktop open.
    for (const stale of sessions || []) {
      if (!stale.attached) {
        api(`/api/sessions/${stale.id}`, { method: 'DELETE' }).catch(() => {});
      }
    }
  } catch { /* not signed in yet, or the gateway went away */ }
}

start();
