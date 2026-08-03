@echo off
setlocal

pushd "%~dp0.."
set "PROJECT_ROOT=%CD%"
set "RUNTIME_DIR=%TEMP%\ReaderGo"

if not exist "%RUNTIME_DIR%" mkdir "%RUNTIME_DIR%"
del /q "%RUNTIME_DIR%\api.pid" "%RUNTIME_DIR%\outbox.pid" "%RUNTIME_DIR%\worker.pid" "%RUNTIME_DIR%\web.pid" 2>nul

echo Starting Docker services...
docker compose up -d
if errorlevel 1 (
  echo Failed to start Docker services.
  popd
  exit /b 1
)

if not exist "%PROJECT_ROOT%\web\node_modules\" (
  echo Installing web dependencies...
  pushd "%PROJECT_ROOT%\web"
  call npm install
  if errorlevel 1 (
    echo Failed to install web dependencies.
    popd
    popd
    exit /b 1
  )
  popd
)

echo Starting ReaderGo Outbox, Worker, API, and web client in separate terminals...
call :start_service "%PROJECT_ROOT%" "go run ./cmd/outbox" "%RUNTIME_DIR%\outbox.pid"
call :start_service "%PROJECT_ROOT%" "go run ./cmd/worker" "%RUNTIME_DIR%\worker.pid"
call :start_service "%PROJECT_ROOT%" "go run ./cmd/api" "%RUNTIME_DIR%\api.pid"
call :start_service "%PROJECT_ROOT%\web" "npm run dev" "%RUNTIME_DIR%\web.pid"

popd
endlocal
exit /b

:start_service
powershell.exe -NoProfile -Command "$process = Start-Process -FilePath 'powershell.exe' -WorkingDirectory '%~1' -ArgumentList '-NoExit', '-Command', '%~2' -PassThru; Set-Content -Path '%~3' -Value $process.Id"
exit /b
