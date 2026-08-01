@echo off
setlocal

pushd "%~dp0.."
set "RUNTIME_DIR=%TEMP%\ReaderGo"
echo Closing ReaderGo web, Worker, and API terminals...
call :stop_service "%RUNTIME_DIR%\web.pid"
call :stop_service "%RUNTIME_DIR%\worker.pid"
call :stop_service "%RUNTIME_DIR%\api.pid"

echo Stopping ReaderGo Docker services while keeping data volumes...
docker compose down
set "EXIT_CODE=%ERRORLEVEL%"
popd

if not "%EXIT_CODE%"=="0" (
  echo Failed to stop Docker services.
  exit /b %EXIT_CODE%
)

echo ReaderGo Docker services stopped.
endlocal
exit /b

:stop_service
if not exist "%~1" exit /b
set "PID="
set /p "PID="<"%~1"
if defined PID taskkill /PID %PID% /T /F >nul 2>&1
del /q "%~1" 2>nul
exit /b
