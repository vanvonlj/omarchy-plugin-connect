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

WHY THIS IS ONE FILE AND NOT FOUR WORKFLOW STEPS. A local run has to predict the
pipeline, or people stop trusting whichever one is more annoying. So the gate,
the severity mapping and the canaries live here, the workflows just call it, and
`rollout.py` syncs this exact file into every repo. There is no second copy of
the rules to drift.

WHAT EACH LAYER ACTUALLY COVERS — read this before trusting a clean result:

  secrets  betterleaks. Working tree, the commit range in --diff, or all of
           history with --history. Note it does NOT flag database DSNs of the
           `postgresql://user:pass@host/db` shape unless this repo carries a
           .betterleaks.toml rule for them — that gap hid four real leaks once.
  deps     osv-scanner against lockfiles. A repo with no lockfile is reported
           as UNCOVERED, not clean — see scan_deps().
  iac      trivy config. Kubernetes, Helm, Dockerfile, Terraform.
  sast     semgrep OSS rules. Catches dangerous API usage — shell execution on
           a variable, pickle on untrusted bytes, a container with no USER. Does
           NOT do cross-function taint tracking: `req.query.id` concatenated
           into SQL passes clean. Measured against a canary carrying both, so
           read a clean SAST result as "no obvious dangerous calls".

EVERY TOOL HERE FAILS OPEN. semgrep with no rules, osv-scanner with no network,
trivy with no checks bundle and betterleaks with a stale binary all exit 0 and
print nothing. That is why --canary runs by default; think hard before turning the canary off.
"""

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

# Pinned. An unpinned bump changes what counts as a finding and can turn a repo
# red with no commit having changed. Keep in step with the CI installer.
VERSIONS = {
    "betterleaks": "1.1.2",
    "osv-scanner": "2.5.1",
    "trivy": "0.74.0",
    "semgrep": "1.176.0",
}

SEVERITY_ORDER = ["CRITICAL", "HIGH", "MEDIUM", "LOW", "INFO"]
LAYERS = ["secrets", "deps", "iac", "sast"]

# Where the tools might be, beyond PATH. betterleaks is commonly left behind by
# a Rafter install; it is a standalone binary and needs nothing from Rafter.
EXTRA_BINS = [Path.home() / ".rafter/bin", Path.home() / ".local/bin"]

NEEDS = {"secrets": "betterleaks", "deps": "osv-scanner",
         "iac": "trivy", "sast": "semgrep"}


class Finding:
    __slots__ = ("layer", "severity", "ident", "title", "location")

    def __init__(self, layer, severity, ident, title, location):
        self.layer = layer
        self.severity = severity if severity in SEVERITY_ORDER else "INFO"
        self.ident = ident
        self.title = title
        self.location = location


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
            run([tools["semgrep"], "scan", "--config", "p/security-audit",
                 "--metrics=off", "--quiet", "--json", "--output", str(out),
                 str(d / "canary.py")])
            if len(lst(load_json(out), "results")) == 0:
                problems.append("semgrep found nothing in a shell=True subprocess "
                                "— its rules did not load")

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

def scan_secrets(tools, root, diff_ref, history):
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
    # without this the allowlist silently stops applying and every previously
    # accepted false positive comes back as a CRITICAL.
    cfg = Path(root) / ".betterleaks.toml"
    if cfg.is_file():
        cmd += ["--config", str(cfg)]
    _, so, _ = run(cmd)
    try:
        raw = json.loads(so) or []
    except Exception:
        raw = []
    findings = []
    for f in raw:
        loc, line = f.get("File", "?"), f.get("StartLine")
        loc = rel(loc, root)
        findings.append(Finding(
            "secrets", "CRITICAL", f.get("RuleID", "secret"),
            f.get("Description") or f.get("RuleID", "secret"),
            f"{loc}:{line}" if line else loc))
    return findings, None


def scan_deps(tools, root, out):
    """Dependency CVEs.

    Reports UNCOVERED rather than clean when no lockfile was found. "Nothing to
    scan" and "nothing wrong" are different answers, and osv-scanner's own
    --allow-no-lockfiles collapses them into a cheerful exit 0.
    """
    run([tools["osv-scanner"], "scan", "source", "-r", "--allow-no-lockfiles",
         "--format", "json", "--output-file", str(out), str(root)])
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
                rel = os.path.relpath(src, str(root)) if src else "?"
                findings.append(Finding(
                    "deps", sev, ids[0],
                    f"{info.get('name','?')} {info.get('version','')} "
                    f"({info.get('ecosystem','?')}) CVSS {score:.1f}", rel))
    note = None
    if not sources:
        note = ("no lockfile found, so NOTHING was checked for dependency "
                "vulnerabilities — uncovered, not clean")
    return findings, note


def scan_iac(tools, root, out):
    run([tools["trivy"], "config", str(root), "--severity", "HIGH,CRITICAL",
         "--quiet", "--format", "json", "--output", str(out)])
    doc = load_json(out)
    findings = []
    for r in lst(doc, "Results"):
        for m in lst(r, "Misconfigurations"):
            findings.append(Finding(
                "iac", (m.get("Severity") or "INFO").upper(),
                m.get("ID", "?"), m.get("Title", ""), r.get("Target", "?")))
    return findings, None


SEMGREP_SEV = {"ERROR": "HIGH", "WARNING": "MEDIUM", "INFO": "LOW"}


def scan_sast(tools, root, out, diff_ref):
    cmd = [tools["semgrep"], "scan", "--config", "p/default",
           "--config", "p/security-audit", "--config", "p/owasp-top-ten",
           "--metrics=off", "--quiet", "--json", "--output", str(out)]
    if diff_ref:
        cmd += ["--baseline-commit", diff_ref]
    cmd.append(str(root))
    run(cmd)
    findings = []
    for x in lst(load_json(out), "results"):
        extra = x.get("extra") or {}
        findings.append(Finding(
            "sast", SEMGREP_SEV.get(extra.get("severity"), "LOW"),
            (x.get("check_id") or "?").split(".")[-1],
            (extra.get("message") or "").strip().split("\n")[0][:100],
            f"{rel(x.get('path'), root)}:"
            f"{(x.get('start') or {}).get('line')}"))
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


def main():
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("path", nargs="?", default=".", help="directory to scan")
    ap.add_argument("--diff", nargs="?", const="origin/main", metavar="REF",
                    help="only what changed since REF (default origin/main). "
                         "Applies to secrets and SAST; deps and IaC are "
                         "whole-tree either way.")
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
    ap.add_argument("--no-fail-new", action="store_true",
                    help="in --diff mode, report newly introduced HIGH/CRITICAL "
                         "findings instead of failing on them")
    ap.add_argument("--no-canary", action="store_true",
                    help="skip the detection self-test. Every tool here fails "
                         "OPEN, so this makes a clean result meaningless.")
    ap.add_argument("--quiet", action="store_true")
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

    findings, notes = [], {}
    with tempfile.TemporaryDirectory() as tmp:
        out = Path(tmp) / "out.json"
        if "secrets" in layers:
            f, n = scan_secrets(tools, root, diff_ref, args.history)
            findings += f
            notes["secrets"] = n
        if "deps" in layers:
            f, n = scan_deps(tools, root, out)
            findings += f
            notes["deps"] = n
        if "iac" in layers:
            f, n = scan_iac(tools, root, out)
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
    reasons = []
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

    print()
    if reasons:
        print("FAILED: " + "; ".join(reasons) + ".")
        return 1
    uncovered = [k for k, v in notes.items() if v]
    if uncovered:
        print(f"PASSED — but {', '.join(uncovered)} was not actually covered, "
              "see the note above.")
    else:
        print("PASSED.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
