'use strict';

/* AD_EXPLORER localisation.
 *
 * English is the default because this is meant to be readable by whoever finds
 * it, and a Chinese-only console is not. Chinese is kept because the operator
 * who asked for this gateway reads it.
 *
 * There is no build step and no module loader, so this is a classic script that
 * publishes one global and app.js uses it. The dictionary is the single place a
 * sentence lives: index.html marks its static text with data-i18n attributes and
 * app.js calls t() for everything it builds itself, so no string is in two
 * languages in two files.
 *
 * Keys are named by where the text appears rather than by what it says, because
 * a key named after its English wording is a key that has to be renamed the
 * first time the wording changes.
 */

const ADX_I18N = (function () {
  const STORAGE_KEY = 'adx_lang';

  // Ordered as they appear in the picker. The first is what a browser with no
  // stored choice gets, which is English.
  const LANGS = [
    { id: 'en', label: 'English' },
    { id: 'zh-CN', label: '中文' },
  ];
  const DEFAULT_LANG = 'en';

  const TEXT = {
    // ---- title bar
    'app.titlebar': { en: 'AD_EXPLORER — Remote Session', 'zh-CN': 'AD_EXPLORER — 远程会话' },
    'app.minimise': { en: 'Minimise (not applicable)', 'zh-CN': '最小化（不适用）' },
    'app.maximise': { en: 'Maximise (not applicable)', 'zh-CN': '最大化（不适用）' },
    'app.signOut': { en: 'Sign out of the console', 'zh-CN': '退出控制台' },
    'app.language': { en: 'Language', 'zh-CN': '语言' },

    // ---- sign in
    'login.title': { en: 'Console locked', 'zh-CN': '控制台已锁定' },
    'login.hint': {
      en: 'Enter the console password to continue. This is the password for the gateway itself, not for a remote host.',
      'zh-CN': '输入控制台密码以继续。这是访问网关本身的密码，不是远端主机的密码。',
    },
    'login.placeholder': { en: 'Console password', 'zh-CN': '控制台密码' },
    'login.submit': { en: 'Enter', 'zh-CN': '进入' },
    'login.verifying': { en: 'Checking…', 'zh-CN': '验证中…' },
    'login.tooMany': {
      en: 'Too many attempts. Try again in {minutes} min.',
      'zh-CN': '尝试次数过多，请等 {minutes} 分钟后再试。',
    },
    'login.wrong': { en: 'Wrong password, {left} left.', 'zh-CN': '密码错误，还能试 {left} 次。' },

    // ---- hosts
    'hosts.heading': { en: 'Hosts', 'zh-CN': '主机' },
    'hosts.new': { en: 'New', 'zh-CN': '新建' },
    'hosts.empty': {
      en: 'No hosts yet. Choose New to add one; the credentials stay in this browser only.',
      'zh-CN': '还没有主机。点「新建」添加一台，凭据只保存在这台浏览器里。',
    },
    'hosts.unreadable': {
      en: 'The host list could not be parsed and was ignored.',
      'zh-CN': '主机列表无法解析，已忽略。',
    },
    'hosts.noAccount': { en: 'no account', 'zh-CN': '未填账号' },

    // ---- host form
    'host.name': { en: 'Name', 'zh-CN': '名称' },
    'host.address': { en: 'Address', 'zh-CN': '地址' },
    'host.addressRequired': { en: 'The address cannot be empty.', 'zh-CN': '地址不能为空。' },
    'host.port': { en: 'Port', 'zh-CN': '端口' },
    'host.account': { en: 'Account', 'zh-CN': '账号' },
    'host.accountPlaceholder': { en: 'DOMAIN\\user or user', 'zh-CN': 'DOMAIN\\user 或 user' },
    'host.password': { en: 'Password', 'zh-CN': '密码' },
    'host.protocol': { en: 'Protocol', 'zh-CN': '协议' },
    'host.protocolNla': { en: 'nla (recommended, Windows)', 'zh-CN': 'nla（推荐，Windows）' },
    'host.protocolTls': { en: 'tls', 'zh-CN': 'tls' },
    'host.protocolRdp': { en: 'rdp (no TLS)', 'zh-CN': 'rdp（无 TLS）' },
    'host.resolution': { en: 'Resolution', 'zh-CN': '分辨率' },
    'host.sizeCustom': { en: 'Custom…', 'zh-CN': '自定义…' },
    'host.width': { en: 'width', 'zh-CN': '宽' },
    'host.height': { en: 'height', 'zh-CN': '高' },
    'host.connect': { en: 'Connect', 'zh-CN': '连接' },
    'host.cancel': { en: 'Cancel', 'zh-CN': '取消' },
    'host.delete': { en: 'Delete', 'zh-CN': '删除' },
    'host.sizeOutOfRange': {
      en: 'A custom resolution has to be between 200 and 8192.',
      'zh-CN': '自定义分辨率要在 200 到 8192 之间。',
    },

    // ---- session
    'session.notConnected': { en: 'Not connected', 'zh-CN': '未连接' },
    'session.connecting': { en: 'Connecting to {name} …', 'zh-CN': '连接 {name} …' },
    'session.connected': { en: 'Connected to {name}', 'zh-CN': '已连接 {name}' },
    'session.connect': { en: 'Connect', 'zh-CN': '连接' },
    'session.disconnect': { en: 'Disconnect', 'zh-CN': '断开' },
    'session.fullscreen': { en: 'Full screen', 'zh-CN': '全屏' },
    'session.exitFullscreen': { en: 'Leave full screen', 'zh-CN': '退出全屏' },
    'session.reconnected': { en: 'Reconnected.', 'zh-CN': '已重新连接。' },
    'session.endedGone': {
      en: 'Connection ended: the session no longer exists',
      'zh-CN': '连接已结束：会话已不存在',
    },
    'session.endedGoneNote': {
      en: 'The session has ended; connect to the host again.',
      'zh-CN': '会话已经结束；请重新连接主机。',
    },
    'session.endedBusy': {
      en: 'Connection ended: the session is in use',
      'zh-CN': '连接已结束：会话被占用',
    },
    'session.endedBusyNote': {
      en: 'Another browser held this session for the whole reconnect. One session shows one picture; close the other tab and connect again.',
      'zh-CN': '重连期间这个会话一直被另一个浏览器占用。一个会话只能有一个画面；请关掉另一个标签页后重新连接。',
    },
    'session.endedCode': { en: 'Connection ended ({code}){reason}', 'zh-CN': '连接已结束 ({code}){reason}' },
    'session.endedNote': { en: 'Session ended, code {code}{reason}', 'zh-CN': '会话结束，代码 {code}{reason}' },
    'session.dropped': { en: 'Disconnected; the session has ended', 'zh-CN': '连接已断开，会话已结束' },
    'session.droppedNoGrace': {
      en: 'The connection dropped and the gateway does not keep sessions; connect to the host again.',
      'zh-CN': '连接已断开，且网关不会保留会话；请重新连接主机。',
    },
    'session.reconnectFailed': {
      en: 'Reconnecting failed and the session has ended; connect to the host again.',
      'zh-CN': '重连失败，会话已结束；请重新连接主机。',
    },
    'session.reconnecting': {
      en: 'Connection lost, reconnecting …',
      'zh-CN': '连接断开，正在尝试重新连接 …',
    },
    'session.socketError': {
      en: 'The WebSocket failed; the connection may be down.',
      'zh-CN': 'WebSocket 出错，连接可能已断开。',
    },
    'session.serverClosed': { en: 'The server closed the session: {reason}', 'zh-CN': '服务端关闭会话：{reason}' },
    'session.noGrace': {
      en: 'The gateway does not keep a disconnected session; once it drops, it ends.',
      'zh-CN': '网关不会保留断开的会话，恢复后一旦断开就会结束。',
    },
    'session.restoring': { en: 'Restoring {name} …', 'zh-CN': '正在恢复 {name} …' },
    'session.restoringNote': {
      en: 'The page was reloaded; restoring the screen for {name}.',
      'zh-CN': '页面重新载入，正在恢复 {name} 的画面。',
    },
    'session.remoteDesktop': {
      en: 'Remote desktop {width}×{height}, protocol {protocol}',
      'zh-CN': '远端桌面 {width}×{height}，协议 {protocol}',
    },
    'session.expired': {
      en: 'The console login has expired; enter the password again.',
      'zh-CN': '控制台登录已过期，请重新输入口令。',
    },

    // ---- resolution
    'resolution.label': { en: 'Resolution', 'zh-CN': '分辨率' },
    'resolution.ended': {
      en: 'The session has ended, so the resolution cannot be changed.',
      'zh-CN': '会话已断开，无法改变分辨率。',
    },
    'resolution.requested': {
      en: 'Asked for {width}×{height}; the server may settle on the nearest size it supports.',
      'zh-CN': '已请求 {width}×{height}；服务器可能就近取一个它支持的尺寸。',
    },

    // ---- readouts
    'stats.idle': { en: 'idle', 'zh-CN': '空闲' },
    'stats.line': { en: '{fps} fps · {kbps} KB/s · quality {quality}', 'zh-CN': '{fps} fps · {kbps} KB/s · 质量 {quality}' },
    'stats.dropped': { en: ' · dropped {n}', 'zh-CN': ' · 丢弃 {n}' },
    'quality.label': { en: 'Quality', 'zh-CN': '质量' },
    'input.hint': { en: 'Keyboard goes straight to the remote', 'zh-CN': '键盘已直通远端' },
    'capability.partial': { en: 'Some features are not ready', 'zh-CN': '部分功能未就绪' },
    'compositor.webgl': {
      en: 'First frame received; composing with WebGL.',
      'zh-CN': '收到第一帧画面；合成走 WebGL。',
    },
    'compositor.canvas2d': {
      en: 'First frame received; composing with a 2D canvas ({why}).',
      'zh-CN': '收到第一帧画面；合成走 2D 画布（{why}）。',
    },
    'gl.noContext': { en: 'this browser did not provide a WebGL context', 'zh-CN': '这个浏览器没有提供 WebGL 上下文' },
    'gl.shaderCompile': { en: 'shader failed to compile: {log}', 'zh-CN': '着色器编译失败：{log}' },
    'gl.shaderMissing': { en: 'the shader produced nothing', 'zh-CN': '着色器没有编译出来' },
    'gl.programLink': { en: 'shader program failed to link: {log}', 'zh-CN': '着色器程序链接失败：{log}' },
    'gl.fboUnavailable': { en: 'offscreen buffer unavailable (0x{status})', 'zh-CN': '离屏缓冲不可用（0x{status}）' },
    'gl.forcedSoftware': {
      en: '?compositor=canvas2d asked for the software path',
      'zh-CN': '被 ?compositor=canvas2d 要求使用软件路径',
    },
    'gl.unknownReason': { en: 'unknown reason', 'zh-CN': '未知原因' },

    // ---- telemetry line
    'telemetry.move': { en: 'moves {n}', 'zh-CN': '移动 {n}' },
    'telemetry.click': { en: 'clicks {n}', 'zh-CN': '点击 {n}' },
    'telemetry.key': { en: 'keys {n}', 'zh-CN': '按键 {n}' },
    'telemetry.wheel': { en: 'wheel {ticks} ticks/{events} events', 'zh-CN': '滚轮 {ticks}刻度/{events}次' },
    'telemetry.remoteCursor': { en: 'remote pointer {x},{y}', 'zh-CN': '远端光标 {x},{y}' },
    'telemetry.noRemoteCursor': { en: 'remote pointer not reported', 'zh-CN': '远端光标 未回报' },
    'telemetry.cursorType': { en: 'cursor type 0x{hex}', 'zh-CN': '光标类型 0x{hex}' },
    'telemetry.frames': { en: 'frames {n}', 'zh-CN': '帧 {n}' },
    'telemetry.batches': { en: 'redraws {n}/{pieces} strips · shown {shown}', 'zh-CN': '批 {n}/{pieces}片 · 呈现 {shown}' },
    'telemetry.batchGap': { en: 'redraw gap {ms}ms (min {min})', 'zh-CN': '批间隔 {ms}ms(最小{min})' },
    'telemetry.decode': { en: 'decode {ms}ms', 'zh-CN': '解码 {ms}ms' },
    'telemetry.compose': { en: 'compose {ms}ms', 'zh-CN': '合成 {ms}ms' },
    'telemetry.batchCost': { en: 'redraw cost {ms}ms (peak {peak})', 'zh-CN': '批成本 {ms}ms(峰{peak})' },
    'telemetry.presentCost': { en: 'show cost {ms}ms (peak {peak})', 'zh-CN': '呈现耗时 {ms}ms(峰{peak})' },
    'telemetry.moving': { en: 'changing', 'zh-CN': '变化中' },
    'telemetry.still': { en: 'still', 'zh-CN': '静止' },
    'telemetry.gaps': { en: 'missing frames {n}', 'zh-CN': '丢帧 {n}' },

    // ---- errors from the wire
    'frame.drawFailed': { en: 'Drawing the screen failed: {error}', 'zh-CN': '画面绘制失败：{error}' },
    'frame.unknownFormat': {
      en: 'A frame arrived in a format that is not recognised (0x{format}).',
      'zh-CN': '画面使用了无法识别的格式 0x{format}。',
    },
    'frame.decodeFailed': { en: 'Decoding a frame failed: {error}', 'zh-CN': '画面解码失败：{error}' },
    'control.unknown': { en: 'Unknown control message {type}', 'zh-CN': '未知控制消息 {type}' },

    // ---- files
    'files.heading': { en: 'Files', 'zh-CN': '文件' },
    'files.share': { en: 'Share files', 'zh-CN': '分享文件' },
    'files.withdraw': { en: 'Withdraw', 'zh-CN': '撤销' },
    'files.dropHint': {
      en: 'Drop files on the screen, or choose Share files, and they can be pasted on the remote.',
      'zh-CN': '把文件拖到画面上，或点「分享文件」，就能在远端粘贴。',
    },
    'files.shared': {
      en: 'Sharing {n} file(s); press Ctrl+V on the remote to paste.',
      'zh-CN': '已分享 {n} 个文件；在远端按 Ctrl+V 即可粘贴。',
    },
    'files.sharedFromClipboard': {
      en: 'Shared {n} file(s) from the clipboard; press Ctrl+V on the remote to paste.',
      'zh-CN': '已从剪贴板分享 {n} 个文件；在远端按 Ctrl+V 即可粘贴。',
    },
    'files.withdrawn': { en: 'The shared files were withdrawn.', 'zh-CN': '已撤销分享的文件。' },
    'files.notShared': {
      en: 'The remote asked for {name}, which is not shared.',
      'zh-CN': '远端请求了未分享的文件 {name}',
    },
    'files.readFailed': {
      en: 'Could not supply the data for {name}: {error}',
      'zh-CN': '无法提供 {name} 的数据：{error}',
    },
    'files.remoteHeading': { en: 'Files on the remote clipboard', 'zh-CN': '远端剪贴板上的文件' },
    'files.folder': { en: 'folder', 'zh-CN': '目录' },
    'files.download': { en: 'Download', 'zh-CN': '下载' },
    'files.none': { en: '—', 'zh-CN': '—' },
    'files.downloading': { en: 'Downloading {name}', 'zh-CN': '开始下载 {name}' },
    'files.saved': { en: 'Saved {name} ({size})', 'zh-CN': '已保存 {name}（{size}）' },

    // ---- clipboard
    'clipboard.heading': { en: 'Clipboard', 'zh-CN': '剪贴板' },
    'clipboard.clear': { en: 'Clear', 'zh-CN': '清空' },
    'clipboard.empty': {
      en: 'What the remote copies appears here, for you to decide whether it goes into the local clipboard.',
      'zh-CN': '远端复制的内容会出现在这里，由你决定要不要放进本机剪贴板。',
    },
    'clipboard.sendTo': { en: 'Send to the remote', 'zh-CN': '发送到远端' },
    'clipboard.placeholder': {
      en: 'The local clipboard is synced automatically; this sends one more piece of text.',
      'zh-CN': '本机剪贴板会自动同步过去；这里可以额外发送一段文本',
    },
    'clipboard.send': { en: 'Send', 'zh-CN': '发送' },
    'clipboard.fetch': { en: 'Fetch from remote', 'zh-CN': '取回远端' },
    'clipboard.copyToLocal': { en: 'Copy to this machine', 'zh-CN': '复制到本机' },
    'clipboard.copied': { en: 'Copied', 'zh-CN': '已复制' },
    'clipboard.copyFailed': {
      en: 'Could not write to the local clipboard: {error}',
      'zh-CN': '无法写入本机剪贴板：{error}',
    },
    'clipboard.sent': { en: 'The local clipboard was sent to the remote.', 'zh-CN': '已发送本机剪贴板到远端。' },

    // ---- notices
    'notices.heading': { en: 'Notices', 'zh-CN': '通知' },
  };

  // Two languages, one key at a time. A missing translation falls back to
  // English rather than to the key, so a gap shows a sentence rather than
  // "files.sharedFromClipboard" in the middle of the interface.
  function translate(lang, key, vars) {
    const entry = TEXT[key];
    if (!entry) return key;
    let text = entry[lang] !== undefined ? entry[lang] : entry[DEFAULT_LANG];
    if (text === undefined) text = key;
    if (vars) {
      text = text.replace(/\{(\w+)\}/g, (whole, name) =>
        vars[name] !== undefined ? String(vars[name]) : whole);
    }
    return text;
  }

  let current = DEFAULT_LANG;
  try {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (stored && LANGS.some((l) => l.id === stored)) current = stored;
  } catch (e) {
    // A browser with storage disabled still gets a working console in English.
  }

  function lang() { return current; }

  function t(key, vars) { return translate(current, key, vars); }

  // Fills in every element that names a key. Attributes are handled separately
  // because a placeholder and a tooltip are text too, and leaving them in
  // whichever language was written first is how a translated interface ends up
  // half translated.
  function apply(root) {
    const scope = root || document;
    document.documentElement.lang = current;
    for (const node of scope.querySelectorAll('[data-i18n]')) {
      node.textContent = t(node.getAttribute('data-i18n'));
    }
    for (const node of scope.querySelectorAll('[data-i18n-placeholder]')) {
      node.placeholder = t(node.getAttribute('data-i18n-placeholder'));
    }
    for (const node of scope.querySelectorAll('[data-i18n-title]')) {
      node.title = t(node.getAttribute('data-i18n-title'));
    }
  }

  function set(next) {
    if (!LANGS.some((l) => l.id === next)) return;
    current = next;
    try { localStorage.setItem(STORAGE_KEY, next); } catch (e) { /* see above */ }
    apply();
    for (const fn of listeners) fn(next);
  }

  const listeners = [];
  function onChange(fn) { listeners.push(fn); }

  // The picker is filled in and wired here rather than in app.js, so that the
  // language a page opens in does not depend on the rest of the frontend having
  // started: a console that cannot reach its session can still be read.
  function start() {
    apply();
    for (const picker of document.querySelectorAll('[data-lang-picker]')) {
      picker.textContent = '';
      for (const l of LANGS) {
        const option = document.createElement('option');
        option.value = l.id;
        option.textContent = l.label;
        picker.append(option);
      }
      picker.value = current;
      picker.addEventListener('change', () => set(picker.value));
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start, { once: true });
  } else {
    start();
  }

  return { LANGS, DEFAULT_LANG, lang, t, apply, set, onChange };
})();
