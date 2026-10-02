# Security scanning in this repository

<!-- MANAGED FILE — do not edit here.
     Source of truth: lukejv-dev/homelab-platform → ops/forgejo-ci/files/
     Edit it there and re-run `ops/forgejo-ci/rollout.py apply --yes`.
     A local edit is reported as drift by `rollout.py check`. -->

**For agents and humans.** This repo is scanned by `.forgejo/scan.py`, the same
file CI runs. Everything is self-hosted: no account, no API key, nothing
metered, and no code leaves the network — only rule and CVE metadata come in.

## Run it before you commit

```bash
scan --diff          # only what THIS change introduced  ← use this by default
scan                 # whole tree
scan --only secrets  # one layer: secrets | deps | iac | sast
scan --history       # secrets across all git history
```

A git worktree parked under a gitignored `.worktrees/` is covered too: osv-scanner
honours ancestor `.gitignore` files, so `scan_deps()` retries with `--no-ignore`
when the first pass finds no lockfile.

If `scan` is not on PATH, `python3 .forgejo/scan.py --diff` is identical — the
wrapper only locates this file.

**Use `--diff` when reviewing code you or an agent just wrote.** It reports only
what the change introduced and gates hard on it: any new HIGH or CRITICAL fails.
Whole-tree runs are lenient because a repo carries a backlog of hundreds of
pre-existing findings; a diff has no backlog, so everything in it is yours.

## Reading the result

| output | meaning |
|---|---|
| `FAILED` | act before committing |
| `PASSED` | the gate passed — **not** "no findings"; IaC and SAST report without blocking on a whole-tree run |
| `FAILED: deps covered nothing` | `--diff` only: no lockfile found anywhere, so nothing was checked. **Uncovered, not clean.** Pass `--allow-uncovered` if this repo genuinely has no lockfile. |
| `PASSED — but deps was not actually covered` | the same hole, on a whole-tree run or accepted with `--allow-uncovered` |
| `CANARY FAILED` | the scan is broken, not clean. Never read it as a pass. |

That last row is the important one. Every tool here **fails open**: opengrep with
no rules, osv-scanner with no network, trivy with no checks bundle and
betterleaks with a stale binary all exit 0 and print nothing. The canary scans
deliberately-bad code first and fails if any tool calls it clean, which is the
only thing separating "nothing found" from "nothing ran".

## Reporting to scan-console (optional)

With `SCAN_CONSOLE_URL` and `SCAN_CONSOLE_TOKEN` both set, a **whole-tree** run
is also sent to scan-console, which tracks each finding across runs (new,
reopened, resolved). It happens after the verdict and cannot change it: console
down, slow or erroring is one `warning: scan-console:` line on stderr, and the
report and exit code are exactly what they would have been without it.

- The token is read from the environment only — never pass it as a flag.
- The target is `--target`, else `SCAN_CONSOLE_TARGET`, else the `origin`
  remote as `host/owner/repo` (credentials in the remote URL are stripped).
- `--diff` and `--history` runs are **not** reported: the console would read a
  partial scan as "everything else was fixed". `--no-report` turns it off.
- Plain `http://` is refused except to localhost. No matched secret text is
  sent — betterleaks runs with `--redact` and only rule, title and location go.

In CI only the weekly default-branch run reports, and only once the org has
variable `SCAN_CONSOLE_URL` and secret `SCAN_CONSOLE_TOKEN`. Pull-request runs
never report and never see the token.

## What it does NOT catch

Do not present a clean scan as proof the code is safe.

- **No cross-function taint tracking.** Semgrep's open rules catch dangerous API
  usage — `shell=True`, `pickle` on untrusted bytes, a container with no `USER`.
  They do **not** follow user input across functions, so `req.query.id`
  concatenated into a SQL string passes clean. Measured against a canary
  carrying both. Read a clean SAST result as *"no obvious dangerous calls"*.
- **Logic and authorization bugs** are invisible to all four tools. So is
  anything a test would catch — and conversely, **tests do not catch any of
  this**: a vulnerable dependency passes a perfect test suite.
- **Secrets in history** are only scanned with `--history`.

So on a diff touching SQL, shell, auth, file paths or deserialization, read it
yourself as well. The scanner is a floor, not a ceiling.

## Per-repo configuration (unmanaged — yours to edit)

| file | tool |
|---|---|
| `.betterleaks.toml` | secrets (gitleaks-compatible) |
| `.osv-scanner.toml` | dependencies |
| `.trivyignore` | IaC misconfiguration |
| `.semgrepignore` | SAST |

Every allowlist entry is a permanent hole in the scan. Record **why**, and never
quote the offending line in the ignore file — these files are themselves
scanned, so pasting the sample just relocates the finding.

Two things worth knowing about the secrets layer specifically: `.rafterignore`
is **not read by anything** any more, so a repo still relying on one has no
allowlist at all; and betterleaks' own rules do **not** match a password in a
connection string (`Server=db.internal;Password=<password>` or `postgresql://user:pass@host/db`) —
that gap once hid four real leaks where only the Kubernetes `stringData` copies
of the same password were reported. So every run adds the two rules in
`.forgejo/betterleaks-default.toml`. They deliberately skip a password whose
host is loopback (`localhost`, `127.0.0.1`, `::1`) or, in key=value strings
only (`Host=`, `Server=`, `Data Source=`…; never URIs), a listed compose/CI
container name (only `db` and `postgres`; a k8s Service literally named one
of those would be missed), plus
`__NAME__` render markers — so a clean scan on a password to `Host=postgres` is
an exemption, not a miss; the toml lists exactly what is skipped. With no `.betterleaks.toml` that file is
the config. With one, scan.py swaps your `useDefault = true` for
`path = <that file>` (or adds that `[extend]` if it has none), so your rules
and allowlists still apply on top. If your file already uses `[extend] path`,
point it at `.forgejo/betterleaks-default.toml` — any other path, or
`useDefault = false`, fails the secrets layer, as does a missing or
unparseable config. To accept a documented example such as a README's sample password,
allowlist it in your own `.betterleaks.toml`, never in the managed default.

## When CI runs it

| workflow | when | gate |
|---|---|---|
| `secret-scan.yml` | every push, every branch | **fails on any secret** |
| `code-scan.yml` | pull requests + weekly | fails on a CRITICAL dependency CVE |

The weekly run is dispatched from a CronJob in the cluster, not a `schedule:`
trigger — a `schedule:` stops Forgejo registering the workflow on any
non-default branch, which would silently kill PR scanning.
