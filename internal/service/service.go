// Package service wires the sole durable worker owner. Clients do not import it.
package service

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"taskworker.local/taskworker/internal/adapters/httpapi"
	"taskworker.local/taskworker/internal/adapters/journal"
	"taskworker.local/taskworker/internal/adapters/ollama"
	"taskworker.local/taskworker/internal/core"
)

type Config struct{ Server, DataDir, Ollama string }

// Run acquires the listener before opening storage or dispatching recovered jobs.
// On shutdown, deadlines stop waiting, never close the owned store prematurely.
func Run(ctx context.Context, c Config, log io.Writer) error {
	u, e := httpapi.Endpoint(c.Server)
	if e != nil {
		return e
	}
	b, e := ollama.New(ollama.Config{Endpoint: c.Ollama})
	if e != nil {
		return e
	}
	defer b.CloseIdleConnections()
	listener, e := net.Listen("tcp", u.Host)
	if e != nil {
		return &core.Fault{Code: core.ErrUnavailable, Message: "cannot listen; address may already be in use"}
	}
	defer listener.Close()
	store, e := journal.Open(c.DataDir)
	if e != nil {
		return &core.Fault{Code: core.ErrStorage, Message: "cannot open data directory; check ownership, access and journal integrity"}
	}
	worker, e := core.NewWorker(store, b)
	if e != nil {
		store.Close()
		return httpapi.PublicFault(e)
	}
	srv := httpapi.Server(httpapi.NewHandler(worker, u.Host))
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.BaseContext = func(net.Listener) context.Context { return base }
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	_, _ = io.WriteString(log, "service ready at "+u.String()+"\n")
	select {
	case <-ctx.Done():
	case e = <-done:
		if errors.Is(e, http.ErrServerClosed) {
			e = nil
		}
	}
	cancel()
	_ = srv.Close()
	stop, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := worker.Shutdown(stop)
	stopCancel()
	if errors.Is(err, context.DeadlineExceeded) {
		_, _ = io.WriteString(log, "shutdown pending; retaining store ownership until backend returns\n")
		err = worker.Shutdown(context.Background())
	}
	if err != nil {
		return httpapi.PublicFault(err)
	}
	if e != nil {
		return httpapi.PublicFault(e)
	}
	return nil
}
