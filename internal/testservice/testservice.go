// Package testservice runs the real core worker, journal and HTTP handler over
// a deterministic scripted backend. It exists only for tests and verification
// scripts; the taskworker executable never links it.
//
// Prompt directives select backend behavior:
//
//	slow:N      emit N "tickI " chunks 50ms apart, then finish
//	hold        emit "started" then block until cancelled
//	gate:NAME   emit "gated", block until file GateDir/NAME exists, emit "released"
//	big:N       emit N bytes of "x" in 16 KiB chunks, then finish
//	unicode     emit "é", "世界", "🙂" as separate chunks
//	fail        emit "partial" then fail with backend_failure
//	other       emit "reply: " + prompt
package testservice

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"taskworker.local/taskworker/internal/adapters/httpapi"
	"taskworker.local/taskworker/internal/adapters/journal"
	"taskworker.local/taskworker/internal/core"
)

// Backend is the scripted deterministic backend.
type Backend struct {
	GateDir    string
	ModelsDown bool // Models fails with backend_unavailable (service health still works)
}

func (b Backend) Models(context.Context) ([]core.Model, error) {
	if b.ModelsDown {
		return nil, &core.Fault{Code: core.ErrBackendUnavailable, Message: "scripted", Retryable: true}
	}
	n := 32768
	return []core.Model{{ID: "fake:latest", Capabilities: core.Capabilities{TextGeneration: true, Streaming: true, Cancellation: true, ContextRequest: true}, Context: core.ContextInfo{ModelCapacityTokens: &n, ReservedOutputTokens: 0}}}, nil
}

func (b Backend) Generate(ctx context.Context, in core.InferenceInput, emit func(string) error) (core.Result, error) {
	done := core.Result{FinishReason: "stop", EffectiveModel: "fake:latest"}
	send := func(s string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return emit(s)
	}
	sleep := func(d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}
	p := in.Instructions.Prompt
	switch {
	case strings.HasPrefix(p, "slow:"):
		n, _ := strconv.Atoi(strings.TrimPrefix(p, "slow:"))
		for i := 0; i < n; i++ {
			if err := send("tick" + strconv.Itoa(i) + " "); err != nil {
				return core.Result{}, err
			}
			if err := sleep(50 * time.Millisecond); err != nil {
				return core.Result{}, err
			}
		}
	case p == "hold":
		if err := send("started"); err != nil {
			return core.Result{}, err
		}
		<-ctx.Done()
		return core.Result{}, ctx.Err()
	case strings.HasPrefix(p, "gate:"):
		if err := send("gated"); err != nil {
			return core.Result{}, err
		}
		name := filepath.Base(strings.TrimPrefix(p, "gate:"))
		for {
			if _, err := os.Stat(filepath.Join(b.GateDir, name)); err == nil {
				break
			}
			if err := sleep(20 * time.Millisecond); err != nil {
				return core.Result{}, err
			}
		}
		if err := send(" released"); err != nil {
			return core.Result{}, err
		}
	case strings.HasPrefix(p, "big:"):
		n, _ := strconv.Atoi(strings.TrimPrefix(p, "big:"))
		for n > 0 {
			c := min(n, 16<<10)
			if err := send(strings.Repeat("x", c)); err != nil {
				return core.Result{}, err
			}
			n -= c
		}
	case p == "unicode":
		for _, s := range []string{"é", "世界", "🙂"} {
			if err := send(s); err != nil {
				return core.Result{}, err
			}
		}
	case p == "fail":
		if err := send("partial"); err != nil {
			return core.Result{}, err
		}
		return core.Result{}, &core.Fault{Code: core.ErrBackendFailure, Message: "scripted", Retryable: true}
	default:
		if err := send("reply: " + p); err != nil {
			return core.Result{}, err
		}
	}
	return done, nil
}

// Config selects the listen address, journal directory and gate directory.
type Config struct {
	Addr, DataDir, GateDir string
	ModelsDown             bool
}

// Running is a started service. Close performs the same orderly worker
// shutdown as serve.
type Running struct {
	URL    string
	worker *core.Worker
	srv    *http.Server
}

func Start(c Config) (*Running, error) {
	if c.Addr == "" {
		c.Addr = "127.0.0.1:0"
	}
	l, err := net.Listen("tcp", c.Addr)
	if err != nil {
		return nil, err
	}
	store, err := journal.Open(c.DataDir)
	if err != nil {
		l.Close()
		return nil, err
	}
	w, err := core.NewWorker(store, Backend{GateDir: c.GateDir, ModelsDown: c.ModelsDown})
	if err != nil {
		store.Close()
		l.Close()
		return nil, err
	}
	srv := httpapi.Server(httpapi.NewHandler(w, l.Addr().String()))
	go func() { _ = srv.Serve(l) }()
	return &Running{URL: "http://" + l.Addr().String(), worker: w, srv: srv}, nil
}

func (r *Running) Close() error {
	_ = r.srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.worker.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return err
}
