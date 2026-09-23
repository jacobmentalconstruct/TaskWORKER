package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"taskworker.local/taskworker/internal/adapters/journal"
	"taskworker.local/taskworker/internal/core"
)

type backendFaultCase struct {
	name      string
	err       error
	code      core.ErrorCode
	retryable bool
	message   string
}

func backendFaultCases() []backendFaultCase {
	// The raw bodies exceed the terminal reserve and contain sensitive strings
	// and characters that expand in JSON. None may cross the public boundary.
	secret := strings.Repeat("PRIVATE_BACKEND_SECRET\x00", 8192)
	typed := func(code core.ErrorCode, retry bool) *core.Fault {
		return &core.Fault{Code: code, Retryable: retry, Message: secret, Details: map[string]string{secret: secret}}
	}
	cases := []backendFaultCase{}
	for _, c := range []struct {
		code        core.ErrorCode
		message     string
		allowsRetry bool
	}{
		{core.ErrInvalidRequest, "backend rejected the request", false},
		{core.ErrUnsupported, "backend does not support the requested operation or option", false},
		{core.ErrLimitExceeded, "backend or worker resource limit exceeded", false},
		{core.ErrBackendUnavailable, "backend is unavailable", true},
		{core.ErrBackendFailure, "backend operation failed", true},
	} {
		for _, retry := range []bool{false, true} {
			cases = append(cases, backendFaultCase{fmt.Sprintf("%s/retry_%t", c.code, retry), typed(c.code, retry), c.code, c.allowsRetry && retry, c.message})
		}
		cases = append(cases, backendFaultCase{string(c.code) + "/wrapped", fmt.Errorf("PRIVATE_WRAPPER: %w", typed(c.code, true)), c.code, c.allowsRetry, c.message})
	}
	for _, code := range []core.ErrorCode{core.ErrorCode(secret), core.ErrCancelled, core.ErrInterrupted, core.ErrStorage, core.ErrQueueFull, core.ErrCursorInvalid} {
		name := string(code)
		if len(name) > 64 {
			name = "unknown"
		}
		cases = append(cases, backendFaultCase{name, fmt.Errorf("PRIVATE_WRAPPER: %w", typed(code, true)), core.ErrBackendFailure, false, "backend operation failed"})
	}
	cases = append(cases,
		backendFaultCase{"generic", errors.New(secret), core.ErrBackendFailure, false, "backend operation failed"},
		backendFaultCase{"wrapped_generic", fmt.Errorf("PRIVATE_WRAPPER: %w", errors.New(secret)), core.ErrBackendFailure, false, "backend operation failed"},
		backendFaultCase{"nil_typed", (*core.Fault)(nil), core.ErrBackendFailure, false, "backend operation failed"},
	)
	return cases
}

func assertPublicBackendFault(t *testing.T, got *core.Fault, tc backendFaultCase) {
	t.Helper()
	if got == nil {
		t.Fatal("missing public fault")
	}
	if got.Code != tc.code || got.Retryable != tc.retryable || got.Message != tc.message || got.Details != nil {
		t.Fatalf("public fault = %+v; want code=%s retryable=%t message=%q and no details", got, tc.code, tc.retryable, tc.message)
	}
	b, err := json.Marshal(got)
	if err != nil || len(b) >= 256 || len(got.Message) >= 128 || !utf8.ValidString(got.Message) || strings.Contains(string(b), "PRIVATE_") {
		t.Fatalf("public fault violates sanitation/size contract: bytes=%d err=%v", len(b), err)
	}
}

func TestBackendFaultGeneratePublicAndDurable(t *testing.T) {
	for _, tc := range backendFaultCases() {
		t.Run(tc.name, func(t *testing.T) {
			w, f, ctx, dir := setup(t)
			j := submit(t, w, ctx, "classified")
			c := nextCall(t, ctx, f)
			if err := emit(t, ctx, c, "retained partial text"); err != nil {
				t.Fatal(err)
			}
			c.finish <- finish{err: tc.err}
			got := waitState(t, w, ctx, j.ID, core.JobFailed)
			assertPublicBackendFault(t, got.Error, tc)
			if got.Result.Text != "retained partial text" || got.FinishedAt == nil {
				t.Fatal("terminal text/metadata lost")
			}
			// A backend failure cannot undo acceptance, cause an automatic retry, or
			// allow a later cancellation to change the committed terminal result.
			before, err := w.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			again := submit(t, w, ctx, "classified")
			if again.ID != j.ID || again.State != core.JobFailed {
				t.Fatal("idempotent failed job changed")
			}
			cancelled, err := w.Cancel(ctx, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertPublicBackendFault(t, cancelled.Error, tc)
			after, err := w.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if before.Cursor != after.Cursor || len(after.Jobs) != 1 || after.Queue.Active != nil || len(after.Queue.Pending) != 0 {
				t.Fatal("failure changed queue/receipt semantics")
			}
			if err = w.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			// Read the physical journal too: arbitrary raw backend bodies must never
			// reach storage, even when the typed classification was allowed.
			bytes, err := os.ReadFile(filepath.Join(dir, "journal.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(bytes), "PRIVATE_") {
				t.Fatal("backend error text leaked to journal")
			}
			s, err := journal.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			w2, err := core.NewWorker(s, newFake())
			if err != nil {
				s.Close()
				t.Fatal(err)
			}
			defer w2.Shutdown(ctx)
			reloaded, err := w2.GetJob(ctx, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertPublicBackendFault(t, reloaded.Error, tc)
			if reloaded.State != core.JobFailed || reloaded.Result.Text != got.Result.Text {
				t.Fatal("durable failure changed")
			}
			resubmitted := submit(t, w2, ctx, "classified")
			if resubmitted.ID != j.ID {
				t.Fatal("receipt lost on reopen")
			}
			assertPublicBackendFault(t, resubmitted.Error, tc)
			// Mutating a caller's fault must not alter the authoritative snapshot.
			reloaded.Error.Message = "changed by caller"
			owned, err := w2.GetJob(ctx, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertPublicBackendFault(t, owned.Error, tc)
		})
	}
}

type faultModelsBackend struct {
	*fake
	err error
}

func (b faultModelsBackend) Models(context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "PRIVATE_PARTIAL_MODEL"}}, b.err
}

func TestBackendFaultModelsPublic(t *testing.T) {
	for _, tc := range backendFaultCases() {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testContext(t)
			s, err := journal.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			w, err := core.NewWorker(s, faultModelsBackend{newFake(), tc.err})
			if err != nil {
				s.Close()
				t.Fatal(err)
			}
			defer w.Shutdown(ctx)
			before, err := w.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			models, err := w.Models(ctx)
			var got *core.Fault
			if !errors.As(err, &got) {
				t.Fatalf("expected classified public error, got %v", err)
			}
			assertPublicBackendFault(t, got, tc)
			if models != nil {
				t.Fatal("returned partial model data alongside failure")
			}
			got.Message = "changed by caller"
			_, err = w.Models(ctx)
			if !errors.As(err, &got) {
				t.Fatal("missing repeat fault")
			}
			assertPublicBackendFault(t, got, tc)
			after, err := w.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if before.Cursor != after.Cursor || len(after.Jobs) != 0 {
				t.Fatal("model discovery mutated durable state")
			}
		})
	}
}

func TestBackendFaultFailureReleasesQueue(t *testing.T) {
	w, f, ctx, _ := setup(t)
	a := submit(t, w, ctx, "a")
	ca := nextCall(t, ctx, f)
	b := submit(t, w, ctx, "b")
	ca.finish <- finish{err: &core.Fault{Code: core.ErrBackendUnavailable, Retryable: true}}
	cb := nextCall(t, ctx, f)
	if cb.input.Instructions.Prompt != "b" {
		t.Fatal("FIFO did not advance after failed inference")
	}
	got := waitState(t, w, ctx, a.ID, core.JobFailed)
	assertPublicBackendFault(t, got.Error, backendFaultCase{code: core.ErrBackendUnavailable, retryable: true, message: "backend is unavailable"})
	cb.finish <- finish{}
	waitState(t, w, ctx, b.ID, core.JobSucceeded)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.max != 1 {
		t.Fatal("backend overlapped")
	}
}

func TestBackendFaultLifecyclePrecedence(t *testing.T) {
	for _, mode := range []string{"cancel", "shutdown", "output_failure"} {
		t.Run(mode, func(t *testing.T) {
			w, f, ctx, dir := setup(t)
			j := submit(t, w, ctx, "precedence")
			c := nextCall(t, ctx, f)
			if err := emit(t, ctx, c, "partial"); err != nil {
				t.Fatal(err)
			}
			wantState, wantCode := core.JobCancelled, core.ErrCancelled
			switch mode {
			case "cancel":
				if _, err := w.Cancel(ctx, j.ID); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				wantState, wantCode = core.JobInterrupted, core.ErrInterrupted
				expired, cancel := context.WithCancel(ctx)
				cancel()
				_ = w.Shutdown(expired)
			case "output_failure":
				wantState, wantCode = core.JobFailed, core.ErrBackendFailure
				code(t, emit(t, ctx, c, "\xff"), core.ErrBackendFailure)
			}
			select {
			case <-c.ctx.Done():
			case <-ctx.Done():
				t.Fatal("control did not reach backend")
			}
			c.finish <- finish{err: &core.Fault{Code: core.ErrBackendUnavailable, Message: "PRIVATE_BACKEND_SECRET", Retryable: true}}
			var got core.Job
			if mode == "shutdown" {
				if err := w.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
				var err error
				got, err = w.GetJob(ctx, j.ID)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				got = waitState(t, w, ctx, j.ID, wantState)
				if err := w.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if got.State != wantState || got.Error == nil || got.Error.Code != wantCode || got.Error.Retryable || got.Result.Text != "partial" {
				t.Fatalf("precedence lost: %+v", got)
			}
			s, err := journal.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r, err := s.Recover(ctx)
			if err != nil {
				t.Fatal(err)
			}
			persisted := r.Snapshot.Jobs[0]
			if persisted.State != wantState || persisted.Error.Code != wantCode || persisted.Error.Retryable || persisted.Result.Text != "partial" {
				t.Fatal("precedence not durable")
			}
		})
	}
}
