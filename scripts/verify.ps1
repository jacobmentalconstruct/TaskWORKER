# Deterministic verification: Go, Python and MCP checks plus a source-only rebuild.
# No Ollama, no network (the MCP SDK is vendored). Run from the project root.
# Logs go to $Out (default: .cache/verify) so the run itself never depends on development records.
param(
    [string]$Out = '.cache/verify',
    [string]$McpVenv = '.cache/mcp-venv'   # optional: venv with the official `mcp` Python SDK
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
$go = Join-Path $root '.tools/go/bin/go.exe'
$gofmt = Join-Path $root '.tools/go/bin/gofmt.exe'
$outDir = Join-Path $root $Out
New-Item -ItemType Directory -Force $outDir | Out-Null
$saved = @{}
foreach ($n in 'GOOS','GOARCH','GOTOOLCHAIN','GOCACHE','GOMODCACHE','GOPROXY','GOSUMDB','GOFLAGS','CGO_ENABLED','GOENV','GOWORK','TASKWORKER_EXE','TASKWORKER_FAKEWORKER') { $saved[$n] = [Environment]::GetEnvironmentVariable($n, 'Process') }
$env:GOTOOLCHAIN = 'local'; $env:GOENV = 'off'; $env:GOWORK = 'off'; $env:GOFLAGS = ''; $env:CGO_ENABLED = '0'
$env:GOCACHE = Join-Path $root '.cache/go-build'; $env:GOPROXY = 'off'; $env:GOSUMDB = 'off'
$env:GOOS = ''; $env:GOARCH = ''
$evidence = [ordered]@{ started = [DateTime]::UtcNow.ToString('o') }
function Step($name, [scriptblock]$body) {
    Write-Output "== $name"
    & $body
    if ($LASTEXITCODE -ne 0 -and $null -ne $LASTEXITCODE) { throw "$name failed ($LASTEXITCODE)" }
}
try {
    Step 'gofmt' {
        $bad = & $gofmt -l cmd internal
        if ($bad) { throw "unformatted: $bad" }
        $global:LASTEXITCODE = 0
    }
    Step 'go vet' { & $go vet ./... 2>&1 | Tee-Object (Join-Path $outDir 'vet.txt') }
    Step 'go test' { & $go test ./... -count=1 -timeout=300s 2>&1 | Tee-Object (Join-Path $outDir 'go-test.txt') }
    Step 'go test -json (counts)' {
        & $go test ./... -count=1 -timeout=300s -json > (Join-Path $outDir 'go-test.jsonl') 2>&1
        $events = Get-Content (Join-Path $outDir 'go-test.jsonl') | ForEach-Object { try { $_ | ConvertFrom-Json } catch { $null } } | Where-Object { $_ -and $_.Test }
        $evidence.go_tests_pass = @($events | Where-Object Action -eq 'pass').Count
        $evidence.go_tests_fail = @($events | Where-Object Action -eq 'fail').Count
        if ($evidence.go_tests_fail -ne 0) { throw 'go tests failed' }
    }
    $fake = Join-Path $outDir 'fakeworker.exe'
    $exe = Join-Path $outDir 'taskworker.exe'
    Step 'build test service and executable' {
        & $go build -trimpath '-ldflags=-s -w' -o $fake ./internal/testservice/cmd/fakeworker
        if ($LASTEXITCODE) { throw 'fakeworker build' }
        & $go build -trimpath -buildvcs=false '-ldflags=-s -w' -o $exe ./cmd/taskworker
    }
    $env:TASKWORKER_FAKEWORKER = $fake; $env:TASKWORKER_EXE = $exe
    $evidence.taskworker_sha256 = (Get-FileHash $exe).Hash
    $evidence.taskworker_bytes = (Get-Item $exe).Length
    # Python: every installed supported version (3.10 - 3.14) runs unit + integration tests.
    $pyResults = [ordered]@{}
    foreach ($v in '3.10','3.11','3.12','3.13','3.14') {
        $py = (& py "-$v" -c 'import sys;print(sys.executable)' 2>$null)
        if (-not $py) { $pyResults[$v] = 'not installed'; continue }
        Step "python $v tests" {
            Push-Location (Join-Path $root 'clients/python')
            try {
                $log = Join-Path $outDir "python-$v.txt"
                & $py -W error::ResourceWarning -m unittest discover -s tests -p 'test_*.py' -v > $log 2>&1
                if ($LASTEXITCODE) { Get-Content $log -Tail 40; throw "python $v tests failed" }
                $ran = (Select-String -Path $log -Pattern '^Ran (\d+) tests' | Select-Object -Last 1).Matches.Groups[1].Value
                $skipped = (Select-String -Path $log -Pattern 'skipped' -SimpleMatch | Measure-Object).Count
                $pyResults[$v] = "ok ($ran tests)"
            } finally { Pop-Location }
        }
    }
    $evidence.python = $pyResults
    # Independent official MCP client (Python SDK), modern and legacy lifecycles.
    $venvPy = Join-Path $root "$McpVenv/Scripts/python.exe"
    if (Test-Path $venvPy) {
        foreach ($mode in 'modern','legacy') {
            Step "official mcp python client ($mode)" {
                $log = Join-Path $outDir "interop-$mode.json"
                & $venvPy (Join-Path $root 'clients/python/tests/interop_mcp_official.py') $mode > $log 2> (Join-Path $outDir "interop-$mode.err")
                if ($LASTEXITCODE) { Get-Content (Join-Path $outDir "interop-$mode.err") -Tail 30; throw "interop $mode failed" }
                $evidence["interop_$mode"] = (Get-Content $log -Raw | ConvertFrom-Json)
            }
        }
    } else { $evidence.interop = "SKIPPED: no venv at $McpVenv with the official mcp package" }
    # Browser-JS regression tests (the embedded UI modules).
    if (Get-Command node -ErrorAction SilentlyContinue) {
        Step 'browser JS tests' {
            & node --experimental-default-type=module internal/adapters/webui/state.test.mjs 2>&1 | Tee-Object (Join-Path $outDir 'js-state.txt')
            if ($LASTEXITCODE) { throw 'state tests' }
            & node --experimental-default-type=module internal/adapters/webui/app.test.mjs 2>&1 | Tee-Object (Join-Path $outDir 'js-app.txt')
        }
    }
    Step 'six-target builds' { & (Join-Path $root 'scripts/build.ps1') 2>&1 | Tee-Object (Join-Path $outDir 'builds.txt') }
    # Source-only rebuild: exactly the product inputs, no .dev-logs, .cache, .tools or data.
    $iso = Join-Path $root ('.cache/source-only-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory $iso | Out-Null
    $assets = 'index.html','app.css','app.js','state.js' | ForEach-Object { Join-Path $root "internal/adapters/webui/$_" }
    $copy = @()
    $copy += Get-Item go.mod, go.sum
    $copy += Get-ChildItem cmd, internal -Recurse -File | Where-Object { $_.Extension -eq '.go' -or $_.FullName -in $assets }
    $copy += Get-ChildItem vendor -Recurse -File
    $copy += Get-ChildItem clients/python -Recurse -File | Where-Object { $_.FullName -notmatch '__pycache__|\.egg-info|\\build\\' -and $_.Extension -in '.py','.toml','.md' }
    $copy += Get-ChildItem docs -File
    $copy += Get-Item README.md
    foreach ($f in $copy) {
        $rel = $f.FullName.Substring($root.Length + 1)
        $dest = Join-Path $iso $rel
        New-Item -ItemType Directory -Force (Split-Path $dest) | Out-Null
        Copy-Item -LiteralPath $f.FullName -Destination $dest
    }
    $evidence.isolated_source = $iso
    $evidence.isolated_file_count = $copy.Count
    if (Test-Path (Join-Path $iso '.dev-logs')) { throw 'isolated source contains .dev-logs' }
    Push-Location $iso
    try {
        Step 'isolated go test' { & $go test ./... -count=1 -timeout=300s 2>&1 | Tee-Object (Join-Path $outDir 'isolated-go-test.txt') }
        Step 'isolated go vet' { & $go vet ./... }
        foreach ($os in 'windows','darwin','linux') { foreach ($arch in 'amd64','arm64') {
            $env:GOOS = $os; $env:GOARCH = $arch
            & $go build ./...
            if ($LASTEXITCODE) { throw "isolated package build $os/$arch" }
            $sfx = if ($os -eq 'windows') { '.exe' } else { '' }
            & $go build -trimpath -buildvcs=false '-ldflags=-s -w' -o (Join-Path $outDir "isolated-$os-$arch$sfx") ./cmd/taskworker
            if ($LASTEXITCODE) { throw "isolated executable build $os/$arch" }
        } }
        $env:GOOS = ''; $env:GOARCH = ''
        $isoExe = Join-Path $outDir 'isolated-windows-amd64.exe'
        $evidence.isolated_binary_sha256 = (Get-FileHash $isoExe).Hash
        # Python module used from the isolated tree, outside any development record.
        $env:PYTHONPATH = (Join-Path $iso 'clients/python/src')
        Push-Location ([System.IO.Path]::GetTempPath())
        try {
            $v = & python -c "import taskworker_client as t; print(t.__version__, t.__file__)"
            if ($LASTEXITCODE) { throw 'python import from isolated source failed' }
            $evidence.python_import = $v
            if ($v -match '\.dev-logs') { throw 'python import resolved inside dev logs' }
        } finally { Pop-Location; Remove-Item Env:PYTHONPATH }
        # The isolated executable must start MCP stdio and answer discover.
        $probe = '{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"probe","version":"0"}}}}'
        # A real client waits for the reply before closing stdin, so do the same.
        $env:TW_PROBE = $probe
        $probeCode = @'
import os, subprocess, sys
p = subprocess.Popen([sys.argv[1], "mcp", "--server", "http://127.0.0.1:1"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
p.stdin.write((os.environ["TW_PROBE"] + "\n").encode()); p.stdin.flush()
line = p.stdout.readline().decode()
p.stdin.close()
code = p.wait(10)
print(line.strip())
sys.exit(0 if '"supportedVersions"' in line and code == 0 else 1)
'@
        $reply = & python -c $probeCode $isoExe
        Remove-Item Env:TW_PROBE
        if ($LASTEXITCODE) { throw 'isolated mcp startup failed' }
        $evidence.isolated_mcp_startup = 'ok'
        # Isolated Python tests (unit + integration with a freshly built fakeworker from the isolated tree).
        $isoFake = Join-Path $outDir 'isolated-fakeworker.exe'
        & $go build -trimpath '-ldflags=-s -w' -o $isoFake ./internal/testservice/cmd/fakeworker
        if ($LASTEXITCODE) { throw 'isolated fakeworker build' }
        $env:TASKWORKER_FAKEWORKER = $isoFake; $env:TASKWORKER_EXE = $isoExe
        Push-Location (Join-Path $iso 'clients/python')
        try {
            & python -m unittest discover -s tests -p 'test_*.py' > (Join-Path $outDir 'isolated-python.txt') 2>&1
            if ($LASTEXITCODE) { Get-Content (Join-Path $outDir 'isolated-python.txt') -Tail 30; throw 'isolated python tests failed' }
        } finally { Pop-Location }
        $evidence.isolated_python = 'ok'
    } finally { Pop-Location; $env:GOOS = ''; $env:GOARCH = '' }
    $evidence.race_compilers = @(Get-Command gcc, clang, cc -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Source)
    $evidence.finished = [DateTime]::UtcNow.ToString('o')
    $evidence.result = 'pass'
} catch {
    $evidence.result = "FAIL: $_"
    throw
} finally {
    foreach ($n in $saved.Keys) { [Environment]::SetEnvironmentVariable($n, $saved[$n], 'Process') }
    $evidence | ConvertTo-Json -Depth 6 | Set-Content (Join-Path $outDir 'evidence.json')
}
