// Command fakeworker starts the test-only scripted service for Python/MCP
// integration tests. It prints "ready URL" on stdout and stops on stdin EOF.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"taskworker.local/taskworker/internal/testservice"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	data := flag.String("data-dir", "", "journal directory")
	gate := flag.String("gate-dir", "", "gate file directory")
	down := flag.Bool("models-down", false, "Models fails with backend_unavailable")
	flag.Parse()
	r, err := testservice.Start(testservice.Config{Addr: *addr, DataDir: *data, GateDir: *gate, ModelsDown: *down})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("ready", r.URL)
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := r.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
