# Local Ollama backend

`internal/adapters/ollama.New(Config)` implements `core.Backend`. The executable
constructs it only in `serve`; see [service configuration](http.md). No model
download, server management, automatic retry, tool execution, or scheduler is
implemented in this adapter.

## Compatibility and local execution

Generation currently supports **Ollama 0.18.3 only**, with installed GGUF text
models using a template and no custom renderer, stored messages, vision, or image
capability. Other versions and paths return `unsupported` until their behavior
is audited. Discovery includes unsupported installed models with generation
capability false. Missing capacity metadata remains unknown. An embedding model
is never treated as a text generator. Capability flags describe this adapter's
supported path, rather than every feature the model might support elsewhere.

Ollama 0.18.3 omits the top-level `renderer` field from `/api/show`, so its absence
does not prove that the model uses the ordinary template path. The adapter also
checks the generated `modelfile`: it must contain one FROM and one TEMPLATE that
matches the separately reported template, and no RENDERER or MESSAGE directive.
If SYSTEM is present, its parsed value must match the separately reported `system`,
exactly once and in the audited serialization order. A nonempty reported system
must also appear in the Modelfile. Omission denotes an empty system in the pinned
GetModelInfo implementation. Parsing quotes alone cannot prove absence: the
serializer sometimes emits literal quote characters without wrapping them.
Every field before RENDERER is constrained: FROM and ADAPTER paths must be
unquoted single-line text with no quote/tab characters; TEMPLATE and optional
SYSTEM must match structured metadata before any parser/options/license fields.
Additional FROM, reordered/duplicate prefix commands and unresolved path quoting
are unsupported. Later fields cannot reopen that prefix. This order matters
because the audited serializer puts the actual renderer before those later fields.
Comments and double/triple-quoted multiline values are parsed as text, so
renderer-looking template/system/license/parameter content is not a directive.
Missing Modelfile, malformed/unknown commands, mismatched template, unterminated
quotes, embedded closing delimiters or trailing text after a quoted value are
unsupported. This deliberately narrower generated-syntax subset can reject
unusual otherwise valid models rather than infer renderer absence ambiguously.
The check applies to discovery, selection and the post-load metadata refresh.

The endpoint defaults to `http://127.0.0.1:11434`. Configuration accepts HTTP on
a literal loopback address; `localhost` is pinned to `127.0.0.1`. Remote addresses,
credentials, path prefixes, queries, HTTPS, and redirects are rejected. Proxy
environment settings and injected transport proxy/dial hooks are disabled. A
supplied `*http.Client` is copied; standard `*http.Transport` configuration and
client timeout are usable for tests. Arbitrary RoundTrippers are unsupported.
Injected clients and the local daemon are trusted application dependencies.

Every operation checks `/api/version`. Selection refreshes `/api/tags` and
`/api/show`, requires an exact installed ID, and rejects remote metadata and cloud
source suffixes. Requests append Ollama's `:local` routing selector, including
metadata and load requests. This also prevents cloud routing if an installed
manifest changes between validation and generation. There is no fallback or
pull request. The local daemon must itself be trustworthy; this is not an OS or
network sandbox around a malicious daemon.

## Input, options, and budgets

Only materialized `Instructions` become messages: an explicit system message
(including empty), explicit user/assistant history, and the exact user prompt.
The explicit empty system overrides a Modelfile system default. Models with
stored messages are rejected because Ollama would otherwise prepend them.
Each call constructs a new message array and sends no conversation/token handle.
Core supplies retry/branch history; model residency can still be shared.

Options map to `num_ctx`, required positive `num_predict`, optional `temperature`,
and optional `seed`. Unspecified values are omitted. Seeds are limited to
0..4294967295, avoiding the runner's narrowing/wrapping and random-seed sentinel;
temperature must be finite, nonnegative and fit float32. Core's generic request
limits also apply. Ollama may use float32 rounding; resolved options are not
reported by this API, so `effective_options` remains absent.

The adapter keeps these quantities separate:

| Quantity | Source and meaning |
| --- | --- |
| Advertised capacity | `/api/show model_info.<architecture>.context_length`; source and UTC read time accompany it |
| Requested allocation | `Request.Options.ContextTokens`, or unknown when omitted |
| Loaded allocation | Matching model ID **and digest** in `/api/ps`; read time and source; shared residency, not a per-request guarantee |
| Estimated input | `ceil((UTF-8 system/history/prompt bytes + template source bytes)/4) + 16 per message` |
| Reserved output | Requested `MaxOutputTokens` |
| Reported usage | Terminal engine counts/durations; absent fields remain unknown |

The input estimate is a heuristic, **not an exact token count or an upper bound**.
Template source bytes approximate formatting overhead; they do not reproduce
rendering or tokenization. Reject estimated input plus reserved output exceeding
known capacity, requested allocation, or observed allocation, and reject an
explicit context above known capacity. This may reject inputs that actually fit
or underestimate others. It does not promise the entire output reservation will
fit. If both requested and observed allocation are unknown, generation is
unsupported. Unknown advertised capacity alone does not prevent generation.

An empty-message `/api/chat` load request uses the same options. After load, the
adapter rechecks installed identity/digest and metadata, observes `/api/ps`, and
revalidates budgets. It observes `/api/ps` again after successful completion;
missing or unmatched residency clears the observation rather than retaining a
stale one. Observation request failure fails the operation; committed output is
still retained by core. No unload or keep-alive override is sent. Concurrent
external daemon users can change residency; neither observations nor a digest
check provide an atomic model snapshot against external manifest modifications.

Every chat request explicitly sends `truncate:false` and `shift:false`. In the
audited version, the server preserves all supplied messages, and both GGUF
runners reject tokenized input beyond their actual allocation. The latter option
prevents context shifting during generation: exhaustion can end with `length`.
Thus heuristic underestimation cannot silently remove input to make it fit.
The exact runner oversized-input error maps to `invalid_request`. Unsupported
versions/model paths are rejected rather than assuming these controls work.

## Streaming and cancellation

Callbacks are synchronous and serial on the generating goroutine. Only
`message.content` is emitted; thinking/tool fields are discarded, count toward
wire limits, and are never executed or mixed into the answer. `think:false` is
requested, but mandatory thinking models may still return separate thinking.
Reported output usage remains the engine's count, not a count of visible text.
Core owns accumulated output; returned `Result.Text` is always empty.

Limits: 4 MiB per metadata response, 1,024 installed models, 512 bytes per model
ID, 256 KiB per stream record, 32 MiB total stream wire bytes, 8 MiB answer bytes,
64 levels of JSON nesting, and 32 KiB final result metadata. Metadata and records
reject duplicate JSON keys and invalid UTF-8. Valid completion requires a matching
model, assistant message, `done:true`, supported reason (`stop` or `length`),
nonnegative reported metrics, and clean response EOF. Missing terminal records,
bad JSON, server error records, extra records after completion, response overflow,
or a broken transport after the terminal record fail the job.

The default operation timeout is five minutes, configurable through `Config`;
it includes discovery, loading, streaming, and observations. Earlier caller or
HTTP client deadlines also apply. Cancellation, callback failure, timeout, and
protocol failure close the response and cancel operation resources. No adapter
callback goroutine exists, and no callback occurs after Generate returns.
Callbacks must return cooperatively: an in-progress durable core callback cannot
be preempted without violating core's ownership contract.

Availability/transport timeouts and HTTP 429/502/503/504 are retryable
`backend_unavailable`. HTTP 400/404/422 and the known oversized-input stream error
are `invalid_request`; version/model/option restrictions are `unsupported`;
bounds are `limit_exceeded`; other statuses/protocol failures are non-retryable
`backend_failure`. Fault messages are fixed and contain no raw daemon body, URL,
prompt or transport diagnostic. Callback errors are returned to their owner;
core applies its accepted fault sanitation and lifecycle precedence.

Transport cancellation is observed locally; it is not a server stop
acknowledgement. Audited source propagates request cancellation to the runner,
which signals sequence termination. The public API exposes no per-sequence stop
timestamp or token counter after disconnection. A subsequent successful request
shows continued availability, not an exact proof of when server computation ended.

## Verification

Ordinary `go test ./...` uses only local deterministic HTTP test servers and
temporary journals; a running Ollama instance is unnecessary. The real-model
test skips unless explicitly enabled. It uses existing models only, changes no
machine-wide setting, and never unloads or stops unrelated work.

From the repository root in PowerShell:

```powershell
$env:GOTOOLCHAIN = 'local'
$env:GOCACHE = "$PWD/.cache/go-build"
$env:GOPROXY = 'off'
$env:TASKWORKER_OLLAMA_REAL = '1'
$env:TASKWORKER_OLLAMA_MODEL = 'qwen2.5:0.5b'
$env:TASKWORKER_OLLAMA_ENDPOINT = 'http://127.0.0.1:11434'
$env:TASKWORKER_OLLAMA_EVIDENCE = "$PWD/.cache/ollama-real-evidence.json"
./.tools/go/bin/go.exe test ./internal/adapters/ollama -run '^TestRealInstalledModel$' -v -count=1 -timeout=210s
Remove-Item Env:TASKWORKER_OLLAMA_REAL
Remove-Item Env:TASKWORKER_OLLAMA_EVIDENCE
```

This check discovers models, submits two isolated jobs with differing allocations,
tests the server's oversized-input guard, cancels after durable partial output,
runs a subsequent job, and reopens the journal. It requires a supported installed
model and enough resources to load it. The model's prose is nondeterministic;
success checks concern contracts and state rather than exact wording.

## Sources checked

Current API documentation: [chat](https://docs.ollama.com/api/chat),
[streaming](https://docs.ollama.com/api/streaming),
[loaded models](https://docs.ollama.com/api/ps),
[model details](https://docs.ollama.com/api-reference/show-model-details).
The current chat reference does not fully specify the audited controls, so the
installed-version implementation is authoritative for this adapter's allowlist:

- [v0.18.3 request/response and option types](https://github.com/ollama/ollama/blob/v0.18.3/api/types.go)
- [local/cloud model reference parsing](https://github.com/ollama/ollama/blob/v0.18.3/internal/modelref/modelref.go)
- [routing, stored messages, loading and request lifetime](https://github.com/ollama/ollama/blob/v0.18.3/server/routes.go)
- [generated Modelfile renderer directives](https://github.com/ollama/ollama/blob/v0.18.3/server/images.go)
- [Modelfile command serialization and quote syntax](https://github.com/ollama/ollama/blob/v0.18.3/parser/parser.go)
- [template rendering and truncation switch](https://github.com/ollama/ollama/blob/v0.18.3/server/prompt.go)
- [runner HTTP request and cancellation propagation](https://github.com/ollama/ollama/blob/v0.18.3/llm/server.go)
- [llama runner input check and sequence cancellation](https://github.com/ollama/ollama/blob/v0.18.3/runner/llamarunner/runner.go)
- [Ollama runner input check and sequence cancellation](https://github.com/ollama/ollama/blob/v0.18.3/runner/ollamarunner/runner.go)
