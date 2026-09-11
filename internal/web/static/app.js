/* repo-man — progressive enhancement only.
 *
 * The tree is server-rendered and works without any of this. What follows adds
 * a theme toggle, live refresh of the #live region, collapse persistence, the
 * two action dialogs, and the per-row fetch/clone buttons.
 *
 * The CSP forbids inline scripts and handlers, so everything is wired here with
 * addEventListener and data-* attributes.
 */
(function () {
  'use strict';

  var LS_THEME = 'repoman.theme';
  var LS_COLLAPSED = 'repoman.collapsed';
  var POLL_IDLE = 10000;
  var POLL_BUSY = 2000;

  function store(key, value) {
    try { value === null ? localStorage.removeItem(key) : localStorage.setItem(key, value); } catch (e) { /* private mode */ }
  }
  function load(key) {
    try { return localStorage.getItem(key); } catch (e) { return null; }
  }

  /* ------------------------------------------------------------- theme -- */
  // Applied at parse time (the script is not deferred) so there is no flash.

  function applyTheme(mode) {
    var root = document.documentElement;
    if (mode === 'light' || mode === 'dark') root.setAttribute('data-theme', mode);
    else root.removeAttribute('data-theme');
  }
  function currentTheme() {
    var saved = load(LS_THEME);
    if (saved) return saved;
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }
  applyTheme(load(LS_THEME));

  /* --------------------------------------------------------------- api -- */

  function api(path, options) {
    var opts = options || {};
    var headers = { Accept: 'application/json' };
    if (opts.body) headers['Content-Type'] = 'application/json';
    return fetch(path, {
      method: opts.method || 'GET',
      credentials: 'same-origin',
      headers: headers,
      body: opts.body ? JSON.stringify(opts.body) : undefined
    }).then(function (res) {
      if (res.status === 401) { location.reload(); throw new Error('signed out'); }
      var type = res.headers.get('content-type') || '';
      if (type.indexOf('text/html') === 0) return res.text().then(function (t) { return okOrThrow(res, t, null); });
      return res.json().catch(function () { return null; }).then(function (data) { return okOrThrow(res, data, data); });
    });
  }
  function okOrThrow(res, value, data) {
    if (res.ok) return value;
    throw new Error((data && data.error) || ('request failed (' + res.status + ')'));
  }

  /* ------------------------------------------------------------ notice -- */

  var notice, noticeTimer;
  function say(message, kind) {
    if (!notice) return;
    notice.textContent = message;
    notice.dataset.kind = kind || 'error';
    notice.hidden = false;
    clearTimeout(noticeTimer);
    if (kind === 'ok') noticeTimer = setTimeout(function () { notice.hidden = true; }, 4000);
  }

  /* --------------------------------------------------- collapse state  -- */

  function collapsedSet() {
    try { return new Set(JSON.parse(load(LS_COLLAPSED) || '[]')); } catch (e) { return new Set(); }
  }
  function rememberCollapsed(rel, isCollapsed) {
    var set = collapsedSet();
    isCollapsed ? set.add(rel) : set.delete(rel);
    store(LS_COLLAPSED, JSON.stringify(Array.from(set)));
  }
  function applyCollapsed(root) {
    var set = collapsedSet();
    root.querySelectorAll('details[data-rel]').forEach(function (d) {
      d.open = !set.has(d.dataset.rel);
    });
  }

  /* ------------------------------------------------------- live region -- */

  var live, pollState, timer, inFlight = false;

  function hasRunningJob() { return !!live.querySelector('.job.is-running'); }

  function refresh() {
    if (inFlight) return Promise.resolve();
    inFlight = true;
    return api('/fragment/tree').then(function (html) {
      if (typeof html === 'string') {
        live.innerHTML = html;
        applyCollapsed(live);
      }
    }).catch(function (err) {
      say(err.message);
    }).then(function () {
      inFlight = false;
    });
  }

  function schedule() {
    clearTimeout(timer);
    var wait = hasRunningJob() ? POLL_BUSY : POLL_IDLE;
    if (pollState) pollState.textContent = document.hidden ? 'live refresh paused' : 'live refresh on';
    timer = setTimeout(tick, wait);
  }
  function tick() {
    if (document.hidden) { schedule(); return; }
    refresh().then(schedule);
  }

  /* ----------------------------------------------------------- dialogs -- */

  function dialogError(form, message) {
    var box = form.querySelector('[data-error]');
    if (!box) return;
    box.textContent = message || '';
    box.hidden = !message;
  }

  function wireDialog(dialogId, formId, submit) {
    var dlg = document.getElementById(dialogId);
    var form = document.getElementById(formId);
    if (!dlg || !form) return null;

    dlg.addEventListener('click', function (e) {
      if (e.target.hasAttribute('data-close')) dlg.close();
    });
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      dialogError(form, '');
      var button = form.querySelector('button[type="submit"]');
      button.disabled = true;
      var data = Object.fromEntries(new FormData(form).entries());
      submit(data).then(function (message) {
        dlg.close();
        form.reset();
        say(message, 'ok');
        return refresh().then(schedule);
      }).catch(function (err) {
        dialogError(form, err.message);
      }).then(function () {
        button.disabled = false;
      });
    });
    return dlg;
  }

  /* ------------------------------------------------------- row actions -- */

  var rowActions = {
    fetch: function (btn) {
      return api('/api/fetch', { method: 'POST', body: { path: btn.dataset.rel } })
        .then(function () { say('Fetched ' + (btn.dataset.rel || 'repo') + '.', 'ok'); });
    },
    clone: function (btn) {
      return api('/api/clone', { method: 'POST', body: { url: btn.dataset.url, dest: btn.dataset.dest || '' } })
        .then(function (job) { say('Cloning ' + ((job && job.target) || btn.dataset.url) + '…', 'ok'); });
    }
  };

  /* -------------------------------------------------------------- init -- */

  document.addEventListener('DOMContentLoaded', function () {
    notice = document.getElementById('notice');
    live = document.getElementById('live');
    pollState = document.getElementById('poll-state');
    if (!live) return;

    applyCollapsed(live);

    // `toggle` does not bubble, so listen in the capture phase.
    live.addEventListener('toggle', function (e) {
      var d = e.target;
      if (d.tagName === 'DETAILS' && d.dataset.rel !== undefined) rememberCollapsed(d.dataset.rel, !d.open);
    }, true);

    var themeButton = document.getElementById('btn-theme');
    if (themeButton) {
      themeButton.addEventListener('click', function () {
        var next = currentTheme() === 'dark' ? 'light' : 'dark';
        store(LS_THEME, next);
        applyTheme(next);
        themeButton.title = 'Switch to ' + (next === 'dark' ? 'light' : 'dark') + ' theme';
      });
    }

    var newDialog = wireDialog('dlg-new', 'form-new', function (data) {
      return api('/api/init', {
        method: 'POST',
        body: { parent: data.parent || '', name: data.name || '', branch: data.branch || 'main' }
      }).then(function (res) { return 'Created ' + ((res && res.path) || data.name) + '.'; });
    });
    var cloneDialog = wireDialog('dlg-clone', 'form-clone', function (data) {
      return api('/api/clone', { method: 'POST', body: { url: data.url || '', dest: data.dest || '' } })
        .then(function (job) { return 'Cloning ' + ((job && job.target) || data.url) + '…'; });
    });

    function openDialog(dlg, prefill) {
      if (!dlg) return;
      dialogError(dlg.querySelector('form'), '');
      if (prefill) prefill();
      dlg.showModal();
    }

    var newButton = document.getElementById('btn-new');
    if (newButton) newButton.addEventListener('click', function () { openDialog(newDialog); });
    var cloneButton = document.getElementById('btn-clone');
    if (cloneButton) cloneButton.addEventListener('click', function () { openDialog(cloneDialog); });

    document.querySelectorAll('.toolbar [data-action]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var full = btn.dataset.action === 'fetchall';
        btn.disabled = true;
        api('/api/refresh' + (full ? '?fetch=1' : ''), { method: 'POST' })
          .then(function () { say(full ? 'Fetching every checkout…' : 'Rescanning…', 'ok'); })
          .catch(function (err) { say(err.message); })
          .then(function () { btn.disabled = false; return refresh().then(schedule); });
      });
    });

    // Delegated: the fragment swap replaces every row, so per-row listeners
    // would not survive it.
    live.addEventListener('click', function (e) {
      var btn = e.target.closest('button[data-action]');
      if (!btn || !live.contains(btn)) return;
      e.preventDefault(); // also stops a <summary> row from toggling
      var action = btn.dataset.action;
      if (action === 'init') {
        openDialog(newDialog, function () {
          var parent = document.getElementById('new-parent');
          if (parent) parent.value = btn.dataset.rel || '';
          var name = document.getElementById('new-name');
          if (name) name.value = '';
        });
        return;
      }
      var run = rowActions[action];
      if (!run) return;
      btn.disabled = true;
      run(btn)
        .catch(function (err) { say(err.message); })
        .then(function () { btn.disabled = false; return refresh().then(schedule); });
    });

    document.addEventListener('visibilitychange', function () {
      if (!document.hidden) { refresh().then(schedule); } else { schedule(); }
    });

    schedule();
  });
})();
