@echo off
setlocal EnableExtensions
chcp 65001 >nul

pushd "%~dp0.."
set "READERGO_EVAL_PROJECT_ROOT=%CD%"
set "READERGO_EVAL_ENV_FILE=%CD%\.env"
set "READERGO_EVAL_SQL_FILE=%CD%\docs\evaldata\evaluation_database.sql"
set "READERGO_EVAL_RUNTIME_DIR=%TEMP%\ReaderGo"
set "READERGO_EVAL_PID_FILE=%TEMP%\ReaderGo\eval.pid"

if not exist "%READERGO_EVAL_ENV_FILE%" (
  echo 未找到项目根目录下的 .env 文件。
  popd
  exit /b 1
)

if not exist "%READERGO_EVAL_SQL_FILE%" (
  echo 未找到评测数据库快照：%READERGO_EVAL_SQL_FILE%
  popd
  exit /b 1
)

if not exist "%READERGO_EVAL_RUNTIME_DIR%" mkdir "%READERGO_EVAL_RUNTIME_DIR%"

if exist "%READERGO_EVAL_PID_FILE%" (
  set "EVAL_PID="
  set /p "EVAL_PID="<"%READERGO_EVAL_PID_FILE%"
  powershell.exe -NoProfile -Command "$p = Get-CimInstance Win32_Process -Filter ('ProcessId=' + $env:EVAL_PID) -ErrorAction SilentlyContinue; if ($null -ne $p -and $p.Name -ieq 'go.exe' -and $p.CommandLine -match 'TestRetrievalOfflineEval') { exit 0 }; exit 1"
  if not errorlevel 1 (
    echo 检测到测评任务已在运行，PID=%EVAL_PID%。
    popd
    exit /b 0
  )
  del /q "%READERGO_EVAL_PID_FILE%" 2>nul
)

call :set_eval_flag 0
if errorlevel 1 (
  echo 无法在数据库恢复前关闭 RUN_RETRIEVAL_EVAL。
  popd
  exit /b 1
)

rem 每次从冻结快照开始，先清理可能残留的评测容器和匿名数据卷，不触碰业务 postgres_data。
echo 正在清理旧评测数据库容器和临时数据卷...
docker compose --profile eval rm -f -s -v postgres-eval
if errorlevel 1 (
  echo 旧评测数据库清理失败。
  popd
  exit /b 1
)

echo 正在启动离线评测 PostgreSQL，并自动导入 evaluation_database.sql...
docker compose --profile eval up -d --wait postgres-eval
if errorlevel 1 (
  echo 离线测评专用 PostgreSQL 启动失败。
  docker compose --profile eval rm -f -s -v postgres-eval >nul 2>&1
  popd
  exit /b 1
)

call :set_eval_flag 1
if errorlevel 1 (
  echo 无法将 RUN_RETRIEVAL_EVAL 设置为 1。
  docker compose --profile eval rm -f -s -v postgres-eval >nul 2>&1
  popd
  exit /b 1
)

echo 开始离线检索测评，请勿在测评期间重新索引评测视频...
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$p = Start-Process -FilePath 'go.exe' -WorkingDirectory $env:READERGO_EVAL_PROJECT_ROOT -ArgumentList @('test','./internal/chat','-run','TestRetrievalOfflineEval','-count=1','-timeout','30m','-v') -NoNewWindow -PassThru; [IO.File]::WriteAllText($env:READERGO_EVAL_PID_FILE, [string]$p.Id); $p.WaitForExit(); exit $p.ExitCode"
set "EVAL_EXIT_CODE=%ERRORLEVEL%"

del /q "%READERGO_EVAL_PID_FILE%" 2>nul
call :set_eval_flag 0
popd

if not "%EVAL_EXIT_CODE%"=="0" (
  echo 测评失败，退出码为 %EVAL_EXIT_CODE%。
  echo.
  echo 窗口将保持打开，请检查错误信息后按任意键关闭，或直接手动关闭窗口。
  pause >nul
  endlocal
  exit /b %EVAL_EXIT_CODE%
)

echo 测评完成，RUN_RETRIEVAL_EVAL 已自动恢复为 0。
echo 评测数据库容器仍在运行，需要清理时请执行 eval_stop.bat。
echo.
echo 窗口将保持打开，请查看评测结果后按任意键关闭，或直接手动关闭窗口。
pause >nul
endlocal
exit /b 0

:set_eval_flag
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$path = $env:READERGO_EVAL_ENV_FILE; $value = '%~1'; $content = [IO.File]::ReadAllText($path); $newline = if ($content.Contains([Environment]::NewLine)) { [Environment]::NewLine } else { [char]10 }; $pattern = [regex]'(?m)^RUN_RETRIEVAL_EVAL=[^\r\n]*'; if ($pattern.IsMatch($content)) { $content = $pattern.Replace($content, ('RUN_RETRIEVAL_EVAL=' + $value), 1) } else { if ($content.Length -gt 0 -and -not $content.EndsWith($newline)) { $content += $newline }; $content += 'RUN_RETRIEVAL_EVAL=' + $value + $newline }; [IO.File]::WriteAllText($path, $content, [Text.UTF8Encoding]::new($false))"
exit /b %ERRORLEVEL%
