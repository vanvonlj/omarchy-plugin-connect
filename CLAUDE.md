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
- Manual: `omarchy-connect serve` in a terminal, connect from a phone on the
  tailnet, and from a second Omarchy box, and confirm both admission paths.
- The Omarchy → Omarchy path is easy to break and easy to forget to test,
  because it is the one that needs no pairing. Test it on every auth change.

## Committing

Conventional commits (`feat:`, `fix:`, `docs:`, `refactor:`), matching the
prevailing style in the user's other repos. Do not commit or push unless asked.
