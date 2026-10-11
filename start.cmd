@echo off
rem start.cmd - launcher for the workbuddy2api gateway.
rem
rem Why this script instead of double-clicking wb2api.exe:
rem   1. Clears proxy env vars. The gateway freezes its env into login.exe
rem      (panel login tool). A stale proxy port (was 127.0.0.1:50561) makes
rem      login fail with "proxyconnect ... actively refused".
rem   2. Redirects output to data\gateway.log. Double-clicked, logs go to a
rem      console window and the panel "Logs" page (which reads the file)
rem      keeps showing stale content.
rem   3. Single instance. A second copy only fails to bind :7863 and leaves
rem      a scary error line in the log. We probe the port first.
rem
rem CHANGED: the gateway now runs in the FOREGROUND (no `start /b`).
rem   `start /b` detached the process from this console, so Ctrl+C was delivered
rem   only to cmd.exe and never reached wb2api.exe - the only way to stop was
rem   `taskkill /F`, which skips p.Flush() and cuts in-flight streaming requests.
rem   Running in the foreground puts the gateway in this console's process group,
rem   so Ctrl+C reaches it and triggers the graceful shutdown path
rem   (flush state.json + drain in-flight, see cmd/server/handoff.go).
rem   To stop from another window, use stop.cmd (calls the graceful shutdown API).
rem
rem Usage: double-click this file, or run start.cmd in a terminal.
rem   Stop: Ctrl+C in this window, or run stop.cmd from another window.
rem NOTE: keep comments ASCII-only (cmd mis-parses UTF-8 comments under the
rem default GBK code page) and avoid parenthesized if-blocks with pipes in
rem echo text (fragile parsing) - use goto labels instead.

cd /d "%~dp0"
set "SLEEP=%SystemRoot%\System32\timeout.exe"

rem -- clear proxy env vars (both cases); gateway dials upstream directly --
set ALL_PROXY=
set HTTP_PROXY=
set HTTPS_PROXY=
set all_proxy=
set http_proxy=
set https_proxy=
set CC_HAHA_SYSTEM_PROXY_URL=

rem -- single instance: 7863 listening = gateway already running --
netstat -ano | findstr /r /c:":7863 .*LISTENING" >nul 2>&1
if errorlevel 1 goto start_gw
echo [start] port 7863 is already listening, gateway is running. not starting again.
echo [start] to restart gracefully: run stop.cmd, then run this script again.
"%SLEEP%" /t 5 /nobreak >nul 2>&1
exit /b 0

:start_gw
if not exist data mkdir data
echo [start] %date% %time% launching wb2api.exe (foreground), log: data\gateway.log >> data\gateway.log
echo [start] gateway running in this window; output -> data\gateway.log
echo [start] press Ctrl+C here to stop gracefully, or run stop.cmd elsewhere.

rem -- foreground run: blocks here until the gateway exits. Ctrl+C in this
rem    console reaches wb2api.exe (same process group) -> graceful shutdown.
wb2api.exe >> data\gateway.log 2>&1

rem -- reached only after the gateway has exited --
echo [start] gateway exited.
netstat -ano | findstr /r /c:":7863 .*LISTENING" >nul 2>&1
if errorlevel 1 goto done
echo [start] WARNING: port 7863 is still held by another process. check tasklist / netstat.
"%SLEEP%" /t 10 /nobreak >nul 2>&1
exit /b 1

:done
exit /b 0
