"""Unit tests against a scripted stub server. Run: python -m unittest discover -s tests"""

import json
import os
import socket
import sys
import time
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))
sys.path.insert(0, os.path.dirname(__file__))

from scratch import scratch_dir  # noqa: E402

from stub import JOB, STORE, Stub, event, frame, job, sse  # noqa: E402

import taskworker_client as tw  # noqa: E402
from taskworker_client import (  # noqa: E402
    Client, Command, ConnectionFailure, Cursor, CursorError, InvalidCommandError, JobCancelled,
    JobFailed, Mirror, ProtocolError, RequestTimeout, ServiceFault, StreamClosed, WaitTimeout,
    branch_command, resolve_endpoint, retry_command, submit_command,
)
from taskworker_client.client import Frame  # noqa: E402

FAULT = {"code": "conflict", "message": "conflict", "retryable": False}


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


class Base(unittest.TestCase):
    def stub(self, handler=None):
        s = Stub(handler)
        self.addCleanup(s.close)
        return s


class EndpointTests(unittest.TestCase):
    def test_resolution_order_and_pinning(self):
        self.assertEqual(resolve_endpoint(None, {}).url, "http://127.0.0.1:7433")
        self.assertEqual(resolve_endpoint(None, {"TASKWORKER_SERVER": "http://127.0.0.1:9"}).port, 9)
        self.assertEqual(resolve_endpoint("http://127.0.0.1:10", {"TASKWORKER_SERVER": "http://127.0.0.1:9"}).port, 10)
        self.assertEqual(resolve_endpoint("http://localhost:11", {}).host, "127.0.0.1")
        self.assertEqual(resolve_endpoint("http://[::1]:12", {}).url, "http://[::1]:12")
        self.assertEqual(resolve_endpoint("http://127.0.0.1", {}).port, 80)
        self.assertEqual(resolve_endpoint("http://127.0.0.1:7433/", {}).port, 7433)
        self.assertEqual(resolve_endpoint("http://[::ffff:127.0.0.1]:13", {}).host, "127.0.0.1")

    def test_rejects_everything_that_is_not_plain_loopback_http(self):
        for bad in ["https://127.0.0.1:1", "http://example.com:1", "http://192.168.0.1:1", "http://user@127.0.0.1:1",
                    "http://127.0.0.1:1/x", "http://127.0.0.1:1?a=b", "http://127.0.0.1:1#f", "http://127.0.0.1:",
                    "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://0.0.0.0:1", "127.0.0.1:1", "http://:1",
                    "http://localhost.evil.test:1"]:
            with self.assertRaises(InvalidCommandError, msg=bad):
                resolve_endpoint(bad, {})


class CommandTests(unittest.TestCase):
    def test_exact_bytes_and_unicode(self):
        c = submit_command("m", "héllo 世界 🙂\nline2", max_output_tokens=8, role="  role ", system_prompt="sys",
                           idempotency_key="k1", origin="o", seed=9007199254740993, temperature=0.25, context_tokens=2048)
        body = json.loads(c.to_bytes())
        self.assertEqual(body["request"]["prompt"], "héllo 世界 🙂\nline2")
        self.assertEqual(body["request"]["options"], {"max_output_tokens": 8, "context_tokens": 2048, "temperature": 0.25, "seed": 9007199254740993})
        self.assertIn(b"9007199254740993", c.to_bytes())
        self.assertNotIn(b"\\u", c.to_bytes())  # real UTF-8, not escapes
        self.assertEqual(c.key, "k1")
        self.assertEqual(c.path, "/v1/submit")

    def test_optional_fields_are_omitted_never_null(self):
        c = submit_command("m", "p", max_output_tokens=1, idempotency_key="k")
        self.assertEqual(json.loads(c.to_bytes())["request"], {"model": "m", "prompt": "p", "options": {"max_output_tokens": 1}})
        self.assertNotIn(b"null", c.to_bytes())

    def test_save_load_round_trip_is_byte_identical_without_bom(self):
        c = submit_command("m", "p 世", max_output_tokens=1, idempotency_key="k")
        with scratch_dir() as d:
            path = c.save(os.path.join(d, "c.json"))
            with open(path, "rb") as handle:
                raw = handle.read()
            self.assertEqual(raw, c.to_bytes())
            self.assertFalse(raw.startswith(b"\xef\xbb\xbf"))
            self.assertEqual(Command.load("submit", path), c)

    def test_local_validation_sends_nothing(self):
        for kwargs in [dict(model="m", prompt="p", max_output_tokens=0), dict(model="m", prompt="p", max_output_tokens=True),
                       dict(model="", prompt="p", max_output_tokens=1), dict(model="m", prompt="  ", max_output_tokens=1),
                       dict(model="m", prompt="p", max_output_tokens=1, temperature=float("nan")),
                       dict(model="m", prompt="p", max_output_tokens=1, temperature=-1),
                       dict(model="m", prompt="p", max_output_tokens=1, seed=1 << 63),
                       dict(model="m", prompt="p", max_output_tokens=1, seed=1.5),
                       dict(model="m", prompt="bad \ud800 surrogate", max_output_tokens=1),
                       dict(model="m", prompt="p", max_output_tokens=1, idempotency_key=""),
                       dict(model="m", prompt="p", max_output_tokens=1, idempotency_key="é" * 65),
                       dict(model="m", prompt="p", max_output_tokens=1, origin="")]:
            with self.assertRaises(InvalidCommandError, msg=kwargs):
                submit_command(**kwargs)
        with self.assertRaises(InvalidCommandError):
            retry_command("not-an-id")
        with self.assertRaises(InvalidCommandError):
            Command("submit", {"origin": "x"})  # no key
        with self.assertRaises(AttributeError):
            submit_command("m", "p", max_output_tokens=1).operation = "retry"

    def test_key_is_generated_before_sending_and_unique(self):
        a, b = submit_command("m", "p", max_output_tokens=1), submit_command("m", "p", max_output_tokens=1)
        self.assertNotEqual(a.key, b.key)
        self.assertTrue(a.key.startswith("py-"))

    def test_retry_and_branch_shapes(self):
        r = retry_command(JOB, idempotency_key="r")
        self.assertEqual(json.loads(r.to_bytes()), {"idempotency_key": "r", "origin": "python", "parent_id": JOB})
        b = branch_command(JOB, "m", "next", max_output_tokens=4, idempotency_key="b")
        self.assertEqual(json.loads(b.to_bytes())["request"]["prompt"], "next")
        self.assertEqual(b.path, "/v1/branch")


class CallTests(Base):
    def test_exact_integers_and_unknown_metadata_are_preserved(self):
        big = 18446744073709551615
        raw = job(future={"n": big, "list": [1, {"x": None}]}, extra_number=9007199254740993)
        s = self.stub(lambda r: (200, raw))
        j = Client(s.url).get_job(JOB)
        self.assertEqual(j["future"]["n"], big)
        self.assertEqual(j["extra_number"], 9007199254740993)
        self.assertEqual(j["future"]["list"], [1, {"x": None}])

    def test_send_posts_exact_bytes_once_and_returns_job_with_command(self):
        s = self.stub(lambda r: (200, job("queued")))
        c = submit_command("m", "héllo", max_output_tokens=2, idempotency_key="k")
        j = Client(s.url).send(c)
        self.assertEqual(len(s.posts()), 1)
        req = s.posts()[0]
        self.assertEqual(req.body, c.to_bytes())
        self.assertEqual(req.path, "/v1/submit")
        self.assertEqual(req.headers["Content-Type"], "application/json")
        self.assertIs(j.command, c)
        self.assertNotIn("Origin", req.headers)

    def test_faults_map_with_command_and_definiteness(self):
        cases = [(409, "conflict", False), (429, "queue_full", False), (404, "not_found", False), (400, "invalid_request", False),
                 (413, "limit_exceeded", False), (503, "unavailable", True), (503, "storage_failure", True), (500, "internal", True),
                 (503, "backend_unavailable", False), (409, "cancelled", False)]
        for status, code, uncertain in cases:
            s = self.stub(lambda r, status=status, code=code: (status, {"code": code, "message": code, "retryable": True, "details": {"k": "v"}}))
            c = submit_command("m", "p", max_output_tokens=1, idempotency_key="k")
            with self.assertRaises(ServiceFault, msg=code) as cm:
                Client(s.url).send(c)
            e = cm.exception
            self.assertEqual((e.code, e.status, e.retryable, e.details, e.command), (code, status, True, {"k": "v"}, c))
            self.assertEqual(e.acceptance_uncertain, uncertain, code)
            self.assertEqual(len(s.posts()), 1, "no automatic retry")
            s.close()

    def test_reads_are_never_uncertain(self):
        s = self.stub(lambda r: (503, {"code": "unavailable", "message": "unavailable", "retryable": True}))
        with self.assertRaises(ServiceFault) as cm:
            Client(s.url).get_job(JOB)
        self.assertFalse(cm.exception.acceptance_uncertain)
        self.assertEqual(cm.exception.job_id, JOB)

    def test_connection_refused_is_definite_not_uncertain(self):
        c = submit_command("m", "p", max_output_tokens=1, idempotency_key="k")
        with self.assertRaises(ConnectionFailure) as cm:
            Client(f"http://127.0.0.1:{free_port()}").send(c)
        self.assertFalse(cm.exception.sent)
        self.assertFalse(cm.exception.acceptance_uncertain)
        self.assertIs(cm.exception.command, c)
        self.assertIn("taskworker serve", str(cm.exception))

    def test_dropped_response_after_send_is_uncertain(self):
        def drop(req):
            req.handler.connection.close()

        s = self.stub(lambda r: drop)
        c = submit_command("m", "p", max_output_tokens=1, idempotency_key="k")
        with self.assertRaises(ConnectionFailure) as cm:
            Client(s.url).send(c)
        self.assertTrue(cm.exception.sent and cm.exception.acceptance_uncertain)
        self.assertIs(cm.exception.command, c)

    def test_timeout_is_not_proof_of_rejection(self):
        def slow(req):
            time.sleep(1.5)

        s = self.stub(lambda r: slow)
        c = submit_command("m", "p", max_output_tokens=1, idempotency_key="k")
        started = time.monotonic()
        with self.assertRaises(RequestTimeout) as cm:
            Client(s.url, operation_timeout=0.3).send(c)
        self.assertLess(time.monotonic() - started, 1.2)
        self.assertTrue(cm.exception.acceptance_uncertain)
        self.assertIs(cm.exception.command, c)
        with self.assertRaises(RequestTimeout) as cm2:
            Client(s.url, operation_timeout=0.3).get_job(JOB)
        self.assertFalse(cm2.exception.acceptance_uncertain)

    def test_total_deadline_covers_a_slow_drip_body(self):
        def drip(req):
            h = req.handler
            h.send_response(200)
            h.send_header("Content-Type", "application/json")
            h.end_headers()
            for _ in range(20):
                h.wfile.write(b" ")
                h.wfile.flush()
                time.sleep(0.1)

        s = self.stub(lambda r: drip)
        started = time.monotonic()
        with self.assertRaises(RequestTimeout):
            Client(s.url, operation_timeout=0.5, read_timeout=5).get_job(JOB)
        self.assertLess(time.monotonic() - started, 1.5)

    def test_response_size_bound(self):
        s = self.stub(lambda r: (200, job(text="x" * 5000)))
        with self.assertRaises(ProtocolError):
            Client(s.url, max_response_bytes=1000).get_job(JOB)

    def test_proxies_and_redirects_are_never_used(self):
        target = self.stub(lambda r: (200, job()))
        redirect = self.stub(lambda r: (302, b""))

        def send_redirect(req):
            h = req.handler
            h.send_response(302)
            h.send_header("Location", target.url + "/v1/jobs/" + JOB)
            h.send_header("Content-Length", "0")
            h.end_headers()

        redirect.handler = lambda r: send_redirect
        old = {k: os.environ.get(k) for k in ("HTTP_PROXY", "http_proxy", "ALL_PROXY", "NO_PROXY")}
        os.environ["HTTP_PROXY"] = os.environ["http_proxy"] = os.environ["ALL_PROXY"] = f"http://127.0.0.1:{free_port()}"
        os.environ.pop("NO_PROXY", None)
        try:
            self.assertEqual(Client(target.url).get_job(JOB).id, JOB)  # direct despite proxy env
            with self.assertRaises(ProtocolError):
                Client(redirect.url).get_job(JOB)
        finally:
            for k, v in old.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v
        self.assertEqual(len(target.requests), 1, "the redirect must not be followed")

    def test_bad_json_and_content_type(self):
        s = self.stub(lambda r: (200, b"{not json"))
        with self.assertRaises(ProtocolError):
            Client(s.url).health()
        s2 = self.stub(lambda r: (200, b'{"a": NaN}'))
        with self.assertRaises(ProtocolError):
            Client(s2.url).health()

    def test_get_job_validates_ids_locally(self):
        s = self.stub()
        with self.assertRaises(InvalidCommandError):
            Client(s.url).get_job("../etc/passwd")
        self.assertEqual(s.requests, [])

    def test_queue_controls_and_cancel_bodies(self):
        s = self.stub(lambda r: (200, job("cancelling") if "cancel" in r.path else {"paused": True, "pending": [], "max_pending": 64}))
        c = Client(s.url)
        self.assertTrue(c.pause_queue()["paused"])
        c.resume_queue()
        self.assertEqual(c.cancel(JOB).state, "cancelling")
        self.assertEqual([r.path for r in s.posts()], ["/v1/queue/pause", "/v1/queue/resume", f"/v1/jobs/{JOB}/cancel"])
        self.assertTrue(all(r.body == b"{}" for r in s.posts()))


class WaitAndInferTests(Base):
    def script(self, states):
        it = iter(states)
        last = [states[-1]]

        def handler(req):
            if req.method == "POST":
                return 200, job("queued")
            return 200, next(it, last[0])

        return self.stub(handler)

    def test_wait_polls_until_terminal(self):
        s = self.script([job("queued"), job("running", "a"), job("succeeded", "ab")])
        j = Client(s.url).wait(JOB, poll_interval=0.01)
        self.assertEqual((j.state, j.text, j.is_terminal), ("succeeded", "ab", True))

    def test_wait_timeout_keeps_the_job_and_never_cancels(self):
        s = self.script([job("running", "part")])
        with self.assertRaises(WaitTimeout) as cm:
            Client(s.url).wait(JOB, timeout=0.15, poll_interval=0.03)
        self.assertEqual(cm.exception.job.text, "part")
        self.assertEqual(s.posts(), [], "waiting must not send any mutation")

    def test_infer_is_one_create_and_a_wait_timeout_does_not_create_again(self):
        s = self.script([job("running", "part")])
        c = submit_command("m", "p", max_output_tokens=1, idempotency_key="k")
        with self.assertRaises(WaitTimeout) as cm:
            Client(s.url).infer(c, wait_timeout=0.15, poll_interval=0.03)
        self.assertEqual(len(s.posts()), 1)
        self.assertIs(cm.exception.command, c)
        self.assertEqual(cm.exception.job_id, JOB)

    def test_on_command_runs_before_any_request(self):
        s = self.script([job("succeeded", "x")])
        seen = []

        def persist(command):
            seen.append((command.key, len(s.requests)))

        j = Client(s.url).infer(model="m", prompt="p", max_output_tokens=1, on_command=persist, poll_interval=0.01)
        self.assertEqual(seen[0][1], 0)
        self.assertEqual(j.command.key, seen[0][0])

    def test_terminal_failure_is_distinct_and_keeps_partial_output(self):
        for state, exc in (("failed", JobFailed), ("cancelled", JobCancelled)):
            s = self.script([job(state, "partial 🙂", error={"code": state, "message": "m", "retryable": False})])
            c = submit_command("m", "p", max_output_tokens=1, idempotency_key="k")
            with self.assertRaises(exc) as cm:
                Client(s.url).infer(c, poll_interval=0.01)
            self.assertEqual(cm.exception.partial_text, "partial 🙂")
            self.assertEqual(cm.exception.job.error["code"], state)
            self.assertEqual(len(s.posts()), 1)
            s.close()
        s = self.script([job("failed", "p")])
        got = Client(s.url).infer(model="m", prompt="p", max_output_tokens=1, raise_on_terminal_failure=False, poll_interval=0.01)
        self.assertEqual(got.state, "failed")

    def test_transport_failure_during_infer_carries_recovery_data(self):
        c = submit_command("m", "p", max_output_tokens=1, idempotency_key="k")
        with self.assertRaises(ConnectionFailure) as cm:
            Client(f"http://127.0.0.1:{free_port()}").infer(c)
        self.assertIs(cm.exception.command, c)
        with self.assertRaises(InvalidCommandError):
            Client("http://127.0.0.1:1").infer(c, model="m")


def snapshot_frame(seq="5", jobs=None, queue=None):
    data = {"cursor": {"store_id": STORE, "sequence": str(seq)}, "queue": queue or {"paused": False, "pending": [], "max_pending": 64}, "jobs": jobs or []}
    return frame(seq, data, "snapshot")


class StreamTests(Base):
    def test_yields_complete_frames_only_and_ignores_comments(self):
        f1 = frame(6, event(6, "queue.state", queue={"paused": True, "pending": [], "max_pending": 64}))
        half = frame(7, event(7, "queue.state", queue={"paused": False, "pending": [], "max_pending": 64}))[:-30]
        s = self.stub(lambda r: sse([b": connected\n\n", f1, b": heartbeat\n\n", half]))
        seen = []
        with self.assertRaises(StreamClosed) as cm:
            with Client(s.url).watch(f"{STORE}:5") as stream:
                for f in stream:
                    seen.append(f)
        self.assertEqual([str(f.cursor) for f in seen], [f"{STORE}:6"])
        self.assertEqual(str(cm.exception.last_cursor), f"{STORE}:6")
        self.assertIn("after=" + STORE + "%3A5", s.requests[0].path)

    def test_eof_is_never_success(self):
        s = self.stub(lambda r: sse([snapshot_frame()]))
        stream = Client(s.url).watch()
        self.assertEqual(next(stream).kind, "snapshot")
        with self.assertRaises(StreamClosed) as cm:
            next(stream)
        self.assertEqual(str(cm.exception.last_cursor), f"{STORE}:5")
        self.assertEqual(s.requests[0].path, "/v1/events")

    def test_error_frame_raises_the_fault(self):
        err = b'event: error\ndata: {"code":"slow_consumer","message":"slow_consumer","retryable":true}\n\n'
        s = self.stub(lambda r: sse([err]))
        with self.assertRaises(ServiceFault) as cm:
            next(Client(s.url).watch(f"{STORE}:1"))
        self.assertEqual(cm.exception.code, "slow_consumer")

    def test_http_faults_before_streaming(self):
        for status, code in ((410, "cursor_expired"), (400, "cursor_invalid")):
            s = self.stub(lambda r, status=status, code=code: (status, {"code": code, "message": code, "retryable": False}))
            with self.assertRaises(ServiceFault) as cm:
                next(Client(s.url).watch(f"{STORE}:1"))
            self.assertEqual((cm.exception.code, cm.exception.status), (code, status))
            s.close()

    def test_exact_uint64_cursors(self):
        top = 18446744073709551615
        s = self.stub(lambda r: sse([frame(top, event(top, "queue.state", queue={"paused": False, "pending": [], "max_pending": 64}))]))
        f = next(Client(s.url).watch(f"{STORE}:{top - 1}"))
        self.assertEqual(f.cursor.sequence, top)
        self.assertEqual(str(f.cursor), f"{STORE}:{top}")
        self.assertEqual(f.cursor.to_json()["sequence"], str(top))
        self.assertIn(str(top - 1), s.requests[0].path)
        with self.assertRaises(ProtocolError):
            Cursor.parse(f"{STORE}:{top + 1}")

    def test_invalid_after_is_rejected_locally(self):
        s = self.stub()
        for bad in ("garbage", f"{STORE}:007", f"{STORE}:-1", f"{STORE.upper()}:1", f"{STORE}:", ""):
            if bad == "":
                continue
            with self.assertRaises(ProtocolError, msg=bad):
                Client(s.url).watch(bad)
        self.assertEqual(s.requests, [])

    def test_mismatched_id_and_data_cursor_and_bad_framing(self):
        bad = f"id: {STORE}:9\nevent: event\ndata: " + json.dumps(event(8, "queue.state")) + "\n\n"
        for raw in (bad.encode(), b"garbage line\n\n", b"event: nonsense\nid: x\ndata: {}\n\n", b"\xff\xfe\n\n",
                    b"id: %s:1\nevent: event\ndata: {}\ndata: {}\n\n" % STORE.encode()):
            s = self.stub(lambda r, raw=raw: sse([raw]))
            with self.assertRaises(ProtocolError, msg=raw):
                next(Client(s.url).watch(f"{STORE}:0"))
            s.close()

    def test_wrong_content_type(self):
        s = self.stub(lambda r: sse([snapshot_frame()], content_type="text/plain"))
        with self.assertRaises(ProtocolError):
            next(Client(s.url).watch())

    def test_snapshot_size_bound(self):
        s = self.stub(lambda r: sse([snapshot_frame(jobs=[job(text="x" * 5000)])]))
        with self.assertRaises(ProtocolError):
            next(Client(s.url, max_response_bytes=1000).watch())

    def test_idle_timeout_and_total_deadline(self):
        s = self.stub(lambda r: sse([b": connected\n\n"], hold=3))
        started = time.monotonic()
        with self.assertRaises(RequestTimeout):
            next(Client(s.url, stream_idle_timeout=0.3).watch(f"{STORE}:0"))
        self.assertLess(time.monotonic() - started, 2)
        with self.assertRaises(RequestTimeout):
            next(Client(s.url, stream_idle_timeout=5).watch(f"{STORE}:0", timeout=0.3))

    def test_closing_a_stream_sends_no_mutation(self):
        s = self.stub(lambda r: sse([snapshot_frame()], hold=1))
        with Client(s.url).watch() as stream:
            next(stream)
        self.assertEqual(s.posts(), [])
        with self.assertRaises(StopIteration):
            next(stream)


def ev(seq, kind, **payload):
    return Frame("event", Cursor(STORE, seq), event(seq, kind, **payload))


class MirrorTests(unittest.TestCase):
    def fresh(self, jobs=None):
        m = Mirror()
        m.apply(Frame("snapshot", Cursor(STORE, 5), {"cursor": {"store_id": STORE, "sequence": "5"}, "queue": {"paused": False, "pending": [], "max_pending": 64}, "jobs": jobs or []}))
        return m

    def test_snapshot_replaces_everything(self):
        m = self.fresh([job("running", "old")])
        self.assertEqual(m.text(JOB), "old")
        m.apply(Frame("snapshot", Cursor(STORE, 9), {"cursor": {"store_id": STORE, "sequence": "9"}, "queue": None, "jobs": []}))
        self.assertEqual((m.jobs, str(m.cursor)), ({}, f"{STORE}:9"))

    def test_unicode_output_uses_utf8_byte_offsets(self):
        m = self.fresh()
        m.apply(ev(6, "job.accepted", job_id=JOB, job=job("running")))
        self.assertTrue(m.apply(ev(7, "job.output", job_id=JOB, output={"offset_bytes": 0, "text": "é"})))
        self.assertTrue(m.apply(ev(8, "job.output", job_id=JOB, output={"offset_bytes": 2, "text": "世界"})))
        self.assertTrue(m.apply(ev(9, "job.output", job_id=JOB, output={"offset_bytes": 8, "text": "🙂"})))
        self.assertEqual((m.text(JOB), str(m.cursor)), ("é世界🙂", f"{STORE}:9"))
        # a code-point offset (4) instead of 12 bytes would be wrong and is refused
        with self.assertRaises(CursorError):
            m.apply(ev(10, "job.output", job_id=JOB, output={"offset_bytes": 4, "text": "x"}))
        self.assertEqual(m.cursor.sequence, 9, "a refused frame changes nothing")

    def test_duplicates_ignored_gaps_and_wrong_store_refused(self):
        m = self.fresh()
        m.apply(ev(6, "queue.state", queue={"paused": True, "pending": [], "max_pending": 64}))
        self.assertFalse(m.apply(ev(6, "queue.state", queue={"paused": False, "pending": [], "max_pending": 64})))
        self.assertFalse(m.apply(ev(2, "queue.state", queue={"paused": False, "pending": [], "max_pending": 64})))
        self.assertTrue(m.queue["paused"])
        with self.assertRaises(CursorError):
            m.apply(ev(8, "queue.state", queue={"paused": False, "pending": [], "max_pending": 64}))
        other = Frame("event", Cursor("f" * 32, 7), event(7, "queue.state", queue={"paused": False, "pending": [], "max_pending": 64}))
        with self.assertRaises(CursorError):
            m.apply(other)
        self.assertEqual(m.cursor.sequence, 6)

    def test_job_replacement_and_atomic_validation(self):
        m = self.fresh()
        m.apply(ev(6, "job.accepted", job_id=JOB, job=job("queued")))
        with self.assertRaises(CursorError):
            m.apply(ev(7, "job.state", job_id="b" * 32, job=job("running")))  # id mismatch
        self.assertEqual((m.job(JOB).state, m.cursor.sequence), ("queued", 6))
        m.apply(ev(7, "job.state", job_id=JOB, job=job("running", "t")))
        m.apply(ev(8, "job.result", job_id=JOB, job=job("succeeded", "tt")))
        self.assertEqual((m.job(JOB).state, m.text(JOB)), ("succeeded", "tt"))
        with self.assertRaises(CursorError):
            m.apply(ev(9, "job.output", job_id="c" * 32, output={"offset_bytes": 0, "text": "x"}))
        with self.assertRaises(CursorError):
            m.apply(ev(9, "job.nonsense"))

    def test_event_before_snapshot_is_refused(self):
        with self.assertRaises(CursorError):
            Mirror().apply(ev(1, "queue.state", queue={}))

    def test_mirror_does_not_alias_frame_data(self):
        m = self.fresh()
        j = job("running", "a")
        m.apply(ev(6, "job.accepted", job_id=JOB, job=j))
        m.apply(ev(7, "job.output", job_id=JOB, output={"offset_bytes": 1, "text": "b"}))
        self.assertEqual(j["result"]["text"], "a")


class FollowTests(Base):
    def test_bounded_reconnect_resumes_from_last_applied_cursor(self):
        q = {"paused": True, "pending": [], "max_pending": 64}
        conns = []

        def handler(req):
            conns.append(req.path)
            n = len(conns)
            if n == 1:
                return sse([snapshot_frame("5"), frame(6, event(6, "queue.state", queue=q))])
            if n == 2:
                return sse([frame(7, event(7, "queue.state", queue=q))])
            return sse([])

        s = self.stub(handler)
        m = Mirror()
        applied = []
        with self.assertRaises(StreamClosed):
            for f, ok in Client(s.url).follow(m, max_reconnects=1, reconnect_delay=0.01):
                applied.append((f.cursor.sequence, ok))
        self.assertEqual(applied, [(5, True), (6, True), (7, True)])
        self.assertEqual(len(conns), 2, "one reconnect, then the bound is reached")
        self.assertTrue(conns[1].endswith(f"after={STORE}%3A6"), conns)

    def test_no_reconnect_by_default(self):
        s = self.stub(lambda r: sse([snapshot_frame()]))
        with self.assertRaises(StreamClosed):
            list(Client(s.url).follow(Mirror()))
        self.assertEqual(len(s.requests), 1)

    def test_cursor_faults_are_never_retried_or_reset(self):
        for status, code in ((410, "cursor_expired"), (400, "cursor_invalid")):
            s = self.stub(lambda r, status=status, code=code: (status, {"code": code, "message": code, "retryable": False}))
            m = Mirror()
            m.cursor = Cursor(STORE, 3)
            with self.assertRaises(ServiceFault) as cm:
                list(Client(s.url).follow(m, max_reconnects=5, reconnect_delay=0.01))
            self.assertEqual(cm.exception.code, code)
            self.assertEqual(len(s.requests), 1)
            self.assertEqual(m.cursor.sequence, 3, "the cursor must not be reset")
            s.close()

    def test_slow_consumer_may_reconnect_within_the_bound(self):
        err = b'event: error\ndata: {"code":"slow_consumer","message":"slow_consumer","retryable":true}\n\n'
        seq = []

        def handler(req):
            seq.append(1)
            return sse([snapshot_frame("5"), err]) if len(seq) == 1 else sse([snapshot_frame("5"), frame(6, event(6, "queue.state", queue={"paused": False, "pending": [], "max_pending": 64}))])

        s = self.stub(handler)
        m = Mirror()
        out = []
        with self.assertRaises(StreamClosed):
            for f, ok in Client(s.url).follow(m, max_reconnects=1, reconnect_delay=0.01):
                out.append(f.cursor.sequence)
        self.assertEqual(out, [5, 5, 6])

    def test_wrong_store_after_reconnect_is_an_error(self):
        other = "f" * 32
        wrong = f"id: {other}:9\nevent: event\ndata: " + json.dumps({"cursor": {"store_id": other, "sequence": "9"}, "at": "x", "kind": "queue.state", "queue": {}}) + "\n\n"
        s = self.stub(lambda r: sse([wrong.encode()]))
        m = Mirror()
        m.cursor = Cursor(STORE, 8)
        with self.assertRaises(CursorError):
            list(Client(s.url).follow(m))


if __name__ == "__main__":
    unittest.main()
