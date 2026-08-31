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
    termStatus.textContent = '';
    term.focus();
  };

  ws.onmessage = (ev) => {
    if (ev.data instanceof ArrayBuffer) term.write(new Uint8Array(ev.data));
  };

  ws.onclose = (ev) => {
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

loadSessions();
