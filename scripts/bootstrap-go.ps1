# Project-local bootstrap for Windows amd64. No machine/user settings are changed.
$ErrorActionPreference = 'Stop'
$taskRoot = Split-Path -Parent $PSScriptRoot
$taskTools = Join-Path $taskRoot '.tools'
$taskGo = Join-Path $taskTools 'go/bin/go.exe'
$taskVersion = 'go1.27.1'
$taskFile = "$taskVersion.windows-amd64.zip"
$taskSHA256 = 'a3911b5e0e1b1053f25ed0675f4c1c6aad1e2bfcf253df2b9be4caabd2edd95d'
if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64' -or
    [System.Environment]::OSVersion.Platform -ne 'Win32NT') {
    throw 'This bootstrap supports Windows amd64. Install the pinned Go release for your host manually.'
}
if (Test-Path -LiteralPath $taskGo) {
    $taskFound = & $taskGo version
    if ($LASTEXITCODE -ne 0 -or $taskFound -ne "go version $taskVersion windows/amd64") {
        throw 'Existing project toolchain differs; inspect .tools before replacing it.'
    }
    Write-Output $taskFound
    exit 0
}
if (Test-Path -LiteralPath (Join-Path $taskTools 'go')) {
    throw 'Incomplete project toolchain exists; inspect it before retrying extraction.'
}
New-Item -ItemType Directory -Force -Path $taskTools | Out-Null
$taskZip = Join-Path $taskTools $taskFile
if (-not (Test-Path -LiteralPath $taskZip)) {
    Invoke-WebRequest -Uri "https://go.dev/dl/$taskFile" -OutFile $taskZip
}
if ((Get-FileHash -LiteralPath $taskZip -Algorithm SHA256).Hash.ToLowerInvariant() -ne $taskSHA256) {
    throw 'Official Go archive SHA-256 mismatch; archive was not extracted.'
}
Expand-Archive -LiteralPath $taskZip -DestinationPath $taskTools
& $taskGo version
if ($LASTEXITCODE -ne 0) { throw 'Extracted Go toolchain failed to run.' }
