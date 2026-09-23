# Build all six targets, then assemble and inspect the release archives in dist/.
#   ./scripts/package.ps1              build, package, inspect, and execute the packaged Windows amd64 binary
#   ./scripts/package.ps1 -SkipBuild   package the binaries already in build/
#   ./scripts/package.ps1 -CheckOnly   inspect the archives already in dist/
# Needs Python 3.10+ (standard library only) and, unless -SkipBuild/-CheckOnly, the pinned Go toolchain.
param(
    [switch]$SkipBuild,
    [switch]$CheckOnly
)
$ErrorActionPreference = 'Stop'
$taskRoot = Split-Path -Parent $PSScriptRoot
Set-Location $taskRoot
$taskPy = $null
foreach ($taskCandidate in @('python', 'python3')) {
    if (Get-Command $taskCandidate -ErrorAction SilentlyContinue) { $taskPy = $taskCandidate; break }
}
if (-not $taskPy) { throw 'Python 3.10+ is required to package a release.' }
if ($CheckOnly) {
    & $taskPy scripts/package.py check
    exit $LASTEXITCODE
}
if (-not $SkipBuild) {
    & (Join-Path $PSScriptRoot 'build.ps1')
}
$taskArgs = @('scripts/package.py', 'build')
if ($IsWindows -or $env:OS -eq 'Windows_NT') { $taskArgs += '--run-host-check' }
& $taskPy @taskArgs
exit $LASTEXITCODE
