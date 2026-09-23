#!/usr/bin/env bash
# Build and start TaskWorker from source for local testing.
# Usage: scripts/run.sh [serve flags, e.g. --data-dir DIR --ollama URL]
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GOEXE="$ROOT/.tools/go/bin/go"
if [ ! -x "$GOEXE" ]; then
  GOEXE="$(command -v go || true)"
fi
if [ -z "$GOEXE" ]; then
  echo "Go was not found in .tools/go/bin or on PATH. Run scripts/bootstrap-go.ps1, or install Go 1.27.1." >&2
  exit 1
fi
export GOTOOLCHAIN=local
export CGO_ENABLED=0
cd "$ROOT"
echo "Starting TaskWorker... it will listen on http://127.0.0.1:7433 unless overridden with --server."
echo "Press Ctrl+C to stop."
exec "$GOEXE" run ./cmd/taskworker serve "$@"
