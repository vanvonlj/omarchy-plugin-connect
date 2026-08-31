import QtQuick
import QtQuick.Controls
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui

Panel {
  id: root
  moduleName: "lucas.connect"
  ipcTarget: "lucas.connect"
  manageIpc: false

  property int cursorIndex: -1
  property bool cursorActive: false
  property string renamingId: ""

  readonly property color foreground: bar ? bar.foreground : Color.foreground
  readonly property color urgent: bar ? bar.urgent : Color.urgent
  readonly property color accent: Color.accent
  readonly property color dim: Qt.darker(foreground, 1.55)
  readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family
  readonly property color hoverFill: bar ? Style.hoverFillFor(bar.foreground, Color.accent) : "transparent"

  readonly property var devices: service.devices
  readonly property bool pairing: service.qrSize > 0
  readonly property bool healthy: service.installed && service.serving && service.problem === ""

  // The icon earns full brightness only when the daemon is actually serving.
  // A dim plug means "nothing is listening", which is the one thing worth
  // reading off the bar without opening anything.
  readonly property color barIconColor: {
    if (!service.installed || service.problem !== "") return urgent
    return service.serving ? barForeground : Qt.darker(barForeground, 1.7)
  }

  function selectedDevice() {
    if (cursorIndex < 0 || cursorIndex >= devices.length) return null
    return devices[cursorIndex]
  }

  function moveCursor(step) {
    if (devices.length === 0) return
    var i = cursorIndex + step
    if (i < 0) i = devices.length - 1
    if (i >= devices.length) i = 0
    cursorIndex = i
  }

  function ensureCursor() {
    if (cursorIndex < 0 || cursorIndex >= devices.length) cursorIndex = devices.length > 0 ? 0 : -1
  }

  onOpenedChanged: {
    if (opened) {
      service.refresh()
    } else {
      cursorActive = false
      renamingId = ""
      service.clearPairing()
      if (panelFlick) panelFlick.contentY = 0
    }
  }

  Service {
    id: service
    settings: root.settings
  }

  Connections {
    target: service
    function onDevicesChanged() { root.ensureCursor() }
  }

  IpcHandler {
    target: root.ipcTarget
    function open(): void { root.open() }
    function close(): void { root.close() }
    function toggle(): void { root.toggle() }
    function refresh(): string { service.refresh(); return "ok" }
    function pair(): string { root.open(); service.startPairing(); return "ok" }
    function url(): string { return service.url }
  }

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  BarIconButton {
    id: button
    anchors.fill: parent
    bar: root.bar
    tooltipText: {
      if (!service.installed) return "Connect: daemon not installed"
      if (service.problem !== "") return "Connect: " + service.problem
      if (!service.serving) return "Connect: nothing listening"
      return service.url !== "" ? "Connect: " + service.url : "Connect"
    }
    iconComponent: Component {
      Text {
        // A plug: this machine, reachable from elsewhere.
        text: "󰃪"
        color: root.barIconColor
        font.family: root.fontFamily
        font.pixelSize: Style.font.body
      }
    }
    onPressed: function(buttonCode) {
      if (buttonCode === Qt.MiddleButton) service.refresh()
      else root.toggle()
    }
  }

  KeyboardPanel {
    id: panel
    anchorItem: button
    owner: root
    bar: root.bar
    open: root.opened
    focusTarget: keyCatcher
    contentWidth: panel.fittedContentWidth(Style.space(400))
    contentHeight: panel.fittedContentHeight(column.implicitHeight + Style.space(10), Style.space(620))

    PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent
      onMoveRequested: function(dx, dy) {
        if (root.renamingId !== "") return
        if (!root.cursorActive) { root.cursorActive = true; root.ensureCursor(); return }
        if (dy !== 0) root.moveCursor(dy > 0 ? 1 : -1)
      }
      onActivateRequested: {
        if (root.renamingId !== "") return
        var d = root.selectedDevice()
        if (d && !d.blocked) service.setWritable(d, !d.canWrite)
      }
      onCloseRequested: {
        if (root.renamingId !== "") { root.renamingId = ""; return }
        if (root.pairing) { service.clearPairing(); return }
        root.close()
      }
      onTextKey: function(t) {
        if (root.renamingId !== "") return
        var key = String(t || "")
        var d = root.selectedDevice()
        if (key === "r") { service.refresh(); return }
        if (key === "p") { service.startPairing(); return }
        if (key === "n" && d) { root.renamingId = d.id; return }
        if (key === "x" && d) { service.revoke(d); return }
        if (key === "u" && d && d.blocked) { service.unblock(d); return }
        if (key === "y" && root.pairing) { service.copyPairUrl(); return }
      }

      Flickable {
        id: panelFlick
        anchors.fill: parent
        contentWidth: width
        contentHeight: column.implicitHeight
        clip: true
        boundsBehavior: Flickable.StopAtBounds
        flickableDirection: Flickable.VerticalFlick
        interactive: contentHeight > height
        ScrollBar.vertical: ScrollBar { policy: ScrollBar.AsNeeded }

        Column {
          id: column
          width: panelFlick.width
          spacing: Style.space(6)

          Item { width: 1; height: Style.space(2) }

          // ---------- daemon not installed ----------

          Column {
            visible: service.checkedInstall && !service.installed
            width: parent.width
            spacing: Style.space(4)

            Text {
              width: parent.width
              text: "The connect daemon is not installed."
              color: root.urgent
              wrapMode: Text.WordWrap
              font.family: root.fontFamily
              font.pixelSize: Style.font.bodySmall
            }
            Text {
              width: parent.width
              text: "Build and install it from the plugin checkout:\n    cd ~/.config/omarchy/plugins/lucas.connect && make install"
              color: root.dim
              wrapMode: Text.WordWrap
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }
          }

          // ---------- binary out of date ----------

          Column {
            visible: service.binaryOutdated
            width: parent.width
            spacing: Style.space(4)

            Text {
              width: parent.width
              text: "The installed daemon is older than this plugin."
              color: root.urgent
              wrapMode: Text.WordWrap
              font.family: root.fontFamily
              font.pixelSize: Style.font.bodySmall
            }
            Text {
              width: parent.width
              text: "Updating the plugin does not rebuild the binary. From the plugin checkout:\n    make install"
              color: root.dim
              wrapMode: Text.WordWrap
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }
          }

          // ---------- status ----------

          Column {
            visible: service.installed
            width: parent.width
            spacing: Style.space(3)

            Row {
              width: parent.width
              spacing: Style.space(6)

              Rectangle {
                width: Style.space(8)
                height: Style.space(8)
                radius: width / 2
                anchors.verticalCenter: parent.verticalCenter
                color: root.healthy ? root.accent : (service.problem !== "" ? root.urgent : root.dim)
              }

              Text {
                text: {
                  if (service.problem !== "") return "Not reachable"
                  if (service.serving) return service.daemonRunning ? "Serving" : "Serving (started by hand)"
                  return "Stopped"
                }
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
                font.bold: true
                anchors.verticalCenter: parent.verticalCenter
              }
            }

            Text {
              visible: service.url !== ""
              width: parent.width
              text: service.url
              color: root.dim
              elide: Text.ElideMiddle
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }

            // A tailnet without HTTPS certificates still carries the terminal,
            // so this is a warning rather than an error -- but it costs the PWA
            // install and push, which is most of the point on a phone.
            Text {
              visible: service.installed && service.problem === "" && !service.certsAvailable
              width: parent.width
              text: "No TLS: enable DNS → HTTPS Certificates in the Tailscale admin console. Until then there is no app install and no notifications."
              color: root.urgent
              wrapMode: Text.WordWrap
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }

            Text {
              visible: service.problem !== ""
              width: parent.width
              text: service.problem
              color: root.urgent
              wrapMode: Text.WordWrap
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }
          }

          PanelSeparator { visible: service.installed; width: parent.width }

          // ---------- actions ----------

          Row {
            visible: service.installed
            width: parent.width
            spacing: Style.space(6)

            Button {
              text: root.pairing ? "Cancel" : "Pair a device"
              bordered: true
              foreground: root.foreground
              accent: root.accent
              fontFamily: root.fontFamily
              onClicked: root.pairing ? service.clearPairing() : service.startPairing()
            }

            Button {
              text: service.daemonRunning ? "Stop" : "Start"
              bordered: true
              foreground: root.foreground
              accent: root.accent
              fontFamily: root.fontFamily
              onClicked: service.daemonRunning ? service.stopDaemon() : service.startDaemon()
            }
          }

          // ---------- pairing QR ----------

          Column {
            visible: root.pairing
            width: parent.width
            spacing: Style.space(5)

            Rectangle {
              id: qrCanvas
              anchors.horizontalCenter: parent.horizontalCenter
              // The quiet zone is baked into the matrix, so the white field
              // reaches the edge of the card and scanners see the full border.
              readonly property int moduleSize: Math.max(3, Math.floor(Style.space(240) / Math.max(1, service.qrSize)))
              width: moduleSize * service.qrSize
              height: width
              color: "#ffffff"
              radius: Style.cornerRadius > 0 ? Style.space(4) : 0

              Grid {
                anchors.centerIn: parent
                columns: service.qrSize

                Repeater {
                  model: service.qrSize * service.qrSize

                  Rectangle {
                    required property int index
                    readonly property int matrixRow: Math.floor(index / service.qrSize)
                    readonly property int matrixColumn: index % service.qrSize

                    width: qrCanvas.moduleSize
                    height: qrCanvas.moduleSize
                    color: service.qrRows[matrixRow].charAt(matrixColumn) === "1" ? "#111111" : "transparent"
                  }
                }
              }
            }

            Text {
              width: parent.width
              horizontalAlignment: Text.AlignHCenter
              text: service.pairSecondsLeft > 0
                    ? "Scan it — expires in " + service.pairSecondsLeft + "s"
                    : "Expired"
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }

            Text {
              width: parent.width
              horizontalAlignment: Text.AlignHCenter
              text: "The device arrives read-only. Press y to copy the link."
              color: root.dim
              wrapMode: Text.WordWrap
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }
          }

          PanelSeparator { visible: service.installed; width: parent.width }

          // ---------- devices ----------

          PanelSectionHeader {
            visible: service.installed
            width: parent.width
            text: "Devices"
          }

          Text {
            visible: service.installed && root.devices.length === 0
            width: parent.width
            horizontalAlignment: Text.AlignHCenter
            text: "No devices yet.\nPair one, or open the link from another machine on your tailnet."
            color: root.dim
            wrapMode: Text.WordWrap
            font.family: root.fontFamily
            font.pixelSize: Style.font.bodySmall
            topPadding: Style.space(10)
            bottomPadding: Style.space(10)
          }

          Column {
            id: listColumn
            width: parent.width
            spacing: Style.space(2)

            Repeater {
              model: root.devices

              Rectangle {
                id: deviceRow
                required property var modelData
                required property int index

                width: listColumn.width
                implicitHeight: rowContent.implicitHeight + Style.space(10)
                radius: Style.cornerRadius > 0 ? Style.space(4) : 0
                color: (root.cursorActive && root.cursorIndex === index) ? root.hoverFill : "transparent"

                MouseArea {
                  anchors.fill: parent
                  hoverEnabled: true
                  onEntered: { root.cursorActive = true; root.cursorIndex = index }
                  onClicked: root.cursorIndex = index
                }

                Column {
                  id: rowContent
                  anchors.left: parent.left
                  anchors.right: parent.right
                  anchors.verticalCenter: parent.verticalCenter
                  anchors.leftMargin: Style.space(6)
                  anchors.rightMargin: Style.space(6)
                  spacing: Style.space(3)

                  Row {
                    width: parent.width
                    spacing: Style.space(6)

                    Text {
                      visible: root.renamingId !== deviceRow.modelData.id
                      text: deviceRow.modelData.name
                      color: root.foreground
                      elide: Text.ElideRight
                      width: Math.min(implicitWidth, parent.width - toggle.width - Style.space(16))
                      font.family: root.fontFamily
                      font.pixelSize: Style.font.bodySmall
                      anchors.verticalCenter: parent.verticalCenter
                    }

                    TextField {
                      id: renameField
                      visible: root.renamingId === deviceRow.modelData.id
                      width: parent.width - toggle.width - Style.space(16)
                      text: deviceRow.modelData.name
                      anchors.verticalCenter: parent.verticalCenter
                      onVisibleChanged: if (visible) { forceActiveFocus(); selectAll() }
                      onAccepted: {
                        service.rename(deviceRow.modelData, text)
                        root.renamingId = ""
                        keyCatcher.forceActiveFocus()
                      }
                      Keys.onEscapePressed: {
                        root.renamingId = ""
                        keyCatcher.forceActiveFocus()
                      }
                    }

                    Item { width: 1; height: 1 }

                    ToggleSwitch {
                      id: toggle
                      anchors.verticalCenter: parent.verticalCenter
                      checked: deviceRow.modelData.canWrite
                      interactive: !deviceRow.modelData.blocked
                      foreground: root.foreground
                      accent: root.accent
                      onToggled: service.setWritable(deviceRow.modelData, !deviceRow.modelData.canWrite)
                    }
                  }

                  Row {
                    width: parent.width
                    spacing: Style.space(5)

                    Text {
                      text: deviceRow.modelData.capabilityLabel
                      color: deviceRow.modelData.blocked
                             ? root.urgent
                             : (deviceRow.modelData.canWrite ? root.accent : root.dim)
                      font.family: root.fontFamily
                      font.pixelSize: Style.font.caption
                    }

                    Text {
                      text: "·  " + deviceRow.modelData.kind + "  ·  " + deviceRow.modelData.lastSeen
                      color: root.dim
                      font.family: root.fontFamily
                      font.pixelSize: Style.font.caption
                    }
                  }
                }
              }
            }
          }

          PanelSeparator { visible: service.installed; width: parent.width }

          Text {
            visible: service.installed
            width: parent.width
            text: "j/k move · enter toggles typing · n rename · x " +
                  "revoke · u unblock · p pair · r refresh"
            color: root.dim
            wrapMode: Text.WordWrap
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
          }

          Text {
            visible: service.actionStatus !== "" || service.lastError !== ""
            width: parent.width
            text: service.lastError !== "" ? service.lastError : service.actionStatus
            color: service.lastError !== "" ? root.urgent : root.accent
            wrapMode: Text.WordWrap
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
          }

          Item { width: 1; height: Style.space(2) }
        }
      }
    }
  }
}
