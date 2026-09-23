"""HTTP client for a running TaskWorker service (standard library only)."""

from __future__ import annotations

import http.client
import ipaddress
import json
import os
import socket
import time
import urllib.parse
from dataclasses import dataclass

from .errors import (
    ConnectionFailure,
    InvalidCommandError,
    JobCancelled,
    JobFailed,
    JobInterrupted,
    ProtocolError,
    RequestTimeout,
    ServiceFault,
    StreamClosed,
    TaskWorkerError,
    WaitTimeout,
)
from .models import (
    Command,
    Cursor,
    Job,
    Snapshot,
    branch_command,
    check_job_id,
    retry_command,
    submit_command,
)

DEFAULT_SERVER = "http://127.0.0.1:7433"
MAX_RESPONSE_BYTES = 256 << 20  # same finite bound as the Go client (retained state)
MAX_EVENT_BYTES = 16 << 20
MAX_FAULT_BYTES = 4096
_UNSET = object()


@dataclass(frozen=True)
class Endpoint:
    """A validated loopback HTTP endpoint."""

    host: str  # literal IP (no brackets)
    port: int

    @property
    def url(self):
        host = f"[{self.host}]" if ":" in self.host else self.host
        return f"http://{host}:{self.port}"


def resolve_endpoint(server=None, environ=None):
    """Resolve like the CLI: explicit value, then TASKWORKER_SERVER, then default.

    Only literal loopback HTTP endpoints are accepted (``localhost`` is pinned
    to 127.0.0.1). Proxies, redirects, credentials, paths and remote hosts are
    never used. The default HTTP port 80 applies when a port is omitted.
    """
    environ = os.environ if environ is None else environ
    raw = server or environ.get("TASKWORKER_SERVER") or DEFAULT_SERVER
    bad = InvalidCommandError(f"server must be a loopback http URL such as {DEFAULT_SERVER}: {raw!r}")
    try:
        parts = urllib.parse.urlsplit(raw)
        host = parts.hostname
        port = parts.port
    except ValueError:
        raise bad from None
    if parts.scheme != "http" or parts.username is not None or parts.password is not None or parts.query or parts.fragment or parts.path not in ("", "/") or "?" in raw or "#" in raw or not host:
        raise bad
    if host == "localhost":
        host = "127.0.0.1"
    try:
        ip = ipaddress.ip_address(host)
    except ValueError:
        raise bad from None
    if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped is not None:
        ip = ip.ipv4_mapped
    if not ip.is_loopback:
        raise bad
    if port is None:
        if parts.netloc.endswith(":"):
            raise bad
        port = 80
    if not 1 <= port <= 65535:
        raise bad
    return Endpoint(str(ip), port)


class _Deadline:
    """A total deadline, optionally nested inside an outer observation budget.

    ``cap`` returns the time allowed for the next socket operation: the smallest
    of the per-operation limit, this deadline's remainder and the budget's
    remainder. It records which of them bounded the operation in
    ``budget_bound``, so an expiry can be attributed to the budget or to an
    ordinary limit without guessing from the clock. A tie goes to the budget.
    """

    def __init__(self, seconds, budget=None):
        self.end = None if seconds is None else time.monotonic() + seconds
        self.budget = budget
        self.budget_bound = False

    def expired(self):
        return self.end is not None and time.monotonic() >= self.end

    def remaining(self):
        """Seconds left, or None for no deadline (never negative)."""
        return None if self.end is None else max(0.0, self.end - time.monotonic())

    def cap(self, limit):
        """Seconds allowed for the next socket operation (None: unbounded), or raise on expiry."""
        candidates = [] if limit is None else [(limit, 1)]
        if self.end is not None:
            left = self.end - time.monotonic()
            if left <= 0:
                # Both may have run out (a stall between reads): the earlier end is
                # the limit that fired, and on a tie the budget wins.
                budget = self.budget
                self.budget_bound = budget is not None and budget.end is not None and budget.end <= self.end
                raise TimeoutError("deadline exceeded")
            candidates.append((left, 1))
        if self.budget is not None:
            left = self.budget.remaining()
            if left is not None:
                if left <= 0:
                    self.budget_bound = True
                    raise TimeoutError("observation budget exceeded")
                candidates.append((left, 0))  # 0 sorts first on a tie: the budget wins
        if not candidates:
            self.budget_bound = False
            return None
        seconds, kind = min(candidates)
        self.budget_bound = kind == 0
        return seconds


class _BoundedSocket(socket.socket):
    """A socket that re-applies its ``_Deadline`` before every send and receive.

    ``http.client`` parses response headers with many small reads under one
    socket timeout, so a peer that trickles bytes (never silent for a whole
    read limit) would otherwise stretch header parsing past the call deadline and
    the observation budget. Each ``recv``/``send`` here is capped by the timeout
    last requested through ``settimeout`` (the per-operation limit), the call's
    own deadline and the budget's remainder, and records which one bounded it.
    """

    def __init__(self, raw, deadline):
        base = raw.gettimeout()
        super().__init__(raw.family, raw.type, raw.proto, fileno=raw.detach())
        self.tw_deadline = deadline
        self.tw_base = base
        super().settimeout(base)

    def settimeout(self, value):
        self.tw_base = value
        super().settimeout(value)

    def recv_into(self, buffer, nbytes=0, flags=0):
        super().settimeout(self.tw_deadline.cap(self.tw_base))
        return super().recv_into(buffer, nbytes, flags)

    def sendall(self, data, flags=0):
        super().settimeout(self.tw_deadline.cap(self.tw_base))
        return super().sendall(data, flags)


def _settimeout(conn, seconds):
    """Bound the next socket operation; the deadline is enforced between reads."""
    try:
        conn.tw_sock.settimeout(seconds)
    except (AttributeError, OSError):
        pass


def _no_constants(name):
    raise ValueError(f"{name} is not valid JSON")


def _loads(data):
    try:
        return json.loads(data.decode("utf-8"), parse_constant=_no_constants)
    except (UnicodeDecodeError, ValueError, RecursionError):
        raise ProtocolError("service sent invalid JSON") from None


def _fault(status, data, command=None, job_id=None):
    try:
        body = _loads(data)
    except ProtocolError:
        body = None
    if isinstance(body, dict) and isinstance(body.get("code"), str) and isinstance(body.get("message"), str):
        return ServiceFault(body["code"], body["message"], body.get("retryable", False), body.get("details"), status, command=command, job_id=job_id)
    return ProtocolError(f"unexpected HTTP status {status} (redirects are never followed)")


class Client:
    """Talks to one explicitly running service. Holds no background threads.

    ``operation_timeout`` bounds ordinary calls in total (None disables it);
    ``connect_timeout`` and ``read_timeout`` bound each connect / socket read;
    ``stream_idle_timeout`` bounds silence on an event stream (the service
    sends a heartbeat every 15 s). Waits and watches have no total deadline
    unless you pass one. Nothing is retried automatically.
    """

    def __init__(self, server=None, *, connect_timeout=5.0, read_timeout=35.0,
                 operation_timeout=30.0, stream_idle_timeout=45.0,
                 max_response_bytes=MAX_RESPONSE_BYTES, environ=None):
        self.endpoint = resolve_endpoint(server, environ)
        self.connect_timeout = connect_timeout
        self.read_timeout = read_timeout
        self.operation_timeout = operation_timeout
        self.stream_idle_timeout = stream_idle_timeout
        self.max_response_bytes = max_response_bytes

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def close(self):
        """Present for symmetry; connections are per request and already closed."""

    # ---- transport -------------------------------------------------------

    def _open(self, method, path, body, deadline, command, job_id, accept):
        """Send one request and return (connection, response)."""
        sent = False
        conn = None
        try:
            conn = http.client.HTTPConnection(self.endpoint.host, self.endpoint.port, timeout=deadline.cap(self.connect_timeout))
            conn.connect()
            # Every read and write of this connection is re-capped by the deadline,
            # including the many small reads of header parsing.
            conn.sock = _BoundedSocket(conn.sock, deadline)
            conn.tw_sock = conn.sock  # http.client drops conn.sock when the reply says "close"
            headers = {"Accept": accept, "User-Agent": "taskworker-client/0.1"}
            if body is not None:
                headers["Content-Type"] = "application/json"
            sent = True  # from here on the service may have seen the request
            conn.request(method, path, body=body, headers=headers)
            _settimeout(conn, deadline.cap(self.read_timeout))
            return conn, conn.getresponse()
        except (TimeoutError, socket.timeout):
            self._close(conn)
            raise RequestTimeout("request timed out; acceptance is uncertain for creates", sent=sent, command=command, job_id=job_id, budget_bound=deadline.budget_bound) from None
        except (OSError, http.client.HTTPException):
            self._close(conn)
            raise ConnectionFailure("cannot reach service; start `taskworker serve` or check the server address" + ("; create acceptance may be uncertain" if sent else ""), sent=sent, command=command, job_id=job_id) from None

    @staticmethod
    def _close(conn):
        if conn is not None:
            try:
                conn.close()
            except OSError:
                pass

    def _read_all(self, conn, resp, deadline, limit, command, job_id):
        chunks, size = [], 0
        try:
            while True:
                _settimeout(conn, deadline.cap(self.read_timeout))
                chunk = resp.read1(65536)  # one recv, so the deadline is re-checked between reads
                if not chunk:
                    break
                size += len(chunk)
                if size > limit:
                    raise ProtocolError("service response exceeds the client's size bound")
                chunks.append(chunk)
        except (TimeoutError, socket.timeout):
            raise RequestTimeout("response timed out; acceptance is uncertain for creates", sent=True, command=command, job_id=job_id, budget_bound=deadline.budget_bound) from None
        except (OSError, http.client.HTTPException):
            raise ConnectionFailure("connection broke while reading the response", sent=True, command=command, job_id=job_id) from None
        return b"".join(chunks)

    def _call(self, method, path, body=None, *, timeout=_UNSET, command=None, job_id=None, budget=None):
        total = self.operation_timeout if timeout is _UNSET else timeout
        deadline = _Deadline(total, budget)
        conn, resp = self._open(method, path, body, deadline, command, job_id, "application/json")
        try:
            limit = self.max_response_bytes if resp.status == 200 else MAX_FAULT_BYTES
            data = self._read_all(conn, resp, deadline, limit, command, job_id)
            if resp.status != 200:
                raise _fault(resp.status, data, command, job_id)
            if resp.getheader("Content-Type", "").split(";")[0].strip() != "application/json":
                raise ProtocolError("service did not answer with application/json")
            return _loads(data)
        finally:
            self._close(conn)

    # ---- read operations ---------------------------------------------------

    def health(self, *, timeout=_UNSET):
        """``{"status": "serving", "backend": "not_checked"}``: the service only.

        Health never says anything about the inference backend; use models()."""
        return self._call("GET", "/v1/health", timeout=timeout)

    def models(self, *, timeout=_UNSET):
        """Installed models. Raises ServiceFault(backend_unavailable, ...) if the
        backend is down even though health() succeeds."""
        return self._call("GET", "/v1/models", timeout=timeout)

    def snapshot(self, *, timeout=_UNSET):
        return Snapshot(self._call("GET", "/v1/snapshot", timeout=timeout))

    def list_jobs(self, *, timeout=_UNSET):
        return [Job(j) for j in (self._call("GET", "/v1/jobs", timeout=timeout) or [])]

    def get_job(self, job_id, *, timeout=_UNSET, _budget=None):
        check_job_id(job_id)
        return Job(self._call("GET", f"/v1/jobs/{job_id}", timeout=timeout, job_id=job_id, budget=_budget))

    def result(self, job_id, *, timeout=_UNSET):
        """The complete job, including partial output and terminal state."""
        check_job_id(job_id)
        return Job(self._call("GET", f"/v1/jobs/{job_id}/result", timeout=timeout, job_id=job_id))

    def queue_status(self, *, timeout=_UNSET):
        return self._call("GET", "/v1/queue", timeout=timeout)

    # ---- controls ----------------------------------------------------------

    def pause_queue(self, *, timeout=_UNSET):
        """Stop dispatching pending jobs; the active job continues."""
        return self._call("POST", "/v1/queue/pause", b"{}", timeout=timeout)

    def resume_queue(self, *, timeout=_UNSET):
        return self._call("POST", "/v1/queue/resume", b"{}", timeout=timeout)

    def cancel(self, job_id, *, timeout=_UNSET):
        """Explicitly cancel a job (the only call that stops inference).

        Returns the current job, possibly ``cancelling``; partial output is kept."""
        check_job_id(job_id)
        return Job(self._call("POST", f"/v1/jobs/{job_id}/cancel", b"{}", timeout=timeout, job_id=job_id))

    # ---- creates -----------------------------------------------------------

    def send(self, command, *, timeout=_UNSET):
        """POST exactly ``command.to_bytes()`` once and return the current job.

        No automatic retry, ever. Once the request was sent, only a valid service
        Fault (other than unavailable/storage_failure/internal) or a valid job
        answers the acceptance question. Anything else that arrives afterwards -
        an undecodable or malformed reply, a wrong content type, a reply over the
        client's size bound, an invalid job object, a redirect - cannot prove
        rejection, so the exception is marked ``acceptance_uncertain`` and carries
        the command. Resend the *same* Command (same key) to resolve to the
        original job."""
        if not isinstance(command, Command):
            raise InvalidCommandError("send() requires a Command")
        try:
            raw = self._call("POST", command.path, command.to_bytes(), timeout=timeout, command=command)
            return Job(raw, command=command)
        except ProtocolError as exc:
            exc.command = command
            exc.acceptance_uncertain = True
            raise

    submit = send  # readability alias: client.submit(submit_command(...))

    def retry(self, parent_id, *, idempotency_key=None, origin="python", on_command=None, timeout=_UNSET):
        """Create a NEW job repeating a terminal parent (explicit retry lineage)."""
        command = retry_command(parent_id, origin=origin, idempotency_key=idempotency_key)
        if on_command:
            on_command(command)
        return self.send(command, timeout=timeout)

    def branch(self, parent_id, model, prompt, *, max_output_tokens, on_command=None, timeout=_UNSET, **fields):
        """Create a NEW job continuing from a terminal parent (explicit branch)."""
        command = branch_command(parent_id, model, prompt, max_output_tokens=max_output_tokens, **fields)
        if on_command:
            on_command(command)
        return self.send(command, timeout=timeout)

    # ---- observation -------------------------------------------------------

    def wait(self, job_id, *, timeout=None, poll_interval=0.25, raise_on_terminal_failure=False):
        """Observe a job until it is terminal. Never cancels or resubmits.

        ``timeout`` is the TOTAL observation budget in seconds (positive, or None
        to wait indefinitely) and bounds every poll, including one in flight: each
        poll is limited to the smaller of the remaining budget and the client's
        ordinary ``operation_timeout``. When the budget expires - during a slow
        poll, or because a late reply arrived after it - WaitTimeout is raised
        with ``job_id`` and ``job`` (the last state observed within the budget, or
        None); a late reply is never accepted, even a terminal one, and expiry
        never starts another poll. The job keeps running. An ordinary request
        limit that is shorter than the budget stays a RequestTimeout (which limit
        fired is recorded per socket operation, not guessed from the clock; on an
        exact tie the budget wins), and other
        failures (connection, service fault) keep their own types with ``job_id``.
        Interrupting (KeyboardInterrupt) likewise leaves the job running."""
        check_job_id(job_id)
        if timeout is not None and (isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or timeout <= 0):
            raise InvalidCommandError("wait timeout must be a positive number of seconds or None (use get_job to check once)")
        deadline = _Deadline(timeout)
        last = None

        def expired():
            state = f"still {last.state}" if last is not None else "not observed"
            return WaitTimeout(f"job {job_id} {state} when the {timeout}s observation budget expired", job=last, job_id=job_id)

        while True:
            if deadline.expired():
                raise expired()  # expiry never starts another poll
            try:
                # The ordinary request limit and the observation budget both bound
                # every socket operation of the poll; the poll records which one did.
                seen = self.get_job(job_id, _budget=deadline)
            except RequestTimeout as exc:
                exc.job_id = job_id
                if exc.budget_bound:
                    raise expired() from None  # the budget ran out mid-poll
                raise  # an ordinary (shorter) request limit fired: a real timeout
            except TaskWorkerError as exc:
                exc.job_id = job_id
                raise
            if deadline.expired():
                raise expired()  # a late reply is not an observation within the budget
            last = seen
            if last.is_terminal:
                if raise_on_terminal_failure:
                    self.raise_for_terminal(last)
                return last
            remaining = deadline.remaining()
            pause = poll_interval if remaining is None else min(poll_interval, remaining)
            if pause > 0:
                time.sleep(pause)

    @staticmethod
    def raise_for_terminal(job):
        """Raise JobFailed/JobCancelled/JobInterrupted for an unsuccessful job."""
        if job.state == "failed":
            raise JobFailed(job)
        if job.state == "cancelled":
            raise JobCancelled(job)
        if job.state == "interrupted":
            raise JobInterrupted(job)
        return job

    def infer(self, command=None, *, on_command=None, wait_timeout=None, poll_interval=0.25,
              raise_on_terminal_failure=True, **request_fields):
        """One create followed by observation of that job.

        Pass a ready ``Command`` or the ``submit_command`` fields (model, prompt,
        max_output_tokens, role, system_prompt, ...). ``on_command`` is called
        with the exact command BEFORE anything is sent so the caller can persist
        it. A timeout while waiting never sends a second create: it raises
        WaitTimeout (job still running). A transport failure raises with
        ``.command`` and ``.acceptance_uncertain`` for recovery via ``send``.
        """
        if command is None:
            command = submit_command(**request_fields)
        elif request_fields:
            raise InvalidCommandError("pass either a Command or request fields, not both")
        if on_command:
            on_command(command)
        job = self.send(command)
        try:
            final = self.wait(job.id, timeout=wait_timeout, poll_interval=poll_interval)
            final.command = command
            if raise_on_terminal_failure:
                self.raise_for_terminal(final)
            return final
        except TaskWorkerError as exc:
            if exc.command is None:
                exc.command = command
            raise

    # ---- events ------------------------------------------------------------

    def watch(self, after=None, *, timeout=None):
        """Open the global event stream and return an iterator of complete Frames.

        Without ``after`` the first frame is a snapshot; with ``after`` (a Cursor
        or ``STORE:SEQUENCE``) delivery resumes strictly after it. Use as a
        context manager or call ``close()``; closing detaches only this
        observer and never cancels a job. See ``Mirror`` for applying frames
        and ``follow`` for bounded reconnection."""
        return EventStream(self, after, timeout)

    def follow(self, mirror, *, max_reconnects=0, reconnect_delay=1.0, timeout=None, stop=None):
        """Yield ``(frame, applied)`` while keeping ``mirror`` current.

        Starts from the mirror's cursor (or a snapshot when it has none). On a
        dropped stream, timeout or slow_consumer fault it reconnects from the
        last APPLIED cursor at most ``max_reconnects`` times (default 0: never),
        waiting ``reconnect_delay`` between attempts. cursor_invalid,
        cursor_expired, wrong-store and every other fault are raised, never
        reset silently. ``stop`` may be a threading.Event to end the loop.
        The reconnect budget covers the whole call; there are no threads."""
        attempts = 0
        while True:
            if stop is not None and stop.is_set():
                return
            try:
                with self.watch(mirror.cursor, timeout=timeout) as stream:
                    for frame in stream:
                        applied = mirror.apply(frame)
                        yield frame, applied
                        if stop is not None and stop.is_set():
                            return
            except (StreamClosed, ConnectionFailure, RequestTimeout) as exc:
                failure = exc
            except ServiceFault as exc:
                if exc.code != "slow_consumer":
                    raise
                failure = exc
            if attempts >= max_reconnects:
                raise failure
            attempts += 1
            if stop is not None:
                if stop.wait(reconnect_delay):
                    return
            else:
                time.sleep(reconnect_delay)


@dataclass(frozen=True)
class Frame:
    """One complete SSE frame. ``data`` is the parsed JSON, ``cursor`` its id."""

    kind: str  # "snapshot" or "event"
    cursor: Cursor
    data: dict


class EventStream:
    """Iterator over complete event frames from one connection.

    Only whole frames are yielded. An ended connection raises StreamClosed
    (never a silent stop): end-of-stream is not job completion. ``last_cursor``
    is the cursor of the last frame *delivered*; advance your own applied
    cursor only after applying (Mirror does this for you)."""

    def __init__(self, client, after, timeout):
        if after is not None:
            after = after if isinstance(after, Cursor) else Cursor.parse(after)
        self._client = client
        self._after = after
        self._deadline = _Deadline(timeout)
        self._conn = None
        self._resp = None
        self.last_cursor = after
        self._closed = False

    def __enter__(self):
        self._start()
        return self

    def __exit__(self, *exc):
        self.close()
        return False

    def __iter__(self):
        return self

    def close(self):
        """Detach this observer. Never cancels a job."""
        self._closed = True
        self._client._close(self._conn)
        self._conn = None

    def _start(self):
        if self._conn is not None:
            return
        path = "/v1/events" + (f"?after={urllib.parse.quote(str(self._after), safe='')}" if self._after else "")
        conn, resp = self._client._open("GET", path, None, self._deadline, None, None, "text/event-stream")
        self._conn, self._resp = conn, resp
        if resp.status != 200:
            try:
                data = resp.read(MAX_FAULT_BYTES)
            except (OSError, http.client.HTTPException):
                data = b""
            self.close()
            raise _fault(resp.status, data)
        if resp.getheader("Content-Type", "") != "text/event-stream":
            self.close()
            raise ProtocolError("service did not answer with text/event-stream")

    def _line(self, limit):
        try:
            _settimeout(self._conn, self._deadline.cap(self._client.stream_idle_timeout))
            line = self._resp.readline(limit + 1)
        except (TimeoutError, socket.timeout):
            self.close()
            raise RequestTimeout("event stream timed out (no data or heartbeat)", sent=True) from None
        except (OSError, http.client.HTTPException):
            self.close()
            raise StreamClosed("event stream connection broke", self.last_cursor) from None
        if len(line) > limit:
            self.close()
            raise ProtocolError("event frame exceeds the client's size bound")
        return line

    def __next__(self):
        if self._closed:
            raise StopIteration
        self._start()
        kind = cursor_id = data = None
        while True:
            try:
                line = self._line(self._client.max_response_bytes + 1024 if kind == "snapshot" else MAX_EVENT_BYTES + 1024)
            except TimeoutError:
                self.close()
                raise RequestTimeout("event stream deadline elapsed", sent=True) from None
            if not line:
                self.close()
                raise StreamClosed("event stream ended; this is not job completion", self.last_cursor)
            if not line.endswith(b"\n"):
                self.close()
                raise StreamClosed("event stream ended inside a frame; the partial frame was discarded", self.last_cursor)
            try:
                text = line[:-1].decode("utf-8")
            except UnicodeDecodeError:
                self.close()
                raise ProtocolError("event stream is not valid UTF-8") from None
            if text.endswith("\r"):
                text = text[:-1]
            if text == "":
                if kind is None:
                    continue
                return self._finish(kind, cursor_id, data)
            if text.startswith(":"):
                continue
            field, sep, value = text.partition(": ")
            if not sep:
                self.close()
                raise ProtocolError("malformed event stream line")
            if field == "id":
                cursor_id = value
            elif field == "event":
                kind = value
            elif field == "data":
                if data is not None:
                    self.close()
                    raise ProtocolError("multiple data lines in one frame")
                data = value
            else:
                self.close()
                raise ProtocolError(f"unexpected event stream field {field!r}")

    def _finish(self, kind, cursor_id, data):
        try:
            if kind == "error":
                try:
                    body = _loads(data.encode("utf-8"))
                except (ProtocolError, AttributeError):
                    raise ProtocolError("malformed error frame") from None
                if not isinstance(body, dict) or "code" not in body:
                    raise ProtocolError("malformed error frame")
                raise ServiceFault(body["code"], body.get("message", ""), body.get("retryable", False), body.get("details"))
            if kind not in ("snapshot", "event") or cursor_id is None or data is None:
                raise ProtocolError("incomplete or unknown event frame")
            cursor = Cursor.parse(cursor_id)
            if kind == "event" and len(data) > MAX_EVENT_BYTES:
                raise ProtocolError("event exceeds the 16 MiB bound")
            body = _loads(data.encode("utf-8"))
            if not isinstance(body, dict) or Cursor.from_json(body.get("cursor")) != cursor:
                raise ProtocolError("frame id does not match its cursor")
        except TaskWorkerError:
            self.close()
            raise
        self.last_cursor = cursor
        return Frame(kind, cursor, body)
