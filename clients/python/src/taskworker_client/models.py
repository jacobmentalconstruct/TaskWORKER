"""Values exchanged with the service: cursors, jobs and create commands."""

from __future__ import annotations

import json
import math
import os
import re
import uuid
from dataclasses import dataclass

from .errors import InvalidCommandError, ProtocolError

MAX_COMMAND_BYTES = 8 << 20
MAX_KEY_BYTES = 128
TERMINAL_STATES = frozenset({"succeeded", "failed", "cancelled", "interrupted"})
_STORE = re.compile(r"[0-9a-f]{32}\Z")
_SEQ = re.compile(r"(0|[1-9][0-9]*)\Z")
_JOB_ID = re.compile(r"[0-9a-f]{32}\Z")


@dataclass(frozen=True)
class Cursor:
    """A committed-event position: store identity plus an exact uint64 sequence.

    The service transports the sequence as a decimal string so no client
    (JavaScript included) rounds it; here it is a Python ``int``. ``str()``
    is the canonical ``STORE:SEQUENCE`` form used by ``after`` and SSE ids.
    """

    store_id: str
    sequence: int

    def __str__(self):
        return f"{self.store_id}:{self.sequence}"

    @classmethod
    def parse(cls, text):
        if not isinstance(text, str):
            raise ProtocolError("cursor must be a string")
        store, sep, seq = text.partition(":")
        if not sep or not _STORE.match(store) or not _SEQ.match(seq) or int(seq) >= 1 << 64:
            raise ProtocolError(f"invalid event cursor: {text!r}")
        return cls(store, int(seq))

    @classmethod
    def from_json(cls, value):
        """Build from the service's ``{"store_id", "sequence": "42"}`` object."""
        if not isinstance(value, dict) or set(value) != {"store_id", "sequence"} or not isinstance(value["sequence"], str):
            raise ProtocolError("invalid cursor object")
        return cls.parse(f"{value['store_id']}:{value['sequence']}")

    def to_json(self):
        return {"store_id": self.store_id, "sequence": str(self.sequence)}


def check_job_id(job_id):
    if not isinstance(job_id, str) or not _JOB_ID.match(job_id):
        raise InvalidCommandError("job id must be 32 lowercase hexadecimal characters")
    return job_id


class Job:
    """A job exactly as the service reported it.

    ``raw`` is the parsed JSON object; unknown fields are preserved and integers
    are exact. Convenience accessors never invent values: absent metrics stay
    absent. ``command`` is set on jobs returned by a create call.
    """

    __slots__ = ("raw", "command")

    def __init__(self, raw, command=None):
        if not isinstance(raw, dict) or not isinstance(raw.get("id"), str) or not isinstance(raw.get("state"), str):
            raise ProtocolError("invalid job object")
        self.raw = raw
        self.command = command

    def __getitem__(self, key):
        return self.raw[key]

    def get(self, key, default=None):
        return self.raw.get(key, default)

    @property
    def id(self):
        return self.raw["id"]

    @property
    def state(self):
        return self.raw["state"]

    @property
    def is_terminal(self):
        return self.state in TERMINAL_STATES

    @property
    def text(self):
        """Retained output; partial while running and after failure/cancel."""
        result = self.raw.get("result")
        text = result.get("text", "") if isinstance(result, dict) else ""
        return text if isinstance(text, str) else ""

    @property
    def error(self):
        return self.raw.get("error")

    @property
    def lineage(self):
        return self.raw.get("lineage")

    @property
    def origin(self):
        return self.raw.get("origin")

    def __repr__(self):
        return f"Job(id={self.id!r}, state={self.state!r}, text_bytes={len(self.text.encode('utf-8'))})"


class Snapshot:
    """Atomic view: cursor, queue and every retained job."""

    __slots__ = ("raw", "cursor", "queue", "jobs")

    def __init__(self, raw):
        if not isinstance(raw, dict) or not isinstance(raw.get("jobs"), (list, type(None))):
            raise ProtocolError("invalid snapshot")
        self.raw = raw
        self.cursor = Cursor.from_json(raw.get("cursor"))
        self.queue = raw.get("queue")
        self.jobs = [Job(j) for j in (raw.get("jobs") or [])]


def new_idempotency_key():
    """A fresh unique key. Generate it BEFORE sending and keep it."""
    return "py-" + uuid.uuid4().hex


def _key(key):
    if key is None:
        return new_idempotency_key()
    if not isinstance(key, str) or not key or len(key.encode("utf-8", "surrogatepass")) > MAX_KEY_BYTES:
        raise InvalidCommandError("idempotency_key must be a nonempty string of at most 128 UTF-8 bytes")
    return key


def _int(name, value, minimum):
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        raise InvalidCommandError(f"{name} must be an integer >= {minimum}")
    return value


def _options(max_output_tokens, context_tokens, temperature, seed):
    options = {"max_output_tokens": _int("max_output_tokens", max_output_tokens, 1)}
    if context_tokens is not None:
        options["context_tokens"] = _int("context_tokens", context_tokens, 1)
    if temperature is not None:
        if isinstance(temperature, bool) or not isinstance(temperature, (int, float)) or not math.isfinite(temperature) or temperature < 0:
            raise InvalidCommandError("temperature must be a finite number >= 0")
        options["temperature"] = temperature
    if seed is not None:
        if isinstance(seed, bool) or not isinstance(seed, int) or not -(1 << 63) <= seed < 1 << 63:
            raise InvalidCommandError("seed must be a 64-bit signed integer")
        options["seed"] = seed
    return options


def _request(model, prompt, role, system_prompt, max_output_tokens, context_tokens, temperature, seed):
    for name, value in (("model", model), ("prompt", prompt)):
        if not isinstance(value, str) or not value.strip():
            raise InvalidCommandError(f"{name} must be a nonempty string")
    request = {"model": model}
    for name, value in (("role", role), ("system_prompt", system_prompt)):
        if value is not None:
            if not isinstance(value, str):
                raise InvalidCommandError(f"{name} must be a string")
            if value:
                request[name] = value
    request["prompt"] = prompt
    request["options"] = _options(max_output_tokens, context_tokens, temperature, seed)
    return request


def _origin(origin):
    if not isinstance(origin, str) or not origin or len(origin.encode("utf-8", "surrogatepass")) > 1024:
        raise InvalidCommandError("origin must be a nonempty string of at most 1024 UTF-8 bytes")
    return origin


def _reject_constant(name):
    raise ValueError(f"{name} is not valid JSON")


class Command:
    """An exact create command: operation, body and idempotency key.

    Immutable. ``to_bytes()`` is precisely what is sent, and ``save()`` writes
    the same bytes, so a saved file can be resent later (``Command.load``) or
    given to ``taskworker submit --command FILE``. Saving before sending is what
    makes an uncertain create recoverable: resend the identical command (same
    key) and the service returns the original job.
    """

    __slots__ = ("operation", "body", "_bytes")

    def __init__(self, operation, body):
        if operation not in ("submit", "retry", "branch"):
            raise InvalidCommandError("operation must be submit, retry or branch")
        if not isinstance(body, dict):
            raise InvalidCommandError("command body must be an object")
        try:
            data = json.dumps(body, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode("utf-8")
        except (TypeError, ValueError) as exc:  # includes lone surrogates and NaN
            raise InvalidCommandError(f"command is not encodable as strict UTF-8 JSON: {exc}") from None
        if len(data) > MAX_COMMAND_BYTES:
            raise InvalidCommandError("command exceeds the 8 MiB limit")
        key = body.get("idempotency_key")
        if not isinstance(key, str):
            raise InvalidCommandError("idempotency_key is required")
        _key(key)
        object.__setattr__(self, "operation", operation)
        object.__setattr__(self, "body", json.loads(data))
        object.__setattr__(self, "_bytes", data)

    def __setattr__(self, name, value):
        raise AttributeError("Command is immutable")

    @property
    def key(self):
        return self.body["idempotency_key"]

    @property
    def path(self):
        return "/v1/" + self.operation

    def to_bytes(self):
        return self._bytes

    def to_json(self):
        return self._bytes.decode("utf-8")

    def save(self, path):
        """Write the exact command (UTF-8, no BOM) and flush it to disk."""
        with open(path, "wb") as handle:
            handle.write(self._bytes)
            handle.flush()
            os.fsync(handle.fileno())
        return path

    @classmethod
    def load(cls, operation, path):
        with open(path, "rb") as handle:
            data = handle.read(MAX_COMMAND_BYTES + 1)
        if len(data) > MAX_COMMAND_BYTES:
            raise InvalidCommandError("command file exceeds the 8 MiB limit")
        try:
            body = json.loads(data.decode("utf-8"), parse_constant=_reject_constant)
        except (UnicodeDecodeError, ValueError) as exc:
            raise InvalidCommandError(f"command file is not valid UTF-8 JSON: {exc}") from None
        return cls(operation, body)

    def __eq__(self, other):
        return isinstance(other, Command) and self.operation == other.operation and self._bytes == other._bytes

    def __hash__(self):
        return hash((self.operation, self._bytes))

    def __repr__(self):
        return f"Command({self.operation!r}, key={self.key!r})"


def submit_command(model, prompt, *, max_output_tokens, role=None, system_prompt=None,
                   context_tokens=None, temperature=None, seed=None,
                   origin="python", idempotency_key=None):
    """Build a fresh-conversation submit command. Omitted options stay unknown."""
    return Command("submit", {
        "idempotency_key": _key(idempotency_key),
        "origin": _origin(origin),
        "request": _request(model, prompt, role, system_prompt, max_output_tokens, context_tokens, temperature, seed),
    })


def retry_command(parent_id, *, origin="python", idempotency_key=None):
    """A NEW job repeating a terminal parent. Not recovery of an uncertain create."""
    return Command("retry", {
        "idempotency_key": _key(idempotency_key),
        "origin": _origin(origin),
        "parent_id": check_job_id(parent_id),
    })


def branch_command(parent_id, model, prompt, *, max_output_tokens, role=None, system_prompt=None,
                   context_tokens=None, temperature=None, seed=None,
                   origin="python", idempotency_key=None):
    """A NEW job continuing from a terminal parent's prompt and retained output."""
    return Command("branch", {
        "idempotency_key": _key(idempotency_key),
        "origin": _origin(origin),
        "parent_id": check_job_id(parent_id),
        "request": _request(model, prompt, role, system_prompt, max_output_tokens, context_tokens, temperature, seed),
    })
