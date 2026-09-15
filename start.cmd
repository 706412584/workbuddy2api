@echo off
rem start.cmd - fixed launcher for the workbuddy2api gateway.
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
rem Usage: double-click this file, or run start.cmd in a terminal.
rem NOTE: keep comments ASCII-only (cmd mis-parses UTF-8 comments under the
rem default GBK code page) and avoid parenthesized if-blocks with pipes in
rem echo text (fragile parsing) - use goto labels instead.

cd /d "%~dp0"

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
echo [start] to restart: taskkill /IM wb2api.exe /F, then run this script again.
timeout /t 5 >nul
exit /b 0

:start_gw
if not exist data mkdir data
echo [start] %date% %time% launching wb2api.exe, log: data\gateway.log >> data\gateway.log
start "" /b wb2api.exe >> data\gateway.log 2>&1

timeout /t 3 >nul
netstat -ano | findstr /r /c:":7863 .*LISTENING" >nul 2>&1
if errorlevel 1 goto failed
echo [start] gateway started, listening on 7863.
timeout /t 3 >nul
exit /b 0

:failed
echo [start] WARNING: 7863 not listening after 3s. check data\gateway.log.
timeout /t 10 >nul
exit /b 1
