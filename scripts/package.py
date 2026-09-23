#!/usr/bin/env python3
"""Assemble and inspect TaskWorker release archives (standard library only).

    python scripts/package.py build [--run-host-check]   assemble dist/ from build/, then inspect
    python scripts/package.py check                      inspect the archives already in dist/
    python scripts/package.py host-check                 execute this machine's packaged binary and record the evidence

Archives are built from an explicit allowlist, never from a copy of the repository.
`check` re-reads the finished archives and shares no state with `build`.
Expects build/taskworker-<os>-<arch>[.exe] for the six targets (scripts/build.ps1).
"""

import argparse
import gzip
import hashlib
import io
import json
import os
import pathlib
import platform
import re
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request
import zipfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
DIST = ROOT / "dist"
TARGETS = [(o, a) for o in ("windows", "darwin", "linux") for a in ("amd64", "arm64")]
EPOCH = (1980, 1, 1, 0, 0, 0)  # fixed timestamp: the same inputs give the same archive
ALLOWED = [re.compile(p) for p in (
    r"^taskworker(\.exe)?$",
    r"^(LICENSE|THIRD_PARTY_LICENSES\.txt|README\.md)$",
    r"^docs/[a-z]+\.md$",
    r"^clients/python/(README\.md|LICENSE|pyproject\.toml)$",
    r"^clients/python/src/taskworker_client/[a-z_]+\.py$",
    r"^clients/python/examples/[a-z_]+\.py$",
)]
REQUIRED = ["LICENSE", "THIRD_PARTY_LICENSES.txt", "README.md", "docs/http.md", "docs/mcp.md",
            "docs/python.md", "clients/python/pyproject.toml", "clients/python/src/taskworker_client/__init__.py"]
# Names that must never appear in a release, and strings that must not appear in any text file.
FORBIDDEN_NAMES = (".dev-logs", ".cache", ".tools", "_projectmapper", "testservice", "fakeworker",
                   "_test.", "__pycache__", ".sqlite", ".env", ".pem", ".key", "/data/", ".git/")
# Paths and addresses that would leak a machine or an account. A name (the licence holder) is not one of them.
FORBIDDEN_TEXT = (b"c:\\users", b"c:/users", b"/users/", b"/home/", b".dev-logs", b"_projectmapper", b"@gmail.", b"@users.noreply.")
# A maintainer can add machine- or account-specific strings (one per line, matched without regard to case) in a local,
# untracked file. It is never committed, so the source does not name them.
_PRIVATE_TERMS = pathlib.Path(__file__).resolve().parent.parent / ".release-private-terms"
if _PRIVATE_TERMS.is_file():
    FORBIDDEN_TEXT += tuple(t.strip().lower().encode() for t in _PRIVATE_TERMS.read_text(encoding="utf-8").splitlines() if t.strip())
GO_MACHINE = {("windows", 0x8664): "amd64", ("windows", 0xAA64): "arm64", ("linux", 62): "amd64", ("linux", 183): "arm64",
              ("darwin", 0x01000007): "amd64", ("darwin", 0x0100000C): "arm64"}


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def read_version():
    """The one source of truth is the CLI constant; the Python metadata must agree."""
    v = re.search(r'const Version = "([^"]+)"', (ROOT / "internal/adapters/cli/cli.go").read_text(encoding="utf-8")).group(1)
    py = re.search(r'^version = "([^"]+)"', (ROOT / "clients/python/pyproject.toml").read_text(encoding="utf-8"), re.M).group(1)
    init = re.search(r'__version__ = "([^"]+)"', (ROOT / "clients/python/src/taskworker_client/__init__.py").read_text(encoding="utf-8")).group(1)
    if not (v == py == init):
        sys.exit(f"version mismatch: cli.go {v!r}, pyproject {py!r}, __init__ {init!r}")
    return v


def binary_arch(data, os_name):
    """(os, arch) of an executable read from its header, or None if it is not a recognized one."""
    try:
        if data[:2] == b"MZ":
            off = struct.unpack("<I", data[0x3C:0x40])[0]
            if data[off:off + 4] != b"PE\0\0":
                return None
            return "windows", GO_MACHINE.get(("windows", struct.unpack("<H", data[off + 4:off + 6])[0]))
        if data[:4] == b"\x7fELF" and data[4] == 2 and data[5] == 1:
            return "linux", GO_MACHINE.get(("linux", struct.unpack("<H", data[18:20])[0]))
        if data[:4] == b"\xcf\xfa\xed\xfe":
            return "darwin", GO_MACHINE.get(("darwin", struct.unpack("<I", data[4:8])[0]))
    except (struct.error, IndexError):
        pass
    return None


def third_party_licenses(go_root):
    lines = ["THIRD-PARTY LICENSES", "",
             "The taskworker executable contains the Go runtime and standard library, and the vendored",
             "modules below. TaskWorker itself is MIT licensed (see LICENSE).", ""]
    mods = []
    for line in (ROOT / "vendor/modules.txt").read_text(encoding="utf-8").splitlines():
        m = re.match(r"^# (\S+) (v\S+)$", line)
        if m:
            mods.append(m.groups())
    if not mods:
        sys.exit("vendor/modules.txt lists no modules")
    entries = [("Go (runtime and standard library)", go_root / "LICENSE")]
    for mod, ver in mods:
        entries.append((f"{mod} {ver}", ROOT / "vendor" / mod / "LICENSE"))
    for title, path in entries:
        if not path.is_file():
            sys.exit(f"missing licence file for {title}: {path}")
        lines += ["=" * 78, title, "=" * 78, "", path.read_text(encoding="utf-8").strip(), ""]
    return "\n".join(lines).encode("utf-8")


def find_go_root(arg):
    for c in (arg, os.environ.get("GOROOT"), str(ROOT / ".tools/go")):
        if c and (pathlib.Path(c) / "LICENSE").is_file():
            return pathlib.Path(c)
    sys.exit("cannot find the Go licence file: pass --go-root or set GOROOT")


def stage_files(os_name, arch, version, go_root):
    """name -> (bytes, is_executable) for one target, from the allowlist only."""
    exe = "taskworker.exe" if os_name == "windows" else "taskworker"
    src = ROOT / "build" / f"taskworker-{os_name}-{arch}{'.exe' if os_name == 'windows' else ''}"
    if not src.is_file():
        sys.exit(f"missing {src}: run scripts/build.ps1 first")
    files = {exe: (src.read_bytes(), True)}
    files["LICENSE"] = ((ROOT / "LICENSE").read_bytes(), False)
    files["THIRD_PARTY_LICENSES.txt"] = (third_party_licenses(go_root), False)
    files["README.md"] = ((ROOT / "README.md").read_bytes(), False)
    for p in sorted((ROOT / "docs").glob("*.md")):
        files[f"docs/{p.name}"] = (p.read_bytes(), False)
    py = ROOT / "clients/python"
    for name in ("README.md", "LICENSE", "pyproject.toml"):
        files[f"clients/python/{name}"] = ((py / name).read_bytes(), False)
    for p in sorted((py / "src/taskworker_client").glob("*.py")):
        files[f"clients/python/src/taskworker_client/{p.name}"] = (p.read_bytes(), False)
    for p in sorted((py / "examples").glob("*.py")):
        files[f"clients/python/examples/{p.name}"] = (p.read_bytes(), False)
    if (py / "LICENSE").read_bytes() != (ROOT / "LICENSE").read_bytes():
        sys.exit("clients/python/LICENSE differs from LICENSE")
    return files


def write_archive(os_name, arch, version, files):
    top = f"taskworker-{version}-{os_name}-{arch}"
    DIST.mkdir(exist_ok=True)
    if os_name == "windows":
        path = DIST / f"{top}.zip"
        with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
            for name in sorted(files):
                data, is_exe = files[name]
                zi = zipfile.ZipInfo(f"{top}/{name}", EPOCH)
                zi.compress_type = zipfile.ZIP_DEFLATED
                zi.create_system = 3  # zipfile stamps the building OS (0 on Windows, 3 elsewhere): pin it so the bytes match everywhere
                zi.external_attr = (0o755 if is_exe else 0o644) << 16
                z.writestr(zi, data)
    else:
        path = DIST / f"{top}.tar.gz"
        raw = io.BytesIO()
        with tarfile.open(fileobj=raw, mode="w", format=tarfile.PAX_FORMAT) as t:
            for name in sorted(files):
                data, is_exe = files[name]
                ti = tarfile.TarInfo(f"{top}/{name}")
                ti.size, ti.mode, ti.mtime = len(data), 0o755 if is_exe else 0o644, 315532800
                ti.uid = ti.gid = 0
                ti.uname = ti.gname = ""
                t.addfile(ti, io.BytesIO(data))
        with open(path, "wb") as out, gzip.GzipFile(fileobj=out, mode="wb", mtime=0, compresslevel=9) as g:
            g.write(raw.getvalue())
    return path


def read_archive(path):
    """member name (without the top directory) -> (bytes, mode) plus the top directory name."""
    members, tops = {}, set()
    if path.suffix == ".zip":
        with zipfile.ZipFile(path) as z:
            for zi in z.infolist():
                if zi.is_dir():
                    continue
                top, _, rest = zi.filename.partition("/")
                tops.add(top)
                members[rest] = (z.read(zi), (zi.external_attr >> 16) & 0o777)
    else:
        with tarfile.open(path, "r:gz") as t:
            for ti in t.getmembers():
                if not ti.isfile():
                    return None, f"{ti.name}: only regular files are allowed"
                top, _, rest = ti.name.partition("/")
                tops.add(top)
                members[rest] = (t.extractfile(ti).read(), ti.mode & 0o777)
    if len(tops) != 1:
        return None, f"expected one top directory, found {sorted(tops)}"
    return (members, tops.pop()), None


def inspect(path, os_name, arch, version):
    """Independent checks of one finished archive; returns a list of problems (empty = passes)."""
    problems = []
    got, err = read_archive(path)
    if err:
        return [err]
    members, top = got
    if top != f"taskworker-{version}-{os_name}-{arch}":
        problems.append(f"top directory {top!r} does not match the target")
    exe = "taskworker.exe" if os_name == "windows" else "taskworker"
    for name, (data, mode) in members.items():
        if not any(p.match(name) for p in ALLOWED):
            problems.append(f"not on the allowlist: {name}")
        low = name.lower()
        for bad in FORBIDDEN_NAMES:
            if bad in "/" + low:
                problems.append(f"forbidden name {bad!r} in {name}")
        if name != exe and b"\0" not in data[:4096]:
            lowered = data.lower()
            for needle in FORBIDDEN_TEXT:
                if needle in lowered:
                    problems.append(f"{name} contains {needle.decode()!r}")
    for r in REQUIRED + [exe]:
        if r not in members:
            problems.append(f"missing required member {r}")
    if exe in members:
        data, mode = members[exe]
        if os_name != "windows" and mode != 0o755:
            problems.append(f"{exe} mode is {oct(mode)}, expected 0o755")
        info = binary_arch(data, os_name)
        if info != (os_name, arch):
            problems.append(f"{exe} header says {info}, expected {(os_name, arch)}")
        if len(data) < 5_000_000:
            problems.append(f"{exe} is only {len(data)} bytes")
        if f"taskworker {version}".encode() not in data and version.encode() not in data:
            problems.append(f"{exe} does not contain the version string {version}")
    return problems


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def host_target():
    """(os, arch) of the machine running this script in the naming of the release targets, or None."""
    os_name = {"win32": "windows", "linux": "linux", "darwin": "darwin"}.get(sys.platform)
    arch = {"amd64": "amd64", "x86_64": "amd64", "arm64": "arm64", "aarch64": "arm64"}.get(platform.machine().lower())
    return (os_name, arch) if os_name and arch else None


def archive_path(version, os_name, arch):
    return DIST / f"taskworker-{version}-{os_name}-{arch}.{'zip' if os_name == 'windows' else 'tar.gz'}"


def extract_archive(archive, work):
    """Unpack a release archive, keeping the executable bit (a .tar.gz stores it; a zip is Windows-only)."""
    if archive.suffix == ".zip":
        with zipfile.ZipFile(archive) as z:
            z.extractall(work)
    else:
        with tarfile.open(archive, "r:gz") as t:
            if hasattr(tarfile, "data_filter"):
                t.extractall(work, filter="data")
            else:
                t.extractall(work)


def host_check(archive, version, os_name):
    """Execute the packaged binary for THIS host: version, help, serve + UI + packaged Python client, mcp startup, stop."""
    # A Windows scratch directory under dist/ (the temp dir is owner-only there); a real temp dir elsewhere,
    # so the executable bit and the file system behave like a normal installation.
    if os_name == "windows":
        work = DIST / ".check"
        shutil.rmtree(work, ignore_errors=True)
        work.mkdir(parents=True)
    else:
        work = pathlib.Path(tempfile.mkdtemp(prefix="twcheck-"))
    result = {"platform": platform.platform(), "machine": platform.machine(), "python": platform.python_version()}
    try:
        extract_archive(archive, work)
        pkg = next(work.iterdir())
        exe = pkg / ("taskworker.exe" if os_name == "windows" else "taskworker")
        if os_name != "windows":
            assert os.access(exe, os.X_OK), "the extracted executable is not executable"
        out = subprocess.run([str(exe), "version"], capture_output=True, text=True, timeout=30)
        result["version"] = out.stdout.strip()
        assert out.returncode == 0 and out.stdout.strip() == f"taskworker {version}", out
        out = subprocess.run([str(exe), "help"], capture_output=True, text=True, timeout=30)
        assert out.returncode == 0 and "Usage: taskworker" in out.stdout, out
        port = free_port()
        url = f"http://127.0.0.1:{port}"
        data_dir = work / "data"
        log = open(work / "serve.log", "wb")
        p = subprocess.Popen([str(exe), "serve", "--server", url, "--data-dir", str(data_dir)], stdout=log, stderr=log,
                             creationflags=getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0), cwd=work)
        try:
            for _ in range(100):
                try:
                    with urllib.request.urlopen(url + "/v1/health", timeout=2) as r:
                        health = json.loads(r.read())
                    break
                except OSError:
                    time.sleep(0.1)
            else:
                raise AssertionError("service did not become healthy")
            result["health"] = health
            req = urllib.request.Request(url + "/")
            with urllib.request.urlopen(req, timeout=5) as r:
                result["ui"] = {"status": r.status, "content_type": r.headers.get("Content-Type"), "bytes": len(r.read())}
            assert result["ui"]["status"] == 200 and "text/html" in result["ui"]["content_type"]
            env = dict(os.environ, PYTHONPATH=str(pkg / "clients/python/src"), TASKWORKER_SERVER=url)
            py = subprocess.run([sys.executable, "-c", "import taskworker_client as t; print(t.__version__, t.Client().health()['status'])"],
                                capture_output=True, text=True, timeout=30, cwd=work, env=env)
            result["python_client"] = py.stdout.strip()
            assert py.returncode == 0 and py.stdout.split()[0] == version, py
            probe = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "server/discover", "params": {"_meta": {
                "io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": {},
                "io.modelcontextprotocol/clientInfo": {"name": "package-check", "version": "0"}}}})
            m = subprocess.Popen([str(exe), "mcp", "--server", url], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            m.stdin.write((probe + "\n").encode())
            m.stdin.flush()
            line = m.stdout.readline().decode()
            m.stdin.close()
            code = m.wait(10)
            assert '"supportedVersions"' in line and code == 0, (line, code)
            result["mcp_startup"] = "ok"
        finally:
            if os.name == "nt":
                p.send_signal(signal.CTRL_BREAK_EVENT)
            else:
                p.terminate()  # SIGTERM: the service shuts down gracefully
            try:
                result["service_exit_code"] = p.wait(15)
            except subprocess.TimeoutExpired:
                p.kill()
                result["service_exit_code"] = "killed"
            log.close()
        assert result["service_exit_code"] == 0, result
        result["passed"] = True
    finally:
        shutil.rmtree(work, ignore_errors=True)
    return result


def record_host_check(target_key, binary_sha, result):
    """Write the passed host check into release/attestations.json, bound to the exact executable."""
    import releaselib as lib
    item = f"host-check:{target_key}"
    by = f"scripts/package.py on {result['platform']} ({result['machine']})"
    binds = {f"binary:{target_key}": binary_sha}
    if any(a["id"] == item and a["binds"] == binds and a["by"] == by for a in lib.attestations()):
        return "already recorded"
    note = "executed from the extracted archive: version, help, serve, embedded UI, packaged Python client, mcp startup, graceful stop"
    lib.add_attestation(lib.make_attestation(item, "auto", by, note, binds))
    return "recorded"


def cmd_host_check(args):
    """Run the host check for this machine's target against the archives in dist/ and record the evidence."""
    version = read_version()
    target = host_target()
    if not target:
        print(f"no release target matches this host ({sys.platform}, {platform.machine()})")
        return 1
    os_name, arch = target
    archive = archive_path(version, os_name, arch)
    if not archive.is_file():
        print(f"missing {archive.name}: run package.py build first")
        return 1
    got, err = read_archive(archive)
    if err:
        print(err)
        return 1
    members, _ = got
    exe_name = "taskworker.exe" if os_name == "windows" else "taskworker"
    binary_sha = sha256(members[exe_name][0])
    res = host_check(archive, version, os_name)
    print("host check:", json.dumps(res))
    if not args.no_record:
        print(f"attestation host-check:{os_name}/{arch}:", record_host_check(f"{os_name}/{arch}", binary_sha, res))
    return 0


def cmd_build(args):
    version = read_version()
    go_root = find_go_root(args.go_root)
    shutil.rmtree(DIST, ignore_errors=True)
    manifest = {"version": version, "targets": {}}
    for os_name, arch in TARGETS:
        files = stage_files(os_name, arch, version, go_root)
        path = write_archive(os_name, arch, version, files)
        exe = "taskworker.exe" if os_name == "windows" else "taskworker"
        manifest["targets"][f"{os_name}/{arch}"] = {
            "archive": path.name, "archive_bytes": path.stat().st_size, "archive_sha256": sha256(path.read_bytes()),
            "binary_bytes": len(files[exe][0]), "binary_sha256": sha256(files[exe][0]),
            "execution": "not executed (cross-compiled; header and contents inspected only)"}
        print(f"built {path.name}  {path.stat().st_size} bytes")
    host_result = None
    if args.run_host_check:
        target = host_target()
        if target and f"{target[0]}/{target[1]}" in manifest["targets"]:
            key = f"{target[0]}/{target[1]}"
            host_result = host_check(DIST / manifest["targets"][key]["archive"], version, target[0])
            manifest["targets"][key]["execution"] = "executed on this host from the extracted archive"
            manifest["targets"][key]["host_check"] = host_result
            print("host check:", json.dumps(host_result))
        else:
            print(f"host check skipped: no release target matches this host ({sys.platform}, {platform.machine()})")
    problems = cmd_check(args, quiet=True)
    manifest["inspection"] = "pass" if not problems else problems
    (DIST / "release-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    (DIST / "SHA256SUMS").write_text("".join(f"{t['archive_sha256']}  {t['archive']}\n" for t in manifest["targets"].values()), encoding="utf-8", newline="\n")
    if host_result and not problems:
        key = f"{host_target()[0]}/{host_target()[1]}"
        print(f"attestation host-check:{key}:", record_host_check(key, manifest["targets"][key]["binary_sha256"], host_result))
    if problems:
        print("\n".join("PROBLEM: " + p for p in problems))
        return 1
    print(f"all {len(TARGETS)} archives passed inspection; dist/release-manifest.json and dist/SHA256SUMS written")
    return 0


def cmd_check(args, quiet=False):
    version = read_version()
    problems = []
    seen = 0
    for os_name, arch in TARGETS:
        ext = "zip" if os_name == "windows" else "tar.gz"
        path = DIST / f"taskworker-{version}-{os_name}-{arch}.{ext}"
        if not path.is_file():
            problems.append(f"missing archive {path.name}")
            continue
        seen += 1
        problems += [f"{path.name}: {p}" for p in inspect(path, os_name, arch, version)]
    extra = sorted(p.name for p in DIST.iterdir() if p.name not in ("release-manifest.json", "SHA256SUMS", "RELEASE_NOTES.md") and not p.name.startswith("taskworker-" + version))
    problems += [f"unexpected file in dist/: {e}" for e in extra]
    sums = DIST / "SHA256SUMS"
    if sums.is_file():
        for line in sums.read_text(encoding="utf-8").splitlines():
            digest, _, name = line.partition("  ")
            if (DIST / name).is_file() and sha256((DIST / name).read_bytes()) != digest:
                problems.append(f"SHA256SUMS does not match {name}")
    if quiet:
        return problems
    print("\n".join("PROBLEM: " + p for p in problems) if problems else f"{seen} archives passed inspection")
    return 1 if problems else 0


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    b = sub.add_parser("build")
    b.add_argument("--run-host-check", action="store_true")
    b.add_argument("--go-root")
    b.set_defaults(fn=cmd_build)
    c = sub.add_parser("check")
    c.set_defaults(fn=cmd_check)
    h = sub.add_parser("host-check", help="execute the packaged binary for this machine and record the evidence")
    h.add_argument("--no-record", action="store_true")
    h.set_defaults(fn=cmd_host_check)
    a = ap.parse_args()
    sys.exit(a.fn(a))
