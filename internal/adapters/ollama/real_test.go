package ollama

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"taskworker.local/taskworker/internal/adapters/journal"
	"taskworker.local/taskworker/internal/core"
)

// Explicit opt-in only. Never pulls, unloads, changes daemon settings, or logs
// arbitrary daemon error bodies. See docs/ollama.md for reproducible commands.
func TestRealInstalledModel(t *testing.T) {
	if os.Getenv("TASKWORKER_OLLAMA_REAL") != "1" {
		t.Skip("set TASKWORKER_OLLAMA_REAL=1 to use an installed local model")
	}
	id := os.Getenv("TASKWORKER_OLLAMA_MODEL")
	if id == "" {
		id = "qwen2.5:0.5b"
	}
	b, err := New(Config{Endpoint: os.Getenv("TASKWORKER_OLLAMA_ENDPOINT"), Timeout: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer b.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	models, err := b.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w, dir := worker(t, b)
	evidence := map[string]any{"observed_at": time.Now().UTC(), "version": supportedVersion, "installed_count": len(models), "model": id, "discovery": models}
	var jobs []core.Job
	for i, prompt := range []string{"Reply exactly FIRST_ISOLATED.", "Reply exactly SECOND_ISOLATED."} {
		n := 4096
		if i == 1 {
			n = 2048
		}
		temp := 0.0
		seed := int64(7)
		r := core.Request{Model: id, SystemPrompt: "Give a short direct answer.", Prompt: prompt, Options: core.GenerationOptions{ContextTokens: &n, MaxOutputTokens: 64, Temperature: &temp, Seed: &seed}}
		j, err := w.Submit(ctx, core.SubmitCommand{Key: prompt, Request: r})
		if err != nil {
			t.Fatal(err)
		}
		j = terminalJob(t, ctx, w, j.ID)
		if j.State != core.JobSucceeded || j.Result.Text == "" || len(j.Instructions.History) != 0 || j.Result.Context.Loaded == nil || j.Result.Context.Loaded.Tokens != n || j.Result.Usage.InputTokens == nil || j.Result.Context.ModelCapacityObservedAt == nil {
			t.Fatalf("real result: %+v", j)
		}
		jobs = append(jobs, j)
	}
	// Exercise the server guard directly as well as the adapter's heuristic
	// admission tests. This input exceeds the tiny real allocation after rendering.
	_, err = b.stream(ctx, chatRequest{Model: id + ":local", Messages: []message{{Role: "system"}, {Role: "user", Content: strings.Repeat("oversized input ", 2048)}}, Stream: true, Options: map[string]any{"num_ctx": 512, "num_predict": 1}}, id, func(string) error { t.Error("oversized input emitted output"); return nil })
	requireCode(t, err, core.ErrInvalidRequest)
	evidence["server_truncate_false_guard"] = "invalid_request; zero callbacks"
	n := 4096
	r := core.Request{Model: id, Prompt: "List every integer from 1 to 100000, one per line. Continue until all are listed.", Options: core.GenerationOptions{ContextTokens: &n, MaxOutputTokens: 2048}}
	j, err := w.Submit(ctx, core.SubmitCommand{Key: "cancel", Request: r})
	if err != nil {
		t.Fatal(err)
	}
	before := waitJob(t, ctx, w, j.ID, func(j core.Job) bool { return j.Result.Text != "" || core.Terminal(j.State) })
	if core.Terminal(before.State) {
		t.Fatal("generation ended before controlled cancellation")
	}
	start := time.Now()
	if _, err = w.Cancel(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	j = terminalJob(t, ctx, w, j.ID)
	if j.State != core.JobCancelled || j.Result.Text == "" {
		t.Fatalf("cancellation: %+v", j)
	}
	evidence["cancellation_to_terminal_ms"] = time.Since(start).Milliseconds()
	jobs = append(jobs, j)
	evidence["server_stop_observation"] = "client request cancelled, adapter returned, worker durably cancelled; no server token counter or per-sequence stop acknowledgement is exposed"
	// Subsequent work tests adapter/core slot release without unloading the model.
	r.Prompt = "Reply exactly READY."
	r.Options.MaxOutputTokens = 16
	j, err = w.Submit(ctx, core.SubmitCommand{Key: "after_cancel", Request: r})
	if err != nil {
		t.Fatal(err)
	}
	j = terminalJob(t, ctx, w, j.ID)
	if j.State != core.JobSucceeded {
		t.Fatalf("post-cancellation inference: %+v", j)
	}
	jobs = append(jobs, j)
	if err = w.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recovered, err := s.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Snapshot.Jobs) != len(jobs) {
		t.Fatal("real journal count")
	}
	for i, j := range jobs {
		if recovered.Snapshot.Jobs[i].Result.Text != j.Result.Text || recovered.Snapshot.Jobs[i].State != j.State {
			t.Fatal("real journal mismatch")
		}
	}
	evidence["jobs"] = jobs
	evidence["journal_reopen"] = "all four terminal states and output match"
	encoded, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
	if path := os.Getenv("TASKWORKER_OLLAMA_EVIDENCE"); path != "" {
		if err = os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
