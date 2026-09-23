$ErrorActionPreference = 'Stop'
$taskRoot = Split-Path -Parent $PSScriptRoot
$taskGo = Join-Path $taskRoot '.tools/go/bin/go.exe'
if (-not (Test-Path -LiteralPath $taskGo)) {
    $taskGo = (Get-Command go -ErrorAction Stop).Source
}
$taskNames = @('GOOS', 'GOARCH', 'GOAMD64', 'CGO_ENABLED', 'GOTOOLCHAIN', 'GOCACHE', 'GOMODCACHE', 'GOPROXY', 'GOSUMDB', 'GOWORK', 'GOFLAGS', 'GOENV')
$taskSaved = @{}
foreach ($taskName in $taskNames) {
    $taskSaved[$taskName] = [Environment]::GetEnvironmentVariable($taskName, 'Process')
}
Push-Location $taskRoot
try {
    $env:GOTOOLCHAIN = 'local'
    $env:GOENV = 'off'
    $env:GOWORK = 'off'
    $env:GOFLAGS = ''
    $env:CGO_ENABLED = '0'
    $env:GOAMD64 = 'v1'
    $env:GOCACHE = Join-Path $taskRoot '.cache/go-build'
    $env:GOMODCACHE = Join-Path $taskRoot '.cache/go-mod'
    $env:GOPROXY = 'off'
    $env:GOSUMDB = 'off'
    $taskFound = & $taskGo version
    if ($LASTEXITCODE -ne 0 -or $taskFound -notmatch '^go version go1\.27\.1 ') {
        throw 'Go 1.27.1 is required; run scripts/bootstrap-go.ps1 on Windows amd64.'
    }
    Write-Output $taskFound
    $env:GOOS = ''
    $env:GOARCH = ''
    & $taskGo vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
    New-Item -ItemType Directory -Force -Path build | Out-Null
    foreach ($taskOS in @('windows', 'darwin', 'linux')) {
        foreach ($taskArch in @('amd64', 'arm64')) {
            $env:GOOS = $taskOS
            $env:GOARCH = $taskArch
            & $taskGo build ./...
            if ($LASTEXITCODE -ne 0) { throw "Package build failed: $taskOS/$taskArch" }
            $taskSuffix = if ($taskOS -eq 'windows') { '.exe' } else { '' }
            $taskOut = "build/taskworker-$taskOS-$taskArch$taskSuffix"
            & $taskGo build -trimpath -buildvcs=false '-ldflags=-s -w' -o $taskOut ./cmd/taskworker
            if ($LASTEXITCODE -ne 0) { throw "Command build failed: $taskOS/$taskArch" }
            Write-Output "$taskOS/$taskArch : $((Get-Item -LiteralPath $taskOut).Length) bytes"
        }
    }
} finally {
    foreach ($taskName in $taskNames) {
        [Environment]::SetEnvironmentVariable($taskName, $taskSaved[$taskName], 'Process')
    }
    Pop-Location
}
