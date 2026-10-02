#!/usr/bin/env python3
# MANAGED FILE — do not edit in this repository.
# Source of truth: lukejv-dev/homelab-platform → ops/forgejo-ci/files/scan.py
# Edit it there and re-run `ops/forgejo-ci/rollout.py apply --yes`. A local edit
# here is reported as drift by `rollout.py check` and overwritten on the next apply.
"""One command for all four scanners, run identically on a laptop and in CI.

    scan.py                     everything, whole tree
    scan.py --diff              only what this branch introduced vs origin/main
    scan.py --diff HEAD~3       ... vs any ref
    scan.py --only secrets      one layer
    scan.py --history           secrets across all git history
    scan.py --format markdown   CI-flavoured output
    scan.py --target HOST/OWNER/REPO   name this run for scan-console
    scan.py --no-report         never report this run to scan-console

REPORTING TO SCAN-CONSOLE IS OPTIONAL AND CANNOT CHANGE THE VERDICT. With
SCAN_CONSOLE_URL and SCAN_CONSOLE_TOKEN both set in the environment, a
whole-tree run is POSTed to the console AFTER the verdict line is printed, with
a 5-second budget. Console down, slow or erroring prints one warning line on
stderr and nothing else: stdout's report and the exit code are exactly what
they would have been with reporting off. The token is read from the
environment only, never a flag (shell history, `ps`). The target defaults to
SCAN_CONSOLE_TARGET, else the `origin` remote as host/owner/repo. --diff and
--history runs are never reported — see report_to_console(). The report names
the commit and branch (GITHUB_SHA/GITHUB_REF_NAME, else git), and stdout ends
with a link to the scan in the console. Neither variable
set, which is how CI runs, means no reporting and no output about it.

WHY THIS IS ONE FILE AND NOT FOUR WORKFLOW STEPS. A local run has to predict the
pipeline, or people stop trusting whichever one is more annoying. So the gate,
the severity mapping and the canaries live here, the workflows just call it, and
`rollout.py` syncs this exact file into every repo. There is no second copy of
the rules to drift.

WHAT EACH LAYER ACTUALLY COVERS — read this before trusting a clean result:

  secrets  betterleaks. Working tree, the commit range in --diff, or all of
           history with --history. Its defaults do NOT flag a password in a
           connection string — that gap hid four real leaks once — so every
           run adds the rules in betterleaks-default.toml beside this file,
           on top of the repo's own .betterleaks.toml if it has one. See
           secrets_config().
  deps     osv-scanner against lockfiles. A repo with no lockfile is reported
           as UNCOVERED, not clean — see scan_deps().
  iac      trivy config. Kubernetes, Helm, Dockerfile, Terraform. trivy has
           no --baseline-commit, so in --diff mode this layer filters its own
           findings to files the change touched — see touched_paths().
  sast     opengrep against the MIT ruleset in sast-rules.yml. Catches
           dangerous API usage — shell execution on a variable, pickle on
           untrusted bytes — and, with --taint-intrafile, tracks taint ACROSS
           FUNCTIONS within one file. `req.query.id` reaching SQL through a
           helper in the same file is caught; semgrep CE missed exactly that,
           which is why this layer moved off it (measured 2026-09-11).
           Still NOT cross-FILE: a source in a.py reaching a sink in b.py is
           invisible. That is opengrep's paid-tier equivalent everywhere.

EVERY TOOL HERE FAILS OPEN. opengrep with no rules, osv-scanner with no network,
trivy with no checks bundle and betterleaks with a stale binary all exit 0 and
print nothing. That is why --canary runs by default; think hard before turning the canary off.
"""

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import tomllib
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

# Pinned. An unpinned bump changes what counts as a finding and can turn a repo
# red with no commit having changed. Keep in step with the CI installer.
VERSIONS = {
    "betterleaks": "1.1.2",
    "osv-scanner": "2.5.1",
    "trivy": "0.74.0",
    "opengrep": "1.30.0",
}

SEVERITY_ORDER = ["CRITICAL", "HIGH", "MEDIUM", "LOW", "INFO"]
LAYERS = ["secrets", "deps", "iac", "sast"]

# Where the tools might be, beyond PATH. betterleaks is commonly left behind by
# a Rafter install; it is a standalone binary and needs nothing from Rafter.
EXTRA_BINS = [Path.home() / ".rafter/bin", Path.home() / ".local/bin"]

NEEDS = {"secrets": "betterleaks", "deps": "osv-scanner",
         "iac": "trivy", "sast": "opengrep"}

# opengrep has NO rules registry — p/default and friends resolve against
# semgrep.dev and simply do not exist here. Rules ship beside this file and are
# synced with it by rollout.py. A missing file is fatal rather than empty: an
# empty ruleset scans clean, which is the exact fail-open this tool exists to
# prevent.
RULES = Path(__file__).resolve().parent / "sast-rules.yml"

# Connection-string password rules every repo gets (SCAN-87), synced beside
# this file the same way. See secrets_config().
SECRETS_DEFAULT = Path(__file__).resolve().parent / "betterleaks-default.toml"


class LayerError(Exception):
    """A layer could not run. It fails the gate: a scanner that errored prints
    nothing, which reads exactly like a scanner that found nothing."""


class Finding:
    __slots__ = ("layer", "severity", "ident", "title", "location", "remediation")

    def __init__(self, layer, severity, ident, title, location, remediation=None):
        self.layer = layer
        self.severity = severity if severity in SEVERITY_ORDER else "INFO"
        self.ident = ident
        self.title = title
        self.location = location
        # What to do about it, for scan-console only. Never part of the title:
        # fingerprint() hashes the title, so guidance there would change the
        # identity of every finding the day the guidance changed.
        self.remediation = remediation


def find_tool(name):
    p = shutil.which(name)
    if p:
        return p
    for d in EXTRA_BINS:
        c = d / name
        if c.is_file() and os.access(c, os.X_OK):
            return str(c)
    return None


def run(cmd, **kw):
    """Run a command, never raising. Returns (rc, stdout, stderr)."""
    kw.setdefault("capture_output", True)
    kw.setdefault("text", True)
    try:
        p = subprocess.run(cmd, **kw)
        return p.returncode, p.stdout or "", p.stderr or ""
    except (OSError, subprocess.SubprocessError) as e:
        return 127, "", str(e)


def load_json(path):
    """Parse a scanner's JSON, treating a null document as empty.

    None of these tools write `[]` for "nothing found" — they write `null`, and
    `.get(k, [])` returns None for a key that exists with a null value.
    betterleaks and osv-scanner both do it. Assume it for the next one too.
    """
    try:
        with open(path) as f:
            return json.load(f) or {}
    except Exception:
        return {}


def lst(obj, key):
    return (obj or {}).get(key) or []


def rel(p, root):
    """Paths relative to the scan root — absolute ones are unreadable in a
    terminal and differ between a laptop and a CI workspace, which makes local
    and pipeline output impossible to compare."""
    if not p:
        return "?"
    try:
        return os.path.relpath(str(p), str(root))
    except ValueError:
        return str(p)


# --------------------------------------------------------------------------
# Canaries — prove each tool detects known-bad before believing a clean result
# --------------------------------------------------------------------------

CANARY_PY = 'import subprocess\ndef r(c):\n    subprocess.call("echo " + c, shell=True)\n'
CANARY_DOCKERFILE = "FROM alpine:3.21\nRUN echo hi\n"

CANARY_LOCK = json.dumps({
    "name": "canary", "version": "1.0.0", "lockfileVersion": 3, "requires": True,
    "packages": {
        "": {"name": "canary", "version": "1.0.0",
             "dependencies": {"lodash": "4.17.11"}},
        "node_modules/lodash": {"version": "4.17.11"},
    },
}, indent=2)


def canary_key():
    """A private-key block, ASSEMBLED AT RUNTIME rather than written out.

    Two reasons, both learned the hard way. Written as a literal, this string
    would be flagged by the very scanner it exists to test — so this file would
    permanently fail its own repo's secret scan, and the pre-commit hook refuses
    to write it in the first place. Splitting the armour markers across a
    concatenation keeps the literal out of the file while producing the exact
    bytes betterleaks needs to see at runtime. Verified: the split form is not
    flagged, the assembled form is.

    A private key is the canary rather than an API-key shape because it is not
    vendor-specific (a rule-set update is unlikely to drop it) and it is not on
    anyone's known-fake allowlist — the canonical AWS example key IS allowlisted,
    and betterleaks reports it clean, which would make a silently useless canary.
    """
    begin = "-----BEGIN RSA PRIVATE" + " KEY-----"
    end = "-----END RSA PRIVATE" + " KEY-----"
    body = "MIIEowIBAAKCAQEAx7Vn8kQpZlKcTdWmGpYqRfNbHjLvXsAoEuIyCwMdRgTnPbVk"
    return f"{begin}\n{body}\n{end}\n"


def canary(tools, layers, quiet):
    """Fail loudly if a tool reports known-bad code as clean."""
    problems = []
    with tempfile.TemporaryDirectory() as d:
        d = Path(d)
        (d / "canary.py").write_text(CANARY_PY)
        (d / "id_rsa").write_text(canary_key())
        (d / "package-lock.json").write_text(CANARY_LOCK)
        (d / "Dockerfile").write_text(CANARY_DOCKERFILE)
        out = d / "out.json"

        if "secrets" in layers:
            _, so, _ = run([tools["betterleaks"], "dir", str(d), "--no-banner",
                            "--redact", "--report-format", "json",
                            "--report-path", "-", "--exit-code", "0"])
            try:
                n = len(json.loads(so) or [])
            except Exception:
                n = 0
            if n == 0:
                problems.append("betterleaks found no secret in a private-key block")

        if "deps" in layers:
            run([tools["osv-scanner"], "scan", "source", "-L",
                 str(d / "package-lock.json"), "--format", "json",
                 "--output-file", str(out)])
            doc = load_json(out)
            n = sum(len(p.get("vulnerabilities") or [])
                    for r in lst(doc, "results") for p in lst(r, "packages"))
            if n == 0:
                problems.append("osv-scanner found nothing against lodash 4.17.11, "
                                "which has six advisories — no reachable data source")

        if "iac" in layers:
            run([tools["trivy"], "config", str(d), "--quiet", "--format", "json",
                 "--output", str(out)])
            doc = load_json(out)
            n = sum(len(lst(r, "Misconfigurations")) for r in lst(doc, "Results"))
            if n == 0:
                problems.append("trivy found no misconfiguration in a Dockerfile "
                                "running as root — its checks bundle did not load")

        if "sast" in layers:
            run([tools["opengrep"], "scan", "--config", str(RULES),
                 "--taint-intrafile", "--quiet", "--json", "--output", str(out),
                 str(d / "canary.py")])
            if len(lst(load_json(out), "results")) == 0:
                problems.append("opengrep found nothing in a shell=True subprocess "
                                f"— its rules did not load from {RULES}")

    if problems:
        sys.stderr.write("\nCANARY FAILED — this is not a clean scan, it is a "
                         "broken one:\n")
        for p in problems:
            sys.stderr.write(f"  - {p}\n")
        sys.stderr.write("\nA scanner that cannot see deliberately bad code tells "
                         "you nothing about real code.\n")
        sys.exit(2)
    if not quiet:
        print(f"canary ok ({', '.join(sorted(layers))})")


# --------------------------------------------------------------------------
# Scanners
# --------------------------------------------------------------------------

def secrets_config(root, tmp):
    """The --config for betterleaks: the default rules, plus the repo's own.

    No .betterleaks.toml: the default file as-is. With one: a temp copy of the
    repo's file with its `useDefault = true` swapped for `path = <default>`
    (or that `[extend]` appended, if the file has none).
    The default file extends betterleaks' built-ins itself, so the chain is
    repo -> default -> built-ins, and the repo's file stays on top. That
    direction is load-bearing: betterleaks drops a top-level `[[allowlists]]`
    that sits one extend below the top (measured on worldmonitor-homelab, whose
    targetRules allowlists all stopped applying), and those allowlists are how
    a repo accepts a documented example.

    A repo file that already extends by path is used as-is if that path is the
    default file, and refused otherwise: betterleaks follows two levels of
    extend and silently ignores a third, which would drop its built-ins.

    Raises LayerError rather than falling back to no config: the fallback
    would scan without these rules and still say PASSED."""
    if not SECRETS_DEFAULT.is_file():
        raise LayerError(f"default config not found at {SECRETS_DEFAULT} — "
                         "restore it from ops/forgejo-ci/files/")
    own = Path(root) / ".betterleaks.toml"
    if not own.is_file():
        return SECRETS_DEFAULT
    try:
        text = own.read_text()
        parsed = tomllib.loads(text)
    except (OSError, ValueError) as e:
        raise LayerError(f".betterleaks.toml is unreadable: {e}")
    extend = parsed.get("extend") or {}
    if extend.get("path"):
        # By name, not full path: the local `scan` may run homelab-platform's
        # copy against a repo that extends its own synced .forgejo/ copy.
        if Path(extend["path"]).name == SECRETS_DEFAULT.name:
            return own
        raise LayerError(
            f".betterleaks.toml extends {extend['path']!r}, so the default "
            "rules cannot be layered under it (betterleaks drops a third level "
            "of extend silently). Extend .forgejo/betterleaks-default.toml "
            "instead, or use `[extend] useDefault = true`")
    target = str(SECRETS_DEFAULT)
    if "extend" not in parsed:
        # No [extend]: betterleaks ran the repo's rules alone. The default
        # brings its built-ins along — more detection, never less.
        text += f"\n[extend]\npath = {json.dumps(target)}\n"
    else:
        # `useDefault = true` on its own line or inside an inline
        # `extend = {...}`. The parse below is the real check, so a match in
        # a comment cannot pass for a swap.
        text = re.sub(r"\buseDefault[ \t]*=[ \t]*true\b",
                      f"path = {json.dumps(target)}", text)
    try:
        swapped = tomllib.loads(text).get("extend") or {}
    except ValueError:
        swapped = {}
    if swapped.get("path") != target or swapped.get("useDefault") is not None:
        raise LayerError(".betterleaks.toml has an [extend] table with no "
                         "`useDefault = true` for scan.py to swap for the default "
                         "rules (it is false or absent). Set `useDefault = true`; "
                         "scan.py will not quietly turn betterleaks' built-in "
                         "rules back on")
    wrapped = Path(tmp) / "betterleaks.toml"
    wrapped.write_text(text)
    return wrapped


def scan_secrets(tools, root, diff_ref, history, tmp):
    if history:
        cmd = [tools["betterleaks"], "git", str(root)]
    elif diff_ref:
        cmd = [tools["betterleaks"], "git", str(root),
               "--log-opts", f"{diff_ref}..HEAD"]
    else:
        cmd = [tools["betterleaks"], "dir", str(root)]
    cmd += ["--no-banner", "--redact", "--report-format", "json",
            "--report-path", "-", "--exit-code", "0"]
    # Standalone betterleaks does NOT read .rafterignore — that was a Rafter
    # invention. Its own config is .betterleaks.toml (gitleaks-compatible), and
    # without it the allowlist silently stops applying and every previously
    # accepted false positive comes back as a CRITICAL.
    cmd += ["--config", str(secrets_config(root, tmp))]
    # cwd=root: betterleaks resolves `[extend] path` against the working
    # directory, not the config file, so a repo-relative path needs this.
    # --exit-code 0 means leaks never set rc; nonzero is betterleaks failing,
    # e.g. a missing or unparseable extend target, with nothing on stdout.
    rc, so, se = run(cmd, cwd=str(root))
    try:
        if rc != 0:
            raise ValueError
        raw = json.loads(so) or []
    except ValueError:
        why = re.sub(r"\x1b\[[0-9;]*m", "", se).strip().splitlines()
        raise LayerError(f"betterleaks exited {rc}: "
                         f"{why[-1] if why else 'unparseable output'}")
    findings = []
    for f in raw:
        loc, line = f.get("File", "?"), f.get("StartLine")
        loc = rel(loc, root)
        findings.append(Finding(
            "secrets", "CRITICAL", f.get("RuleID", "secret"),
            f.get("Description") or f.get("RuleID", "secret"),
            f"{loc}:{line}" if line else loc))
    return findings, None


def version_key(v):
    """Sort key for a package version, numeric-aware: 1.0.9 < 1.0.10.

    A letter run sorts below the end of the version, which sorts below a
    number, so 1.0.0-rc.1 < 1.0.0 < 1.0.1."""
    # ponytail: one ordering for every ecosystem, not each one's own rules
    # (PEP 440 post-releases and epochs, Debian `~` sort wrong). Upgrade path:
    # per-ecosystem comparators if a hint is ever seen naming a wrong version.
    return [(2, int(t)) if t.isdigit() else (0, t)
            for t in re.findall(r"\d+|[A-Za-z]+", str(v or ""))] + [(1, "")]


def fixed_version(pkg, ids, vulns):
    """The version of `pkg` that fixes every advisory in `ids` that has a
    published fix, above the installed one; None if none has one.

    Per advisory, its lowest `fixed` above the installed version: an
    advisory with backport branches fixes 1.2.5 AND 2.0.3, and a 2.0.0
    install is not fixed by downgrading. Across the group, the highest of
    those: the ids are usually aliases of one vulnerability, but when they
    are distinct advisories the lowest would leave the others open. An id
    with no fix is passed over — an alias often lacks the range data its
    twin has, and "no fix published" would then be wrong for the group.

    This package only — an advisory lists siblings too (lodash's names
    lodash-es), and their fix versions mean nothing here. GIT ranges are
    skipped: their events are commit hashes, not versions."""
    name, have = pkg.get("name"), version_key(pkg.get("version"))
    best = []
    for v in vulns:
        if v.get("id") not in ids:
            continue
        fixes = [e["fixed"]
                 for a in lst(v, "affected")
                 if (a.get("package") or {}).get("name") == name
                 for r in lst(a, "ranges") if r.get("type") != "GIT"
                 for e in lst(r, "events")
                 if e.get("fixed") and version_key(e["fixed"]) > have]
        if fixes:
            best.append(min(fixes, key=version_key))
    return max(best, key=version_key) if best else None


def osv_source_scan(tools, root, out, extra=()):
    """One osv-scanner pass over root. Returns (findings, set of lockfile paths).

    --all-packages is load-bearing: without it osv-scanner emits a `results`
    entry only for a source that HAS a vulnerability, so a clean lockfile is
    indistinguishable from no lockfile and every clean run reads UNCOVERED.
    """
    run([tools["osv-scanner"], "scan", "source", "-r", "--allow-no-lockfiles",
         "--all-packages", *extra, "--format", "json", "--output-file",
         str(out), str(root)])
    doc = load_json(out)
    findings, sources = [], set()
    for r in lst(doc, "results"):
        src = (r.get("source") or {}).get("path") or ""
        if src:
            sources.add(src)
        for p in lst(r, "packages"):
            info = p.get("package") or {}
            for g in lst(p, "groups"):
                try:
                    score = float(g.get("max_severity") or 0)
                except ValueError:
                    score = 0.0
                sev = ("CRITICAL" if score >= 9 else "HIGH" if score >= 7
                       else "MEDIUM" if score >= 4 else "LOW")
                ids = g.get("ids") or ["?"]
                fix = fixed_version(info, ids, lst(p, "vulnerabilities"))
                findings.append(Finding(
                    "deps", sev, ids[0],
                    f"{info.get('name','?')} {info.get('version','')} "
                    f"({info.get('ecosystem','?')}) CVSS {score:.1f}",
                    rel(src, root),
                    f"Upgrade {info.get('name','?')} to {fix} or later" if fix
                    else "No fixed version published"))
    return findings, sources


def scan_deps(tools, root, out):
    """Dependency CVEs.

    Reports UNCOVERED rather than clean when no lockfile was found. "Nothing to
    scan" and "nothing wrong" are different answers, and osv-scanner's own
    --allow-no-lockfiles collapses them into a cheerful exit 0.

    THE --no-ignore RETRY (CNVYR-156, SCAN-111). osv-scanner honours .gitignore,
    and it honours it from directories ABOVE the scan root as well. So scanning
    a path that some ancestor repository ignores walks exactly one directory,
    finds no lockfile, and reports UNCOVERED with a package-lock.json in plain
    sight. The case that matters: a git worktree under a gitignored
    `.worktrees/`, which is where every agent works, so this is the DEFAULT
    path and not an edge case. It is not about the worktree `.git` being a
    file — a worktree outside the ignored directory scans fine.

    The retry only fires when the first pass found NOTHING, because
    --no-ignore is otherwise actively wrong: in a normal checkout it walks
    node_modules and every sibling worktree, and reports the same advisory once
    per copy. A repo with no lockfile of its own also takes the retry, so its
    result is filtered through root's OWN .gitignore: a sibling worktree's
    lockfile under .worktrees/ is not this tree's coverage. Only an ancestor's
    ignore is the bug, and from inside a real worktree that one does not apply.
    """
    findings, sources = osv_source_scan(tools, root, out)
    if not sources:
        findings, sources = osv_source_scan(tools, root, out, ["--no-ignore"])
        rc, so, _ = run(["git", "-C", str(root), "check-ignore", "-z", "--stdin"],
                        input="\0".join(sorted(sources)))
        if rc == 0:   # 1 = none ignored; 128 = not a repo, nothing to filter by
            ignored = set(so.split("\0")) - {""}
            sources -= ignored
            findings = [f for f in findings
                        if f.location not in {rel(i, root) for i in ignored}]
    note = None
    if not sources:
        note = ("no lockfile found, so NOTHING was checked for dependency "
                "vulnerabilities — uncovered, not clean")
    return findings, note


def touched_paths(root, diff_ref):
    """Absolute paths this change touched: committed since diff_ref, staged,
    unstaged, and untracked-but-not-ignored.

    `trivy config` has no --baseline-commit, unlike betterleaks and opengrep, so
    the iac layer has to do its own diffing. Filtering by path is sound here in
    a way it would not be for sast: a misconfiguration finding is a pure
    function of one file's content, so a file the change did not touch cannot
    have gained one. (A rule-bundle bump can change the answer, but that is a
    trivy version change, not a diff.)

    Returns None if git cannot answer, which leaves the layer unfiltered — noisy
    rather than silently empty.
    """
    rc, top, _ = run(["git", "-C", str(root), "rev-parse", "--show-toplevel"])
    if rc != 0 or not top.strip():
        return None
    top = Path(top.strip())
    paths = set()
    # --full-name on ls-files is load-bearing: run from a subdirectory it
    # otherwise prints paths relative to CWD ("velero/x.yaml"), while
    # `diff --name-only` already prints them relative to the repo top
    # ("rke2/velero/x.yaml"). Joining the cwd-relative form onto `top` yields a
    # path that does not exist, so every untracked file silently drops out of
    # the touched set — and a brand-new misconfigured manifest sails through the
    # gate reporting "Nothing found".
    for cmd in (["git", "-C", str(root), "diff", "--name-only", diff_ref],
                ["git", "-C", str(root), "ls-files", "--others",
                 "--exclude-standard", "--full-name"]):
        rc, so, _ = run(cmd)
        if rc != 0:
            return None
        for line in so.splitlines():
            if line.strip():
                paths.add((top / line.strip()).resolve())
    return paths


def base_iac_counts(tools, root, diff_ref, touched):
    """{(target, rule): count} for the BASE version of each touched file.

    trivy has no baseline, so we build one: check the pre-change content of
    exactly the touched files out into a temp tree and scan that. Returns None
    if the base cannot be materialised, which leaves findings unsubtracted —
    noisy rather than silently empty, same failure direction as everywhere else.
    """
    rc, top, _ = run(["git", "-C", str(root), "rev-parse", "--show-toplevel"])
    if rc != 0 or not top.strip():
        return None
    top = Path(top.strip())
    counts = {}
    with tempfile.TemporaryDirectory() as tmp:
        tmp = Path(tmp)
        wrote = False
        for abs_path in touched:
            try:
                rel_to_top = abs_path.relative_to(top)
            except ValueError:
                continue
            rc, blob, _ = run(["git", "-C", str(root), "show",
                               f"{diff_ref}:{rel_to_top.as_posix()}"])
            if rc != 0:          # added by this change: no base, all findings new
                continue
            dst = tmp / rel_to_top
            dst.parent.mkdir(parents=True, exist_ok=True)
            dst.write_text(blob)
            wrote = True
        if not wrote:
            return {}
        out = tmp / "_base.json"
        run([tools["trivy"], "config", str(tmp), "--severity", "HIGH,CRITICAL",
             "--quiet", "--format", "json", "--output", str(out)])
        doc = load_json(out)
        for r in lst(doc, "Results"):
            # Key by ABSOLUTE repo path, not by trivy's Target.
            #
            # trivy reports Target relative to the directory it was handed. The
            # temp tree mirrors the repo from the GIT TOP, so these Targets are
            # top-relative — but the head scan runs `trivy config <root>`, and
            # root is the CLI's path argument, which defaults to cwd and need
            # not be the git top. `cd rke2 && scan --diff` makes the head emit
            # "velero/velero-values.yaml" while the base holds
            # "rke2/velero/velero-values.yaml", nothing subtracts, and every
            # pre-existing finding in a touched file reports as new — the
            # permanently-red gate this whole function exists to prevent,
            # reintroduced for any scan root that is not the repo root.
            #
            # Both sides resolve to the same absolute path, so they cannot drift.
            target = r.get("Target", "?")
            try:
                key_path = (top / target).resolve()
            except (OSError, ValueError):
                continue
            for m in lst(r, "Misconfigurations"):
                key = (key_path, m.get("ID", "?"))
                counts[key] = counts.get(key, 0) + 1
    return counts


def scan_iac(tools, root, out, diff_ref):
    run([tools["trivy"], "config", str(root), "--severity", "HIGH,CRITICAL",
         "--quiet", "--format", "json", "--output", str(out)])
    doc = load_json(out)
    # In --diff mode the gate fails HARD on any new HIGH/CRITICAL. Without this
    # filter trivy rescans the whole tree every run, every pre-existing finding
    # counts as newly introduced, and the diff gate can never pass — the
    # permanently-red check the gate comment below warns against.
    touched = touched_paths(root, diff_ref) if diff_ref else None
    # Touching a file is not the same as breaking it. Filtering to touched files
    # alone would report that file's whole pre-existing backlog as newly
    # introduced — measured: pinning authelia's image tag reported 3 HIGH
    # findings that were identical on the base ref. So subtract what the base
    # version of those same files already had, per (file, rule), by count.
    base = base_iac_counts(tools, root, diff_ref, touched) if touched else None
    findings = []
    for r in lst(doc, "Results"):
        target = r.get("Target", "?")
        try:
            abs_target = (Path(root) / target).resolve()
        except (OSError, ValueError):
            continue
        if touched is not None and abs_target not in touched:
            continue
        for m in lst(r, "Misconfigurations"):
            rule = m.get("ID", "?")
            if base is not None and base.get((abs_target, rule), 0) > 0:
                base[(abs_target, rule)] -= 1   # already there before the change
                continue
            findings.append(Finding(
                "iac", (m.get("Severity") or "INFO").upper(),
                rule, m.get("Title", ""), target))
    return findings, None


SEMGREP_SEV = {"ERROR": "HIGH", "WARNING": "MEDIUM", "INFO": "LOW"}


def scan_sast(tools, root, out, diff_ref):
    # No --metrics flag: opengrep removed telemetry entirely and errors on it.
    cmd = [tools["opengrep"], "scan", "--config", str(RULES),
           "--taint-intrafile", "--quiet", "--json", "--output", str(out)]
    if diff_ref:
        cmd += ["--baseline-commit", diff_ref]
    cmd.append(str(root))
    run(cmd)
    findings = []
    for x in lst(load_json(out), "results"):
        extra = x.get("extra") or {}
        message = (extra.get("message") or "").strip()
        findings.append(Finding(
            "sast", SEMGREP_SEV.get(extra.get("severity"), "LOW"),
            (x.get("check_id") or "?").split(".")[-1],
            message.split("\n")[0][:100],
            f"{rel(x.get('path'), root)}:"
            f"{(x.get('start') or {}).get('line')}",
            # The rule's whole message: the title is only its first line, and
            # the how-to-fix is usually below it. Redacted in build_payload().
            message or None))
    return findings, None


# --------------------------------------------------------------------------
# Output
# --------------------------------------------------------------------------

LAYER_TITLE = {
    "secrets": "Secrets",
    "deps": "Dependency vulnerabilities",
    "iac": "Infrastructure misconfiguration",
    "sast": "Static analysis",
}


def report(findings, notes, layers, fmt, diff_ref, limit=20):
    lines, md = [], fmt == "markdown"
    scope = f"changes since {diff_ref}" if diff_ref else "whole tree"
    lines.append(f"# Scan report ({scope})" if md else f"\nScan report — {scope}")

    for layer in layers:
        fs = sorted([f for f in findings if f.layer == layer],
                    key=lambda f: SEVERITY_ORDER.index(f.severity))
        t = LAYER_TITLE[layer]
        lines.append(f"\n## {t}" if md else f"\n{t}\n{'-' * len(t)}")
        if notes.get(layer):
            lines.append(f"**{notes[layer]}**" if md else f"!! {notes[layer]}")
        if not fs:
            if not notes.get(layer):
                lines.append("Nothing found.")
            continue
        counts = {s: sum(1 for f in fs if f.severity == s) for s in SEVERITY_ORDER}
        lines.append(", ".join(f"{v} {k}" for k, v in counts.items() if v))
        if md:
            lines += ["", "| Severity | Finding | Location |", "|---|---|---|"]
        for f in fs[:limit]:
            if md:
                lines.append(f"| {f.severity} | {f.ident} — {f.title} | {f.location} |")
            else:
                lines.append(f"  {f.severity:9}{f.location}")
                lines.append(f"           {f.ident} — {f.title}")
        if len(fs) > limit:
            lines.append(f"| … | {len(fs) - limit} more | |" if md
                         else f"  … {len(fs) - limit} more")
    return "\n".join(lines)


# --------------------------------------------------------------------------
# Reporting to scan-console (ADR 0021 D3) — best-effort, after the verdict
# --------------------------------------------------------------------------
#
# Everything below runs after the verdict line is printed and the exit code is
# decided, and nothing it does can change either. A gate that answers
# differently when a server is down is the fail-open this whole file exists to
# catch, so every failure here is one line on stderr and nothing more.

REPORT_TIMEOUT = 5.0       # seconds, for the whole request — not per socket op
REPORT_TITLE_MAX = 200
REMEDIATION_MAX = 2000     # a rule message is a paragraph, not a document
LOCAL_HOSTS = {"localhost", "127.0.0.1", "::1"}

# The shapes the console refuses as unredacted (api/Validation/Validate.cs).
# scan.py never keeps a scanner's matched text — betterleaks runs with
# --redact and scan_secrets() keeps only RuleID, Description, File and
# StartLine — so this is a backstop, not the control: an opengrep rule message
# can interpolate a matched metavariable into a sast title, and the rules file
# is not frozen.
_RAW_SECRET = re.compile(
    r"sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}"
    r"|AKIA[0-9A-Z]{12,}|xox[baprs]-[A-Za-z0-9-]{12,}|AIza[0-9A-Za-z_\-]{30,}"
    r"|-----BEGIN[ A-Z]*PRIVATE KEY-----")


def redact(text):
    # No prefix kept: even eight characters of a key are more than the
    # console needs, and "no matched secret text is sent" has to stay true.
    return _RAW_SECRET.sub("[redacted]", text or "")


def finding_path(location):
    """A location without its line (and column): `a.py:12:5` -> `a.py`.

    Per layer, as emitted above: secrets `path:line` (bare `path` when
    betterleaks gives no line), sast `path:line` (`path:None` if opengrep omits
    `start`), deps the lockfile path, iac trivy's Target — the last two never
    carry a line."""
    return re.sub(r"(:(\d+|None))+$", "", location or "")


def fingerprint(f):
    """Identity of a finding ACROSS runs — review.py's fingerprint(), plus the
    layer and rule so two layers can never collide on one file and title.

    No line number, on purpose: the same problem shifted three lines is the same
    problem, and a fingerprint that moved with it would resolve and re-open
    every finding above an edit. A deps title's trailing CVSS is dropped for
    the same reason — an advisory being re-scored is not a new vulnerability.

    Two hits of one rule in one file therefore share a fingerprint and report
    as one finding. That is the price of not keying on the line."""
    title = redact(f.title).strip().lower()
    if f.layer == "deps":
        title = re.sub(r"\s+cvss\s+[\d.]+$", "", title)
    key = f"{f.layer}\n{f.ident or ''}\n{finding_path(f.location)}\n{title}"
    # sha256 for the reason review.py gives: a scanner flags sha1 on sight.
    return hashlib.sha256(key.encode()).hexdigest()[:12]


def normalise_remote(url):
    """A git remote as `host/owner/repo`, or None if it is not one.

    Userinfo is stripped wherever it appears — `https://user:TOKEN@host/…` is a
    common way to push from CI, and a credential in the target would be stored
    and displayed as the identity of every finding. So is a query string
    (`?private_token=`). The port is dropped so an ssh and an https remote of
    the same repository name the same target. A local-path or file:// remote
    has no stable identity and returns None rather than a filesystem path,
    which the console refuses anyway."""
    s = (url or "").strip()
    if not s:
        return None
    m = re.match(r"^([A-Za-z][A-Za-z0-9+.-]*)://(.*)$", s)
    if m:
        if m.group(1).lower() == "file":
            return None
        rest = re.split(r"[?#]", m.group(2), 1)[0]
        # Cut at the LAST '@': a password containing '/' or '@' must not
        # survive by confusing the authority/path split.
        rest = rest.rsplit("@", 1)[-1]
        authority, _, path = rest.partition("/")
        if authority.startswith("["):                      # [::1]:22
            host = authority[1:].split("]", 1)[0]
        else:
            host = authority.split(":", 1)[0]
    else:
        # scp-style: [user@]host:owner/repo. A '/' before the ':' means a
        # local path, and a one-letter "host" is a Windows drive.
        m = re.match(r"^(?:[^/]*@)?([^@/:]+):(.+)$", s)
        if not m or len(m.group(1)) < 2:
            return None
        host, path = m.group(1), m.group(2)
    path = path.strip("/")
    if path.endswith(".git"):
        path = path[:-4].rstrip("/")
    host = host.strip().lower()
    if not host or not path or ".." in path or "\\" in path:
        return None
    return f"{host}/{path}"


def resolve_target(explicit, root):
    for t in (explicit, os.environ.get("SCAN_CONSOLE_TARGET")):
        if t and t.strip():
            t = t.strip()
            # A pasted remote URL gets the same credential stripping as a
            # derived one; a plain host/owner/repo is used as given.
            return normalise_remote(t) if "://" in t or "@" in t else t
    rc, so, _ = run(["git", "-C", str(root), "remote", "get-url", "origin"])
    return normalise_remote(so) if rc == 0 else None


def git_head(root):
    """{"commit", "ref"} for the console, each key left out — never "" — when
    unknown: a detached HEAD has no branch, a tarball no commit. CI's own
    GITHUB_SHA / GITHUB_REF_NAME win, because a CI checkout is often detached.
    Exec form, never a shell; the ref goes as read and the console validates
    it. git failing or missing just drops the key — run() never raises."""
    head = {}
    for key, var, cmd in (
            ("commit", "GITHUB_SHA", ["rev-parse", "--verify", "-q", "HEAD"]),
            ("ref", "GITHUB_REF_NAME", ["symbolic-ref", "--short", "-q", "HEAD"])):
        v = (os.environ.get(var) or "").strip()
        if not v:
            rc, so, _ = run(["git", "-C", str(root)] + cmd)
            v = so.strip() if rc == 0 else ""
        if v:
            head[key] = v
    return head


def build_payload(target, started_at, rc, layers, findings, notes, canaried,
                  head=None):
    """The console's ReportScanRequest, and nothing else: it rejects unknown
    fields (JsonUnmappedMemberHandling.Disallow), so an extra key is a 400.

    No `evidence`: nothing here holds matched text to redact, and "not sent" is
    stronger than "redacted". A layer is True only if it ran, had no coverage
    note AND passed the canary — the console resolves every open finding in a
    layer that "ran" and was not reported, so claiming coverage for an
    uncovered or unverified layer would mark live findings fixed."""
    out, seen = [], set()
    for f in findings:
        fp = fingerprint(f)
        if fp in seen:        # first wins; see fingerprint() on why dupes exist
            continue
        seen.add(fp)
        out.append({
            "fingerprint": fp,
            "layer": f.layer,
            "severity": f.severity,
            "title": redact(f.title)[:REPORT_TITLE_MAX] or f.ident or "?",
            "ruleId": f.ident,
            "location": redact(f.location),
            # redact() before the cap, so a cut can never leave half a key
            # unrecognisable to it.
            "remediation": (redact(f.remediation)[:REMEDIATION_MAX]
                            if f.remediation else None),
        })
    return {
        "target": target,
        "startedAt": started_at,
        "exitStatus": "passed" if rc == 0 else "failed",
        "layers": {l: bool(canaried and not notes.get(l)) for l in layers},
        "toolVersions": {NEEDS[l]: VERSIONS[NEEDS[l]] for l in layers},
        "findings": out,
        **(head or {}),
    }


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """urllib follows a 301/302 on a POST as a GET and carries the
    Authorization header with it — to whatever host, over whatever scheme, the
    Location names. The console never redirects this endpoint, so one is an
    error, and the token stays where it was sent."""

    def redirect_request(self, *a, **kw):
        return None


def post_report(url, token, payload, timeout):
    """(status, parsed body) or raise. Runs in a thread so `timeout` bounds the
    WHOLE exchange: urllib's own timeout is per socket operation, does not cover
    DNS, and a server dripping one byte a second would never trip it."""
    req = urllib.request.Request(
        url, data=json.dumps(payload).encode(), method="POST",
        headers={"Authorization": f"Bearer {token}",
                 "Content-Type": "application/json", "Accept": "application/json"})
    opener = urllib.request.build_opener(_NoRedirect)
    box = {}

    def go():
        try:
            with opener.open(req, timeout=timeout) as r:
                box["ok"] = (r.status, r.read(65536))
        except BaseException as e:            # re-raised on the main thread
            box["err"] = e

    t = threading.Thread(target=go, daemon=True)
    t.start()
    t.join(timeout)
    if t.is_alive():
        raise TimeoutError(f"timed out after {timeout:g}s")
    if "err" in box:
        raise box["err"]
    status, body = box["ok"]
    try:
        doc = json.loads(body or b"null")
    except ValueError:
        doc = None
    return status, doc if isinstance(doc, dict) else {}


def _why(e):
    if isinstance(e, urllib.error.HTTPError):
        msg = ""
        try:
            doc = json.loads(e.read(65536) or b"null")
            if isinstance(doc, dict) and doc.get("error"):
                msg = str(doc["error"])
        except Exception:
            pass
        msg = " ".join(msg.split())
        if len(msg) > 200:
            msg = msg[:200] + "…"
        return f"HTTP {e.code}" + (f": {msg}" if msg else "")
    if isinstance(e, urllib.error.URLError):
        return str(e.reason)
    return f"{type(e).__name__}: {e}" if str(e) else type(e).__name__


def report_to_console(args, root, layers, findings, notes, rc, started_at,
                      canaried):
    """Report this run to scan-console. Never raises, never touches rc; every
    way it can go wrong is exactly one line on stderr.

    Not reached on a canary failure: canary() calls sys.exit(2) before any
    layer runs, so there is nothing to report and the console never hears of
    a broken scanner as a clean run."""
    token = (os.environ.get("SCAN_CONSOLE_TOKEN") or "").strip()

    def warn(msg):
        if len(token) >= 6:      # a shorter one would mangle ordinary words
            msg = msg.replace(token, "***")
        sys.stderr.write(f"warning: scan-console: {msg}\n")

    try:
        base = (os.environ.get("SCAN_CONSOLE_URL") or "").strip()
        if args.no_report or (not base and not token):
            return
        if not base or not token:
            warn(f"{'SCAN_CONSOLE_TOKEN' if base else 'SCAN_CONSOLE_URL'} is "
                 "not set, so this run was not reported (set both, or neither)")
            return
        # Whole-tree runs only. The console treats a layer that ran as having
        # seen everything, and resolves every open finding in it that this
        # report does not repeat. A --diff run sees only the change and
        # --history sees a different thing again, so reporting either would
        # mark every finding outside that scope fixed. One line on stderr,
        # not silence: someone who set both variables expects the console to
        # move, and should learn why it did not.
        if args.diff or args.history:
            if not args.quiet:
                sys.stderr.write("scan-console: not reported — only whole-tree "
                                 "runs are (this was --diff/--history)\n")
            return
        u = urllib.parse.urlsplit(base)
        if u.scheme not in ("http", "https") or not u.hostname:
            warn("SCAN_CONSOLE_URL must be an http(s) URL; not reported")
            return
        if u.scheme == "http" and u.hostname.lower() not in LOCAL_HOSTS:
            warn(f"refusing to send the token over plain http to {u.hostname}; "
                 "use https (http is allowed only for localhost). Not reported")
            return
        target = resolve_target(args.target, root)
        if not target:
            warn("no target — pass --target or set SCAN_CONSOLE_TARGET "
                 "(no usable `origin` remote to derive it from). Not reported")
            return
        payload = build_payload(target, started_at, rc, layers, findings,
                                notes, canaried, git_head(root))
        _, doc = post_report(base.rstrip("/") + "/scans/reported", token,
                             payload, REPORT_TIMEOUT)
    except Exception as e:
        warn(f"not reported ({_why(e)}). The result above stands.")
        return

    if args.quiet:
        return
    try:
        sid = doc.get("id", "?")
        # SCAN-102's route, last on stdout so a CI log ends on it. Built from
        # the URL only; the token is never in it.
        link = (f"{base.rstrip('/')}/#/scans/{sid}"
                if isinstance(sid, int) and not isinstance(sid, bool) else None)
        new, reopened = doc.get("new"), doc.get("reopened")
        if not isinstance(new, list) or not isinstance(reopened, list):
            print(f"console: scan {sid} recorded")
            if link:
                print(f"console: {link}")
            return
        print(f"console: scan {sid} recorded — {len(new)} new, "
              f"{len(reopened)} reopened, {doc.get('resolved', 0)} resolved")
        local = {}
        for f in findings:
            local.setdefault(fingerprint(f), f)
        rows = [("new", fp) for fp in new] + [("reopened", fp) for fp in reopened]
        limit = 20
        for label, fp in rows[:limit]:
            f = local.get(fp)
            if f is None:
                print(f"  {label:9}{fp}")
                continue
            print(f"  {label:9}{f.severity:9}{f.location}")
            print(f"                    {f.ident} — {f.title}")
        if len(rows) > limit:
            print(f"  … {len(rows) - limit} more")
        if link:
            print(f"console: {link}")
    except Exception as e:
        warn(f"recorded, but the response was unreadable ({_why(e)})")


def main():
    # When the run began, for the console. Taken before anything slow so it
    # means the same thing on a laptop and in CI.
    started_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("path", nargs="?", default=".", help="directory to scan")
    ap.add_argument("--diff", nargs="?", const="origin/main", metavar="REF",
                    help="only what changed since REF (default origin/main). "
                         "Applies to secrets, SAST and IaC; deps is whole-tree "
                         "either way.")
    ap.add_argument("--history", action="store_true",
                    help="secrets: scan all git history, not the working tree")
    ap.add_argument("--only", action="append", choices=LAYERS, default=[],
                    metavar="LAYER", help=f"limit to {'/'.join(LAYERS)} (repeatable)")
    ap.add_argument("--format", choices=["text", "markdown"], default="text")
    ap.add_argument("--fail-cvss", type=float, default=9.0,
                    help="fail on a dependency CVE at or above this CVSS "
                         "(default 9.0; above 10 disables)")
    ap.add_argument("--fail-on", action="append", choices=LAYERS, default=[],
                    metavar="LAYER", help="also fail on any finding in LAYER")
    ap.add_argument("--allow-uncovered", action="store_true",
                    help="in --diff mode, do not fail when a layer covered "
                         "nothing at all (a repo with no lockfile anywhere). "
                         "The hole is still printed; you are accepting it.")
    ap.add_argument("--no-fail-new", action="store_true",
                    help="in --diff mode, report newly introduced HIGH/CRITICAL "
                         "findings instead of failing on them")
    ap.add_argument("--no-canary", action="store_true",
                    help="skip the detection self-test. Every tool here fails "
                         "OPEN, so this makes a clean result meaningless.")
    ap.add_argument("--quiet", action="store_true")
    ap.add_argument("--target", metavar="HOST/OWNER/REPO",
                    help="scan-console target for this run (default: "
                         "$SCAN_CONSOLE_TARGET, else the origin remote)")
    ap.add_argument("--no-report", action="store_true",
                    help="do not report this run to scan-console, even with "
                         "SCAN_CONSOLE_URL and SCAN_CONSOLE_TOKEN set")
    args = ap.parse_args()

    root = Path(args.path).resolve()
    layers = args.only or LAYERS

    tools, missing = {}, {}
    for layer in layers:
        binname = NEEDS[layer]
        p = find_tool(binname)
        if p:
            tools[binname] = p
        else:
            missing[binname] = VERSIONS[binname]
    # An absent ruleset is the fail-open this whole file exists to prevent:
    # opengrep with no rules exits 0 and prints nothing, which reads as clean.
    if "sast" in layers and not RULES.is_file():
        sys.stderr.write(
            f"error: SAST ruleset not found at {RULES}\n"
            "opengrep has no rules registry, so there is no remote fallback — "
            "without this file the sast layer would scan clean and mean\n"
            "nothing. Restore it from ops/forgejo-ci/files/sast-rules.yml, or "
            "narrow the run with --only.\n")
        return 2

    if missing:
        sys.stderr.write("missing scanner(s):\n")
        for n, v in missing.items():
            sys.stderr.write(f"  {n} (pinned {v})\n")
        sys.stderr.write("\nInstall them, or narrow the run with --only.\n")
        return 2

    diff_ref = args.diff
    if diff_ref:
        rc, _, _ = run(["git", "-C", str(root), "rev-parse", "--verify",
                        "--quiet", diff_ref])
        if rc != 0:
            sys.stderr.write(f"error: --diff ref {diff_ref!r} does not resolve. "
                             "Fetch it, or pass a ref that exists.\n")
            return 2

    if not args.no_canary:
        canary(tools, layers, args.quiet)

    findings, notes, broken = [], {}, []
    with tempfile.TemporaryDirectory() as tmp:
        out = Path(tmp) / "out.json"
        if "secrets" in layers:
            try:
                f, n = scan_secrets(tools, root, diff_ref, args.history, tmp)
            except LayerError as e:
                f, n = [], f"the secrets layer did NOT run — {e}"
                broken.append("secrets")
            findings += f
            notes["secrets"] = n
        if "deps" in layers:
            f, n = scan_deps(tools, root, out)
            findings += f
            notes["deps"] = n
        if "iac" in layers:
            f, n = scan_iac(tools, root, out, diff_ref)
            findings += f
            notes["iac"] = n
        if "sast" in layers:
            f, n = scan_sast(tools, root, out, diff_ref)
            findings += f
            notes["sast"] = n

    print(report(findings, notes, layers, args.format, diff_ref))

    # ---- the gate ---------------------------------------------------------
    # Whole-tree runs gate NARROWLY. Secrets are binary, so any hit fails.
    # Dependencies fail at CRITICAL, where a published fix almost always exists.
    # IaC and SAST only report: their first-run backlogs run to hundreds, and a
    # permanently red check is worth exactly as much as no check.
    #
    # --diff runs gate HARD, and that is not an inconsistency. In a diff there
    # IS no backlog — every finding was introduced by the change in front of
    # you. "Hundreds of pre-existing findings" is the entire reason to be lenient
    # whole-tree, and it does not apply here. So anything HIGH or CRITICAL that
    # this change introduced fails, which is the useful answer when the question
    # is "is the code I just wrote safe to commit".
    reasons = [f"the {layer} layer did not run" for layer in broken]
    if diff_ref:
        new_high = [f for f in findings
                    if f.severity in ("CRITICAL", "HIGH") and f.layer != "deps"]
        if new_high and not args.no_fail_new:
            by = {}
            for f in new_high:
                by[f.layer] = by.get(f.layer, 0) + 1
            reasons.append("this change introduces " + ", ".join(
                f"{n} {lay} finding(s)" for lay, n in sorted(by.items()))
                + " at HIGH or above")
    n_secrets = sum(1 for f in findings if f.layer == "secrets")
    if n_secrets:
        reasons.append(f"{n_secrets} secret(s) in the scanned range")
    crit = [f for f in findings
            if f.layer == "deps" and f.severity == "CRITICAL"]
    if crit and args.fail_cvss <= 10:
        reasons.append(f"{len(crit)} dependency vulnerabilit(y/ies) at "
                       f"CVSS >= {args.fail_cvss}")
    for layer in args.fail_on:
        n = sum(1 for f in findings if f.layer == layer)
        if n:
            reasons.append(f"{n} {layer} finding(s) (--fail-on {layer})")
    # A gate that could not check something must not report success, and the
    # one word an automated caller reads is PASSED: it opens the PR having
    # checked nothing (CNVYR-156). --diff only, because whole-tree is CI, and
    # half the org has no lockfile at all — failing there turns those repos red
    # for a hole nobody can fix. --allow-uncovered accepts it on purpose.
    uncovered = [k for k, v in notes.items() if v and k not in broken]
    if diff_ref and uncovered and not args.allow_uncovered:
        reasons.append(f"{', '.join(uncovered)} covered nothing — uncovered, "
                       "not clean (--allow-uncovered to accept)")

    print()
    if reasons:
        print("FAILED: " + "; ".join(reasons) + ".")
        rc = 1
    else:
        if uncovered:
            print(f"PASSED — but {', '.join(uncovered)} was not actually "
                  "covered, see the note above.")
        else:
            print("PASSED.")
        rc = 0

    # The verdict is printed and rc is final. Reporting cannot change either.
    # A --no-canary run reports every layer as not covered: its clean result is
    # unverified, and a layer the console believes ran resolves findings.
    sys.stdout.flush()
    report_to_console(args, root, layers, findings, notes, rc, started_at,
                      canaried=not args.no_canary)
    return rc


if __name__ == "__main__":
    sys.exit(main())
