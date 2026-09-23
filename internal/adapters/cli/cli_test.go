package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"taskworker.local/taskworker/internal/adapters/cli"
	"taskworker.local/taskworker/internal/adapters/httpapi"
	"taskworker.local/taskworker/internal/adapters/journal"
	"taskworker.local/taskworker/internal/core"
)

type backend struct{}

func (backend) Models(context.Context) ([]core.Model, error) { return []core.Model{{ID: "fake"}}, nil }
func (backend) Generate(ctx context.Context, _ core.InferenceInput, emit func(string) error) (core.Result, error) {
	_ = emit("partial")
	<-ctx.Done()
	return core.Result{}, ctx.Err()
}
func TestCLIJSONExitAndDetachedWait(t *testing.T) {
	store, e := journal.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	w, e := core.NewWorker(store, backend{})
	if e != nil {
		t.Fatal(e)
	}
	defer w.Shutdown(context.Background())
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = httpapi.Server(httpapi.NewHandler(w, ts.Listener.Addr().String()))
	ts.Start()
	defer ts.Close()
	run := func(input string, args ...string) (int, string, string) {
		var out, err bytes.Buffer
		code := cli.RunContext(context.Background(), args, strings.NewReader(input), &out, &err, nil)
		return code, out.String(), err.String()
	}
	input := `{"idempotency_key":"cli-stable","request":{"model":"fake","role":" role ","system_prompt":"system\nline","prompt":"Exact\n多字","options":{"max_output_tokens":8}}}`
	code, out, diag := run(input, "submit", "--server", ts.URL, "--command", "-")
	if code != 0 || !strings.Contains(diag, "cli-stable") {
		t.Fatal(code, out, diag)
	}
	var j core.Job
	if json.Unmarshal([]byte(out), &j) != nil || j.Request.Prompt != "Exact\n多字" {
		t.Fatal(out)
	}
	code, out, _ = run("", "wait", "--server", ts.URL, "--timeout", "20ms", string(j.ID))
	if code != 1 || out != "" {
		t.Fatal(code, out)
	}
	current, _ := w.GetJob(context.Background(), j.ID)
	if current.State != core.JobRunning {
		t.Fatal(current.State)
	}
	code, out, _ = run(input, "submit", "--server", ts.URL, "--command", "-")
	var again core.Job
	json.Unmarshal([]byte(out), &again)
	if code != 0 || again.ID != j.ID {
		t.Fatal(code, out)
	}
	code, out, _ = run("", "cancel", "--server", ts.URL, string(j.ID))
	if code != 0 && code != 5 {
		t.Fatal(code, out)
	}
	code, out, _ = run("", "wait", "--server", ts.URL, "--timeout", "5s", string(j.ID))
	if code != 5 || !strings.Contains(out, "partial") {
		t.Fatal(code, out)
	}
	for _, cmd := range []string{"get", "result"} {
		code, out, _ = run("", cmd, "--server", ts.URL, string(j.ID))
		if code != 5 || !strings.Contains(out, `"cancelled"`) {
			t.Fatal(code, out)
		}
	}
	code, out, _ = run("", "list", "--server", ts.URL)
	if code != 0 || !strings.HasPrefix(out, "[") {
		t.Fatal(code, out)
	}
	code, out, _ = run("", "models", "--server", ts.URL)
	if code != 0 || !strings.Contains(out, "fake") {
		t.Fatal(code, out)
	}
	for _, args := range [][]string{{"wat"}, {"get"}, {"queue", "bad"}, {"list", "--command", "x"}, {"submit", "--command", "-"}, {"list", "--timeout", "-1s"}} {
		code, _, _ := run(`{}`, args...)
		if code != 2 {
			t.Fatal(args, code)
		}
	}
	// mcp is a service-client bridge: closed stdin is an orderly end with no
	// stdout, and stray arguments/flags are usage errors.
	code, out, _ = run("", "mcp", "--server", ts.URL)
	if code != 0 || out != "" {
		t.Fatal(code, out)
	}
	for _, args := range [][]string{{"mcp", "extra"}, {"mcp", "--command", "x"}, {"mcp", "--server", "http://example.com:1"}} {
		code, _, _ := run("", args...)
		if code != 2 {
			t.Fatal(args, code)
		}
	}
	for _, args := range [][]string{{"help"}, {"version"}, {}} {
		code, out, _ := run("", args...)
		if code != 0 || out == "" {
			t.Fatal(args, code)
		}
	}
}
func TestCLINoImplicitServiceAndServeResolution(t *testing.T) {
	ts := httptest.NewServer(nil)
	address := ts.URL
	ts.Close()
	var out, err bytes.Buffer
	code := cli.RunContext(context.Background(), []string{"list", "--server", address, "--timeout", "1s"}, strings.NewReader(""), &out, &err, nil)
	if code != 1 || !strings.Contains(err.String(), "start serve") {
		t.Fatal(code, err.String())
	}
	t.Setenv("TASKWORKER_SERVER", "http://127.0.0.1:4567")
	t.Setenv("TASKWORKER_DATA_DIR", "chosen-data")
	t.Setenv("TASKWORKER_OLLAMA_ENDPOINT", "http://127.0.0.1:11435")
	called := false
	serve := func(_ context.Context, c cli.ServeConfig, _ io.Writer) error {
		called = true
		if c.Server != "http://127.0.0.1:4568" || c.DataDir != "chosen-data" || c.Ollama != "http://127.0.0.1:11435" {
			t.Fatal(c)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	code = cli.RunContext(ctx, []string{"serve", "--server", "http://127.0.0.1:4568"}, nil, &out, &err, serve)
	if code != 0 || !called {
		t.Fatal(code)
	}
}

func TestCLIObservedFailureExit(t *testing.T) {
	store, e := journal.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	w, e := core.NewWorker(store, failingBackend{})
	if e != nil {
		t.Fatal(e)
	}
	defer w.Shutdown(context.Background())
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = httpapi.Server(httpapi.NewHandler(w, ts.Listener.Addr().String()))
	ts.Start()
	defer ts.Close()
	input := `{"idempotency_key":"failure","request":{"model":"fake","prompt":"fail","options":{"max_output_tokens":8}}}`
	var out, err bytes.Buffer
	code := cli.RunContext(context.Background(), []string{"submit", "--server", ts.URL, "--command", "-", "--wait"}, strings.NewReader(input), &out, &err, nil)
	if code != 4 || !strings.Contains(out.String(), `"state":"failed"`) || strings.Contains(out.String(), "SECRET") {
		t.Fatal(code, out.String(), err.String())
	}
}

type failingBackend struct{ backend }

func (failingBackend) Generate(context.Context, core.InferenceInput, func(string) error) (core.Result, error) {
	return core.Result{}, &core.Fault{Code: core.ErrBackendFailure, Message: "SECRET"}
}
