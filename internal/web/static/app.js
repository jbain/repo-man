/* repo-man — progressive enhancement only.
 *
 * The tree is server-rendered and works without any of this. What follows adds
 * a theme toggle, live refresh of the #live region, collapse persistence, the
 * two action dialogs, the per-row fetch/pull/clone buttons, and the toasts that
 * report what those buttons did.
 *
 * The CSP forbids inline scripts and handlers, so everything is wired here with
 * addEventListener and data-* attributes.
 */
(function () {
  'use strict';

  var LS_THEME = 'repoman.theme';
  var LS_COLLAPSED = 'repoman.collapsed';
  var LS_ONLY_LOCAL = 'repoman.onlylocal';
  var POLL_IDLE = 10000;
  var POLL_BUSY = 2000;
  var TOAST_TTL = 30000;

  // Tells the stylesheet that the scripted UI is in charge, so the pieces that
  // exist only as a no-JS fallback (the server-rendered operations list, which
  // here becomes the toast feed) can be hidden. Set at parse time, before any
  // of it has been painted.
  document.documentElement.classList.add('js');

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

  /* -------------------------------------------------- only-cloned filter -- */
  // Purely presentational: the server always sends the whole tree, and CSS on
  // <html> hides the ghosts. Applied at parse time, alongside the theme, so a
  // filtered view never flashes its ghosts before hiding them.

  function onlyLocal() { return load(LS_ONLY_LOCAL) === '1'; }
  function applyOnlyLocal(on) {
    document.documentElement.classList.toggle('only-local', on);
  }
  applyOnlyLocal(onlyLocal());

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

  /* ------------------------------------------------------------ toasts -- */
  // Status lives in a fixed stack in the lower right rather than a banner above
  // the tree. A banner that appears, grows, or disappears reflows everything
  // under it, which moves the row you were about to click — and the operations
  // list did exactly that on every poll while a clone ran.
  //
  // Each toast has a key. Calling toast() again with the same key updates that
  // toast in place instead of stacking a second one, which is what lets a
  // running job's single toast follow it from "cloning" to "cloned".

  var toasts;
  var shown = {};   // key -> {el, msg, detail, timer, onDismiss}
  var saySeq = 0;

  function dismissToast(key) {
    var t = shown[key];
    if (!t) return;
    delete shown[key];
    clearTimeout(t.timer);
    if (t.onDismiss) t.onDismiss();
    t.el.classList.remove('is-in');
    // Drop it when the fade ends, with a timer as the backstop: a browser with
    // transitions disabled never fires transitionend, and a toast stuck at
    // opacity 0 would still occupy space in the stack.
    var drop = function () { if (t.el.parentNode) t.el.parentNode.removeChild(t.el); };
    t.el.addEventListener('transitionend', drop);
    setTimeout(drop, 400);
  }

  function toast(key, opts) {
    if (!toasts) return null;
    var t = shown[key];
    if (!t) {
      var el = document.createElement('div');
      el.className = 'toast';
      var body = document.createElement('div');
      body.className = 'toast-body';
      var msg = document.createElement('p');
      msg.className = 'toast-msg';
      var detail = document.createElement('pre');
      detail.className = 'toast-detail mono';
      detail.hidden = true;
      body.appendChild(msg);
      body.appendChild(detail);
      var x = document.createElement('button');
      x.type = 'button';
      x.className = 'toast-x';
      x.setAttribute('aria-label', 'Dismiss');
      x.textContent = '\u00d7';
      x.addEventListener('click', function () { dismissToast(key); });
      el.appendChild(body);
      el.appendChild(x);
      toasts.appendChild(el);
      t = shown[key] = { el: el, msg: msg, detail: detail };
      // One frame at the starting opacity, so the entry actually transitions
      // rather than the element simply appearing finished.
      requestAnimationFrame(function () { el.classList.add('is-in'); });
    }

    t.el.className = 'toast is-in toast--' + (opts.kind || 'info') + (opts.busy ? ' is-busy' : '');
    t.msg.textContent = opts.message || '';
    t.detail.textContent = opts.detail || '';
    t.detail.hidden = !opts.detail;
    t.onDismiss = opts.onDismiss;

    clearTimeout(t.timer);
    // Work still in flight is sticky: a clone can easily outlive the timeout,
    // and a progress toast that vanishes mid-clone is worse than none.
    if (!opts.sticky) t.timer = setTimeout(function () { dismissToast(key); }, TOAST_TTL);
    return t;
  }

  function say(message, kind) {
    toast('say-' + (++saySeq), { message: message, kind: kind || 'error' });
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

  /* --------------------------------------------------------------- jobs -- */
  // Jobs arrive as markup inside the polled fragment (hidden by CSS once the
  // `js` class is on <html>) rather than through a request of their own, so
  // watching an operation costs no extra round trip. This turns that markup
  // into toasts, and tells the clone buttons which destinations are busy.

  var jobState = {};      // id -> the state we last raised a toast for
  var jobDismissed = {};  // id -> the state it was dismissed at
  var firstJobSync = true;
  var runningClones = {}; // destination -> a clone job is in flight for it
  var pendingClones = {}; // destination -> we just started one, job not seen yet

  var jobWords = {
    clone: { running: 'Cloning', done: 'Cloned', failed: 'Clone failed' },
    init: { running: 'Creating', done: 'Created', failed: 'Create failed' }
  };

  function nodeText(root, selector) {
    var el = root.querySelector(selector);
    return el ? el.textContent.trim() : '';
  }

  // git's progress output is hundreds of lines of counting; only where it got
  // to belongs in a toast.
  function lastLine(text) {
    if (!text) return '';
    var lines = text.split('\n');
    for (var i = lines.length - 1; i >= 0; i--) {
      if (lines[i].trim()) return lines[i].trim();
    }
    return '';
  }

  function toastForJob(li, state) {
    var id = li.dataset.job;
    var kind = li.dataset.kind || '';
    var what = li.dataset.target || nodeText(li, '.job-label');
    var words = jobWords[kind] || { running: kind, done: kind + ' finished', failed: kind + ' failed' };
    var remember = function () { jobDismissed[id] = state; };

    if (state === 'running') {
      toast('job-' + id, {
        kind: 'info', busy: true, sticky: true, onDismiss: remember,
        message: words.running + ' ' + what + '…',
        detail: lastLine(nodeText(li, '.job-out'))
      });
    } else if (state === 'failed') {
      toast('job-' + id, {
        kind: 'error', onDismiss: remember,
        message: words.failed + ': ' + what,
        detail: nodeText(li, '.job-err') || lastLine(nodeText(li, '.job-out'))
      });
    } else {
      toast('job-' + id, { kind: 'ok', onDismiss: remember, message: words.done + ' ' + what });
    }
  }

  function syncJobs() {
    var running = {};
    live.querySelectorAll('.jobs .job').forEach(function (li) {
      var id = li.dataset.job;
      var state = li.dataset.state || '';
      var target = li.dataset.target || '';
      if (!id) return;

      if (li.dataset.kind === 'clone' && target) {
        if (state === 'running') running[target] = true;
        else delete pendingClones[target];
      }

      if (jobDismissed[id] === state) return;
      // A page load must not replay every job the registry still remembers:
      // on the first pass only work still in flight is news. After that, a
      // change of state is.
      if (firstJobSync && state !== 'running') { jobState[id] = state; return; }
      // A running job's toast is refreshed every poll so its progress line
      // keeps up; a finished one is written once, or its 30s timer would be
      // restarted forever and it would never fade.
      if (jobState[id] === state && state !== 'running') return;
      jobState[id] = state;
      toastForJob(li, state);
    });
    firstJobSync = false;
    runningClones = running;
  }

  // markCloning swaps a clone button for a spinner. The row's actions are
  // invisible until hover, so the busy class on the container is what keeps a
  // clone in progress visible once the pointer moves away.
  function markCloning(btn) {
    if (btn.dataset.busy === '1') return;
    btn.dataset.busy = '1';
    btn.disabled = true;
    btn.textContent = 'cloning';
    var spinner = document.createElement('span');
    spinner.className = 'spinner';
    spinner.setAttribute('aria-hidden', 'true');
    btn.insertBefore(spinner, btn.firstChild);
    var actions = btn.closest('.actions');
    if (actions) actions.classList.add('is-busy');
  }

  // Re-applied after every fragment swap: the poll replaces each row wholesale,
  // so the spinner has to be derived from job state rather than left in the DOM.
  function applyCloneBusy(root) {
    root.querySelectorAll('button[data-action="clone"]').forEach(function (btn) {
      var dest = btn.dataset.dest || '';
      if (runningClones[dest] || pendingClones[dest]) markCloning(btn);
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
        syncJobs();
        applyCloneBusy(live);
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
        // An empty message means the caller has a better report coming (a job
        // toast); an empty toast would just be a floating X to close.
        if (message) say(message, 'ok');
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
    pull: function (btn) {
      return api('/api/pull', { method: 'POST', body: { path: btn.dataset.rel } })
        .then(function () { say('Pulled ' + (btn.dataset.rel || 'repo') + '.', 'ok'); });
    },
    clone: function (btn) {
      // Optimistic: the button spins from the click, not from the first poll
      // that happens to see the job. The destination is remembered so the
      // spinner survives the fragment swap that follows immediately after.
      markCloning(btn);
      return api('/api/clone', { method: 'POST', body: { url: btn.dataset.url, dest: btn.dataset.dest || '' } })
        .then(function (job) {
          if (job && job.target) pendingClones[job.target] = true;
          // No message here: the job itself raises a toast that then tracks it
          // through to done or failed, and two toasts for one clone is noise.
        });
    }
  };

  /* -------------------------------------------------------------- init -- */

  document.addEventListener('DOMContentLoaded', function () {
    toasts = document.getElementById('toasts');
    live = document.getElementById('live');
    pollState = document.getElementById('poll-state');
    if (!live) return;

    applyCollapsed(live);
    syncJobs();
    applyCloneBusy(live);

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

    var localButton = document.getElementById('btn-local');
    if (localButton) {
      var syncLocalButton = function () {
        var on = onlyLocal();
        localButton.setAttribute('aria-pressed', on ? 'true' : 'false');
        localButton.title = on
          ? 'Showing only repositories cloned here — click to show un-cloned ones too'
          : 'Hide repositories that exist on the provider but are not cloned here';
      };
      syncLocalButton();
      localButton.addEventListener('click', function () {
        var next = !onlyLocal();
        store(LS_ONLY_LOCAL, next ? '1' : null);
        applyOnlyLocal(next);
        syncLocalButton();
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
        .then(function (job) {
          if (job && job.target) pendingClones[job.target] = true;
          return ''; // the job's own toast reports it; see rowActions.clone
        });
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
