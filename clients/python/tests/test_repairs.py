"""Regression tests for two review findings.

Uncertain creates: after a create was sent, a reply that cannot establish an
authoritative outcome is uncertain and keeps the exact command.
Wait budget: wait(timeout=...) is a TOTAL budget that also bounds every poll.
"""

import json
import os
import socket
import sys
import time
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))
sys.path.insert(0, os.path.dirname(__file__))

from stub import JOB, STORE, Stub, drip_body, drip_headers, job, slow_json, sse  # noqa: E402

from taskworker_client.client import _Deadline  # noqa: E402
from taskworker_client import (  # noqa: E402
    Client, ConnectionFailure, InvalidCommandError, ProtocolError, RequestTimeout, ServiceFault,
    WaitTimeout, branch_command, retry_command, submit_command,
)

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


class UnreadableCreateReplyTests(Base):
    def replies(self):
        deep = b"[" * 100000 + b"]" * 100000
        return {
            "invalid_json": (lambda r: (200, b"{"), {}),
            "invalid_job": (lambda r: (200, {}), {}),
            "job_missing_state": (lambda r: (200, {"id": JOB}), {}),
            "valid_job_over_client_bound": (lambda r: (200, job()), {"max_response_bytes": 32}),
            "deeply_nested_json": (lambda r: (200, deep), {}),
            "wrong_content_type": (lambda r: sse([json.dumps(job()).encode()], content_type="text/plain"), {}),
            "malformed_fault_body": (lambda r: (500, b"<html>oops"), {}),
            "fault_without_code": (lambda r: (409, {"message": "x"}), {}),
            "unexpected_status_no_fault": (lambda r: (302, b""), {}),
        }

    def commands(self):
        return {
            "submit": submit_command("m", "p", max_output_tokens=1, idempotency_key="u-submit"),
            "retry": retry_command(JOB, idempotency_key="u-retry"),
            "branch": branch_command(JOB, "m", "p", max_output_tokens=1, idempotency_key="u-branch"),
        }

    def test_send_keeps_uncertainty_and_command_for_every_unreadable_reply(self):
        for name, (handler, opts) in self.replies().items():
            for op, command in self.commands().items():
                with self.subTest(reply=name, operation=op):
                    s = Stub(handler)
                    try:
                        with self.assertRaises(ProtocolError) as caught:
                            Client(s.url, **opts).send(command)
                        exc = caught.exception
                        self.assertTrue(exc.acceptance_uncertain)
                        self.assertIs(exc.command, command)
                        self.assertEqual(len(s.posts()), 1, "never resent automatically")
                        self.assertEqual(s.posts()[0].body, command.to_bytes())
                    finally:
                        s.close()

    def test_submit_retry_branch_and_infer_helpers_share_the_rule(self):
        for name in ("invalid_json", "invalid_job", "valid_job_over_client_bound"):
            handler, opts = self.replies()[name]
            s = self.stub(handler)
            client = Client(s.url, **opts)
            seen = []
            with self.subTest(reply=name):
                with self.assertRaises(ProtocolError) as c1:
                    client.submit(self.commands()["submit"])
                self.assertTrue(c1.exception.acceptance_uncertain)
                with self.assertRaises(ProtocolError) as c2:
                    client.retry(JOB, idempotency_key="u-retry-helper", on_command=seen.append)
                self.assertTrue(c2.exception.acceptance_uncertain)
                self.assertEqual(c2.exception.command.operation, "retry")
                self.assertIs(c2.exception.command, seen[-1], "the caller saw this exact command first")
                with self.assertRaises(ProtocolError) as c3:
                    client.branch(JOB, "m", "p", max_output_tokens=1, idempotency_key="u-branch-helper", on_command=seen.append)
                self.assertTrue(c3.exception.acceptance_uncertain)
                self.assertEqual(c3.exception.command.operation, "branch")
                with self.assertRaises(ProtocolError) as c4:
                    client.infer(model="m", prompt="p", max_output_tokens=1, idempotency_key="u-infer", on_command=seen.append)
                self.assertTrue(c4.exception.acceptance_uncertain)
                self.assertEqual(c4.exception.command.key, "u-infer")
                self.assertEqual(len(s.posts()), 4, "exactly one POST per call, none resent")

    def test_recovery_by_resending_the_same_command_resolves_to_one_job(self):
        state = {"n": 0}

        def handler(req):
            state["n"] += 1
            return (200, b"{") if state["n"] == 1 else (200, job("queued"))

        s = self.stub(handler)
        command = submit_command("m", "p", max_output_tokens=1, idempotency_key="u-recover")
        client = Client(s.url)
        with self.assertRaises(ProtocolError) as caught:
            client.send(command)
        again = client.send(caught.exception.command)
        self.assertEqual(again.id, JOB)
        self.assertEqual([r.body for r in s.posts()], [command.to_bytes()] * 2)

    def test_read_only_calls_and_definite_outcomes_stay_distinct(self):
        for name, (handler, opts) in self.replies().items():
            with self.subTest(read_only=name):
                s = Stub(handler)
                try:
                    with self.assertRaises(ProtocolError) as caught:
                        Client(s.url, **opts).get_job(JOB)
                    self.assertFalse(caught.exception.acceptance_uncertain)
                    self.assertIsNone(caught.exception.command)
                finally:
                    s.close()
        s = self.stub(lambda r: (409, FAULT))
        c = submit_command("m", "p", max_output_tokens=1, idempotency_key="u-def")
        with self.assertRaises(ServiceFault) as fault:
            Client(s.url).send(c)
        self.assertFalse(fault.exception.acceptance_uncertain, "a valid definitive Fault stays definite")
        with self.assertRaises(ConnectionFailure) as refused:
            Client(f"http://127.0.0.1:{free_port()}").send(c)
        self.assertFalse(refused.exception.acceptance_uncertain, "a pre-send failure stays definite")
        self.assertFalse(refused.exception.sent)


class WaitBudgetTests(Base):
    def test_slow_headers_are_bounded_by_the_budget(self):
        s = self.stub(slow_json(0.8, payload=job("running")))
        started = time.monotonic()
        with self.assertRaises(WaitTimeout) as caught:
            Client(s.url).wait(JOB, timeout=0.2)
        self.assertLess(time.monotonic() - started, 0.6)
        self.assertEqual(caught.exception.job_id, JOB)
        self.assertIsNone(caught.exception.job, "nothing was observed within the budget")

    def test_slow_body_is_bounded_by_the_budget(self):
        s = self.stub(lambda r: drip_body(1.5, job("running", "x" * 200)))
        started = time.monotonic()
        with self.assertRaises(WaitTimeout):
            Client(s.url).wait(JOB, timeout=0.3)
        self.assertLess(time.monotonic() - started, 0.9)

    def test_late_terminal_reply_is_never_success(self):
        s = self.stub(slow_json(0.5, payload=job("succeeded", "late")))
        with self.assertRaises(WaitTimeout):
            Client(s.url).wait(JOB, timeout=0.15)
        with self.assertRaises(WaitTimeout):
            Client(s.url, operation_timeout=None).wait(JOB, timeout=0.15, raise_on_terminal_failure=True)

    def test_prior_observation_is_retained_and_expiry_starts_no_new_poll(self):
        calls = {"n": 0}

        def handler(req):
            calls["n"] += 1
            if calls["n"] == 1:
                return 200, job("running", "earlier")
            time.sleep(0.6)
            return 200, job("succeeded", "later")

        s = self.stub(handler)
        with self.assertRaises(WaitTimeout) as caught:
            Client(s.url).wait(JOB, timeout=0.3, poll_interval=0.02)
        self.assertEqual(caught.exception.job.text, "earlier")
        gets = len([r for r in s.requests if r.method == "GET"])
        time.sleep(0.8)
        self.assertEqual(len([r for r in s.requests if r.method == "GET"]), gets, "expiry must not start another poll")
        self.assertEqual(s.posts(), [], "observation expiry never cancels or resubmits")

    def test_ordinary_request_timeout_shorter_than_the_budget_stays_a_request_timeout(self):
        s = self.stub(slow_json(1.0, payload=job("running")))
        started = time.monotonic()
        with self.assertRaises(RequestTimeout) as caught:
            Client(s.url, operation_timeout=0.2).wait(JOB, timeout=5.0)
        self.assertLess(time.monotonic() - started, 0.9)
        self.assertEqual(caught.exception.job_id, JOB)
        self.assertNotIsInstance(caught.exception, WaitTimeout)

    def test_earlier_failures_keep_their_own_types(self):
        s = self.stub(lambda r: (404, {"code": "not_found", "message": "not_found", "retryable": False}))
        with self.assertRaises(ServiceFault) as fault:
            Client(s.url).wait(JOB, timeout=1.0)
        self.assertEqual((fault.exception.code, fault.exception.job_id), ("not_found", JOB))
        with self.assertRaises(ConnectionFailure):
            Client(f"http://127.0.0.1:{free_port()}").wait(JOB, timeout=10.0)

    def test_infer_wait_timeout_is_one_create_then_a_bounded_observation(self):
        def handler(req):
            if req.method == "POST":
                return 200, job("queued")
            time.sleep(0.8)
            return 200, job("succeeded", "late")

        s = self.stub(handler)
        command = submit_command("m", "p", max_output_tokens=1, idempotency_key="w-infer")
        started = time.monotonic()
        with self.assertRaises(WaitTimeout) as caught:
            Client(s.url).infer(command, wait_timeout=0.2, poll_interval=0.02)
        self.assertLess(time.monotonic() - started, 0.6)
        self.assertEqual(len(s.posts()), 1, "one create, never a second")
        self.assertIs(caught.exception.command, command)
        self.assertEqual(caught.exception.job_id, JOB)

    def test_infer_ordinary_timeout_shorter_than_the_wait_budget(self):
        def handler(req):
            if req.method == "POST":
                return 200, job("queued")
            time.sleep(0.8)
            return 200, job("succeeded", "late")

        s = self.stub(handler)
        with self.assertRaises(RequestTimeout) as caught:
            Client(s.url, operation_timeout=0.2).infer(model="m", prompt="p", max_output_tokens=1, wait_timeout=10)
        self.assertEqual(len(s.posts()), 1)
        self.assertEqual(caught.exception.job_id, JOB)

    def test_unbounded_wait_and_budget_validation(self):
        s = self.stub(lambda r: (200, job("succeeded", "ok")))
        self.assertEqual(Client(s.url, operation_timeout=None).wait(JOB, timeout=None).text, "ok")
        polls = len(s.requests)
        for bad in (0, -1, True, "1"):
            with self.assertRaises(InvalidCommandError, msg=repr(bad)):
                Client(s.url).wait(JOB, timeout=bad)
        self.assertEqual(len(s.requests), polls, "invalid budgets are rejected before any poll")


class WaitLimitAttributionTests(Base):
    """Which limit fired is recorded per socket operation, so the
    result never depends on a clock tolerance near the end of the budget."""

    def slow(self, delay=0.6):
        return self.stub(slow_json(delay, payload=job("running")))

    def test_ordinary_limit_shorter_than_budget_stays_request_timeout_even_near_the_end(self):
        s = self.slow()
        for op, budget in ((0.02, 0.09), (0.05, 0.12), (0.28, 0.3), (0.1, 0.15)):
            for _ in range(3):
                with self.subTest(operation_timeout=op, budget=budget):
                    with self.assertRaises(RequestTimeout) as caught:
                        Client(s.url, operation_timeout=op).wait(JOB, timeout=budget)
                    self.assertNotIsInstance(caught.exception, WaitTimeout)
                    self.assertFalse(caught.exception.budget_bound)
                    self.assertEqual(caught.exception.job_id, JOB)

    def test_budget_shorter_than_ordinary_limit_is_a_wait_timeout(self):
        s = self.slow()
        for op, budget in ((0.3, 0.1), (30.0, 0.15), (None, 0.15), (0.5, 0.05)):
            for _ in range(3):
                with self.subTest(operation_timeout=op, budget=budget):
                    started = time.monotonic()
                    with self.assertRaises(WaitTimeout) as caught:
                        Client(s.url, operation_timeout=op).wait(JOB, timeout=budget)
                    self.assertLess(time.monotonic() - started, budget + 0.4)
                    self.assertEqual(caught.exception.job_id, JOB)

    def test_exact_tie_goes_to_the_budget(self):
        s = self.slow()
        with self.assertRaises(WaitTimeout):
            Client(s.url, operation_timeout=0.15).wait(JOB, timeout=0.15)

    def test_read_limit_shorter_than_budget_is_not_a_budget_expiry(self):
        s = self.slow()
        with self.assertRaises(RequestTimeout) as caught:
            Client(s.url, read_timeout=0.1).wait(JOB, timeout=3.0)
        self.assertFalse(caught.exception.budget_bound, "the read limit fired, not the budget")

    def test_when_both_deadlines_have_expired_the_earlier_end_is_attributed(self):
        """A stall between reads can leave the call deadline AND the budget expired
        by the time the next operation is capped; the limit that ran out first is
        the one that fired, and on an exact tie the budget wins."""
        now = time.monotonic()
        # (budget end offset, call end offset, expected budget_bound)
        cases = (
            (-0.2, -0.1, True),    # the budget ended first
            (-0.1, -0.2, False),   # the call deadline ended first
            (-0.1, -0.1, True),    # exact tie
            (10.0, -0.1, False),   # only the call deadline has expired
            (None, -0.1, False),   # no budget at all
        )
        for budget_end, call_end, expected in cases:
            with self.subTest(budget_end=budget_end, call_end=call_end):
                budget = None
                if budget_end is not None:
                    budget = _Deadline(None)
                    budget.end = now + budget_end
                call = _Deadline(None, budget)
                call.end = now + call_end
                with self.assertRaises(TimeoutError):
                    call.cap(5.0)
                self.assertIs(call.budget_bound, expected)


class TrickledHeaderTests(Base):
    """Review finding (repair 02): the budget and the call deadline must bound a
    response whose HEADERS trickle in. http.client parses headers with many small
    reads under one socket timeout, so a peer that is never silent for a whole read
    limit used to stretch a poll far past a 1 s budget (measured 6.6 s). Each drip
    here is 0.1 s apart while ``read_timeout`` is 1 s: only a deadline enforced
    across reads can stop it. Undripped, the headers would take about 1.4 s."""

    def trickle(self):
        return self.stub(lambda r: drip_headers(0.1, job("running")))

    def test_budget_bounds_a_trickled_header_poll(self):
        s = self.trickle()
        for _ in range(2):
            started = time.monotonic()
            with self.assertRaises(WaitTimeout) as caught:
                Client(s.url, read_timeout=1.0, operation_timeout=None).wait(JOB, timeout=0.5)
            self.assertLess(time.monotonic() - started, 0.5 + 0.4)
            self.assertEqual(caught.exception.job_id, JOB)
            self.assertIsNone(caught.exception.job, "nothing was observed within the budget")

    def test_call_deadline_shorter_than_budget_stays_a_request_timeout(self):
        s = self.trickle()
        started = time.monotonic()
        with self.assertRaises(RequestTimeout) as caught:
            Client(s.url, read_timeout=1.0, operation_timeout=0.3).wait(JOB, timeout=10)
        self.assertNotIsInstance(caught.exception, WaitTimeout)
        self.assertFalse(caught.exception.budget_bound)
        self.assertLess(time.monotonic() - started, 0.3 + 0.4)
        self.assertEqual(caught.exception.job_id, JOB)

    def test_call_deadline_bounds_a_trickled_ordinary_call(self):
        s = self.trickle()
        started = time.monotonic()
        with self.assertRaises(RequestTimeout):
            Client(s.url, read_timeout=1.0, operation_timeout=0.3).get_job(JOB)
        self.assertLess(time.monotonic() - started, 0.3 + 0.4)

    def test_total_deadline_bounds_a_trickled_event_stream_start(self):
        s = self.stub(lambda r: drip_headers(0.1, {}, content_type="text/event-stream"))
        started = time.monotonic()
        with self.assertRaises(RequestTimeout):
            next(Client(s.url, read_timeout=1.0, stream_idle_timeout=5).watch(f"{STORE}:0", timeout=0.4))
        self.assertLess(time.monotonic() - started, 0.4 + 0.4)

    def test_a_trickle_that_finishes_inside_the_budget_is_accepted(self):
        s = self.stub(lambda r: drip_headers(0.005, job("succeeded", "ok")))
        seen = Client(s.url, read_timeout=1.0).wait(JOB, timeout=5)
        self.assertEqual(seen.text, "ok")
        self.assertEqual(len(s.requests), 1)


if __name__ == "__main__":
    unittest.main()
