"""A tiny scriptable HTTP server for client unit tests (no real service)."""

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

STORE = "0123456789abcdef0123456789abcdef"
JOB = "a" * 32


class Request:
    def __init__(self, handler, body):
        self.method = handler.command
        self.path = handler.path
        self.headers = handler.headers
        self.body = body
        self.handler = handler


def job(state="running", text="", job_id=JOB, **extra):
    raw = {"id": job_id, "state": state, "request": {"model": "m", "prompt": "p", "options": {"max_output_tokens": 1}},
           "instructions": {"system": "", "prompt": "p"}, "created_at": "2026-01-01T00:00:00Z",
           "result": {"text": text, "context": {"reserved_output_tokens": 0}, "usage": {}}}
    raw.update(extra)
    return raw


def frame(seq, data, kind="event", cursor=None):
    cursor = cursor or f"{STORE}:{seq}"
    return f"id: {cursor}\nevent: {kind}\ndata: {json.dumps(data)}\n\n".encode("utf-8")


def event(seq, kind, **payload):
    return {"cursor": {"store_id": STORE, "sequence": str(seq)}, "at": "2026-01-01T00:00:00Z", "kind": kind, **payload}


class Stub:
    """``handler(request)`` returns (status, obj_or_bytes) or a callable(request)
    that writes the raw response itself."""

    def __init__(self, handler=None):
        self.requests = []
        self.handler = handler or (lambda r: (404, {"code": "not_found", "message": "not_found", "retryable": False}))
        stub = self

        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"  # body ends at close: simplest raw control

            def log_message(self, *a):
                pass

            def _serve(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(n) if n else b""
                req = Request(self, body)
                stub.requests.append(req)
                out = stub.handler(req)
                if callable(out):
                    out(req)
                    return
                status, payload = out
                data = payload if isinstance(payload, bytes) else json.dumps(payload).encode("utf-8")
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            do_GET = do_POST = _serve

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.server.daemon_threads = True
        self.server.handle_error = lambda *args: None  # clients abort on purpose
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        self.thread = threading.Thread(target=lambda: self.server.serve_forever(poll_interval=0.02), daemon=True)
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()

    def posts(self):
        return [r for r in self.requests if r.method == "POST"]


def sse(chunks, *, hold=0.0, content_type="text/event-stream"):
    """Respond with the given raw byte chunks, optionally holding the stream open."""

    def write(req):
        h = req.handler
        h.send_response(200)
        h.send_header("Content-Type", content_type)
        h.end_headers()
        for c in chunks:
            h.wfile.write(c)
            h.wfile.flush()
        if hold:
            time.sleep(hold)

    return write


def slow_json(delay, status=200, payload=None):
    """Handler that answers correctly, but only after ``delay`` seconds of silence."""

    def handler(req):
        time.sleep(delay)
        return status, payload

    return handler


def drip_headers(interval, payload, *, step=8, content_type="application/json", status=200):
    """Send the response HEADERS ``step`` bytes at a time, ``interval`` seconds apart,
    then the body at once. The peer is never silent for longer than ``interval``, so
    only a deadline enforced across reads (not a per-read timeout) can bound it."""
    data = json.dumps(payload).encode("utf-8")
    head = (f"HTTP/1.0 {status} OK\r\nContent-Type: {content_type}\r\nX-Pad: {'p' * 40}\r\n"
            f"Content-Length: {len(data)}\r\n\r\n").encode("ascii")

    def write(req):
        w = req.handler.wfile
        for i in range(0, len(head), step):
            w.write(head[i:i + step])
            w.flush()
            time.sleep(interval)
        w.write(data)
        w.flush()

    return write


def drip_body(total, payload, status=200):
    """Send headers at once, then the JSON body slowly over about ``total`` seconds."""
    data = json.dumps(payload).encode("utf-8")

    def write(req):
        h = req.handler
        h.send_response(status)
        h.send_header("Content-Type", "application/json")
        h.send_header("Content-Length", str(len(data)))
        h.end_headers()
        step = max(1, len(data) // 10)
        for i in range(0, len(data), step):
            h.wfile.write(data[i:i + step])
            h.wfile.flush()
            time.sleep(total / 10)

    return write
