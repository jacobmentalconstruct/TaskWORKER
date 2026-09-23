"""Tests for the release gate in scripts/package.py: the inspection must FAIL on tampered archives.

    python scripts/test_package.py

Needs the archives from `package.py build` in dist/ (run scripts/package.ps1 first).
Tampered copies are written under .cache/, never into dist/.
"""

import io
import pathlib
import shutil
import sys
import tarfile
import unittest
import uuid

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import package  # noqa: E402

REAL = package.DIST
VERSION = package.read_version()


class ReleaseGateTests(unittest.TestCase):
    def setUp(self):
        if not (REAL / f"taskworker-{VERSION}-linux-amd64.tar.gz").is_file():
            self.skipTest("no archives in dist/: run scripts/package.ps1 first")
        self.tmp = package.ROOT / ".cache" / ("pkgtest-" + uuid.uuid4().hex)
        self.tmp.mkdir(parents=True)
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.saved = package.DIST
        package.DIST = self.tmp
        self.addCleanup(setattr, package, "DIST", self.saved)

    def files(self, os_name, arch):
        got, err = package.read_archive(REAL / f"taskworker-{VERSION}-{os_name}-{arch}.{'zip' if os_name == 'windows' else 'tar.gz'}")
        self.assertIsNone(err)
        members, _ = got
        return {n: (d, bool(m & 0o111)) for n, (d, m) in members.items()}

    def tampered(self, os_name, arch, change):
        files = self.files(os_name, arch)
        change(files)
        return package.write_archive(os_name, arch, VERSION, files)

    def problems(self, path, os_name, arch):
        return package.inspect(path, os_name, arch, VERSION)

    def test_untouched_archives_pass(self):
        for os_name, arch in package.TARGETS:
            ext = "zip" if os_name == "windows" else "tar.gz"
            self.assertEqual(package.inspect(REAL / f"taskworker-{VERSION}-{os_name}-{arch}.{ext}", os_name, arch, VERSION), [], (os_name, arch))

    def test_development_record_is_rejected(self):
        p = self.tampered("linux", "amd64", lambda f: f.update({".dev-logs/CURRENT_STATE.md": (b"x", False)}))
        found = self.problems(p, "linux", "amd64")
        self.assertTrue(any(".dev-logs" in x for x in found), found)

    def test_unlisted_file_is_rejected(self):
        p = self.tampered("linux", "arm64", lambda f: f.update({"docs/notes.txt": (b"x", False)}))
        self.assertTrue(any("not on the allowlist" in x for x in self.problems(p, "linux", "arm64")))

    def test_personal_string_in_a_text_file_is_rejected(self):
        p = self.tampered("darwin", "arm64", lambda f: f.update({"README.md": (f["README.md"][0] + b"\nsee C:\\Users\\someone\\project\n", False)}))
        found = self.problems(p, "darwin", "arm64")
        self.assertTrue(any(x.startswith("README.md contains") for x in found), found)

    def test_a_local_project_path_or_an_email_address_is_rejected(self):
        for leak in (b"C:\\Users\\someone\\project\\notes", b"contact me at someone@gmail.com", b"12345+name@users.noreply.github.com"):
            p = self.tampered("linux", "amd64", lambda f, leak=leak: f.update({"docs/http.md": (f["docs/http.md"][0] + b"\n" + leak + b"\n", False)}))
            found = self.problems(p, "linux", "amd64")
            self.assertTrue(any(x.startswith("docs/http.md contains") for x in found), (leak, found))

    def test_the_licence_holders_name_is_allowed_in_the_licence(self):
        got = self.files("linux", "arm64")
        self.assertIn(b"Jacob Lambert", got["LICENSE"][0])
        self.assertEqual(self.problems(REAL / f"taskworker-{VERSION}-linux-arm64.tar.gz", "linux", "arm64"), [])

    def test_missing_licence_is_rejected(self):
        p = self.tampered("windows", "amd64", lambda f: f.pop("THIRD_PARTY_LICENSES.txt"))
        self.assertTrue(any("missing required member THIRD_PARTY_LICENSES.txt" in x for x in self.problems(p, "windows", "amd64")))

    def test_binary_for_the_wrong_architecture_is_rejected(self):
        wrong = self.files("windows", "arm64")["taskworker.exe"]
        p = self.tampered("windows", "amd64", lambda f: f.update({"taskworker.exe": wrong}))
        self.assertTrue(any("header says" in x for x in self.problems(p, "windows", "amd64")))

    def test_binary_for_the_wrong_os_is_rejected(self):
        wrong = self.files("linux", "amd64")["taskworker"]
        p = self.tampered("darwin", "amd64", lambda f: f.update({"taskworker": wrong}))
        self.assertTrue(any("header says" in x for x in self.problems(p, "darwin", "amd64")))

    def test_non_executable_unix_binary_is_rejected(self):
        files = self.files("linux", "amd64")
        top = f"taskworker-{VERSION}-linux-amd64"
        path = self.tmp / f"{top}.tar.gz"
        with tarfile.open(path, "w:gz") as t:
            for name, (data, _) in sorted(files.items()):
                ti = tarfile.TarInfo(f"{top}/{name}")
                ti.size, ti.mode = len(data), 0o644  # the executable lost its mode
                t.addfile(ti, io.BytesIO(data))
        self.assertTrue(any("mode is" in x for x in self.problems(path, "linux", "amd64")))

    def test_truncated_binary_is_rejected(self):
        p = self.tampered("linux", "amd64", lambda f: f.update({"taskworker": (f["taskworker"][0][:100000], True)}))
        self.assertTrue(any("bytes" in x for x in self.problems(p, "linux", "amd64")))

    def test_binary_arch_reader(self):
        self.assertEqual(package.binary_arch(self.files("windows", "arm64")["taskworker.exe"][0], "windows"), ("windows", "arm64"))
        self.assertEqual(package.binary_arch(self.files("darwin", "amd64")["taskworker"][0], "darwin"), ("darwin", "amd64"))
        self.assertEqual(package.binary_arch(self.files("linux", "arm64")["taskworker"][0], "linux"), ("linux", "arm64"))
        self.assertIsNone(package.binary_arch(b"not an executable at all", "linux"))

    def test_zip_entries_carry_no_build_host_stamp(self):
        """zipfile records the OS that built the archive in every entry; a Windows build and a Linux build of the
        same inputs only match if that field is pinned (found by building the release on both)."""
        import zipfile
        for arch in ("amd64", "arm64"):
            with zipfile.ZipFile(REAL / f"taskworker-{VERSION}-windows-{arch}.zip") as z:
                self.assertEqual({i.create_system for i in z.infolist()}, {3}, arch)

    def test_archives_are_reproducible(self):
        """The same inputs give byte-identical archives (fixed timestamps, sorted members)."""
        for os_name, arch in (("windows", "amd64"), ("linux", "arm64")):
            again = package.write_archive(os_name, arch, VERSION, self.files(os_name, arch))
            ext = "zip" if os_name == "windows" else "tar.gz"
            self.assertEqual(again.read_bytes(), (REAL / f"taskworker-{VERSION}-{os_name}-{arch}.{ext}").read_bytes(), (os_name, arch))


if __name__ == "__main__":
    unittest.main(verbosity=2)
