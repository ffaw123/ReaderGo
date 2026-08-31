@echo off
setlocal EnableExtensions
chcp 65001 >nul

pushd "%~dp0.."
set "READERGO_EVAL_ENV_FILE=%CD%\.env"
set "READERGO_EVAL_PID_FILE=%TEMP%\ReaderGo\eval.pid"

if not exist "%READERGO_EVAL_ENV_FILE%" (
  echo 未找到项目根目录下的 .env 文件。
  popd
  exit /b 1
)

call :set_eval_flag 0
if errorlevel 1 (
  echo 无法将 RUN_RETRIEVAL_EVAL 设置为 0。
  popd
  exit /b 1
)

if exist "%READERGO_EVAL_PID_FILE%" goto stop_eval_process

echo 当前没有记录中的测评进程，评测开关已关闭。
set "STOP_EXIT_CODE=3"
goto remove_database_container

:stop_eval_process
set "EVAL_PID="
set /p "EVAL_PID="<"%READERGO_EVAL_PID_FILE%"
set "READERGO_EVAL_PID=%EVAL_PID%"

rem 只终止由评测启动脚本记录且命令行确认为离线评测的 Go 进程，避免误杀其他 Go 服务。
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$idText = $env:READERGO_EVAL_PID; [int]$id = 0; if (-not [int]::TryParse($idText, [ref]$id)) { exit 2 }; $p = Get-CimInstance Win32_Process -Filter ('ProcessId=' + $id) -ErrorAction SilentlyContinue; if ($null -eq $p) { exit 3 }; if ($p.Name -ine 'go.exe' -or $p.CommandLine -notmatch 'TestRetrieval(?:OfflineEval|BM25SourceEval)') { exit 4 }; $null = taskkill.exe /PID $id /T /F; exit $LASTEXITCODE"
set "STOP_EXIT_CODE=%ERRORLEVEL%"
del /q "%READERGO_EVAL_PID_FILE%" 2>nul

:remove_database_container
rem 先删除评测容器，再精确删除评测具名卷；不能使用 down -v，以免删除业务卷。
echo 正在停止评测 PostgreSQL，并删除评测容器及其临时数据卷...
call :remove_eval_database
set "DATABASE_REMOVE_EXIT_CODE=%ERRORLEVEL%"
popd

if not "%DATABASE_REMOVE_EXIT_CODE%"=="0" (
  echo 评测 PostgreSQL 容器或临时数据卷删除失败。
  endlocal
  exit /b %DATABASE_REMOVE_EXIT_CODE%
)

if "%STOP_EXIT_CODE%"=="0" (
  echo 测评任务已终止，评测 PostgreSQL 容器和临时数据卷已删除。
  endlocal
  exit /b 0
)
if "%STOP_EXIT_CODE%"=="3" (
  echo 测评进程已结束，评测 PostgreSQL 容器和临时数据卷已删除。
  endlocal
  exit /b 0
)
if "%STOP_EXIT_CODE%"=="4" (
  echo PID 对应的进程不是 ReaderGo 离线测评，未终止该进程；评测 PostgreSQL 容器和临时数据卷已删除。
  endlocal
  exit /b 1
)

echo 无法终止测评任务，退出码为 %STOP_EXIT_CODE%；评测 PostgreSQL 容器和临时数据卷已删除。
endlocal
exit /b %STOP_EXIT_CODE%

:remove_eval_database
docker compose --profile eval rm -f -s -v postgres-eval
if errorlevel 1 exit /b %ERRORLEVEL%
docker volume inspect readergo_postgres_eval_data >nul 2>&1
if errorlevel 1 exit /b 0
docker volume rm readergo_postgres_eval_data
exit /b %ERRORLEVEL%

:set_eval_flag
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$path = $env:READERGO_EVAL_ENV_FILE; $value = '%~1'; $content = [IO.File]::ReadAllText($path); $newline = if ($content.Contains([Environment]::NewLine)) { [Environment]::NewLine } else { [char]10 }; $pattern = [regex]'(?m)^RUN_RETRIEVAL_EVAL=[^\r\n]*'; if ($pattern.IsMatch($content)) { $content = $pattern.Replace($content, ('RUN_RETRIEVAL_EVAL=' + $value), 1) } else { if ($content.Length -gt 0 -and -not $content.EndsWith($newline)) { $content += $newline }; $content += 'RUN_RETRIEVAL_EVAL=' + $value + $newline }; [IO.File]::WriteAllText($path, $content, [Text.UTF8Encoding]::new($false))"
exit /b %ERRORLEVEL%
