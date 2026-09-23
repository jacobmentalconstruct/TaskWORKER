package mcpbridge_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"taskworker.local/taskworker/internal/adapters/httpapi"
	"taskworker.local/taskworker/internal/core"
	"taskworker.local/taskworker/internal/testservice"
)

// These tests drive the real taskworker executable over stdio.

var (
	exeOnce sync.Once
	exeFile string
	exeErr  error
)

func moduleRoot(t testing.TB) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func executable(t testing.TB) string {
	t.Helper()
	if p := os.Getenv("TASKWORKER_EXE"); p != "" {
		return p
	}
	exeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "twexe")
		if err != nil {
			exeErr = err
			return
		}
		exeFile = filepath.Join(dir, "taskworker")
		if runtime.GOOS == "windows" {
			exeFile += ".exe"
		}
		goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
		if runtime.GOOS == "windows" {
			goBin += ".exe"
		}
		cmd := exec.Command(goBin, "build", "-o", exeFile, "./cmd/taskworker")
		cmd.Dir = moduleRoot(t)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=")
		if out, err := cmd.CombinedOutput(); err != nil {
			exeErr = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if exeErr != nil {
		t.Fatal(exeErr)
	}
	return exeFile
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type stdio struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	lines  chan string
	stderr *syncBuf
	mu     sync.Mutex
	all    []string
	done   chan struct{}
	exit   error
}

const (
	metaVersion = "io.modelcontextprotocol/protocolVersion"
	metaCaps    = "io.modelcontextprotocol/clientCapabilities"
	metaInfo    = "io.modelcontextprotocol/clientInfo"
)

func modernMeta() map[string]any {
	return map[string]any{metaVersion: "2026-07-28", metaCaps: map[string]any{}, metaInfo: map[string]any{"name": "raw", "version": "0"}}
}

func startStdio(t *testing.T, server string, args ...string) *stdio {
	t.Helper()
	cmd := exec.Command(executable(t), append([]string{"mcp", "--server", server}, args...)...)
	// Keep the bridge away from any real data directory or proxy settings.
	cmd.Env = append(os.Environ(), "TASKWORKER_DATA_DIR="+filepath.Join(t.TempDir(), "must-not-be-created"), "HTTP_PROXY=http://127.0.0.1:1", "http_proxy=http://127.0.0.1:1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	s := &stdio{t: t, cmd: cmd, in: in, lines: make(chan string, 1024), stderr: &syncBuf{}, done: make(chan struct{})}
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		r := bufio.NewReaderSize(out, 1<<20)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				s.mu.Lock()
				s.all = append(s.all, line)
				s.mu.Unlock()
				s.lines <- strings.TrimRight(line, "\r\n")
			}
			if err != nil {
				close(s.lines)
				return
			}
		}
	}()
	go func() { s.exit = cmd.Wait(); close(s.done) }()
	t.Cleanup(func() {
		_ = in.Close()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-s.done
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, l := range s.all {
			// Stdout purity: every byte is exactly one JSON-RPC 2.0 message per line.
			var m map[string]any
			if !strings.HasSuffix(l, "\n") || strings.Contains(strings.TrimSuffix(l, "\n"), "\n") || json.Unmarshal([]byte(l), &m) != nil || m["jsonrpc"] != "2.0" {
				t.Errorf("stdout carried a non-protocol line: %q", l)
			}
		}
	})
	return s
}

func (s *stdio) send(v any) {
	s.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		s.t.Fatal(err)
	}
	s.sendLine(string(b))
}

func (s *stdio) sendLine(l string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, l+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

func (s *stdio) read(d time.Duration) (map[string]any, bool) {
	s.t.Helper()
	select {
	case l, ok := <-s.lines:
		if !ok {
			return nil, false
		}
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(l))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			s.t.Fatalf("stdout not JSON: %q", l)
		}
		return m, true
	case <-time.After(d):
		return nil, false
	}
}

func (s *stdio) expect(id int) map[string]any {
	s.t.Helper()
	m, ok := s.read(20 * time.Second)
	if !ok {
		s.t.Fatalf("no response for %d; stderr: %s", id, s.stderr.String())
	}
	if n, _ := m["id"].(json.Number); n.String() != fmt.Sprint(id) {
		s.t.Fatalf("expected id %d, got %v", id, m)
	}
	return m
}

func (s *stdio) waitExit(d time.Duration) error {
	s.t.Helper()
	select {
	case <-s.done:
		return s.exit
	case <-time.After(d):
		s.t.Fatal("bridge did not exit")
		return nil
	}
}

func modernCall(id int, name string, args map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"_meta": modernMeta(), "name": name, "arguments": args}}
}

func toolResult(t *testing.T, m map[string]any) (map[string]any, bool) {
	t.Helper()
	res, _ := m["result"].(map[string]any)
	if res == nil {
		t.Fatalf("not a result: %v", m)
	}
	content := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return parse(text), isErr
}

func liveService(t *testing.T) (*testservice.Running, *httpapi.Client) {
	t.Helper()
	svc, _ := startService(t, testservice.Config{GateDir: t.TempDir()})
	c, err := httpapi.NewClient(svc.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return svc, c
}

// alive round-trips a cheap modern request (ping was removed in 2026-07-28).
func (s *stdio) alive(id int) {
	s.t.Helper()
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "server/discover", "params": map[string]any{"_meta": modernMeta()}})
	if m := s.expect(id); m["error"] != nil {
		s.t.Fatalf("%v", m)
	}
}

func TestStdioModernLifecycle(t *testing.T) {
	svc, _ := liveService(t)
	s := startStdio(t, svc.URL)
	s.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "server/discover", "params": map[string]any{"_meta": modernMeta()}})
	d := s.expect(1)["result"].(map[string]any)
	versions, _ := json.Marshal(d["supportedVersions"])
	if !strings.Contains(string(versions), "2026-07-28") || !strings.Contains(string(versions), "2025-11-25") {
		t.Fatalf("versions %s", versions)
	}
	caps := d["capabilities"].(map[string]any)
	if len(caps) != 1 || caps["tools"] == nil {
		t.Fatalf("only implemented capabilities may be advertised: %v", caps)
	}
	// ping was removed from the stateless revision; it exists for legacy sessions.
	s.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping", "params": map[string]any{"_meta": modernMeta()}})
	if s.expect(2)["error"] == nil {
		t.Fatal("ping is not part of protocol 2026-07-28")
	}
	s.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list", "params": map[string]any{"_meta": modernMeta()}})
	if n := len(s.expect(3)["result"].(map[string]any)["tools"].([]any)); n != 14 {
		t.Fatalf("tools: %d", n)
	}
	s.send(modernCall(4, "service_health", nil))
	body, isErr := toolResult(t, s.expect(4))
	if isErr || body["status"] != "serving" {
		t.Fatal(body)
	}
	// Unknown method: protocol error, and the session keeps working.
	s.send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "no/such", "params": map[string]any{"_meta": modernMeta()}})
	if e := s.expect(5)["error"].(map[string]any); e["code"].(json.Number).String() != "-32601" {
		t.Fatalf("%v", e)
	}
	// Unknown tool is a protocol error, distinct from a tool-operation fault.
	s.send(modernCall(6, "nope", nil))
	if s.expect(6)["error"] == nil {
		t.Fatal("unknown tool must be a JSON-RPC error")
	}
	// Notifications never get a response.
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/unknown", "params": map[string]any{}})
	_ = s.in.Close()
	if err := s.waitExit(5 * time.Second); err != nil {
		t.Fatalf("EOF must be a clean exit: %v; stderr %s", err, s.stderr.String())
	}
	if !strings.Contains(s.stderr.String(), "mcp bridge ready") {
		t.Fatalf("diagnostics belong on stderr: %q", s.stderr.String())
	}
}

func TestStdioLegacyInitializeHandshake(t *testing.T) {
	svc, _ := liveService(t)
	s := startStdio(t, svc.URL)
	init := func(id int, version string) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": id, "method": "initialize", "params": map[string]any{"protocolVersion": version, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "legacy", "version": "0"}}}
	}
	// Requests before the handshake completes are rejected.
	s.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if s.expect(1)["error"] == nil {
		t.Fatal("tools/list before initialization must fail")
	}
	s.send(init(2, "2025-06-18"))
	r := s.expect(2)["result"].(map[string]any)
	if r["protocolVersion"] != "2025-06-18" {
		t.Fatalf("negotiated %v", r["protocolVersion"])
	}
	if caps := r["capabilities"].(map[string]any); len(caps) != 1 || caps["tools"] == nil {
		t.Fatalf("capabilities %v", caps)
	}
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	s.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "ping"})
	s.expect(3)
	s.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/list"})
	if n := len(s.expect(4)["result"].(map[string]any)["tools"].([]any)); n != 14 {
		t.Fatal(n)
	}
	s.send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": map[string]any{"name": "queue_status", "arguments": map[string]any{}}})
	if body, isErr := toolResult(t, s.expect(5)); isErr || body["max_pending"] == nil {
		t.Fatal(body)
	}
	_ = s.in.Close()
	if err := s.waitExit(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	// A future client version negotiates down to a version the bridge supports.
	s2 := startStdio(t, svc.URL)
	s2.send(init(1, "2099-01-01"))
	got := s2.expect(1)["result"].(map[string]any)["protocolVersion"]
	if got != "2025-11-25" {
		t.Fatalf("future version negotiated %v", got)
	}
}

func TestStdioUnsupportedProtocolVersion(t *testing.T) {
	svc, _ := liveService(t)
	s := startStdio(t, svc.URL)
	bad := modernMeta()
	bad[metaVersion] = "2099-01-01"
	s.send(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "server/discover", "params": map[string]any{"_meta": bad}})
	e, _ := s.expect(7)["error"].(map[string]any)
	if e == nil || e["code"].(json.Number).String() != "-32022" {
		t.Fatalf("expected UnsupportedProtocolVersionError: %v", e)
	}
	data, _ := json.Marshal(e["data"])
	if !strings.Contains(string(data), "2026-07-28") || !strings.Contains(string(data), `"requested":"2099-01-01"`) {
		t.Fatalf("error must list supported versions: %s", data)
	}
	// The session survives and still serves the supported version.
	s.alive(8)
}

func TestStdioMalformedAndOversizedLinesGetErrorsAndSessionContinues(t *testing.T) {
	svc, _ := liveService(t)
	s := startStdio(t, svc.URL)
	ping := s.alive
	expectRPCError := func(code string) {
		t.Helper()
		m, ok := s.read(10 * time.Second)
		if !ok {
			t.Fatal("no error reply; stderr: " + s.stderr.String())
		}
		e, _ := m["error"].(map[string]any)
		if e == nil || e["code"].(json.Number).String() != code || m["id"] != nil {
			t.Fatalf("want %s with null id: %v", code, m)
		}
	}
	ping(1)
	// Truncated JSON on one line must not swallow the next line.
	s.sendLine(`{"jsonrpc":"2.0","id":2,"method":"ping","params":`)
	expectRPCError("-32700")
	ping(3)
	s.sendLine(`this is not json`)
	expectRPCError("-32700")
	s.sendLine(`42`)
	expectRPCError("-32600")
	s.sendLine("") // blank lines are ignored
	ping(4)
	// One line over the bound is discarded whole and reported.
	huge := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"submit_job","arguments":{"junk":"` + strings.Repeat("a", 10<<20) + `"}}}`
	go func() { _, _ = io.WriteString(s.in, huge+"\n") }()
	expectRPCError("-32600")
	ping(6)
	// A JSON-RPC batch is not required by any supported revision but is valid JSON: it is the SDK's to answer.
	_ = s.in.Close()
	if err := s.waitExit(10 * time.Second); err != nil {
		t.Fatalf("clean EOF expected after recoverable framing errors: %v", err)
	}
	if !strings.Contains(s.stderr.String(), "not valid JSON") || !strings.Contains(s.stderr.String(), "size bound") {
		t.Fatalf("diagnostics belong on stderr: %s", s.stderr.String())
	}
}

// Valid JSON that is not a JSON-RPC message used to end the session
// (exit 1, no reply) because the SDK treats an undecodable envelope as fatal.
// Ids the SDK cannot return exactly used to be answered with a
// different number. Both are now an Invalid Request with a null id, and the
// session keeps serving.
func TestStdioInvalidEnvelopesGetErrorsAndSessionContinues(t *testing.T) {
	svc, _ := liveService(t)
	s := startStdio(t, svc.URL)
	meta, _ := json.Marshal(modernMeta())
	s.alive(1)
	invalid := []string{
		`{}`,
		`{"jsonrpc":"2.0"}`,
		`{"jsonrpc":"2.0","id":null}`,
		`{"jsonrpc":"2.0","id":null,"result":{}}`,
		`{"jsonrpc":"1.0","id":2,"method":"server/discover"}`,
		`{"id":2,"method":"server/discover"}`,
		`{"jsonrpc":"2.0","id":2,"method":7}`,
		`[]`,
		`[{}]`,
		`{"jsonrpc":"2.0","id":{"a":1},"method":"server/discover"}`,
		`{"jsonrpc":"2.0","id":true,"method":"server/discover"}`,
		`{"jsonrpc":"2.0","id":1.5,"method":"server/discover","params":{"_meta":` + string(meta) + `}}`,
		`{"jsonrpc":"2.0","id":9007199254740993,"method":"server/discover","params":{"_meta":` + string(meta) + `}}`,
		`{"jsonrpc":"2.0","id":9223372036854775808,"method":"server/discover","params":{"_meta":` + string(meta) + `}}`,
		`{"jsonrpc":"2.0","id":123456789012345678901234567890,"method":"server/discover","params":{"_meta":` + string(meta) + `}}`,
		`{"jsonrpc":"2.0","id":13,"method":"server/discover","params":` + strings.Repeat("[", 5000) + strings.Repeat("]", 5000) + `}`,
	}
	for n, line := range invalid {
		s.sendLine(line)
		m, ok := s.read(10 * time.Second)
		if !ok {
			t.Fatalf("case %d (%.80s): no reply, the session ended; stderr: %s", n, line, s.stderr.String())
		}
		e, _ := m["error"].(map[string]any)
		if e == nil || e["code"].(json.Number).String() != "-32600" || m["id"] != nil {
			t.Fatalf("case %d (%.80s): want -32600 with a null id: %v", n, line, m)
		}
		s.alive(100 + n) // the session still answers
	}
	// Ids at the edge of the exactly-representable range, and string ids, come back unchanged.
	for _, id := range []string{`9007199254740991`, `-9007199254740991`, `0`, `"9007199254740993"`, `"abc"`} {
		s.sendLine(`{"jsonrpc":"2.0","id":` + id + `,"method":"server/discover","params":{"_meta":` + string(meta) + `}}`)
		m, ok := s.read(10 * time.Second)
		if !ok {
			t.Fatalf("id %s: no reply; stderr: %s", id, s.stderr.String())
		}
		got, _ := json.Marshal(m["id"])
		if string(got) != id || m["error"] != nil {
			t.Fatalf("id %s was answered as %s: %v", id, got, m)
		}
	}
	_ = s.in.Close()
	if err := s.waitExit(10 * time.Second); err != nil {
		t.Fatalf("clean EOF expected after recoverable envelope errors: %v", err)
	}
	if !strings.Contains(s.stderr.String(), "JSON-RPC message the bridge can pass on") {
		t.Fatalf("diagnostics belong on stderr: %s", s.stderr.String())
	}
}

func TestStdioCancellationAndEOFDetachOnly(t *testing.T) {
	svc, hc := liveService(t)
	ctx := context.Background()
	s := startStdio(t, svc.URL)
	s.send(modernCall(1, "submit_job", map[string]any{"idempotency_key": "stdio-hold", "request": req("hold")}))
	sub, isErr := toolResult(t, s.expect(1))
	if isErr {
		t.Fatal(sub)
	}
	id := sub["job"].(map[string]any)["id"].(string)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := hc.Get(ctx, core.JobID(id))
		if j.Result.Text == "started" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A blocked wait must not hold up ping or explicit control calls.
	s.send(modernCall(10, "wait_job", map[string]any{"job_id": id, "timeout_seconds": 120}))
	time.Sleep(300 * time.Millisecond)
	s.alive(11)
	s.send(modernCall(12, "queue_status", nil))
	s.expect(12)
	// MCP cancellation stops only that call: no response for id 10, job unaffected.
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 10, "reason": "test"}})
	s.send(map[string]any{"jsonrpc": "2.0", "id": 13, "method": "server/discover", "params": map[string]any{"_meta": modernMeta()}})
	if m, _ := s.read(10 * time.Second); m["id"].(json.Number).String() != "13" {
		t.Fatalf("a cancelled call must not respond: %v", m)
	}
	if j, _ := hc.Get(ctx, core.JobID(id)); j.State != core.JobRunning {
		t.Fatalf("cancelling an MCP call cancelled inference: %s", j.State)
	}
	// stdio EOF during another wait: prompt clean exit, job still running.
	s.send(modernCall(14, "wait_job", map[string]any{"job_id": id, "timeout_seconds": 120}))
	time.Sleep(300 * time.Millisecond)
	_ = s.in.Close()
	if err := s.waitExit(10 * time.Second); err != nil {
		t.Fatalf("EOF during a wait must exit cleanly: %v", err)
	}
	if j, _ := hc.Get(ctx, core.JobID(id)); j.State != core.JobRunning || j.Result.Text != "started" {
		t.Fatalf("bridge exit changed inference: %+v", j)
	}
	// Only the explicit tool stops it.
	s2 := startStdio(t, svc.URL)
	s2.send(modernCall(1, "cancel_job", map[string]any{"job_id": id}))
	if _, isErr := toolResult(t, s2.expect(1)); isErr {
		t.Fatal("cancel failed")
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := hc.Get(ctx, core.JobID(id)); j.State == core.JobCancelled {
			if j.Result.Text != "started" {
				t.Fatal("partial output lost")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("explicit cancel did not finish")
}

func TestStdioMissingServiceNeverStartsAWorker(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	s := startStdio(t, "http://"+addr)
	s.send(modernCall(1, "submit_job", map[string]any{"idempotency_key": "down", "request": req("x")}))
	body, isErr := toolResult(t, s.expect(1))
	if !isErr || body["acceptance_uncertain"] != true || body["error"].(map[string]any)["kind"] != "service_unreachable" {
		t.Fatal(body)
	}
	s.alive(2)
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("something is now listening: the bridge must never start serve")
	}
	for _, e := range s.cmd.Env {
		if strings.HasPrefix(e, "TASKWORKER_DATA_DIR=") {
			if _, err := os.Stat(strings.TrimPrefix(e, "TASKWORKER_DATA_DIR=")); err == nil {
				t.Fatal("the bridge created a data directory")
			}
		}
	}
	_ = s.in.Close()
	_ = s.waitExit(5 * time.Second)
}

func TestStdioBridgeRestartRecoversSameJob(t *testing.T) {
	svc, hc := liveService(t)
	args := map[string]any{"idempotency_key": "restart-key", "request": req("hold")}
	a := startStdio(t, svc.URL)
	a.send(modernCall(1, "submit_job", args))
	first, _ := toolResult(t, a.expect(1))
	id := first["job"].(map[string]any)["id"].(string)
	_ = a.cmd.Process.Kill() // agent/bridge crash
	<-a.done
	b := startStdio(t, svc.URL)
	b.send(modernCall(1, "submit_job", args))
	again, isErr := toolResult(t, b.expect(1))
	if isErr || again["job"].(map[string]any)["id"] != id {
		t.Fatalf("resend must resolve to the original job: %v", again)
	}
	b.send(modernCall(2, "submit_job", map[string]any{"idempotency_key": "restart-key", "request": req("changed")}))
	if body, isErr := toolResult(t, b.expect(2)); !isErr || body["error"].(map[string]any)["code"] != "conflict" {
		t.Fatal(body)
	}
	jobs, _ := hc.Get(context.Background(), core.JobID(id))
	if jobs.State != core.JobRunning && jobs.State != core.JobQueued {
		t.Fatalf("state %s", jobs.State)
	}
	if snap := func() int {
		var s core.Snapshot
		_ = hc.Call(context.Background(), "GET", "/v1/snapshot", nil, &s)
		return len(s.Jobs)
	}(); snap != 1 {
		t.Fatalf("expected exactly one job, got %d", snap)
	}
	b.send(modernCall(3, "cancel_job", map[string]any{"job_id": id}))
	b.expect(3)
}

func TestStdioViaOfficialGoClient(t *testing.T) {
	svc, hc := liveService(t)
	cmd := exec.Command(executable(t), "mcp", "--server", svc.URL)
	cl := mcp.NewClient(&mcp.Implementation{Name: "go-sdk-client", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cs, err := cl.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 14 {
		t.Fatal(err, len(tools.Tools))
	}
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "submit_job", Arguments: map[string]any{"idempotency_key": "sdk-k", "request": req("via sdk")}})
	if err != nil || r.IsError {
		t.Fatal(err, r)
	}
	id := parse(r.Content[0].(*mcp.TextContent).Text)["job"].(map[string]any)["id"].(string)
	w, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "wait_job", Arguments: map[string]any{"job_id": id, "timeout_seconds": 10}})
	if err != nil || w.IsError {
		t.Fatal(err, w)
	}
	j, _ := hc.Get(ctx, core.JobID(id))
	if j.State != core.JobSucceeded || j.Result.Text != "reply: via sdk" {
		t.Fatalf("%+v", j)
	}
	if err := cs.Ping(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestExecutableUsageAndStdoutPurity(t *testing.T) {
	exe := executable(t)
	for _, args := range [][]string{{"mcp", "--bogus"}, {"mcp", "extra"}, {"mcp", "--server", "http://example.com:1"}} {
		cmd := exec.Command(exe, args...)
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = &so, &se
		cmd.Stdin = strings.NewReader("")
		err := cmd.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 || so.Len() != 0 || se.Len() == 0 {
			t.Fatalf("%v: err=%v stdout=%q stderr=%q", args, err, so.String(), se.String())
		}
	}
	// help documents the command without going through the bridge.
	out, err := exec.Command(exe, "help").Output()
	if err != nil || !strings.Contains(string(out), "mcp") {
		t.Fatal(err, string(out))
	}
	// The bridge itself never prints help or banners to stdout.
	cmd := exec.Command(exe, "mcp", "--server", "http://127.0.0.1:1")
	cmd.Stdin = strings.NewReader("")
	so, err := cmd.Output()
	if err != nil || len(so) != 0 {
		t.Fatalf("stdout must be empty on EOF: %q %v", so, err)
	}
}
