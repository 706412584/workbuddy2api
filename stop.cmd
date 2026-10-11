@echo off
rem stop.cmd - gracefully stop the workbuddy2api gateway.
rem
rem Prefer this over `taskkill /IM wb2api.exe /F`:
rem   The gateway's shutdown path does two things a hard kill skips -
rem   p.Flush() persists the latest state.json, and srv.Shutdown drains
rem   in-flight requests (up to 5s) so streaming responses are not cut off.
rem   This script asks the gateway to shut down through
rem   POST /__admin/system/shutdown, which runs that exact path.
rem
rem Why HTTP and not a signal: Windows has no SIGTERM, and the gateway is
rem   often started detached (from another window / a wrapper), so Ctrl+C
rem   cannot reach it. The shutdown endpoint is the portable way in.
rem
rem If the endpoint is unavailable (older build without /system/shutdown,
rem   or the process is wedged), this script falls back to reporting the
rem   exact taskkill command instead of killing anything by itself.
rem
rem Usage: run stop.cmd. Add "force" to hard-kill if graceful fails:
rem   stop.cmd force
rem
rem NOTE: keep comments ASCII-only (cmd mis-parses UTF-8 comments under the
rem default GBK code page) and avoid parenthesized if-blocks with pipes in
rem echo text (fragile parsing) - use goto labels instead.

setlocal
cd /d "%~dp0"

rem -- port: default 7863; override with  set PORT=7869  before running (multi-instance / testing) --
if not defined PORT set "PORT=7863"
set "BASE=http://127.0.0.1:%PORT%"
set "FORCE=%~1"

rem -- is the gateway even up? --
netstat -ano | findstr /r /c:":%PORT% .*LISTENING" >nul 2>&1
if errorlevel 1 goto not_running

echo [stop] asking gateway to shut down gracefully (flush state.json + drain in-flight)...
curl -s -m 10 -X POST "%BASE%/__admin/system/shutdown" >nul 2>&1
if errorlevel 1 goto endpoint_failed

rem -- wait for the port to be released (graceful drain can take ~5s) --
set "SLEEP=%SystemRoot%\System32\timeout.exe"
set /a WAITED=0
:wait_loop
"%SLEEP%" /t 1 /nobreak >nul 2>&1
netstat -ano | findstr /r /c:":%PORT% .*LISTENING" >nul 2>&1
if errorlevel 1 goto stopped
set /a WAITED+=1
if %WAITED% lss 15 goto wait_loop
echo [stop] WARNING: port %PORT% still listening after %WAITED%s (graceful shutdown did not finish).
goto hard_fallback

:stopped
echo [stop] gateway stopped, port %PORT% released.
endlocal
exit /b 0

:not_running
echo [stop] port %PORT% is not listening - gateway is not running.
endlocal
exit /b 0

:endpoint_failed
echo [stop] graceful shutdown endpoint unavailable (old build or wedged process).
goto hard_fallback

:hard_fallback
echo [stop] the running build may not support graceful shutdown (endpoint added in v1.2.5+).
echo [stop] to hard-kill anyway (skips state flush and cuts in-flight requests), run:
echo [stop]     taskkill /IM wb2api.exe /F
if /i "%FORCE%"=="force" goto do_force
echo [stop] or re-run as:  stop.cmd force
endlocal
exit /b 1

:do_force
echo [stop] force requested - hard-killing wb2api.exe...
taskkill /IM wb2api.exe /F >nul 2>&1
"%SLEEP%" /t 1 /nobreak >nul 2>&1
netstat -ano | findstr /r /c:":%PORT% .*LISTENING" >nul 2>&1
if errorlevel 1 goto force_ok
echo [stop] WARNING: port %PORT% still held after taskkill /F.
endlocal
exit /b 1

:force_ok
echo [stop] gateway killed, port %PORT% released.
endlocal
exit /b 0
