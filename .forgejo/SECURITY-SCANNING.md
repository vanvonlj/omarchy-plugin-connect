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
| `PASSED — but deps was not actually covered` | no lockfile found, so nothing was checked. **Uncovered, not clean.** |
| `CANARY FAILED` | the scan is broken, not clean. Never read it as a pass. |

That last row is the important one. Every tool here **fails open**: semgrep with
no rules, osv-scanner with no network, trivy with no checks bundle and
betterleaks with a stale binary all exit 0 and print nothing. The canary scans
deliberately-bad code first and fails if any tool calls it clean, which is the
only thing separating "nothing found" from "nothing ran".

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
allowlist at all; and the default rules do **not** match
`postgresql://user:pass@host/db`, which is why `.betterleaks.toml` here carries
a `database-dsn-credentials` rule — that gap once hid four real leaks where only
the Kubernetes `stringData` copies of the same password were reported.

## When CI runs it

| workflow | when | gate |
|---|---|---|
| `secret-scan.yml` | every push, every branch | **fails on any secret** |
| `code-scan.yml` | pull requests + weekly | fails on a CRITICAL dependency CVE |

The weekly run is dispatched from a CronJob in the cluster, not a `schedule:`
trigger — a `schedule:` stops Forgejo registering the workflow on any
non-default branch, which would silently kill PR scanning.
