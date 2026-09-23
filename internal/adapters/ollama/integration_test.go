package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"taskworker.local/taskworker/internal/adapters/journal"
	"taskworker.local/taskworker/internal/core"
)

func worker(t *testing.T, b *Backend) (*core.Worker, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := core.NewWorker(s, b)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return w, dir
}

func waitJob(t *testing.T, ctx context.Context, w *core.Worker, id core.JobID, predicate func(core.Job) bool) core.Job {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := w.GetJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if predicate(j) {
			return j
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
}

func terminalJob(t *testing.T, ctx context.Context, w *core.Worker, id core.JobID) core.Job {
	return waitJob(t, ctx, w, id, func(j core.Job) bool { return core.Terminal(j.State) })
}

func TestCoreJournalTerminalConsistency(t *testing.T) {
	for _, mode := range []string{"success", "failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := defaults()
			f.chat = func(w http.ResponseWriter, r *http.Request, _ chatRequest) {
				io.WriteString(w, partial)
				w.(http.Flusher).Flush()
				switch mode {
				case "success":
					io.WriteString(w, terminal)
				case "failure":
					io.WriteString(w, `{"error":"SECRET backend prompt and credential"}`)
				case "cancel":
					<-r.Context().Done()
				}
			}
			b, _ := f.server(t)
			w, dir := worker(t, b)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			j, err := w.Submit(ctx, core.SubmitCommand{Key: "first", Request: input().Request})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				waitJob(t, ctx, w, j.ID, func(j core.Job) bool { return j.Result.Text == "partial" })
				if _, err := w.Cancel(ctx, j.ID); err != nil {
					t.Fatal(err)
				}
			}
			j = terminalJob(t, ctx, w, j.ID)
			want := core.JobSucceeded
			text := "partial!"
			if mode == "failure" {
				want = core.JobFailed
				text = "partial"
			}
			if mode == "cancel" {
				want = core.JobCancelled
				text = "partial"
			}
			if j.State != want || j.Result.Text != text {
				t.Fatalf("job %+v", j)
			}
			if mode == "success" {
				if j.Error != nil || j.Result.EffectiveOptions != nil || j.Result.Context.ModelCapacitySource == "" || j.Result.Context.Loaded == nil {
					t.Fatal(j)
				}
			} else if j.Error == nil || strings.Contains(j.Error.Message, "SECRET") {
				t.Fatal(j)
			}
			if mode == "failure" && j.Error.Code != core.ErrBackendFailure {
				t.Fatal(j.Error)
			}
			if mode == "cancel" && j.Error.Code != core.ErrCancelled {
				t.Fatal(j.Error)
			}
			if err := w.Shutdown(ctx); err != nil {
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
			if !reflect.DeepEqual(recovered.Snapshot.Jobs[0], j) || recovered.Snapshot.Queue.Active != nil {
				t.Fatal("recovered state differs")
			}
			cursor := core.Cursor{StoreID: recovered.Snapshot.Cursor.StoreID}
			var output strings.Builder
			for cursor.Sequence < recovered.Snapshot.Cursor.Sequence {
				events, err := s.ReadEvents(ctx, cursor, 256)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range events {
					if e.Output != nil {
						if e.Output.OffsetBytes != int64(output.Len()) {
							t.Fatal("offset")
						}
						output.WriteString(e.Output.Text)
					}
					cursor = e.Cursor
				}
			}
			if output.String() != j.Result.Text {
				t.Fatal("event output differs from terminal")
			}
		})
	}
}

func TestCoreRetryBranchAndFreshIsolation(t *testing.T) {
	f := defaults()
	b, _ := f.server(t)
	w, _ := worker(t, b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := w.Submit(ctx, core.SubmitCommand{Key: "parent", Request: input().Request})
	if err != nil {
		t.Fatal(err)
	}
	p = terminalJob(t, ctx, w, p.ID)
	r, err := w.Retry(ctx, core.RetryCommand{Key: "retry", ParentID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	terminalJob(t, ctx, w, r.ID)
	br, err := w.Branch(ctx, core.BranchCommand{Key: "branch", ParentID: p.ID, Request: input().Request})
	if err != nil {
		t.Fatal(err)
	}
	terminalJob(t, ctx, w, br.ID)
	fresh, err := w.Submit(ctx, core.SubmitCommand{Key: "fresh", Request: input().Request})
	if err != nil {
		t.Fatal(err)
	}
	terminalJob(t, ctx, w, fresh.ID)
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls []chatRequest
	for _, r := range f.requests {
		if len(r.Messages) > 0 {
			calls = append(calls, r)
		}
	}
	if len(calls) != 4 || !reflect.DeepEqual(calls[0], calls[1]) || !reflect.DeepEqual(calls[0], calls[3]) || len(calls[2].Messages) != 4 || calls[2].Messages[2].Content != p.Result.Text {
		t.Fatalf("materialized history lost: %+v", calls)
	}
}

func TestContextProvenanceBackwardEncoding(t *testing.T) {
	// Omitting the new optional fields preserves historical canonical JSON, which
	// journal recovery checks byte-for-byte. Existing records need no migration.
	b, err := json.Marshal(core.ContextInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"reserved_output_tokens":0}` {
		t.Fatalf("old encoding changed: %s", b)
	}
}
