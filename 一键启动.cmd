@echo off
rem ============================================================================
rem  一键启动.cmd - workbuddy2api LAN launcher (double-click friendly).
rem
rem  What it does, in order:
rem    1. stop a leftover instance holding the port - but ONLY if that instance
rem       is this directory's own wb2api.exe (see the SAFETY note below);
rem    2. check wb2api.exe exists;
rem    3. detect the LAN IPv4 via the default-route interface (a bare "first
rem       non-loopback address" scan picks Hyper-V's 172.x virtual adapter);
rem    4. ask whether to open LAN access; 10s no-answer = localhost only;
rem    5. add the Windows Firewall inbound rule when LAN is chosen and missing
rem       (needs admin; degrades to printing the command otherwise);
rem    6. launch the gateway in the FOREGROUND and print the access URLs.
rem
rem  Why WB2A_LISTEN instead of editing config.json: the env var is an override
rem  applied after the file is read (cmd/server/config.go applyEnv), so the
rem  config file stays untouched and the choice is per-run.
rem
rem  Why foreground: Ctrl+C then reaches wb2api.exe (same process group) and
rem  triggers the graceful shutdown path (flush state.json + drain in-flight).
rem  Detaching would leave taskkill /F as the only way out, which cuts streams.
rem
rem  SAFETY - why the takeover check compares ExecutablePath, not process name:
rem  one machine can easily run two workbuddy2api copies (prod + test). Matching
rem  on "wb2api.exe" alone stops whichever one holds the port, including another
rem  directory's production instance. That is not hypothetical - an earlier
rem  version of this script read config.json with a broken for /f delims=: parse,
rem  fell back to the default port 7863, found production's instance there, and
rem  killed it (47 minutes of downtime). Hence both fixes: robust port
rem  extraction, and a per-directory executable path match.
rem
rem  ENCODING: this file is GBK (code page 936) with CRLF line endings. Chinese
rem  text and comments are verified to work in that combination. Do NOT convert
rem  to UTF-8: a UTF-8 batch mis-parses under the default 936 console code page
rem  (tested - commands run garbled). Edit this file as GBK, not UTF-8.
rem
rem  NOTE: keep echoes free of parentheses when a pipe is involved - cmd parses
rem  parenthesized blocks containing pipes unreliably. goto labels are used.
rem ============================================================================

setlocal enabledelayedexpansion
chcp 936 >nul 2>&1
cd /d "%~dp0"
title workbuddy2api - 一键启动

set "PS=powershell -NoProfile -ExecutionPolicy Bypass -Command"
set "SLEEP=%SystemRoot%\System32\timeout.exe"
set "CURL=%SystemRoot%\System32\curl.exe"

echo ==============================================
echo    workbuddy2api - 一键启动
echo ==============================================
echo.

rem ---------- 读取端口（config.json 的 listen，失败回落 7863） ----------
rem 端口解析交给 PowerShell 正则，不用 for /f "delims=:"：":7863" 的冒号是行首
rem 分隔符，被 delims 吃掉后只剩 token 1，tokens=2 取不到值 -> 静默回落 7863。
rem 实测踩过：脚本放进 listen=":17869" 的目录，仍去操作 7863（生产端口）。
rem 正则 :(\d+)\s*$ 对 ":7863" / "0.0.0.0:7863" / "127.0.0.1:7863" 都取尾部端口。
set "PORT="
for /f "usebackq delims=" %%p in (`%PS% "$l=try{(Get-Content -Raw '%~dp0config.json' | ConvertFrom-Json).listen}catch{''}; if($l -match ':(\d+)\s*$'){$matches[1]}else{'7863'}"`) do set "PORT=%%p"
if "!PORT!"=="" set "PORT=7863"

rem ---------- 0. 停掉占用端口的旧实例（仅限本目录） ----------
set "OLDPID="
for /f "tokens=5" %%p in ('netstat -ano ^| findstr /r /c:":!PORT! .*LISTENING" 2^>nul') do set "OLDPID=%%p"
if not defined OLDPID goto no_old

rem 取该 PID 的可执行文件全路径，只有等于本目录的 wb2api.exe 才允许接管。
set "OLDEXE="
for /f "usebackq delims=" %%e in (`%PS% "try{(Get-CimInstance Win32_Process -Filter 'ProcessId=!OLDPID!').ExecutablePath}catch{''}"`) do set "OLDEXE=%%e"
if "!OLDEXE!"=="" goto foreign_port
if /i not "!OLDEXE!"=="%~dp0wb2api.exe" goto foreign_port

echo [提示] 端口 !PORT! 已被本目录的实例占用（PID !OLDPID!），先优雅停止 ...
"%CURL%" -s -m 10 -X POST "http://127.0.0.1:!PORT!/__admin/system/shutdown" >nul 2>&1
set /a WAITED=0
:wait_old
"%SLEEP%" /t 1 /nobreak >nul 2>&1
netstat -ano | findstr /r /c:":!PORT! .*LISTENING" >nul 2>&1
if errorlevel 1 goto old_stopped
set /a WAITED+=1
if !WAITED! lss 12 goto wait_old
echo [提示] 优雅停止超时，改为强制结束 PID !OLDPID! ...
"%SystemRoot%\System32\taskkill.exe" /PID !OLDPID! /F >nul 2>&1
"%SLEEP%" /t 2 /nobreak >nul 2>&1
goto old_stopped

:old_stopped
netstat -ano | findstr /r /c:":!PORT! .*LISTENING" >nul 2>&1
if errorlevel 1 goto no_old
echo [错误] 旧实例仍占用端口 !PORT!，无法继续。
goto err

:foreign_port
echo [错误] 端口 !PORT! 已被占用，但该进程不属于本目录，脚本不会动它。
if defined OLDEXE echo        占用进程：!OLDEXE!
echo        为免误停别的服务，请自行处理，或在 config.json 里改 listen 端口。
goto err

:no_old
echo [1/4] 端口 !PORT! 可用。

rem ---------- 1. 检查主程序 ----------
if not exist wb2api.exe goto no_exe
echo [2/4] 主程序 wb2api.exe 已就绪。

rem ---------- 2. 目录初始化 ----------
if not exist config.json goto no_config
if not exist auths mkdir auths >nul 2>&1
if not exist data mkdir data >nul 2>&1

rem ---------- 3. 探测局域网 IP ----------
rem 走默认路由所在网卡：多网卡机器上（Hyper-V / WSL / VMware 虚拟网卡）
rem "第一个非回环地址" 会选中 172.x 虚拟网卡，那个地址局域网设备连不上。
set "LANIP="
for /f "usebackq delims=" %%a in (`%PS% "$r=Get-NetRoute -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | Sort-Object RouteMetric | Select-Object -First 1; if($r){(Get-NetIPAddress -InterfaceIndex $r.InterfaceIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object {$_.IPAddress -notlike '127.*' -and $_.IPAddress -notlike '169.254.*'} | Select-Object -First 1).IPAddress}"`) do set "LANIP=%%a"

rem ---------- 4. 询问是否开放局域网 ----------
set "BINDHOST=127.0.0.1"
if "!LANIP!"=="" goto no_lan

echo.
echo [局域网] 检测到本机局域网 IP：!LANIP!
choice /c YN /t 10 /d N /m "是否开放局域网访问（其他设备可连 !LANIP!:!PORT!）？10 秒不选=仅本机"
if errorlevel 2 goto lan_declined
set "BINDHOST=0.0.0.0"
goto lan_chosen

:lan_declined
echo [本机模式] 仅本机可访问。
goto after_choice

:no_lan
echo [局域网] 未检测到局域网连接（WiFi/网线未连接），本次仅本机可访问。
goto after_choice

:lan_chosen
echo [局域网] 已开放，其他设备访问：http://!LANIP!:!PORT!
echo [安全提示] 局域网内任何设备都能调用你的账号池；面板管理接口仍只允许本机
echo            （局域网要开面板需在 config.json 配 admin.token）。

rem ---------- 5. 防火墙放行（仅开放局域网时） ----------
rem netsh show rule 对不存在的规则同样返回 0，不能用 errorlevel 判存在：
rem 改为把输出落文件再 findstr 找规则名，找不到才添加。
netsh advfirewall firewall show rule name="workbuddy2api-lan" > "%TEMP%\wb2api_fw.txt" 2>nul
"%SystemRoot%\System32\findstr.exe" /i /c:"workbuddy2api-lan" "%TEMP%\wb2api_fw.txt" >nul 2>&1
if not errorlevel 1 goto fw_ok

net session >nul 2>&1
if errorlevel 1 goto fw_need_admin

echo [防火墙] 未找到放行规则，正在添加 ...
netsh advfirewall firewall add rule name="workbuddy2api-lan" dir=in action=allow program="%~dp0wb2api.exe" protocol=TCP localport=!PORT! profile=any enable=yes >nul 2>&1
if errorlevel 1 goto fw_failed
echo [防火墙] 已添加 TCP !PORT! 入站放行规则 workbuddy2api-lan。
goto fw_ok

:fw_need_admin
echo [防火墙] 未找到放行规则，且当前不是管理员，无法自动添加。
echo            其他设备可能连不上。请用管理员身份运行一次本脚本，或手动执行：
echo            netsh advfirewall firewall add rule name="workbuddy2api-lan" dir=in action=allow program="%~dp0wb2api.exe" protocol=TCP localport=!PORT! profile=any enable=yes
goto fw_ok

:fw_failed
echo [防火墙] 添加规则失败（可能被组策略限制），其他设备可能连不上。
echo            请手动执行上面的 netsh 命令排查。

:fw_ok
del "%TEMP%\wb2api_fw.txt" >nul 2>&1

:after_choice
echo.
echo [3/4] 绑定地址：!BINDHOST!:!PORT!
echo [4/4] 启动中 ... 关闭本窗口即停止，或用 stop.cmd 优雅停止。
echo.
echo   本机面板：http://127.0.0.1:!PORT!/
echo   本机健康：http://127.0.0.1:!PORT!/healthz
if "!BINDHOST!"=="0.0.0.0" echo   局域网接口：http://!LANIP!:!PORT!/v1
echo   运行日志：%~dp0data\gateway.log
echo.

rem 代理变量清空：网关自己直连上游，残留的代理端口会让登录工具连接失败。
set ALL_PROXY=
set HTTP_PROXY=
set HTTPS_PROXY=
set all_proxy=
set http_proxy=
set https_proxy=
set CC_HAHA_SYSTEM_PROXY_URL=

rem 绑定地址经环境变量覆盖，config.json 保持原样（applyEnv 优先级高于文件）。
set "WB2A_LISTEN=!BINDHOST!:!PORT!"

echo [启动] %date% %time% 一键启动（listen=!WB2A_LISTEN!） >> data\gateway.log
rem 用 "%~dp0" 显式限定路径：裸写 wb2api.exe 在本机实测返回 9009（命令找不到），
rem 因为 cd 后的当前目录不参与可执行文件解析（NoDefaultCurrentDirectoryInExePath）。
"%~dp0wb2api.exe" >> data\gateway.log 2>&1
set "RC=%errorlevel%"

echo.
if "%RC%"=="0" (
  echo [完成] 网关已停止。
) else (
  echo [错误] 网关异常退出（退出码 %RC%）。请查看 data\gateway.log 末尾。
)
echo.
pause
exit /b 0

:no_exe
echo [错误] 未找到 wb2api.exe。请先构建：
echo        go build -o wb2api.exe ./cmd/server
goto err

:no_config
echo [错误] 未找到 config.json。请先复制模板：
echo        copy config.example.json config.json
echo        然后编辑它，至少设置 api_key（留空 = 不鉴权）。
goto err

:err
echo.
pause
exit /b 1
