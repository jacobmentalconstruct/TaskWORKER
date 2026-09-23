package mcpbridge_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// wait_job's budget must bound a poll in flight, exactly like the Python
// client's wait budget. These tests drive the real stdio executable
// against loopback service stubs.

const waitJob = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func stubJob(state, text string) string {
	return fmt.Sprintf(`{"id":%q,"state":%q,"request":{"model":"m","prompt":"p","options":{"max_output_tokens":1}},"instructions":{"system":"","prompt":"p"},"created_at":"2026-01-01T00:00:00Z","result":{"text":%q,"context":{"reserved_output_tokens":0},"usage":{}}}`, waitJob, state, text)
}

// waitStub serves GET /v1/jobs/ID through handler(n) where n counts GETs from 1.
func waitStub(t *testing.T, handler func(n int, w http.ResponseWriter)) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var gets, posts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			return
		}
		handler(int(gets.Add(1)), w)
	}))
	t.Cleanup(ts.Close)
	return ts, &gets, &posts
}

func reply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, body)
}

func drip(w http.ResponseWriter, body string, total time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(200)
	step := len(body)/10 + 1
	for i := 0; i < len(body); i += step {
		end := min(i+step, len(body))
		if _, err := fmt.Fprint(w, body[i:end]); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		time.Sleep(total / 10)
	}
}

func stdioWait(t *testing.T, s *stdio, args map[string]any) (map[string]any, bool, time.Duration) {
	t.Helper()
	started := time.Now()
	s.send(modernCall(1, "wait_job", args))
	body, isErr := toolResult(t, s.expect(1))
	return body, isErr, time.Since(started)
}

func assertNotObserved(t *testing.T, body map[string]any, elapsed, max time.Duration) {
	t.Helper()
	if elapsed > max {
		t.Fatalf("wait overran its budget: %v (limit %v): %v", elapsed, max, body)
	}
	if body["timed_out"] != true || body["terminal"] != false || body["observed"] != false {
		t.Fatalf("expected timed_out, not terminal, not observed: %v", body)
	}
	if _, present := body["job"]; present {
		t.Fatalf("no job may be fabricated when nothing was observed: %v", body)
	}
	if body["job_id"] != waitJob {
		t.Fatalf("job_id must be echoed: %v", body)
	}
}

func TestStdioWaitBudgetBoundsSlowHeaders(t *testing.T) {
	ts, gets, posts := waitStub(t, func(n int, w http.ResponseWriter) {
		time.Sleep(2 * time.Second)
		reply(w, stubJob("succeeded", "late reply"))
	})
	s := startStdio(t, ts.URL)
	body, isErr, elapsed := stdioWait(t, s, map[string]any{"job_id": waitJob, "timeout_seconds": 1})
	if isErr {
		t.Fatalf("budget expiry is a timeout result, not a failure: %v", body)
	}
	assertNotObserved(t, body, elapsed, 1600*time.Millisecond)
	if strings.Contains(fmt.Sprint(body), "late reply") {
		t.Fatalf("the late reply leaked into the result: %v", body)
	}
	time.Sleep(1500 * time.Millisecond)
	if gets.Load() != 1 || posts.Load() != 0 {
		t.Fatalf("expiry must not start another poll or mutate: gets=%d posts=%d", gets.Load(), posts.Load())
	}
}

func TestStdioWaitBudgetBoundsSlowBody(t *testing.T) {
	ts, _, _ := waitStub(t, func(n int, w http.ResponseWriter) {
		drip(w, stubJob("running", strings.Repeat("x", 300)), 3*time.Second)
	})
	s := startStdio(t, ts.URL)
	body, isErr, elapsed := stdioWait(t, s, map[string]any{"job_id": waitJob, "timeout_seconds": 1})
	if isErr {
		t.Fatalf("%v", body)
	}
	assertNotObserved(t, body, elapsed, 1600*time.Millisecond)
}

func TestStdioWaitKeepsLastObservationAndDiscardsLateTerminalReply(t *testing.T) {
	ts, gets, posts := waitStub(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			reply(w, stubJob("running", "earlier"))
			return
		}
		time.Sleep(2 * time.Second)
		reply(w, stubJob("succeeded", "later"))
	})
	s := startStdio(t, ts.URL)
	body, isErr, elapsed := stdioWait(t, s, map[string]any{"job_id": waitJob, "timeout_seconds": 1})
	if isErr || elapsed > 1600*time.Millisecond {
		t.Fatalf("%v after %v", body, elapsed)
	}
	job, _ := body["job"].(map[string]any)
	if body["observed"] != true || body["timed_out"] != true || body["terminal"] != false || job["state"] != "running" || job["result"].(map[string]any)["text"] != "earlier" {
		t.Fatalf("the last state observed within the budget must be retained: %v", body)
	}
	time.Sleep(1500 * time.Millisecond)
	if gets.Load() != 2 || posts.Load() != 0 {
		t.Fatalf("no further poll and no mutation after expiry: gets=%d posts=%d", gets.Load(), posts.Load())
	}
}

func TestStdioWaitCallLimitShorterThanBudgetStaysATimeoutFault(t *testing.T) {
	ts, _, _ := waitStub(t, func(n int, w http.ResponseWriter) {
		time.Sleep(4 * time.Second)
		reply(w, stubJob("running", ""))
	})
	s := startStdio(t, ts.URL, "--timeout", "1s")
	body, isErr, elapsed := stdioWait(t, s, map[string]any{"job_id": waitJob, "timeout_seconds": 20})
	if !isErr || elapsed > 2500*time.Millisecond {
		t.Fatalf("a call-limit timeout is an explicit fault: %v after %v", body, elapsed)
	}
	if kind := body["error"].(map[string]any)["kind"]; kind != "timeout" {
		t.Fatalf("kind %v: %v", kind, body)
	}
	if _, present := body["timed_out"]; present {
		t.Fatalf("must not look like a budget expiry: %v", body)
	}
}

func TestStdioWaitZeroChecksOnceAndInBudgetResultsAreUnchanged(t *testing.T) {
	ts, gets, _ := waitStub(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			reply(w, stubJob("running", "part"))
			return
		}
		reply(w, stubJob("succeeded", "done"))
	})
	s := startStdio(t, ts.URL)
	body, isErr, _ := stdioWait(t, s, map[string]any{"job_id": waitJob, "timeout_seconds": 0})
	if isErr || body["observed"] != true || body["timed_out"] != true || body["terminal"] != false || gets.Load() != 1 {
		t.Fatalf("0 must check exactly once: %v gets=%d", body, gets.Load())
	}
	body, isErr, _ = stdioWait(t, s, map[string]any{"job_id": waitJob, "timeout_seconds": 0})
	if isErr || body["observed"] != true || body["terminal"] != true || body["timed_out"] != false || gets.Load() != 2 {
		t.Fatalf("%v gets=%d", body, gets.Load())
	}
	// A terminal reply inside the budget is returned normally.
	ts2, _, _ := waitStub(t, func(n int, w http.ResponseWriter) { reply(w, stubJob("succeeded", "quick")) })
	s2 := startStdio(t, ts2.URL)
	body, isErr, elapsed := stdioWait(t, s2, map[string]any{"job_id": waitJob, "timeout_seconds": 5})
	job, _ := body["job"].(map[string]any)
	if isErr || body["terminal"] != true || body["timed_out"] != false || body["observed"] != true || job["result"].(map[string]any)["text"] != "quick" || elapsed > 2*time.Second {
		t.Fatalf("%v after %v", body, elapsed)
	}
}

func TestStdioWaitEarlierFailuresKeepTheirOwnKinds(t *testing.T) {
	ts, _, _ := waitStub(t, func(n int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = fmt.Fprint(w, `{"code":"not_found","message":"not_found","retryable":false}`)
	})
	s := startStdio(t, ts.URL)
	body, isErr, _ := stdioWait(t, s, map[string]any{"job_id": waitJob, "timeout_seconds": 5})
	if !isErr || body["error"].(map[string]any)["code"] != "not_found" || body["error"].(map[string]any)["kind"] != "service_fault" {
		t.Fatalf("%v", body)
	}
}
