# HTTP and CLI (version 1)

Start one foreground owner with `taskworker serve`. Clients never start a service
or open journal files. Resolution is `--server`, then `TASKWORKER_SERVER`, then
`http://127.0.0.1:7433`; serve uses that same URL to bind. Only literal loopback
HTTP endpoints are supported; `localhost` is pinned to `127.0.0.1`. No proxies,
redirects, remote binding, authentication or wildcard CORS. Data resolution is
`serve --data-dir`, `TASKWORKER_DATA_DIR`, then the OS user configuration directory
plus `taskworker/data`. Ollama resolution is `serve --ollama`,
`TASKWORKER_OLLAMA_ENDPOINT`, then `http://127.0.0.1:11434`. The existing narrow
[Ollama compatibility gate](ollama.md) remains mandatory.

The configured canonical host and effective port are required on every request.
The listening address always includes a port. For HTTP port 80 only, Host may
omit `:80` (IPv6 brackets remain required); explicit `:80` is also accepted.
Origin, when present, must be `http://` plus one of those same authority forms.
Nondefault ports must be explicit and exact. Host aliases, other loopback sites,
other schemes/ports, paths and query/fragment suffixes are not equivalent.
Multiple, empty, null, hostile and mismatched origins fail 403. Sec-Fetch-Site must be absent,
`none` or `same-origin`. No CORS headers are emitted. Mutations require POST and
application/json, so browser simple forms cannot mutate. CLI requests omit Origin.
This protects against arbitrary websites, not other local processes or a malicious
local user. The [embedded browser UI](ui.md) uses this same policy and service.

## Routes and representations

All successful operations return HTTP 200 and the core JSON object directly.
No transport-specific copy of job state exists. GET bodies and unknown queries
are rejected. Methods are exact; unsupported methods return 405 with Allow.
Unknown routes return 404. IDs are 32 lowercase hexadecimal characters.

| Method / route | Request | Response |
| --- | --- | --- |
| GET /v1/health | none | `{"status":"serving","backend":"not_checked"}` |
| GET /v1/models | none | model array, with capabilities/context |
| GET /v1/snapshot | none | atomic snapshot: cursor, queue, jobs |
| GET /v1/jobs | none | snapshot's job array |
| GET /v1/jobs/ID | none | job, including partial result/error |
| GET /v1/jobs/ID/result | none | same complete job, including terminal status |
| POST /v1/submit | SubmitCommand | current accepted job |
| POST /v1/retry | RetryCommand | current accepted job |
| POST /v1/branch | BranchCommand | current accepted job |
| POST /v1/jobs/ID/cancel | `{}` | current job (possibly cancelling) |
| GET /v1/queue | none | snapshot's queue |
| POST /v1/queue/pause or /resume | `{}` | queue |
| GET /v1/events[?after=CURSOR] | none | global SSE |

Health indicates that the HTTP owner is serving; it does not query the backend or
promise that inference/storage is healthy. Models checks backend availability;
mutation failures report storage faults. Recovery completes before traffic is
accepted. The listener is acquired before journal recovery/dispatch. A second
store owner fails. Ctrl+C/SIGTERM stops HTTP, detaches observers, requests worker
shutdown and cancels inference. After ten seconds it reports pending shutdown but
retains ownership and keeps waiting for backend return. The journal closes only
through Worker.Shutdown after successful construction. There is no force-release
of ownership or automatic inference restart.

Submit example (save as `command.json`, UTF-8 without BOM):

```json
{"idempotency_key":"example-unique-001","origin":"my-client","request":{"model":"qwen2.5:0.5b","role":"Be concise.","system_prompt":"Use plain text.","prompt":"Hello\nworld","options":{"max_output_tokens":64,"context_tokens":2048,"temperature":0.2,"seed":42}}}
```

Retry: `{"idempotency_key":"unique-retry-001","parent_id":"32-hex-job-id"}`.
Branch includes idempotency_key, parent_id, optional origin and a complete request
of the same shape as submit. Optional fields must be omitted, not null. Role,
system prompt and prompt are strings in the file, so no shell reconstruction is
involved. JSON escapes support multiline text exactly. `max_output_tokens` is
required; unspecified options remain unknown, and requested/loaded/capacity
quantities remain distinct in results. Queue pause affects dispatch only. Retry
and branch create new attempts; no exact inference pause/rewind/checkpoint exists.

Create responses are full jobs, e.g. `{"id":"…","state":"running","request":…,
"instructions":…,"created_at":"…","result":{"text":"",…}}`. The schema is the
[core contract](contracts.md); terminal failure/cancellation retains partial text.

Keep the complete original command file and its key **before** sending. All
creates require a caller-chosen nonempty key. The CLI prints the key/operation on
stderr before the request; it never generates a replacement key or auto-retries.
A timeout/disconnect can leave acceptance uncertain. Resend the exact same file,
operation, origin and key, even after restart, to recover the same job. Changing
any command content with that key yields conflict. Do not use a fresh key to
resolve uncertain acceptance. Stdin (`--command -`) is supported only when the
caller also retains its original input for recovery.

## Bounds and failures

Commands: 8 MiB encoded UTF-8, depth at most 16, exact field spelling, no unknown
or duplicate fields, nulls, unpaired Unicode surrogate escapes or trailing values.
Exactly one Content-Type is required: application/json, optionally charset=utf-8.
Content-Encoding is unsupported and rejected. Core's tighter input,
output and storage limits still apply. All errors are sanitized core Fault JSON:
`{"code":"conflict","message":"conflict","retryable":false}`. Raw backend,
filesystem and transport diagnostics are not returned. Retry advice never means
automatically create a new inference.

| HTTP | Fault codes |
| --- | --- |
| 400 | invalid_request, cursor_invalid |
| 404 | not_found |
| 409 | conflict, cancelled, interrupted |
| 410 | cursor_expired |
| 413 | limit_exceeded |
| 422 | unsupported |
| 429 | queue_full, slow_consumer |
| 502 | backend_failure |
| 503 | backend_unavailable, storage_failure, unavailable |
| 500 | internal |

Policy/method/content-type errors use invalid_request with 403/405/415. Malformed
HTTP framing may be rejected by net/http before JSON handling. Headers are bounded
by net/http's 16 KiB setting (plus its parser allowance); header read timeout 5s,
body read timeout 15s, idle keepalive 60s. At most 32 concurrent handlers, including
streams; saturation returns unavailable. Non-stream core calls have a 30s deadline;
writes have a 10s deadline. Accepted work outlives those deadlines. CLI ordinary
calls default to a 30s total deadline; wait/watch default to none. `--timeout 0`
removes the total deadline; client header wait remains bounded to 35s. Responses
are bounded to 256 MiB in the client, consistent with finite retained journal
state. No automatic retry, unbounded observer queue or per-event goroutine exists.

## SSE and reconnect

Without a cursor, GET /v1/events obtains an atomic snapshot at C, registers
Watch(C), then sends `event: snapshot`, `id: STORE:SEQUENCE`, and one JSON `data:`
line followed by a blank line. Watch replays strictly after C before live events,
so writes between snapshot and registration cannot disappear. Existing clients
can instead GET /v1/snapshot and open events with that cursor.

Subsequent frames use `event: event`, the event cursor as id, and core Event JSON.
Example:

```text
id: 0123456789abcdef0123456789abcdef:42
event: event
data: {"cursor":{"store_id":"0123456789abcdef0123456789abcdef","sequence":"42"},"at":"2026-09-19T00:00:00Z","kind":"job.output","job_id":"…","output":{"offset_bytes":0,"text":"Hello"}}

```

The ID uses a 32 lowercase hex store ID and canonical unsigned uint64 decimal
(`0` valid; no signs/leading zeroes). JSON sequence stays a string. Use either
`after` or Last-Event-ID; if both are present they must agree exactly. Empty,
repeated, invalid, wrong-store and future cursors fail explicitly. Expired cursors
require an intentional new snapshot; never silently fall forward. No filters are
supported. Apply frames atomically, deduplicate by cursor, verify output's UTF-8
byte offset, and advance the reconnect cursor only after application. Reconnect
with the last applied cursor. A snapshot replaces the whole view. CLI watch emits
NDJSON frames `{kind,id,data}`; callers retain the last successfully applied id
and explicitly restart `watch --after ID` after a disconnection.

Every frame flushes. Comments heartbeat every 15s. There is no total SSE lifetime
limit; each write has a fresh 10s deadline. Individual events fit the existing
16 MiB commit limit; initial snapshot has the larger retained-state bound. Core
observer limits are 256 events/32 MiB. Overflow closes the observer without blocking
inference. Disconnect closes it as well. Errors before streaming are HTTP Faults;
after headers, `event: error` contains Fault JSON with no id and the connection
closes. If a write already failed, no error frame is guaranteed. EOF/network loss
is not success and does not advance a cursor or cancel any job.

## CLI

Flags precede positional arguments. Output is JSON, except help/version; watch is
NDJSON. Diagnostics and create-key receipts go to stderr. Examples:

```text
taskworker serve --server http://127.0.0.1:7433 --data-dir ./data --ollama http://127.0.0.1:11434
taskworker submit --command command.json --wait --timeout 2m
taskworker wait --timeout 2m JOB_ID
taskworker watch --after STORE_ID:SEQUENCE
taskworker list
taskworker get JOB_ID
taskworker result JOB_ID
taskworker cancel JOB_ID
taskworker retry --command retry.json
taskworker branch --command branch.json
taskworker models
taskworker queue status
taskworker queue pause
taskworker queue resume
taskworker mcp --server http://127.0.0.1:7433
```

Stopping wait/watch leaves inference running. Submit --wait performs one create
then observes that job; it never resubmits. Result returns the complete job so
partial output and terminal state cannot be confused. Exit codes: 0 successful
command/succeeded job (including accepted nonterminal jobs); 1 service/transport/
output error or wait timeout; 2 invalid local usage/input; 3 unused (formerly
"MCP unimplemented"; reserved, never emitted); 4 observed failed/interrupted job;
5 observed cancelled job. Cancel can return 0
with cancelling, then wait returns 5 after backend return. list remains 0 even
when it contains failed jobs. The [Python client](python.md) and
[`taskworker mcp` bridge](mcp.md) use these same routes.

## Embedded product assets

GET /, /app.css, /app.js and /state.js serve exact embedded assets under the same
Host/Origin policy, with no-store, nosniff and a restrictive same-origin CSP.
Other asset paths have no filesystem or SPA fallback; encoded paths are rejected.
See [browser controls and local recovery storage](ui.md).

