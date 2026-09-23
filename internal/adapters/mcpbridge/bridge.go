// Package mcpbridge is a stdio Model Context Protocol server that is only a
// client of an explicitly running TaskWorker service. It never opens the
// journal, constructs a backend or starts serve. Protocol framing, lifecycle,
// negotiation, ping and cancellation belong to the official MCP Go SDK.
package mcpbridge

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"taskworker.local/taskworker/internal/adapters/httpapi"
)

// Bounds. Every one is documented in docs/mcp.md.
const (
	MaxMessageBytes  = 9 << 20   // one inbound JSON-RPC line; commands are <= 8 MiB
	MaxResultBytes   = 256 << 10 // encoded job view or event batch in one tool result
	MaxInlineEvent   = 128 << 10 // larger events are returned as explicit markers
	MaxConcurrent    = 16        // tool calls in flight
	MaxWaiters       = 4         // of those, blocking waits/event reads
	DefaultCallLimit = 30 * time.Second
	pollInterval     = 250 * time.Millisecond
)

// Config carries the injected service client and presentation values.
type Config struct {
	Client      *httpapi.Client
	Server      string        // canonical service URL, reported by service_health only
	Version     string        // product version reported to MCP clients
	CallTimeout time.Duration // per service call; zero selects DefaultCallLimit
	Stderr      io.Writer     // diagnostics only; stdout is protocol-exclusive
}

type bridge struct {
	client  *httpapi.Client
	server  string
	timeout time.Duration
	slots   chan struct{}
	waiters chan struct{}
	logMu   sync.Mutex
	stderr  io.Writer
}

const instructions = `TaskWorker bridge: submit, observe and control jobs in the already running TaskWorker service that humans, the CLI and the web UI also use. The bridge never starts a service, never runs inference itself and reports a missing service as a connection error.
Workflow: (1) submit_job with your own unique idempotency_key and a complete request; keep the exact arguments. The result contains job.id. (2) wait_job with that id (bounded; call again while it reports timed_out) or read_events from a cursor returned by list_jobs. (3) get_job for state and retained output, or read_job_output to page large output by UTF-8 byte offset.
If submit_job ends in a timeout or connection error, acceptance is uncertain: call submit_job again with the identical arguments and the same key to resolve to the original job. Never invent a new key to resolve uncertainty. retry_job and branch_job intentionally create NEW jobs.
Stopping a wait, cancelling a tool call or disconnecting never cancels inference; only cancel_job does. A job that failed, was cancelled or interrupted is a normal successful tool result showing that state and any retained partial output. A fault's retryable flag is advice only and never permission to submit again.`

// Run serves MCP over in/out until the peer closes input or ctx ends.
// EOF/shutdown detaches observation only; accepted inference is untouched.
func Run(ctx context.Context, in io.Reader, out io.Writer, cfg Config) error {
	b := newBridge(cfg)
	srv := b.newServer(cfg.Version)
	b.logf(map[string]any{"level": "info", "msg": "mcp bridge ready", "service": cfg.Server, "note": "service is not contacted until a tool is called"})
	w := newFramer(out)
	note := func(msg string) { b.logf(map[string]any{"level": "warn", "msg": msg}) }
	// The framing layer already bounds lines; the SDK limit is a second guard.
	err := srv.Run(ctx, &mcp.IOTransport{Reader: filterInput(in, w, MaxMessageBytes, note), Writer: w, MaxLineLength: MaxMessageBytes + 4096})
	if err != nil && ctx.Err() == nil {
		b.logf(map[string]any{"level": "error", "msg": "mcp transport ended", "error": err.Error()})
	}
	return err
}

func newBridge(cfg Config) *bridge {
	t := cfg.CallTimeout
	if t <= 0 {
		t = DefaultCallLimit
	}
	return &bridge{client: cfg.Client, server: cfg.Server, timeout: t, slots: make(chan struct{}, MaxConcurrent), waiters: make(chan struct{}, MaxWaiters), stderr: cfg.Stderr}
}

// NewServerForTest exposes the configured server for in-memory protocol tests.
func NewServerForTest(cfg Config) *mcp.Server { return newBridge(cfg).newServer(cfg.Version) }

func (b *bridge) newServer(version string) *mcp.Server {
	if version == "" {
		version = "0.0.0"
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "taskworker", Title: "TaskWorker", Version: version}, &mcp.ServerOptions{
		Instructions: instructions,
		// Only tools are implemented; do not inherit the default logging capability.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	b.register(srv)
	return srv
}

func (b *bridge) logf(v map[string]any) {
	if b.stderr == nil {
		return
	}
	v["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(v)
	if err != nil {
		return
	}
	b.logMu.Lock()
	defer b.logMu.Unlock()
	_, _ = b.stderr.Write(append(line, '\n'))
}
