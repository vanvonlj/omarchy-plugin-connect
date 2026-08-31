import QtQuick
import Quickshell
import Quickshell.Io
import "Model.js" as Model

// Service owns every call to the omarchy-connect CLI.
//
// The panel never reasons about devices itself: the daemon is the single source
// of truth, the CLI is its interface, and this file is the only thing that
// speaks it. That is what keeps the panel and the terminal from disagreeing.
Item {
  id: root

  property var settings: ({})

  property bool installed: true
  property bool daemonRunning: false   // the systemd unit, which drives Start/Stop
  property bool serving: false         // something is actually listening
  property bool checkedInstall: false

  property string node: ""
  property string url: ""
  property bool certsAvailable: false
  property int port: 0
  property string problem: ""

  property var devices: []
  property string lastError: ""
  property string actionStatus: ""

  // True when the installed binary is older than this plugin.
  //
  // `omarchy plugin update` refreshes the QML and leaves the daemon binary
  // alone, so the panel can be a version ahead of the CLI it drives. That
  // surfaces as an unknown-subcommand error, which says nothing about the
  // actual problem or its one-line fix.
  property bool binaryOutdated: false

  // Pairing state. qrSize of 0 means "nothing to draw", which the panel reads
  // rather than tracking a separate flag.
  property var qrRows: []
  property int qrSize: 0
  property string pairUrl: ""
  property string pairExpires: ""
  property int pairSecondsLeft: 0

  readonly property int refreshIntervalSec: intSetting("refreshIntervalSec", 30, 5, 300)
  readonly property bool busy: statusProcess.running || devicesProcess.running || pairProcess.running || actionProcess.running

  function setting(name, fallback) {
    var value = settings ? settings[name] : undefined
    return value === undefined || value === null ? fallback : value
  }

  function intSetting(name, fallback, min, max) {
    var n = parseInt(String(setting(name, fallback)), 10)
    if (!isFinite(n)) n = fallback
    if (n < min) n = min
    if (n > max) n = max
    return n
  }

  function refresh() {
    if (!checkedInstall) {
      if (!whichProcess.running) {
        whichProcess.command = ["which", "omarchy-connect"]
        whichProcess.running = true
      }
      return
    }
    if (!installed) return

    if (!statusProcess.running) {
      statusProcess.command = ["omarchy-connect", "status", "--json"]
      statusProcess.running = true
    }
    if (!devicesProcess.running) {
      devicesProcess.command = ["omarchy-connect", "devices", "--json"]
      devicesProcess.running = true
    }
    if (!healthProcess.running) {
      healthProcess.command = ["systemctl", "--user", "is-active", "--quiet", "omarchy-connect"]
      healthProcess.running = true
    }
  }

  function startPairing() {
    if (pairProcess.running) return
    clearPairing()
    pairProcess.command = ["omarchy-connect", "pair", "--json"]
    pairProcess.running = true
  }

  function clearPairing() {
    qrRows = []
    qrSize = 0
    pairUrl = ""
    pairExpires = ""
    pairSecondsLeft = 0
  }

  function setWritable(device, writable) {
    if (!device) return
    runAction(["omarchy-connect", "devices", writable ? "allow" : "readonly", device.id],
              writable ? device.name + " can type" : device.name + " is read-only")
  }

  function rename(device, name) {
    if (!device || !name) return
    runAction(["omarchy-connect", "devices", "rename", device.id, name], "Renamed to " + name)
  }

  function revoke(device) {
    if (!device) return
    runAction(["omarchy-connect", "devices", "revoke", device.id],
              device.revokeVerb === "Block" ? device.name + " blocked" : device.name + " revoked")
  }

  function unblock(device) {
    if (!device) return
    runAction(["omarchy-connect", "devices", "unblock", device.id], device.name + " unblocked, read-only")
  }

  function startDaemon() {
    runAction(["systemctl", "--user", "enable", "--now", "omarchy-connect"], "Daemon started")
  }

  function stopDaemon() {
    runAction(["systemctl", "--user", "disable", "--now", "omarchy-connect"], "Daemon stopped")
  }

  function copyPairUrl() {
    if (!pairUrl) return
    Quickshell.execDetached(["bash", "-c", "printf %s " + shellQuote(pairUrl) + " | wl-copy"])
    flash("Pairing link copied")
  }

  function shellQuote(value) {
    return "'" + String(value).replace(/'/g, "'\\''") + "'"
  }

  function runAction(command, successMessage) {
    if (actionProcess.running) return
    actionProcess.successMessage = successMessage
    actionProcess.command = command
    actionProcess.running = true
  }

  function flash(message) {
    actionStatus = message
    statusTimer.restart()
  }

  Timer {
    id: statusTimer
    interval: 2500
    onTriggered: root.actionStatus = ""
  }

  Timer {
    id: refreshTimer
    interval: root.refreshIntervalSec * 1000
    running: true
    repeat: true
    triggeredOnStart: true
    onTriggered: root.refresh()
  }

  // The pairing code expires on its own, so the countdown is what tells someone
  // the QR on screen has gone stale rather than leaving them scanning a code
  // the daemon will refuse.
  Timer {
    interval: 1000
    running: root.qrSize > 0
    repeat: true
    onTriggered: {
      root.pairSecondsLeft = Model.secondsUntil(root.pairExpires)
      if (root.pairSecondsLeft <= 0) root.clearPairing()
    }
  }

  Process {
    id: whichProcess
    running: false
    command: []
    onExited: function(exitCode) {
      root.checkedInstall = true
      root.installed = exitCode === 0
      if (root.installed) root.refresh()
    }
  }

  Process {
    id: statusProcess
    running: false
    command: []
    stdout: StdioCollector { id: statusOut; waitForEnd: true }
    onExited: function(exitCode) {
      var payload = null
      try {
        payload = JSON.parse(String(statusOut.text || ""))
      } catch (e) {
        // A non-zero exit with unparseable output means the tailnet is not
        // usable. status still prints a report in that case, so an empty parse
        // is a real failure rather than the expected unhappy path.
        root.problem = "Could not read daemon status"
        return
      }
      if (!payload) return
      root.node = String(payload.node || "")
      root.url = String(payload.url || "")
      root.certsAvailable = payload.certsAvailable === true
      root.port = parseInt(payload.port, 10) || 0
      root.serving = payload.serving === true
      root.problem = String(payload.problem || "")
    }
  }

  Process {
    id: devicesProcess
    running: false
    command: []
    stdout: StdioCollector { id: devicesOut; waitForEnd: true }
    stderr: StdioCollector { id: devicesErr; waitForEnd: true }
    onExited: function(exitCode) {
      if (exitCode !== 0) {
        var err = String(devicesErr.text || "").trim().split("\n")[0]
        if (err.indexOf("unknown command") !== -1) {
          root.binaryOutdated = true
          root.lastError = ""
          return
        }
        root.binaryOutdated = false
        root.lastError = err || "devices exited " + exitCode
        return
      }
      root.binaryOutdated = false
      try {
        root.devices = Model.buildDevices(JSON.parse(String(devicesOut.text || "[]")))
        root.lastError = ""
      } catch (e) {
        root.lastError = "Could not parse the device list"
      }
    }
  }

  Process {
    id: healthProcess
    running: false
    command: []
    onExited: function(exitCode) { root.daemonRunning = exitCode === 0 }
  }

  Process {
    id: pairProcess
    running: false
    command: []
    stdout: StdioCollector { id: pairOut; waitForEnd: true }
    stderr: StdioCollector { id: pairErr; waitForEnd: true }
    onExited: function(exitCode) {
      if (exitCode !== 0) {
        root.lastError = String(pairErr.text || "").trim().split("\n")[0] || "Could not start pairing"
        return
      }
      var payload = null
      try {
        payload = JSON.parse(String(pairOut.text || ""))
      } catch (e) {
        root.lastError = "Could not parse the pairing response"
        return
      }

      var matrix = Model.parseQrMatrix(payload.matrix)
      if (matrix.size === 0) {
        // Refusing to draw beats drawing a code that cannot scan: a broken QR
        // looks like a broken camera, and nobody debugs that quickly.
        root.lastError = "The QR code came back malformed"
        return
      }

      root.qrRows = matrix.rows
      root.qrSize = matrix.size
      root.pairUrl = String(payload.url || "")
      root.pairExpires = String(payload.expires || "")
      root.pairSecondsLeft = Model.secondsUntil(root.pairExpires)
      root.lastError = ""
    }
  }

  Process {
    id: actionProcess
    property string successMessage: ""
    running: false
    command: []
    stderr: StdioCollector { id: actionErr; waitForEnd: true }
    onExited: function(exitCode) {
      if (exitCode === 0) {
        root.flash(actionProcess.successMessage)
        root.lastError = ""
      } else {
        root.lastError = String(actionErr.text || "").trim().split("\n")[0] || "Command failed"
      }
      root.refresh()
    }
  }
}
