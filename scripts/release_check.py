#!/usr/bin/env python3
"""The release gate: run every automatable check, verify the recorded evidence, say READY or NOT READY.

    python scripts/release_check.py                     run all checks (about a minute)
    python scripts/release_check.py --fast              quick feedback; skips the slow checks and can never say READY
    python scripts/release_check.py attest ITEM --by NAME --note TEXT
                                                        record a manual check (prints its steps first)
    python scripts/release_check.py notes               write dist/RELEASE_NOTES.md from the recorded evidence

What it enforces (see docs/development.md, "Release process"):
  * the git tree is clean, versions agree, the licence files match;
  * README.md and docs/limitations.md match release/limitations.json, and a limitation can only assert facts
    that have valid evidence and cannot be marked closed without it;
  * dist/ archives pass inspection and the gate's own tests pass;
  * a fresh clone of HEAD rebuilds every release executable byte for byte;
  * the last full local verification (scripts/verify.ps1) passed on these exact binaries;
  * every check in release/manual-checks.json "required_for_release" has an attestation that still holds
    (bound to the binary, the UI assets or the commit it vouches for).
Exit status 0 only for READY.
"""

import argparse
import hashlib
import json
import os
import shutil
import stat
import subprocess
import sys
import types

import limitations as limmod
import package
import releaselib as lib

PASS, FAIL, MISSING, WARN, NOTRUN = "PASS", "FAIL", "MISSING", "WARN", "NOT RUN"
BLOCKING = (FAIL, MISSING, NOTRUN)


class Result:
    def __init__(self, name, status, detail=""):
        self.name, self.status, self.detail = name, status, detail


def rmtree(path):
    """Remove a directory tree even when it holds read-only files (a git clone's object files on Windows)."""
    def fix(func, p, _exc):
        os.chmod(p, stat.S_IWRITE)
        func(p)
    if sys.version_info >= (3, 12):
        shutil.rmtree(path, onexc=fix)
    else:
        shutil.rmtree(path, onerror=fix)


# ---- automatable checks --------------------------------------------------------------

def check_git_clean():
    dirty = [l for l in lib.git("status", "--porcelain").splitlines() if l.strip()]
    if dirty:
        return Result("git tree is clean", FAIL, f"{len(dirty)} uncommitted path(s), e.g. {dirty[0].strip()} (commit first)")
    return Result("git tree is clean", PASS, f"HEAD {lib.head_commit()[:10]} on {lib.git('branch', '--show-current').strip() or 'detached HEAD'}")


def check_versions():
    try:
        return Result("versions agree", PASS, package.read_version())
    except SystemExit as e:
        return Result("versions agree", FAIL, str(e))


def check_licence():
    a, b = (lib.ROOT / "LICENSE"), (lib.ROOT / "clients/python/LICENSE")
    if not (a.is_file() and b.is_file()):
        return Result("licence files", FAIL, "LICENSE or clients/python/LICENSE is missing")
    if a.read_bytes() != b.read_bytes():
        return Result("licence files", FAIL, "LICENSE and clients/python/LICENSE differ")
    line = next((l for l in a.read_text(encoding="utf-8").splitlines() if l.startswith("Copyright")), "no copyright line")
    return Result("licence files", PASS if "Copyright (c)" in line else FAIL, line)


def check_limitations_sync():
    stale = limmod.out_of_sync()
    return Result("limitations are in sync", FAIL if stale else PASS,
                  ("out of date: " + ", ".join(stale) + " (run scripts/limitations.py --write)") if stale else "README.md and docs/limitations.md match release/limitations.json")


def check_limitations_evidence():
    errors, warnings = lib.limitation_problems(lib.limitations())
    if errors:
        return Result("limitations have evidence", FAIL, "; ".join(errors[:3]))
    if warnings:
        return Result("limitations have evidence", WARN, "; ".join(warnings[:3]))
    return Result("limitations have evidence", PASS, f"{len(lib.limitations())} limitations, every asserted fact has valid evidence")


def check_archives():
    m = lib.manifest()
    if not m:
        return Result("release archives", FAIL, "dist/release-manifest.json is missing (run scripts/package.ps1)")
    try:
        version = package.read_version()
    except SystemExit as e:
        return Result("release archives", FAIL, str(e))
    if m.get("version") != version:
        return Result("release archives", FAIL, f"dist/ holds version {m.get('version')}, the source says {version}: rebuild")
    problems = package.cmd_check(types.SimpleNamespace(), quiet=True)
    if problems:
        return Result("release archives", FAIL, f"{len(problems)} problem(s): {problems[0]}")
    return Result("release archives", PASS, f"{len(m['targets'])} archives inspected")


def check_gate_tests():
    p = subprocess.run([sys.executable, str(lib.ROOT / "scripts/test_package.py")], cwd=lib.ROOT, capture_output=True, text=True)
    tail = (p.stderr or p.stdout).strip().splitlines()
    ran = next((l for l in tail if l.startswith("Ran ")), "")
    return Result("release gate tests", PASS if p.returncode == 0 else FAIL, ran or (tail[-1] if tail else "no output"))


def find_go():
    exe = "go.exe" if os.name == "nt" else "go"
    local = lib.ROOT / ".tools" / "go" / "bin" / exe
    return str(local) if local.is_file() else shutil.which("go")


def check_reproducible():
    m, go = lib.manifest(), find_go()
    name = "release is reproducible from HEAD"
    if not m:
        return Result(name, FAIL, "needs dist/")
    if not go:
        return Result(name, FAIL, "needs a Go toolchain (.tools/go or PATH)")
    work = lib.ROOT / ".cache" / f"repro-{os.getpid()}"
    if work.exists():
        rmtree(work)
    try:
        lib.git("clone", "--quiet", str(lib.ROOT), str(work))
        env = dict(os.environ, GOTOOLCHAIN="local", GOENV="off", GOWORK="off", GOFLAGS="", CGO_ENABLED="0", GOPROXY="off",
                   GOSUMDB="off", GOCACHE=str(lib.ROOT / ".cache" / "go-build"))
        env.pop("GOOS", None), env.pop("GOARCH", None)
        differ = []
        for key, t in m["targets"].items():
            os_name, arch = key.split("/")
            out = work / "out" / f"{os_name}-{arch}"
            p = subprocess.run([go, "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", str(out), "./cmd/taskworker"],
                               cwd=work, env=dict(env, GOOS=os_name, GOARCH=arch), capture_output=True, text=True)
            if p.returncode != 0:
                return Result(name, FAIL, f"build of {key} failed: {p.stderr.strip()[:200]}")
            if hashlib.sha256(out.read_bytes()).hexdigest() != t["binary_sha256"]:
                differ.append(key)
        if differ:
            return Result(name, FAIL, "a fresh clone builds different executables for: " + ", ".join(differ))
        return Result(name, PASS, f"a fresh clone of HEAD rebuilds all {len(m['targets'])} executables byte for byte")
    except RuntimeError as e:
        return Result(name, FAIL, str(e))
    finally:
        if work.exists():
            rmtree(work)


def check_local_verification():
    name = "full local verification"
    p = lib.ROOT / ".cache" / "verify" / "evidence.json"
    try:
        ev = json.loads(p.read_text(encoding="utf-8-sig")) if p.is_file() else None  # PowerShell writes a BOM
    except ValueError:
        ev = None
    if ev is None:
        return Result(name, FAIL, "no .cache/verify/evidence.json: run scripts/verify.ps1")
    m = lib.manifest() or {}
    want = m.get("targets", {}).get("windows/amd64", {}).get("binary_sha256", "")
    if ev.get("result") != "pass" or ev.get("go_tests_fail") not in (0, None):
        return Result(name, FAIL, f"the last run did not pass ({ev.get('result')})")
    if str(ev.get("taskworker_sha256", "")).lower() != want.lower():
        return Result(name, FAIL, "the last verification ran on a different executable than dist/ holds: rerun scripts/verify.ps1")
    return Result(name, PASS, f"{ev.get('go_tests_pass')} Go tests, Python {', '.join(f'{k} {v}' for k, v in (ev.get('python') or {}).items())}")


def check_attestations(required, manual):
    out = []
    for item in required:
        att, why = lib.latest_valid(item)
        if att:
            out.append(Result(f"evidence: {item}", PASS, f"{att['by']} on {att['at']}"))
        else:
            how = ("run: python scripts/package.py host-check" if item.startswith("host-check:")
                   else f"do it, then: python scripts/release_check.py attest {item} --by \"NAME\" --note \"...\"" if item in manual else "")
            out.append(Result(f"evidence: {item}", MISSING, f"{why}. {how}".strip()))
    return out


# ---- commands ------------------------------------------------------------------------

def load_manual():
    return lib.load_json(lib.release_dir() / "manual-checks.json", {"required_for_release": [], "manual": {}})


def run_checks(fast=False):
    m = load_manual()
    results = [check_git_clean(), check_versions(), check_licence(), check_limitations_sync(), check_limitations_evidence(), check_archives()]
    if fast:
        results += [Result("release gate tests", NOTRUN, "skipped by --fast"), Result("release is reproducible from HEAD", NOTRUN, "skipped by --fast")]
    else:
        results += [check_gate_tests(), check_reproducible()]
    results.append(check_local_verification())
    results += check_attestations(m["required_for_release"], m["manual"])
    return results


def cmd_check(args):
    results = run_checks(args.fast)
    width = max(len(r.name) for r in results)
    for r in results:
        print(f"[{r.status:^7}] {r.name:<{width}}  {r.detail}")
    on_record = [a for a in lib.attestations() if lib.attestation_problem(a) is None and a["id"] not in load_manual()["required_for_release"]]
    if on_record:
        print("\nOther evidence on record that still holds:")
        for a in on_record:
            print(f"  - {a['id']}: {a['by']} on {a['at']}")
    blocking = [r for r in results if r.status in BLOCKING]
    print()
    if blocking:
        print(f"NOT READY: {len(blocking)} blocking item(s)" + (" (this was a --fast run, which can never say READY)" if args.fast else ""))
        return 1
    warns = [r for r in results if r.status == WARN]
    print("READY TO TAG" + (f" ({len(warns)} warning(s) above)" if warns else ""))
    print("Next: python scripts/release_check.py notes   then tag and attach dist/ (see docs/development.md).")
    return 0


def cmd_attest(args):
    manual = load_manual()["manual"]
    spec = manual.get(args.item)
    if not spec:
        print(f"'{args.item}' is not a manual check. Manual checks: {', '.join(sorted(manual))}")
        return 2
    print(f"{spec['title']}\nSteps:")
    for i, step in enumerate(spec["steps"], 1):
        print(f"  {i}. {step}")
    if not (args.by and args.note):
        print("\nNothing recorded. Do the steps, then rerun with --by \"Your Name\" --note \"what you saw\".")
        return 2
    binds = lib.current_bindings(spec["binds"])
    missing = [k for k, v in binds.items() if v is None]
    if missing:
        print(f"cannot record: no current value for {', '.join(missing)} (build dist/ first)")
        return 1
    if "commit" in binds:
        extra = [p for p in lib.changed_since("HEAD") if p not in lib.ALLOWED_AFTER_COMMIT]
        if extra:
            print(f"cannot record against a commit while the tree differs from it ({extra[0]}...): commit or stash first")
            return 1
    lib.add_attestation(lib.make_attestation(args.item, "manual", args.by, args.note, binds))
    print(f"\nrecorded {args.item} by {args.by}, bound to " + ", ".join(f"{k}={v[:10]}" for k, v in binds.items()))
    return 0


def render_notes(version, m, atts, items):
    valid = lambda item: lib.latest_valid(item, atts)[0]
    required = load_manual()["required_for_release"]
    ready = all(valid(r) for r in required)
    out = [f"# TaskWorker {version}" + ("" if ready else " (DRAFT: required evidence is missing)"), "",
           "A small local inference worker that people and agents share: browser UI, CLI, Python client and an MCP bridge",
           "all control the same jobs. Inference is done by an installed Ollama 0.18.3.", "",
           "## Downloads", "", "| Platform | File | SHA-256 | Executed |", "| --- | --- | --- | --- |"]
    for key, t in m["targets"].items():
        att = valid(f"host-check:{key}")
        ran = f"yes ({att['by'].split(' on ', 1)[-1]})" if att else "no (cross-compiled, header inspected)"
        out.append(f"| {key} | `{t['archive']}` | `{t['archive_sha256']}` | {ran} |")
    out += ["", "Verify a download with `sha256sum -c SHA256SUMS` (or `Get-FileHash` on Windows). The executables are reproducible: a fresh",
            "`git clone` built with the flags in `docs/development.md` gives byte-identical binaries.", "", "## What was checked", ""]
    for r in required:
        att = valid(r)
        out.append(f"- `{r}`: " + (f"{att['by']}, {att['at']}. {att['note']}" if att else "**not recorded**"))
    out += ["", "## Known limitations", ""]
    out += [f"- **{it['title']}.** {it['text']}" for it in items if it["status"] != "closed"]
    return "\n".join(out) + "\n"


def cmd_notes(args):
    m = lib.manifest()
    if not m:
        print("dist/release-manifest.json is missing (run scripts/package.ps1)")
        return 1
    text = render_notes(m["version"], m, lib.attestations(), lib.limitations())
    path = lib.dist_dir() / "RELEASE_NOTES.md"
    path.write_text(text, encoding="utf-8", newline="\n")
    print(f"wrote {path.relative_to(lib.ROOT)}" + ("  (DRAFT: some required evidence is missing)" if "(DRAFT" in text.splitlines()[0] else ""))
    return 0


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--fast", action="store_true", help="skip the slow checks; never reports READY")
    sub = ap.add_subparsers(dest="cmd")
    a = sub.add_parser("attest", help="record a manual check")
    a.add_argument("item")
    a.add_argument("--by")
    a.add_argument("--note")
    a.set_defaults(fn=cmd_attest)
    n = sub.add_parser("notes", help="write dist/RELEASE_NOTES.md")
    n.set_defaults(fn=cmd_notes)
    args = ap.parse_args(argv)
    return (args.fn if getattr(args, "fn", None) else cmd_check)(args)


if __name__ == "__main__":
    sys.exit(main())
