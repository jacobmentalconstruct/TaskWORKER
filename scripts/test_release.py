"""Tests for the release gate logic: evidence must stop counting when what it vouches for changes.

    python scripts/test_release.py

Each test builds a throwaway git repository under .cache/ and points releaselib at it.
"""

import json
import os
import pathlib
import shutil
import subprocess
import sys
import types
import unittest
import uuid

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import limitations as limmod  # noqa: E402
import release_check as rc  # noqa: E402
import releaselib as lib  # noqa: E402

REPO_ROOT = lib.ROOT
GIT_ENV = dict(os.environ, GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@example.invalid", GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@example.invalid")


def sh(root, *args):
    return subprocess.run(["git", *args], cwd=root, env=GIT_ENV, capture_output=True, text=True, check=True).stdout


def write(root, rel, text):
    p = root / rel
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(text, encoding="utf-8", newline="\n")


class GateTests(unittest.TestCase):
    def setUp(self):
        self.root = REPO_ROOT / ".cache" / ("reltest-" + uuid.uuid4().hex)
        self.root.mkdir(parents=True)
        self.addCleanup(rc.rmtree, self.root)
        sh(self.root, "init", "-q", "-b", "main")
        for name in lib.UI_ASSETS:
            write(self.root, f"internal/adapters/webui/{name}", f"asset {name}\n")
        write(self.root, "README.md", f"# t\n\n{lib.README_BEGIN}\n{lib.README_END}\n")
        write(self.root, "docs/limitations.md", "")
        write(self.root, "dist/release-manifest.json", json.dumps({"version": "0.0.1", "targets": {
            "linux/amd64": {"binary_sha256": "a" * 64, "archive": "x.tar.gz", "archive_sha256": "b" * 64},
            "windows/amd64": {"binary_sha256": "c" * 64, "archive": "x.zip", "archive_sha256": "d" * 64}}}))
        write(self.root, "release/manual-checks.json", json.dumps({
            "required_for_release": ["host-check:linux/amd64", "interactive-browser-check", "ci-green"],
            "manual": {"interactive-browser-check": {"title": "Browser", "binds": ["ui_assets"], "steps": ["open it"]},
                       "ci-green": {"title": "CI", "binds": ["commit"], "steps": ["push"]}}}))
        self.set_limitations([])
        write(self.root, "release/attestations.json", json.dumps({"attestations": []}))
        sh(self.root, "add", "-A")
        sh(self.root, "commit", "-q", "-m", "base")
        self.saved = lib.ROOT
        lib.ROOT = self.root
        self.addCleanup(setattr, lib, "ROOT", self.saved)

    def set_limitations(self, items):
        write(self.root, "release/limitations.json", json.dumps({"limitations": items}))

    def att(self, item, binds, by="tester", kind="manual"):
        return lib.make_attestation(item, kind, by, "note", binds)

    # ---- bindings -----------------------------------------------------------------------

    def test_a_binary_attestation_holds_until_the_executable_changes(self):
        a = self.att("host-check:linux/amd64", {"binary:linux/amd64": "a" * 64}, kind="auto")
        self.assertIsNone(lib.attestation_problem(a))
        write(self.root, "dist/release-manifest.json", json.dumps({"version": "0.0.1", "targets": {"linux/amd64": {"binary_sha256": "e" * 64}}}))
        self.assertIn("changed since it was recorded", lib.attestation_problem(a))

    def test_a_ui_attestation_holds_until_an_embedded_asset_changes(self):
        a = self.att("interactive-browser-check", lib.current_bindings(["ui_assets"]))
        self.assertIsNone(lib.attestation_problem(a))
        write(self.root, "internal/adapters/webui/app.js", "changed\n")
        self.assertIn("ui_assets changed", lib.attestation_problem(a))

    def test_a_ui_attestation_ignores_other_changes(self):
        a = self.att("interactive-browser-check", lib.current_bindings(["ui_assets"]))
        write(self.root, "README.md", "edited\n")
        self.assertIsNone(lib.attestation_problem(a))

    def test_a_commit_attestation_survives_only_evidence_changes(self):
        c = lib.head_commit()
        a = self.att("ci-green", {"commit": c})
        write(self.root, "release/attestations.json", json.dumps({"attestations": [a]}))  # recording evidence is allowed
        self.assertIsNone(lib.attestation_problem(a))
        write(self.root, "README.md", "edited after CI ran\n")
        self.assertIn("changed since commit", lib.attestation_problem(a))
        sh(self.root, "add", "-A")
        sh(self.root, "commit", "-q", "-m", "later")
        self.assertIn("README.md", lib.attestation_problem(a))  # committing it does not make it valid again

    def test_the_newest_attestation_that_still_holds_wins_and_a_stale_one_says_so(self):
        good = self.att("interactive-browser-check", lib.current_bindings(["ui_assets"]), by="first")
        stale = self.att("interactive-browser-check", {"ui_assets": "0" * 64}, by="second")
        att, why = lib.latest_valid("interactive-browser-check", [good, stale])
        self.assertEqual(att["by"], "first")
        att, why = lib.latest_valid("interactive-browser-check", [stale])
        self.assertIsNone(att)
        self.assertIn("no longer holds", why)
        self.assertEqual(lib.latest_valid("nothing", [])[1], "no attestation recorded")

    # ---- limitations --------------------------------------------------------------------

    def lim(self, **kw):
        base = {"id": "x", "title": "X", "text": "text", "status": "accepted", "closes_when": "never"}
        base.update(kw)
        return base

    def test_a_limitation_cannot_be_closed_without_evidence(self):
        errors, _ = lib.limitation_problems([self.lim(status="closed", closes_with=["interactive-browser-check"])], [])
        self.assertTrue(any("closed without valid evidence" in e for e in errors), errors)
        errors, _ = lib.limitation_problems([self.lim(status="closed")], [])
        self.assertTrue(errors)

    def test_a_limitation_may_be_closed_once_the_evidence_holds(self):
        a = self.att("interactive-browser-check", lib.current_bindings(["ui_assets"]))
        errors, warnings = lib.limitation_problems([self.lim(status="closed", closes_with=["interactive-browser-check"])], [a])
        self.assertEqual((errors, warnings), ([], []))

    def test_an_open_limitation_whose_evidence_exists_is_flagged_so_it_is_not_forgotten(self):
        a = self.att("interactive-browser-check", lib.current_bindings(["ui_assets"]))
        errors, warnings = lib.limitation_problems([self.lim(status="open", closes_with=["interactive-browser-check"])], [a])
        self.assertEqual(errors, [])
        self.assertTrue(any("set its status to closed" in w for w in warnings), warnings)

    def test_a_limitation_cannot_assert_a_fact_that_has_no_evidence(self):
        errors, _ = lib.limitation_problems([self.lim(evidence=["host-check:darwin/arm64"])], [])
        self.assertTrue(any("relies on 'host-check:darwin/arm64'" in e for e in errors), errors)

    def test_limitation_structure_is_enforced(self):
        errors, _ = lib.limitation_problems([self.lim(closes_when=""), self.lim(id="y", status="maybe"), self.lim(), self.lim()], [])
        joined = " | ".join(errors)
        self.assertIn("needs closes_when", joined)
        self.assertIn("status must be one of", joined)
        self.assertIn("duplicate id", joined)

    # ---- generated documents ------------------------------------------------------------

    def test_generated_documents_drift_is_detected_and_repaired(self):
        self.set_limitations([self.lim(id="a", title="Alpha", text="first")])
        self.assertEqual(sorted(limmod.out_of_sync()), ["README.md", "docs/limitations.md"])
        self.assertEqual(limmod.main(["--write"]), 0)
        self.assertEqual(limmod.out_of_sync(), [])
        self.assertIn("**Alpha.** first", (self.root / "README.md").read_text(encoding="utf-8"))
        self.set_limitations([self.lim(id="a", title="Alpha", text="second")])
        self.assertEqual(sorted(limmod.out_of_sync()), ["README.md", "docs/limitations.md"])
        write(self.root, "README.md", (self.root / "README.md").read_text(encoding="utf-8").replace("**Alpha.**", "**Edited by hand.**"))
        self.assertIn("README.md", limmod.out_of_sync())

    def test_closed_limitations_leave_the_readme_but_stay_in_the_full_list(self):
        self.set_limitations([self.lim(id="a", title="Alpha", status="open", closes_with=["ci-green"]),
                              self.lim(id="b", title="Beta", status="closed", closes_with=["ci-green"])])
        limmod.main(["--write"])
        readme = (self.root / "README.md").read_text(encoding="utf-8")
        self.assertIn("Alpha", readme)
        self.assertNotIn("Beta", readme)
        self.assertIn("Beta", (self.root / "docs/limitations.md").read_text(encoding="utf-8"))

    # ---- the verdict --------------------------------------------------------------------

    def test_missing_required_evidence_blocks_and_says_how_to_record_it(self):
        results = rc.check_attestations(*[rc.load_manual()[k] for k in ("required_for_release", "manual")])
        by_name = {r.name: r for r in results}
        self.assertEqual(by_name["evidence: host-check:linux/amd64"].status, rc.MISSING)
        self.assertIn("package.py host-check", by_name["evidence: host-check:linux/amd64"].detail)
        self.assertEqual(by_name["evidence: interactive-browser-check"].status, rc.MISSING)
        self.assertIn("release_check.py attest interactive-browser-check", by_name["evidence: interactive-browser-check"].detail)

    def test_recorded_evidence_turns_required_items_to_pass_and_stale_evidence_blocks_again(self):
        lib.add_attestation(self.att("interactive-browser-check", lib.current_bindings(["ui_assets"])))
        get = lambda: {r.name: r.status for r in rc.check_attestations(*[rc.load_manual()[k] for k in ("required_for_release", "manual")])}
        self.assertEqual(get()["evidence: interactive-browser-check"], rc.PASS)
        write(self.root, "internal/adapters/webui/state.js", "the UI changed\n")
        self.assertEqual(get()["evidence: interactive-browser-check"], rc.MISSING)

    def test_a_dirty_tree_blocks(self):
        self.assertEqual(rc.check_git_clean().status, rc.PASS)
        write(self.root, "README.md", "dirty\n")
        r = rc.check_git_clean()
        self.assertEqual(r.status, rc.FAIL)
        self.assertIn("commit first", r.detail)

    # ---- attesting ----------------------------------------------------------------------

    def attest(self, item, by="A Person", note="looked fine"):
        return rc.cmd_attest(types.SimpleNamespace(item=item, by=by, note=note))

    def test_attest_records_a_bound_manual_check_and_refuses_without_a_name_or_note(self):
        self.assertEqual(self.attest("interactive-browser-check", by=None), 2)
        self.assertEqual(self.attest("interactive-browser-check", note=None), 2)
        self.assertEqual(lib.attestations(), [])
        self.assertEqual(self.attest("interactive-browser-check"), 0)
        [a] = lib.attestations()
        self.assertEqual((a["id"], a["kind"], a["by"]), ("interactive-browser-check", "manual", "A Person"))
        self.assertEqual(a["binds"], lib.current_bindings(["ui_assets"]))

    def test_attest_only_accepts_manual_checks(self):
        self.assertEqual(self.attest("host-check:linux/amd64"), 2)
        self.assertEqual(self.attest("made-up"), 2)
        self.assertEqual(lib.attestations(), [])

    def test_attest_will_not_bind_to_a_commit_while_the_tree_differs_from_it(self):
        write(self.root, "README.md", "uncommitted change\n")
        self.assertEqual(self.attest("ci-green"), 1)
        self.assertEqual(lib.attestations(), [])
        sh(self.root, "add", "-A")
        sh(self.root, "commit", "-q", "-m", "commit it")
        self.assertEqual(self.attest("ci-green"), 0)
        self.assertEqual(lib.attestations()[0]["binds"], {"commit": lib.head_commit()})

    # ---- release notes ------------------------------------------------------------------

    def test_release_notes_are_a_draft_until_all_required_evidence_holds(self):
        m = lib.manifest()
        text = rc.render_notes("0.0.1", m, [], [self.lim(id="a", title="Alpha", text="alpha text")])
        self.assertIn("(DRAFT", text.splitlines()[0])
        self.assertIn("**not recorded**", text)
        self.assertIn("**Alpha.** alpha text", text)
        atts = [self.att("host-check:linux/amd64", {"binary:linux/amd64": "a" * 64}, by="scripts/package.py on Linux (x86_64)", kind="auto"),
                self.att("interactive-browser-check", lib.current_bindings(["ui_assets"])),
                self.att("ci-green", {"commit": lib.head_commit()})]
        text = rc.render_notes("0.0.1", m, atts, [])
        self.assertNotIn("DRAFT", text)
        self.assertIn("| linux/amd64 |", text)
        self.assertRegex(text, r"linux/amd64 \| `x.tar.gz` \| `b+` \| yes")
        self.assertRegex(text, r"windows/amd64 \| `x.zip` \| `d+` \| no")


if __name__ == "__main__":
    unittest.main(verbosity=2)
