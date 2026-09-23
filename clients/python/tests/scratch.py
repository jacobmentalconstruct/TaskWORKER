"""Scratch directories for tests.

``tempfile.mkdtemp``/``TemporaryDirectory`` create owner-only directories on
Windows (Python 3.12+ applies a restrictive ACL), which a test service started as
a child process, or a differently sandboxed test runner, cannot always use or
clean up (WinError 5). Tests only need an ordinary private-by-location folder, so
these helpers create plain directories with the parent's inherited permissions.

Set TASKWORKER_TEST_TMP to place them somewhere other than the system temp dir.
"""

import contextlib
import os
import shutil
import tempfile
import uuid


def make_dir(prefix="tw-"):
    base = os.environ.get("TASKWORKER_TEST_TMP") or tempfile.gettempdir()
    path = os.path.join(base, prefix + uuid.uuid4().hex[:12])
    os.makedirs(path)
    return path


def remove_dir(path):
    shutil.rmtree(path, ignore_errors=True)


def scratch(testcase, prefix="tw-"):
    """A directory removed when the test finishes (after later-registered cleanups)."""
    path = make_dir(prefix)
    testcase.addCleanup(remove_dir, path)
    return path


@contextlib.contextmanager
def scratch_dir(prefix="tw-"):
    path = make_dir(prefix)
    try:
        yield path
    finally:
        remove_dir(path)
