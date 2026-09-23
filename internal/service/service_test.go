package service

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"taskworker.local/taskworker/internal/adapters/httpapi"
	"taskworker.local/taskworker/internal/adapters/journal"
	"taskworker.local/taskworker/internal/core"
)

func address(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := "http://" + l.Addr().String()
	l.Close()
	return a
}
func TestOwnerShutdownRecoveryAndStartupFailures(t *testing.T) {
	dir := t.TempDir()
	config := Config{Server: address(t), DataDir: dir, Ollama: "http://127.0.0.1:1"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, config, io.Discard) }()
	defer cancel()
	c, _ := httpapi.NewClient(config.Server)
	defer c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var health any
		if c.Call(context.Background(), "GET", "/v1/health", nil, &health) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("service did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Listener conflict cannot open/create this alternate directory.
	alternate := filepath.Join(t.TempDir(), "untouched")
	if e := Run(context.Background(), Config{Server: config.Server, DataDir: alternate}, io.Discard); e == nil {
		t.Fatal("listener conflict succeeded")
	}
	if _, e := os.Stat(alternate); !os.IsNotExist(e) {
		t.Fatal("opened store despite listener conflict")
	}
	if e := Run(context.Background(), Config{Server: address(t), DataDir: dir}, io.Discard); e == nil {
		t.Fatal("second owner succeeded")
	}
	var q core.QueueState
	if e := c.Call(context.Background(), "POST", "/v1/queue/pause", struct{}{}, &q); e != nil {
		t.Fatal(e)
	}
	var j core.Job
	command := core.SubmitCommand{Key: "queued", Request: core.Request{Model: "fake", Prompt: "pending", Options: core.GenerationOptions{MaxOutputTokens: 8}}}
	if e := c.Call(context.Background(), "POST", "/v1/submit", command, &j); e != nil {
		t.Fatal(e)
	}
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	store, e := journal.Open(dir)
	if e != nil {
		t.Fatal("owner not released", e)
	}
	recovery, e := store.Recover(context.Background())
	store.Close()
	if e != nil || !recovery.Snapshot.Queue.Paused || len(recovery.Snapshot.Jobs) != 1 || recovery.Snapshot.Jobs[0].State != core.JobQueued {
		t.Fatal(recovery, e)
	}
	bad := t.TempDir()
	if e := os.WriteFile(filepath.Join(bad, "journal.bin"), []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := Run(context.Background(), Config{Server: address(t), DataDir: bad}, io.Discard); e == nil {
		t.Fatal("corrupt journal started")
	}
	// Failed Open must release ownership, allowing repeated inspection attempts.
	if e := Run(context.Background(), Config{Server: address(t), DataDir: bad}, io.Discard); e == nil {
		t.Fatal("corrupt journal started twice")
	}
	if e := Run(context.Background(), Config{Server: address(t), DataDir: t.TempDir(), Ollama: "http://remote.invalid"}, io.Discard); e == nil {
		t.Fatal("remote backend accepted")
	}
}
