"""Applying snapshot and event frames to a local view.

The rules mirror docs/http.md and docs/contracts.md:

* A ``snapshot`` frame replaces the whole view and sets the cursor.
* An ``event`` frame is applied only if it is exactly the next sequence of the
  same store. Duplicates (sequence <= cursor) are ignored; a wrong store or a
  gap raises ``CursorError`` and changes nothing: resynchronize deliberately
  with a new snapshot instead of guessing.
* ``job.accepted`` / ``job.state`` / ``job.result`` replace a whole job,
  ``queue.state`` replaces the queue, and ``job.output`` appends text only when
  its UTF-8 byte offset equals the job's current byte length.
* Each frame is applied atomically. ``cursor`` advances only after the frame
  was applied, so it is always the right value to persist and to resume from.
"""

from __future__ import annotations

import copy

from .errors import CursorError
from .models import Cursor, Job, Snapshot


class Mirror:
    """A local projection of the service, kept current by applying Frames."""

    def __init__(self, snapshot=None):
        self.cursor = None
        self.queue = None
        self.jobs = {}
        self._sizes = {}
        if snapshot is not None:
            self.reset(snapshot)

    def reset(self, snapshot):
        """Replace everything from a Snapshot (the explicit resynchronization)."""
        if not isinstance(snapshot, Snapshot):
            raise TypeError("reset() requires a Snapshot")
        self._replace(snapshot.cursor, snapshot.queue, [j.raw for j in snapshot.jobs])

    def _replace(self, cursor, queue, jobs):
        self.jobs = {j["id"]: copy.deepcopy(j) for j in jobs}
        self._sizes = {i: len(_text(j).encode("utf-8")) for i, j in self.jobs.items()}
        self.queue = copy.deepcopy(queue)
        self.cursor = cursor

    def job(self, job_id):
        raw = self.jobs.get(job_id)
        return None if raw is None else Job(raw)

    def text(self, job_id):
        return _text(self.jobs[job_id])

    def apply(self, frame):
        """Apply one Frame. Returns True if applied, False for a duplicate."""
        if frame.kind == "snapshot":
            data = frame.data
            self._replace(frame.cursor, data.get("queue"), data.get("jobs") or [])
            return True
        if frame.kind != "event":
            raise CursorError(f"cannot apply a {frame.kind!r} frame")
        if self.cursor is None:
            raise CursorError("no snapshot applied yet; start from a snapshot")
        if frame.cursor.store_id != self.cursor.store_id:
            raise CursorError("event is from a different store; resynchronize with a new snapshot")
        if frame.cursor.sequence <= self.cursor.sequence:
            return False
        if frame.cursor.sequence != self.cursor.sequence + 1:
            raise CursorError(f"event gap: have {self.cursor.sequence}, received {frame.cursor.sequence}; resynchronize")
        data = frame.data
        kind = data.get("kind")
        if kind in ("job.accepted", "job.state", "job.result"):
            job = data.get("job")
            if not isinstance(job, dict) or job.get("id") != data.get("job_id"):
                raise CursorError("job event does not match its job_id")
            new_job = copy.deepcopy(job)
            self.jobs[new_job["id"]] = new_job
            self._sizes[new_job["id"]] = len(_text(new_job).encode("utf-8"))
        elif kind == "job.output":
            out = data.get("output")
            job_id = data.get("job_id")
            current = self.jobs.get(job_id)
            if current is None:
                raise CursorError("output for an unknown job; resynchronize")
            if not isinstance(out, dict) or not isinstance(out.get("text"), str) or isinstance(out.get("offset_bytes"), bool) or not isinstance(out.get("offset_bytes"), int):
                raise CursorError("malformed output event")
            if out["offset_bytes"] != self._sizes[job_id]:
                raise CursorError(f"output offset {out['offset_bytes']} != applied {self._sizes[job_id]} bytes; resynchronize")
            result = current.setdefault("result", {})
            result["text"] = _text(current) + out["text"]
            self._sizes[job_id] += len(out["text"].encode("utf-8"))
        elif kind == "queue.state":
            queue = data.get("queue")
            if not isinstance(queue, dict):
                raise CursorError("malformed queue event")
            self.queue = copy.deepcopy(queue)
        else:
            raise CursorError(f"unknown event kind {kind!r}")
        self.cursor = frame.cursor
        return True


def _text(job):
    result = job.get("result")
    text = result.get("text", "") if isinstance(result, dict) else ""
    return text if isinstance(text, str) else ""
