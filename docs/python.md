# Python client

`clients/python` is a small standard-library-only package (`taskworker_client`)
for scripts. It talks to an **already running** service (`taskworker serve`) over
the [HTTP API](http.md). It never starts a service, opens the journal or runs
inference; a missing service is a `ConnectionFailure`, not permission to create a
separate worker. State, ordering, lineage and fault codes are the service's; the
client does not reconstruct scheduler state.

* Supported Python: **3.10 to 3.14** (CPython; Windows, macOS, Linux). No third-party
  dependencies at runtime.
* Install: `pip install ./clients/python` (a standard `setuptools>=61` build), or use
  it without installing by adding `clients/python/src` to `PYTHONPATH`, or by copying
  the `taskworker_client` directory next to your script. Nothing depends on
  the workspace layout.
* Server resolution matches the CLI: `Client(server)` argument, then
  `TASKWORKER_SERVER`, then `http://127.0.0.1:7433`. Only literal loopback HTTP
  endpoints are accepted (`localhost` is pinned to `127.0.0.1`; `[::1]` and a
  default port 80 work). Proxy variables are ignored and redirects are never
  followed, so a command cannot be sent anywhere else.

```python
from taskworker_client import Client, submit_command

client = Client()
command = submit_command(model="qwen2.5:0.5b", role="Be concise.",
                         system_prompt="Use plain text.", prompt="Say hello.",
                         max_output_tokens=64)
command.save("command.json")       # keep the exact command BEFORE sending
job = client.infer(command)        # exactly one create, then observe that job
print(job.text)
```

## Operations

`health()`, `models()`, `snapshot()`, `list_jobs()`, `get_job(id)`, `result(id)`,
`queue_status()`, `pause_queue()`, `resume_queue()`, `cancel(id)`, `send(command)`
(alias `submit`), `retry(parent_id)`, `branch(parent_id, model, prompt, ...)`,
`wait(id)`, `infer(...)`, `watch(after)`, `follow(mirror)`.
Health only says the service is serving; `models()` reports backend
availability (`ServiceFault` `backend_unavailable`). `Job` exposes the exact JSON as
`job.raw` (unknown fields and exact integers preserved) plus `id`, `state`, `text`,
`is_terminal`, `error`, `lineage`.

Examples (run against a live service; each takes `--server`):
`examples/basic_infer.py` (role/system/prompt), `examples/observe_and_cancel.py`
(live output, explicit cancel, detach), `examples/recover_uncertain.py`.

## Idempotent creates and uncertain outcomes

All creates need a caller key. `submit_command`, `retry_command` and `branch_command`
generate one (`py-<uuid>`) *at construction time* unless you pass
`idempotency_key=`; the immutable `Command` exposes `.operation`, `.key`,
`.to_bytes()` and `.save(path)`. `send()` posts exactly those bytes once. Nothing is
ever retried automatically and a replacement key is never generated.
`infer(..., on_command=callback)` calls you with the command *before* sending.

The exceptions (all subclasses of `TaskWorkerError`) separate the outcomes:

| Exception | Meaning |
| --- | --- |
| `InvalidCommandError` | Rejected locally; nothing was sent. |
| `ConnectionFailure` (`sent=False`) | Could not connect; nothing reached the service. |
| `ConnectionFailure` / `RequestTimeout` (`sent=True`) | A create may or may not have been accepted: `acceptance_uncertain` is true and `.command` is attached. A timeout is not proof of rejection. |
| `ServiceFault` | The service answered: `.code`, `.message`, `.retryable`, `.details`, `.status`. `retryable` is advice, never permission to resubmit. Only `unavailable`, `storage_failure` and `internal` leave a create uncertain. |
| `WaitTimeout` | The `wait()`/`infer(wait_timeout=)` observation budget expired (also during a slow poll, or because a late reply arrived after it): `.job_id`, and `.job` = the last state observed within the budget or `None`. The job keeps running; a late reply is never accepted, not even a terminal one. |
| `JobFailed` / `JobCancelled` / `JobInterrupted` | An *observed* unsuccessful terminal job (`infer`/`wait(raise_on_terminal_failure=True)`); `.job` and `.partial_text` keep the retained output. |
| `ProtocolError`, `StreamClosed`, `CursorError` | Malformed data, an ended event stream (never success), or a cursor/offset inconsistency. On a **create** (`send`, `submit`, `retry`, `branch`, `infer`) a `ProtocolError` - undecodable or malformed reply, wrong content type, reply over `max_response_bytes`, invalid job object, redirect - happens after the command was sent, so it is marked `acceptance_uncertain` and carries `.command`. The same reply to a read-only call stays a plain `ProtocolError`. |

Recovery: resend the identical saved command.

```python
from taskworker_client import Command
job = Client().send(Command.load("submit", "command.json"))   # same key -> original job
```

This works after a script or service restart. A different command under the same key
is `conflict`. `retry()` and `branch()` make **new** jobs with their own keys; they
are not recovery. `infer()` performs one create then observes that job; a wait timeout
never sends a second create.

## Timeouts

`Client(connect_timeout=5, read_timeout=35, operation_timeout=30,
stream_idle_timeout=45)`. Ordinary calls have a 30 s total deadline (`timeout=`
per call, `None` to disable). `wait()` and `watch()` have no total deadline unless you
pass `timeout=`. `wait(timeout=T)` is a total budget that also bounds each poll (the smaller of the remaining budget and `operation_timeout`): expiry raises `WaitTimeout` even mid-poll, an ordinary limit (`operation_timeout`, `read_timeout`, `connect_timeout`) that is shorter than the remaining budget stays a `RequestTimeout` (each socket operation records which limit bounded it, so this never depends on how close the budget is; on an exact tie the budget wins; the deadline is re-applied before every read and write, including the small reads that parse response headers, so a peer that trickles bytes cannot stretch a call past its deadline or the budget), and `T` must be positive (use `get_job` to check once). Response bodies are bounded to 256 MiB and event frames to 16 MiB
(snapshots to the 256 MiB response bound). Interrupting a wait/watch, closing a stream
or exiting the interpreter detaches the observer and never cancels inference; only
`cancel()` does.

## Events, snapshots and reconnect

`watch(after=None)` returns an iterator of complete `Frame(kind, cursor, data)`
values; partial frames are never yielded. Without `after` the first frame is a
`snapshot`. `Cursor` holds `store_id` and an exact integer `sequence` (the wire form
keeps it a decimal string; `str(cursor)` is `STORE:SEQUENCE`). If the connection ends
the iterator raises `StreamClosed`; end-of-stream is not job completion.

`Mirror` applies frames: a snapshot replaces everything; an event applies only if it
is the next sequence of the same store (duplicates are ignored; a gap or wrong store
raises `CursorError`); `job.output` requires its UTF-8 byte offset to equal the
applied text's byte length (offsets count bytes, not characters); job/queue events
replace whole objects. `mirror.cursor` advances only after a frame applied, so it is
the value to persist and to reconnect from.

`client.follow(mirror, max_reconnects=0, reconnect_delay=1.0)` reconnects from the
last applied cursor at most `max_reconnects` times (default never), also for a
`slow_consumer` disconnect; `cursor_invalid`, `cursor_expired`, wrong-store and other
faults are raised, never reset silently. To resynchronize deliberately take
`Mirror(client.snapshot())`. There are no background threads or queues.

## Tests

`python -m unittest discover -s clients/python/tests` runs the deterministic unit
tests. Integration tests need the test-only scripted service: build
`./internal/testservice/cmd/fakeworker` and set `TASKWORKER_FAKEWORKER` (and
optionally `TASKWORKER_EXE` for CLI comparison). `scripts/verify.ps1` does all of
this. No Ollama or network is required. Tests create their scratch folders with plain
`os.makedirs` under the system temp directory (or `TASKWORKER_TEST_TMP`) instead of
`tempfile.mkdtemp`, whose owner-only Windows ACL (Python 3.12+) breaks child-process
and sandboxed runners (WinError 5).
