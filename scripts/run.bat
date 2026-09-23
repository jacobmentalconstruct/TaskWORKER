@echo off
rem Build and start TaskWorker from source for local testing.
rem Usage: scripts\run.bat [serve flags, e.g. --data-dir DIR --ollama URL]
setlocal
set "ROOT=%~dp0.."
set "GOEXE=%ROOT%\.tools\go\bin\go.exe"
if exist "%GOEXE%" goto :found
for /f "delims=" %%G in ('where go 2^>nul') do set "GOEXE=%%G"
if not defined GOEXE (
  echo Go was not found in .tools\go\bin or on PATH. Run scripts\bootstrap-go.ps1, or install Go 1.27.1.
  exit /b 1
)
:found
set "GOTOOLCHAIN=local"
set "CGO_ENABLED=0"
pushd "%ROOT%"
echo Starting TaskWorker... it will listen on http://127.0.0.1:7433 unless overridden with --server.
echo Press Ctrl+C to stop.
"%GOEXE%" run ./cmd/taskworker serve %*
set "EXITCODE=%ERRORLEVEL%"
popd
exit /b %EXITCODE%
