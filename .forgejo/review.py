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
  * each finding gets a PR-wide id (F1, F2, … continuing across pushes) and a
    `review-findings` commit status on the head: green at 0 open, red with the
    open ids otherwise. A finding closes when a reply carries a resolve marker
    (`<!-- claude-review-resolve:F2 declined: <reason> -->`, written by the
    agent-side script in lukejv-dev/assistant, scripts/review-findings.ts) or
    when someone clicks Resolve in the UI. Forgejo 16 has NO API to resolve a
    conversation (the comment's `resolver` is read-only, the web route needs a
    session and CSRF token), so the marker is the record and Resolve is optional.

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

# The two markers the tracking runs on. A finding: fingerprint plus PR-wide id
# (findings posted before ids existed have none, and are known as c<comment id>).
# A resolution: posted by whoever dealt with it, in a reply or a PR comment.
FINDING = re.compile(r"<!-- claude-review:([0-9a-f]+)(?: (F\d+))? -->")
RESOLVE = re.compile(r"<!-- claude-review-resolve:(F\d+|c\d+) (fixed|declined)(?:: (.*?))? -->")
# One per review run, in its summary (or fallback comment): how many findings
# never became threads, and whether the diff was cut short. The latest run's
# marker keeps the status red until a run with no gaps, whatever the threads say.
RUN = re.compile(r"<!-- claude-review-run:(fallback|\d+ [01]) -->")
STATUS_CONTEXT = "review-findings"


def defuse(text):
    """Model-written text with the marker name broken by a zero-width space: it
    reads the same, even inside code, but can no longer fake an id, a
    resolution or a run."""
    return text.replace("claude-review", "claude\u200b-review")


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


def get_all(path):
    """GET a list, every page. Raises instead of returning a partial list: the
    findings status is a merge signal, and a failed read must never turn into
    "0 open"."""
    out, page = [], 1
    while True:
        sep = "&" if "?" in path else "?"
        batch = api("GET", f"{path}{sep}limit=50&page={page}")
        if batch is None:
            raise RuntimeError(f"GET {path} failed")
        out += batch
        if len(batch) < 50:
            return out
        page += 1


def review_comments(repo, pr, reviews=None):
    """Every inline review comment on the PR, oldest first."""
    out = []
    for review in reviews if reviews is not None else get_all(f"/repos/{repo}/pulls/{pr}/reviews"):
        out += get_all(f"/repos/{repo}/pulls/{pr}/reviews/{review['id']}/comments")
    return sorted(out, key=lambda c: c["id"])


def gaps(reviews, issue_comments):
    """What the latest review run could not track, as status text, or "".
    Reviews and comments are two id sequences, so newest is by timestamp."""
    runs = [(r.get("submitted_at") or "", r.get("body") or "") for r in reviews]
    runs += [(c.get("created_at") or "", c.get("body") or "") for c in issue_comments]
    for _, body in sorted(runs, key=lambda r: r[0], reverse=True):
        m = RUN.search(body)
        if not m:
            continue
        if m.group(1) == "fallback":
            return "latest review fell back to a plain comment, read it"
        untracked, truncated = m.group(1).split()
        return "; ".join(t for t in (
            f"{untracked} finding(s) only in the summary" if untracked != "0" else "",
            "diff truncated, partly reviewed" if truncated == "1" else "") if t)
    return ""


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
    here are moved onto one that does by place(), rather than vanishing.
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
correctness and anything that could affect a running system over style. Do not
invent findings to fill space, and do not restate what the diff does.

REVIEW THE WHOLE CHANGE, NOT ITS HIGHLIGHTS. Work through every file the diff
touches. Stopping after the two or three most obvious problems is the failure
mode here: it drips one issue per push and the author pays for another round
trip to learn the rest, so a partial review is worse than a slow one.

READ AROUND THE DIFF. You have Read, Grep and Glob over the checkout, and a hunk
in isolation is not enough to judge one. Before reporting — or clearing — a
change, look at what it depends on: the callers of a function whose contract
moved, the other implementations of an interface, the tests that cover it, the
comments and docs that describe it. Most real defects are only visible from
outside the hunk.

Sweep these, in this order, and record what each one found in `checked`:

  1. CORRECTNESS — wrong results, broken edge cases, off-by-one, unhandled
     None/null/empty, races, resource leaks.
  2. CONTRACTS AND CALLERS — did this change a signature, a return shape, a
     default, an exit code or an invariant? Grep for who depends on it. A
     caller left on the old assumption is the most common real bug.
  3. CLAIMS THE DIFF ITSELF INVALIDATES — comments, docstrings, type docs,
     READMEs and error strings that were true before this change and are false
     after it, especially ones edited in this same diff.
  4. TESTS — do the new or changed tests actually fail when the code is wrong?
     Assertions that hold by construction, a mock that makes the assertion
     vacuous, a skipped or filtered-out case, a missing negative/control case.
  5. ERROR AND FAILURE PATHS — what happens on timeout, non-zero exit, a denied
     permission, a partial write, an empty response. Silent fallbacks that turn
     a failure into a plausible-looking success.
  6. CONFIG, ENVIRONMENT AND SECURITY — env vars and config read but not
     validated, secrets or tokens in logs or error text, injection through a
     shell or query, a widened permission, a new dependency.

An empty findings list is a valid, good answer — but only once every item above
has actually been looked at. Say so per item in `checked`; "nothing found" for
a dimension you examined is a useful answer, and `checked` is how the author
can tell that apart from a dimension you skipped.

Answer with ONE JSON object and nothing else. No prose before or after, no
markdown fence:

{{"summary": "<=3 sentences on the change as a whole, or why it is fine",
  "checked": [
    {{"area": "correctness" | "contracts" | "stale-claims" | "tests"
             | "failure-paths" | "config-security",
      "note": "<=15 words: what you looked at, and what you concluded"}}
  ],
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


def already_raised(comments):
    """(fingerprints of every finding posted before, highest F number used).
    Resolved or not: resolved means dealt with, unresolved means it is already
    on screen. Neither wants saying twice."""
    seen, top = set(), 0
    for c in comments:
        for fp, fid in FINDING.findall(c.get("body", "")):
            seen.add(fp)
            if fid:
                top = max(top, int(fid[1:]))
    return seen, top


def findings_state(comments, issue_comments):
    """{id: {title, url, state, reason}} in posting order. state is open,
    fixed, declined (a resolve marker; the latest wins) or resolved (clicked
    Resolve in the UI, no marker)."""
    found, verdicts = {}, {}
    for c in comments:
        body = c.get("body", "")
        m = FINDING.search(body)
        if m:
            fid = m.group(2) or f"c{c['id']}"
            # Two runs racing on one PR can both hand out the same F<n>. Keyed
            # by id, the second would silently replace the first: one open
            # finding gone from the count, and one marker closing both.
            if fid in found:
                fid = f"c{c['id']}"
            title = body.split("\n", 1)[0].rsplit(" · ", 1)[-1].strip()
            found[fid] = {"title": title, "url": c.get("html_url", ""),
                          "state": "resolved" if c.get("resolver") else "open",
                          "reason": ""}
    # Oldest first across both lists (they share one id sequence), so the
    # latest marker for an id really is the last one applied.
    for c in sorted(comments + issue_comments, key=lambda c: c["id"]):
        body = c.get("body", "")
        # Never from a body carrying a finding: that text is the model's, and a
        # diff that talks the model into writing a marker would close itself.
        if not FINDING.search(body):
            for fid, verdict, reason in RESOLVE.findall(body):
                verdicts[fid] = (verdict, reason.strip())
    for fid, (verdict, reason) in verdicts.items():
        if fid in found:
            found[fid].update(state=verdict, reason=reason)
    return found


def describe(found, gap=""):
    """(commit status state, description). Declined reasons are in it so Lucas
    can skim just those without opening the PR. A gap is always red."""
    n = {k: [i for i, f in found.items() if f["state"] == k]
         for k in ("open", "fixed", "declined", "resolved")}
    counts = " · ".join(f"{len(v)} {k}" for k, v in n.items() if v and k != "open")
    if n["open"]:
        desc = f"{len(n['open'])} open: {', '.join(n['open'])}" + (f" · {counts}" if counts else "")
        state = "failure"
    else:
        desc = f"0 open · {counts}" if counts else "0 findings"
        if n["declined"]:
            desc += " — " + "; ".join(f"{i} {found[i]['reason'][:60]}" for i in n["declined"])
        state = "success"
    if gap:
        state, desc = "failure", f"{gap} · {desc}"
    # ponytail: hard cap for the status column; the full list is on the PR.
    return state, desc if len(desc) <= 255 else desc[:254] + "…"


def publish_status(repo, pr, head):
    """Recompute every finding on the PR and set `review-findings` on head."""
    try:
        reviews = get_all(f"/repos/{repo}/pulls/{pr}/reviews")
        issue_comments = get_all(f"/repos/{repo}/issues/{pr}/comments")
        found = findings_state(review_comments(repo, pr, reviews), issue_comments)
        state, desc = describe(found, gaps(reviews, issue_comments))
    except RuntimeError as e:
        print(e, file=sys.stderr)
        state, desc = "error", "could not read the findings; see the review job log"
    if not set_status(repo, pr, head, state, desc):
        return 1
    print(f"{STATUS_CONTEXT}: {state} — {desc}")
    return 0


def set_status(repo, pr, head, state, desc):
    """True if the status was set. A failure is loud: a merge signal that did
    not land must not look like one that did."""
    server = os.environ["API"].removesuffix("/api/v1")
    ok = api("POST", f"/repos/{repo}/statuses/{head}", {
        "state": state, "context": STATUS_CONTEXT, "description": desc,
        "target_url": f"{server}/{repo}/pulls/{pr}/files"}) is not None
    if not ok:
        print(f"{STATUS_CONTEXT} NOT set on {head} (wanted {state}: {desc})", file=sys.stderr)
    return ok


def render(f, fid, note=""):
    sev = SEVERITY.get(f.get("severity", "medium"), SEVERITY["medium"])
    clean = defuse
    parts = [f"**{fid}** · {sev} · {clean(f['title'])}", ""]
    if note:
        parts += [note, ""]
    parts += [clean(f["body"].strip())]
    if f.get("suggestion"):
        parts += ["", "**Suggested change**", "```suggestion",
                  clean(f["suggestion"].rstrip()), "```"]
    parts += ["", f"<!-- claude-review:{fingerprint(f)} {fid} -->"]
    return "\n".join(parts)


def place(f, valid):
    """(path, line, extra_lines_count, note) to post a finding at, or None.
    Out of the hunks it still goes inline — on the file's first changed line,
    or the diff's — because only an inline comment is a thread that can be
    tracked and resolved. The note says where it was meant."""
    if not valid:
        return None
    line, end = f.get("line"), f.get("end_line") or f.get("line")
    path, lines, note = f["path"], valid.get(f["path"], set()), ""
    if not isinstance(line, int) or line not in lines:
        note = (f"_Meant `{path}" + (f":{line}" if isinstance(line, int) else "")
                + "`, outside this diff's hunks._")
        if not lines:
            path = next(iter(valid))
            lines = valid[path]
        line = end = min(lines)
    # A multi-line comment whose tail runs past the hunk is rejected whole,
    # so the span is clipped to what the diff actually shows.
    extra = 0
    if isinstance(end, int) and end > line:
        while extra < end - line and line + extra + 1 in lines:
            extra += 1
    return path, line, extra, note


def main():
    repo, pr = os.environ["REPO"], os.environ["PR"]
    head = os.environ["HEAD_SHA"]
    diff, guidance, truncated = collect_diff(os.environ["BASE_SHA"], head)
    if not diff.strip():
        print("empty diff, nothing to review")
        return publish_status(repo, pr, head)

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
                             "Check the job log.",
                             "review output unparseable: no findings checked, see the job log")

    valid = anchors(diff)
    try:
        seen, top = already_raised(review_comments(repo, pr))
    except RuntimeError as e:
        # Unknown history: posting anyway would re-raise and renumber, so stop.
        print(e, file=sys.stderr)
        set_status(repo, pr, head, "error", "could not read earlier findings; see the review job log")
        return 1
    inline, demoted, repeats = [], [], 0

    for f in result.get("findings", []):
        if not all(k in f for k in ("path", "title", "body")):
            continue
        if fingerprint(f) in seen:
            repeats += 1
            continue
        spot = place(f, valid) if len(inline) < MAX_INLINE else None
        if spot is None:
            demoted.append(f)
            continue
        path, line, extra, note = spot
        top += 1
        inline.append({"path": path, "new_position": line,
                       "extra_lines_count": extra, "body": render(f, f"F{top}", note)})

    body = summary(result, inline, demoted, repeats, truncated, head)
    posted = api("POST", f"/repos/{repo}/pulls/{pr}/reviews",
                 {"body": body, "event": "COMMENT", "commit_id": head,
                  "comments": inline})
    if posted:
        print(f"review posted: {len(inline)} inline, {len(demoted)} in summary, "
              f"{repeats} already raised")
        return publish_status(repo, pr, head)
    # A rejected review (a stale commit_id, a line the API disagrees about)
    # must not swallow the findings — they go up as a plain comment instead.
    return post_fallback(repo, pr, head, body + inline_as_text(inline))


def summary(result, inline, demoted, repeats, truncated, head):
    out = ["### Claude review", "", result.get("summary", "").strip()]
    if not inline and not demoted:
        out.append("\nNothing to change." if not repeats else
                   f"\nNothing new. {repeats} earlier finding(s) already on this PR.")
    if demoted:
        out += ["", f"**Not posted inline** (past the {MAX_INLINE}-comment cap, or no "
                    "changed line to hang them on; untracked until a later push "
                    "raises them again):", ""]
        out += [f"- `{f['path']}`"
                + (f":{f['line']}" if isinstance(f.get("line"), int) else "")
                + f" — {f['title']}: {f['body'].strip()}" for f in demoted]
    # The coverage sweep, folded away. Posted because a claim you can see is a
    # claim you can call out: "nothing found in tests" next to a PR that added
    # a vacuous assertion tells the author the reviewer looked and was wrong,
    # which is actionable. An unposted sweep is one the model can quietly skip.
    areas = [c for c in result.get("checked", [])
             if isinstance(c, dict) and c.get("area")]
    if areas:
        out += ["", "<details><summary>What was checked</summary>", ""]
        for c in areas:
            note = str(c.get("note", "")).strip()
            out.append(f"- **{c['area']}**" + (f" — {note}" if note else ""))
        out += ["", "</details>"]

    if repeats:
        out += ["", f"<sub>{repeats} finding(s) raised on an earlier push are not "
                    "repeated here.</sub>"]
    if truncated:
        out += ["", f"<sub>Diff truncated at {MAX_DIFF_BYTES // 1000}KB — review "
                    "is partial.</sub>"]
    out += ["", f"<sub>Automated review of `{head}`. Not a substitute for a "
                "human look.</sub>"]
    # Everything above is or quotes model text, so it is defused whole; the
    # run marker goes on after, as the only real one.
    return (defuse("\n".join(out))
            + f"\n\n<!-- claude-review-run:{len(demoted)} {int(truncated)} -->")


def inline_as_text(inline):
    if not inline:
        return ""
    return "\n\n---\n\n" + "\n\n".join(
        f"**`{c['path']}:{c['new_position']}`**\n\n{c['body']}" for c in inline)


def post_fallback(repo, pr, head, body,
                  why="review fell back to a plain comment: findings untracked, read it"):
    # The body's own run marker (if any) says nothing was lost; this one says
    # everything was. Resolve markers in it are model text, never honoured.
    body = RUN.sub("", body).replace("claude-review-resolve", "claude\u200b-review-resolve")
    body += "\n\n<!-- claude-review-run:fallback -->"
    ok = api("POST", f"/repos/{repo}/issues/{pr}/comments", {"body": body})
    # A plain comment has no threads to resolve, so nothing here can go green.
    if not set_status(repo, pr, head, "failure", why):
        return 1
    if not ok:
        print("both the review and the fallback comment failed", file=sys.stderr)
        return 1
    print("posted as a plain comment (review API refused)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
