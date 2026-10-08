/* AD_EXPLORER prototype — shared interactions.
   Deliberately small: this is a style sample, not the app. */
(function () {
  'use strict';

  var pad = function (n) { return String(n).padStart(2, '0'); };
  var start = Date.now();

  /* ---- clock + uptime ---- */
  function tick() {
    var d = new Date();
    var clock = document.getElementById('clock');
    if (clock) clock.textContent = pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());

    var desk = document.getElementById('deskClock');
    if (desk) desk.textContent = pad(d.getUTCHours()) + ':' + pad(d.getUTCMinutes()) + ' UTC';

    var uptime = document.getElementById('uptime');
    if (uptime) {
      var s = Math.floor((Date.now() - start) / 1000);
      uptime.textContent = pad(Math.floor(s / 3600)) + ':' + pad(Math.floor(s / 60) % 60) + ':' + pad(s % 60);
    }
  }
  tick();
  setInterval(tick, 1000);

  /* ---- toast ---- */
  var toastTimer;
  function toast(msg) {
    var t = document.getElementById('toast');
    if (!t) return;
    var m = document.getElementById('toastMsg');
    if (m) m.textContent = msg;
    t.dataset.open = 'true';
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.dataset.open = 'false'; }, 1800);
  }
  window.__toast = toast;

  document.querySelectorAll('[data-toast]').forEach(function (b) {
    b.addEventListener('click', function () { toast(b.dataset.toast); });
  });

  /* ---- segmented controls ---- */
  document.querySelectorAll('.seg').forEach(function (seg) {
    seg.addEventListener('click', function (e) {
      var b = e.target.closest('button');
      if (!b) return;
      seg.querySelectorAll('button').forEach(function (x) {
        x.setAttribute('aria-selected', String(x === b));
      });
      toast((seg.id === 'resSeg' ? 'RESOLUTION · ' : 'SCALE · ') + b.textContent.trim());
    });
  });

  /* ---- host selection ---- */
  document.querySelectorAll('.host').forEach(function (h) {
    h.addEventListener('click', function () {
      document.querySelectorAll('.host').forEach(function (x) {
        x.dataset.active = String(x === h);
      });
      var n = h.querySelector('.host__name');
      if (n) toast('SESSION · ' + n.textContent.trim());
    });
  });

  /* ---- session tabs ---- */
  document.querySelectorAll('.tab').forEach(function (t) {
    t.addEventListener('click', function () {
      document.querySelectorAll('.tab').forEach(function (x) {
        x.setAttribute('aria-selected', String(x === t));
      });
    });
  });

  /* ---- dock tabs ---- */
  document.querySelectorAll('.dock__tabs button').forEach(function (b) {
    b.addEventListener('click', function () {
      document.querySelectorAll('.dock__tabs button').forEach(function (x) {
        x.setAttribute('aria-selected', String(x === b));
      });
      var key = b.dataset.dockTab;
      document.querySelectorAll('[data-dock-panel]').forEach(function (p) {
        p.hidden = p.dataset.dockPanel !== key;
      });
    });
  });

  /* ---- files button reveals the dock ---- */
  var filesBtn = document.getElementById('filesBtn');
  if (filesBtn) {
    filesBtn.addEventListener('click', function () {
      var shell = document.getElementById('shellBody');
      if (shell) shell.classList.remove('is-dock-collapsed');
      var tab = document.querySelector('[data-dock-tab="files"]');
      if (tab) tab.click();
    });
  }

  /* ---- fullscreen ---- */
  var fsBtn = document.getElementById('fsBtn');
  if (fsBtn) {
    fsBtn.addEventListener('click', function () {
      if (!document.fullscreenElement) {
        document.documentElement.requestFullscreen && document.documentElement.requestFullscreen();
        toast('FULLSCREEN · on');
      } else {
        document.exitFullscreen && document.exitFullscreen();
        toast('FULLSCREEN · off');
      }
    });
  }
  document.addEventListener('fullscreenchange', function () {
    if (fsBtn) fsBtn.classList.toggle('btn--accent', !!document.fullscreenElement);
  });

  /* ---- lock / access control ---- */
  var lock = document.getElementById('lockOverlay');
  function setLock(v) {
    if (!lock) return;
    lock.dataset.open = String(v);
    var a = document.getElementById('accessState');
    if (a) a.textContent = v ? 'locked' : 'unlocked';
  }
  var lockBtn = document.getElementById('lockBtn');
  if (lockBtn) lockBtn.addEventListener('click', function () { setLock(true); });

  var unlockBtn = document.getElementById('unlockBtn');
  if (unlockBtn) unlockBtn.addEventListener('click', function () { setLock(false); toast('ACCESS GRANTED'); });

  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') setLock(false);
  });
  if (lock) {
    var pw = lock.querySelector('input');
    if (pw) pw.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') { setLock(false); toast('ACCESS GRANTED'); }
    });
  }
})();
