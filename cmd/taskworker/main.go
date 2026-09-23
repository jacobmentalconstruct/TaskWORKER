// Command taskworker is the only production service composition root.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"taskworker.local/taskworker/internal/adapters/cli"
	"taskworker.local/taskworker/internal/service"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	code := cli.RunContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, func(ctx context.Context, c cli.ServeConfig, log io.Writer) error {
		return service.Run(ctx, service.Config{Server: c.Server, DataDir: c.DataDir, Ollama: c.Ollama}, log)
	})
	os.Exit(code)
}
