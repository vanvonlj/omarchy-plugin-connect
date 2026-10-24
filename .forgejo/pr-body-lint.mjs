// MANAGED FILE — do not edit in this repository.
// Source of truth: lukejv-dev/homelab-platform → ops/forgejo-ci/files/pr-body-lint.mjs
// (synced here as .forgejo/pr-body-lint.mjs by rollout.py; a local edit is drift).
//
// Vendored from lukejv-dev/assistant scripts/pr-body-lint.ts
//   commit 8d749f03a7f8b3fe4d63d41443be43b7e8ed8730 (blob 57d84514a992658a89f75d2235d09c02a9881b66)
// Only the TypeScript types are removed (node:module stripTypeScriptTypes, mode "strip":
// same lines, same columns) and `import.meta.main` (Node >= 24.2 only) becomes an
// argv[1] check, so it runs on any Node >= 18 with no install. Logic is unchanged.
// To update: re-vendor from assistant, bump the commit above, rerun test_pr_body.py.
//
// PR-body lint (CNVYR-412): fail a PR whose body lacks the facts Lucas reads first.
//   node scripts/pr-body-lint.ts --event "$GITHUB_EVENT_PATH"        (CI)
//   node scripts/pr-body-lint.ts [--repo R] [--title T] [--branch B] [--base main] [--files "a.md\nb.ts"] [body-file]
// --event reads body/title/head/base from the event JSON (PR text never passes through a
// workflow expression: the runner would evaluate expressions inside it) and diffs the
// changed files itself. Otherwise body from the file argument, else stdin.
// Exit 1 lists what is missing, 0 passes.
// Section names mirror docs/conventions.md § Pull requests and the card ready checklist.
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

                                                                                                                   

const heading = (name        ) => new RegExp(`^#{1,6}[ \\t]*${name}\\b.*$`, "im");
/** The body's lines, each flagged true when it is a ``` or ~~~ fence line or sits inside a fence. */
function fenceLines(body        )                                         {
  let fence                         = null; // the opening fence while inside one
  const lines = body.split("\n");
  const fenced = lines.map((line) => {
    const m = /^ {0,3}(`{3,}|~{3,})(.*)$/.exec(line);
    if (!fence) {
      if (m && !(m[1][0] === "`" && m[2].includes("`"))) fence = m;
      return !!fence;
    }
    if (m && m[1][0] === fence[1][0] && m[1].length >= fence[1].length && !m[2].trim()) fence = null;
    return true;
  });
  return { lines, fenced };
}
/** body with a "#" at the start of any line inside a fence blanked (same length), so it is no heading. */
function maskFences(body        )         {
  const { lines, fenced } = fenceLines(body);
  return lines.map((l, i) => (fenced[i] ? l.replace(/^(\s*)#/, "$1 ") : l)).join("\n");
}
/** body without its fenced lines: an escape line quoted in a code block is not an escape. */
const unfenced = (body        ) => {
  const { lines, fenced } = fenceLines(body);
  return lines.filter((_, i) => !fenced[i]).join("\n");
};
/** Text under a heading, up to the next heading. Null when the heading is missing. Fenced code is never a heading. */
function section(body        , name        )                {
  const masked = maskFences(body);
  const m = heading(name).exec(masked);
  if (!m) return null;
  const start = m.index + m[0].length;
  return body.slice(start, start + masked.slice(start).split(/^#{1,6}[ \t]/m)[0].length);
}
/** A docs/conventions.md template placeholder: "<what wasn't demonstrated…>" (has a space, unlike <br>). */
const PLACEHOLDER = /<[^<>!/]*\s[^<>]*>/g;
const blank = (line        ) => line.replace(PLACEHOLDER, "").replace(/[\s|*_-]/g, "") === "";
/** Has a line that isn't empty, a comment or an unedited template placeholder. */
const filled = (s               ) => !!s && !s.replace(/<!--[\s\S]*?-->/g, "").split("\n").every(blank);
const SEPARATOR = /^\s*\|?[\s:|-]+\|[\s:|-]*$/;
/** A table with a header and at least one data row. */
const hasTableRow = (s               ) => {
  const rows = (s ?? "").split("\n").filter((l) => l.trim().startsWith("|") && l.includes("|", l.indexOf("|") + 1));
  const [header, ...data] = rows.filter((l) => !SEPARATOR.test(l));
  return rows.some((l) => SEPARATOR.test(l)) && !!header && data.some((l) => !blank(l));
};

const SKIP = /^[ \t>*_-]*lint:[ \t]*skip\b[ \t:-]*(.*)$/im;
const DEPENDS = /^[ \t>*_-]*depends on\**:[ \t]*\**[ \t]*(.*)$/im;
/** The body says it is stacked on its own line ("Stacked on X", "Stacked: yes"); prose that mentions the word is not a claim. */
const STACKED = /^[ \t>*_-]*stacked(?:[ \t]+on\b|\**:)/im;
const WHY = /^[ \t>*_-]*why this order\**:[ \t]*\S/im;
const PROOF = /\bfails?\s+(?:on\s+(?:origin\/)?main|on\s+the\s+base|without\s+the\s+fix|before\s+the\s+fix)\b/gi;
const NEGATION = /(?:\b(?:not|never|no)|n['’]t)\b/i;
/** A bare answer right after the phrase: "fails on main? no." (a "not ok" later in the line is test output, not a denial). */
const DENIAL = /^[\s?:,—-]*(?:no|nope|false)\s*(?:[,)]|$)/i;
/** A sentence with the proof phrase, no negation before it ("does not fail on main") and no bare "no" right after. "?" does not end a sentence. */
const hasProof = (body        ) =>
  body.split(/[.!;\n]+/).some((sentence) =>
    [...sentence.matchAll(PROOF)].some((m) =>
      !NEGATION.test(sentence.slice(0, m.index)) && !DENIAL.test(sentence.slice(m.index + m[0].length))));
/** A fix/bugfix title: "fix", "fixes", "fixed", "bugfix" or "bug" LEADS it, optionally after a card key ("CNVYR-1: Fix crash", "fix(scope): x"). "Docs: explain how to fix X" is not a fix. */
const FIX_TITLE = /^(?:[A-Z][A-Z0-9]*-\d+:\s*)?(?:fix(?:es|ed)?|bug ?fix|bug)\b/i;

/** A PR reference: repo#N, owner/repo#N, or a full PR URL (.../pulls/N). */
const PR_REF = /[\w.-]+(?:\/[\w.-]+)?#\d+\b|\/pulls?\/\d+\b/g;
/** The PRs the "Depends on:" line names, wherever on the line ("(see …/pulls/9)" counts). A placeholder ("<repo#N>"), hint text, "none", "n/a" or nothing holds no reference. */
export function dependencies(body        )           {
  const value = DEPENDS.exec(unfenced(body))?.[1] ?? "";
  return value.match(PR_REF) ?? [];
}

/** What is wrong with a PR body, one line each; "skip: <reason>" when the escape line applies. */
export function lint({ body, repo = "", title = "", branch = "", base = "main", files = [] }    )                                           {
  body = body.replace(/\r\n?/g, "\n"); // the Forgejo web editor saves CRLF; fence detection needs LF
  const claims = unfenced(body); // a line quoted in a code block is not a claim
  const skip = SKIP.exec(claims);
  const problems           = [];
  if (skip && /\w/.test(skip[1])) return { problems: [], skipped: skip[1].trim() };
  if (skip) problems.push("`lint: skip` needs a reason (e.g. `lint: skip generated rollout PR`)");
  const need = (ok         , what        ) => ok || problems.push(what);

  need(filled(section(body, "Evidence")), "## Evidence (a section with content)");
  need(filled(section(body, "Not proven")), "## Not proven (a section with content; write None if nothing)");
  if (files.length && files.every((f) => f.endsWith(".md"))) return { problems }; // docs-only: those two only

  need(hasTableRow(section(body, "Reachability")), "## Reachability (a table with a header and at least one row)");
  const hasDeps = dependencies(claims).length > 0;
  // A gitops PR is nearly always paired with one in another repo, so it always says which (or none).
  if (/(^|\/)gitops$/i.test(repo)) need(hasDeps || /^none\b/i.test(DEPENDS.exec(claims)?.[1] ?? ""), "Depends on: <repo#N, ...> or Depends on: none (gitops PRs always declare it)");
  if (STACKED.test(claims) || base !== "main") need(hasDeps, `Depends on: <repo#N> (stacked PR${base !== "main" ? `, base is ${base}` : ""})`);
  if (hasDeps) need(WHY.test(claims), "Why this order: <reason> (the PR has Depends on:)");
  if (/^fix\//.test(branch) || FIX_TITLE.test(title.trim())) {
    need(hasProof(body), 'a "fails without the fix" proof line (e.g. "the new test fails on main")');
  }
  return { problems };
}

/** The PR from a pull_request event file; changed files from git (no shell). */
export function fromEvent(path        , diff = changedFiles)     {
  if (!path) throw new Error("--event: no path and GITHUB_EVENT_PATH is unset");
  const event = JSON.parse(readFileSync(path, "utf8"));
  const pr = event.pull_request ?? {};
  const base = String(pr.base?.ref ?? "main");
  return { body: String(pr.body ?? ""), repo: String(event.repository?.full_name ?? ""), title: String(pr.title ?? ""), branch: String(pr.head?.ref ?? ""), base, files: diff(base) };
}
/** No diff (e.g. a shallow clone) → [], so the full lint applies, never the docs-only one. */
export function changedFiles(base        )           {
  try {
    // quotePath off + NUL-split: a non-ASCII name stays "docs/ü.md", not "\"docs/\\303\\274.md\"".
    // --no-renames: a rename lists the old path too, so scripts/x.ts -> scripts/x.md is not docs-only.
    const out = execFileSync("git", ["-c", "core.quotePath=false", "diff", "--name-only", "--no-renames", "-z", `origin/${base}...HEAD`], { encoding: "utf8" });
    return out.split("\0").filter(Boolean);
  } catch {
    console.error(`pr-body-lint: git diff against origin/${base} failed; linting as a code PR`);
    return [];
  }
}

function main(argv          )         {
  const opt                         = {};
  const rest           = [];
  for (let i = 0; i < argv.length; i++) {
    if (argv[i].startsWith("--")) opt[argv[i].slice(2)] = argv[++i] ?? "";
    else rest.push(argv[i]);
  }
  const { problems, skipped } = lint("event" in opt ? fromEvent(opt.event || process.env.GITHUB_EVENT_PATH || "") : {
    body: readFileSync(rest[0] ?? 0, "utf8"),
    repo: opt.repo,
    title: opt.title,
    branch: opt.branch,
    base: opt.base,
    files: (opt.files ?? "").split(/[\n,]/).map((f) => f.trim()).filter(Boolean),
  });
  if (skipped !== undefined) console.log(`pr-body-lint: skipped (lint: skip) — ${skipped}`);
  else if (!problems.length) console.log("pr-body-lint: ok");
  else {
    console.log("pr-body-lint: this PR body is missing:");
    for (const p of problems) console.log(`  - ${p}`);
    console.log("See docs/conventions.md § Pull requests. Docs-only PRs need Evidence + Not proven; `lint: skip <reason>` skips.");
  }
  return problems.length ? 1 : 0;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) process.exitCode = main(process.argv.slice(2));
