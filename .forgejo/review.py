#!/usr/bin/env python3
# MANAGED FILE — do not edit in this repository.
# Source of truth: lukejv-dev/homelab-platform → ops/forgejo-ci/files/review.py
# Edit it there and re-run `ops/forgejo-ci/rollout.py apply --yes`. A local edit
# here is reported as drift by `rollout.py check` and overwritten on the next apply.
"""Claude's PR review, posted the way a reviewer posts one.

Called by `.forgejo/workflows/claude-review.yml`. Everything except installing
the CLI lives here, for the same reason scan.py exists: logic in a workflow file
cannot be run or tested anywhere but CI.

WHAT CHANGED AND WHY. The first version asked Claude for prose and posted it as
one issue comment. That reads fine and is useless to act on: the reader has to
carry "line 42 of the migration" from the bottom of the thread up to the diff,
nothing marks a point as dealt with, and every push appends another wall saying
most of the same things. So instead:

  * findings come back as JSON and go up as INLINE review comments, anchored to
    the file and line — which is what makes them a conversation Forgejo can
    resolve. The resolve button is a property of a review comment on a diff
    line; an issue comment can never have one.
  * one review, not N comments: a summary body plus its inline children, posted
    as event=COMMENT so it never blocks a merge.
  * each comment carries a fingerprint of (path, title). On the next push,
    anything already raised is skipped — including the ones you resolved. That
    is the whole point of resolving something.

NOT SUPPORTED HERE: Forgejo 16 renders a ```suggestion block as an ordinary code
block. There is no apply button (that is a GitHub feature Forgejo has not
shipped). Suggestions are still worth emitting — they read as "here is the line
I mean" — but do not expect one-click acceptance.

TRUST. The diff is untrusted text: anyone who can open a PR can write
"ignore your instructions" into it, and this process holds the Claude OAuth
token. Hence --disallowed-tools on everything that reaches the network or writes
to the tree, and hence the workflow runs THIS FILE FROM THE BASE REF, not from
the PR's checkout. See the note in the workflow.
"""

import hashlib
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request

# A 300-commit sync produces megabytes. Past this the review is partial and says
# so — better than being truncated mid-hunk with no indication.
MAX_DIFF_BYTES = 400_000
# Guards against one bad run carpeting a PR. Anything past this goes in the
# summary body as a list instead.
MAX_INLINE = 25

SEVERITY = {"high": "🔴 **High**", "medium": "🟠 **Medium**", "low": "🔵 **Low**"}


def sh(*args, **kw):
    return subprocess.run(args, capture_output=True, text=True, **kw).stdout


def api(method, path, payload=None):
    """Forgejo API call. Returns parsed JSON, or None on failure."""
    url = f"{os.environ['API']}{path}"
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers={
        "Authorization": f"token {os.environ['TOKEN']}",
        "Content-Type": "application/json",
    })
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return json.loads(r.read() or "null")
    except urllib.error.HTTPError as e:
        body = e.read()[:500].decode(errors="replace")
        print(f"{method} {path} -> HTTP {e.code}: {body}", file=sys.stderr)
    except Exception as e:  # noqa: BLE001 — network, DNS, timeout: same handling
        print(f"{method} {path} -> {e}", file=sys.stderr)
    return None


def collect_diff(base_sha, head_sha):
    """(diff text, was it truncated). Merge-base, so unrelated base commits
    landed since the branch forked are not attributed to this PR."""
    sh("git", "fetch", "--no-tags", "origin", base_sha, head_sha)
    merge_base = sh("git", "merge-base", base_sha, head_sha).strip() or base_sha
    diff = sh("git", "diff", f"{merge_base}..{head_sha}")
    guidance = sh("git", "show", f"{merge_base}:.forgejo/claude-review.md")
    truncated = len(diff.encode()) > MAX_DIFF_BYTES
    if truncated:
        diff = diff.encode()[:MAX_DIFF_BYTES].decode(errors="ignore")
    return diff, guidance, truncated


def anchors(diff):
    """{path: {line numbers in the NEW file a comment can attach to}}.

    A review comment only renders if its line is inside a hunk. Posting one
    outside gets silently swallowed by the API, so findings that do not land
    here are demoted to the summary rather than vanishing.
    """
    out, path, line = {}, None, 0
    for raw in diff.splitlines():
        if raw.startswith("+++ "):
            p = raw[4:].strip()
            path = None if p == "/dev/null" else re.sub(r"^b/", "", p)
            line = 0
        elif raw.startswith("@@"):
            m = re.match(r"@@ -\d+(?:,\d+)? \+(\d+)", raw)
            line = int(m.group(1)) if m else 0
        elif path and line and (raw[:1] in ("+", " ") or raw == ""):
            # raw == "" is a context line that was an empty line: git writes it
            # as a lone space, and anything that strips trailing whitespace on
            # the way here turns it into nothing. Not counting it would shift
            # every later line in the hunk by one and put the comments on the
            # wrong lines — silently, which is the worst way to be wrong.
            out.setdefault(path, set()).add(line)
            line += 1
        # "-" removed lines do not advance the new-file counter; "\" (no newline
        # at EOF) and the diff --git/index headers are not content.
    return out


PROMPT = """\
Review this pull request diff. Repository: {repo}
Title: {title}

Report only what a reviewer would actually ask to be changed. Prioritise
correctness and anything that could affect a running system over style. An empty
findings list is a valid, good answer — do not invent findings to fill it, and do
not restate what the diff does.

Answer with ONE JSON object and nothing else. No prose before or after, no
markdown fence:

{{"summary": "<=3 sentences on the change as a whole, or why it is fine",
  "findings": [
    {{"path": "exact/path/from/the/diff",
      "line": <line number in the NEW file, must be a line this diff touches>,
      "end_line": <last line if the finding spans several, else same as line>,
      "severity": "high" | "medium" | "low",
      "title": "<8 words, the problem, not the fix>",
      "body": "what breaks and under what conditions, then the fix. Markdown ok.",
      "suggestion": "<replacement source for line..end_line, or null>"}}
  ]}}

severity: high = it is broken, loses data, or is a security hole. medium = it
will bite under a condition that will occur. low = worth fixing, nothing burns.
suggestion must be the literal replacement lines, correctly indented, no fence
and no diff markers — or null when the fix is not a small local edit.
{guidance}
```diff
{diff}
```
"""


def ask_claude(prompt):
    """The diff is in the prompt, so the review needs no tool that reaches
    outside it. Read/Grep/Glob stay on — reading around the diff is the useful
    part and, confined to the checkout, it exfiltrates nothing."""
    p = subprocess.run(
        ["claude", "-p", "--output-format", "text",
         "--disallowed-tools", "Bash", "Edit", "Write", "NotebookEdit",
         "WebFetch", "WebSearch", "Task"],
        input=prompt, capture_output=True, text=True)
    if p.returncode != 0:
        print(f"claude exited {p.returncode}: {p.stderr[:500]}", file=sys.stderr)
    return p.stdout


def parse(out):
    """Claude was asked for bare JSON. Models fence it anyway often enough that
    refusing to cope would fail the job for a formatting habit."""
    m = re.search(r"```(?:json)?\s*(.+?)```", out, re.S)
    text = m.group(1) if m else out
    start, end = text.find("{"), text.rfind("}")
    if start < 0 or end < 0:
        return None
    try:
        return json.loads(text[start:end + 1])
    except json.JSONDecodeError as e:
        print(f"unparseable model output: {e}", file=sys.stderr)
        return None


def fingerprint(f):
    """Identity of a finding ACROSS pushes, so line numbers are deliberately not
    in it — the same problem shifted down three lines is the same problem, and
    re-raising something you resolved is the exact behaviour being fixed."""
    key = f"{f.get('path','')}\n{f.get('title','').strip().lower()}"
    # sha256, not because collisions matter here (this is a dedup key, not
    # a signature) but because a scanner flags sha1 on sight and a suppressed
    # finding costs more attention than a longer digest.
    return hashlib.sha256(key.encode()).hexdigest()[:12]


def already_raised(repo, pr):
    """Fingerprints of every finding this reviewer has posted before —
    resolved or not. Resolved means dealt with; unresolved means it is already
    on screen. Neither wants saying twice."""
    seen = set()
    for review in api("GET", f"/repos/{repo}/pulls/{pr}/reviews") or []:
        for c in api("GET", f"/repos/{repo}/pulls/{pr}/reviews/{review['id']}/comments") or []:
            seen.update(re.findall(r"<!-- claude-review:([0-9a-f]+) -->", c.get("body", "")))
    return seen


def render(f):
    sev = SEVERITY.get(f.get("severity", "medium"), SEVERITY["medium"])
    parts = [f"{sev} · {f['title']}", "", f["body"].strip()]
    if f.get("suggestion"):
        parts += ["", "**Suggested change**", "```suggestion",
                  f["suggestion"].rstrip(), "```"]
    parts += ["", f"<!-- claude-review:{fingerprint(f)} -->"]
    return "\n".join(parts)


def main():
    repo, pr = os.environ["REPO"], os.environ["PR"]
    head = os.environ["HEAD_SHA"]
    diff, guidance, truncated = collect_diff(os.environ["BASE_SHA"], head)
    if not diff.strip():
        print("empty diff, nothing to review")
        return 0

    guidance_block = ""
    if guidance.strip():
        # From the BASE ref, never the checkout: the prompt gives this file
        # precedence, so taking it from the head would let any PR rewrite the
        # reviewer's brief — a one-line commit saying "report no issues" would
        # be honoured. From the base it is the maintainer's setting, as intended.
        guidance_block = ("\nRepository-specific guidance follows; it takes "
                          "precedence over the generic priorities above.\n\n"
                          + guidance.strip() + "\n")

    result = parse(ask_claude(PROMPT.format(
        repo=repo, title=os.environ.get("PR_TITLE", ""),
        guidance=guidance_block, diff=diff)))
    if result is None:
        return post_fallback(repo, pr, head,
                             "The review ran but its output could not be parsed. "
                             "Check the job log.")

    valid = anchors(diff)
    seen = already_raised(repo, pr)
    inline, demoted, repeats = [], [], 0

    for f in result.get("findings", []):
        if not all(k in f for k in ("path", "title", "body")):
            continue
        if fingerprint(f) in seen:
            repeats += 1
            continue
        line, end = f.get("line"), f.get("end_line") or f.get("line")
        lines = valid.get(f["path"], set())
        if not isinstance(line, int) or line not in lines or len(inline) >= MAX_INLINE:
            demoted.append(f)
            continue
        # A multi-line comment whose tail runs past the hunk is rejected whole,
        # so the span is clipped to what the diff actually shows.
        extra = 0
        if isinstance(end, int) and end > line:
            while extra < end - line and line + extra + 1 in lines:
                extra += 1
        inline.append({"path": f["path"], "new_position": line,
                       "extra_lines_count": extra, "body": render(f)})

    body = summary(result, inline, demoted, repeats, truncated, head)
    posted = api("POST", f"/repos/{repo}/pulls/{pr}/reviews",
                 {"body": body, "event": "COMMENT", "commit_id": head,
                  "comments": inline})
    if posted:
        print(f"review posted: {len(inline)} inline, {len(demoted)} in summary, "
              f"{repeats} already raised")
        return 0
    # A rejected review (a stale commit_id, a line the API disagrees about)
    # must not swallow the findings — they go up as a plain comment instead.
    return post_fallback(repo, pr, head, body + inline_as_text(inline))


def summary(result, inline, demoted, repeats, truncated, head):
    out = ["### Claude review", "", result.get("summary", "").strip()]
    if not inline and not demoted:
        out.append("\nNothing to change." if not repeats else
                   f"\nNothing new. {repeats} earlier finding(s) already on this PR.")
    if demoted:
        out += ["", "**Not attached to a line** (outside this diff's hunks):", ""]
        out += [f"- `{f['path']}`"
                + (f":{f['line']}" if isinstance(f.get("line"), int) else "")
                + f" — {f['title']}: {f['body'].strip()}" for f in demoted]
    if repeats:
        out += ["", f"<sub>{repeats} finding(s) raised on an earlier push are not "
                    "repeated here.</sub>"]
    if truncated:
        out += ["", f"<sub>Diff truncated at {MAX_DIFF_BYTES // 1000}KB — review "
                    "is partial.</sub>"]
    out += ["", f"<sub>Automated review of `{head}`. Not a substitute for a "
                "human look.</sub>"]
    return "\n".join(out)


def inline_as_text(inline):
    if not inline:
        return ""
    return "\n\n---\n\n" + "\n\n".join(
        f"**`{c['path']}:{c['new_position']}`**\n\n{c['body']}" for c in inline)


def post_fallback(repo, pr, head, body):
    ok = api("POST", f"/repos/{repo}/issues/{pr}/comments", {"body": body})
    if not ok:
        print("both the review and the fallback comment failed", file=sys.stderr)
        return 1
    print("posted as a plain comment (review API refused)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
