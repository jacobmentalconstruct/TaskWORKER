#!/usr/bin/env python3
"""Measure the idle footprint of the packaged Windows amd64 service (standard library only).

    python scripts/measure_idle.py [--idle-seconds 60] [--watch-seconds 30]

Extracts dist/taskworker-<version>-windows-amd64.zip, starts `serve` on a private port and
data directory (no Ollama is contacted), waits for it to settle, then samples the process
twice: with no clients, and with one event-stream observer attached. Reports working set,
private bytes, threads, handles and CPU use over each window. Prints one JSON object.
Windows only (it reads process counters through wmic); numbers are for the host it ran on.
"""

import argparse
import csv
import io
import json
import os
import platform
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.request
import zipfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import package  # noqa: E402


def counters(pid):
    out = subprocess.run(["wmic", "process", "where", f"ProcessId={pid}", "get",
                          "HandleCount,KernelModeTime,PageFileUsage,ThreadCount,UserModeTime,WorkingSetSize", "/format:csv"],
                         capture_output=True, text=True, timeout=30).stdout
    rows = [r for r in csv.DictReader(io.StringIO(out.replace("\r", "").strip())) if r.get("ThreadCount")]
    r = rows[0]
    return {"working_set_mib": int(r["WorkingSetSize"]) / 2**20, "private_mib": int(r["PageFileUsage"]) / 1024,
            "threads": int(r["ThreadCount"]), "handles": int(r["HandleCount"]),
            "cpu_100ns": int(r["KernelModeTime"]) + int(r["UserModeTime"])}


def window(pid, seconds):
    a, t0 = counters(pid), time.monotonic()
    time.sleep(seconds)
    b, t1 = counters(pid), time.monotonic()
    cpu = (b["cpu_100ns"] - a["cpu_100ns"]) * 100e-9
    return {"seconds": round(t1 - t0, 1), "cpu_seconds": round(cpu, 3), "cpu_percent_of_one_core": round(100 * cpu / (t1 - t0), 3),
            "working_set_mib": round(b["working_set_mib"], 1), "private_mib": round(b["private_mib"], 1),
            "threads": b["threads"], "handles": b["handles"]}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--idle-seconds", type=int, default=60)
    ap.add_argument("--watch-seconds", type=int, default=30)
    args = ap.parse_args()
    if sys.platform != "win32":
        sys.exit("this measurement reads Windows process counters")
    version = package.read_version()
    archive = package.DIST / f"taskworker-{version}-windows-amd64.zip"
    work = package.ROOT / ".cache" / f"idle-{os.getpid()}"
    shutil.rmtree(work, ignore_errors=True)
    work.mkdir(parents=True)
    with zipfile.ZipFile(archive) as z:
        z.extractall(work)
    exe = next(work.iterdir()) / "taskworker.exe"
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        url = f"http://127.0.0.1:{s.getsockname()[1]}"
    log = open(work / "serve.log", "wb")
    started = time.monotonic()
    p = subprocess.Popen([str(exe), "serve", "--server", url, "--data-dir", str(work / "data")], stdout=log, stderr=log,
                         creationflags=subprocess.CREATE_NEW_PROCESS_GROUP)
    result = {"host": {"os": platform.platform(), "cpus": os.cpu_count(), "python": platform.python_version()},
              "binary": {"archive": archive.name, "executable_bytes": exe.stat().st_size, "version": version}}
    try:
        for _ in range(200):
            try:
                urllib.request.urlopen(url + "/v1/health", timeout=2).read()
                break
            except OSError:
                time.sleep(0.05)
        else:
            sys.exit("service never became healthy")
        result["startup_to_healthy_seconds"] = round(time.monotonic() - started, 2)
        time.sleep(5)  # let start-up work settle
        result["idle_no_clients"] = window(p.pid, args.idle_seconds)
        stop = threading.Event()

        def observe():  # one event-stream observer, like an open browser tab
            try:
                with urllib.request.urlopen(url + "/v1/events", timeout=60) as r:
                    while not stop.is_set():
                        if not r.readline():
                            return
            except OSError:
                pass

        t = threading.Thread(target=observe, daemon=True)
        t.start()
        time.sleep(2)
        result["idle_with_one_event_stream"] = window(p.pid, args.watch_seconds)
        stop.set()
    finally:
        p.send_signal(signal.CTRL_BREAK_EVENT)
        try:
            result["service_exit_code"] = p.wait(15)
        except subprocess.TimeoutExpired:
            p.kill()
            result["service_exit_code"] = "killed"
        log.close()
        shutil.rmtree(work, ignore_errors=True)
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
