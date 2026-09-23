# Worker contracts (version 1)

The core lifecycle, journal, and local Ollama adapter implement these contracts.
The executable exposes them through [HTTP/SSE and CLI](http.md); the
[Python client](python.md) and [MCP bridge](mcp.md) are further clients of that
same service.
See [Ollama compatibility and limits](ollama.md).
The Go declarations in `internal/core` are the shared adapter boundary. Core
owns validation, serialization of mutations, and immutable snapshots. Callers
receive copies, never mutable references to authoritative state.

## Authority and connection

Only `serve` opens the store, acquires exclusive ownership, constructs a backend,
and starts the scheduler. A second service using the same data directory must
fail before reading or mutating jobs. Different directories are distinct explicit
workers. Clients never open storage or launch an implicit service.

Service address resolution: explicit `--server`, then `TASKWORKER_SERVER`, then
`http://127.0.0.1:7433`. Only loopback HTTP is supported. No service means a
connection error. Data directory resolution: `serve --data-dir`, then
`TASKWORKER_DATA_DIR`, then `os.UserConfigDir()/taskworker/data`. Ollama endpoint
resolution: `serve --ollama`, then `TASKWORKER_OLLAMA_ENDPOINT`, then
`http://127.0.0.1:11434`. See [transport contract](http.md) for exact routes,
commands, status codes, timeouts, SSE and browser policy.

`Service` defines submission, retry, branch, cancel, queue controls, snapshots,
job lookup, model discovery, and event watching. Listing derives from the bounded
snapshot; result lookup uses `GetJob`. Wait/infer convenience submits once and
observes that job to a terminal state. A wait timeout, watch close, disconnect,
or HTTP request cancellation cancels only that call. Accepted inference uses a
service-owned context; only explicit job cancellation or service shutdown stops it.

Calls use buffered replies so their contexts can stop waiting during a durable
commit or replay read. An in-flight commit continues; a timed-out create must be
resolved by resubmitting its original command and key. Commands cancelled before
entering the mutation owner do not change state. Options are copied at the public
call boundary, including when a caller detaches before acceptance completes.

## Requests and isolation

`Request` contains model ID, role, system_prompt, prompt, and generation options.
Model and prompt must not be whitespace-only. Model IDs are exact installed IDs;
the service must never silently fetch a model. Strings must be valid UTF-8. Role
is persona/instruction text, never a transport message role.

Canonical instruction composition uses `strings.TrimSpace` on role and
system_prompt, then joins nonempty parts with exactly two newlines, role first:

```text
role = "  Be a concise reviewer. "
system_prompt = " Use only the supplied evidence. "
system = "Be a concise reviewer.\n\nUse only the supplied evidence."
```

An empty part contributes no separator. The user prompt is preserved verbatim
and sent separately as a user message. Both original fields and composed
`Instructions` are stored for inspection. Both instruction fields have the same
backend system-message authority; their order is deterministic but cannot promise
that a model resolves conflicting instructions as intended.

Every fresh submission has empty history. No prior prompt, output, conversation
ID, backend context handle, or KV-cache token handle is carried between jobs.
Backend model residency may be shared; this is conversation isolation, not an OS
sandbox. There is no automatic tool execution. The backend consumes materialized
instructions, not an implicit conversation reconstructed from its own state.

`max_output_tokens` is required and positive. Requested context must be positive
when set; zero and negative are invalid. Temperature must be finite and
nonnegative; backend-specific unsupported ranges/options are rejected, never
silently ignored. Nil seed/temperature/context means unspecified. Backend defaults
must be labeled unknown until actually reported; `effective_options` is present
only when resolved, not a copy pretending requests were observed settings.

## Context and results

- **Model capacity** is an advertised model maximum, possibly unknown.
- **Requested allocation** is the requested context window, possibly unspecified.
- **Observed loaded allocation** is a backend report with model, source, and UTC
  timestamp. It may be unavailable or stale; it is not a per-job guarantee.

Never fill one field from another. Refresh model metadata on selection and loaded
allocation after loading where supported. Embedding-only models are rejected for
generation. Backend capabilities describe text generation, streaming,
cancellation, context requests/observations, exact pause/resume, and checkpoints.
The Ollama adapter must report the last two false. Worker queue pause is a worker
feature and does not depend on backend inference pause support.

`model_capacity_source` and `model_capacity_observed_at` optionally record the
source/time of advertised capacity. They do not turn advertised capacity into
an effective allocation. Added with the Ollama backend adapter, these fields omit when unknown, preserving
the canonical encoding of older records. New code reads existing journals without
migration; older executables may reject records containing these new fields, so
downgrading a journal after this metadata has been persisted is unsupported.

Budget validation reserves `max_output_tokens` for output, counting composed
instructions, materialized history, user prompt, and backend template overhead
as input. Reject an explicit request above a known model capacity. Reject a known
insufficient allocation; label estimated counts with method and `exact=false`.
Unknown allocation/tokenization cannot yield an exact fit guarantee. No silent
input truncation: if limits prevent preparation, return `invalid_request`.

`Result` retains text throughout execution, plus reported effective model/options,
context observations, finish reason, and usage. Nil metrics mean unknown, not
zero. Counts reported by the engine are separate from estimates; durations are
nanoseconds. Terminal failure/cancellation retains committed partial output.
Backend callbacks append text serially. The core owns accumulated text; final
backend metadata must not overwrite or double-append streamed output.

`Backend.Generate` must validate installed model selection, capabilities,
supported options, and known context budgets before generation. Core validates
UTF-8, required fields, generic option ranges, materialized input/output bounds,
and that explicit context exceeds the output reservation. It does not invent
model capacity or a tokenizer estimate. Backend-specific validation and real
context observations are implemented with the Ollama backend adapter.
Final metadata excluding text must encode to at most 32 KiB. Invalid or oversized
metadata fails the job while retaining committed text. Origin labels are limited
to 1,024 UTF-8 bytes. These bounds protect the reserved terminal record capacity.

## Lifecycle and controls

| From | Allowed next states |
| --- | --- |
| queued | running, cancelled |
| running | cancelling, succeeded, failed, interrupted |
| cancelling | cancelled, interrupted |
| succeeded / failed / cancelled / interrupted | none |

One serialized mutation owner orders all operations and backend notifications.
Only one inference may be active, including while cancelling. FIFO order follows
durable acceptance order, not timestamps. The pending queue is bounded; new
requests fail with `queue_full` when full. Pausing persists a queue flag and
prevents dispatch of pending jobs; active inference continues. Submission while
paused is allowed within the bound. Resume permits normal dispatch. Repeating
the same pause/resume operation is a no-op.

Cancellation of queued work directly commits cancelled. Running cancellation
first commits cancelling and requests backend context cancellation; only after
the backend returns does it commit cancelled and release the execution slot.
Output emitted before backend return may still be retained. If completion was
committed before cancellation was serialized, the terminal result wins. If
cancelling was committed first, cancellation wins even if the backend returns
success. Repeated cancellation returns the current job, including terminal jobs.
Cancellation is cooperative; an unresponsive backend must not permit a second
active request. This is not an exact engine checkpoint or pause.

Retry requires a terminal parent and creates a new queued job/ID, with copied
request and materialized instructions, empty result, and a retry parent link.
There are no automatic retries. Branch also requires a terminal parent and a
complete new request. It copies the parent's materialized history, appends the
parent's user prompt and nonempty retained assistant output, and uses the new
composed system instruction and prompt. Partial output from failed/cancelled/
interrupted parents is included explicitly by this branch action. Branch budgets
include the full resulting history. Parent data is immutable. Continuing is a
branch with an explicit prompt such as "Continue"; identical generation is not
promised, even with the same seed. No prompt patches or ambiguous inherited options.

Restart preserves queued jobs and queue pause state. Previously running or
cancelling jobs become interrupted via new durable events before dispatch resumes.
They retain partial output and require an explicit retry/branch for new inference.
Graceful shutdown stops dispatch, cancels active backend work, and records
interrupted. An unclean shutdown recovers the last durable prefix in the same way.

Construct with `core.NewWorker(store, backend)`; on construction failure the
caller closes the store. On success `Worker.Shutdown(ctx)` owns store closure.
Shutdown preserves pending jobs and the queue pause flag. A shutdown deadline
stops waiting but retains ownership until the backend returns; no second inference
is permitted while an unresponsive backend remains active. Snapshots and job reads
remain available after shutdown or a storage fault as the last published state.

## Mutations, errors, and idempotency

All create commands require a nonempty caller-generated idempotency key, at most
128 UTF-8 bytes. Scope is the whole persistent store, across clients and operation
types. Persist the key, SHA-256 digest of canonical typed command content, and new
job ID in the same transaction as acceptance. Canonical content includes operation,
parent, origin, and all request fields (including omitted versus explicit options),
with fixed field order and no arbitrary JSON maps. Retransmitted identical content
returns the existing job's current state, even if the queue is now full. Reuse with
different content is `conflict`. A dropped response does not establish failure;
retry the same command and key to determine acceptance. No accepted receipt is
evicted in the initial journal implementation.

`Fault` carries stable code, human message, retryable flag, and optional string
details. See `errors.go` for the code vocabulary. Adapters translate transport
errors to this shape and keep protocol status outside the core. Validation and
queue errors do not create jobs; backend failures terminate accepted jobs. Do not
expose prompts or backend secrets in incidental error details. A storage failure
halts mutation/dispatch: no success response or event may advertise unsynced state.

Backend errors from both `Generate` completion and `Models` follow this explicit
allowlist. Wrapped typed faults are recognized using `errors.As`; its first
matching `*Fault`, if non-nil, supplies only its code and allowed retry advice:

| Backend code | Public message | Public retryable |
| --- | --- | --- |
| `invalid_request` | backend rejected the request | false |
| `unsupported` | backend does not support the requested operation or option | false |
| `limit_exceeded` | backend or worker resource limit exceeded | false |
| `backend_unavailable` | backend is unavailable | backend's boolean value |
| `backend_failure` | backend operation failed | backend's boolean value |
| Any other code, generic error, or nil typed fault | backend operation failed (`backend_failure`) | false |

Retryability is advice that an explicit retry may help, not a success guarantee
or permission for an automatic retry. Availability/failure faults retain both
true and false; validation, unsupported operations, and resource limits require
input/configuration/resource intervention. Generic errors do not establish that
retrying is appropriate. Backend-supplied cancellation, interruption, storage,
queue, and cursor codes are not worker control decisions and use the fallback.
Actual worker cancellation/shutdown, output limits, and store failures retain
their existing precedence and handling. A backend validation fault after durable
acceptance leaves a failed job; it does not undo acceptance or its receipt.

All mapped faults are newly allocated, with fixed ASCII messages under 128 bytes,
no details, and a JSON encoding under 256 bytes. Arbitrary backend messages,
details, unknown codes, and wrapper text are never formatted, copied, truncated,
or persisted by this mapping. The bounded fault body fits the existing terminal
reserve; no journal format or public method signature changes. Previously
persisted generic errors remain historical facts and are not reclassified.

A definite size/storage-budget rejection before writing is `limit_exceeded` and
leaves the store usable for operations that still fit its reserves. It is distinct
from an ambiguous write/sync failure (`storage_failure`); see the persistence
policy. Idempotency receipts cover durable acceptance even if subsequent dispatch
fails: resubmission after store recovery resolves the accepted job identity.

## Ordered events and reconnect

The store has a durable random 128-bit lowercase-hex ID. Event sequences are
global across jobs and queue changes, contiguous, increasing uint64 values,
starting at 1. They survive restart and are never reused. Overflow stops writes.
Timestamps are UTC metadata, not an ordering mechanism. A fresh store's cursor
has its new store ID and sequence 0. A replaced/reset store must use a new ID.

JSON cursors are `{ "store_id": "...", "sequence": "42" }`; decimal strings avoid
JavaScript integer precision loss. SSE IDs encode `<store_id>:<sequence>`.
Only canonical unsigned decimal is valid. Wrong store, malformed, or future cursor
returns `cursor_invalid`; an evicted prefix returns `cursor_expired`. Never silently
skip to live output. Cursor expiration requires a new snapshot.

Clients first obtain an atomic `Snapshot` through cursor C, replace their view,
then `Watch(C)`, which replays only events greater than C and bridges atomically
into live delivery. Committed event delivery is at least once across reconnects:
clients deduplicate by cursor, then advance only after application. They reconnect
from the last applied cursor, not the last received network byte. Global watches
have no filtering in version 1, avoiding gaps hidden by filters.

Each event has exactly one typed payload: accepted/state/result carries a full
job replacement; output carries text and its prior UTF-8 byte offset; queue carries
a full queue replacement. Accepted/state/result JobID must match its job. A commit
contains all related changes (for example accepted job plus queue update); the
core publishes only after durable append and in event order. Snapshots occur only
between commits. Intermediate event delivery within a commit can temporarily
precede its queue replacement; clients must not infer a new authoritative state
transition themselves. Offset checks and cursor deduplication prevent double text.

Initial bounds: 64 pending jobs, 1 MiB total materialized input per job, 8 MiB output
per job, 1,000 retained jobs, 16 MiB maximum encoded commit, 256 queued events per
observer and 32 MiB queued event bytes per observer, whichever is reached first.
Backend output should be persisted in bounded chunks, at most 16 KiB each on UTF-8
boundaries. Exceeding an output bound fails that job with `limit_exceeded` and
retained partial text. Encoded size also matters: acceptance must reserve space
for a full terminal job event, accounting for JSON escaping and duplicated original
and composed inputs. Stop accepting output before a terminal replacement could
exceed the encoded record limit. Raw byte bounds alone do not establish a fit.
Observers exceeding either bound disconnect with `slow_consumer`; they replay from
their last applied cursor. They never block generation. Disk sync backpressure may
slow generation, since durable publication takes priority over throughput.

Watch registers its live boundary under the mutation lock, then reads replay in
one-event pages through that boundary. Newer commits enter its bounded live queue
while replay is being read. A slow replay reader can therefore disconnect just
like a slow live reader; reconnect from the last applied cursor. A watch context
cancels only that observer. Closing or overflowing drops its queued copies.

The initial store does not evict jobs/events/receipts or compact. Admission must
reject when retention/storage bounds are reached; it must not silently discard
history. A future retention design must preserve receipt semantics and return
explicit cursor expiration. See persistence policy for storage exhaustion.
