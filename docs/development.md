# Development: building, verifying and packaging

The core imports only the Go standard library and knows nothing about HTTP,
Ollama, UI, MCP or command parsing. Adapters depend inward on core contracts.

## Build

The module path `taskworker.local/taskworker` is a project-local import namespace,
not a claim that a hosted repository or module server exists. Go 1.27.1 is pinned
and there is no CGO requirement. The one third-party dependency is the official MCP
Go SDK (used only by `taskworker mcp`) and its transitive modules; they are
**vendored** in `vendor/` (see `go.mod`/`go.sum`), so builds need no network and no
module cache. The core, journal, Ollama adapter, HTTP API, CLI and UI import only
the standard library. Updating the vendor tree is a maintainer step described in
[mcp.md](mcp.md).

Windows PowerShell, from the project root:

```powershell
./scripts/bootstrap-go.ps1
./scripts/build.ps1
./build/taskworker-windows-amd64.exe version
./build/taskworker-windows-amd64.exe help
```

Bootstrap downloads the official Windows amd64 ZIP into `.tools`, checks its
pinned SHA-256, and extracts it. It never edits machine/user PATH or Go settings.
The build script uses `.tools/go/bin/go.exe` when available, otherwise PATH,
requires the pinned version, and keeps caches under `.cache`. It restores
process environment variables when finished. Network access is needed only for
bootstrap. Install the matching official Go toolchain manually on other hosts.

With Go 1.27.1 already available (POSIX shell):

```sh
GOTOOLCHAIN=local CGO_ENABLED=0 go build ./...
GOTOOLCHAIN=local CGO_ENABLED=0 go vet ./...
GOTOOLCHAIN=local CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w' -o build/taskworker ./cmd/taskworker
```

`-buildvcs=false` matters: inside a git checkout Go stamps the commit and a dirty flag
into the executable, which changes its hash. With the flags above (and `GOAMD64=v1`, the
default) a fresh `git clone` builds a byte-identical executable: the Windows amd64
build from the 0.1.1 commit matches the released binary
(`377ec391741cb82bd3144f31d2cec831079f84f03ccd4c23e276c14dd623f0a5`). That
comparison was made for Windows amd64 only.

Target matrix: `windows/amd64`, `windows/arm64`, `darwin/amd64`, `darwin/arm64`,
`linux/amd64`, and `linux/arm64`. `scripts/build.ps1` builds **every package** for
each target before emitting the stripped command binary. Native GOAMD64 baseline
is v1; Linux libc is unnecessary with CGO disabled. OS runtime/version support
must still be checked against the selected Go release and tested on real hosts.

To just start the service from source without a full build, use
`scripts/run.bat` (Windows) or `scripts/run.sh` (macOS/Linux). Either builds and
runs `taskworker serve` in one step, listens on `http://127.0.0.1:7433` by
default, and forwards any extra arguments (`--data-dir`, `--server`, `--ollama`)
to `serve`.

## Verification status

All six targets compile. The Go tests cover lifecycle concurrency, client
detachment, lineage, event handoff, store locking/process death, crash recovery,
corruption, injected write/sync failures, and encoded storage bounds. CI runs them
on Linux, macOS and Windows, and runs the race detector (`go test -race`, which needs
a C compiler) on the same three over every package except the MCP bridge, whose tests
drive a separately built executable the detector cannot see into, and the test
service. The Python client is tested on CPython 3.10 to 3.14 on Linux and on 3.13 on
macOS and Windows.

The packaged release binary has been executed (`version`, `help`, `serve`, the
embedded UI, the packaged Python client, `mcp` startup and a graceful stop) on
Windows amd64, Linux amd64, Linux arm64 and macOS arm64: on Windows 10 and on Ubuntu
under WSL2 by the maintainer, and on GitHub Actions runners for all four. **Windows
arm64 and macOS amd64 (Intel) have never been executed**; they have
cross-compilation evidence only (their executable headers are checked by the
packaging script). Real inference has run on Windows amd64 using Ollama 0.18.3 and
installed small models (`qwen2.5:0.5b`, `qwen2.5:7b`). Power-loss filesystem/device
testing has not occurred. MCP interoperability is verified with the official Go and
Python SDK clients, not with any specific agent host.

Focused native verification with the project-local toolchain:

```powershell
$env:GOTOOLCHAIN = 'local'
$env:GOCACHE = "$PWD/.cache/go-build"
$env:CGO_ENABLED = '0'
./.tools/go/bin/go.exe test ./... -count=1 -timeout=120s
./.tools/go/bin/go.exe vet ./...
./.tools/go/bin/gofmt.exe -l cmd internal
```

`scripts/verify.ps1` runs the whole deterministic suite: gofmt, vet, every Go test,
the Python tests on each installed CPython 3.10 to 3.14, interoperability with the official MCP Python SDK when a virtual
environment with the `mcp` package is available (`-McpVenv`), the browser-JS
tests, the six-target builds, and a rebuild from a copy that contains only the
product inputs. It needs no Ollama and no network. The Python integration tests
use the test-only scripted service `internal/testservice/cmd/fakeworker`, which is
never part of the `taskworker` executable.

## Measured footprint

`python scripts/measure_idle.py` extracts the packaged Windows amd64 archive, starts
`serve` on a private port and data directory (no Ollama is contacted), lets it settle
for 5 s and samples the process twice. Result on the development host (Windows 10
10.0.19045, 16 logical CPUs, version 0.1.0):

| Measurement | Value |
| --- | --- |
| Executable size (stripped) | 10,005,504 bytes (about 9.5 MiB) |
| Start to healthy | 0.47 s |
| Idle, no clients (60 s window) | 47.0 MiB working set, 45.7 MiB private, 6 threads, 110 handles, 0.00 s CPU |
| Idle, one event-stream observer (30 s window) | 47.2 MiB working set, 45.9 MiB private, 6 threads, 111 handles, 0.00 s CPU |

"0.00 s CPU" means below the counter resolution over the window. These are numbers
for one host, taken while no job was running; memory while a model generates is
dominated by Ollama, not by this service. The other five targets were not measured, so
they have no footprint measurements (their executables are 9.0 to 10.0 MB).

## Packaging a release

```powershell
./scripts/package.ps1            # build, package, inspect, execute the packaged Windows amd64 binary
./scripts/package.ps1 -SkipBuild # package the binaries already in build/
./scripts/package.ps1 -CheckOnly # re-inspect the archives already in dist/
```

`scripts/package.py` (Python standard library only) assembles one archive per target
into `dist/` (`.zip` for Windows, `.tar.gz` with a `0755` executable for macOS and
Linux) together with `SHA256SUMS` and `release-manifest.json`. A release contains
only an explicit allowlist: the executable, `LICENSE`, `THIRD_PARTY_LICENSES.txt`
(the Go runtime and every vendored module), the README, `docs/`, and the Python
client (`clients/python` source, examples, README, `pyproject.toml`, `LICENSE`).
Nothing is taken from a copy of the repository. After assembly the script
re-reads every finished archive and fails on: a member outside the allowlist; a
forbidden name (development records, caches, toolchains, tooling output, test
services, data directories); a personal or machine-specific string in any text
file; a missing required file; an executable whose header does not match its
target OS and architecture (read from the PE, ELF or Mach-O header); or a wrong
mode. On a Windows amd64 host it also extracts the packaged binary and runs it:
`version`, `help`, `serve` on a private port and data directory, the embedded UI,
the packaged Python client against that service, `mcp` startup, and a graceful
stop. The other five targets are recorded in the manifest as *not executed*.

The packaged binaries are not code-signed or notarized.

`python scripts/test_package.py` tests the release gate itself: it tampers with real
archives (a development record added, an unlisted file, a personal path in a text
file, a missing licence, an executable for the wrong OS or architecture, a lost
executable mode, a truncated binary) and requires the inspection to reject each,
and it checks that rebuilding an archive from its own contents is byte-identical.

## Release process

CI (`.github/workflows/ci.yml`) runs Go build, vet and tests plus the race detector on
Linux, macOS and Windows; the Python client suite on CPython 3.10 to 3.14 (Ubuntu) and
3.13 (macOS, Windows); the browser-JS tests; packaging with inspection of every archive
and a check that the generated limitation documents are current; and the packaged
binary executed on Windows amd64, Linux amd64, Linux arm64 and macOS arm64. The arm64
Linux job is allowed to fail.

`python scripts/release_check.py` is the release gate. It runs every automatable check
(clean git tree, agreeing versions, licence files, limitation documents in sync with
their source, archive inspection, the gate's own tests, a byte-for-byte rebuild of
every executable from a fresh clone of HEAD, and the last full local verification),
verifies the recorded manual evidence, and prints READY or NOT READY. It exits 0 only for
READY.

- `python scripts/release_check.py --fast` skips the slow checks for quick feedback and
  can never report READY.
- `python scripts/release_check.py attest ITEM --by NAME --note TEXT` records a manual
  check from `release/manual-checks.json` (it prints the steps first) in
  `release/attestations.json`.
- `python scripts/release_check.py notes` writes `dist/RELEASE_NOTES.md` from the
  recorded evidence.

An attestation is bound to what it vouches for, and stops counting when that changes:
`ui_assets` (the four embedded browser assets), `binary:<os>/<arch>` (that target's
executable hash in `dist/release-manifest.json`) or `commit` (a git commit; it survives
later commits that change only `release/attestations.json`). Changing the UI therefore
invalidates the interactive browser check, and any code change invalidates the
quick-start check and the CI attestation. Record the CI attestation last, on the final
commit, after CI is green for it.

Known limitations live in `release/limitations.json`. Edit that file, then run
`python scripts/limitations.py --write` to regenerate the README block and
[limitations.md](limitations.md); `--check` (also run by the gate and CI) fails if
they differ. A limitation can only be marked closed, or assert a fact, with valid
evidence behind it.

Release executables are reproducible: `CGO_ENABLED=0`, `GOTOOLCHAIN=local`,
`GOPROXY=off`, and `go build -trimpath -buildvcs=false -ldflags "-s -w"` (see
[Build](#build)).

## Source guide

- `cmd/taskworker/main.go`: executable entry point.
- `internal/core/`: worker lifecycle, validation/reducer, bounded observers, ports,
  and deterministic fake-backend tests.
- `internal/adapters/cli/`: HTTP client commands and JSON/exit presentation.
- `internal/adapters/httpapi/`: strict local JSON/SSE server and shared HTTP client.
- `internal/adapters/mcpbridge/`: MCP stdio service-client bridge (tools, framing).
- `clients/python/`: standard-library Python client, examples and tests.
- `internal/testservice/`: test-only scripted backend service (never linked into
  the `taskworker` executable).
- `internal/service/`: sole-owner startup, recovery and signal shutdown wiring.
- `internal/adapters/journal/`: journal, platform ownership locks, recovery, and
  fault-injection/process-death tests.
- `internal/adapters/ollama/`: local-only discovery, bounded streaming backend,
  deterministic HTTP/core/journal tests, and explicitly opt-in real-model checks.
- [Contracts](contracts.md): behavior all adapters must preserve.
- [Persistence](persistence.md): selected format, durability, recovery, limits.

Builds contain product code, the vendored MCP SDK modules and four explicitly
embedded browser assets. Journal runtime reads are limited to its explicit data
directory. The executable includes the service, CLI, core, journal and Ollama
adapter.

Go references: [official downloads](https://go.dev/dl/),
[target environments](https://go.dev/doc/install/source#environment).
