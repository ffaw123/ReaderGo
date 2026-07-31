@echo off
setlocal

pushd "%~dp0.."
echo Closing ReaderGo web, Worker, and API terminals...
taskkill /FI "WINDOWTITLE eq ReaderGo Web" /T /F >nul 2>&1
taskkill /FI "WINDOWTITLE eq ReaderGo Worker" /T /F >nul 2>&1
taskkill /FI "WINDOWTITLE eq ReaderGo API" /T /F >nul 2>&1

echo Stopping ReaderGo Docker services and removing all project data volumes...
docker compose down -v
set "EXIT_CODE=%ERRORLEVEL%"
popd

if not "%EXIT_CODE%"=="0" (
  echo Failed to stop and reset Docker services.
  exit /b %EXIT_CODE%
)

echo ReaderGo Docker services and data volumes removed.
endlocal
