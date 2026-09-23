"""Exception hierarchy.

The classes separate the outcomes a caller must treat differently:

* ``InvalidCommandError`` - rejected locally, nothing was sent.
* ``ConnectionFailure`` / ``RequestTimeout`` - transport trouble. A *create*
  that fails this way after bytes were sent has UNCERTAIN acceptance
  (``acceptance_uncertain``); it is not proof of rejection.
* ``ServiceFault`` - the service answered with a Fault (code, message,
  retryable, details). ``retryable`` is advice, never permission to resubmit.
* ``WaitTimeout`` - an observation deadline elapsed; the job is untouched.
* ``JobEnded`` and subclasses - a job was *observed* to end unsuccessfully.
  The job (with retained partial output) is on ``.job``.
* ``ProtocolError`` / ``StreamClosed`` / ``CursorError`` - malformed data, an
  ended event stream (never success), or a cursor/state inconsistency. On a
  create, a ``ProtocolError`` (undecodable, malformed or oversized reply, invalid
  job, redirect) is uncertain too: the command was already sent.
"""

from __future__ import annotations


class TaskWorkerError(Exception):
    """Base class. ``command`` is set when a create command was involved."""

    acceptance_uncertain = False

    def __init__(self, message, *, command=None, job_id=None):
        super().__init__(message)
        self.command = command
        self.job_id = job_id


class InvalidCommandError(TaskWorkerError, ValueError):
    """The command or argument is invalid; nothing was sent."""


class ConnectionFailure(TaskWorkerError):
    """The service could not be reached or the connection broke.

    ``sent`` is False only when the connection was never established (nothing
    reached the service). Otherwise a create may or may not have been accepted.
    """

    def __init__(self, message, *, sent, command=None, job_id=None):
        super().__init__(message, command=command, job_id=job_id)
        self.sent = sent
        self.acceptance_uncertain = bool(sent and command is not None)


class RequestTimeout(TaskWorkerError):
    """A request or observation deadline elapsed. Not proof of rejection.

    ``budget_bound`` is True only inside ``Client.wait``, when the limit that
    fired was the caller's observation budget rather than an ordinary limit."""

    def __init__(self, message, *, sent, command=None, job_id=None, budget_bound=False):
        super().__init__(message, command=command, job_id=job_id)
        self.sent = sent
        self.budget_bound = budget_bound
        self.acceptance_uncertain = bool(sent and command is not None)


class ServiceFault(TaskWorkerError):
    """The service returned a Fault."""

    def __init__(self, code, message, retryable=False, details=None, status=None,
                 *, command=None, job_id=None):
        super().__init__(f"{code}: {message}", command=command, job_id=job_id)
        self.code = code
        self.message = message
        self.retryable = bool(retryable)
        self.details = details
        self.status = status
        # Only these describe an ambiguous outcome for a create; every other
        # code is a definite answer.
        self.acceptance_uncertain = bool(command is not None and code in ("unavailable", "storage_failure", "internal"))


class ProtocolError(TaskWorkerError):
    """The service sent something this client cannot accept.

    For a create this is raised only after the command was sent, so it cannot
    prove rejection: ``Client.send`` marks it ``acceptance_uncertain`` and
    attaches the command. For read-only calls it stays a plain protocol error."""


class CursorError(ProtocolError):
    """Event application found a wrong store, gap or output-offset mismatch."""


class StreamClosed(TaskWorkerError):
    """The event stream ended. EOF is not job completion.

    ``last_cursor`` is the last frame delivered before the end (or None).
    """

    def __init__(self, message, last_cursor=None):
        super().__init__(message)
        self.last_cursor = last_cursor


class WaitTimeout(TaskWorkerError):
    """``wait`` gave up. ``job`` is the last observed state (or None)."""

    def __init__(self, message, job=None, job_id=None):
        super().__init__(message, job_id=job_id)
        self.job = job


class JobEnded(TaskWorkerError):
    """A job was observed in a non-successful terminal state."""

    def __init__(self, job):
        super().__init__(f"job {job.id} {job.state}", job_id=job.id)
        self.job = job

    @property
    def partial_text(self):
        return self.job.text


class JobFailed(JobEnded):
    pass


class JobCancelled(JobEnded):
    pass


class JobInterrupted(JobEnded):
    pass
