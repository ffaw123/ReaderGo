@echo off
setlocal EnableExtensions
chcp 65001 >nul

pushd "%~dp0.."
set "READERGO_EVAL_PROJECT_ROOT=%CD%"
set "READERGO_EVAL_ENV_FILE=%CD%\.env"
set "READERGO_EVAL_SQL_FILE=%CD%\docs\evaldata\retrieval_tuning_database.sql"
set "READERGO_EVAL_QUESTIONS_FILE=%CD%\docs\evaldata\retrieval_tuning_questions.jsonl"
set "READERGO_EVAL_RUNTIME_DIR=%TEMP%\ReaderGo"
set "READERGO_EVAL_PID_FILE=%TEMP%\ReaderGo\eval.pid"

rem BM25 消融固定使用调参数据库和调参问题集，不写入或修改 .env。
set "RETRIEVAL_EVAL_SQL_FILE=./docs/evaldata/retrieval_tuning_database.sql"
set "RETRIEVAL_EVAL_DATASET=docs/evaldata/retrieval_tuning_questions.jsonl"
set "RUN_RETRIEVAL_BM25_EVAL=1"

if not exist "%READERGO_EVAL_ENV_FILE%" (
  echo 未找到项目根目录下的 .env 文件。
  popd
  exit /b 1
)
if not exist "%READERGO_EVAL_SQL_FILE%" (
  echo 未找到调参数据库快照：%READERGO_EVAL_SQL_FILE%
  popd
  exit /b 1
)
if not exist "%READERGO_EVAL_QUESTIONS_FILE%" (
  echo 未找到调参问题集：%READERGO_EVAL_QUESTIONS_FILE%
  popd
  exit /b 1
)

if not exist "%READERGO_EVAL_RUNTIME_DIR%" mkdir "%READERGO_EVAL_RUNTIME_DIR%"
if exist "%READERGO_EVAL_PID_FILE%" (
  set "EVAL_PID="
  set /p "EVAL_PID="<"%READERGO_EVAL_PID_FILE%"
  set "READERGO_EVAL_PID=%EVAL_PID%"
  powershell.exe -NoProfile -Command "$p = Get-CimInstance Win32_Process -Filter ('ProcessId=' + $env:READERGO_EVAL_PID) -ErrorAction SilentlyContinue; if ($null -ne $p -and $p.Name -ieq 'go.exe' -and $p.CommandLine -match 'TestRetrieval(?:OfflineEval|BM25SourceEval)') { exit 0 }; exit 1"
  if not errorlevel 1 (
    echo 检测到测评任务已在运行，PID=%EVAL_PID%。
    popd
    exit /b 0
  )
  del /q "%READERGO_EVAL_PID_FILE%" 2>nul
)

rem 每次从冻结的调参快照恢复独立数据库，不会触碰业务 postgres_data。
echo 正在清理旧评测数据库容器和临时数据卷...
call :remove_eval_database
if errorlevel 1 (
  echo 旧评测数据库清理失败。
  popd
  exit /b 1
)

echo 正在启动 BM25 消融测试 PostgreSQL，并导入 retrieval_tuning_database.sql...
docker compose --profile eval up -d --wait postgres-eval
if errorlevel 1 (
  echo BM25 消融测试 PostgreSQL 启动失败。
  call :remove_eval_database >nul 2>&1
  popd
  exit /b 1
)

echo 开始 BM25 文本来源消融测试，运行结束后仅打印汇总结果...
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$p = Start-Process -FilePath 'go.exe' -WorkingDirectory $env:READERGO_EVAL_PROJECT_ROOT -ArgumentList @('test','./internal/chat','-run','TestRetrievalBM25SourceEval','-count=1','-timeout','30m','-v') -NoNewWindow -PassThru; [IO.File]::WriteAllText($env:READERGO_EVAL_PID_FILE, [string]$p.Id); $p.WaitForExit(); exit $p.ExitCode"
set "EVAL_EXIT_CODE=%ERRORLEVEL%"
del /q "%READERGO_EVAL_PID_FILE%" 2>nul
popd

if not "%EVAL_EXIT_CODE%"=="0" (
  echo BM25 消融测试失败，退出码为 %EVAL_EXIT_CODE%。
  echo.
  echo 窗口将保持打开，请检查错误信息后按任意键关闭，或直接手动关闭窗口。
  pause >nul
  endlocal
  exit /b %EVAL_EXIT_CODE%
)

echo BM25 消融测试完成，未写入评测结果文件。
echo 评测数据库容器仍在运行，需要清理时请执行 eval_stop.bat。
echo.
echo 窗口将保持打开，请查看结果后按任意键关闭，或直接手动关闭窗口。
pause >nul
endlocal
exit /b 0

:remove_eval_database
docker compose --profile eval rm -f -s -v postgres-eval
if errorlevel 1 exit /b %ERRORLEVEL%
docker volume inspect readergo_postgres_eval_data >nul 2>&1
if errorlevel 1 exit /b 0
docker volume rm readergo_postgres_eval_data
exit /b %ERRORLEVEL%
