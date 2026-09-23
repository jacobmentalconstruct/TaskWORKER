package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"taskworker.local/taskworker/internal/adapters/httpapi"
	"taskworker.local/taskworker/internal/core"
)

// localErr is a bridge-side validation failure whose message is safe to show.
type localErr struct{ f *core.Fault }

func (e *localErr) Error() string { return e.f.Error() }
func local(code core.ErrorCode, msg string) error {
	return &localErr{&core.Fault{Code: code, Message: msg}}
}

// timeoutErr marks the bridge's own per-call deadline expiring.
type timeoutErr struct{ error }

type slotKind int

const (
	plain slotKind = iota
	blocking
)

func (b *bridge) acquire(k slotKind) (func(), bool) {
	select {
	case b.slots <- struct{}{}:
	default:
		return nil, false
	}
	if k == blocking {
		select {
		case b.waiters <- struct{}{}:
			return func() { <-b.waiters; <-b.slots }, true
		default:
			<-b.slots
			return nil, false
		}
	}
	return func() { <-b.slots }, true
}

func (b *bridge) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	return b.doFor(ctx, b.timeout, method, path, body)
}

// doFor is do with an explicit limit for this one call. Exceeding it (while the
// caller's own context is still live) is reported as timeoutErr.
func (b *bridge) doFor(ctx context.Context, limit time.Duration, method, path string, body any) (json.RawMessage, error) {
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var raw json.RawMessage
	err := b.client.Call(cctx, method, path, body, &raw)
	if err != nil && ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return nil, timeoutErr{err}
	}
	return raw, err
}

func (b *bridge) doObject(ctx context.Context, method, path string, body any) (map[string]any, error) {
	return b.doObjectFor(ctx, b.timeout, method, path, body)
}

func (b *bridge) doObjectFor(ctx context.Context, limit time.Duration, method, path string, body any) (map[string]any, error) {
	raw, err := b.doFor(ctx, limit, method, path, body)
	if err != nil {
		return nil, err
	}
	return decodeObject(raw)
}

// doJob is doObject for routes that answer with a job. A reply without a job ID
// and state cannot establish an outcome (for a create the command was already
// sent), so it is reported as an unreadable response, never as a job.
func (b *bridge) doJob(ctx context.Context, method, path string, body any) (map[string]any, error) {
	return b.doJobFor(ctx, b.timeout, method, path, body)
}

func (b *bridge) doJobFor(ctx context.Context, limit time.Duration, method, path string, body any) (map[string]any, error) {
	job, err := b.doObjectFor(ctx, limit, method, path, body)
	if err != nil {
		return nil, err
	}
	id, _ := job["id"].(string)
	state, _ := job["state"].(string)
	if len(id) != 32 || state == "" {
		return nil, &core.Fault{Code: core.ErrInternal, Message: httpapi.MessageInvalidResponse}
	}
	return job, nil
}

func (b *bridge) faults(err error, creates bool) map[string]any {
	var le *localErr
	if errors.As(err, &le) {
		return map[string]any{"error": map[string]any{"code": le.f.Code, "message": le.f.Message, "retryable": false, "kind": "bridge_validation"}}
	}
	return b.faultBody(err, creates)
}

func textJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":{"code":"internal","message":"result not encodable"}}`
	}
	return string(b)
}

func result(v map[string]any, isErr bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: isErr, Content: []mcp.Content{&mcp.TextContent{Text: textJSON(v)}}, StructuredContent: v}
}

// compile turns a hand-written schema into a validator. The SDK's typed AddTool
// is deliberately not used: it round-trips arguments through float64 maps, which
// silently rounds integers above 2^53 (for example a seed). Here the raw
// argument bytes are validated as a document and then decoded once, exactly.
func compile(schema map[string]any) *jsonschema.Resolved {
	raw, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	var sch jsonschema.Schema
	if err := json.Unmarshal(raw, &sch); err != nil {
		panic(err)
	}
	r, err := sch.Resolve(nil)
	if err != nil {
		panic(err)
	}
	return r
}

func decodeArgs(raw json.RawMessage, schema *jsonschema.Resolved, dst any) error {
	doc := map[string]any{}
	// Omitted or null arguments mean "no arguments" (the spec's optional field).
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
			return local(core.ErrInvalidRequest, "arguments must be a JSON object")
		}
		raw = trimmed
	} else {
		raw = nil
	}
	if err := schema.Validate(doc); err != nil {
		return local(core.ErrInvalidRequest, "arguments do not match the tool schema: "+err.Error())
	}
	if len(raw) > 0 {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(dst); err != nil {
			return local(core.ErrInvalidRequest, "arguments not representable exactly (integers must be integral and in range): "+err.Error())
		}
	}
	return nil
}

func register[In any](b *bridge, s *mcp.Server, name, title, desc string, schema map[string]any, ann *mcp.ToolAnnotations, k slotKind, creates bool, h func(context.Context, In) (map[string]any, error)) {
	resolved := compile(schema)
	s.AddTool(&mcp.Tool{Name: name, Title: title, Description: desc, InputSchema: schema, Annotations: ann},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in In
			if err := decodeArgs(req.Params.Arguments, resolved, &in); err != nil {
				return result(b.faults(err, false), true), nil
			}
			release, ok := b.acquire(k)
			if !ok {
				return result(map[string]any{"error": map[string]any{"code": core.ErrUnavailable, "message": "too many concurrent tool calls; retry this call later", "retryable": true, "kind": "bridge_busy"}}, true), nil
			}
			defer release()
			out, err := h(ctx, in)
			if err != nil {
				body := b.faults(err, creates)
				if f, _ := body["error"].(map[string]any); f != nil {
					b.logf(map[string]any{"level": "warn", "tool": name, "code": f["code"], "kind": f["kind"]})
				}
				return result(body, true), nil
			}
			return result(out, false), nil
		})
}

func ptr[T any](v T) *T { return &v }

var (
	readOnly    = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
	control     = &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)}
	createNote  = &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)}
	cancelNote  = &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)}
	jobIDSchema = map[string]any{"type": "string", "pattern": "^[0-9a-f]{32}$", "description": "32 lowercase hexadecimal job ID from a create result or list_jobs."}
	keySchema   = map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Caller-chosen unique idempotency key (at most 128 UTF-8 bytes) for this create. Keep it with the exact arguments. Resending identical arguments with the same key returns the original job; the same key with different content is a conflict."}
	originSch   = map[string]any{"type": "string", "minLength": 1, "maxLength": 1024, "description": "Display label for who created the job (not authentication). Defaults to \"mcp\". It is part of the command content, so keep it identical when resending."}
)

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func requestSchema(what string) map[string]any {
	return obj(map[string]any{
		"model":         map[string]any{"type": "string", "minLength": 1, "description": "Exact installed model ID from list_models. Models are never downloaded."},
		"role":          map[string]any{"type": "string", "description": "Optional persona/instruction text placed first in the system message. Not a chat role."},
		"system_prompt": map[string]any{"type": "string", "description": "Optional system instructions, placed after role."},
		"prompt":        map[string]any{"type": "string", "minLength": 1, "description": "The user prompt, sent verbatim."},
		"options": obj(map[string]any{
			"max_output_tokens": map[string]any{"type": "integer", "minimum": 1, "description": "Required output token budget."},
			"context_tokens":    map[string]any{"type": "integer", "minimum": 1, "description": "Requested context window; omit to leave unspecified (backend default stays unknown)."},
			"temperature":       map[string]any{"type": "number", "minimum": 0, "description": "Sampling temperature; omit to leave unspecified."},
			"seed":              map[string]any{"type": "integer", "description": "Sampling seed; omit to leave unspecified. Integer, never rounded."},
		}, "max_output_tokens"),
	}, "model", "prompt", "options")
}

type createIn struct {
	Key      string        `json:"idempotency_key"`
	Origin   string        `json:"origin"`
	ParentID string        `json:"parent_id"`
	Request  *core.Request `json:"request"`
}

type jobIn struct {
	JobID         string `json:"job_id"`
	IncludeOutput *bool  `json:"include_output"`
	IncludeInputs *bool  `json:"include_inputs"`
}

func flag(p *bool) bool { return p == nil || *p }

func (b *bridge) create(op string) func(context.Context, createIn) (map[string]any, error) {
	return func(ctx context.Context, in createIn) (map[string]any, error) {
		if in.Key == "" || len(in.Key) > 128 {
			return nil, local(core.ErrInvalidRequest, "idempotency_key must be 1..128 UTF-8 bytes")
		}
		if in.Origin == "" {
			in.Origin = "mcp"
		}
		var body any
		switch op {
		case "submit":
			if in.Request == nil {
				return nil, local(core.ErrInvalidRequest, "request is required")
			}
			body = core.SubmitCommand{Key: in.Key, Origin: in.Origin, Request: *in.Request}
		case "retry":
			body = core.RetryCommand{Key: in.Key, Origin: in.Origin, ParentID: core.JobID(in.ParentID)}
		case "branch":
			if in.Request == nil {
				return nil, local(core.ErrInvalidRequest, "request is required")
			}
			body = core.BranchCommand{Key: in.Key, Origin: in.Origin, ParentID: core.JobID(in.ParentID), Request: *in.Request}
		}
		job, err := b.doJob(ctx, "POST", "/v1/"+op, body)
		if err != nil {
			return nil, err
		}
		out := jobView(job, true, true)
		out["operation"] = op
		out["idempotency_key"] = in.Key
		out["origin"] = in.Origin
		return out, nil
	}
}

func (b *bridge) getJob(ctx context.Context, id string) (map[string]any, error) {
	return b.doJob(ctx, "GET", "/v1/jobs/"+url.PathEscape(id), nil)
}

func (b *bridge) register(s *mcp.Server) {
	const noParams = "This tool takes no arguments."
	register(b, s, "service_health", "Service health", "Check that the TaskWorker HTTP service is reachable. Health is only 'the service is serving'; it says nothing about the inference backend (use list_models for backend availability). "+noParams,
		obj(map[string]any{}), readOnly, plain, false, func(ctx context.Context, _ struct{}) (map[string]any, error) {
			h, err := b.doObject(ctx, "GET", "/v1/health", nil)
			if err != nil {
				return nil, err
			}
			h["service"] = b.server
			return h, nil
		})
	register(b, s, "list_models", "List models", "List installed models with capabilities and context information. This queries the inference backend, so it fails with backend_unavailable when Ollama is down even if service_health succeeds. "+noParams,
		obj(map[string]any{}), readOnly, plain, false, func(ctx context.Context, _ struct{}) (map[string]any, error) {
			raw, err := b.do(ctx, "GET", "/v1/models", nil)
			if err != nil {
				return nil, err
			}
			var models []json.RawMessage
			if json.Unmarshal(raw, &models) != nil {
				return nil, &core.Fault{Code: core.ErrInternal, Message: httpapi.MessageInvalidResponse}
			}
			return map[string]any{"models": models}, nil
		})
	register(b, s, "submit_job", "Submit a job", "Create ONE new inference job in the shared service and return it (state queued or running) with job.id. A fresh job has empty history and sees no other job's output. You must supply idempotency_key and keep these exact arguments: if the call times out or the connection drops, acceptance is uncertain and you must call submit_job again with identical arguments and the same key to get the original job. Never create a replacement key to resolve uncertainty; a fault's retryable flag never authorizes another submission. Errors: queue_full, conflict (key reused with different content), backend faults appear later on the job.",
		obj(map[string]any{"idempotency_key": keySchema, "origin": originSch, "request": requestSchema("submit")}, "idempotency_key", "request"),
		createNote, plain, true, b.create("submit"))
	register(b, s, "retry_job", "Retry a terminal job", "Create a NEW job that repeats a terminal parent's request (retry lineage). This is an explicit new inference attempt, not recovery of an uncertain submission: to recover an uncertain create, resend the ORIGINAL create call with its original key. Requires a terminal parent and a fresh idempotency_key for this retry.",
		obj(map[string]any{"idempotency_key": keySchema, "origin": originSch, "parent_id": jobIDSchema}, "idempotency_key", "parent_id"),
		createNote, plain, true, b.create("retry"))
	register(b, s, "branch_job", "Branch from a terminal job", "Create a NEW job whose history is the terminal parent's prompt plus its retained assistant output (including partial output of a failed, cancelled or interrupted parent), followed by your complete new request. The parent is immutable. Use it to continue: identical generation is not promised. Requires a fresh idempotency_key.",
		obj(map[string]any{"idempotency_key": keySchema, "origin": originSch, "parent_id": jobIDSchema, "request": requestSchema("branch")}, "idempotency_key", "parent_id", "request"),
		createNote, plain, true, b.create("branch"))
	jobProps := map[string]any{
		"job_id":         jobIDSchema,
		"include_output": map[string]any{"type": "boolean", "description": "Include result.text (default true). Output too large for one result is omitted explicitly; page it with read_job_output."},
		"include_inputs": map[string]any{"type": "boolean", "description": "Include request text and instructions (default true)."},
	}
	register(b, s, "get_job", "Get a job", "Return the complete current job: state, request, instructions, lineage, retained output (partial while running, cancelling or after failure/cancellation), error and metrics. Terminal states are succeeded, failed, cancelled, interrupted; observing them is a successful call. Parts that exceed the result bound are listed in omitted; read output with read_job_output.",
		obj(jobProps, "job_id"), readOnly, plain, false, func(ctx context.Context, in jobIn) (map[string]any, error) {
			job, err := b.getJob(ctx, in.JobID)
			if err != nil {
				return nil, err
			}
			out := jobView(job, flag(in.IncludeOutput), flag(in.IncludeInputs))
			out["terminal"] = core.Terminal(stateOf(job))
			return out, nil
		})
	waitProps := map[string]any{}
	for k, v := range jobProps {
		waitProps[k] = v
	}
	waitProps["timeout_seconds"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 120, "description": "Maximum time to wait (default 25; 0 checks once, bounded only by the call limit). The budget also bounds a poll in flight. A timeout is not a failure."}
	register(b, s, "wait_job", "Wait for a job", "Observe a job until it is terminal or timeout_seconds elapses, then return the last view observed within the budget. timed_out=true means the job was still non-terminal; observed=false (with no job) means no poll completed within the budget and the state is unknown; call again to keep waiting. A reply that arrives after the budget is discarded, never reported as an in-budget observation. Waiting, timing out, cancelling this call or disconnecting never cancels the job; use cancel_job for that.",
		obj(waitProps, "job_id"), readOnly, blocking, false, func(ctx context.Context, in struct {
			jobIn
			TimeoutSeconds *int `json:"timeout_seconds"`
		}) (map[string]any, error) {
			limit := 25 * time.Second
			if in.TimeoutSeconds != nil {
				limit = time.Duration(*in.TimeoutSeconds) * time.Second
			}
			start := time.Now()
			includeOut, includeIn := flag(in.IncludeOutput), flag(in.IncludeInputs)
			// answer builds the result. job == nil means no poll completed within the
			// budget: the job's state is unknown and no job is fabricated.
			answer := func(job map[string]any, terminal bool) map[string]any {
				var out map[string]any
				if job != nil {
					out = jobView(job, includeOut, includeIn)
				} else {
					out = map[string]any{"job_id": in.JobID, "note": "no poll completed within the budget; the job state is unknown; call wait_job or get_job again"}
				}
				out["observed"] = job != nil
				out["terminal"] = terminal
				out["timed_out"] = !terminal
				out["waited_ms"] = time.Since(start).Milliseconds()
				return out
			}
			if limit == 0 {
				// "0 checks once": one observation, bounded only by the call limit.
				job, err := b.getJob(ctx, in.JobID)
				if err != nil {
					return nil, err
				}
				return answer(job, core.Terminal(stateOf(job))), nil
			}
			deadline := start.Add(limit)
			var last map[string]any // last job observed WITHIN the budget
			for {
				left := time.Until(deadline)
				if left <= 0 {
					return answer(last, false), nil // expiry never starts another poll
				}
				// Every poll is bounded by the smaller of the remaining budget and the
				// bridge's own call limit; which one applied decides how it ends.
				pollLimit, budgetBound := b.timeout, false
				if left <= b.timeout {
					pollLimit, budgetBound = left, true
				}
				job, err := b.doJobFor(ctx, pollLimit, "GET", "/v1/jobs/"+url.PathEscape(in.JobID), nil)
				if err != nil {
					var te timeoutErr
					if budgetBound && errors.As(err, &te) {
						return answer(last, false), nil // the budget ended the poll: a timeout result, not a failure
					}
					return nil, err // call-limit timeouts and every other failure keep their own kind
				}
				if !time.Now().Before(deadline) {
					return answer(last, false), nil // a late reply is not an in-budget observation
				}
				last = job
				if terminal := core.Terminal(stateOf(job)); terminal {
					return answer(job, true), nil
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(min(pollInterval, time.Until(deadline))):
				}
			}
		})
	register(b, s, "read_job_output", "Read job output", "Page a job's retained output by UTF-8 byte offset. Returns text starting at offset_bytes (a character boundary) with next_offset_bytes to continue from, total_bytes so far, state and end_of_output (true only when the job is terminal and next_offset_bytes equals total_bytes). Output of a running job can still grow. Text is never truncated silently: continue at next_offset_bytes.",
		obj(map[string]any{
			"job_id":       jobIDSchema,
			"offset_bytes": map[string]any{"type": "integer", "minimum": 0, "description": "Start offset in UTF-8 bytes (default 0); use the previous next_offset_bytes."},
			"max_bytes":    map[string]any{"type": "integer", "minimum": 4, "maximum": 131072, "description": "Page size in UTF-8 bytes (default 32768). The page ends on a character boundary."},
		}, "job_id"), readOnly, plain, false, func(ctx context.Context, in struct {
			JobID  string `json:"job_id"`
			Offset *int   `json:"offset_bytes"`
			Max    *int   `json:"max_bytes"`
		}) (map[string]any, error) {
			off, max := 0, 32768
			if in.Offset != nil {
				off = *in.Offset
			}
			if in.Max != nil {
				max = *in.Max
			}
			job, err := b.getJob(ctx, in.JobID)
			if err != nil {
				return nil, err
			}
			text := outputOf(job)
			page, next, err := pageOutput(text, off, max)
			if err != nil {
				return nil, err
			}
			st := stateOf(job)
			return map[string]any{"job_id": in.JobID, "state": st, "text": page, "offset_bytes": off, "next_offset_bytes": next, "total_bytes": len(text), "end_of_output": core.Terminal(st) && next == len(text)}, nil
		})
	register(b, s, "list_jobs", "List jobs", "List job summaries (id, state, origin, model, lineage, timestamps, output_bytes, error_code) from an atomic service snapshot, with the snapshot cursor to start read_events from. Summaries never include prompts or output; use get_job or read_job_output. Order is by the service's durable acceptance order.",
		obj(map[string]any{
			"limit":        map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Page size (default 50)."},
			"offset":       map[string]any{"type": "integer", "minimum": 0, "description": "Number of jobs to skip in the chosen order (default 0)."},
			"newest_first": map[string]any{"type": "boolean", "description": "Newest accepted first (default true)."},
		}), readOnly, plain, false, func(ctx context.Context, in struct {
			Limit       *int  `json:"limit"`
			Offset      *int  `json:"offset"`
			NewestFirst *bool `json:"newest_first"`
		}) (map[string]any, error) {
			limit, offset := 50, 0
			if in.Limit != nil {
				limit = *in.Limit
			}
			if in.Offset != nil {
				offset = *in.Offset
			}
			snap, err := b.doObject(ctx, "GET", "/v1/snapshot", nil)
			if err != nil {
				return nil, err
			}
			all, _ := snap["jobs"].([]any)
			if all == nil {
				all = []any{}
			}
			newest := flag(in.NewestFirst)
			sums := []any{}
			for i := offset; i < len(all) && len(sums) < limit; i++ {
				j, _ := all[i].(map[string]any)
				if newest {
					j, _ = all[len(all)-1-i].(map[string]any)
				}
				if j != nil {
					sums = append(sums, summary(j))
				}
			}
			return map[string]any{"cursor": snap["cursor"], "total": len(all), "offset": offset, "limit": limit, "newest_first": newest, "queue": snap["queue"], "jobs": sums}, nil
		})
	register(b, s, "cancel_job", "Cancel a job", "Explicitly request cancellation of a queued or running job. This is the ONLY tool that stops inference. A running job first becomes cancelling and reaches cancelled after the backend returns, retaining partial output; poll with wait_job. Repeating is safe and returns the current job.",
		obj(map[string]any{"job_id": jobIDSchema}, "job_id"), cancelNote, plain, false, func(ctx context.Context, in jobIn) (map[string]any, error) {
			job, err := b.doJob(ctx, "POST", "/v1/jobs/"+url.PathEscape(in.JobID)+"/cancel", struct{}{})
			if err != nil {
				return nil, err
			}
			out := jobView(job, true, false)
			out["terminal"] = core.Terminal(stateOf(job))
			return out, nil
		})
	register(b, s, "queue_status", "Queue status", "Return the queue: paused flag, pending job IDs in FIFO order, the active job ID and the pending bound. "+noParams,
		obj(map[string]any{}), readOnly, plain, false, func(ctx context.Context, _ struct{}) (map[string]any, error) {
			return b.doObject(ctx, "GET", "/v1/queue", nil)
		})
	for _, verb := range []string{"pause", "resume"} {
		desc := "Pause dispatch of pending jobs. The active job keeps running; submissions are still accepted within the queue bound. Repeating is a no-op. " + noParams
		if verb == "resume" {
			desc = "Resume normal dispatch of pending jobs. Repeating is a no-op. " + noParams
		}
		register(b, s, verb+"_queue", verb+" queue", desc, obj(map[string]any{}), control, plain, false, func(ctx context.Context, _ struct{}) (map[string]any, error) {
			return b.doObject(ctx, "POST", "/v1/queue/"+verb, struct{}{})
		})
	}
	register(b, s, "read_events", "Read events", "Read committed service events strictly after a cursor, for at most timeout_seconds or max_events. Get a starting cursor from list_jobs. Continue with next_cursor exactly as returned (it is an opaque STORE:SEQUENCE string; do not alter it). Events are delivered at least once: deduplicate by cursor and check output offsets when applying. Invalid, expired or wrong-store cursors return an explicit fault and are never silently reset; take a new list_jobs snapshot to resynchronize. An event larger than the inline bound is replaced by a marker with omitted=true (fetch the job with get_job). The bridge stopping or this call ending never cancels inference. Ending without events is not job completion.",
		obj(map[string]any{
			"after":           map[string]any{"type": "string", "pattern": "^[0-9a-f]{32}:(0|[1-9][0-9]*)$", "description": "Last applied cursor (STORE:SEQUENCE)."},
			"job_id":          jobIDSchema,
			"max_events":      map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "description": "Maximum events to return (default 100)."},
			"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 60, "description": "Maximum time to watch (default 10)."},
		}, "after"), readOnly, blocking, false, b.readEvents)
}

type eventsIn struct {
	After          string `json:"after"`
	JobID          string `json:"job_id"`
	MaxEvents      *int   `json:"max_events"`
	TimeoutSeconds *int   `json:"timeout_seconds"`
}

var errStop = errors.New("stop")

func (b *bridge) readEvents(ctx context.Context, in eventsIn) (map[string]any, error) {
	maxEvents, timeout := 100, 10*time.Second
	if in.MaxEvents != nil {
		maxEvents = *in.MaxEvents
	}
	if in.TimeoutSeconds != nil {
		timeout = time.Duration(*in.TimeoutSeconds) * time.Second
	}
	if _, err := httpapi.ParseCursor(in.After); err != nil {
		return nil, local(core.ErrCursorInvalid, "after must be a canonical STORE:SEQUENCE cursor")
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	events := []json.RawMessage{}
	next, size, stopped := in.After, 0, "timeout"
	err := b.client.Watch(wctx, in.After, func(f httpapi.Frame) error {
		var head struct {
			Kind  string `json:"kind"`
			JobID string `json:"job_id"`
		}
		if json.Unmarshal(f.Data, &head) != nil {
			return errors.New("bad frame")
		}
		if f.Kind != "event" {
			return errors.New("unexpected frame")
		}
		if in.JobID != "" && head.JobID != in.JobID {
			next = f.ID // scanned and applied: cursor advances over filtered events too
			return nil
		}
		item := f.Data
		if len(item) > MaxInlineEvent {
			item = json.RawMessage(`{"cursor":` + textJSON(cursorOf(f.Data)) + `,"kind":` + strconv.Quote(head.Kind) + `,"job_id":` + strconv.Quote(head.JobID) + `,"omitted":true,"reason":"event_exceeds_inline_bound","bytes":` + strconv.Itoa(len(f.Data)) + `,"retrieve_via":"get_job"}`)
		}
		if size+len(item) > MaxResultBytes && len(events) > 0 {
			stopped = "max_bytes"
			return errStop // not applied: next_cursor stays before this event
		}
		events = append(events, item)
		size += len(item)
		next = f.ID
		if len(events) >= maxEvents {
			stopped = "max_events"
			return errStop
		}
		return nil
	})
	switch {
	case err == nil:
		stopped = "stream_ended"
	case errors.Is(err, errStop):
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case wctx.Err() != nil:
		// Our own deadline ended the watch: normal, bounded observation.
	case len(events) == 0 && next == in.After:
		return nil, err
	default:
		e := b.faultBody(err, false)["error"]
		return map[string]any{"events": events, "count": len(events), "next_cursor": next, "stopped_by": "stream_error", "stream_error": e}, nil
	}
	return map[string]any{"events": events, "count": len(events), "next_cursor": next, "stopped_by": stopped, "timed_out": stopped == "timeout"}, nil
}

func cursorOf(data []byte) json.RawMessage {
	var c struct {
		Cursor json.RawMessage `json:"cursor"`
	}
	if json.Unmarshal(data, &c) != nil || c.Cursor == nil {
		return json.RawMessage("null")
	}
	return c.Cursor
}
