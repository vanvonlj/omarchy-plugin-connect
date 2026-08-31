'use strict';

const listView = document.getElementById('list-view');
const termView = document.getElementById('term-view');
const sessionsEl = document.getElementById('sessions');
const listStatus = document.getElementById('list-status');
const termName = document.getElementById('term-name');
const termStatus = document.getElementById('term-status');

let term = null;
let fit = null;
let ws = null;
let ctrlArmed = false;
let me = null;   // this device: name, capability, canWrite
let knownSessions = [];

// ---------- pairing ----------

// A QR carries the daemon URL plus a one-time code. Redeem it before anything
// else, then strip it from the address bar: a code left in the URL survives in
// history and in whatever the browser syncs between devices, long after it has
// stopped being single-use in any meaningful sense.
async function redeemPairingCode() {
  const params = new URLSearchParams(location.search);
  const code = params.get('pair');
  if (!code) return;

  history.replaceState(null, '', location.pathname);

  try {
    const res = await fetch('/api/pair', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code, name: deviceName() }),
    });
    if (!res.ok) {
      listStatus.textContent = 'Pairing failed: the code was invalid or had expired.';
      return;
    }
  } catch (err) {
    listStatus.textContent = `Pairing failed: ${err.message}`;
  }
}

// A first guess at a name, so the device list is not a row of "Paired device".
// It is editable from the plugin panel, which is the point of storing it.
function deviceName() {
  const ua = navigator.userAgent;
  if (/iPhone/.test(ua)) return 'iPhone';
  if (/iPad/.test(ua)) return 'iPad';
  if (/Android/.test(ua)) return 'Android phone';
  if (/Macintosh/.test(ua)) return 'Mac';
  if (/Linux/.test(ua)) return 'Linux browser';
  if (/Windows/.test(ua)) return 'Windows browser';
  return 'Paired device';
}

async function loadMe() {
  try {
    const res = await fetch('/api/me');
    if (!res.ok) return;
    me = await res.json();
  } catch {
    me = null;
  }
  renderCapability();
}

// Say read-only up front. tmux enforces it regardless, but letting someone type
// into a terminal for a while and wonder why nothing lands is a poor way to
// find out.
function renderCapability() {
  const banner = document.getElementById('capability');
  const canWrite = !me || me.canWrite;

  // Hide the controls a read-only device cannot use rather than letting it
  // press them and collect an error.
  document.getElementById('new-session').hidden = !canWrite;
  document.getElementById('term-menu').hidden = !canWrite;
  document.getElementById('keys').hidden = !canWrite;

  if (canWrite) {
    banner.hidden = true;
    return;
  }
  banner.hidden = false;
  banner.textContent = `Read-only — promote "${me.name}" on the desktop to type`;
}

// ---------- session list ----------

async function loadSessions() {
  listStatus.textContent = 'Loading…';
  sessionsEl.replaceChildren();

  let sessions;
  try {
    const res = await fetch('/api/sessions');
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    sessions = await res.json();
  } catch (err) {
    listStatus.textContent = `Could not reach the daemon: ${err.message}`;
    return;
  }

  if (sessions.length === 0) {
    knownSessions = [];
    listStatus.textContent = 'No tmux sessions. Start one on the desktop.';
    return;
  }

  listStatus.textContent = '';
  knownSessions = sessions.map((s) => s.name);
  for (const s of sessions) {
    sessionsEl.append(sessionRow(s));
  }
}

function sessionRow(s) {
  const row = document.createElement('button');
  row.className = 'session';
  row.setAttribute('role', 'listitem');

  const top = document.createElement('div');
  top.className = 'session-top';

  const name = document.createElement('span');
  name.className = 'session-name';
  // textContent, never innerHTML: a tmux session name is arbitrary text that
  // someone can set from a shell, and it renders on a page that can open a
  // terminal.
  name.textContent = s.name;
  top.append(name);

  if (s.command) {
    const cmd = document.createElement('span');
    cmd.className = 'badge';
    cmd.textContent = s.command;
    top.append(cmd);
  }

  if (s.attached) {
    const at = document.createElement('span');
    at.className = 'badge attached';
    at.textContent = 'attached';
    top.append(at);
  }

  const meta = document.createElement('div');
  meta.className = 'session-meta';
  // The tmux window count used to be here. It means nothing to someone who does
  // not use tmux, and where the session is and when it last did something are
  // the two things that actually identify it.
  const bits = [];
  if (s.path) bits.push(s.path.replace(/^\/home\/[^/]+/, '~'));
  if (s.activity) bits.push(relativeTime(s.activity));
  meta.textContent = bits.join(' · ');

  row.append(top, meta);
  row.addEventListener('click', () => attach(s.name));
  return row;
}

function relativeTime(iso) {
  const secs = (Date.now() - new Date(iso).getTime()) / 1000;
  if (secs < 60) return 'just now';
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
  return `${Math.floor(secs / 86400)}d ago`;
}

// ---------- terminal ----------

// Roaming is the normal case, not an error case. Walking out of the house
// changes the path from a direct wifi connection to a Tailscale subnet route,
// and every TCP connection in flight dies with it. tmux keeps the session, so
// the only thing that needs to survive is this client's willingness to dial
// again -- which is what the whole block below is for.

let currentSession = null;
let reconnectAttempt = 0;
let reconnectTimer = null;
let deliberateClose = false;

// Backoff: quick at first because most drops are a two-second network change,
// then longer so a phone in a pocket is not dialling all night. Jitter keeps
// several open tabs from retrying in lockstep.
const BACKOFF_MS = [500, 1000, 2000, 4000, 8000, 15000];

function backoffDelay() {
  const base = BACKOFF_MS[Math.min(reconnectAttempt, BACKOFF_MS.length - 1)];
  return base + Math.floor(Math.random() * 400);
}

const SESSION_KEY = 'omarchy_connect_last_session';

function rememberSession(name) {
  try { localStorage.setItem(SESSION_KEY, name); } catch {}
}

function forgetSession() {
  try { localStorage.removeItem(SESSION_KEY); } catch {}
}

function lastSession() {
  try { return localStorage.getItem(SESSION_KEY); } catch { return null; }
}

function attach(name) {
  listView.hidden = true;
  termView.hidden = false;
  termName.textContent = name;

  currentSession = name;
  reconnectAttempt = 0;
  deliberateClose = false;
  rememberSession(name);

  term = new Terminal({
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
    fontSize: 13,
    cursorBlink: true,
    scrollback: 5000,
    theme: { background: '#0e0e10', foreground: '#e4e4e7' },
  });
  fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(document.getElementById('terminal'));
  fit.fit();

  term.onData((data) => send(data));

  window.addEventListener('resize', onResize);
  // The mobile keyboard opening changes the visual viewport without firing a
  // window resize, so the terminal would otherwise stay sized for a screen half
  // of which is now covered by the keyboard.
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', onResize);
  }

  connect();
}

function connect() {
  if (!currentSession || !term) return;
  clearTimeout(reconnectTimer);

  setStatus(reconnectAttempt === 0 ? 'connecting' : `reconnecting…`);

  const url = new URL(`/api/sessions/${encodeURIComponent(currentSession)}/attach`, location.href);
  url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  url.searchParams.set('cols', term.cols);
  url.searchParams.set('rows', term.rows);

  ws = new WebSocket(url);
  ws.binaryType = 'arraybuffer';

  ws.onopen = () => {
    reconnectAttempt = 0;
    setStatus(me && !me.canWrite ? 'read-only' : '');
    term.options.cursorBlink = true;
    // tmux repaints the whole screen for a newly attached client, so there is
    // nothing to replay here -- the session's current state arrives on its own.
    term.focus();
    onResize();
  };

  ws.onmessage = (ev) => {
    if (ev.data instanceof ArrayBuffer) term.write(new Uint8Array(ev.data));
  };

  ws.onclose = (ev) => {
    term.options.cursorBlink = false;
    if (deliberateClose) return;

    // 1008 is the daemon cutting this device off: revoked, or demoted to
    // read-only while holding a terminal open. Retrying would be a client
    // arguing with a decision made at the desktop, so it stops.
    if (ev.code === 1008) {
      setStatus(ev.reason || 'access withdrawn');
      loadMe();
      return;
    }

    // 1000 means the session itself ended. There is nothing to reconnect to.
    if (ev.code === 1000) {
      setStatus('session ended');
      forgetSession();
      return;
    }

    scheduleReconnect();
  };

  // onerror always precedes onclose, so reconnection is driven from onclose
  // alone. Scheduling from both would double every backoff.
  ws.onerror = () => {};
}

function scheduleReconnect() {
  if (!currentSession) return;

  // Offline is a state to wait out, not to back off through: burning retries
  // while the radio is off means the queue is long precisely when the network
  // returns. The online listener below wakes this up instead.
  if (navigator.onLine === false) {
    setStatus('offline');
    return;
  }

  const delay = backoffDelay();
  reconnectAttempt++;
  setStatus(`reconnecting…`);
  reconnectTimer = setTimeout(connect, delay);
}

// A dropped connection that the phone has not noticed yet looks exactly like a
// live one. These three are the moments when the truth is likely to have
// changed, and reconnecting eagerly at them is what makes coming home feel
// instant rather than taking a backoff step.
function reconnectNow() {
  if (!currentSession) return;
  if (ws && ws.readyState === WebSocket.OPEN) return;
  reconnectAttempt = 0;
  connect();
}

window.addEventListener('online', reconnectNow);
window.addEventListener('focus', reconnectNow);
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') reconnectNow();
});

function setStatus(text) {
  termStatus.textContent = text;
}

function detach() {
  // Deliberate: going back to the list must not trigger the reconnect logic.
  deliberateClose = true;
  currentSession = null;
  reconnectAttempt = 0;
  clearTimeout(reconnectTimer);
  forgetSession();

  window.removeEventListener('resize', onResize);
  if (window.visualViewport) {
    window.visualViewport.removeEventListener('resize', onResize);
  }
  if (ws) { ws.close(); ws = null; }
  if (term) { term.dispose(); term = null; }
  fit = null;
  setCtrl(false);

  document.getElementById('menu').hidden = true;
  termView.hidden = true;
  listView.hidden = false;
  showNewForm(false);
  loadSessions();
}

// ---------- key bar ----------

function setCtrl(on) {
  ctrlArmed = on;
  document.getElementById('ctrl').setAttribute('aria-pressed', String(on));
}

document.getElementById('keys').addEventListener('click', (ev) => {
  const btn = ev.target.closest('button');
  if (!btn) return;

  if (btn.dataset.modifier === 'ctrl') {
    setCtrl(!ctrlArmed);
    if (term) term.focus();
    return;
  }

  // Escapes are written as \x1b in the HTML so the markup stays readable; JSON
  // parses them back into the real control bytes. sendRaw, not send: ^C is
  // already a control byte and must not be run through the modifier again.
  sendRaw(JSON.parse(`"${btn.dataset.seq}"`));
  if (term) term.focus();
});

// Ctrl is a sticky modifier -- tap ctrl, then a letter -- because holding two
// keys at once is not something a touchscreen does well. It applies only to
// plain letters from the on-screen keyboard; anything else passes through and
// disarms it, so a stuck modifier cannot silently corrupt later input.
function wrapCtrl(data) {
  if (!ctrlArmed) return data;
  setCtrl(false);
  if (data.length !== 1) return data;
  const c = data.toLowerCase();
  if (c >= 'a' && c <= 'z') {
    return String.fromCharCode(c.charCodeAt(0) - 96);
  }
  return data;
}

// ---------- creating and ending sessions ----------

const newForm = document.getElementById('new-form');
const newName = document.getElementById('new-name');
const newPath = document.getElementById('new-path');
const newError = document.getElementById('new-error');

function showNewForm(show) {
  newForm.hidden = !show;
  newError.textContent = '';
  if (show) {
    newName.value = '';
    newPath.value = '';
    newName.focus();
  }
}

document.getElementById('new-session').addEventListener('click', () => {
  if (me && !me.canWrite) {
    listStatus.textContent = 'This device is read-only. Promote it on the desktop to start sessions.';
    return;
  }
  showNewForm(newForm.hidden);
});

document.getElementById('new-cancel').addEventListener('click', () => showNewForm(false));

newForm.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const name = newName.value.trim();
  if (!name) { newError.textContent = 'Give it a name.'; return; }

  try {
    const res = await fetch('/api/sessions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, path: newPath.value.trim() }),
    });
    if (!res.ok) {
      // The daemon's message is the useful one here -- it knows why tmux
      // refused the name.
      newError.textContent = (await res.text()).trim() || `HTTP ${res.status}`;
      return;
    }
  } catch (err) {
    newError.textContent = err.message;
    return;
  }

  showNewForm(false);
  await loadSessions();
  attach(name);
});

const menu = document.getElementById('menu');

document.getElementById('term-menu').addEventListener('click', () => {
  menu.hidden = !menu.hidden;
});

menu.addEventListener('click', async (ev) => {
  const btn = ev.target.closest('button');
  if (!btn) return;
  const action = btn.dataset.action;
  menu.hidden = true;

  if (action === 'interrupt') { sendRaw('\x03'); term && term.focus(); return; }

  // Ctrl-L rather than clearing xterm.js: the shell redraws its prompt, so the
  // screen matches what the desktop sees instead of only looking cleared here.
  if (action === 'clear') { sendRaw('\x0c'); term && term.focus(); return; }

  if (action === 'kill') {
    if (!confirm(`End "${currentSession}"? Anything running in it stops.`)) return;
    const name = currentSession;
    try {
      const res = await fetch(`/api/sessions/${encodeURIComponent(name)}`, { method: 'DELETE' });
      if (!res.ok) { setStatus((await res.text()).trim() || 'could not end session'); return; }
    } catch (err) {
      setStatus(err.message);
      return;
    }
    detach();
  }
});

document.getElementById('back').addEventListener('click', detach);
document.getElementById('refresh').addEventListener('click', loadSessions);

async function start() {
  await redeemPairingCode();
  await loadMe();
  await loadSessions();

  // iOS discards a backgrounded tab aggressively, so "open the app and carry
  // on" has to survive a full page load, not just a dropped socket. Resume the
  // session that was open, but only if it is still there -- landing on a
  // terminal for a session that has since been killed is worse than the list.
  const last = lastSession();
  if (last && knownSessions.includes(last)) attach(last);
}

start();
