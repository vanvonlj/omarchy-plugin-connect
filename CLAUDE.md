# CLAUDE.md

Guidance for working in this repo. Read [README.md](README.md) first for what
the project is; this file is about how to change it without breaking things.

## What this repo is

One repo, two artifacts:

- **`omarchy-connect`** — a Go daemon + CLI (one binary, subcommands) that runs
  on the Omarchy device, serves the PWA, and bridges browsers to tmux sessions.
- **`lucas.connect`** — an Omarchy shell plugin (QML/Quickshell) that provides
  the settings UI, pairing QR, and device management.

They ship together because `omarchy plugin add` clones a git repo, and the
plugin needs to live in one.

## Hard constraints

These are enforced by Omarchy, not by us. Violating one means the plugin
silently fails to load or refuses to install.

1. **`manifest.json` stays at the repo root.** `omarchy plugin add` clones into
   `~/.config/omarchy/plugins/<id>/` and reads `manifest.json` from the top
   level. Moving it into `plugin/` breaks installation.
2. **No symlinks anywhere in the repo.** `omarchy-plugin-validate` walks the
   whole tree and refuses any symlink outside `.git`. This rules out the obvious
   trick of symlinking built assets into place.
3. **`schemaVersion` must be the JSON number `1`.** The string `"1"` is
   rejected — the check is type-aware.
4. **Every declared kind needs its entry point.** `kinds: ["bar-widget"]`
   requires `entryPoints.barWidget`; `service` requires `entryPoints.service`.
   A kind without its entry point installs fine and then does nothing, so the
   validator refuses it up front.
5. **Entry-point paths are relative, contain no `..`, and must exist.**
   Subdirectories are fine — `"plugin/Panel.qml"` is valid.
6. **The `omarchy.*` id namespace is reserved.** Ours is `lucas.connect`.

Run the validator before every commit that touches the manifest or QML:

```bash
omarchy plugin validate .
```

## Gotchas specific to this repo

- **Saving any file under `~/.config/omarchy/plugins/` hot-reloads plugin code.**
  If you are developing against an installed checkout, a Go build in that
  directory will thrash the shell. Build in a working copy elsewhere (this
  repo, `~/Projects/omarchy-plugin-connect`) and install the binary out to
  `~/.local/bin`; keep the installed plugin a clean checkout.
- **Go is installed via mise, not system-wide** (`~/.local/share/mise/shims/go`,
  currently 1.27.0). It is on an interactive shell's PATH and is *not* on the
  PATH of a systemd user unit. That is fine — the daemon ships as a static
  binary and never invokes `go` at runtime — but nothing at runtime may start
  depending on the toolchain being reachable, and a PKGBUILD must depend on
  system `go` rather than assuming this shim.
- **Never bind a listener to `0.0.0.0`.** Bind the specific tailnet address, and
  the specific LAN address only when LAN mode is explicitly enabled. This is a
  security property stated in the README, not an implementation detail.
- **Do not invent a second config store.** `~/.config/omarchy/connect/config.json`
  is the only source of truth for daemon settings. The QML panel must read and
  write it by shelling out to `omarchy-connect`, never by parsing or writing
  the file itself. Bar-widget settings in `shell.json` are for presentation
  only (badge visibility, refresh interval).
- **Agent state detection is a heuristic and is allowed to be wrong.** Never
  gate a capability on it. The terminal is ground truth; the badge is a hint.
- **Deleting a tailnet device does not revoke it.** Tailscale keeps vouching for
  the node, so the record is recreated on its next request with a fresh id. That
  is why `Blocked` exists. Any new "remove this device" path must go through
  `Revoke`, which picks deletion or blocking by kind.
- **Capability checks must fail closed.** `EffectiveCapability` treats an
  unrecognised value, and any blocked device, as `read`. A hand-edited or
  future-versioned `devices.json` must never grant more than it names.
- **The device store is written by two processes** — the daemon touching
  LastSeen and the CLI changing capabilities from the panel. Every mutation is a
  read-modify-write under `flock`. A test removes the lock and shows 20
  concurrent registrations losing half their devices; keep it that way.
- **`pkill -f 'omarchy-connect serve'` kills your own shell** when the command
  line that runs it contains that string. Use `pkill -x omarchy-connect`.

## Testing the plugin on a live desktop

- **`omarchy-shell shell rescanPlugins` can crash the running shell.** Observed
  once: the reload put `omarchy.lock` into `lock-stranded: recovering`, which
  reached `FATAL: Tried to show lockscreen surfaces without active lock` and
  aborted Quickshell. It restarted itself, so the desktop recovered, but this is
  an upstream Omarchy fault worth knowing about before rescanning someone's live
  session -- especially while the machine is idle or locking.
- **`QS_DISABLE_FILE_WATCHER=1` is set in this session**, so saving a plugin
  file does *not* hot-reload it despite what the shell README says. `rescanPlugins`
  is the only reload path, which is unfortunate given the point above.
- **`grim` blocks forever when the display is asleep** (`hyprctl monitors -j`
  → `dpmsStatus: false`) and also while a panel holds a Wayland focus grab. Both
  look identical to a hung screenshot. Check DPMS before concluding anything is
  broken, and never wake the user's display to take one.
- **Verify a widget rendered, not just that it loaded.** A bar widget with no
  `implicitWidth`/`implicitHeight` lays out at 0x0: the plugin loads, its IPC
  answers, and nothing appears. `hyprctl layers -j` showing a real surface plus
  a clean journal is the evidence; `omarchy-shell lucas.connect open` returning
  0 is not.
- **`WhoIs` on a tagged node returns tags and no user profile.** Do not write
  the admission check as "compare UserID to ours" — a tagged node's absent or
  synthetic user must be an explicit refusal, not a comparison that happens to
  fall through. This tailnet has live tagged nodes (`tag:k8s`), so the case is
  reachable today, not hypothetical.
- **Get certificates from `LocalClient.GetCertificate`, not from `tailscale
  cert`.** It plugs directly into `tls.Config.GetCertificate` and handles fetch,
  cache, and renewal in the handshake path. Shelling out to the CLI means owning
  a renewal timer and a pair of files on disk for a 90-day certificate — a
  standing bug waiting for a machine nobody logs into. Warm the cert at `serve`
  so the first visitor does not pay the fetch.
- **Still treat a missing certificate as a reportable degraded state**, not a
  fatal startup error. Certs are working on this tailnet now, but an expired
  trial, a revoked toggle, or a tailnet that never enabled them are all real,
  and the terminal works fine over HTTP on the tailnet.
- **The daemon runs as a systemd *user* unit, not as root.** This works because
  `OperatorUser` is set to `lucas`, which is what makes the LocalAPI — `WhoIs`
  and `GetCertificate` both — reachable without sudo. That is a load-bearing
  precondition: if the operator is ever unset, admission and TLS both fail at
  once. Check it explicitly at startup and say so plainly rather than failing
  with a permissions error from two layers down.

## This machine's tailnet

Verified 2026-08-30, useful because every manual test names one of these.

| Node | Address | Identity | Role in testing |
|---|---|---|---|
| `omarchy-starfighter` | `100.98.170.119` | `luke-serv@outlook.com` | this device — the daemon host |
| `omarchy-hp` | `100.73.104.114` | same user, Linux | the **Omarchy → Omarchy** target |
| `iphone172` | `100.97.101.19` | same user, iOS | the phone target |
| `local-network-connector` | `100.73.191.125` | `tag:k8s` | the tagged node that must be **refused** |

MagicDNS suffix is `tail18c58.ts.net`, so this host is
`omarchy-starfighter.tail18c58.ts.net`. Several of these are offline most of
the time; bring one up rather than substituting a different test.

HTTPS certificates are enabled and verified: a Let's Encrypt ECDSA P-256 cert
issues for the MagicDNS name, single SAN, 90-day validity. The single SAN is
why tier 2 (plain LAN) can never be HTTPS — the certificate does not cover a
LAN IP, and nothing will make it.

## House style

The two existing personal plugins on this machine — `~/.config/omarchy/plugins/lucas.idle`
and `lucas.pr-review` — are the reference for QML structure: a `Service.qml` or
`Panel.qml` entry point, a plain-JS `Model.js` holding logic that does not need
QML, and a `README.md` for anything with a non-obvious UI.

Follow Omarchy's own conventions for anything shell-side:

- Shell scripts carry `# omarchy:summary=`, `# omarchy:group=`, `# omarchy:args=`
  header comments, as every script in `/usr/share/omarchy/bin` does.
- Commands are interactive when run bare and non-interactive when given
  arguments, with `--yes` skipping every prompt. Agents and scripts use the
  latter path.
- Comments explain *why*, not *what*. The Omarchy source is unusually good about
  this; match it. A comment that restates the line below it is noise.

Go code: standard library first, `internal/` for everything not meant to be
imported, no framework unless it earns its place. The daemon should stay a
single static binary with no runtime dependencies beyond `tmux` and the
`tailscale` CLI.

## Useful Omarchy surfaces

Discovered by reading `/usr/share/omarchy` — worth knowing before reinventing
any of it.

| Thing | Where |
|---|---|
| Plugin docs | `/usr/share/omarchy/shell/README.md` |
| Manifest schema (authoritative) | `/usr/share/omarchy/shell/services/PluginRegistry.qml` |
| Validator | `/usr/share/omarchy/bin/omarchy-plugin-validate` |
| Shell IPC | `omarchy-shell shell <ping\|summon\|call\|rescanPlugins\|listPlugins>` |
| First-party plugin examples | `/usr/share/omarchy/shell/plugins/` |
| QR generation prior art | `omarchy-capture-qr`, `omarchy-network-qr` |
| sshd/firewall prior art | `omarchy-setup-security-sshd` |
| tmux integration prior art | `omarchy-launch-terminal-tmux`, `omarchy-refresh-tmux` |

Omarchy version on this machine is `4.0.0.alpha`; the plugin system is new
enough that its schema may still move. Re-read the validator rather than
trusting this file if something stops loading.

## Testing

- `omarchy plugin validate .` — manifest and layout
- `go test ./...` — daemon
- Manual: `omarchy-connect serve` in a terminal, connect from `iphone172` and
  from `omarchy-hp`, and confirm both admission paths.
- The Omarchy → Omarchy path is easy to break and easy to forget to test,
  because it is the one that needs no pairing. Test it on every auth change.
- Every auth change also needs the negative test: `local-network-connector`
  (`tag:k8s`) must be refused. An admission bug that only shows up as "a tagged
  node got a shell" is not one you want to find in production.
- The tailnet listener can be built and unit-tested against a faked LocalAPI.
  Do not let the absence of a second running box block progress on it.

## Committing

Conventional commits (`feat:`, `fix:`, `docs:`, `refactor:`), matching the
prevailing style in the user's other repos. Do not commit or push unless asked.
