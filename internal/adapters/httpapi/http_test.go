package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"taskworker.local/taskworker/internal/adapters/httpapi"
	"taskworker.local/taskworker/internal/adapters/journal"
	"taskworker.local/taskworker/internal/core"
)

type invocation struct {
	ctx    context.Context
	emit   func(string) error
	finish chan struct{}
}
type backend struct{ calls chan invocation }

func (b *backend) Models(context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "fake"}}, nil
}
func (b *backend) Generate(ctx context.Context, _ core.InferenceInput, emit func(string) error) (core.Result, error) {
	c := invocation{ctx, emit, make(chan struct{})}
	b.calls <- c
	<-c.finish
	return core.Result{}, nil
}
func start(t *testing.T, dir string) (*core.Worker, *backend, *httptest.Server, *httpapi.Client) {
	t.Helper()
	s, e := journal.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	b := &backend{make(chan invocation, 10)}
	w, e := core.NewWorker(s, b)
	if e != nil {
		s.Close()
		t.Fatal(e)
	}
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = httpapi.Server(httpapi.NewHandler(w, ts.Listener.Addr().String()))
	ts.Start()
	c, e := httpapi.NewClient(ts.URL)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		c.Close()
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if e := w.Shutdown(ctx); e != nil {
			t.Error(e)
		}
	})
	return w, b, ts, c
}
func command(key string) core.SubmitCommand {
	return core.SubmitCommand{Key: key, Request: core.Request{Model: "fake", Prompt: "multiline\né世界", Options: core.GenerationOptions{MaxOutputTokens: 8}}}
}
func call(t *testing.T, c *httpapi.Client, method, path string, body, dst any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := c.Call(ctx, method, path, body, dst); e != nil {
		t.Fatal(e)
	}
}
func next(t *testing.T, b *backend) invocation {
	t.Helper()
	select {
	case c := <-b.calls:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("no backend invocation")
		return invocation{}
	}
}
func wait(t *testing.T, c *httpapi.Client, id core.JobID) core.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	j, e := c.Wait(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func TestTwoClientsDisconnectReplayCancelAndLineage(t *testing.T) {
	_, b, ts, c := start(t, t.TempDir())
	other, _ := httpapi.NewClient(ts.URL)
	defer other.Close()
	var initial core.Snapshot
	call(t, c, "GET", "/v1/snapshot", nil, &initial)
	var j core.Job
	call(t, c, "POST", "/v1/submit", command("one"), &j)
	run := next(t, b)
	if e := run.emit("é世界"); e != nil {
		t.Fatal(e)
	}
	timeout, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	_, e := other.Wait(timeout, j.ID)
	cancel()
	if e == nil {
		t.Fatal("wait did not time out")
	}
	select {
	case <-run.ctx.Done():
		t.Fatal("wait cancelled inference")
	default:
	}
	// Replay prefix, disconnect after applying the UTF-8 delta, reconnect at its ID.
	cursor := httpapi.CursorID(initial.Cursor)
	text := ""
	stop := errors.New("stop watch")
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	e = other.Watch(ctx, cursor, func(f httpapi.Frame) error {
		var ev core.Event
		if e := json.Unmarshal(f.Data, &ev); e != nil {
			return e
		}
		cursor = f.ID
		if ev.Output != nil {
			if ev.Output.OffsetBytes != int64(len(text)) {
				t.Fatal("offset")
			}
			text += ev.Output.Text
			return stop
		}
		return nil
	})
	if e != stop {
		t.Fatal(e)
	}
	select {
	case <-run.ctx.Done():
		t.Fatal("watch cancelled inference")
	default:
	}
	if e := run.emit("!"); e != nil {
		t.Fatal(e)
	}
	e = other.Watch(ctx, cursor, func(f httpapi.Frame) error {
		var ev core.Event
		_ = json.Unmarshal(f.Data, &ev)
		cursor = f.ID
		if ev.Output != nil {
			if ev.Output.OffsetBytes != int64(len(text)) {
				t.Fatal("reconnect offset")
			}
			text += ev.Output.Text
			return stop
		}
		return nil
	})
	if e != stop || text != "é世界!" {
		t.Fatalf("%s %v", text, e)
	}
	var got core.Job
	call(t, other, "GET", "/v1/jobs/"+string(j.ID)+"/result", nil, &got)
	if got.Result.Text != text {
		t.Fatal(got)
	}
	call(t, other, "POST", "/v1/jobs/"+string(j.ID)+"/cancel", struct{}{}, &got)
	if got.State != core.JobCancelling {
		t.Fatal(got.State)
	}
	<-run.ctx.Done()
	var second core.Job
	call(t, c, "POST", "/v1/submit", command("two"), &second)
	select {
	case <-b.calls:
		t.Fatal("slot released before backend return")
	case <-time.After(30 * time.Millisecond):
	}
	close(run.finish)
	if got = wait(t, c, j.ID); got.State != core.JobCancelled || got.Result.Text != text {
		t.Fatal(got)
	}
	run2 := next(t, b)
	close(run2.finish)
	wait(t, c, second.ID)
	var q core.QueueState
	call(t, c, "POST", "/v1/queue/pause", struct{}{}, &q)
	var retry, branch core.Job
	call(t, other, "POST", "/v1/retry", core.RetryCommand{Key: "retry", ParentID: j.ID}, &retry)
	call(t, c, "POST", "/v1/branch", core.BranchCommand{Key: "branch", ParentID: j.ID, Request: command("b").Request}, &branch)
	if retry.Lineage.Relation != core.RelationRetry || branch.Lineage.Relation != core.RelationBranch || branch.Instructions.History[1].Content != text {
		t.Fatal("lineage")
	}
	call(t, c, "GET", "/v1/queue", nil, &q)
	if !q.Paused || len(q.Pending) != 2 {
		t.Fatal(q)
	}
	call(t, c, "POST", "/v1/queue/resume", struct{}{}, &q)
	r := next(t, b)
	close(r.finish)
	r = next(t, b)
	close(r.finish)
	wait(t, c, branch.ID)
}
func TestDroppedCreateResponseIdempotencyRestart(t *testing.T) {
	dir := t.TempDir()
	w, _, ts, c := start(t, dir)
	var q core.QueueState
	call(t, c, "POST", "/v1/queue/pause", struct{}{}, &q)
	// A real proxy accepts the upstream response then drops it before replying.
	dropped := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest("POST", ts.URL+"/v1/submit", r.Body)
		req.Header.Set("Content-Type", "application/json")
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Error(e)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		conn, _, _ := rw.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer dropped.Close()
	lost, _ := httpapi.NewClient(dropped.URL)
	defer lost.Close()
	var j core.Job
	if e := lost.Call(context.Background(), "POST", "/v1/submit", command("stable"), &j); e == nil {
		t.Fatal("response not dropped")
	}
	call(t, c, "POST", "/v1/submit", command("stable"), &j)
	var jobs []core.Job
	call(t, c, "GET", "/v1/jobs", nil, &jobs)
	if len(jobs) != 1 {
		t.Fatal(len(jobs))
	}
	ts.Close()
	if e := w.Shutdown(context.Background()); e != nil {
		t.Fatal(e)
	}
	_, _, _, c2 := start(t, dir)
	var same core.Job
	call(t, c2, "POST", "/v1/submit", command("stable"), &same)
	if same.ID != j.ID {
		t.Fatal("duplicate after restart")
	}
	changed := command("stable")
	changed.Request.Prompt = "changed"
	e := c2.Call(context.Background(), "POST", "/v1/submit", changed, &same)
	var f *core.Fault
	if !errors.As(e, &f) || f.Code != core.ErrConflict {
		t.Fatal(e)
	}
}
func TestStrictParsingAndBrowserPolicy(t *testing.T) {
	_, _, ts, c := start(t, t.TempDir())
	var q core.QueueState
	call(t, c, "POST", "/v1/queue/pause", struct{}{}, &q)
	good, _ := json.Marshal(command("safe"))
	cases := []struct {
		name, body, method, media, origin, host string
		status                                  int
	}{
		{"valid", string(good), "POST", "application/json", "", "", 200},
		{"duplicate", `{"idempotency_key":"a","idempotency_key":"b"}`, "POST", "application/json", "", "", 400},
		{"nested_duplicate", strings.Replace(string(good), `"model":"fake"`, `"model":"fake","model":"other"`, 1), "POST", "application/json", "", "", 400},
		{"unknown", `{"unknown":0}`, "POST", "application/json", "", "", 400},
		{"case", strings.Replace(string(good), "idempotency_key", "IDEMPOTENCY_KEY", 1), "POST", "application/json", "", "", 400},
		{"null", `null`, "POST", "application/json", "", "", 400},
		{"nested_null", strings.Replace(string(good), `"model":"fake"`, `"model":null`, 1), "POST", "application/json", "", "", 400},
		{"trailing", string(good) + `{}`, "POST", "application/json", "", "", 400},
		{"utf8", string([]byte{0xff}), "POST", "application/json", "", "", 400},
		{"surrogate", `{"idempotency_key":"\ud800"}`, "POST", "application/json", "", "", 400},
		{"oversized", strings.Repeat(" ", httpapi.MaxBody+1), "POST", "application/json", "", "", 413},
		{"form", string(good), "POST", "application/x-www-form-urlencoded", "", "", 415},
		{"simple", string(good), "POST", "text/plain", "", "", 415},
		{"wrong_method", "", "GET", "", "", "", 405},
		{"hostile", string(good), "POST", "application/json", "https://evil.example", "", 403},
		{"null_origin", string(good), "POST", "application/json", "null", "", 403},
		{"bad_host", string(good), "POST", "application/json", "", "evil.example", 403},
		{"same_origin", string(good), "POST", "application/json", ts.URL, "", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, ts.URL+"/v1/submit", strings.NewReader(tc.body))
			if tc.media != "" {
				req.Header.Set("Content-Type", tc.media)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.host != "" {
				req.Host = tc.host
			}
			resp, e := http.DefaultClient.Do(req)
			if e != nil {
				t.Fatal(e)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("%d %s", resp.StatusCode, b)
			}
		})
	}
	for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/health", nil)
		req.Header[header] = []string{""}
		if header == "Sec-Fetch-Site" {
			req.Header.Set(header, "cross-site")
		}
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatal(header, resp.StatusCode)
		}
	}
	var jobs []core.Job
	call(t, c, "GET", "/v1/jobs", nil, &jobs)
	if len(jobs) != 1 {
		t.Fatal("malformed command accepted", len(jobs))
	}
}
func TestCursorAndSnapshotStream(t *testing.T) {
	_, _, ts, c := start(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop := errors.New("stop")
	var cursor string
	if e := c.Watch(ctx, "", func(f httpapi.Frame) error {
		if f.Kind != "snapshot" {
			t.Fatal(f.Kind)
		}
		cursor = f.ID
		return stop
	}); e != stop {
		t.Fatal(e)
	}
	prefix := strings.Split(cursor, ":")[0]
	for _, s := range []string{"", prefix + ":01", prefix + ":+1", prefix + ":-1", prefix + ":18446744073709551616", strings.Repeat("f", 32) + ":0", prefix + ":999"} {
		t.Run("cursor_"+s, func(t *testing.T) {
			req, _ := http.NewRequest("GET", ts.URL+"/v1/events?after="+strings.ReplaceAll(s, "+", "%2B"), nil)
			resp, e := http.DefaultClient.Do(req)
			if e != nil {
				t.Fatal(e)
			}
			resp.Body.Close()
			if resp.StatusCode != 400 {
				t.Fatal(resp.StatusCode)
			}
		})
	}
	for _, suffix := range []string{"?after=" + cursor + "&after=" + cursor, "?job_id=a", "?after=" + cursor} {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/events"+suffix, nil)
		if suffix == "?after="+cursor {
			req.Header.Set("Last-Event-ID", prefix+":1")
		}
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatal(resp.StatusCode)
		}
	}
	for _, s := range []string{prefix + ":0", prefix + ":18446744073709551615"} {
		p, e := httpapi.ParseCursor(s)
		if e != nil || httpapi.CursorID(p) != s {
			t.Fatal(s, e)
		}
	}
}

// A writer stalled in the kernel is bounded by the per-write deadline and never
// holds the core mutation lock. Synthetic stream isolates this transport bound.
type endless struct {
	core.Service
	closed chan struct{}
	once   sync.Once
}

func (s *endless) Watch(context.Context, core.Cursor) (core.EventStream, error) { return s, nil }
func (s *endless) Next(context.Context) (core.Event, error) {
	return core.Event{Cursor: core.Cursor{StoreID: strings.Repeat("a", 32), Sequence: 1}, Kind: core.EventJobOutput, Output: &core.OutputDelta{Text: strings.Repeat("x", 1<<20)}}, nil
}
func (s *endless) Close() error { s.once.Do(func() { close(s.closed) }); return nil }
func TestStalledSocketWriteClosesObserver(t *testing.T) {
	s := &endless{closed: make(chan struct{})}
	ts := httptest.NewUnstartedServer(nil)
	h := httpapi.NewHandler(s, ts.Listener.Addr().String())
	h.WriteTimeout = 50 * time.Millisecond
	ts.Config = httpapi.Server(h)
	ts.Start()
	defer ts.Close()
	conn, e := net.Dial("tcp", ts.Listener.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(1024)
	}
	fmt.Fprintf(conn, "GET /v1/events?after=%s:0 HTTP/1.1\r\nHost: %s\r\n\r\n", strings.Repeat("a", 32), ts.Listener.Addr())
	select {
	case <-s.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("stalled observer leaked")
	}
}
func TestDecoderUnicodeAndUnknownErrors(t *testing.T) {
	for _, s := range []string{`{"idempotency_key":"\ud83d\ude00"}`, `{"idempotency_key":"slash\/and\\quote\""}`} {
		var c core.SubmitCommand
		if e := httpapi.DecodeCommand([]byte(s), &c); e != nil {
			t.Fatal(e)
		}
	}
	f := httpapi.PublicFault(errors.New("SECRET /path/prompt"))
	b, _ := json.Marshal(f)
	if bytes.Contains(b, []byte("SECRET")) || f.Code != core.ErrInternal {
		t.Fatal(string(b))
	}
	f = httpapi.PublicFault(&core.Fault{Code: core.ErrStorage, Message: "SECRET", Details: map[string]string{"path": "SECRET"}})
	b, _ = json.Marshal(f)
	if bytes.Contains(b, []byte("SECRET")) {
		t.Fatal(string(b))
	}
	for _, s := range []string{"http://evil.example:7433", "http://0.0.0.0:7433", "https://127.0.0.1:7433", "http://127.0.0.1:7433/path", "http://user@127.0.0.1:7433", "http://127.0.0.1:0"} {
		if _, e := httpapi.NewClient(s); e == nil {
			t.Fatal(s)
		}
	}
}

type faultService struct {
	core.Service
	fault *core.Fault
	after bool
}

func (s faultService) Models(context.Context) ([]core.Model, error) { return nil, s.fault }
func (s faultService) Watch(context.Context, core.Cursor) (core.EventStream, error) {
	if !s.after {
		return nil, s.fault
	}
	return faultStream{s.fault}, nil
}

type faultStream struct{ err error }

func (s faultStream) Next(context.Context) (core.Event, error) { return core.Event{}, s.err }
func (s faultStream) Close() error                             { return nil }
func TestFaultMappingsAndSSEErrors(t *testing.T) {
	codes := map[core.ErrorCode]int{core.ErrInvalidRequest: 400, core.ErrNotFound: 404, core.ErrConflict: 409, core.ErrQueueFull: 429, core.ErrLimitExceeded: 413, core.ErrUnsupported: 422, core.ErrBackendUnavailable: 503, core.ErrBackendFailure: 502, core.ErrCancelled: 409, core.ErrInterrupted: 409, core.ErrCursorExpired: 410, core.ErrCursorInvalid: 400, core.ErrSlowConsumer: 429, core.ErrStorage: 503, core.ErrUnavailable: 503, core.ErrInternal: 500}
	for code, status := range codes {
		t.Run(string(code), func(t *testing.T) {
			s := faultService{fault: &core.Fault{Code: code, Message: "SECRET", Retryable: true, Details: map[string]string{"x": "SECRET"}}}
			h := httpapi.NewHandler(s, "127.0.0.1:1234")
			r := httptest.NewRequest("GET", "http://127.0.0.1:1234/v1/models", nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != status || strings.Contains(w.Body.String(), "SECRET") || !strings.Contains(w.Body.String(), `"retryable":true`) {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	for _, after := range []bool{false, true} {
		s := faultService{fault: &core.Fault{Code: core.ErrCursorExpired}, after: after}
		ts := httptest.NewUnstartedServer(nil)
		ts.Config = httpapi.Server(httpapi.NewHandler(s, ts.Listener.Addr().String()))
		ts.Start()
		c, _ := httpapi.NewClient(ts.URL)
		e := c.Watch(context.Background(), strings.Repeat("a", 32)+":0", func(httpapi.Frame) error { t.Fatal("unexpected event"); return nil })
		var f *core.Fault
		if !errors.As(e, &f) || f.Code != core.ErrCursorExpired {
			t.Fatal(e)
		}
		c.Close()
		ts.Close()
	}
}

type gapService struct {
	core.Service
	once sync.Once
}

func (s *gapService) Snapshot(ctx context.Context) (core.Snapshot, error) {
	snapshot, e := s.Service.Snapshot(ctx)
	if e == nil {
		s.once.Do(func() { _, e = s.Service.SetQueuePaused(ctx, true) })
	}
	return snapshot, e
}
func TestSnapshotRegistrationGapAndMatchingLastEventID(t *testing.T) {
	w, _, _, _ := start(t, t.TempDir())
	s := &gapService{Service: w}
	ts := httptest.NewUnstartedServer(nil)
	h := httpapi.NewHandler(s, ts.Listener.Addr().String())
	h.Heartbeat = 10 * time.Millisecond
	ts.Config = httpapi.Server(h)
	ts.Start()
	defer ts.Close()
	c, _ := httpapi.NewClient(ts.URL)
	defer c.Close()
	var ids []string
	stop := errors.New("stop")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	e := c.Watch(ctx, "", func(f httpapi.Frame) error {
		ids = append(ids, f.ID)
		if len(ids) == 1 && f.Kind != "snapshot" {
			t.Fatal(f)
		}
		if len(ids) == 2 {
			var ev core.Event
			json.Unmarshal(f.Data, &ev)
			if ev.Queue == nil || !ev.Queue.Paused {
				t.Fatal(ev)
			}
			return stop
		}
		return nil
	})
	if e != stop || len(ids) != 2 {
		t.Fatal(ids, e)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/events?after="+ids[1], nil)
	req.Header.Set("Last-Event-ID", ids[1])
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	buf := make([]byte, 64)
	n, e := resp.Body.Read(buf)
	if e != nil || !strings.Contains(string(buf[:n]), "connected") {
		t.Fatal(string(buf[:n]), e)
	}
}

type idleService struct{ core.Service }

func (s idleService) Watch(context.Context, core.Cursor) (core.EventStream, error) {
	return idleStream{}, nil
}

type idleStream struct{}

func (idleStream) Next(ctx context.Context) (core.Event, error) {
	<-ctx.Done()
	return core.Event{}, ctx.Err()
}
func (idleStream) Close() error { return nil }
func TestStreamAdmissionAndDisconnectRelease(t *testing.T) {
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = httpapi.Server(httpapi.NewHandler(idleService{}, ts.Listener.Addr().String()))
	ts.Start()
	defer ts.Close()
	var responses []*http.Response
	defer func() {
		for _, r := range responses {
			r.Body.Close()
		}
	}()
	for i := 0; i < 32; i++ {
		r, e := http.Get(ts.URL + "/v1/events?after=" + strings.Repeat("a", 32) + ":0")
		if e != nil {
			t.Fatal(e)
		}
		responses = append(responses, r)
		if r.StatusCode != 200 {
			t.Fatal(r.StatusCode)
		}
	}
	r, e := http.Get(ts.URL + "/v1/health")
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 503 {
		t.Fatal(r.StatusCode)
	}
	responses[0].Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r, e = http.Get(ts.URL + "/v1/health")
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnect did not release admission")
		}
		time.Sleep(time.Millisecond)
	}
}

// IsConnectionFault separates this client's own transport failure from a fault
// the service returned; the service can never forge the client's message.
func TestIsConnectionFaultDistinguishesTransportFromServiceFaults(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	dead, err := httpapi.NewClient("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport := dead.Call(ctx, "GET", "/v1/health", nil, &json.RawMessage{})
	if !httpapi.IsConnectionFault(transport) {
		t.Fatalf("connection failure not recognized: %v", transport)
	}
	_, _, _, c := start(t, t.TempDir())
	var j core.Job
	svc := c.Call(ctx, "GET", "/v1/jobs/"+strings.Repeat("a", 32), nil, &j)
	var f *core.Fault
	if !errors.As(svc, &f) || f.Code != core.ErrNotFound || httpapi.IsConnectionFault(svc) {
		t.Fatalf("service fault misclassified: %v", svc)
	}
	// A service fault claiming to be unavailable is rewritten by PublicFault and
	// never carries the client's connection message.
	if httpapi.IsConnectionFault(&core.Fault{Code: core.ErrUnavailable, Message: "unavailable"}) || httpapi.IsConnectionFault(errors.New("x")) || httpapi.IsConnectionFault(nil) {
		t.Fatal("only the client's own connection fault qualifies")
	}
}

// IsUnreadableResponse marks the client's own "reply cannot be read" faults (the
// only ones that leave a sent create uncertain) without confusing them with
// service faults that merely share a code.
func TestIsUnreadableResponseOnlyForClientGeneratedFaults(t *testing.T) {
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{")
		default:
			w.WriteHeader(500)
			_, _ = io.WriteString(w, strings.Repeat("x", 5000))
		}
	}))
	defer garbage.Close()
	c, err := httpapi.NewClient(garbage.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var m map[string]any
	if e := c.Call(ctx, "GET", "/v1/health", nil, &m); !httpapi.IsUnreadableResponse(e) || httpapi.IsConnectionFault(e) {
		t.Fatalf("undecodable success reply: %v", e)
	}
	if e := c.Call(ctx, "GET", "/v1/other", nil, &m); !httpapi.IsUnreadableResponse(e) {
		t.Fatalf("oversized error body: %v", e)
	}
	// A real service fault with the same code is rewritten and never qualifies.
	forged := &core.Fault{Code: core.ErrLimitExceeded, Message: "limit_exceeded"}
	if httpapi.IsUnreadableResponse(forged) || httpapi.IsUnreadableResponse(nil) || httpapi.IsUnreadableResponse(errors.New("x")) {
		t.Fatal("only the client's own messages qualify")
	}
}
