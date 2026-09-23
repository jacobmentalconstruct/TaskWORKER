# MCP stdio bridge

`taskworker mcp` lets an MCP client (an agent host) submit, inspect, observe and
control jobs in the **same running service** used by HTTP, the CLI, the browser UI
and the [Python client](python.md). It is a service client only: it never opens the
journal, constructs a backend, runs inference, or starts `serve`. If no service
answers, tool calls fail with a clear connection error and the bridge keeps
serving protocol traffic, so you can start the service afterwards.

```text
taskworker serve                       # once, in its own terminal/service
taskworker mcp [--server URL] [--timeout 30s]   # launched by the MCP host
```

Server resolution is identical to every other client: `--server`, then
`TASKWORKER_SERVER`, then `http://127.0.0.1:7433` (loopback HTTP only, no proxy, no
redirects). `--timeout` bounds each individual service call (default 30 s; 0 selects
the default). stdout carries MCP messages and nothing else; diagnostics are JSON
lines on stderr (no prompts or output text). Closing stdin ends the bridge with
exit code 0. Usage errors exit 2 before any protocol traffic.

## Launch configuration (generic)

Most hosts accept a command and arguments; nothing else is required. Use absolute
paths. Example `mcpServers`-style entry (adapt to your host's format):

```json
{
  "mcpServers": {
    "taskworker": {
      "command": "/absolute/path/to/taskworker",
      "args": ["mcp", "--server", "http://127.0.0.1:7433"]
    }
  }
}
```

Windows: `"command": "C:\\Tools\\taskworker.exe"`. To use a non-default service,
change `--server` or set `TASKWORKER_SERVER` in the host's environment block. This
project never edits your installed agent configuration.

## Protocol, SDK and compatibility

* Implementation: the official **MCP Go SDK v1.8.0**
  (`github.com/modelcontextprotocol/go-sdk`, Apache-2.0 for new code and MIT for
  earlier contributions), vendored under `vendor/` with its transitive modules
  (`google/jsonschema-go`, `segmentio/encoding` + `segmentio/asm`,
  `yosida95/uritemplate`, `golang.org/x/oauth2`, `x/sync`, `x/sys`, `x/time`; all
  permissive licenses, copies in `vendor/**/LICENSE`). Building needs no network: the
  vendor tree is the source of truth (`go build` uses it automatically). Refreshing it
  is a maintainer action (`go get …@version && go mod tidy && go mod vendor`).
  Compared with the earlier dependency-free build this adds about 2.2 MB to the
  stripped Windows amd64 executable (7,780,864 to about 10,000,000 bytes).
* Specification: the **2026-07-28** revision, current at the time of writing
  ([versioning](https://modelcontextprotocol.io/specification/versioning),
  [stdio transport](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/stdio)).
  That revision has no `initialize` handshake: every request carries its version and
  client capabilities in `_meta`, and `server/discover` reports supported versions.
* The bridge is **dual-era**: it also serves the legacy handshake revisions
  `2025-11-25`, `2025-06-18`, `2025-03-26` and `2024-11-05` (negotiated by
  `initialize`; `notifications/initialized` gates requests; `ping` works). An
  `initialize` from a client newer than any supported legacy version negotiates
  `2025-11-25`. A request declaring an unknown modern version receives
  `UnsupportedProtocolVersionError` (-32022) listing supported versions. `ping` was
  removed by the 2026-07-28 revision and answers "method not found" for modern
  requests, exactly as the SDK implements it.
* Advertised capabilities: **tools only** (no `listChanged`, prompts, resources,
  logging, completions, sampling, roots, elicitation or subscriptions). Tool results
  are `content` (JSON text) plus `structuredContent` (the same object); the bridge
  declares input schemas but no output schemas.
* Transport: newline-delimited JSON-RPC, one message per line, no embedded newlines.
  The SDK reads stdin as a stream of JSON values, so the bridge adds a small framing
  layer *in front of* it: each inbound line is bounded to 9 MiB and must be valid JSON
  and a JSON-RPC message (or a non-empty batch of them). A bad line is answered with a
  JSON-RPC error with `id: null` (`-32700` parse error, `-32600` invalid
  request/oversize) and the session continues; valid lines pass to the SDK unchanged.
  "Invalid request" covers valid JSON that is not a message the SDK can decode, which
  the SDK would otherwise treat as fatal (exit 1, no reply): `jsonrpc` missing or not
  `"2.0"`, a `method` that is not a string, neither `method` nor `id`, a response
  (no `method`) with a null `id`, an `id` that is an object, array or boolean, an empty
  batch or a batch with a bad member, and nesting deeper than 512 levels.
  **Ids** must be a string, null (requests only) or a plainly written integer within
  ±(2^53 − 1); anything else (`1.5`, `1e2`, `-0`, `9007199254740993`, …) is an invalid
  request instead of being answered with a different id, as the SDK would do. The layer also implements the
  cancellation rule: after `notifications/cancelled` for an in-flight request no
  response is sent for it.
* Errors: protocol failures (unknown method or tool, unsupported version, malformed
  frame) are JSON-RPC errors. Tool arguments that violate a tool's schema and every
  operation fault are **tool errors**: `isError: true` with a JSON body described
  below. An observed failed/cancelled/interrupted job is a **successful** call whose
  result shows that state.
* Known limits: use a small integer or a string as request ids (an id outside
  ±(2^53 − 1) is refused, not echoed inexactly); a request sent before the legacy
  `initialize` handshake is refused by the SDK with error code 0 rather than a
  registered JSON-RPC code; no batching guarantees beyond the SDK, no `tools/listChanged`, no
  progress notifications, no HTTP transport; the packaged binary's `mcp` startup
  is executed on Windows amd64, Linux amd64, Linux arm64 and macOS arm64 (Windows
  arm64 and macOS Intel are cross-compiled only).

## Tools

Every tool has a strict input schema (`additionalProperties: false`, patterns and
bounds). The bridge validates the *raw argument bytes* itself rather than through the
SDK's typed path, which round-trips numbers through floating point and would round an
integer above 2^53 (a `seed`, for example). Integers that are not integral or out of
range are rejected, never rounded.

| Tool | Purpose |
| --- | --- |
| `service_health` | Service reachable? Says nothing about the backend. |
| `list_models` | Installed models; fails `backend_unavailable` if Ollama is down while health is fine. |
| `submit_job` | One new job. Requires `idempotency_key` and a full `request`. |
| `retry_job` / `branch_job` | NEW jobs from a terminal parent (explicit lineage). |
| `get_job` | Full current job incl. retained (partial) output. |
| `wait_job` | Bounded observation (`timeout_seconds` 0..120, default 25; the budget also bounds a poll in flight). |
| `read_job_output` | Page output by UTF-8 byte offset. |
| `list_jobs` | Summaries plus the snapshot cursor. |
| `read_events` | Bounded event batch after a cursor. |
| `cancel_job` | The only tool that stops inference. |
| `queue_status`, `pause_queue`, `resume_queue` | Queue view and dispatch control. |

### Finding the job and continuing observation

`submit_job` returns `job.id` (also visible via `list_jobs`, the CLI and the UI).
Then either loop `wait_job` (while `timed_out` is true) and finish with `get_job`, or
page `read_job_output` until `end_of_output` is true. Use `read_events` for event
level tracking: start from the `cursor` object of `list_jobs`, formatted
`STORE_ID:SEQUENCE`, pass each `next_cursor` back unchanged, and deduplicate by
cursor. Events use the core JSON exactly (`sequence` stays a string).

### `wait_job` budget semantics

`timeout_seconds` is a total budget. Every poll of the service is limited to the
smaller of the remaining budget and the bridge's call limit (`--timeout`, default
30 s), so a slow or late reply cannot overrun the budget:

* Terminal job observed within the budget: `terminal:true, timed_out:false,
  observed:true` and the job view.
* Budget expired: `timed_out:true, terminal:false`. If a poll had completed within the
  budget, `observed:true` and `job` is the last state observed then. If none had
  (for example the very first reply was slower than the budget), `observed:false`,
  there is **no** `job` key (nothing is fabricated), `job_id` is echoed and a `note`
  says the state is unknown; call `wait_job` or `get_job` again.
* A reply that arrives after the budget is discarded, even a terminal one, and expiry
  never starts another poll. Nothing is cancelled or resubmitted.
* When the call limit is the shorter bound and it fires, that is an explicit tool
  error of `kind: "timeout"`, distinct from budget expiry.
* `timeout_seconds: 0` checks exactly once, bounded only by the call limit, and
  reports the observed state (`timed_out:true` if it is not terminal).

### Idempotent creates and recovery

`idempotency_key` is required (1 to 128 UTF-8 bytes) so the caller owns and can
persist it; results echo `operation`, `idempotency_key` and `origin` (default
`"mcp"`; part of the command content, so keep it identical when resending). Keep
the exact arguments you sent. If a create returns a timeout or connection error,
`acceptance_uncertain` is `true`: **call the same tool again with identical
arguments and the same key** (also after an agent or bridge restart) to get the
original job; a changed request under the same key is a `conflict`. Never invent a
new key to resolve uncertainty. `retry_job` and `branch_job` are deliberate new
attempts with their own keys. A fault's `retryable` flag is advice and never
permission to submit again.

### Result shape, bounds and omissions

Job results are `{"job": {...service job...}, "output_bytes": N, "omitted": [...]}`.
Unknown service fields and exact integers pass through untouched. The bridge never
truncates silently:

* A job view is bounded to 256 KiB. If output (then inputs) would exceed it, that part
  is removed and named in `omitted` with `reason: "exceeds_result_bound"` and how to
  fetch it (`read_job_output` for output; HTTP/CLI for inputs). `include_output` and
  `include_inputs` can also ask for less (`reason: "not_requested"`).
* `read_job_output` pages of 4..131072 bytes end on UTF-8 boundaries; an
  `offset_bytes` inside a character is an `invalid_request`.
* `read_events` returns at most 500 events (default 100) and 256 KiB per response,
  reporting `stopped_by` (`max_events`, `max_bytes`, `timeout`, `stream_ended`,
  `stream_error`). An event larger than 128 KiB appears as a marker
  `{"omitted": true, "reason": "event_exceeds_inline_bound", "bytes": N}` with its
  cursor, and the job is available from `get_job`.
* Inbound messages are limited to 9 MiB; the service applies its own 8 MiB command
  and per-job input/output limits.

### Faults

Tool errors carry `{"error": {"code", "message", "retryable", "kind"}}` where `code`
is the service fault code (`invalid_request`, `not_found`, `conflict`, `queue_full`,
`backend_unavailable`, `cursor_invalid`, `cursor_expired`, …) and `kind` is one of
`service_fault`, `service_unreachable`, `timeout`, `response_unreadable`
(a reply that arrived but cannot be read: undecodable, over the client bound, or not a job),
`bridge_validation`,
`bridge_busy`. Creates add `acceptance_uncertain` and a `recovery` sentence: it is `true` for
`service_unreachable`, `timeout`, `response_unreadable` and the fault codes
`unavailable`, `storage_failure` and `internal`, because the command was already
sent; read-only tools never carry it.
Cursor faults are returned as is and never reset.

## Lifetimes, cancellation and concurrency

* A request still in flight when stdin closes is abandoned (no response is
  written), so a client must read a reply before closing its end.
* Ending a `wait_job` or `read_events` call (timeout, MCP `notifications/cancelled`,
  client disconnect, stdin EOF, bridge exit) stops that observation only. Inference
  continues; only `cancel_job` cancels a job, and partial output is retained.
* At most 16 tool calls run at once, of which at most 4 may be blocking observations
  (`wait_job`, `read_events`). Excess calls fail immediately with
  `bridge_busy`; they are never queued, so control calls, `server/discover` and (in
  legacy sessions) `ping` stay responsive while waits are blocked.
* No background threads other than the SDK's read loop and one goroutine per active
  call; no retries; no queues; no automatic inference.

## Scope

No model-generated tool execution, model pause/checkpoints or backend bypass exist.
The Ollama compatibility gate is enforced by the service, unchanged.
