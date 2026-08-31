# omarchy-plugin-connect

Attach to the tmux and coding-agent sessions running on your Omarchy machine
from your phone — or from another Omarchy machine — over a direct connection
when there is one, and over Tailscale when there is not.

```
  phone browser                         another Omarchy box
       │                                        │
       │  https://tower.tail1234.ts.net         │
       └────────────────┬───────────────────────┘
                        ▼
              omarchy-connectd            ← Go daemon, one static binary
                ├── PWA (xterm.js)
                ├── session list ──────► tmux
                ├── agent state  ──────► claude / codex / opencode
                └── auth: tailnet identity | paired device token
                        ▲
                        │  settings, pairing QR, device revocation
                lucas.connect             ← Omarchy shell plugin (bar widget + panel)
```

> **Status: step 1 of 7 done.** The daemon serves over the tailnet with real
> TLS and refuses anyone Tailscale will not vouch for — verified end to end on
> a live tailnet, under systemd, with a publicly-verified certificate chain.
> There are no sessions yet: the only route is `/healthz`. `serve` and `status`
> work today; `pair` and `devices` are step 3. See [Roadmap](#roadmap).

## Why

Coding-agent sessions are long-running and bursty. An agent works for ten
minutes, then needs one word from you. Being away from the desk should not
mean that word waits an hour. The existing options are all worse than they
need to be:

- **SSH from a phone terminal app** works, but you supply your own client, your
  own key management, your own session discovery, and mobile keyboards make
  `Ctrl-C` and `Esc` a chore.
- **Screen sharing** ships pixels for something that is already text.
- **Nothing at all** is what most people pick, so the agent sits idle.

This ships the whole path: a daemon on the device, a mobile web UI it serves
itself, an identity model that treats your own machines as your own machines,
and an Omarchy plugin so the settings live where every other Omarchy setting
lives.

## The connection ladder

The requirement is "direct when possible, Tailscale when not." The design
satisfies it by leaning on the fact that **Tailscale is already that ladder**:

| Tier | Path | When | Origin |
|---|---|---|---|
| 0 | Tailscale, direct | Peers can reach each other — same LAN, or successful NAT hole-punch | `https://<host>.<tailnet>.ts.net` |
| 1 | Tailscale, DERP relay | Hole-punching fails (symmetric NAT, hostile guest wifi) | same |
| 2 | Plain LAN, no Tailscale | Opt-in, for a phone with no Tailscale installed | `http://<lan-ip>:7433` |

Tiers 0 and 1 are the *same origin*. That is the point. Tailscale picks the
direct UDP path on its own when your phone is on the house wifi, and silently
falls back to a relay when you are on cellular — but the URL, the TLS
certificate, the installed PWA, and the stored device token never change. One
origin means one PWA install and one token store, and moving between wifi and
cellular mid-session is a websocket reconnect rather than a re-pair.

Tier 2 exists because "my phone does not have Tailscale on it" is a real
situation, and it is honest about its costs: an HTTP origin is not a secure
context, so no PWA install, no service worker, and no web push. It is off by
default and gated behind pairing.

**TLS on the tailnet is real TLS — once you enable it.** `tailscale cert`
issues a Let's Encrypt certificate for the MagicDNS name, so there is no
self-signed certificate to click through and no private CA to install on a
phone: the two things that make every other self-hosted remote-terminal setup
unpleasant on iOS.

This is confirmed working on this tailnet: a genuine Let's Encrypt certificate
(ECDSA P-256) for `omarchy-starfighter.tail18c58.ts.net`, 90-day validity.
HTTPS certificates are off by default on a new tailnet and are enabled once, in
the admin console under **DNS → HTTPS Certificates**.

**The daemon never shells out to `tailscale cert`, and never writes a
certificate to disk.** Tailscale's `LocalClient.GetCertificate` is shaped to
drop straight into `tls.Config.GetCertificate`, which means fetch, cache, and
renewal-before-expiry all happen inside the TLS handshake path. A 90-day
certificate otherwise implies a renewal timer, a file to keep in sync, and a
class of "expired three weeks ago on a machine nobody logged into" bug. Wiring
the handshake to the LocalAPI deletes all of it. The first handshake after
startup pays the fetch, so the daemon warms it during `serve` rather than making
the first visitor wait.

If certificates are ever unavailable, the daemon still serves HTTP over the
tailnet and reports the degradation in `status` and in the plugin. That path is
not *insecure* — WireGuard encrypts every byte end to end regardless — but the
browser cannot know that, and an `http://` origin is not a secure context, so
the PWA install, the service worker, and web push are all lost. It is a
fallback, not a supported configuration.

## Identity

Two ways in, deliberately different.

**Tailnet peers identify themselves.** A request arriving on the tailnet
listener is resolved through the Tailscale LocalAPI (`WhoIs`) to a tailnet
user. A peer owned by the same user as the device is admitted with no pairing
step at all — this is what makes the **Omarchy → Omarchy** case a matter of
opening a URL. Peers belonging to other tailnet members are refused unless
explicitly allowed in the plugin.

**Tagged nodes are never auto-admitted**, whatever the tailnet. A tagged node —
a CI runner, a subnet router, a Kubernetes operator — is owned by an ACL tag
rather than by a person, so "same user" is not a question that has an answer for
it. `WhoIs` reports its tags and no user profile, and that is treated as a
refusal rather than as a missing field to fall back on. A tagged node that
should reach a shell can be paired by hand like any other device.

**Everything else pairs.** The plugin panel shows a QR code carrying the URL
plus a short-lived single-use code. The phone posts the code and receives a
long-lived device token. Tokens are named, listed, and revocable from the
plugin — revocation is immediate and kills live websockets.

**Devices carry a capability, not a binary.** A newly paired device starts at
`read`: it can list sessions and watch a terminal, and it cannot type. Promoting
it to `write` is a deliberate act at the desktop. These sessions have a shell
and an agent in them; a phone left on a café table should not be one tap from
`rm -rf`.

## Sessions

tmux is the transport; agent awareness is a layer on top.

Sessions are tmux sessions, which means detach-and-reattach already works, the
session you attach to from your phone is the same one on the monitor in front
of you, and a plain shell is as attachable as an agent. Attaching allocates a
PTY running `tmux attach`, bridged to `xterm.js` over a websocket.

On top of that, the daemon watches what is actually running in each pane —
`#{pane_current_command}` plus a walk of the process tree — and recognises
`claude`, `codex`, and `opencode`. Recognised sessions get:

- a state badge in the list: *working*, *awaiting input*, *awaiting approval*, *idle*
- a web push notification when a session enters an awaiting state, so the phone
  tells you the agent needs a word instead of you checking
- approve / deny / interrupt as buttons rather than as keystrokes you have to
  find on a mobile keyboard
- a persistent key bar — `Esc`, `Tab`, `Ctrl-C`, arrows — because those keys are
  the actual reason phone terminals are miserable

State detection is a heuristic over pane content and process state. It is
allowed to be wrong; the terminal underneath is always the ground truth, and
nothing in the UI is gated on the badge being right.

## Layout

```
omarchy-plugin-connect/
├── manifest.json            plugin manifest — MUST stay at the repo root
├── plugin/                  QML: bar widget, settings panel, model
├── cmd/omarchy-connect/     single Go binary: `serve`, `pair`, `devices`, `status`
├── internal/
│   ├── server/              HTTP, websocket, PTY bridge
│   ├── session/             tmux enumeration and attach
│   ├── agent/               agent detection and state inference
│   ├── auth/                pairing, device tokens, Tailscale WhoIs
│   ├── transport/           tailnet and LAN listeners, `tailscale cert`
│   ├── push/                VAPID web push
│   └── config/              ~/.config/omarchy/connect/config.json
├── web/                     the PWA
└── packaging/               PKGBUILD, systemd user unit
```

The manifest sits at the root because `omarchy plugin add` clones a repo
straight into `~/.config/omarchy/plugins/<id>/` and expects to find
`manifest.json` there. One repo therefore holds both halves: the shell plugin
that Omarchy installs, and the daemon source that it does not.

## Install

Prerequisites: `tmux`, `go` (build only), and `tailscale` for anything past
tier 2 — with **HTTPS certificates enabled** on the tailnet (admin console,
**DNS → HTTPS Certificates**) for the PWA to install, and the Tailscale
**operator set to your user** (`sudo tailscale set --operator=$USER`) so the
daemon can reach the LocalAPI without root.

```bash
# 1. the daemon
git clone https://github.com/vanvonlj/omarchy-plugin-connect.git
cd omarchy-plugin-connect
make install                            # builds, installs the binary + user unit
systemctl --user enable --now omarchy-connect

# 2. the shell plugin (not built yet — roadmap step 4)
omarchy plugin add https://github.com/vanvonlj/omarchy-plugin-connect.git --enable
```

`make install` deliberately targets `~/.local` and a systemd **user** unit, never
root. The Tailscale operator grant is per-user, and it is what makes `WhoIs` and
`GetCertificate` reachable over the LocalAPI socket — so running as the user is
what makes the daemon work, not a compromise. It also declines to give a
shell-adjacent service privileges it has no use for.

`omarchy plugin add` deliberately never runs code, install hooks, or sudo — so
the two steps stay two steps. Installing the plugin alone gets you a bar widget
that tells you the daemon is missing and shows you the command to fix it.

## Use

```bash
omarchy-connect status          # listeners, tier, paired devices, sessions
omarchy-connect serve           # foreground; the unit does this for you
omarchy-connect pair            # print a pairing URL + QR to the terminal
omarchy-connect devices         # list paired devices
omarchy-connect devices revoke <name>
```

Everything above is also in the plugin panel, which is where it is meant to be
used from. The CLI is the source of truth the QML calls into, so the two can
never disagree.

## Security

- The daemon never listens on `0.0.0.0`. Listeners are bound to the tailnet
  address and, only if enabled, one specific LAN address.
- The tailnet listener trusts Tailscale's identity assertion, not a header a
  client can set.
- LAN pairing codes are single-use and short-lived; device tokens are long-lived
  but individually revocable, and revocation closes live connections.
- New devices are read-only until promoted from the desktop.
- No inbound port is opened on your router, and no third-party server sees your
  terminal bytes. DERP relays, when used, carry WireGuard-encrypted traffic they
  cannot read.
- The shell plugin runs unsandboxed inside `omarchy-shell`, like every Omarchy
  plugin. It is deliberately thin: it renders state and shells out to
  `omarchy-connect`. The privileged surface is the daemon, which is reviewable
  on its own.

## Roadmap

1. ~~**Daemon skeleton** — config, `serve`, tailnet listener with LocalAPI certs, health endpoint~~ ✅
2. **Sessions** — tmux enumeration, websocket PTY attach, `xterm.js` client
3. **Identity** — Tailscale WhoIs admission, LAN pairing + device tokens, capabilities
4. **Plugin** — bar widget, settings panel, pairing QR, device management
5. **Agent awareness** — detection, state badges, approve/deny, key bar
6. **Push** — VAPID, subscription storage, awaiting-state notifications
7. **Packaging** — PKGBUILD, systemd user unit, `make install`

## License

MIT.
