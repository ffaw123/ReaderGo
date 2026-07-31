@echo off
setlocal

pushd "%~dp0.."
set "PROJECT_ROOT=%CD%"

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

echo Starting ReaderGo API, Worker, and web client in separate terminals...
start "ReaderGo API" /D "%PROJECT_ROOT%" powershell.exe -NoExit -Command "$Host.UI.RawUI.WindowTitle = 'ReaderGo API'; go run ./cmd/api"
start "ReaderGo Worker" /D "%PROJECT_ROOT%" powershell.exe -NoExit -Command "$Host.UI.RawUI.WindowTitle = 'ReaderGo Worker'; go run ./cmd/worker"
start "ReaderGo Web" /D "%PROJECT_ROOT%\web" powershell.exe -NoExit -Command "$Host.UI.RawUI.WindowTitle = 'ReaderGo Web'; npm run dev"

popd
endlocal
