@echo off
:: install.bat — install SuitCode binaries from the Go module registry
::
:: Usage:
::   install.bat            install suitcode, coordinator (with tray), investigator
::   install.bat --ci       CGo-free build (no tray icon, safe for headless servers)
::
:: The coordinator binary includes the system-tray icon by default.
:: Use --ci only for servers or build agents where no desktop is available.

setlocal EnableDelayedExpansion

set "REPO=github.com/GreenFuze/SuitCode"
set CI=0

:: Parse arguments.
:parse_args
if "%~1"=="" goto args_done
if "%~1"=="--ci"   set CI=1
:: --tray is no longer needed: the coordinator now includes the tray icon.
if "%~1"=="--tray" (
    echo Note: --tray is no longer needed. The coordinator binary now includes
    echo       the system-tray icon. Continuing with default install.
)
shift
goto parse_args
:args_done

:: Verify Go is available.
where go >nul 2>&1
if errorlevel 1 (
    echo error: 'go' not found in PATH -- install Go 1.21+ from https://go.dev/dl/
    exit /b 1
)

for /f "tokens=3" %%v in ('go version') do set GO_VER=%%v
echo Using !GO_VER!

:: Core binaries.
echo.
echo Installing core binaries...

if "!CI!"=="1" (
    echo   [headless mode: no tray icon]
    set CGO_ENABLED=0
    go install !REPO!/suitcode@latest     || exit /b 1
    go install !REPO!/coordinator@latest  || exit /b 1
    go install !REPO!/investigator@latest || exit /b 1
) else (
    go install !REPO!/suitcode@latest                  || exit /b 1
    go install -tags systray !REPO!/coordinator@latest || exit /b 1
    go install !REPO!/investigator@latest              || exit /b 1
)

echo   suitcode       OK
echo   coordinator    OK  (tray icon included)
echo   investigator   OK

:: PATH reminder.
echo.
echo Done^^! Binaries are in: %USERPROFILE%\go\bin
echo.
echo Get started:
echo   coordinator                     ^& start the coordinator (shows tray icon)
echo   suitcode . warmup
echo   suitcode . context --files ^<file^> --budget 8000

endlocal
