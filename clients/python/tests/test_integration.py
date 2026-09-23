"""Integration tests against the real service core/journal/HTTP handler with a
deterministic scripted backend (internal/testservice). No Ollama needed.

Set TASKWORKER_FAKEWORKER to the built fakeworker executable (scripts/verify.ps1
does this). Optionally set TASKWORKER_EXE to compare with the real CLI.
"""

import http.client
import json
import os
import subprocess
import sys
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))
sys.path.insert(0, os.path.dirname(__file__))

from scratch import scratch, scratch_dir  # noqa: E402

from taskworker_client import (  # noqa: E402
    Client, ConnectionFailure, CursorError, JobCancelled, JobFailed, Mirror, ServiceFault,
    StreamClosed, WaitTimeout, retry_command, submit_command,
)

FAKE = os.environ.get("TASKWORKER_FAKEWORKER")
CLI = os.environ.get("TASKWORKER_EXE")


class Service:
    """A task-owned fakeworker process; restartable on the same port and data."""

    def __init__(self, data_dir, gate_dir, addr="127.0.0.1:0"):
        self.data_dir, self.gate_dir, self.addr, self.proc, self.url = data_dir, gate_dir, addr, None, None

    def start(self):
        self.proc = subprocess.Popen([FAKE, "--addr", self.addr, "--data-dir", self.data_dir, "--gate-dir", self.gate_dir],
                                     stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        line = self.proc.stdout.readline().split()
        assert line and line[0] == "ready", line
        self.url = line[1]
        self.addr = self.url[len("http://"):]
        return self

    def stop(self):
        if self.proc:
            self.proc.stdin.close()
            self.proc.wait(30)
            self.proc.stdout.close()
            self.proc = None


class Lossy:
    """Forwards to the real service, but drops the reply to /v1/submit."""

    def __init__(self, upstream):
        outer = self
        self.dropped = 0
        host = upstream[len("http://"):]

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _go(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(n) if n else b""
                conn = http.client.HTTPConnection(host, timeout=10)
                conn.request(self.command, self.path, body=body or None, headers={"Content-Type": "application/json", "Host": host} if body else {"Host": host})
                resp = conn.getresponse()
                data = resp.read()
                conn.close()
                if self.path == "/v1/submit":
                    outer.dropped += 1
                    self.connection.close()  # accepted upstream; the reply is lost
                    return
                self.send_response(resp.status)
                self.send_header("Content-Type", resp.getheader("Content-Type"))
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            do_GET = do_POST = _go

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.server.daemon_threads = True
        self.server.handle_error = lambda *args: None
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


@unittest.skipUnless(FAKE, "TASKWORKER_FAKEWORKER not set")
class IntegrationTests(unittest.TestCase):
    def setUp(self):
        self.dir_name = scratch(self)
        self.gate = os.path.join(self.dir_name, "gate")
        os.makedirs(self.gate)
        self.svc = Service(os.path.join(self.dir_name, "data"), self.gate).start()
        self.addCleanup(self.svc.stop)
        self.client = Client(self.svc.url)

    def running(self, prompt="hold", key=None, want="started"):
        job = self.client.send(submit_command("fake:latest", prompt, max_output_tokens=8, idempotency_key=key))
        self.until(job.id, lambda j: want in j.text)
        return job

    def until(self, job_id, cond, timeout=20):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            j = self.client.get_job(job_id)
            if cond(j):
                return j
            time.sleep(0.02)
        self.fail(f"condition never met for {job_id}: {self.client.get_job(job_id)}")

    # ---- shared jobs -------------------------------------------------------

    def test_infer_matches_http_and_cli(self):
        job = self.client.infer(model="fake:latest", prompt="héllo 世界", role="r", system_prompt="s", max_output_tokens=8, origin="py-test")
        self.assertEqual((job.state, job.text, job.origin), ("succeeded", "reply: héllo 世界", "py-test"))
        raw = json.loads(self.raw_get(f"/v1/jobs/{job.id}"))
        self.assertEqual((raw["id"], raw["state"], raw["result"]["text"]), (job.id, job.state, job.text))
        self.assertEqual(raw["instructions"]["system"], "r\n\ns")
        if CLI:
            out = subprocess.run([CLI, "get", "--server", self.svc.url, job.id], capture_output=True, check=True).stdout
            cli = json.loads(out.decode("utf-8"))
            self.assertEqual((cli["id"], cli["state"], cli["result"]["text"]), (job.id, "succeeded", job.text))
            self.assertEqual([j.id for j in self.client.list_jobs()], [job.id])

    def raw_get(self, path):
        conn = http.client.HTTPConnection(self.svc.addr, timeout=10)
        conn.request("GET", path)
        data = conn.getresponse().read()
        conn.close()
        return data

    def test_health_models_and_snapshot(self):
        self.assertEqual(self.client.health(), {"status": "serving", "backend": "not_checked"})
        self.assertEqual([m["id"] for m in self.client.models()], ["fake:latest"])
        snap = self.client.snapshot()
        self.assertEqual((snap.jobs, snap.queue["paused"]), ([], False))
        self.assertEqual(len(snap.cursor.store_id), 32)

    def test_lost_acceptance_and_client_restart_resolve_to_one_job(self):
        proxy = Lossy(self.svc.url)
        self.addCleanup(proxy.close)
        command = submit_command("fake:latest", "hold", max_output_tokens=8, idempotency_key="lost-1")
        with scratch_dir() as d:
            path = command.save(os.path.join(d, "command.json"))
            with self.assertRaises(ConnectionFailure) as cm:
                Client(proxy.url).send(command)
            self.assertTrue(cm.exception.acceptance_uncertain)
            self.assertIs(cm.exception.command, command)
            # "Restart": a brand-new process would reload the saved file.
            from taskworker_client import Command
            recovered = Command.load("submit", path)
            self.assertEqual(recovered, command)
            job = Client(self.svc.url).send(recovered)
        self.assertEqual(proxy.dropped, 1)
        self.assertEqual(len(self.client.list_jobs()), 1)
        self.assertEqual(job.id, self.client.list_jobs()[0].id)
        again = Client(self.svc.url).send(command)
        self.assertEqual(again.id, job.id)
        changed = submit_command("fake:latest", "different", max_output_tokens=8, idempotency_key="lost-1")
        with self.assertRaises(ServiceFault) as cm2:
            self.client.send(changed)
        self.assertEqual(cm2.exception.code, "conflict")
        self.assertFalse(cm2.exception.acceptance_uncertain)
        self.assertEqual(len(self.client.list_jobs()), 1)
        self.client.cancel(job.id)

    def test_service_restart_then_resend_returns_the_original_job(self):
        command = submit_command("fake:latest", "hold", max_output_tokens=8, idempotency_key="restart-1")
        job = self.client.send(command)
        self.until(job.id, lambda j: "started" in j.text)
        self.svc.stop()
        with self.assertRaises(ConnectionFailure) as cm:
            self.client.send(command)
        self.assertFalse(cm.exception.sent, "nothing listens: definite non-delivery")
        self.svc.start()
        again = Client(self.svc.url).send(command)
        self.assertEqual(again.id, job.id)
        self.assertEqual(again.state, "interrupted", "unfinished work is marked interrupted, not restarted")
        self.assertEqual(again.text, "started", "partial output survives the restart")
        self.assertEqual(len(Client(self.svc.url).list_jobs()), 1)

    def test_closing_observation_leaves_inference_running_and_cancel_keeps_partial(self):
        job = self.running()
        m = Mirror(self.client.snapshot())
        frames = []
        with self.client.watch(m.cursor) as stream:
            self.client.pause_queue()  # a durable event for the observer to see
            for f in stream:
                frames.append(f)
                m.apply(f)
                break  # detach after the first frame
        self.assertEqual(frames[0].data["kind"], "queue.state")
        self.client.resume_queue()
        time.sleep(0.2)
        self.assertEqual(self.client.get_job(job.id).state, "running")
        # A wait that gives up also leaves it running.
        with self.assertRaises(WaitTimeout) as cm:
            self.client.wait(job.id, timeout=0.3, poll_interval=0.05)
        self.assertEqual(cm.exception.job.state, "running")
        cur = self.client.snapshot().cursor
        after = self.client.cancel(job.id)
        self.assertIn(after.state, ("cancelling", "cancelled"))
        final = self.until(job.id, lambda j: j.is_terminal)
        self.assertEqual((final.state, final.text), ("cancelled", "started"))
        # Lifecycle ordering from the durable event stream.
        seen = []
        with self.client.watch(cur, timeout=3) as stream:
            try:
                for f in stream:
                    d = f.data
                    if d["kind"] in ("job.state", "job.result") and d["job_id"] == job.id:
                        seen.append(d["job"]["state"])
                    if d["kind"] == "job.result":
                        break
            except Exception:
                pass
        self.assertEqual(seen, ["cancelling", "cancelled"])
        with self.assertRaises(JobCancelled) as cm2:
            self.client.wait(job.id, raise_on_terminal_failure=True)
        self.assertEqual(cm2.exception.partial_text, "started")

    def test_queue_pause_resume_and_fifo(self):
        self.client.pause_queue()
        a = self.client.send(submit_command("fake:latest", "first", max_output_tokens=8))
        b = self.client.send(submit_command("fake:latest", "second", max_output_tokens=8))
        q = self.client.queue_status()
        self.assertEqual((q["paused"], q["pending"]), (True, [a.id, b.id]))
        self.assertEqual(self.client.get_job(a.id).state, "queued")
        self.client.resume_queue()
        ja, jb = self.client.wait(a.id), self.client.wait(b.id)
        self.assertEqual((ja.state, jb.state), ("succeeded", "succeeded"))
        self.assertLessEqual(ja["started_at"], jb["started_at"])

    def test_retry_and_branch_lineage_with_immutable_parent(self):
        parent = self.client.send(submit_command("fake:latest", "fail", max_output_tokens=8))
        failed = self.client.wait(parent.id)
        self.assertEqual((failed.state, failed.text, failed.error["code"]), ("failed", "partial", "backend_failure"))
        with self.assertRaises(JobFailed) as cm:
            self.client.wait(parent.id, raise_on_terminal_failure=True)
        self.assertEqual(cm.exception.partial_text, "partial")
        r = self.client.retry(parent.id)
        self.assertEqual(r.lineage, {"parent_id": parent.id, "relation": "retry"})
        b = self.client.branch(parent.id, "fake:latest", "continue", max_output_tokens=8)
        self.assertEqual(b.lineage["relation"], "branch")
        self.assertEqual([m["content"] for m in b["instructions"]["history"]], ["fail", "partial"])
        self.assertEqual(self.client.get_job(parent.id).text, "partial")
        self.assertEqual(self.client.get_job(parent.id).state, "failed")
        self.client.wait(r.id)
        self.client.wait(b.id)
        with self.assertRaises(ServiceFault) as cm2:
            live = self.running()
            try:
                self.client.retry(live.id)
            finally:
                self.client.cancel(live.id)
        self.assertEqual(cm2.exception.status, 409)  # parent not terminal

    def test_unicode_offsets_and_exact_cursors_over_the_live_stream(self):
        m = Mirror(self.client.snapshot())
        start = m.cursor
        job = self.client.send(submit_command("fake:latest", "unicode", max_output_tokens=8))
        offsets = []
        with self.client.watch(m.cursor, timeout=20) as stream:
            for f in stream:
                if f.data["kind"] == "job.output":
                    offsets.append(f.data["output"]["offset_bytes"])
                m.apply(f)
                if f.data["kind"] == "job.result" and f.data["job_id"] == job.id:
                    break
        self.assertEqual(offsets, [0, 2, 8])
        self.assertEqual(m.text(job.id), "é世界🙂")
        self.assertEqual(m.cursor.store_id, start.store_id)
        self.assertGreater(m.cursor.sequence, start.sequence)
        # Replay from the last applied cursor delivers only newer events, and the
        # mirror then equals the service's own snapshot.
        applied_through = m.cursor.sequence
        snap = self.client.snapshot()
        replay = []
        with self.client.watch(m.cursor, timeout=10) as stream:
            for f in stream:
                replay.append(f.cursor.sequence)
                m.apply(f)
                if f.cursor == snap.cursor:
                    break
        self.assertTrue(all(seq > applied_through for seq in replay), replay)
        self.assertEqual(m.cursor, snap.cursor)
        self.assertEqual(m.job(job.id).raw, next(j for j in snap.jobs if j.id == job.id).raw)

    def test_cursor_errors_are_explicit_and_never_reset(self):
        cur = self.client.snapshot().cursor
        for bad, why in ((f"{'f' * 32}:1", "wrong store"), (f"{cur.store_id}:{cur.sequence + 1000}", "future")):
            with self.assertRaises(ServiceFault) as cm:
                next(self.client.watch(bad))
            self.assertEqual(cm.exception.code, "cursor_invalid", why)
        m = Mirror()
        m.cursor = type(cur)("f" * 32, 1)
        with self.assertRaises(ServiceFault):
            list(self.client.follow(m, max_reconnects=3, reconnect_delay=0.01))
        self.assertEqual(m.cursor.store_id, "f" * 32, "the invalid cursor is kept, not reset")

    def test_follow_reconnects_from_last_applied_cursor_across_a_dropped_stream(self):
        m = Mirror(self.client.snapshot())
        job = self.client.send(submit_command("fake:latest", "gate:go", max_output_tokens=8))
        got = []
        # A stream deadline stands in for a network drop; the bound is one reconnect.
        gen = self.client.follow(m, max_reconnects=1, reconnect_delay=0.01, timeout=0.6)
        stopper = threading.Timer(0.9, lambda: open(os.path.join(self.gate, "go"), "w").close())
        stopper.start()
        try:
            for f, applied in gen:
                got.append((f.cursor.sequence, applied))
                if f.data["kind"] == "job.result" and f.data["job_id"] == job.id:
                    break
        except Exception as exc:  # the bound may be reached before completion
            self.assertIsInstance(exc, Exception)
        finally:
            stopper.cancel()
        seqs = [s for s, _ in got]
        self.assertEqual(seqs, sorted(set(seqs)), "no event applied twice across the reconnect")
        self.until(job.id, lambda j: j.is_terminal)
        self.assertEqual(self.client.get_job(job.id).state, "succeeded")

    def test_slow_and_abandoned_streams_do_not_block_or_leak(self):
        streams = [self.client.watch() for _ in range(5)]
        for s in streams:
            s._start()  # connected, never read
        started = time.monotonic()
        job = self.client.infer(model="fake:latest", prompt="big:300000", max_output_tokens=8)
        self.assertEqual(len(job.text), 300000)
        self.assertLess(time.monotonic() - started, 15)
        for s in streams:
            s.close()
        # 40 open/close cycles would exhaust the 32-handler cap if handlers leaked.
        for _ in range(40):
            stream = self.client.watch()
            next(stream)
            stream.close()
        self.assertEqual(self.client.health()["status"], "serving")
        for _ in range(3):
            self.client.queue_status()

    def test_validation_and_faults_reach_the_caller_intact(self):
        with self.assertRaises(ServiceFault) as cm:
            self.client.get_job("0" * 32)
        self.assertEqual((cm.exception.code, cm.exception.status), ("not_found", 404))
        bad = submit_command("fake:latest", "x", max_output_tokens=8, idempotency_key="k-bad")
        self.client.send(bad)
        with self.assertRaises(ServiceFault) as cm2:
            self.client.send(retry_command("1" * 32, idempotency_key="k-bad"))
        self.assertEqual(cm2.exception.code, "conflict")


@unittest.skipUnless(FAKE, "TASKWORKER_FAKEWORKER not set")
class ExampleScriptTests(unittest.TestCase):
    """Run the shipped examples as a user would."""

    def setUp(self):
        self.dir_name = scratch(self)
        gate = os.path.join(self.dir_name, "gate")
        os.makedirs(gate)
        self.svc = Service(os.path.join(self.dir_name, "data"), gate).start()
        self.addCleanup(self.svc.stop)
        self.examples = os.path.join(os.path.dirname(__file__), "..", "examples")
        self.env = dict(os.environ, PYTHONPATH=os.path.join(os.path.dirname(__file__), "..", "src"), PYTHONIOENCODING="utf-8")

    def run_example(self, name, *args):
        return subprocess.run([sys.executable, os.path.join(self.examples, name), "--server", self.svc.url, *args],
                              capture_output=True, env=self.env, cwd=self.dir_name, timeout=60)

    def test_basic_infer_and_recover(self):
        r = self.run_example("basic_infer.py", "--model", "fake:latest", "héllo")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(r.stdout.decode("utf-8").strip(), "reply: héllo")
        saved = os.path.join(self.dir_name, "command.json")
        self.assertTrue(os.path.exists(saved))
        rec = self.run_example("recover_uncertain.py", saved)
        self.assertEqual(rec.returncode, 0, rec.stderr)
        self.assertEqual(len(Client(self.svc.url).list_jobs()), 1, "recovery must not create a second job")

    def test_observe_and_cancel_example(self):
        r = self.run_example("observe_and_cancel.py", "--model", "fake:latest", "--cancel-after", "0.5", "hold")
        self.assertEqual(r.returncode, 0, r.stderr)
        jobs = Client(self.svc.url).list_jobs()
        self.assertEqual((len(jobs), jobs[0].state, jobs[0].text), (1, "cancelled", "started"))
        self.assertIn("started", r.stdout.decode("utf-8"))


if __name__ == "__main__":
    unittest.main()
