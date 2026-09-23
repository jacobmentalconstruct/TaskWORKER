package mcpbridge_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The Python client's uncertain-create rule has a bridge counterpart: once a create was sent, a
// reply this side cannot read (undecodable, malformed job, over the client's
// bound) is not a definite rejection. The result must say acceptance is
// uncertain and tell the caller to resend the same arguments; read-only tools
// must not claim that.
func TestUnreadableCreateRepliesStayUncertain(t *testing.T) {
	replies := map[string]struct {
		status int
		body   string
	}{
		"invalid_json":        {200, "{"},
		"array_not_object":    {200, "[1,2]"},
		"object_without_job":  {200, `{"hello":"world"}`},
		"job_without_state":   {200, `{"id":"` + strings.Repeat("a", 32) + `"}`},
		"job_with_short_id":   {200, `{"id":"abc","state":"queued"}`},
		"oversized_fault":     {500, strings.Repeat("x", 5000)},
		"malformed_fault":     {500, "<html>nope"},
		"fault_without_code":  {409, `{"message":"x"}`},
		"empty_success_reply": {200, ""},
	}
	for name, reply := range replies {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			posts := 0
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					mu.Lock()
					posts++
					mu.Unlock()
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(reply.status)
				_, _ = io.WriteString(w, reply.body)
			}))
			defer stub.Close()
			cs, _ := bridgeTo(t, stub.URL)
			e := &env{t: t, sess: cs}
			for _, tool := range []struct {
				name string
				args map[string]any
			}{
				{"submit_job", map[string]any{"idempotency_key": "unreadable-" + name, "request": req("p")}},
				{"retry_job", map[string]any{"idempotency_key": "unreadable-r-" + name, "parent_id": strings.Repeat("a", 32)}},
				{"branch_job", map[string]any{"idempotency_key": "unreadable-b-" + name, "parent_id": strings.Repeat("a", 32), "request": req("p")}},
			} {
				o := e.call(tool.name, tool.args)
				if !o.isErr {
					t.Fatalf("%s: an unreadable reply must not be reported as success: %s", tool.name, o.text)
				}
				if o.obj["acceptance_uncertain"] != true || !strings.Contains(o.text, "same idempotency_key") {
					t.Fatalf("%s: uncertainty and recovery guidance required: %s", tool.name, o.text)
				}
			}
			mu.Lock()
			got := posts
			mu.Unlock()
			if got != 3 {
				t.Fatalf("exactly one POST per call and never an automatic resend, got %d", got)
			}
			// Read-only tools never claim create uncertainty.
			g := e.call("get_job", map[string]any{"job_id": strings.Repeat("a", 32)})
			if !g.isErr {
				t.Fatalf("read of an unreadable reply must be an error: %s", g.text)
			}
			if _, present := g.obj["acceptance_uncertain"]; present {
				t.Fatalf("read-only call must not carry acceptance_uncertain: %s", g.text)
			}
		})
	}
}

// A valid definitive fault stays definite, and recovery by resending the same
// arguments resolves to one job.
func TestDefiniteFaultsStayDefiniteAndResendRecovers(t *testing.T) {
	first := true
	var mu sync.Mutex
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bad := first
		first = false
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if bad {
			_, _ = io.WriteString(w, "{")
			return
		}
		_, _ = io.WriteString(w, `{"id":"`+strings.Repeat("c", 32)+`","state":"queued","request":{"model":"m","prompt":"p","options":{"max_output_tokens":1}}}`)
	}))
	defer stub.Close()
	cs, _ := bridgeTo(t, stub.URL)
	e := &env{t: t, sess: cs}
	args := map[string]any{"idempotency_key": "resend-me", "request": req("p")}
	if o := e.call("submit_job", args); !o.isErr || o.obj["acceptance_uncertain"] != true {
		t.Fatal(o.text)
	}
	again := e.ok("submit_job", args)
	if again.obj["job"].(map[string]any)["id"] != strings.Repeat("c", 32) || again.obj["idempotency_key"] != "resend-me" {
		t.Fatal(again.text)
	}
	conflict := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = io.WriteString(w, `{"code":"conflict","message":"conflict","retryable":false}`)
	}))
	defer conflict.Close()
	cs2, _ := bridgeTo(t, conflict.URL)
	c := (&env{t: t, sess: cs2}).call("submit_job", args)
	if !c.isErr || c.obj["acceptance_uncertain"] != false || code(c) != "conflict" {
		t.Fatal(c.text)
	}
}
