// Package cli presents service clients. The serve callback is injected by main.
package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"taskworker.local/taskworker/internal/adapters/httpapi"
	"taskworker.local/taskworker/internal/adapters/mcpbridge"
	"taskworker.local/taskworker/internal/core"
)

const Version = "0.1.0"
const usage = `TaskWorker — shared local inference worker
Usage: taskworker COMMAND [flags] [arguments]
  serve [--server URL] [--data-dir DIR] [--ollama URL]
  submit|retry|branch --command FILE [--wait] [--timeout 30s]
  wait ID [flags must precede ID]; watch [--after STORE:SEQUENCE]
  list; get ID; result ID; cancel ID; models; health
  queue status|pause|resume
  mcp [--server URL] [--timeout DURATION]   MCP stdio bridge to the running service
Common client flags: --server URL --timeout DURATION (0 = no total deadline)
Create commands require a complete JSON command file, including idempotency_key.
Use --command - for stdin. Keep the exact original command/key for retries.
All client output is JSON; diagnostics are JSON on stderr. watch emits NDJSON.
See docs/http.md for command schemas, reconnect, security and exit codes.
The mcp command speaks MCP on stdout only; see docs/mcp.md. It never starts a service.
`

type ServeConfig struct{ Server, DataDir, Ollama string }
type ServeFunc func(context.Context, ServeConfig, io.Writer) error

func env(name, fallback string) string {
	if s := os.Getenv(name); s != "" {
		return s
	}
	return fallback
}
func Run(args []string, stdout, stderr io.Writer) int {
	return RunContext(context.Background(), args, os.Stdin, stdout, stderr, nil)
}
func RunContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, serve ServeFunc) int {
	emitError := func(e error, code int) int { _ = json.NewEncoder(stderr).Encode(e); return code }
	usageError := func() int {
		return emitError(&core.Fault{Code: core.ErrInvalidRequest, Message: "invalid arguments; run taskworker help"}, 2)
	}
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")) {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(stdout, "taskworker %s\n", Version)
		return 0
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	address := fs.String("server", env("TASKWORKER_SERVER", "http://127.0.0.1:7433"), "service endpoint")
	timeoutDefault := 30 * time.Second
	if cmd == "wait" || cmd == "watch" {
		timeoutDefault = 0
	}
	timeout := fs.Duration("timeout", timeoutDefault, "call/wait deadline")
	command := fs.String("command", "", "JSON command file or -")
	wait := fs.Bool("wait", false, "wait after submission")
	after := fs.String("after", "", "last applied cursor")
	data := fs.String("data-dir", env("TASKWORKER_DATA_DIR", ""), "service data directory")
	ollama := fs.String("ollama", env("TASKWORKER_OLLAMA_ENDPOINT", "http://127.0.0.1:11434"), "Ollama endpoint")
	if fs.Parse(args[1:]) != nil || *timeout < 0 {
		return usageError()
	}
	rest := fs.Args()
	allowed := map[string]bool{"server": true, "timeout": true}
	switch cmd {
	case "serve":
		allowed = map[string]bool{"server": true, "data-dir": true, "ollama": true}
	case "submit", "retry", "branch":
		allowed["command"] = true
		allowed["wait"] = true
	case "watch":
		allowed["after"] = true
	case "wait", "list", "get", "result", "cancel", "models", "health", "queue", "mcp":
	default:
		return usageError()
	}
	valid := true
	fs.Visit(func(f *flag.Flag) {
		if !allowed[f.Name] {
			valid = false
		}
	})
	if !valid {
		return usageError()
	}
	if cmd == "serve" {
		if len(rest) != 0 || serve == nil {
			return usageError()
		}
		if *data == "" {
			d, e := os.UserConfigDir()
			if e != nil {
				return emitError(httpapi.PublicFault(e), 1)
			}
			*data = filepath.Join(d, "taskworker", "data")
		}
		if e := serve(ctx, ServeConfig{*address, *data, *ollama}, stderr); e != nil {
			return emitError(e, 1)
		}
		return 0
	}
	client, e := httpapi.NewClient(*address)
	if e != nil {
		return usageError()
	}
	defer client.Close()
	if cmd == "mcp" {
		// stdout carries MCP messages only; the bridge owns it exclusively.
		if len(rest) != 0 {
			return usageError()
		}
		if e := mcpbridge.Run(ctx, stdin, stdout, mcpbridge.Config{Client: client, Server: *address, Version: Version, CallTimeout: *timeout, Stderr: stderr}); e != nil && ctx.Err() == nil {
			return emitError(httpapi.PublicFault(e), 1)
		}
		return 0
	}
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	var result any
	switch cmd {
	case "submit", "retry", "branch":
		if len(rest) != 0 || *command == "" {
			return usageError()
		}
		reader := stdin
		if *command != "-" {
			f, e := os.Open(*command)
			if e != nil {
				return emitError(&core.Fault{Code: core.ErrInvalidRequest, Message: "cannot read command file"}, 2)
			}
			defer f.Close()
			reader = f
		}
		b, e := io.ReadAll(io.LimitReader(reader, httpapi.MaxBody+1))
		if e != nil {
			return usageError()
		}
		var body any
		key := ""
		switch cmd {
		case "submit":
			c := new(core.SubmitCommand)
			e = httpapi.DecodeCommand(b, c)
			body = c
			key = c.Key
		case "retry":
			c := new(core.RetryCommand)
			e = httpapi.DecodeCommand(b, c)
			body = c
			key = c.Key
		case "branch":
			c := new(core.BranchCommand)
			e = httpapi.DecodeCommand(b, c)
			body = c
			key = c.Key
		}
		if e != nil {
			return emitError(e, 2)
		}
		if key == "" || len(key) > 128 {
			return emitError(&core.Fault{Code: core.ErrInvalidRequest, Message: "idempotency_key required in saved command"}, 2)
		}
		_ = json.NewEncoder(stderr).Encode(map[string]string{"idempotency_key": key, "operation": cmd})
		var j core.Job
		e = client.Call(ctx, "POST", "/v1/"+cmd, body, &j)
		if e != nil {
			return emitError(e, 1)
		}
		if *wait {
			j, e = client.Wait(ctx, j.ID)
			if e != nil {
				return emitError(e, 1)
			}
		}
		result = j
	case "get", "result", "cancel", "wait":
		if len(rest) != 1 {
			return usageError()
		}
		id := core.JobID(rest[0])
		decoded, decodeErr := hex.DecodeString(rest[0])
		if decodeErr != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != rest[0] {
			return usageError()
		}
		var j core.Job
		if cmd == "cancel" {
			e = client.Call(ctx, "POST", "/v1/jobs/"+rest[0]+"/cancel", struct{}{}, &j)
		} else if cmd == "wait" {
			j, e = client.Wait(ctx, id)
		} else {
			j, e = client.Get(ctx, id)
		}
		result = j
	case "watch":
		if len(rest) != 0 {
			return usageError()
		}
		e = client.Watch(ctx, *after, func(f httpapi.Frame) error { return json.NewEncoder(stdout).Encode(f) })
		if e != nil {
			return emitError(httpapi.PublicFault(e), 1)
		}
		return 0
	case "queue":
		if len(rest) != 1 {
			return usageError()
		}
		path := "/v1/queue"
		method := "GET"
		var body any
		if rest[0] == "pause" || rest[0] == "resume" {
			path += "/" + rest[0]
			method = "POST"
			body = struct{}{}
		} else if rest[0] != "status" {
			return usageError()
		}
		var q core.QueueState
		e = client.Call(ctx, method, path, body, &q)
		result = q
	default:
		if len(rest) != 0 {
			return usageError()
		}
		path := "/v1/" + cmd
		if cmd == "list" {
			path = "/v1/jobs"
		}
		var raw json.RawMessage
		e = client.Call(ctx, "GET", path, nil, &raw)
		result = raw
	}
	if e != nil {
		return emitError(e, 1)
	}
	if e = json.NewEncoder(stdout).Encode(result); e != nil {
		return emitError(&core.Fault{Code: core.ErrUnavailable, Message: "output write failed"}, 1)
	}
	if j, ok := result.(core.Job); ok && core.Terminal(j.State) {
		switch j.State {
		case core.JobSucceeded:
			return 0
		case core.JobCancelled:
			return 5
		default:
			return 4
		}
	}
	return 0
}
