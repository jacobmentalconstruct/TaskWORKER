"""Shared helpers for the examples (not part of the library)."""

import argparse
import sys


def utf8_stdout():
    # Model output is Unicode; do not depend on the console code page.
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="replace")


def parser(description):
    p = argparse.ArgumentParser(description=description)
    p.add_argument("--server", help="service URL (default: TASKWORKER_SERVER or http://127.0.0.1:7433)")
    return p
