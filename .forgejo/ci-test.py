# MANAGED FILE — do not edit in this repository.
# Source of truth: lukejv-dev/homelab-platform → ops/forgejo-ci/files/ci-test.py
# Edit it there and re-run `ops/forgejo-ci/rollout.py apply --yes`.
#
# Reads and validates `.forgejo/ci.toml` for the managed test.yml (CNVYR-508) and
# prints what the workflow needs as KEY=VALUE lines, ready for $GITHUB_OUTPUT.
# Stdlib only; the workflow runs it as `python3 -I -S`, from the BASE ref.
#
# Every value that leaves here is substituted into the workflow by the runner (image,
# timeout, workdir, postgres settings), and act re-evaluates expressions found inside
# substituted text (CNVYR-446). So each one is held to a strict character set with no
# braces or dollar signs. The test command is free text: it is emitted base64-encoded,
# an alphabet that cannot spell an expression, and decoded inside the job.
#
# No file means "nothing to test": enabled=false, exit 0. Anything wrong in a file
# that exists exits 1 with one `ci-test:` line per problem on stderr.
#
# On a pull request the workflow also passes `--base-file`, the base branch's ci.toml.
# The image and the services come from THAT file, never from the PR: a PR cannot choose
# the container its own test runs in. The PR's file contributes only `test`, `workdir` and
# `timeout_minutes` (its own image and services are still checked, so a typo is caught
# before it is merged, but they are not used, and a difference is warned about). A PR that
# adds the first ci.toml has no base file: enabled=false, the tests run after the merge.
import argparse
import base64
import re
import sys
import tomllib

MAX_TIMEOUT = 30
DEFAULT_TIMEOUT = 15
MAX_TEST_BYTES = 4096

TOP_KEYS = {"image", "test", "workdir", "timeout_minutes", "services"}
SERVICES = {"postgres"}
POSTGRES_KEYS = {"image", "env"}
# Fixed names, because a service's env map cannot be built from an expression: each one
# is a separate workflow output.
POSTGRES_ENV = ("POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB")
# The password defaults to the user's name, so no password text lives in this file.
POSTGRES_DEFAULTS = {"POSTGRES_USER": "postgres", "POSTGRES_DB": "test"}

IMAGE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._/:@-]{0,199}")
# No leading "-": the workflow runs `cd -- "$WORKDIR"`, and this keeps it from ever looking like an option.
WORKDIR = re.compile(r"[A-Za-z0-9._/][A-Za-z0-9._/-]{0,199}")
ENV_VALUE = re.compile(r"[A-Za-z0-9._-]{1,64}")


def need_str(table, key, where, pattern, errs):
    v = table.get(key)
    if not isinstance(v, str) or not v:
        errs.append(f"{where}{key} is required and must be a non-empty string")
        return None
    if pattern and not pattern.fullmatch(v):
        errs.append(f"{where}{key} = {v!r} has characters outside {pattern.pattern}")
        return None
    return v


def infra(cfg, errs, where=""):
    """(image, postgres settings or None) of a loaded ci.toml; problems go to errs."""
    image = need_str(cfg, "image", where, IMAGE, errs)
    services = cfg.get("services", {})
    pg = None
    if not isinstance(services, dict):
        errs.append(f"{where}services must be a table")
        return image, pg
    for k in sorted(set(services) - SERVICES):
        errs.append(f"{where}unknown service {k!r} (allowed: {', '.join(sorted(SERVICES))})")
    if isinstance(services.get("postgres"), dict):
        raw = services["postgres"]
        for k in sorted(set(raw) - POSTGRES_KEYS):
            errs.append(f"{where}unknown key {k!r} in [services.postgres] (allowed: {', '.join(sorted(POSTGRES_KEYS))})")
        pg = {"image": need_str(raw, "image", f"{where}services.postgres.", IMAGE, errs)}
        env = raw.get("env", {})
        if not isinstance(env, dict):
            errs.append(f"{where}services.postgres.env must be a table")
            env = {}
        for k in sorted(set(env) - set(POSTGRES_ENV)):
            errs.append(f"{where}unknown key {k!r} in [services.postgres.env] (allowed: {', '.join(POSTGRES_ENV)})")
        for k in POSTGRES_ENV:
            if k in env:
                pg[k] = need_str(env, k, f"{where}services.postgres.env.", ENV_VALUE, errs)
            else:
                pg[k] = POSTGRES_DEFAULTS.get(k) or pg["POSTGRES_USER"]
    elif "postgres" in services:
        errs.append(f"{where}services.postgres must be a table")
    return image, pg


def load(path):
    """(table or None if the file is absent, error or None)."""
    try:
        with open(path, "rb") as f:
            return tomllib.load(f), None
    except FileNotFoundError:
        return None, None
    except tomllib.TOMLDecodeError as e:
        return None, f"{path}: not valid TOML: {e}"


def check(path, base_path=None):
    """Return (outputs, errors) for the toml text at `path`.

    base_path: the base branch's copy, on a pull request; image and services come from it.
    None means `path` is merged code (a push to the default branch) and supplies everything.
    """
    cfg, err = load(path)
    if err:
        return {}, [err]
    if cfg is None:
        return {"enabled": "false"}, []
    base = cfg
    if base_path is not None:
        base, err = load(base_path)
        if err:
            return {}, [err]
        if base is None:
            print("ci-test: ci.toml is new in this PR; the image is taken from base, so tests run after merge",
                  file=sys.stderr)
            return {"enabled": "false"}, []

    errs = []
    for k in sorted(set(cfg) - TOP_KEYS):
        errs.append(f"unknown key {k!r} (allowed: {', '.join(sorted(TOP_KEYS))})")

    # The PR's own image and services are checked too, so a typo fails here and not after the
    # merge, but on a PR only the base's are used.
    image, pg = infra(cfg, errs)
    if base is not cfg:
        image, pg = infra(base, errs, "base ci.toml: ")
        if (cfg.get("image"), cfg.get("services")) != (base.get("image"), base.get("services")):
            print("::warning::image / services in this PR's ci.toml differ from base; the base's are used until this merges",
                  file=sys.stderr)

    test = need_str(cfg, "test", "", None, errs)
    if test and len(test.encode()) > MAX_TEST_BYTES:
        errs.append(f"test is longer than {MAX_TEST_BYTES} bytes: put it in a script in the repo")

    workdir = cfg.get("workdir", ".")
    if not isinstance(workdir, str) or not WORKDIR.fullmatch(workdir) \
            or workdir.startswith("/") or ".." in workdir.split("/"):
        errs.append(f"workdir = {workdir!r} must be a relative path of [A-Za-z0-9._/-] that does not start with '-', without '..'")

    timeout = cfg.get("timeout_minutes", DEFAULT_TIMEOUT)
    if isinstance(timeout, bool) or not isinstance(timeout, int) or timeout < 1:
        errs.append(f"timeout_minutes = {timeout!r} must be a whole number of minutes, 1 or more")
    elif timeout > MAX_TIMEOUT:
        print(f"::warning::ci.toml timeout_minutes = {timeout} is capped at {MAX_TIMEOUT}", file=sys.stderr)
        timeout = MAX_TIMEOUT

    if errs:
        return {}, errs
    out = {
        "enabled": "true",
        "image": image,
        "test_b64": base64.b64encode(test.encode()).decode(),
        "workdir": workdir,
        "timeout": str(timeout),
        "postgres": "true" if pg else "false",
    }
    if pg:
        out.update({"pg_image": pg["image"], "pg_user": pg["POSTGRES_USER"],
                    "pg_password": pg["POSTGRES_PASSWORD"], "pg_db": pg["POSTGRES_DB"]})
    return out, []


def main():
    ap = argparse.ArgumentParser(description="Validate .forgejo/ci.toml for the managed test job")
    ap.add_argument("--file", default=".forgejo/ci.toml")
    ap.add_argument("--base-file", help="the base branch's ci.toml (a pull request); image and services come from it")
    args = ap.parse_args()
    out, errs = check(args.file, args.base_file)
    for e in errs:
        print(f"ci-test: {e}", file=sys.stderr)
    if errs:
        sys.exit(1)
    if out["enabled"] == "false":
        print("ci-test: no ci.toml, nothing to test", file=sys.stderr)
    for k, v in out.items():
        print(f"{k}={v}")


if __name__ == "__main__":
    main()
