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
  if (!me || me.canWrite) {
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
    listStatus.textContent = 'No tmux sessions. Start one on the desktop.';
    return;
  }

  listStatus.textContent = '';
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
  const bits = [];
  if (s.path) bits.push(s.path.replace(/^\/home\/[^/]+/, '~'));
  bits.push(`${s.windows} window${s.windows === 1 ? '' : 's'}`);
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

function attach(name) {
  listView.hidden = true;
  termView.hidden = false;
  termName.textContent = name;
  termStatus.textContent = '';

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

  const url = new URL(`/api/sessions/${encodeURIComponent(name)}/attach`, location.href);
  url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  url.searchParams.set('cols', term.cols);
  url.searchParams.set('rows', term.rows);

  ws = new WebSocket(url);
  ws.binaryType = 'arraybuffer';

  ws.onopen = () => {
    termStatus.textContent = me && !me.canWrite ? 'read-only' : '';
    term.focus();
  };

  ws.onmessage = (ev) => {
    if (ev.data instanceof ArrayBuffer) term.write(new Uint8Array(ev.data));
  };

  ws.onclose = (ev) => {
    // 1008 is the daemon cutting a device off mid-session: revoked, or demoted
    // to read-only while holding a terminal open.
    if (ev.code === 1008) {
      termStatus.textContent = ev.reason || 'access withdrawn';
      loadMe();
      term.options.cursorBlink = false;
      return;
    }
    // 1000 is the daemon saying the session ended, which is information, not a
    // fault. Anything else is a drop worth flagging so nobody types into a
    // terminal that stopped listening several minutes ago.
    termStatus.textContent = ev.code === 1000 ? 'session ended' : 'disconnected';
    term.options.cursorBlink = false;
  };

  ws.onerror = () => { termStatus.textContent = 'connection error'; };

  term.onData((data) => send(data));

  window.addEventListener('resize', onResize);
  // The mobile keyboard opening changes the visual viewport without firing a
  // window resize, so the terminal would otherwise stay sized for a screen half
  // of which is now covered by the keyboard.
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', onResize);
  }
}

function onResize() {
  if (!fit || !term) return;
  fit.fit();
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
  }
}

// sendRaw writes bytes exactly as given. The key bar uses it: an escape
// sequence must not be reinterpreted by the ctrl modifier.
function sendRaw(data) {
  if (!ws || ws.readyState !== WebSocket.OPEN) return;
  ws.send(new TextEncoder().encode(data));
}

// send applies the sticky ctrl modifier, and is what the on-screen keyboard
// feeds through.
function send(data) {
  sendRaw(wrapCtrl(data));
}

function detach() {
  window.removeEventListener('resize', onResize);
  if (window.visualViewport) {
    window.visualViewport.removeEventListener('resize', onResize);
  }
  if (ws) { ws.close(); ws = null; }
  if (term) { term.dispose(); term = null; }
  fit = null;
  setCtrl(false);

  termView.hidden = true;
  listView.hidden = false;
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

document.getElementById('back').addEventListener('click', detach);
document.getElementById('refresh').addEventListener('click', loadSessions);

async function start() {
  await redeemPairingCode();
  await loadMe();
  await loadSessions();
}

start();
