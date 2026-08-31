// Shaping for the Connect panel. Kept out of QML so the parsing has somewhere
// to be read and reasoned about without a running shell.

// parseQrMatrix accepts the 0/1 rows `omarchy-connect pair --json` returns.
//
// A matrix that is not square, or carries anything but 0 and 1, is rejected
// outright rather than drawn. Half a QR renders perfectly happily and scans as
// nothing, which looks like a broken camera rather than a broken payload.
function parseQrMatrix(rows) {
  if (!rows || rows.length === 0) return { rows: [], size: 0 }

  var size = rows[0].length
  if (size !== rows.length) return { rows: [], size: 0 }

  for (var i = 0; i < rows.length; i++) {
    if (rows[i].length !== size || !/^[01]+$/.test(rows[i])) return { rows: [], size: 0 }
  }
  return { rows: rows, size: size }
}

// buildDevices turns the CLI's device array into rows the panel renders.
function buildDevices(raw) {
  if (!raw || raw.length === 0) return []

  var out = []
  for (var i = 0; i < raw.length; i++) {
    var d = raw[i] || {}
    var blocked = d.blocked === true
    var canWrite = !blocked && d.capability === "write"

    out.push({
      id: String(d.id || ""),
      name: String(d.name || "Unnamed device"),
      kind: String(d.kind || ""),
      blocked: blocked,
      canWrite: canWrite,
      // A tailnet device is vouched for by Tailscale on every request, so
      // revoking it blocks rather than deletes. The panel says which, because
      // "revoke" meaning two different things is worth being explicit about.
      revokeVerb: d.kind === "tailnet" ? "Block" : "Revoke",
      capabilityLabel: blocked ? "blocked" : (canWrite ? "can type" : "read-only"),
      lastSeen: relativeTime(d.lastSeen)
    })
  }
  return out
}

function relativeTime(iso) {
  if (!iso) return "never"
  var then = Date.parse(iso)
  if (!isFinite(then)) return "never"

  var secs = (Date.now() - then) / 1000
  if (secs < 60) return "just now"
  if (secs < 3600) return Math.floor(secs / 60) + "m ago"
  if (secs < 86400) return Math.floor(secs / 3600) + "h ago"
  return Math.floor(secs / 86400) + "d ago"
}

// secondsUntil is the pairing countdown. It floors at zero so an expired code
// counts down to "expired" rather than into negative numbers.
function secondsUntil(iso) {
  if (!iso) return 0
  var at = Date.parse(iso)
  if (!isFinite(at)) return 0
  return Math.max(0, Math.round((at - Date.now()) / 1000))
}

if (typeof module !== "undefined") {
  module.exports = {
    parseQrMatrix: parseQrMatrix,
    buildDevices: buildDevices,
    relativeTime: relativeTime,
    secondsUntil: secondsUntil
  }
}
