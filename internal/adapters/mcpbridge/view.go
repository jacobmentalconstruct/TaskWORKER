package mcpbridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"taskworker.local/taskworker/internal/adapters/httpapi"
	"taskworker.local/taskworker/internal/core"
)

// decodeObject keeps every field (including ones this build does not know) and
// exact numbers: json.Number preserves integer digits through re-encoding.
func decodeObject(raw []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil || m == nil {
		return nil, &core.Fault{Code: core.ErrInternal, Message: httpapi.MessageInvalidResponse}
	}
	return m, nil
}

func objectAt(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func stateOf(job map[string]any) core.JobState {
	s, _ := job["state"].(string)
	return core.JobState(s)
}

func outputOf(job map[string]any) string {
	s, _ := objectAt(job, "result")["text"].(string)
	return s
}

type omission struct {
	Part        string `json:"part"`
	Reason      string `json:"reason"`
	Bytes       int    `json:"bytes,omitempty"`
	RetrieveVia string `json:"retrieve_via,omitempty"`
}

// jobView is the one place a service job becomes a tool result. Anything left
// out is named in omitted, never dropped silently. Output beyond the result
// bound is retrievable through read_job_output; oversized inputs only through
// HTTP/CLI.
func jobView(job map[string]any, includeOutput, includeInputs bool) map[string]any {
	text := outputOf(job)
	view := map[string]any{}
	for k, v := range job {
		view[k] = v
	}
	var omitted []omission
	strip := func(part string, reason string) {
		switch part {
		case "output":
			res := map[string]any{}
			for k, v := range objectAt(view, "result") {
				res[k] = v
			}
			delete(res, "text")
			view["result"] = res
			omitted = append(omitted, omission{Part: "output", Reason: reason, Bytes: len(text), RetrieveVia: "read_job_output"})
		case "inputs":
			req := map[string]any{}
			size := 0
			for k, v := range objectAt(view, "request") {
				switch k {
				case "prompt", "role", "system_prompt":
					if s, ok := v.(string); ok {
						size += len(s)
					}
					continue
				}
				req[k] = v
			}
			view["request"] = req
			if in, err := json.Marshal(view["instructions"]); err == nil {
				size += len(in)
			}
			delete(view, "instructions")
			omitted = append(omitted, omission{Part: "inputs", Reason: reason, Bytes: size, RetrieveVia: "HTTP GET /v1/jobs/ID or taskworker get"})
		}
	}
	if !includeOutput {
		strip("output", "not_requested")
	}
	if !includeInputs {
		strip("inputs", "not_requested")
	}
	size := func() int { b, _ := json.Marshal(view); return len(b) }
	if size() > MaxResultBytes && includeOutput {
		strip("output", "exceeds_result_bound")
	}
	if size() > MaxResultBytes && includeInputs {
		strip("inputs", "exceeds_result_bound")
	}
	out := map[string]any{"job": view, "output_bytes": len(text)}
	if len(omitted) > 0 {
		out["omitted"] = omitted
	}
	return out
}

func summary(job map[string]any) map[string]any {
	s := map[string]any{"id": job["id"], "state": job["state"], "output_bytes": len(outputOf(job))}
	for _, k := range []string{"origin", "lineage", "created_at", "started_at", "finished_at"} {
		if v, ok := job[k]; ok {
			s[k] = v
		}
	}
	if m, ok := objectAt(job, "request")["model"]; ok {
		s["model"] = m
	}
	if e := objectAt(job, "error"); e != nil {
		s["error_code"] = e["code"]
	}
	return s
}

// pageOutput returns [offset, offset+max) of text on UTF-8 boundaries. The end
// backs up to a rune boundary; an offset inside a rune is a caller error.
func pageOutput(text string, offset, max int) (string, int, error) {
	if offset < 0 || offset > len(text) || (offset < len(text) && !utf8.RuneStart(text[offset])) {
		return "", 0, &core.Fault{Code: core.ErrInvalidRequest, Message: "offset_bytes must be within output and on a UTF-8 character boundary"}
	}
	end := offset + max
	if end >= len(text) {
		return text[offset:], len(text), nil
	}
	for end > offset && !utf8.RuneStart(text[end]) {
		end--
	}
	if end == offset {
		return "", 0, &core.Fault{Code: core.ErrInvalidRequest, Message: "max_bytes is smaller than one character at offset_bytes"}
	}
	return text[offset:end], end, nil
}

// faultBody classifies a failed service call. kind separates an unreachable
// service, a call/observation timeout and an explicit service fault.
func (b *bridge) faultBody(err error, creates bool) map[string]any {
	kind := "service_fault"
	f := httpapi.PublicFault(err)
	var te timeoutErr
	switch {
	case errors.As(err, &te):
		kind = "timeout"
	case httpapi.IsConnectionFault(err):
		kind = "service_unreachable"
	case httpapi.IsUnreadableResponse(err):
		// The reply was received but cannot establish an outcome.
		kind = "response_unreadable"
	}
	body := map[string]any{"code": f.Code, "message": f.Message, "retryable": f.Retryable, "kind": kind}
	e := map[string]any{"error": body}
	if creates {
		// Ambiguous outcomes: the service may have durably accepted the command.
		switch {
		case kind != "service_fault", f.Code == core.ErrUnavailable, f.Code == core.ErrStorage, f.Code == core.ErrInternal:
			e["acceptance_uncertain"] = true
			e["recovery"] = "Call the same tool again with identical arguments and the same idempotency_key to resolve to the original job. Do not use a new key. retryable is advice only."
		default:
			e["acceptance_uncertain"] = false
		}
	}
	return e
}
