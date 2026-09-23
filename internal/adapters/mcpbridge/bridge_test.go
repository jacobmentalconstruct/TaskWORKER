package mcpbridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"taskworker.local/taskworker/internal/adapters/httpapi"
	"taskworker.local/taskworker/internal/adapters/mcpbridge"
	"taskworker.local/taskworker/internal/core"
	"taskworker.local/taskworker/internal/testservice"
)

type env struct {
	t    *testing.T
	svc  *testservice.Running
	http *httpapi.Client
	sess *mcp.ClientSession
	gate string
	data string
}

func startService(t *testing.T, cfg testservice.Config) (*testservice.Running, string) {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	svc, err := testservice.Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Error(err)
		}
	})
	return svc, cfg.DataDir
}

// bridgeTo connects an in-memory MCP client to a bridge for the given URL.
func bridgeTo(t *testing.T, url string) (*mcp.ClientSession, *httpapi.Client) {
	t.Helper()
	c, err := httpapi.NewClient(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	srv := mcpbridge.NewServerForTest(mcpbridge.Config{Client: c, Server: url, Version: "test", CallTimeout: 5 * time.Second, Stderr: io.Discard})
	t1, t2 := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	cl := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	cs, err := cl.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Wait() })
	return cs, c
}

func newEnv(t *testing.T) *env {
	t.Helper()
	gate := t.TempDir()
	svc, data := startService(t, testservice.Config{GateDir: gate})
	cs, hc := bridgeTo(t, svc.URL)
	return &env{t: t, svc: svc, http: hc, sess: cs, gate: gate, data: data}
}

type out struct {
	isErr bool
	text  string
	obj   map[string]any
}

func parse(text string) map[string]any {
	d := json.NewDecoder(strings.NewReader(text))
	d.UseNumber()
	var m map[string]any
	if d.Decode(&m) != nil {
		return nil
	}
	return m
}

func (e *env) call(name string, args map[string]any) out {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := e.sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		e.t.Fatalf("%s: protocol error: %v", name, err)
	}
	var text string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return out{r.IsError, text, parse(text)}
}

func (e *env) ok(name string, args map[string]any) out {
	e.t.Helper()
	o := e.call(name, args)
	if o.isErr {
		e.t.Fatalf("%s failed: %s", name, o.text)
	}
	return o
}

func req(prompt string) map[string]any {
	return map[string]any{"model": "fake:latest", "prompt": prompt, "options": map[string]any{"max_output_tokens": 8}}
}

func (e *env) submit(key, prompt string) string {
	e.t.Helper()
	o := e.ok("submit_job", map[string]any{"idempotency_key": key, "request": req(prompt)})
	return o.obj["job"].(map[string]any)["id"].(string)
}

func (e *env) waitState(id string, states ...string) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		o := e.ok("get_job", map[string]any{"job_id": id})
		job := o.obj["job"].(map[string]any)
		for _, s := range states {
			if job["state"] == s {
				return job
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("job %s never reached %v", id, states)
	return nil
}

func (e *env) outputContains(id, want string) {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		o := e.ok("read_job_output", map[string]any{"job_id": id})
		if strings.Contains(o.obj["text"].(string), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("output of %s never contained %q", id, want)
}

func code(o out) string {
	if o.obj == nil {
		return ""
	}
	e, _ := o.obj["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestToolsAreStrictAndCapabilitiesMinimal(t *testing.T) {
	e := newEnv(t)
	res, err := e.sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"branch_job", "cancel_job", "get_job", "list_jobs", "list_models", "pause_queue", "queue_status", "read_events", "read_job_output", "resume_queue", "retry_job", "service_health", "submit_job", "wait_job"}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
		raw, _ := json.Marshal(tool.InputSchema)
		var s map[string]any
		_ = json.Unmarshal(raw, &s)
		if s["type"] != "object" || s["additionalProperties"] != false {
			t.Errorf("%s: schema not strict: %s", tool.Name, raw)
		}
		if len(tool.Description) < 40 {
			t.Errorf("%s: description too thin", tool.Name)
		}
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("missing tool %s", n)
		}
	}
	if len(res.Tools) != len(want) {
		t.Errorf("unexpected tool count %d", len(res.Tools))
	}
	caps := e.sess.InitializeResult().Capabilities
	if caps.Tools == nil || caps.Logging != nil || caps.Prompts != nil || caps.Resources != nil || caps.Completions != nil {
		t.Errorf("capabilities must be tools only: %+v", caps)
	}
	if err := e.sess.Ping(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitObserveMatchesHTTP(t *testing.T) {
	e := newEnv(t)
	id := e.submit("k-basic", "hello 世界")
	w := e.ok("wait_job", map[string]any{"job_id": id, "timeout_seconds": 10})
	if w.obj["terminal"] != true || w.obj["timed_out"] != false {
		t.Fatal(w.text)
	}
	job := w.obj["job"].(map[string]any)
	if job["state"] != "succeeded" || job["result"].(map[string]any)["text"] != "reply: hello 世界" {
		t.Fatal(w.text)
	}
	// The same durable job is visible through the HTTP client used by CLI/UI.
	hj, err := e.http.Get(context.Background(), core.JobID(id))
	if err != nil || string(hj.State) != "succeeded" || hj.Result.Text != "reply: hello 世界" || hj.Origin != "mcp" {
		t.Fatalf("%+v %v", hj, err)
	}
	// Job listing and queue status agree.
	l := e.ok("list_jobs", nil)
	jobs := l.obj["jobs"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["id"] != id || l.obj["cursor"] == nil {
		t.Fatal(l.text)
	}
	if strings.Contains(l.text, "reply:") {
		t.Fatal("summaries must not carry output")
	}
	q := e.ok("queue_status", nil)
	if q.obj["paused"] != false || q.obj["max_pending"] == nil {
		t.Fatal(q.text)
	}
}

func TestHealthAndBackendAvailabilityAreDistinct(t *testing.T) {
	svc, _ := startService(t, testservice.Config{ModelsDown: true})
	cs, _ := bridgeTo(t, svc.URL)
	e := &env{t: t, sess: cs}
	h := e.ok("service_health", nil)
	if h.obj["status"] != "serving" || h.obj["backend"] != "not_checked" {
		t.Fatal(h.text)
	}
	m := e.call("list_models", nil)
	if !m.isErr || code(m) != "backend_unavailable" {
		t.Fatal(m.text)
	}
	good := newEnv(t).ok("list_models", nil)
	ms := good.obj["models"].([]any)
	if len(ms) != 1 || ms[0].(map[string]any)["id"] != "fake:latest" {
		t.Fatal(good.text)
	}
}

func TestMissingServiceIsConnectionFailureAndProtocolStillWorks(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	url := "http://" + l.Addr().String()
	l.Close()
	cs, _ := bridgeTo(t, url)
	e := &env{t: t, sess: cs}
	h := e.call("service_health", nil)
	if !h.isErr || h.obj["error"].(map[string]any)["kind"] != "service_unreachable" {
		t.Fatal(h.text)
	}
	s := e.call("submit_job", map[string]any{"idempotency_key": "k-down", "request": req("x")})
	if !s.isErr || s.obj["acceptance_uncertain"] != true || !strings.Contains(s.text, "same idempotency_key") {
		t.Fatal(s.text)
	}
	if err := cs.Ping(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestIdempotencyAndConflict(t *testing.T) {
	e := newEnv(t)
	a := e.submit("k-idem", "same")
	b := e.submit("k-idem", "same")
	if a != b {
		t.Fatalf("resend created a second job: %s %s", a, b)
	}
	c := e.call("submit_job", map[string]any{"idempotency_key": "k-idem", "request": req("different")})
	if !c.isErr || code(c) != "conflict" || c.obj["acceptance_uncertain"] != false {
		t.Fatal(c.text)
	}
	// Origin is content: a changed origin conflicts, a repeated default does not.
	d := e.call("submit_job", map[string]any{"idempotency_key": "k-idem", "origin": "other", "request": req("same")})
	if code(d) != "conflict" {
		t.Fatal(d.text)
	}
	if n := len(e.ok("list_jobs", nil).obj["jobs"].([]any)); n != 1 {
		t.Fatalf("expected one job, got %d", n)
	}
	// Local validation never reaches the service.
	long := e.call("submit_job", map[string]any{"idempotency_key": strings.Repeat("é", 65), "request": req("x")})
	if !long.isErr || code(long) != "invalid_request" {
		t.Fatal(long.text)
	}
}

// lossyProxy forwards submits upstream and then drops the response, modeling a
// lost acceptance reply.
func lossyProxy(t *testing.T, upstream string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	dropped := 0
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		up, err := http.NewRequest(r.Method, upstream+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			t.Error(err)
			return
		}
		up.Header = r.Header.Clone()
		up.Host = strings.TrimPrefix(upstream, "http://")
		resp, err := http.DefaultTransport.RoundTrip(up)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		if r.URL.Path == "/v1/submit" {
			mu.Lock()
			dropped++
			mu.Unlock()
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

func TestLostAcceptanceResolvesToOneJob(t *testing.T) {
	svc, _ := startService(t, testservice.Config{})
	proxy := lossyProxy(t, svc.URL)
	// Point the bridge at the proxy: Host must equal the proxy authority, so
	// rewrite it upstream (done in lossyProxy). The service authority check
	// otherwise rejects it.
	lossy, _ := bridgeTo(t, proxy.URL)
	direct, _ := bridgeTo(t, svc.URL)
	le, de := &env{t: t, sess: lossy}, &env{t: t, sess: direct}
	args := map[string]any{"idempotency_key": "k-lost", "request": req("lost reply")}
	first := le.call("submit_job", args)
	if !first.isErr || first.obj["acceptance_uncertain"] != true || first.obj["error"].(map[string]any)["kind"] != "service_unreachable" {
		t.Fatal(first.text)
	}
	// A bridge restart is just another bridge: the resend resolves to the job.
	again := de.ok("submit_job", args)
	id := again.obj["job"].(map[string]any)["id"].(string)
	jobs := de.ok("list_jobs", nil).obj["jobs"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["id"] != id {
		t.Fatalf("expected exactly the original job: %v", jobs)
	}
	if again.obj["idempotency_key"] != "k-lost" {
		t.Fatal("key must be echoed for recovery")
	}
}

func TestCancelPreservesPartialOutputAndLifecycleOrder(t *testing.T) {
	e := newEnv(t)
	id := e.submit("k-hold", "hold")
	e.outputContains(id, "started")
	// Cursor before cancellation, then explicit cancel.
	cur := e.ok("list_jobs", nil).obj["cursor"].(map[string]any)
	after := fmt.Sprintf("%s:%s", cur["store_id"], cur["sequence"])
	c := e.ok("cancel_job", map[string]any{"job_id": id})
	if s := c.obj["job"].(map[string]any)["state"]; s != "cancelling" && s != "cancelled" {
		t.Fatal(c.text)
	}
	w := e.ok("wait_job", map[string]any{"job_id": id, "timeout_seconds": 10})
	job := w.obj["job"].(map[string]any)
	if job["state"] != "cancelled" || job["result"].(map[string]any)["text"] != "started" || w.obj["terminal"] != true {
		t.Fatal(w.text)
	}
	ev := e.ok("read_events", map[string]any{"after": after, "job_id": id, "timeout_seconds": 1})
	var states []string
	for _, x := range ev.obj["events"].([]any) {
		m := x.(map[string]any)
		if m["kind"] == "job.state" || m["kind"] == "job.result" {
			states = append(states, m["job"].(map[string]any)["state"].(string))
		}
	}
	if strings.Join(states, ",") != "cancelling,cancelled" {
		t.Fatalf("lifecycle order %v: %s", states, ev.text)
	}
	// Cancelling again is safe and returns the terminal job unchanged.
	again := e.ok("cancel_job", map[string]any{"job_id": id})
	if again.obj["job"].(map[string]any)["state"] != "cancelled" {
		t.Fatal(again.text)
	}
}

func TestCancelledWaitAndBridgeCallDoNotCancelInference(t *testing.T) {
	e := newEnv(t)
	id := e.submit("k-detach", "hold")
	e.outputContains(id, "started")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.sess.CallTool(ctx, &mcp.CallToolParams{Name: "wait_job", Arguments: map[string]any{"job_id": id, "timeout_seconds": 120}})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	cancel() // MCP notifications/cancelled for the blocked call
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled wait did not return")
	}
	// The wait slot is released and the job is still running.
	if j := e.waitState(id, "running"); j["state"] != "running" {
		t.Fatal(j)
	}
	// A wait that merely times out also leaves the job running.
	w := e.ok("wait_job", map[string]any{"job_id": id, "timeout_seconds": 1})
	if w.obj["timed_out"] != true || w.obj["terminal"] != false {
		t.Fatal(w.text)
	}
	e.ok("cancel_job", map[string]any{"job_id": id})
	e.waitState(id, "cancelled")
}

func TestBlockedWaitsDoNotBlockControlOrPing(t *testing.T) {
	e := newEnv(t)
	id := e.submit("k-block", "hold")
	e.outputContains(id, "started")
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < mcpbridge.MaxWaiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.sess.CallTool(ctx, &mcp.CallToolParams{Name: "wait_job", Arguments: map[string]any{"job_id": id, "timeout_seconds": 120}})
		}()
	}
	time.Sleep(500 * time.Millisecond)
	// Over the waiter policy: explicit, prompt refusal instead of queueing.
	busy := e.call("wait_job", map[string]any{"job_id": id, "timeout_seconds": 1})
	if !busy.isErr || busy.obj["error"].(map[string]any)["kind"] != "bridge_busy" {
		t.Fatal(busy.text)
	}
	// Control and protocol traffic still works.
	pctx, pc := context.WithTimeout(context.Background(), 3*time.Second)
	defer pc()
	if err := e.sess.Ping(pctx, nil); err != nil {
		t.Fatal(err)
	}
	e.ok("queue_status", nil)
	e.ok("cancel_job", map[string]any{"job_id": id})
	e.waitState(id, "cancelled")
	cancel()
	wg.Wait()
}

func TestQueuePauseResumeAndFIFO(t *testing.T) {
	e := newEnv(t)
	e.ok("pause_queue", nil)
	a, b := e.submit("k-q1", "first"), e.submit("k-q2", "second")
	q := e.ok("queue_status", nil).obj
	pending := q["pending"].([]any)
	if q["paused"] != true || len(pending) != 2 || pending[0] != a || pending[1] != b {
		t.Fatalf("%v", q)
	}
	if g := e.ok("get_job", map[string]any{"job_id": a}).obj["job"].(map[string]any)["state"]; g != "queued" {
		t.Fatal(g)
	}
	if again := e.ok("pause_queue", nil).obj["paused"]; again != true {
		t.Fatal("repeat pause must be a no-op")
	}
	e.ok("resume_queue", nil)
	e.waitState(a, "succeeded")
	e.waitState(b, "succeeded")
	// FIFO: a started no later than b.
	ta := e.ok("get_job", map[string]any{"job_id": a}).obj["job"].(map[string]any)["started_at"].(string)
	tb := e.ok("get_job", map[string]any{"job_id": b}).obj["job"].(map[string]any)["started_at"].(string)
	if ta > tb {
		t.Fatal("started out of order")
	}
}

func TestRetryAndBranchLineage(t *testing.T) {
	e := newEnv(t)
	parent := e.submit("k-par", "fail")
	pj := e.waitState(parent, "failed")
	if pj["result"].(map[string]any)["text"] != "partial" {
		t.Fatal(pj)
	}
	// Terminal failure is an observed state, not a tool error.
	w := e.call("wait_job", map[string]any{"job_id": parent, "timeout_seconds": 1})
	if w.isErr || w.obj["job"].(map[string]any)["error"].(map[string]any)["code"] != "backend_failure" {
		t.Fatal(w.text)
	}
	r := e.ok("retry_job", map[string]any{"idempotency_key": "k-retry", "parent_id": parent})
	rj := r.obj["job"].(map[string]any)
	lin := rj["lineage"].(map[string]any)
	if lin["parent_id"] != parent || lin["relation"] != "retry" || rj["id"] == parent {
		t.Fatal(r.text)
	}
	// Resending the same retry key resolves to the same new job, not a third.
	if r2 := e.ok("retry_job", map[string]any{"idempotency_key": "k-retry", "parent_id": parent}); r2.obj["job"].(map[string]any)["id"] != rj["id"] {
		t.Fatal("retry resend created another job")
	}
	br := e.ok("branch_job", map[string]any{"idempotency_key": "k-branch", "parent_id": parent, "request": req("continue")})
	bj := br.obj["job"].(map[string]any)
	if bj["lineage"].(map[string]any)["relation"] != "branch" {
		t.Fatal(br.text)
	}
	hist := bj["instructions"].(map[string]any)["history"].([]any)
	if len(hist) != 2 || hist[0].(map[string]any)["content"] != "fail" || hist[1].(map[string]any)["content"] != "partial" {
		t.Fatalf("branch must carry the parent's full history including partial output: %s", br.text)
	}
	// The parent is immutable.
	if e.ok("get_job", map[string]any{"job_id": parent}).obj["job"].(map[string]any)["state"] != "failed" {
		t.Fatal("parent changed")
	}
	// A non-terminal parent is refused with the service's own fault.
	live := e.submit("k-live", "hold")
	e.outputContains(live, "started")
	bad := e.call("retry_job", map[string]any{"idempotency_key": "k-badretry", "parent_id": live})
	if !bad.isErr || bad.obj["error"].(map[string]any)["kind"] != "service_fault" {
		t.Fatal(bad.text)
	}
	e.ok("cancel_job", map[string]any{"job_id": live})
}

func TestUnicodeOffsetsAndOutputPaging(t *testing.T) {
	e := newEnv(t)
	id := e.submit("k-uni", "unicode")
	e.waitState(id, "succeeded")
	var got []string
	off := 0
	for i := 0; i < 10; i++ {
		o := e.ok("read_job_output", map[string]any{"job_id": id, "offset_bytes": off, "max_bytes": 4})
		got = append(got, o.obj["text"].(string))
		next := jsonInt(t, o.obj["next_offset_bytes"])
		if o.obj["total_bytes"].(json.Number).String() != "12" {
			t.Fatal(o.text)
		}
		if o.obj["end_of_output"] == true {
			off = next
			break
		}
		off = next
	}
	if strings.Join(got, "|") != "é|世|界|🙂" || off != 12 {
		t.Fatalf("pages %q end %d", got, off)
	}
	mid := e.call("read_job_output", map[string]any{"job_id": id, "offset_bytes": 3})
	if !mid.isErr || code(mid) != "invalid_request" {
		t.Fatal("offset inside a character must be rejected")
	}
	tiny := e.call("read_job_output", map[string]any{"job_id": id, "offset_bytes": 8, "max_bytes": 4})
	if tiny.isErr || tiny.obj["text"] != "🙂" {
		t.Fatal(tiny.text)
	}
}

func jsonInt(t *testing.T, v any) int {
	t.Helper()
	n, err := v.(json.Number).Int64()
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func TestLargeOutputIsOmittedExplicitlyAndPageable(t *testing.T) {
	e := newEnv(t)
	const size = 600 << 10
	id := e.submit("k-big", fmt.Sprintf("big:%d", size))
	e.waitState(id, "succeeded")
	g := e.ok("get_job", map[string]any{"job_id": id})
	om, _ := g.obj["omitted"].([]any)
	if len(om) != 1 || om[0].(map[string]any)["part"] != "output" || om[0].(map[string]any)["reason"] != "exceeds_result_bound" {
		t.Fatalf("omission must be explicit: %s", g.text[:min(len(g.text), 400)])
	}
	if jsonInt(t, g.obj["output_bytes"]) != size || len(g.text) > 2*mcpbridge.MaxResultBytes {
		t.Fatal("bounded result with exact size expected")
	}
	if strings.Contains(g.text, "xxxxxxxx") {
		t.Fatal("omitted output leaked")
	}
	total, off := 0, 0
	for {
		o := e.ok("read_job_output", map[string]any{"job_id": id, "offset_bytes": off, "max_bytes": 131072})
		total += len(o.obj["text"].(string))
		off = jsonInt(t, o.obj["next_offset_bytes"])
		if o.obj["end_of_output"] == true {
			break
		}
	}
	if total != size {
		t.Fatalf("paged %d of %d", total, size)
	}
	// The caller may also ask for metadata only.
	m := e.ok("get_job", map[string]any{"job_id": id, "include_output": false, "include_inputs": false})
	if len(m.obj["omitted"].([]any)) != 2 {
		t.Fatal(m.text)
	}
}

func TestFaultMappingsAreExplicit(t *testing.T) {
	e := newEnv(t)
	nf := e.call("get_job", map[string]any{"job_id": strings.Repeat("a", 32)})
	if !nf.isErr || code(nf) != "not_found" {
		t.Fatal(nf.text)
	}
	// Schema violations are rejected by the strict schema before any service call.
	for _, args := range []map[string]any{
		{"job_id": "not-hex"},
		{"job_id": strings.Repeat("a", 32), "extra": 1},
	} {
		if r := e.call("get_job", args); !r.isErr || r.obj != nil && r.obj["job"] != nil {
			t.Fatalf("expected rejection: %v %s", args, r.text)
		}
	}
	badOpt := e.call("submit_job", map[string]any{"idempotency_key": "k1", "request": map[string]any{"model": "fake:latest", "prompt": "p", "options": map[string]any{"max_output_tokens": 1.5}}})
	if !badOpt.isErr {
		t.Fatal("fractional integer option must not be rounded")
	}
	// Protocol error is distinct from a tool error.
	_, err := e.sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "no_such_tool"})
	if err == nil {
		t.Fatal("unknown tool must be a protocol error")
	}
	// queue_full is a definite rejection.
	e.ok("pause_queue", nil)
	for i := 0; i < 64; i++ {
		e.submit(fmt.Sprintf("k-fill-%d", i), "fill")
	}
	full := e.call("submit_job", map[string]any{"idempotency_key": "k-over", "request": req("over")})
	if !full.isErr || code(full) != "queue_full" || full.obj["acceptance_uncertain"] != false {
		t.Fatal(full.text)
	}
}

func TestExactNumbersOptionsAndUnknownMetadata(t *testing.T) {
	// Options round-trip exactly, including integers beyond 2^53.
	e := newEnv(t)
	args := map[string]any{"idempotency_key": "k-num", "request": map[string]any{"model": "fake:latest", "prompt": "n", "options": map[string]any{"max_output_tokens": 8, "context_tokens": 1024, "seed": json.Number("9007199254740993"), "temperature": 0.25}}}
	raw, _ := json.Marshal(args)
	var wire map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	_ = d.Decode(&wire)
	o := e.ok("submit_job", wire)
	if !strings.Contains(o.text, `"seed":9007199254740993`) {
		t.Fatalf("seed must not be rounded: %s", o.text)
	}
	// Unknown fields from a newer service pass through untouched.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"`+strings.Repeat("b", 32)+`","state":"running","future":{"n":18446744073709551615,"deep":[1,{"x":null}]},"request":{"model":"m","prompt":"p","options":{"max_output_tokens":1}},"instructions":{"system":"","prompt":"p"},"created_at":"2026-01-01T00:00:00Z","result":{"text":"abc","context":{"reserved_output_tokens":0},"usage":{}}}`)
	}))
	defer stub.Close()
	cs, _ := bridgeTo(t, stub.URL)
	s := &env{t: t, sess: cs}
	g := s.ok("get_job", map[string]any{"job_id": strings.Repeat("b", 32)})
	if !strings.Contains(g.text, `"n":18446744073709551615`) || !strings.Contains(g.text, `"x":null`) {
		t.Fatalf("unknown metadata not preserved exactly: %s", g.text)
	}
}

// sseStub serves canned SSE frames for read_events cursor exactness tests.
func sseStub(t *testing.T, status int, body string, hold bool) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
		w.(http.Flusher).Flush()
		if hold {
			<-r.Context().Done()
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

const store = "0123456789abcdef0123456789abcdef"

func frame(seq string, job string, kind string) string {
	data := fmt.Sprintf(`{"cursor":{"store_id":"%s","sequence":"%s"},"at":"2026-01-01T00:00:00Z","kind":"%s","job_id":"%s","output":{"offset_bytes":0,"text":"é"}}`, store, seq, kind, job)
	return fmt.Sprintf("id: %s:%s\nevent: event\ndata: %s\n\n", store, seq, data)
}

func TestReadEventsKeepsExactCursorsAndFilters(t *testing.T) {
	j1, j2 := strings.Repeat("1", 32), strings.Repeat("2", 32)
	body := ": connected\n\n" + frame("18446744073709551614", j1, "job.output") + frame("18446744073709551615", j2, "job.output")
	stub := sseStub(t, 200, body, true)
	cs, _ := bridgeTo(t, stub.URL)
	e := &env{t: t, sess: cs}
	after := store + ":18446744073709551613"
	all := e.ok("read_events", map[string]any{"after": after, "timeout_seconds": 1})
	if all.obj["next_cursor"] != store+":18446744073709551615" || jsonInt(t, all.obj["count"]) != 2 || all.obj["stopped_by"] != "timeout" || all.obj["timed_out"] != true {
		t.Fatal(all.text)
	}
	if !strings.Contains(all.text, `\"sequence\":\"18446744073709551614\"`) && !strings.Contains(all.text, `"sequence":"18446744073709551614"`) {
		t.Fatal("sequence must stay a string")
	}
	// A filter advances the cursor over scanned events so nothing is re-read.
	f := e.ok("read_events", map[string]any{"after": after, "job_id": j1, "timeout_seconds": 1})
	if jsonInt(t, f.obj["count"]) != 1 || f.obj["next_cursor"] != store+":18446744073709551615" {
		t.Fatal(f.text)
	}
	// max_events stops early and next_cursor is the last returned event.
	m := e.ok("read_events", map[string]any{"after": after, "max_events": 1, "timeout_seconds": 5})
	if m.obj["stopped_by"] != "max_events" || m.obj["next_cursor"] != store+":18446744073709551614" {
		t.Fatal(m.text)
	}
	// Noncanonical cursors are rejected by the schema, not normalized.
	for _, bad := range []string{store + ":007", "XYZ", store + ":18446744073709551616"} {
		r := e.call("read_events", map[string]any{"after": bad})
		if !r.isErr {
			t.Fatalf("%q must be rejected: %s", bad, r.text)
		}
	}
}

func TestReadEventsCursorFaultsAreNeverReset(t *testing.T) {
	for _, c := range []struct {
		status int
		code   string
	}{{410, "cursor_expired"}, {400, "cursor_invalid"}} {
		stub := sseStub(t, c.status, fmt.Sprintf(`{"code":"%s","message":"x","retryable":false}`, c.code), false)
		cs, _ := bridgeTo(t, stub.URL)
		e := &env{t: t, sess: cs}
		r := e.call("read_events", map[string]any{"after": store + ":5", "timeout_seconds": 1})
		if !r.isErr || code(r) != c.code || r.obj["next_cursor"] != nil {
			t.Fatalf("%s: %s", c.code, r.text)
		}
	}
	// Against the real service: wrong store and a future sequence.
	e := newEnv(t)
	for _, after := range []string{"ffffffffffffffffffffffffffffffff:1", jsonCursorFuture(e)} {
		r := e.call("read_events", map[string]any{"after": after, "timeout_seconds": 1})
		if !r.isErr || code(r) != "cursor_invalid" {
			t.Fatalf("%s: %s", after, r.text)
		}
	}
}

func jsonCursorFuture(e *env) string {
	c := e.ok("list_jobs", nil).obj["cursor"].(map[string]any)
	return fmt.Sprintf("%s:%s", c["store_id"], "18446744073709551615")
}

func TestOversizedEventIsAnExplicitMarker(t *testing.T) {
	huge := strings.Repeat("y", mcpbridge.MaxInlineEvent+10)
	data := fmt.Sprintf(`{"cursor":{"store_id":"%s","sequence":"7"},"at":"2026-01-01T00:00:00Z","kind":"job.output","job_id":"%s","output":{"offset_bytes":0,"text":"%s"}}`, store, strings.Repeat("3", 32), huge)
	stub := sseStub(t, 200, fmt.Sprintf("id: %s:7\nevent: event\ndata: %s\n\n", store, data), true)
	cs, _ := bridgeTo(t, stub.URL)
	e := &env{t: t, sess: cs}
	r := e.ok("read_events", map[string]any{"after": store + ":6", "timeout_seconds": 1})
	if !strings.Contains(r.text, `"omitted":true`) || strings.Contains(r.text, "yyyyyyyy") || r.obj["next_cursor"] != store+":7" {
		t.Fatal(r.text[:min(len(r.text), 500)])
	}
}

func TestReadEventsFromRealServiceAndReconnectFromLastApplied(t *testing.T) {
	e := newEnv(t)
	cur := e.ok("list_jobs", nil).obj["cursor"].(map[string]any)
	after := fmt.Sprintf("%s:%s", cur["store_id"], cur["sequence"])
	id := e.submit("k-ev", "unicode")
	e.waitState(id, "succeeded")
	var kinds []string
	var offsets []int
	for i := 0; i < 20; i++ {
		r := e.ok("read_events", map[string]any{"after": after, "max_events": 2, "timeout_seconds": 1})
		for _, x := range r.obj["events"].([]any) {
			m := x.(map[string]any)
			kinds = append(kinds, m["kind"].(string))
			if m["kind"] == "job.output" {
				offsets = append(offsets, jsonInt(t, m["output"].(map[string]any)["offset_bytes"]))
			}
		}
		after = r.obj["next_cursor"].(string)
		if r.obj["stopped_by"] == "timeout" {
			break
		}
	}
	// Accepted, dispatch, three ordered output chunks, terminal result; queue
	// replacements accompany the transitions.
	joined := strings.Join(kinds, ",")
	if !strings.HasPrefix(joined, "job.accepted,") || !strings.Contains(joined, ",job.output,job.output,job.output,job.result") {
		t.Fatalf("unexpected event sequence: %v", kinds)
	}
	if fmt.Sprint(offsets) != "[0 2 8]" {
		t.Fatalf("UTF-8 byte offsets: %v", offsets)
	}
}
