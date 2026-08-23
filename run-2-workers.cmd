@echo off
setlocal

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0run-2-workers.ps1"
set "worker_exit_code=%ERRORLEVEL%"

if not "%worker_exit_code%"=="0" (
    echo.
    echo Launcher failed with exit code %worker_exit_code%.
    echo Press any key to close this window.
    pause >nul
)

endlocal & exit /b %worker_exit_code%
