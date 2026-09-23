#!/usr/bin/env python3
"""Generate the README limitations block and docs/limitations.md from release/limitations.json.

    python scripts/limitations.py --write   regenerate both files
    python scripts/limitations.py --check   fail if either differs from what the source produces
"""

import argparse
import sys

import releaselib as lib


def targets():
    return {
        lib.ROOT / "README.md": lambda text, items: lib.readme_with_block(text, lib.render_readme_block(items)),
        lib.ROOT / "docs" / "limitations.md": lambda text, items: lib.render_limitations_doc(items),
    }


def out_of_sync():
    items = lib.limitations()
    stale = []
    for path, render in targets().items():
        current = path.read_text(encoding="utf-8") if path.is_file() else ""
        if render(current, items) != current:
            stale.append(str(path.relative_to(lib.ROOT)).replace("\\", "/"))
    return stale


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    g = ap.add_mutually_exclusive_group(required=True)
    g.add_argument("--write", action="store_true")
    g.add_argument("--check", action="store_true")
    args = ap.parse_args(argv)
    items = lib.limitations()
    if args.check:
        stale = out_of_sync()
        print("generated limitations are in sync" if not stale else "OUT OF SYNC with release/limitations.json: " + ", ".join(stale) + " (run scripts/limitations.py --write)")
        return 1 if stale else 0
    for path, render in targets().items():
        current = path.read_text(encoding="utf-8") if path.is_file() else ""
        path.write_text(render(current, items), encoding="utf-8", newline="")
        print("wrote", path.relative_to(lib.ROOT))
    return 0


if __name__ == "__main__":
    sys.exit(main())
