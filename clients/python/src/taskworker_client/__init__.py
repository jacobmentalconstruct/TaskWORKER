"""Standard-library client for a running TaskWorker service.

Only a *running* ``taskworker serve`` process is ever contacted. This package
never opens the journal, runs inference or starts a service. See
docs/python.md for the contract (idempotent creates, timeouts, cursors).
"""

from .client import DEFAULT_SERVER, Client, EventStream, Frame, resolve_endpoint
from .errors import (
    ConnectionFailure,
    CursorError,
    InvalidCommandError,
    JobCancelled,
    JobEnded,
    JobFailed,
    JobInterrupted,
    ProtocolError,
    RequestTimeout,
    ServiceFault,
    StreamClosed,
    TaskWorkerError,
    WaitTimeout,
)
from .mirror import Mirror
from .models import (
    Command,
    Cursor,
    Job,
    Snapshot,
    branch_command,
    new_idempotency_key,
    retry_command,
    submit_command,
)

__version__ = "0.1.0"

__all__ = [
    "Client", "EventStream", "Frame", "Mirror", "Command", "Cursor", "Job", "Snapshot",
    "DEFAULT_SERVER", "resolve_endpoint", "new_idempotency_key",
    "submit_command", "retry_command", "branch_command",
    "TaskWorkerError", "InvalidCommandError", "ConnectionFailure", "RequestTimeout",
    "ServiceFault", "ProtocolError", "CursorError", "StreamClosed", "WaitTimeout",
    "JobEnded", "JobFailed", "JobCancelled", "JobInterrupted",
]
